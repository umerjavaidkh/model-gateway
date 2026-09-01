package openaicompat_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/adapters/openaicompat"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/contracts"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

// upstream is a stand-in for a provider. Recording the request it received is
// what lets the tests assert on what the adapter actually sent, which is where
// the interesting behaviour is.
type upstream struct {
	server  *httptest.Server
	gotBody []byte
	gotAuth string
	gotPath string
}

func newUpstream(t *testing.T, handler func(w http.ResponseWriter, body []byte)) *upstream {
	t.Helper()
	u := &upstream{}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.gotBody, u.gotAuth, u.gotPath = body, r.Header.Get("Authorization"), r.URL.Path
		handler(w, body)
	}))
	t.Cleanup(u.server.Close)
	return u
}

func callFor(u *upstream, body string) *core.ProviderCall {
	return &core.ProviderCall{
		Deployment: core.Deployment{
			ID: "d1", Key: core.RoutingKey{BaseModel: "gpt-4o-mini"},
			Provider: openaicompat.Name, Endpoint: u.server.URL, TrustTier: core.TrustExternal, Weight: 100,
		},
		Meta:       core.RequestMeta{RequestID: "r1", Model: "fast", Endpoint: core.EndpointChatCompletions},
		Body:       []byte(body),
		Credential: core.Credential{Ref: "env:KEY", Secret: []byte("sk-test")},
	}
}

const jsonResponse = `{"id":"c1","choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":11,"completion_tokens":7}}`

func TestSatisfiesProviderPort(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, body []byte) {
		var parsed struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &parsed)
		if parsed.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7}}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		_, _ = io.WriteString(w, jsonResponse)
	})

	contracts.RunProviderSuite(t,
		func(*testing.T) core.ProviderPort { return openaicompat.New() },
		func(*testing.T) *core.ProviderCall { return callFor(u, `{"model":"fast","messages":[]}`) },
	)
}

