package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/httpapi"
)

// CORS is off unless a deployment names its origins. A gateway that holds
// provider credentials and spends tenants' budgets should not be reachable
// from whatever page a browser happens to have open.

func originHeaders(t *testing.T, handler http.Handler, method, origin string) http.Header {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, "/v1/chat/completions", nil)
	req.Header.Set("Origin", origin)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Result().Header
}

func TestNoCORSHeadersWithoutConfiguredOrigins(t *testing.T) {
	header := originHeaders(t, newTestServer(t), http.MethodPost, "http://evil.example")

	if got := header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("allowed %q with no origins configured", got)
	}
}

func TestOnlyAConfiguredOriginIsAllowed(t *testing.T) {
	handler := newTestServerWithOrigins(t, "http://localhost:18081")

	// Not a prefix match, not a suffix match, not a scheme-insensitive one.
	for origin, want := range map[string]string{
		"http://localhost:18081":              "http://localhost:18081",
		"http://localhost:18081.evil.example": "",
		"http://evil.example":                 "",
		"https://localhost:18081":             "",
		"http://localhost:18081/":             "",
	} {
		got := originHeaders(t, handler, http.MethodPost, origin).Get("Access-Control-Allow-Origin")
		if got != want {
			t.Errorf("origin %q echoed as %q, want %q", origin, got, want)
		}
	}
}

func TestAPreflightIsAnswered(t *testing.T) {
	// The mux has no OPTIONS route, so without the middleware a preflight is a
	// 405 and the browser reports a CORS failure with no cause attached.
	handler := newTestServerWithOrigins(t, "http://localhost:18081")

	req := httptest.NewRequestWithContext(t.Context(), http.MethodOptions, "/v1/chat/completions", nil)
	req.Header.Set("Origin", "http://localhost:18081")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight = %d, want 204", rec.Code)
	}
	allowed := headerValues(rec.Result().Header, "Access-Control-Allow-Headers")
	if !slices.Contains(allowed, "Authorization") {
		t.Fatalf("allowed headers = %v, without Authorization no browser can send a key", allowed)
	}
}

func TestAPreflightFromAnUnknownOriginIsNotAnswered(t *testing.T) {
	handler := newTestServerWithOrigins(t, "http://localhost:18081")

	req := httptest.NewRequestWithContext(t.Context(), http.MethodOptions, "/v1/chat/completions", nil)
	req.Header.Set("Origin", "http://evil.example")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code == http.StatusNoContent {
		t.Fatal("answered a preflight for an origin that is not allowed")
	}
}

func TestTheResponseVariesByOrigin(t *testing.T) {
	// Without this a shared cache can hand one origin's response to another,
	// which is the allowlist quietly not applying.
	header := originHeaders(t, newTestServerWithOrigins(t, "http://localhost:18081"),
		http.MethodPost, "http://localhost:18081")

	if vary := headerValues(header, "Vary"); !slices.Contains(vary, "Origin") {
		t.Fatalf("Vary = %v, want it to include Origin", vary)
	}
}

func TestTheRequestIDIsReadableByTheBrowser(t *testing.T) {
	// A cross-origin caller can read the body by default but no header, and
	// the request id is what they would quote when asking what went wrong.
	header := originHeaders(t, newTestServerWithOrigins(t, "http://localhost:18081"),
		http.MethodPost, "http://localhost:18081")

	exposed := headerValues(header, "Access-Control-Expose-Headers")
	if !slices.Contains(exposed, httpapi.HeaderRequestID) {
		t.Fatalf("exposed headers = %v, want %s among them", exposed, httpapi.HeaderRequestID)
	}
}

func TestCredentialsAreNeverAllowed(t *testing.T) {
	// This API authenticates with a bearer token the caller supplies, never a
	// cookie. Echoing Allow-Credentials would re-enable exactly the cross-site
	// request that carrying the key in a header avoids.
	header := originHeaders(t, newTestServerWithOrigins(t, "http://localhost:18081"),
		http.MethodPost, "http://localhost:18081")

	if got := header.Get("Access-Control-Allow-Credentials"); got != "" {
		t.Fatalf("Allow-Credentials = %q, want it unset", got)
	}
}

// headerValues splits a possibly-folded header into its individual values.
func headerValues(header http.Header, name string) []string {
	values := []string{}
	for _, line := range header.Values(name) {
		for _, part := range strings.Split(line, ",") {
			values = append(values, strings.TrimSpace(part))
		}
	}
	return values
}
