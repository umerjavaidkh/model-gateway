package httpapi_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/adapters/echo"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/gateway"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/httpapi"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/snapshot"
)

var pepper = []byte("a-test-pepper-that-is-long-enough")

func newTestServer(t *testing.T) http.Handler {
	t.Helper()

	global, err := core.NewGlobalLayer(core.GlobalSpec{
		Version: core.LayerVersion{Number: 42, Digest: "sha256:test"},
		Deployments: []core.Deployment{
			{ID: "echo-1", Key: core.RoutingKey{BaseModel: "echo-model"}, Provider: "echo",
				TrustTier: core.TrustInternal, Weight: 100},
		},
		TenantPrefixes: map[core.KeyPrefix]core.TenantID{"acme": "acme"},
	})
	if err != nil {
		t.Fatalf("NewGlobalLayer: %v", err)
	}
	tenant, err := core.NewTenantLayer(core.TenantSpec{
		Tenant: "acme", Version: core.LayerVersion{Number: 1}, Tier: "enterprise",
		Principals: []core.Principal{{
			KeyID: "key-1", Tenant: "acme", Models: core.ModelAllowlist{AllowAll: true},
		}},
		Keys:         map[core.KeyLookup]core.KeyID{core.ComputeKeyLookup(pepper, "secret-1"): "key-1"},
		MinTrustTier: core.TrustExternal,
	})
	if err != nil {
		t.Fatalf("NewTenantLayer: %v", err)
	}
	snap, err := core.Compose(global, tenant)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	holder, err := snapshot.New(snap)
	if err != nil {
		t.Fatalf("snapshot.New: %v", err)
	}

	providers, err := gateway.NewStaticProviders(echo.New())
	if err != nil {
		t.Fatalf("NewStaticProviders: %v", err)
	}
	pipeline, err := gateway.New(providers, gateway.NoCredentials{}, pepper)
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}

	server, err := httpapi.NewServer(holder, pipeline, httpapi.Options{
		// Discard logs so a failing test's output is the assertion, not a wall
		// of structured JSON.
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		NewID:  func() string { return "req-fixed" },
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return server.Handler()
}

func post(t *testing.T, handler http.Handler, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestChatCompletionsHappyPath(t *testing.T) {
	rec := post(t, newTestServer(t), "gw_acme_secret-1", `{"model":"echo-model","messages":[]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get(httpapi.HeaderRequestID); got != "req-fixed" {
		t.Fatalf("%s = %q", httpapi.HeaderRequestID, got)
	}
	// Which configuration served the request has to be on the response, or
	// "why did this route there?" is unanswerable after a rollout.
	if got := rec.Header().Get(httpapi.HeaderSnapshotVersion); got != "42@sha256:test" {
		t.Fatalf("%s = %q", httpapi.HeaderSnapshotVersion, got)
	}
}

func TestErrorsMapToTheRightStatus(t *testing.T) {
	handler := newTestServer(t)
	tests := []struct {
		name string
		key  string
		body string
		want int
		code string
	}{
		{name: "no credential", key: "", body: `{"model":"echo-model"}`, want: http.StatusUnauthorized, code: "unauthenticated"},
		{name: "unknown key", key: "gw_acme_nope", body: `{"model":"echo-model"}`, want: http.StatusUnauthorized, code: "unauthenticated"},
		{name: "unknown model", key: "gw_acme_secret-1", body: `{"model":"nope"}`, want: http.StatusNotFound, code: "model_not_found"},
		{name: "malformed json", key: "gw_acme_secret-1", body: `{`, want: http.StatusBadRequest, code: "invalid_request"},
		{name: "no model field", key: "gw_acme_secret-1", body: `{}`, want: http.StatusBadRequest, code: "invalid_request"},
		{name: "streaming not supported", key: "gw_acme_secret-1", body: `{"model":"echo-model","stream":true}`, want: http.StatusBadRequest, code: "invalid_request"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := post(t, handler, tc.key, tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body)
			}

			// The envelope is OpenAI-shaped so existing client libraries surface
			// gateway errors the same way they surface provider ones.
			var body struct {
				Error struct {
					Message   string `json:"message"`
					Type      string `json:"type"`
					Code      string `json:"code"`
					RequestID string `json:"request_id"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("error body is not JSON: %v (%s)", err, rec.Body)
			}
			if body.Error.Code != tc.code {
				t.Fatalf("code = %q, want %q", body.Error.Code, tc.code)
			}
			if body.Error.RequestID != "req-fixed" {
				t.Fatalf("the error body must carry the request id, got %q", body.Error.RequestID)
			}
		})
	}
}

func TestRequestIDIsReturnedEvenOnFailure(t *testing.T) {
	rec := post(t, newTestServer(t), "", `{"model":"echo-model"}`)
	if got := rec.Header().Get(httpapi.HeaderRequestID); got == "" {
		t.Fatal("a failed request must still be correlatable")
	}
}

func TestABareKeyWithoutBearerIsAccepted(t *testing.T) {
	// Enough clients send the key without the scheme that rejecting it costs
	// more in support than it buys in strictness.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"echo-model"}`))
	req.Header.Set("Authorization", "gw_acme_secret-1")
	rec := httptest.NewRecorder()
	newTestServer(t).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
}

func TestOversizedBodyIsRejectedNotBuffered(t *testing.T) {
	// Without a cap one caller can make the worker allocate until it is killed,
	// which no requests-per-minute limit catches.
	huge := `{"model":"echo-model","pad":"` + strings.Repeat("x", 9<<20) + `"}`
	rec := post(t, newTestServer(t), "gw_acme_secret-1", huge)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHealthAndReadiness(t *testing.T) {
	handler := newTestServer(t)

	for path, want := range map[string]int{"/healthz": http.StatusOK, "/readyz": http.StatusOK} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		if rec.Code != want {
			t.Fatalf("%s = %d, want %d", path, rec.Code, want)
		}
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))
	var ready map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &ready); err != nil {
		t.Fatalf("readyz is not JSON: %v", err)
	}
	if ready["snapshot_version"] != float64(42) {
		t.Fatalf("readyz must report the served snapshot version, got %v", ready["snapshot_version"])
	}
}

func TestWrongMethodAndUnknownPath(t *testing.T) {
	handler := newTestServer(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/chat/completions"},
		{http.MethodPost, "/v1/nope"},
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, nil))
		if rec.Code == http.StatusOK {
			t.Fatalf("%s %s returned 200", tc.method, tc.path)
		}
	}
}