func TestInvokeRewritesTheModelAndKeepsEverythingElse(t *testing.T) {
	// The caller asked for the alias "fast"; the provider has never heard of
	// it. Everything else the caller sent must survive, including parameters
	// this gateway has never heard of, or a provider adding one needs a
	// gateway release.
	u := newUpstream(t, func(w http.ResponseWriter, _ []byte) { _, _ = io.WriteString(w, jsonResponse) })

	_, err := openaicompat.New().Invoke(t.Context(), callFor(u,
		`{"model":"fast","messages":[{"role":"user"}],"temperature":0.4,"some_future_field":{"a":1}}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	var sent map[string]any
	if err := json.Unmarshal(u.gotBody, &sent); err != nil {
		t.Fatalf("upstream got invalid JSON: %v", err)
	}
	if sent["model"] != "gpt-4o-mini" {
		t.Fatalf("model = %v, want the deployment's real model id", sent["model"])
	}
	if sent["temperature"] != 0.4 {
		t.Fatalf("temperature was lost: %v", sent["temperature"])
	}
	if _, ok := sent["some_future_field"]; !ok {
		t.Fatal("an unrecognised field was dropped; the adapter must forward what it does not understand")
	}
	if sent["stream"] != false {
		t.Fatalf("stream = %v, want false for Invoke", sent["stream"])
	}
}

func TestCredentialBecomesABearerToken(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, _ []byte) { _, _ = io.WriteString(w, jsonResponse) })

	if _, err := openaicompat.New().Invoke(t.Context(), callFor(u, `{"model":"fast"}`)); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if u.gotAuth != "Bearer sk-test" {
		t.Fatalf("Authorization = %q", u.gotAuth)
	}
	if u.gotPath != "/chat/completions" {
		t.Fatalf("path = %q", u.gotPath)
	}
}

func TestNoCredentialSendsNoAuthorizationHeader(t *testing.T) {
	// A self-hosted vLLM pod inside the network needs no credential, and
	// sending an empty bearer token would make it reject the request.
	u := newUpstream(t, func(w http.ResponseWriter, _ []byte) { _, _ = io.WriteString(w, jsonResponse) })
	call := callFor(u, `{"model":"fast"}`)
	call.Credential = core.Credential{}

	if _, err := openaicompat.New().Invoke(t.Context(), call); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if u.gotAuth != "" {
		t.Fatalf("Authorization = %q, want none", u.gotAuth)
	}
}

func TestReportedUsageIsCarriedBack(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, _ []byte) { _, _ = io.WriteString(w, jsonResponse) })

	resp, err := openaicompat.New().Invoke(t.Context(), callFor(u, `{"model":"fast"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.Usage.Input != 11 || resp.Usage.Output != 7 {
		t.Fatalf("usage = %+v, want 11/7", resp.Usage)
	}
}

func TestMissingUsageStaysZeroRatherThanBeingEstimated(t *testing.T) {
	// Accounting treats reported and estimated usage differently. Substituting
	// an estimate here would erase that distinction at the only point where it
	// is knowable.
	u := newUpstream(t, func(w http.ResponseWriter, _ []byte) {
		_, _ = io.WriteString(w, `{"id":"c1","choices":[]}`)
	})

	resp, err := openaicompat.New().Invoke(t.Context(), callFor(u, `{"model":"fast"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.Usage != (core.TokenUsage{}) {
		t.Fatalf("usage = %+v, want zero", resp.Usage)
	}
}

func TestUpstreamErrorsAreClassifiedForRetry(t *testing.T) {
	// A 4xx is the caller's fault and retrying it against another deployment
	// just burns the shared deadline on the same rejection. A 429 or 5xx is
	// worth another candidate.
	tests := []struct {
		status    int
		retryable bool
	}{
		{status: http.StatusBadRequest, retryable: false},
		{status: http.StatusUnauthorized, retryable: false},
		{status: http.StatusTooManyRequests, retryable: true},
		{status: http.StatusInternalServerError, retryable: true},
		{status: http.StatusServiceUnavailable, retryable: true},
	}

	for _, tc := range tests {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			u := newUpstream(t, func(w http.ResponseWriter, _ []byte) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"error":"upstream said no"}`)
			})

			_, err := openaicompat.New().Invoke(t.Context(), callFor(u, `{"model":"fast"}`))
			if err == nil {
				t.Fatal("expected an error")
			}
			if core.IsRetryable(err) != tc.retryable {
				t.Fatalf("IsRetryable = %v, want %v (%v)", core.IsRetryable(err), tc.retryable, err)
			}
		})
	}
}

func TestStreamAsksForUsageAndYieldsIt(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ": a comment that is framing, not payload\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"llo\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})

	stream, err := openaicompat.New().Stream(t.Context(), callFor(u, `{"model":"fast"}`))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = stream.Close() }()

	var payloads []string
	var usage core.TokenUsage
	for {
		chunk, err := stream.Next(t.Context())
		if len(chunk.Body) > 0 {
			payloads = append(payloads, string(chunk.Body))
		}
		if chunk.Usage.Input != 0 {
			usage = chunk.Usage
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
	}

	if len(payloads) != 3 {
		t.Fatalf("got %d payload chunks, want 3 (the comment must be skipped): %v", len(payloads), payloads)
	}
	if usage.Input != 3 || usage.Output != 2 {
		t.Fatalf("usage = %+v, want 3/2", usage)
	}

	var sent map[string]any
	_ = json.Unmarshal(u.gotBody, &sent)
	if sent["stream"] != true {
		t.Fatal("the upstream request must set stream")
	}
	if _, ok := sent["stream_options"]; !ok {
		t.Fatal("the adapter must ask for usage on the final chunk")
	}
}

