package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/httpapi"
)

func TestStreamingReturnsServerSentEvents(t *testing.T) {
	rec := post(t, newTestServer(t), "gw_acme_secret-1", `{"model":"echo-model","stream":true}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q", got)
	}
	// Intermediaries that buffer defeat streaming as thoroughly as not
	// flushing does.
	if got := rec.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("X-Accel-Buffering = %q, want no", got)
	}

	body := rec.Body.String()
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("the stream must end with a terminator, got:\n%s", body)
	}
	if strings.Count(body, "data: ") < 2 {
		t.Fatalf("expected several events, got:\n%s", body)
	}
}

func TestStreamingReassemblesToTheOriginalBody(t *testing.T) {
	// The echo provider returns the request, chunked. Concatenating the events
	// back must reproduce it exactly, which is what proves no chunk is dropped
	// or duplicated at a buffer boundary.
	sent := `{"model":"echo-model","stream":true,"messages":[{"role":"user","content":"a longer message so that it spans several chunks"}]}`
	rec := post(t, newTestServer(t), "gw_acme_secret-1", sent)

	var got strings.Builder
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok || payload == "[DONE]" {
			continue
		}
		got.WriteString(payload)
	}
	if got.String() != sent {
		t.Fatalf("reassembled stream differs from what was sent:\n got %q\nwant %q", got.String(), sent)
	}
}

func TestStreamingErrorsBeforeTheFirstByteAreStillNormalResponses(t *testing.T) {
	// Until a byte is written the status code is uncommitted, and that is the
	// only window in which a stream request can fail properly.
	handler := newTestServer(t)

	for name, tc := range map[string]struct {
		key, body string
		want      int
	}{
		"bad key":       {key: "gw_acme_wrong", body: `{"model":"echo-model","stream":true}`, want: http.StatusUnauthorized},
		"unknown model": {key: "gw_acme_secret-1", body: `{"model":"nope","stream":true}`, want: http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			rec := post(t, handler, tc.key, tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
			if strings.Contains(rec.Header().Get("Content-Type"), "event-stream") {
				t.Fatal("a pre-stream failure must not be sent as an event stream")
			}
		})
	}
}

func TestStreamingRequestIsNotAffectedByASnapshotSwap(t *testing.T) {
	// The lease is held for the whole handler, streaming included, so a swap
	// mid-stream cannot change which deployment the rest of the response comes
	// from.
	handler := newTestServer(t)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"echo-model","stream":true}`))
	req.Header.Set("Authorization", "Bearer gw_acme_secret-1")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get(httpapi.HeaderSnapshotVersion); got == "" {
		t.Fatal("a streamed response must still report which snapshot served it")
	}
}
