package sandbox_test

import (
	"slices"
	"testing"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/sandbox"
)

// These assert the argv rather than observed container behaviour, deliberately.
// The flags *are* the boundary, and a test that needs a container runtime
// installed is a test that does not run — which is how a dropped flag survives
// a review and ships.

const pinned = "ghcr.io/acme/guard@sha256:" + "ab12" + "0000000000000000000000000000000000000000000000000000000000"

func args(t *testing.T, spec sandbox.Spec) []string {
	t.Helper()

	got, err := sandbox.New().Args(spec, "gw-admission-test")
	if err != nil {
		t.Fatalf("Args: %v", err)
	}
	return got
}

func validSpec() sandbox.Spec {
	return sandbox.Spec{Image: pinned, SocketDir: "/tmp/sock", SocketName: "component.sock"}
}

func TestTheIsolationFlagsArePassed(t *testing.T) {
	got := args(t, validSpec())

	for _, want := range []string{
		// No network: a component that fetches its real behaviour at startup
		// would otherwise pass a suite that tested something else.
		"--network=none",
		// Nothing written to the host outside the socket mount.
		"--read-only",
		// No capability can be gained, including through a setuid binary in
		// the component's own image.
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges",
		// Removed when it exits, whatever it did.
		"--rm",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %s in %v", want, got)
		}
	}
}

func TestResourceCapsArePassed(t *testing.T) {
	got := args(t, validSpec())

	for flag, want := range map[string]string{
		"--memory":      "512m",
		"--memory-swap": "512m", // equal to --memory, so it cannot swap instead
		"--cpus":        "1.0",
		"--pids-limit":  "64",
		"--user":        "65534:65534",
	} {
		if value := valueAfter(got, flag); value != want {
			t.Errorf("%s = %q, want %q", flag, value, want)
		}
	}
}

func TestLimitsAreOverridable(t *testing.T) {
	spec := validSpec()
	spec.Limits = sandbox.Limits{MemoryMB: 128, CPUs: "0.5", PidsLimit: 16, Timeout: time.Minute}

	got := args(t, spec)

	if value := valueAfter(got, "--memory"); value != "128m" {
		t.Fatalf("--memory = %q", value)
	}
	if value := valueAfter(got, "--pids-limit"); value != "16" {
		t.Fatalf("--pids-limit = %q", value)
	}
}

func TestTheComponentIsToldWhereToBind(t *testing.T) {
	// The component cannot guess the mount point, and two ways of saying the
	// same thing is one of them being wrong.
	got := args(t, validSpec())

	if value := valueAfter(got, "--env"); value != "COMPONENT_SOCKET=/run/component/component.sock" {
		t.Fatalf("--env = %q", value)
	}
	if value := valueAfter(got, "--volume"); value != "/tmp/sock:/run/component:rw" {
		t.Fatalf("--volume = %q", value)
	}
}

func TestTheImageIsTheLastArgument(t *testing.T) {
	// Anything after it is an argument to the container, not to the runtime —
	// a flag that drifts past the image silently stops being an isolation flag.
	got := args(t, validSpec())

	if got[len(got)-1] != pinned {
		t.Fatalf("last argument = %q, want the image", got[len(got)-1])
	}
}

func TestAnImagePinnedByTagIsRefused(t *testing.T) {
	// The registry already refuses this on the manifest. Checked again because
	// this is the process that actually runs the bytes, and a boundary that
	// trusts its caller to have validated is not one.
	spec := validSpec()
	spec.Image = "ghcr.io/acme/guard:latest"

	if _, err := sandbox.New().Args(spec, "n"); core.CodeOf(err) != core.CodeInvalidRequest {
		t.Fatalf("err = %v, want a refusal", err)
	}
}

func TestASocketNameThatIsAPathIsRefused(t *testing.T) {
	// "../../etc/x" as a socket name would escape the mount.
	spec := validSpec()
	spec.SocketName = "../escape.sock"

	if _, err := sandbox.New().Args(spec, "n"); err == nil {
		t.Fatal("a socket name containing a path separator was accepted")
	}
}

func TestAnEmptySpecIsRefused(t *testing.T) {
	for _, spec := range []sandbox.Spec{
		{},
		{Image: pinned},
		{Image: pinned, SocketDir: "/tmp/sock"},
	} {
		if _, err := sandbox.New().Args(spec, "n"); err == nil {
			t.Fatalf("accepted an incomplete spec: %+v", spec)
		}
	}
}

func TestTheRuntimeIsConfigurable(t *testing.T) {
	// A deployment admitting genuinely untrusted code points this at a
	// VM-isolated runtime; the package must not hardcode which.
	box := sandbox.New(sandbox.WithRuntime("gvisor-runsc"))

	if _, err := box.Args(validSpec(), "n"); err != nil {
		t.Fatalf("Args: %v", err)
	}
}

func TestStartingAnUnknownRuntimeFailsRatherThanHangs(t *testing.T) {
	box := sandbox.New(sandbox.WithRuntime("definitely-not-a-real-runtime"))
	spec := validSpec()
	spec.SocketDir = t.TempDir()
	spec.Limits = sandbox.Limits{Timeout: 2 * time.Second}

	if _, err := box.Start(t.Context(), spec); err == nil {
		t.Fatal("starting a nonexistent runtime reported success")
	}
}

func valueAfter(args []string, flag string) string {
	index := slices.Index(args, flag)
	if index < 0 || index+1 >= len(args) {
		return ""
	}
	return args[index+1]
}
