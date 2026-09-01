// Package anthropic implements ProviderPort for the Anthropic Messages API.
//
// It is the second real provider adapter, and it is deliberately not a variant
// of the OpenAI-compatible one. The two schemas differ in ways that matter:
//
//   - Authentication is an x-api-key header plus a required API version header,
//     not a bearer token.
//   - `system` is a top-level field rather than a message with role "system".
//   - `max_tokens` is required rather than optional.
//   - Streaming is a sequence of *typed* events — message_start,
//     content_block_delta, message_delta, message_stop — rather than a series of
//     identically-shaped deltas ending in a [DONE] sentinel.
//   - Token usage is split across two events: input counts arrive with
//     message_start, output counts with message_delta.
//
// The adapter forwards the caller's body essentially untouched, so a client
// written against Anthropic's API works through the gateway unchanged. What it
// normalizes is the framing: where the stream ends and where usage is reported.
package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

// Name is how a deployment refers to this adapter in a snapshot.
const Name = "anthropic"

// APIVersion is the Anthropic API version header value.
//
// Pinned rather than passed through from the caller: the response shape this
// adapter parses is tied to a version, and letting a caller choose a different
// one would silently change what usage parsing sees. Upgrading it is a
// deliberate change with a test run behind it.
const APIVersion = "2023-06-01"

const (
	// DefaultTimeout bounds one attempt when the request carries no deadline.
	DefaultTimeout = 10 * time.Minute
	// defaultMaxTokens is used only when the caller omits the field, which the
	// Anthropic API rejects. Refusing instead would break callers migrating
	// from an API where it is optional, for no benefit.
	defaultMaxTokens = 4096

	maxErrorBodyBytes = 8 << 10
	maxSSELineBytes   = 1 << 20
)

// Provider calls the Anthropic Messages API.
//
// Safe for concurrent use; the HTTP client is shared so connections pool.
type Provider struct {
	client *http.Client
}

// Option configures a Provider.
type Option func(*Provider)

// WithHTTPClient replaces the client, for tests and custom transports.
func WithHTTPClient(c *http.Client) Option {
	return func(p *Provider) { p.client = c }
}

// New returns a provider with pooled, keep-alive HTTP.
func New(opts ...Option) *Provider {
	p := &Provider{client: &http.Client{Timeout: DefaultTimeout}}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Name identifies the adapter.
func (*Provider) Name() string { return Name }

// Endpoints reports the one surface this adapter speaks.
func (*Provider) Endpoints() []core.Endpoint {
	return []core.Endpoint{core.EndpointMessages}
}

// Invoke makes a non-streaming call.
func (p *Provider) Invoke(ctx context.Context, call *core.ProviderCall) (*core.ProviderResponse, error) {
	resp, err := p.send(ctx, call, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= http.StatusBadRequest {
		return nil, upstreamError(resp)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, core.Wrap(core.CodeUpstreamError, err, "reading the provider response")
	}
	return &core.ProviderResponse{
		StatusCode: resp.StatusCode,
		Body:       body,
		Usage:      usageFromMessage(body),
	}, nil
}

// Stream makes a streaming call.
func (p *Provider) Stream(ctx context.Context, call *core.ProviderCall) (core.ChunkStream, error) {
	resp, err := p.send(ctx, call, true)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		defer func() { _ = resp.Body.Close() }()
		return nil, upstreamError(resp)
	}
	return newStream(resp.Body), nil
}

func (p *Provider) send(ctx context.Context, call *core.ProviderCall, stream bool) (*http.Response, error) {
	body, err := prepareBody(call, stream)
	if err != nil {
		return nil, err
	}

	url := strings.TrimSuffix(call.Deployment.Endpoint, "/") + "/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, core.Wrap(core.CodeInvalidRequest, err, "building the upstream request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", APIVersion)
	if len(call.Credential.Secret) > 0 {
		// Not a bearer token. Sending Authorization instead is a 401 that looks
		// like a bad key rather than a bad adapter.
		req.Header.Set("x-api-key", string(call.Credential.Secret))
	}
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}

	resp, err := p.client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, core.WrapRetryable(core.CodeUpstreamTimeout, err, "provider did not respond in time")
		}
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		return nil, core.WrapRetryable(core.CodeUpstreamError, err, "calling the provider")
	}
	return resp, nil
}

// prepareBody rewrites the model to the deployment's real id and fills in the
// fields the API requires.
//
// Decoding into a map rather than a struct keeps every other field the caller
// sent, including ones this gateway has never heard of, so a new Anthropic
// parameter does not need a gateway release.
func prepareBody(call *core.ProviderCall, stream bool) ([]byte, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(call.Body, &payload); err != nil {
		return nil, core.Wrap(core.CodeInvalidRequest, err, "the request body is not a JSON object")
	}

	model, err := json.Marshal(call.Deployment.Key.BaseModel)
	if err != nil {
		return nil, core.Wrap(core.CodeInternal, err, "encoding the model name")
	}
	payload["model"] = model
	payload["stream"] = json.RawMessage("false")
	if stream {
		payload["stream"] = json.RawMessage("true")
	}
	if _, ok := payload["max_tokens"]; !ok {
		payload["max_tokens"] = json.RawMessage(strconv.Itoa(defaultMaxTokens))
	}

	out, err := json.Marshal(payload)
	if err != nil {
		return nil, core.Wrap(core.CodeInternal, err, "re-encoding the request body")
	}
	return out, nil
}

