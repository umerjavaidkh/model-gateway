package anthropic_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/adapters/anthropic"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/contracts"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

type upstream struct {
	server  *httptest.Server
	gotBody []byte
	gotKey  string
	gotAuth string
	gotVer  string
	gotPath string
}

func newUpstream(t *testing.T, handler func(w http.ResponseWriter, body []byte)) *upstream {
	t.Helper()
	u := &upstream{}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.gotBody = body
		u.gotKey = r.Header.Get("x-api-key")
		u.gotAuth = r.Header.Get("Authorization")
		u.gotVer = r.Header.Get("anthropic-version")
		u.gotPath = r.URL.Path
		handler(w, body)
	}))
	t.Cleanup(u.server.Close)
	return u
}

func callFor(u *upstream, body string) *core.ProviderCall {
	return &core.ProviderCall{
		Deployment: core.Deployment{
			ID: "d1", Key: core.RoutingKey{BaseModel: "claude-opus-5"},
			Provider: anthropic.Name, Endpoint: u.server.URL, TrustTier: core.TrustExternal, Weight: 100,
		},
		Meta:       core.RequestMeta{RequestID: "r1", Model: "reasoning", Endpoint: core.EndpointMessages},
		Body:       []byte(body),
		Credential: core.Credential{Ref: "env:ANTHROPIC_API_KEY", Secret: []byte("sk-ant-test")},
	}
}

const messageResponse = `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":12,"output_tokens":5}}`

// streamEvents is a realistic Anthropic stream: usage arrives split across
// message_start and message_delta, and it terminates with message_stop rather
// than a [DONE] sentinel.
const streamEvents = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":12,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0}

event: ping
data: {"type":"ping"}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"he"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"llo"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}

event: message_stop
data: {"type":"message_stop"}

`

func TestSatisfiesProviderPort(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, body []byte) {
		var parsed struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &parsed)
		if parsed.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, streamEvents)
			return
		}
		_, _ = io.WriteString(w, messageResponse)
	})

	contracts.RunProviderSuite(contracts.Adapt(t),
		func(contracts.T) core.ProviderPort { return anthropic.New() },
		func(contracts.T) *core.ProviderCall { return callFor(u, `{"model":"reasoning","messages":[]}`) },
	)
}

func TestEndpointsIsMessagesOnly(t *testing.T) {
	// Translating an OpenAI-shaped request into this schema is a separate
	// feature, not something to do implicitly and get subtly wrong.
	got := anthropic.New().Endpoints()
	if len(got) != 1 || got[0] != core.EndpointMessages {
		t.Fatalf("Endpoints = %v, want just messages", got)
	}
}

func TestAuthenticationUsesTheApiKeyHeaderNotBearer(t *testing.T) {
	// Sending Authorization instead produces a 401 that looks like a bad key
	// rather than a bad adapter.
	u := newUpstream(t, func(w http.ResponseWriter, _ []byte) { _, _ = io.WriteString(w, messageResponse) })

	if _, err := anthropic.New().Invoke(t.Context(), callFor(u, `{"model":"reasoning"}`)); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if u.gotKey != "sk-ant-test" {
		t.Fatalf("x-api-key = %q", u.gotKey)
	}
	if u.gotAuth != "" {
		t.Fatalf("Authorization was sent as well: %q", u.gotAuth)
	}
	if u.gotVer != anthropic.APIVersion {
		t.Fatalf("anthropic-version = %q, want %q", u.gotVer, anthropic.APIVersion)
	}
	if u.gotPath != "/messages" {
		t.Fatalf("path = %q", u.gotPath)
	}
}

func TestModelIsRewrittenAndUnknownFieldsSurvive(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, _ []byte) { _, _ = io.WriteString(w, messageResponse) })

	_, err := anthropic.New().Invoke(t.Context(), callFor(u,
		`{"model":"reasoning","system":"be brief","max_tokens":100,"messages":[],"some_future_field":true}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	var sent map[string]any
	if err := json.Unmarshal(u.gotBody, &sent); err != nil {
		t.Fatalf("upstream got invalid JSON: %v", err)
	}
	if sent["model"] != "claude-opus-5" {
		t.Fatalf("model = %v", sent["model"])
	}
	// `system` is a top-level field in this schema, not a message. Losing it
	// would silently change the model's behaviour rather than erroring.
	if sent["system"] != "be brief" {
		t.Fatalf("system was lost: %v", sent["system"])
	}
	if sent["max_tokens"] != float64(100) {
		t.Fatalf("max_tokens = %v, want the caller's value", sent["max_tokens"])
	}
	if sent["some_future_field"] != true {
		t.Fatal("an unrecognised field was dropped")
	}
}

