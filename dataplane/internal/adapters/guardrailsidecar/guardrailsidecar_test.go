package guardrailsidecar_test

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/adapters/guardrailsidecar"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

// socketPath returns a path short enough to bind: a Unix socket address is
// capped near 100 bytes and the per-test temp directory spends most of that on
// the test name.
func socketPath(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "gs")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

func serve(t *testing.T, handler http.Handler) *guardrailsidecar.Guardrail {
	t.Helper()

	path := socketPath(t)
	var config net.ListenConfig
	listener, err := config.Listen(t.Context(), "unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	guardrail, err := guardrailsidecar.New("test-guard", path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return guardrail
}

func replying(body map[string]any) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
			return
		}
		_ = json.NewEncoder(w).Encode(body)
	})
}

func input(payload string) *core.GuardrailInput {
	return &core.GuardrailInput{Phase: core.PhaseRequest, Payload: []byte(payload)}
}

func TestVerdictsAreTranslated(t *testing.T) {
	for _, tc := range []struct {
		name string
		body map[string]any
		want core.Verdict
	}{
		{"allow", map[string]any{"verdict": "allow"}, core.VerdictAllow},
		{"deny", map[string]any{"verdict": "deny", "reason": "secret"}, core.VerdictDeny},
		{"mutate", map[string]any{"verdict": "mutate", "payload": []byte("redacted")}, core.VerdictMutate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := serve(t, replying(tc.body)).Inspect(t.Context(), input("hello"))
			if err != nil {
				t.Fatalf("Inspect: %v", err)
			}
			if result.Verdict != tc.want {
				t.Fatalf("verdict = %v, want %v", result.Verdict, tc.want)
			}
		})
	}
}

func TestAnUnknownVerdictIsRefusedRatherThanTreatedAsAllow(t *testing.T) {
	// The two sides disagree about the protocol. Resolving that in favour of
	// letting the request through is the wrong default for a component whose
	// job is to refuse things.
	_, err := serve(t, replying(map[string]any{"verdict": "maybe"})).Inspect(t.Context(), input("x"))

	if core.CodeOf(err) != core.CodeUnavailable {
		t.Fatalf("err = %v, want unavailable", err)
	}
}

func TestMutateWithoutAPayloadIsRefused(t *testing.T) {
	// The chain would forward the original, so the rewrite silently does not
	// happen — a control that appears to be working and is not.
	_, err := serve(t, replying(map[string]any{"verdict": "mutate"})).Inspect(t.Context(), input("x"))

	if err == nil {
		t.Fatal("mutate with no payload was accepted")
	}
}

func TestAPayloadOnANonMutateVerdictIsRefused(t *testing.T) {
	// It would be returned and never applied. Refusing is louder than ignoring.
	_, err := serve(t, replying(map[string]any{
		"verdict": "allow", "payload": []byte("ignored"),
	})).Inspect(t.Context(), input("x"))

	if err == nil {
		t.Fatal("a payload on an allow verdict was accepted")
	}
}

func TestTheNameComesFromTheBindingNotTheComponent(t *testing.T) {
	// A snapshot binds by name. Asking an untrusted process what it is called
	// would let it answer with someone else's binding.
	guardrail := serve(t, replying(map[string]any{"verdict": "allow"}))

	if guardrail.Name() != "test-guard" {
		t.Fatalf("Name = %q", guardrail.Name())
	}
}

func TestAnUnreachableSidecarIsUnavailable(t *testing.T) {
	guardrail, err := guardrailsidecar.New("test-guard", socketPath(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := guardrail.Inspect(t.Context(), input("x")); core.CodeOf(err) != core.CodeUnavailable {
		t.Fatalf("err = %v, want unavailable", err)
	}
	if err := guardrail.Ping(t.Context()); err == nil {
		t.Fatal("a missing socket passed the health check")
	}
}

func TestAnErrorStatusSurfaces(t *testing.T) {
	guardrail := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))

	if _, err := guardrail.Inspect(t.Context(), input("x")); err == nil {
		t.Fatal("a 500 was treated as a verdict")
	}
}

func TestNewRequiresANameAndASocket(t *testing.T) {
	if _, err := guardrailsidecar.New("", "/tmp/s"); err == nil {
		t.Fatal("a nameless guardrail cannot be bound, so it must not construct")
	}
	if _, err := guardrailsidecar.New("g", ""); err == nil {
		t.Fatal("a guardrail with no socket would fail every call while looking configured")
	}
}

func TestTheRequestCarriesThePayloadAndPhase(t *testing.T) {
	// The wire shape another language has to implement. Asserted here so a
	// change to it is a change to this test rather than a silent break.
	var got map[string]any
	guardrail := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(map[string]any{"verdict": "allow"})
	}))

	in := input("hello")
	in.Phase = core.PhaseResponse
	if _, err := guardrail.Inspect(t.Context(), in); err != nil {
		t.Fatalf("Inspect: %v", err)
	}

	if got["phase"] != "response" {
		t.Fatalf("phase = %v", got["phase"])
	}
	// []byte is base64 in JSON, which is the part a component in another
	// language has to get right.
	if got["payload"] != "aGVsbG8=" {
		t.Fatalf("payload = %v, want base64 of \"hello\"", got["payload"])
	}
}
