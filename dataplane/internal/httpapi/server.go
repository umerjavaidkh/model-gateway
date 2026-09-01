// Package httpapi is the HTTP transport for the data plane.
//
// It does three things and no more: turn an HTTP request into a
// gateway.Request, lease a snapshot for the life of that request, and turn the
// result or error back into an HTTP response. All policy lives in the pipeline,
// so this package can be read in one sitting and a second transport would not
// duplicate any decision.
package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/gateway"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/snapshot"
)

const (
	// HeaderRequestID carries the id the gateway generated. It is returned on
	// every response, success or failure, so a client-side error can always be
	// correlated with a server-side trace.
	HeaderRequestID = "X-Request-Id"
	// HeaderSnapshotVersion says which configuration served the request. After
	// a rollout, "why did this route there?" is otherwise unanswerable.
	HeaderSnapshotVersion = "X-Gateway-Snapshot-Version"
	// HeaderWarning is the standard header used to tell a caller their key is
	// the outgoing generation of a rotation.
	HeaderWarning        = "Warning"
	deprecatedKeyWarning = `299 - "This API key is deprecated and will stop working after its rotation window"`

	// maxBodyBytes caps an inbound payload. Without a cap, one caller can make
	// the worker allocate until it is killed, which is a denial of service that
	// no rate limit expressed in requests per minute will catch.
	maxBodyBytes = 8 << 20 // 8 MiB
)

// Server holds the dependencies the handlers need.
type Server struct {
	holder   *snapshot.Holder
	pipeline *gateway.Pipeline
	logger   *slog.Logger
	newID    func() string
}

// Options configures a Server. Fields left zero take a sensible default.
type Options struct {
	Logger *slog.Logger
	// NewID overrides request-id generation, for deterministic tests.
	NewID func() string
}

// NewServer builds the HTTP handler set.
func NewServer(holder *snapshot.Holder, pipeline *gateway.Pipeline, opts Options) (*Server, error) {
	if holder == nil || pipeline == nil {
		return nil, core.New(core.CodeInternal, "the server needs a snapshot holder and a pipeline")
	}
	s := &Server{holder: holder, pipeline: pipeline, logger: opts.Logger, newID: opts.NewID}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	if s.newID == nil {
		s.newID = randomID
	}
	return s, nil
}

// Handler returns the routed, wrapped handler to serve.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Liveness answers "is this process running", readiness answers "can it
	// serve". They are separate because a worker with no snapshot must be taken
	// out of the load balancer, not restarted.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)

	return s.recoverPanics(mux)
}

func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	stats := s.holder.Stats()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"snapshot_version": stats.Version.Number,
		"snapshot_digest":  stats.Version.Digest,
		"in_flight":        stats.InFlight,
		"draining":         stats.PreviousInFlight,
		"previous_loaded":  stats.PreviousLoaded,
		"previous_version": stats.PreviousVersion.Number,
	})
}

// chatRequest is the sliver of the OpenAI payload the gateway itself needs.
//
// The rest stays opaque and is forwarded byte for byte. Parsing the whole
// schema would mean tracking every provider's additions forever, and would make
// the gateway reject payloads a provider would have accepted.
type chatRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	requestID := s.newID()
	w.Header().Set(HeaderRequestID, requestID)

	// One lease for the whole request. Every stage then sees the same
	// configuration however many snapshot swaps happen while it runs.
	lease := s.holder.Acquire()
	defer lease.Release()
	snap := lease.Snapshot()
	w.Header().Set(HeaderSnapshotVersion, snap.GlobalVersion().String())

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, s.logger, requestID, core.Newf(core.CodeInvalidRequest,
				"request body exceeds the %d byte limit", maxBodyBytes))
			return
		}
		writeError(w, s.logger, requestID, core.Wrap(core.CodeInvalidRequest, err, "reading the request body"))
		return
	}

	var parsed chatRequest
	if err := json.Unmarshal(body, &parsed); err != nil {
		writeError(w, s.logger, requestID, core.Wrap(core.CodeInvalidRequest, err, "parsing the request body"))
		return
	}
	if parsed.Model == "" {
		writeError(w, s.logger, requestID, core.New(core.CodeInvalidRequest, "the request has no model"))
		return
	}
	// Validate at the boundary so that no stage downstream — routing, logging,
	// metrics, error messages — has to defend against a control character or an
	// unbounded string in a caller-supplied name.
	if !validModelName(parsed.Model) {
		writeError(w, s.logger, requestID, core.New(core.CodeInvalidRequest,
			"the model name is malformed or too long"))
		return
	}
	req := &gateway.Request{
		APIKey: bearerToken(r),
		Body:   body,
		Meta: core.RequestMeta{
			RequestID:      requestID,
			Model:          parsed.Model,
			Endpoint:       core.EndpointChatCompletions,
			Stream:         parsed.Stream,
			PayloadBytes:   len(body),
			SourceIP:       clientIP(r),
			IdempotencyKey: r.Header.Get("Idempotency-Key"),
			ReceivedAt:     time.Now(),
		},
	}
	if deadline, ok := r.Context().Deadline(); ok {
		req.Meta.Deadline = deadline
	}

	if parsed.Stream {
		s.streamCompletion(w, r, snap, req, requestID)
		return
	}

	result, err := s.pipeline.Handle(r.Context(), snap, req)
	if result != nil && result.Principal.Deprecated {
		w.Header().Set(HeaderWarning, deprecatedKeyWarning)
	}
	if err != nil {
		writeError(w, s.logger, requestID, err)
		return
	}

	s.logger.Info("request served",
		slog.String("request_id", requestID),
		slog.String("tenant", string(result.Principal.Tenant)),
		// The only caller-controlled value in this record. Validated above and
		// sanitised here, because a log record must be safe whatever handler is
		// configured.
		slog.String("model", logSafe(parsed.Model)),
		slog.String("deployment", string(result.Deployment)),
		slog.Int64("input_tokens", result.Usage.Input),
		slog.Int64("output_tokens", result.Usage.Output),
		slog.Duration("latency", result.Latency))

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(result.StatusCode)
	_, _ = w.Write(result.Body)
}

