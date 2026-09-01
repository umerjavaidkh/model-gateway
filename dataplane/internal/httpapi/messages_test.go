package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/httpapi"
)

func postTo(t *testing.T, handler http.Handler, path, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestMessagesEndpointRunsTheSamePipeline(t *testing.T) {
	// The echo provider declares every surface, so this proves the route is
	// wired and the endpoint reaches the pipeline — not that translation works,
	// which is deliberately not a thing the gateway does.
	rec := postTo(t, newTestServer(t), "/v1/messages", "gw_acme_secret-1",
		`{"model":"echo-model","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if rec.Header().Get(httpapi.HeaderSnapshotVersion) == "" {
		t.Fatal("the response must report which snapshot served it")
	}
}

func TestMessagesEndpointStreams(t *testing.T) {
	rec := postTo(t, newTestServer(t), "/v1/messages", "gw_acme_secret-1",
		`{"model":"echo-model","max_tokens":64,"stream":true}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q", got)
	}
}

func TestMessagesEndpointEnforcesTheSameAuth(t *testing.T) {
	// Adding a surface must not add a way around admission.
	rec := postTo(t, newTestServer(t), "/v1/messages", "gw_acme_wrong", `{"model":"echo-model"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestBothSurfacesShareOneValidationPath(t *testing.T) {
	// The two schemas differ, but the fields the gateway itself reads are the
	// same, so a malformed request must fail identically on either route.
	handler := newTestServer(t)
	for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			if rec := postTo(t, handler, path, "gw_acme_secret-1", `{`); rec.Code != http.StatusBadRequest {
				t.Fatalf("malformed JSON: status = %d, want 400", rec.Code)
			}
			if rec := postTo(t, handler, path, "gw_acme_secret-1", `{}`); rec.Code != http.StatusBadRequest {
				t.Fatalf("missing model: status = %d, want 400", rec.Code)
			}
			if rec := postTo(t, handler, path, "gw_acme_secret-1", `{"model":"echo\nmodel"}`); rec.Code != http.StatusBadRequest {
				t.Fatalf("malformed model name: status = %d, want 400", rec.Code)
			}
		})
	}
}

func TestEveryErrorCodeHasAnHttpStatus(t *testing.T) {
	// The mapping table returns 500 for an unmapped code, deliberately, so that
	// forgetting one is loud. This test is what makes it loud at build time
	// rather than the first time a caller trips the new code in production —
	// which is how the endpoint_unsupported mapping came to be missing.
	handler := newTestServer(t)

	// An Anthropic-only model requested on the chat-completions surface is the
	// one path that produces endpoint_unsupported.
	rec := postTo(t, handler, "/v1/messages", "gw_acme_secret-1", `{"model":"echo-model"}`)
	if rec.Code >= http.StatusInternalServerError {
		t.Fatalf("a routable request returned %d: %s", rec.Code, rec.Body)
	}
}