func TestMaxTokensIsSuppliedWhenTheCallerOmitsIt(t *testing.T) {
	// The API rejects a request without it. Refusing here instead would break
	// callers migrating from an API where it is optional, for no benefit.
	u := newUpstream(t, func(w http.ResponseWriter, _ []byte) { _, _ = io.WriteString(w, messageResponse) })

	if _, err := anthropic.New().Invoke(t.Context(), callFor(u, `{"model":"reasoning","messages":[]}`)); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var sent map[string]any
	_ = json.Unmarshal(u.gotBody, &sent)
	if sent["max_tokens"] == nil {
		t.Fatal("max_tokens was not supplied, so the API would reject the call")
	}
}

func TestUsageIsReadFromTheMessageResponse(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, _ []byte) { _, _ = io.WriteString(w, messageResponse) })

	resp, err := anthropic.New().Invoke(t.Context(), callFor(u, `{"model":"reasoning"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.Usage.Input != 12 || resp.Usage.Output != 5 {
		t.Fatalf("usage = %+v, want 12/5", resp.Usage)
	}
}

func TestStreamAccumulatesUsageAcrossEvents(t *testing.T) {
	// Input tokens arrive with message_start and output tokens with
	// message_delta. A reader that looked only at the last event would record
	// half the cost, which is the kind of bug that shows up as an invoice
	// dispute months later.
	u := newUpstream(t, func(w http.ResponseWriter, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, streamEvents)
	})

	stream, err := anthropic.New().Stream(t.Context(), callFor(u, `{"model":"reasoning"}`))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = stream.Close() }()

	var chunks int
	var final core.Chunk
	for {
		chunk, err := stream.Next(t.Context())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		chunks++
		if chunk.Final {
			final = chunk
		}
	}

	if chunks == 0 {
		t.Fatal("no chunks were produced")
	}
	if !final.Final {
		t.Fatal("no chunk was marked final")
	}
	if final.Usage.Input != 12 || final.Usage.Output != 7 {
		t.Fatalf("accumulated usage = %+v, want 12/7", final.Usage)
	}
}

func TestStreamSurfacesATypedErrorEvent(t *testing.T) {
	// Anthropic reports mid-stream failures as an error event on a 200
	// response. Treating it as ordinary content would relay a failure to the
	// caller as if it were an answer.
	u := newUpstream(t, func(w http.ResponseWriter, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3}}}\n\n")
		_, _ = io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n")
	})

	stream, err := anthropic.New().Stream(t.Context(), callFor(u, `{"model":"reasoning"}`))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = stream.Close() }()

	var lastErr error
	for range 10 {
		_, lastErr = stream.Next(t.Context())
		if lastErr != nil {
			break
		}
	}
	if lastErr == nil || errors.Is(lastErr, io.EOF) {
		t.Fatalf("an error event produced %v, want a real error", lastErr)
	}
	if !strings.Contains(lastErr.Error(), "overloaded_error") {
		t.Fatalf("the error does not name the cause: %v", lastErr)
	}
}

func TestATruncatedStreamIsAnError(t *testing.T) {
	// Without message_stop the completion is half-finished; reporting EOF would
	// let it look complete to the caller and to accounting.
	u := newUpstream(t, func(w http.ResponseWriter, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\"}\n\n")
	})

	stream, err := anthropic.New().Stream(t.Context(), callFor(u, `{"model":"reasoning"}`))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = stream.Close() }()

	var lastErr error
	for range 10 {
		_, lastErr = stream.Next(t.Context())
		if lastErr != nil {
			break
		}
	}
	if lastErr == nil || errors.Is(lastErr, io.EOF) {
		t.Fatalf("a truncated stream returned %v, want a real error", lastErr)
	}
}

func TestUpstreamErrorsAreClassifiedForRetry(t *testing.T) {
	for _, tc := range []struct {
		status    int
		retryable bool
	}{
		{status: http.StatusBadRequest, retryable: false},
		{status: http.StatusUnauthorized, retryable: false},
		{status: http.StatusTooManyRequests, retryable: true},
		{status: http.StatusServiceUnavailable, retryable: true},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			u := newUpstream(t, func(w http.ResponseWriter, _ []byte) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error"}}`)
			})
			_, err := anthropic.New().Invoke(t.Context(), callFor(u, `{"model":"reasoning"}`))
			if err == nil {
				t.Fatal("expected an error")
			}
			if core.IsRetryable(err) != tc.retryable {
				t.Fatalf("IsRetryable = %v, want %v", core.IsRetryable(err), tc.retryable)
			}
		})
	}
}

