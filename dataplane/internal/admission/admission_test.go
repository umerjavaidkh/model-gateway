package admission_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/admission"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/sandbox"
)

const pinned = "ghcr.io/acme/guard@sha256:" +
	"0000000000000000000000000000000000000000000000000000000000000000"

func manifest() admission.Manifest {
	return admission.Manifest{
		Name: "acme-guard", Version: "1.0.0", Port: "guardrail",
		Digest: strings.Repeat("a", 64), Image: pinned, LatencyBudgetMS: 50,
	}
}

func fixtures() admission.Fixtures {
	return admission.Fixtures{Benign: []byte("hello"), Trigger: []byte("AKIAIOSFODNN7EXAMPLE")}
}

// fakeSandbox stands in for the container runtime by serving the component on
// a real Unix socket. The isolation flags are asserted in the sandbox package;
// what matters here is that the runner drives whatever the sandbox produced.
type fakeSandbox struct {
	handler  http.Handler
	startErr error
}

func (f fakeSandbox) Start(ctx context.Context, spec sandbox.Spec) (*sandbox.Handle, error) {
	if f.startErr != nil {
		return nil, f.startErr
	}

	path := filepath.Join(spec.SocketDir, spec.SocketName)
	var config net.ListenConfig
	listener, err := config.Listen(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	server := &http.Server{Handler: f.handler, ReadHeaderTimeout: time.Second}
	go func() { _ = server.Serve(listener) }()

	return sandbox.NewHandle(path, func() { _ = server.Close() }), nil
}

// component is a guardrail that denies anything containing the trigger.
func component() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		var request struct {
			Payload []byte `json:"payload"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)

		reply := map[string]any{"verdict": "allow"}
		if strings.Contains(string(request.Payload), "AKIA") {
			reply = map[string]any{"verdict": "deny", "reason": "aws-access-key"}
		}
		_ = json.NewEncoder(w).Encode(reply)
	})
}

func runner(t *testing.T, box sandbox.Runner) *admission.Runner {
	t.Helper()

	r, err := admission.NewRunner("sandbox://test", box)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return r
}

func TestAWellBehavedComponentPasses(t *testing.T) {
	verdict, err := runner(t, fakeSandbox{handler: component()}).
		Run(t.Context(), manifest(), fixtures())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !verdict.Passed {
		t.Fatalf("a conforming component failed:\n%s", verdict.Report)
	}
	// The record has to say what examined what, or it is an opinion.
	if verdict.ManifestDigest != manifest().Digest {
		t.Fatalf("digest = %q", verdict.ManifestDigest)
	}
	if verdict.Runner != "sandbox://test" || verdict.Suite != "guardrail" {
		t.Fatalf("verdict = %+v", verdict)
	}
	if verdict.SuiteVersion == "" {
		t.Fatal("the suite version is what makes an older admission visible as older")
	}
}

func TestAComponentThatDeniesEverythingFails(t *testing.T) {
	// It would pass every assertion about denying, and refuse all real traffic.
	denyAll := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		_, _ = w.Write([]byte(`{"verdict":"deny","reason":"no"}`))
	})

	verdict, err := runner(t, fakeSandbox{handler: denyAll}).
		Run(t.Context(), manifest(), fixtures())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if verdict.Passed {
		t.Fatal("a component that denies benign traffic was admitted")
	}
	if !strings.Contains(verdict.Report, "allows a benign payload unchanged") {
		t.Fatalf("the report does not name the failure:\n%s", verdict.Report)
	}
}

func TestAComponentThatAllowsItsOwnTriggerFails(t *testing.T) {
	allowAll := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		_, _ = w.Write([]byte(`{"verdict":"allow"}`))
	})

	verdict, err := runner(t, fakeSandbox{handler: allowAll}).
		Run(t.Context(), manifest(), fixtures())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if verdict.Passed {
		t.Fatal("a guardrail that allows what it claims to catch was admitted")
	}
}

func TestASandboxThatWillNotStartIsNotAFailingVerdict(t *testing.T) {
	// "We could not test this" and "this failed its tests" are different facts.
	// Recording the first as the second lets an infrastructure problem look
	// like a component defect.
	_, err := runner(t, fakeSandbox{startErr: core.New(core.CodeUnavailable, "no runtime")}).
		Run(t.Context(), manifest(), fixtures())

	if err == nil {
		t.Fatal("a sandbox failure produced a verdict")
	}
}

func TestAPortWithNoSandboxedSuiteIsRefused(t *testing.T) {
	// Running an empty battery and reporting a pass is the worst option.
	spec := manifest()
	spec.Port = "provider"

	if _, err := runner(t, fakeSandbox{handler: component()}).
		Run(t.Context(), spec, fixtures()); err == nil {
		t.Fatal("a port with no suite reported a verdict")
	}
}

func TestAManifestWithNoDigestIsRefused(t *testing.T) {
	spec := manifest()
	spec.Digest = ""

	if _, err := runner(t, fakeSandbox{handler: component()}).
		Run(t.Context(), spec, fixtures()); err == nil {
		t.Fatal("a verdict was produced without saying which bytes it examined")
	}
}

func TestABenignFixtureIsRequired(t *testing.T) {
	if _, err := runner(t, fakeSandbox{handler: component()}).
		Run(t.Context(), manifest(), admission.Fixtures{Trigger: []byte("x")}); err == nil {
		t.Fatal("the suite ran without the case that catches a deny-everything component")
	}
}

func TestARunnerMustBeNamed(t *testing.T) {
	// An auditor has to be able to tell one sandbox host from the control plane.
	if _, err := admission.NewRunner("", fakeSandbox{}); err == nil {
		t.Fatal("an anonymous runner was accepted")
	}
}

// --- reporting to the control plane -----------------------------------------

func TestTheVerdictIsPostedAgainstTheComponentItExamined(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	client, err := admission.NewClient(server.URL, "token", 5*time.Second)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	verdict := admission.Verdict{
		Suite: "guardrail", SuiteVersion: "1", ManifestDigest: strings.Repeat("a", 64),
		Passed: true, Runner: "sandbox://test", EvidenceRef: "s3://runs/1",
	}

	if err := client.Report(t.Context(), "acme-guard", "1.0.0", verdict); err != nil {
		t.Fatalf("Report: %v", err)
	}

	if gotPath != "/v1/components/acme-guard/1.0.0/admissions" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotBody["manifest_digest"] != verdict.ManifestDigest {
		t.Fatalf("body = %v", gotBody)
	}
	// The report itself is not sent; the control plane stores a reference.
	if _, sent := gotBody["report"]; sent {
		t.Fatalf("the full report was posted to the control plane: %v", gotBody)
	}
}

func TestAControlPlaneRefusalSurfaces(t *testing.T) {
	// The control plane rejects a verdict covering a different manifest. That
	// rejection must reach the operator rather than be swallowed.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"message":"different manifest"}}`))
	}))
	defer server.Close()

	client, _ := admission.NewClient(server.URL, "token", 5*time.Second)
	err := client.Report(t.Context(), "acme-guard", "1.0.0", admission.Verdict{})

	if err == nil || !strings.Contains(err.Error(), "different manifest") {
		t.Fatalf("err = %v", err)
	}
}