func TestATruncatedStreamIsAnErrorNotACleanEnd(t *testing.T) {
	// Without [DONE] the completion is half-finished. Reporting EOF would let
	// it look complete to the caller and to accounting.
	u := newUpstream(t, func(w http.ResponseWriter, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n")
	})

	stream, err := openaicompat.New().Stream(t.Context(), callFor(u, `{"model":"fast"}`))
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

func TestStreamSurfacesAnUpstreamErrorStatusBeforeStreaming(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, _ []byte) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"slow down"}`)
	})

	_, err := openaicompat.New().Stream(t.Context(), callFor(u, `{"model":"fast"}`))
	if err == nil {
		t.Fatal("expected the 429 to surface before any chunk")
	}
	if !core.IsRetryable(err) {
		t.Fatal("a 429 must be retryable")
	}
}

func TestAMalformedRequestBodyIsRejectedBeforeTheCall(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, _ []byte) { _, _ = io.WriteString(w, jsonResponse) })

	_, err := openaicompat.New().Invoke(t.Context(), callFor(u, `not json`))
	if !errors.Is(err, core.ErrInvalidRequest) {
		t.Fatalf("err = %v, want invalid_request", err)
	}
	if u.gotBody != nil {
		t.Fatal("the adapter called upstream with a body it could not parse")
	}
}

func TestAnUnreachableEndpointIsRetryable(t *testing.T) {
	call := &core.ProviderCall{
		Deployment: core.Deployment{
			ID: "d1", Key: core.RoutingKey{BaseModel: "m"}, Provider: openaicompat.Name,
			// Port 1 is reserved and nothing listens on it.
			Endpoint: "http://127.0.0.1:1", TrustTier: core.TrustExternal, Weight: 100,
		},
		Body: []byte(`{"model":"fast"}`),
	}

	_, err := openaicompat.New().Invoke(t.Context(), call)
	if err == nil {
		t.Fatal("expected a connection error")
	}
	if !core.IsRetryable(err) {
		t.Fatalf("a refused connection must be retryable, got %v", err)
	}
}

func TestEndpointTrailingSlashIsTolerated(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, _ []byte) { _, _ = io.WriteString(w, jsonResponse) })
	call := callFor(u, `{"model":"fast"}`)
	call.Deployment.Endpoint = strings.TrimSuffix(u.server.URL, "/") + "/"

	if _, err := openaicompat.New().Invoke(t.Context(), call); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if u.gotPath != "/chat/completions" {
		t.Fatalf("path = %q, want no doubled slash", u.gotPath)
	}
}

func TestCachedTokensAreSubtractedFromTheInputTotal(t *testing.T) {
	// prompt_tokens includes the cached ones in this schema. Recording both
	// as-is would double-count input and over-bill every cached request.
	u := newUpstream(t, func(w http.ResponseWriter, _ []byte) {
		_, _ = io.WriteString(w, `{"id":"c1","usage":{"prompt_tokens":1000,`+
			`"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":900}}}`)
	})

	resp, err := openaicompat.New().Invoke(t.Context(), callFor(u, `{"model":"fast"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	if resp.Usage.Input != 100 || resp.Usage.CachedInput != 900 {
		t.Fatalf("usage = %+v, want 100 standard and 900 cached", resp.Usage)
	}
	if resp.Usage.TotalInput() != 1000 {
		t.Fatalf("TotalInput = %d, want the reported prompt_tokens", resp.Usage.TotalInput())
	}
}

func TestAnImpossibleCachedCountIsClamped(t *testing.T) {
	// A provider reporting more cached than total is malfunctioning, and a
	// negative input count would surface as a credit on an invoice.
	u := newUpstream(t, func(w http.ResponseWriter, _ []byte) {
		_, _ = io.WriteString(w, `{"usage":{"prompt_tokens":10,"completion_tokens":1,`+
			`"prompt_tokens_details":{"cached_tokens":999}}}`)
	})

	resp, err := openaicompat.New().Invoke(t.Context(), callFor(u, `{"model":"fast"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.Usage.Input < 0 {
		t.Fatalf("Input = %d, want it clamped at zero", resp.Usage.Input)
	}
}