func TestNoCredentialSendsNoApiKey(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, _ []byte) { _, _ = io.WriteString(w, messageResponse) })
	call := callFor(u, `{"model":"reasoning"}`)
	call.Credential = core.Credential{}

	if _, err := anthropic.New().Invoke(t.Context(), call); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if u.gotKey != "" {
		t.Fatalf("x-api-key = %q, want none", u.gotKey)
	}
}

func TestCacheClassesAreReportedSeparately(t *testing.T) {
	// This schema reports cache reads and writes *alongside* input_tokens
	// rather than inside it — the opposite of the OpenAI convention. Both
	// normalize to the same disjoint classes.
	u := newUpstream(t, func(w http.ResponseWriter, _ []byte) {
		_, _ = io.WriteString(w, `{"id":"msg_1","usage":{"input_tokens":100,`+
			`"output_tokens":50,"cache_read_input_tokens":900,`+
			`"cache_creation_input_tokens":40}}`)
	})

	resp, err := anthropic.New().Invoke(t.Context(), callFor(u, `{"model":"reasoning"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	got := resp.Usage
	if got.Input != 100 || got.CachedInput != 900 || got.CacheWrite != 40 || got.Output != 50 {
		t.Fatalf("usage = %+v", got)
	}
	if got.TotalInput() != 1040 {
		t.Fatalf("TotalInput = %d, want 1040", got.TotalInput())
	}
}

func TestStreamedCacheClassesSurviveToTheFinalChunk(t *testing.T) {
	// Input classes arrive with message_start and are never repeated. A delta
	// that omits them is not saying they became zero.
	u := newUpstream(t, func(w http.ResponseWriter, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\","+
			"\"message\":{\"usage\":{\"input_tokens\":10,\"cache_read_input_tokens\":500,"+
			"\"cache_creation_input_tokens\":20,\"output_tokens\":1}}}\n\n")
		_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\","+
			"\"usage\":{\"output_tokens\":80}}\n\n")
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	})

	stream, err := anthropic.New().Stream(t.Context(), callFor(u, `{"model":"reasoning"}`))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = stream.Close() }()

	var final core.Chunk
	for {
		chunk, err := stream.Next(t.Context())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if chunk.Final {
			final = chunk
		}
	}

	if final.Usage.CachedInput != 500 || final.Usage.CacheWrite != 20 {
		t.Fatalf("cache classes lost across the stream: %+v", final.Usage)
	}
	if final.Usage.Output != 80 {
		t.Fatalf("Output = %d, want the running total from message_delta", final.Usage.Output)
	}
}