// upstreamError classifies a provider error response. 429 and 5xx are worth
// another candidate; a 4xx is the caller's fault and retrying burns the shared
// deadline on the same rejection.
func upstreamError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	detail := strings.TrimSpace(string(body))
	if detail == "" {
		detail = resp.Status
	}

	err := core.Newf(core.CodeUpstreamError, "provider returned %d: %s", resp.StatusCode, detail)
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError {
		return err.AsRetryable()
	}
	return err
}

// message is the sliver of a non-streaming response holding usage.
//
// input_tokens here excludes cache reads and writes, which are reported
// alongside it — the opposite of the OpenAI convention, where the cached count
// is a subset of the total. TokenUsage normalizes both so nothing downstream
// has to know which it came from.
type message struct {
	Usage anthropicUsage `json:"usage"`
}

type anthropicUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

func (u anthropicUsage) normalized() core.TokenUsage {
	return core.TokenUsage{
		Input:       u.InputTokens,
		CachedInput: u.CacheReadInputTokens,
		CacheWrite:  u.CacheCreationInputTokens,
		Output:      u.OutputTokens,
	}
}

// usageFromMessage extracts reported usage, returning zero when absent. Zero is
// meaningful: accounting distinguishes reported from estimated usage, and
// substituting an estimate here would erase that at the only point it is known.
func usageFromMessage(body []byte) core.TokenUsage {
	var m message
	if err := json.Unmarshal(body, &m); err != nil {
		return core.TokenUsage{}
	}
	return m.Usage.normalized()
}

// --- streaming --------------------------------------------------------------

// Event types this adapter reacts to. Everything else — content_block_start,
// content_block_stop, ping — is forwarded to the caller without interpretation.
const (
	eventMessageStart = "message_start"
	eventMessageDelta = "message_delta"
	eventMessageStop  = "message_stop"
	eventError        = "error"
)

// streamEvent is the union of the fields this adapter reads across event types.
type streamEvent struct {
	Type    string `json:"type"`
	Message *struct {
		Usage anthropicUsage `json:"usage"`
	} `json:"message"`
	Usage *anthropicUsage `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// stream reads Anthropic's typed SSE events and yields normalized chunks.
type stream struct {
	body    io.ReadCloser
	scanner *bufio.Scanner

	mu       sync.Mutex
	closed   bool
	finished bool
	usage    core.TokenUsage
}

func newStream(body io.ReadCloser) *stream {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64<<10), maxSSELineBytes)
	return &stream{body: body, scanner: scanner}
}

// Next returns the next chunk, or io.EOF once the stream is complete.
//
// Usage is accumulated rather than read from one place: Anthropic reports input
// tokens with message_start and output tokens with message_delta, so a reader
// that looked at only the last event would record half the cost.
func (s *stream) Next(ctx context.Context) (core.Chunk, error) {
	if err := ctx.Err(); err != nil {
		return core.Chunk{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed || s.finished {
		return core.Chunk{}, io.EOF
	}

	for s.scanner.Scan() {
		line := strings.TrimSpace(s.scanner.Text())
		// Blank lines separate events; `event:` names the type, which the
		// payload also carries, so only `data:` needs parsing.
		if line == "" || strings.HasPrefix(line, ":") || strings.HasPrefix(line, "event:") {
			continue
		}
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}

		var event streamEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			// An unparseable event is forwarded rather than dropped: the caller
			// may understand a shape this adapter does not.
			return core.Chunk{Body: []byte(payload)}, nil
		}

		switch event.Type {
		case eventMessageStart:
			if event.Message != nil {
				// message_start carries the input classes, which never change
				// afterwards.
				s.usage = event.Message.Usage.normalized()
			}
		case eventMessageDelta:
			if event.Usage != nil {
				// message_delta carries a running total, so the last one wins
				// rather than accumulating. Input counts are only overwritten
				// when present, because a delta that omits them is not saying
				// they became zero.
				reported := event.Usage.normalized()
				if reported.Output > 0 {
					s.usage.Output = reported.Output
				}
				if reported.Input > 0 {
					s.usage.Input = reported.Input
				}
				if reported.CachedInput > 0 {
					s.usage.CachedInput = reported.CachedInput
				}
				if reported.CacheWrite > 0 {
					s.usage.CacheWrite = reported.CacheWrite
				}
			}
		case eventError:
			detail := "provider reported a stream error"
			if event.Error != nil {
				detail = event.Error.Type + ": " + event.Error.Message
			}
			return core.Chunk{}, core.New(core.CodeUpstreamError, detail)
		case eventMessageStop:
			// The terminating chunk carries the accumulated usage and a nil
			// error; io.EOF comes on the next call. Returning both at once is
			// the Go iterator trap that loses the value.
			s.finished = true
			return core.Chunk{Body: []byte(payload), Final: true, Usage: s.usage}, nil
		}

		return core.Chunk{Body: []byte(payload)}, nil
	}

	if err := s.scanner.Err(); err != nil {
		return core.Chunk{}, core.Wrap(core.CodeUpstreamError, err, "reading the provider stream")
	}
	// No message_stop. The completion is truncated, and reporting EOF would let
	// a half-finished answer look complete to the caller and to accounting.
	return core.Chunk{}, core.New(core.CodeUpstreamError, "provider stream ended without message_stop")
}

// Close releases the upstream connection. Safe to call more than once.
func (s *stream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil
	}
	s.closed = true
	if err := s.body.Close(); err != nil {
		return core.Wrap(core.CodeUpstreamError, err, "closing the provider stream")
	}
	return nil
}