func TestTheManifestIsFetchedFromTheControlPlane(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name":"acme-guard","version":"1.0.0","port":"guardrail",
			"digest":"` + strings.Repeat("a", 64) + `","image":"` + pinned + `"}`))
	}))
	defer server.Close()

	client, _ := admission.NewClient(server.URL, "token", 5*time.Second)
	got, err := client.Manifest(t.Context(), "acme-guard", "1.0.0")
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}

	if got.Image != pinned || got.Port != "guardrail" {
		t.Fatalf("manifest = %+v", got)
	}
}

func TestAComponentThatBindsButDoesNotAnswerIsNotAFailingVerdict(t *testing.T) {
	// The socket existing only means the file was created. If nothing accepts
	// a connection, every case fails with the same dial error and the report
	// reads as though the component were broken — when the mount, the runtime
	// or the host is just as likely. Docker Desktop on macOS does exactly this
	// with a bind-mounted socket, which is how it was found.
	deaf := deafSandbox{}

	_, err := runner(t, deaf).Run(t.Context(), manifest(), fixtures())

	if err == nil {
		t.Fatal("an unreachable component produced a verdict")
	}
	if !strings.Contains(err.Error(), "does not answer") {
		t.Fatalf("err = %v, want it to name the reachability failure", err)
	}
}

// deafSandbox creates the socket file and never listens on it.
type deafSandbox struct{}

func (deafSandbox) Start(_ context.Context, spec sandbox.Spec) (*sandbox.Handle, error) {
	path := filepath.Join(spec.SocketDir, spec.SocketName)
	address, err := net.ResolveUnixAddr("unix", path)
	if err != nil {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", address)
	if err != nil {
		return nil, err
	}
	// Go unlinks the socket on Close by default. Keeping the file is the whole
	// point: a socket that exists with nothing behind it is exactly the state
	// an unsupported mount presents.
	listener.SetUnlinkOnClose(false)
	_ = listener.Close()

	info, err := os.Stat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return nil, errNotDeaf
	}
	return sandbox.NewHandle(path, func() { _ = os.Remove(path) }), nil
}

var errNotDeaf = errors.New("the fake sandbox failed to leave a dead socket behind")
