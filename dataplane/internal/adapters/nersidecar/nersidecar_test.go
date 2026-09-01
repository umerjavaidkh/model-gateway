package nersidecar_test

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/adapters/nersidecar"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

// socketPath returns a path short enough to bind. A Unix socket address is
// capped at about 100 bytes, and the per-test temp directory on macOS already
// spends most of that on the test name.
func socketPath(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "ner")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

// serveSocket runs a stand-in sidecar on a Unix socket, which is the transport
// the real one uses — a TCP fake would not exercise the dialer that matters.
func serveSocket(t *testing.T, handler http.Handler) string {
	t.Helper()

	path := socketPath(t)
	var config net.ListenConfig
	listener, err := config.Listen(t.Context(), "unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		_ = os.Remove(path)
	})
	return path
}

func entityHandler(entities []map[string]any) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"entities": entities, "backend": "test",
		})
	})
}

func TestEntitiesBecomeMatches(t *testing.T) {
	payload := []byte("call Dr. Ada Lovelace today")
	start, end := 9, 22 // "Ada Lovelace" within the payload

	path := serveSocket(t, entityHandler([]map[string]any{
		{"kind": "PERSON", "start": start, "end": end,
			"value": string(payload[start:end]), "score": 0.9},
	}))

	detector, err := nersidecar.New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	matches, err := detector.Detect(t.Context(), payload)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}

	if len(matches) != 1 || matches[0].Kind != "PERSON" {
		t.Fatalf("matches = %+v", matches)
	}
	if matches[0].Value != string(payload[start:end]) {
		t.Fatalf("value = %q", matches[0].Value)
	}
}

func TestAnEntityWhoseOffsetsDoNotVerifyIsDropped(t *testing.T) {
	// The sidecar reports byte offsets, and a mismatch means the two sides
	// disagree about the encoding. Substituting on a bad offset corrupts the
	// payload at a position nobody chose, which is worse than missing the
	// entity — so it is dropped rather than trusted.
	payload := []byte("call Ada today")
	path := serveSocket(t, entityHandler([]map[string]any{
		{"kind": "PERSON", "start": 5, "end": 8, "value": "Bob", "score": 0.9},
		{"kind": "PERSON", "start": 99, "end": 120, "value": "Ada", "score": 0.9},
		{"kind": "PERSON", "start": 5, "end": 5, "value": "", "score": 0.9},
	}))

	detector, _ := nersidecar.New(path)
	matches, err := detector.Detect(t.Context(), payload)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("matches = %+v, want every unverifiable entity dropped", matches)
	}
}

func TestAnUnreachableSidecarIsUnavailableNotFatal(t *testing.T) {
	// The caller decides what an unavailable statistical tier means, because
	// that depends on the data classification and only the caller knows it.
	detector, err := nersidecar.New(socketPath(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := detector.Detect(t.Context(), []byte("text")); !errorIsUnavailable(err) {
		t.Fatalf("err = %v, want unavailable", err)
	}
	if err := detector.Ping(t.Context()); err == nil {
		t.Fatal("a missing socket must fail the health check")
	}
}

func TestAnErrorStatusSurfaces(t *testing.T) {
	path := serveSocket(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))

	detector, _ := nersidecar.New(path)
	if _, err := detector.Detect(t.Context(), []byte("text")); err == nil {
		t.Fatal("a 500 from the sidecar was treated as success")
	}
}

func TestAHungSidecarDoesNotHangTheRequest(t *testing.T) {
	// The sidecar is on the hot path when policy asks for it, so its failure
	// mode must be a timeout rather than a stall.
	path := serveSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		w.WriteHeader(http.StatusOK)
	}))

	detector, _ := nersidecar.New(path, nersidecar.WithTimeout(50*time.Millisecond))

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = detector.Detect(t.Context(), []byte("text"))
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Detect did not respect its timeout")
	}
}

func TestPingSucceedsAgainstAHealthySidecar(t *testing.T) {
	path := serveSocket(t, entityHandler(nil))
	detector, _ := nersidecar.New(path)

	if err := detector.Ping(t.Context()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestNewRequiresASocketPath(t *testing.T) {
	if _, err := nersidecar.New(""); err == nil {
		t.Fatal("a detector with no socket would fail every call while looking configured")
	}
}

func errorIsUnavailable(err error) bool { return core.CodeOf(err) == core.CodeUnavailable }