// streamCompletion relays a provider stream to the caller as server-sent
// events.
//
// Two things make this harder than copying bytes:
//
// Once the first byte is written the status code is committed, so an error
// mid-stream cannot become a 502. The only honest signal left is an SSE error
// event followed by closing the stream, and the record of what happened lives
// in the log and the usage event rather than in the HTTP status.
//
// Each event must be flushed immediately. Without that, Go buffers the
// response and the caller sees nothing until the completion finishes — which
// is exactly the latency that streaming exists to avoid.
func (s *Server) streamCompletion(w http.ResponseWriter, r *http.Request, snap *core.Snapshot, req *gateway.Request, requestID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, s.logger, requestID, core.New(core.CodeInternal, "this server cannot stream"))
		return
	}

	result, err := s.pipeline.HandleStream(r.Context(), snap, req)
	if result != nil && result.Principal.Deprecated {
		w.Header().Set(HeaderWarning, deprecatedKeyWarning)
	}
	if err != nil {
		// Nothing has been written yet, so a normal error response is still
		// possible. This is the only window in which that is true.
		writeError(w, s.logger, requestID, err)
		return
	}
	defer func() { _ = result.Chunks.Close() }()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	// Tell any intermediary not to buffer, which would defeat streaming just as
	// thoroughly as not flushing here.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	var usage core.TokenUsage
	for {
		chunk, err := result.Chunks.Next(r.Context())
		if chunk.Usage.Input != 0 || chunk.Usage.Output != 0 {
			usage = chunk.Usage
		}
		if len(chunk.Body) > 0 {
			if _, writeErr := fmt.Fprintf(w, "data: %s\n\n", chunk.Body); writeErr != nil {
				// The caller hung up. Not an error worth alarming on, but the
				// upstream call still cost money and is still accounted for.
				s.logger.Info("client disconnected mid-stream",
					slog.String("request_id", requestID))
				return
			}
			flusher.Flush()
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			s.logger.Error("stream failed after the response began",
				slog.String("request_id", requestID),
				slog.String("code", string(core.CodeOf(err))),
				slog.String("error", err.Error()))
			// The status is already 200. An error event is the only thing left
			// that a client can act on.
			_, _ = fmt.Fprintf(w, "event: error\ndata: {\"error\":{\"code\":%q,\"request_id\":%q}}\n\n",
				core.CodeOf(err), requestID)
			flusher.Flush()
			return
		}
	}

	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()

	s.logger.Info("stream served",
		slog.String("request_id", requestID),
		slog.String("tenant", string(result.Principal.Tenant)),
		slog.String("model", logSafe(req.Meta.Model)),
		slog.String("deployment", string(result.Deployment)),
		slog.Int64("input_tokens", usage.Input),
		slog.Int64("output_tokens", usage.Output))
}

// recoverPanics turns a panic in one request into a 500 for that request.
//
// A worker serves thousands of concurrent requests; letting one nil dereference
// take down the process takes every other in-flight request with it.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				// http.ErrAbortHandler is the standard way to abort a response
				// deliberately; swallowing it would break that contract.
				if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(v)
				}
				s.logger.Error("panic serving request",
					slog.String("request_id", w.Header().Get(HeaderRequestID)),
					slog.Any("panic", v))
				writeError(w, s.logger, w.Header().Get(HeaderRequestID),
					core.New(core.CodeInternal, "internal error"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// bearerToken pulls the key out of an Authorization header. It also accepts a
// bare key, because that is what a surprising number of clients send.
func bearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if after, found := strings.CutPrefix(header, "Bearer "); found {
		return strings.TrimSpace(after)
	}
	return strings.TrimSpace(header)
}

func randomID() string {
	var b [16]byte
	// crypto/rand.Read cannot fail on any supported platform; it panics
	// internally rather than returning an error, so there is nothing to handle.
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
