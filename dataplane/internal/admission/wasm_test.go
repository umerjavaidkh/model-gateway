package admission_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/admission"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/wasm"
)

var (
	guestOnce  sync.Once
	guestBytes []byte
	guestErr   error
)

// guest compiles the WASM guardrail the adapter's tests already use, so the
// runner is exercised against a component that a publisher could plausibly
// ship rather than against a stub written to pass.
func guest(t *testing.T) []byte {
	t.Helper()

	guestOnce.Do(func() {
		out := filepath.Join(t.TempDir(), "guest.wasm")
		cmd := exec.CommandContext(context.Background(),
			"go", "build", "-buildmode=c-shared", "-o", out, ".")
		cmd.Dir = "../adapters/wasmguardrail/testdata/guest"
		cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
		if output, err := cmd.CombinedOutput(); err != nil {
			guestErr = err
			t.Logf("building the guest: %v\n%s", err, output)
			return
		}
		guestBytes, guestErr = os.ReadFile(out)
	})
	if guestErr != nil {
		t.Fatalf("the test guest could not be built: %v", guestErr)
	}
	return guestBytes
}

// storeWith writes bytes into a module directory under their own digest and
// returns the directory and the reference a manifest would carry.
func storeWith(t *testing.T, module []byte) (*wasm.ModuleStore, string) {
	t.Helper()

	sum := sha256.Sum256(module)
	digest := hex.EncodeToString(sum[:])
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, digest+".wasm"), module, 0o600); err != nil {
		t.Fatalf("writing the module: %v", err)
	}

	store, err := wasm.NewModuleStore(dir)
	if err != nil {
		t.Fatalf("NewModuleStore: %v", err)
	}
	return store, "sha256:" + digest
}

func wasmManifest(module string) admission.Manifest {
	m := manifest()
	m.Execution = admission.ExecutionInProcess
	m.Image = ""
	m.Module = module
	return m
}

func TestAConformingWasmComponentPasses(t *testing.T) {
	store, digest := storeWith(t, guest(t))
	runner, err := admission.NewRunner("wasm://test", admission.WithModules(store))
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	verdict, err := runner.Run(t.Context(), wasmManifest(digest), fixtures())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !verdict.Passed {
		t.Fatalf("a conforming module failed:\n%s", verdict.Report)
	}
	// The same battery as a sidecar, recorded the same way. A component that
	// passed in one execution mode must not read differently from one that
	// passed in the other.
	if verdict.Suite != "guardrail" || verdict.ManifestDigest != manifest().Digest {
		t.Fatalf("verdict = %+v", verdict)
	}
}

func TestAdmittingAWasmComponentNeedsNoContainerRuntime(t *testing.T) {
	// Not needing one is most of the point of running a component in process.
	store, digest := storeWith(t, guest(t))

	runner, err := admission.NewRunner("wasm://test", admission.WithModules(store))
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	if _, err := runner.Run(t.Context(), wasmManifest(digest), fixtures()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestModuleBytesAreVerifiedAgainstTheManifest(t *testing.T) {
	// The whole guarantee. An admission record vouches for specific bytes; a
	// runner that compiles whatever is at the expected path runs whatever an
	// attacker with write access to the volume put there.
	store, digest := storeWith(t, guest(t))

	// Same digest, different bytes: a swapped artifact.
	hexDigest := strings.TrimPrefix(digest, "sha256:")
	path := filepath.Join(storeDir(t, store), hexDigest+".wasm")
	if err := os.WriteFile(path, append(guest(t), 0x00), 0o600); err != nil {
		t.Fatalf("swapping the module: %v", err)
	}

	_, err := store.Load(digest)

	if core.CodeOf(err) != core.CodeInvalidRequest {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if !strings.Contains(err.Error(), "not the admitted") {
		t.Fatalf("err = %v, want it to say the bytes are not what was admitted", err)
	}
}

func TestAMissingModuleIsAnErrorNotAVerdict(t *testing.T) {
	// "We could not test this" and "this failed its tests" are different facts.
	store, _ := storeWith(t, guest(t))
	absent := "sha256:" + strings.Repeat("f", 64)

	runner, err := admission.NewRunner("wasm://test", admission.WithModules(store))
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	if _, err := runner.Run(t.Context(), wasmManifest(absent), fixtures()); err == nil {
		t.Fatal("a missing module produced a verdict")
	}
}

func TestAModuleThatDoesNotImplementTheABIFailsRatherThanErrors(t *testing.T) {
	// The bytes are the thing under test and they are right here, so this is a
	// component that failed — not a run that could not happen.
	empty := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	store, digest := storeWith(t, empty)

	runner, err := admission.NewRunner("wasm://test", admission.WithModules(store))
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	verdict, err := runner.Run(t.Context(), wasmManifest(digest), fixtures())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if verdict.Passed {
		t.Fatal("a module with no ABI was admitted")
	}
	if !strings.Contains(verdict.Report, "ABI") {
		t.Fatalf("report does not name the problem: %s", verdict.Report)
	}
}

func TestAnInProcessComponentNeedsAModuleStore(t *testing.T) {
	// Refused rather than skipped: a component nobody can test is not a
	// component that passed.
	runner, err := admission.NewRunner("sandbox://test",
		admission.WithSandbox(fakeSandbox{handler: component()}))
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	_, err = runner.Run(t.Context(), wasmManifest("sha256:"+strings.Repeat("a", 64)), fixtures())

	if err == nil {
		t.Fatal("an in-process component was admitted without a module store")
	}
}

func TestAnUnknownExecutionModeIsRefused(t *testing.T) {
	// Mapping it onto a default would run a component under isolation the
	// publisher did not ask for.
	store, digest := storeWith(t, guest(t))
	spec := wasmManifest(digest)
	spec.Execution = "kubernetes-job"

	runner, err := admission.NewRunner("wasm://test", admission.WithModules(store))
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	if _, err := runner.Run(t.Context(), spec, fixtures()); err == nil {
		t.Fatal("an unknown execution mode was run anyway")
	}
}

func TestAReferenceThatIsNotADigestIsRefused(t *testing.T) {
	store, _ := storeWith(t, guest(t))

	for _, reference := range []string{
		"", "v1.2.3", "latest",
		"sha256:short",
		"sha256:" + strings.Repeat("z", 64), // not hexadecimal
		"md5:" + strings.Repeat("a", 64),
		// A path traversal, if the reference reached the filesystem unchecked.
		"sha256:../../../../etc/passwd",
	} {
		if _, err := store.Load(reference); err == nil {
			t.Errorf("accepted %q as a module reference", reference)
		}
	}
}

func TestAModuleStoreNeedsADirectory(t *testing.T) {
	if _, err := wasm.NewModuleStore(""); err == nil {
		t.Fatal("a store with no directory would fail every load while looking configured")
	}
}

// storeDir recovers the directory a store was built with, so a test can put
// something unexpected in it.
func storeDir(t *testing.T, store *wasm.ModuleStore) string {
	t.Helper()

	dir := store.Dir()
	if dir == "" {
		t.Fatal("the store reported no directory")
	}
	return dir
}
