package wasmguardrail_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/adapters/wasmguardrail"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/wasm"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// registryWith builds a registry over a directory holding the test guest.
func registryWith(t *testing.T, module []byte) (*wasmguardrail.Registry, string) {
	t.Helper()

	sum := sha256.Sum256(module)
	digest := hex.EncodeToString(sum[:])
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, digest+".wasm"), module, 0o600); err != nil {
		t.Fatalf("writing the module: %v", err)
	}

	runtime, err := wasm.NewRuntime(t.Context(), wasm.Limits{})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })

	store, err := wasm.NewModuleStore(dir)
	if err != nil {
		t.Fatalf("NewModuleStore: %v", err)
	}
	registry, err := wasmguardrail.NewRegistry(runtime, store, quietLogger())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Close(context.Background()) })
	return registry, "sha256:" + digest
}

func binding(component, module string) core.GuardrailBinding {
	return core.GuardrailBinding{
		Component: component,
		Version:   "1.0.0",
		Execution: wasmguardrail.ExecutionInProcess,
		Module:    module,
	}
}

func TestSyncMakesABoundComponentAvailable(t *testing.T) {
	registry, digest := registryWith(t, guest(t))

	if _, ok := registry.Guardrail("acme-guard"); ok {
		t.Fatal("a component was available before anything bound it")
	}

	registry.Sync(t.Context(), []core.GuardrailBinding{binding("acme-guard", digest)})

	guardrail, ok := registry.Guardrail("acme-guard")
	if !ok {
		t.Fatal("the component is not available after Sync")
	}
	if guardrail.Name() != "acme-guard" {
		t.Fatalf("Name = %q", guardrail.Name())
	}
}

func TestLookupNeverCompiles(t *testing.T) {
	// Compiling on lookup would mean the first request after a configuration
	// change pays hundreds of milliseconds, and the request that pays it would
	// be an arbitrary tenant's.
	registry, digest := registryWith(t, guest(t))

	if _, ok := registry.Guardrail("never-synced"); ok {
		t.Fatal("a component nothing had loaded was returned")
	}
	// Even naming a module that exists on disk is not enough: only Sync loads.
	if _, ok := registry.Guardrail(digest); ok {
		t.Fatal("a module was resolved by digest rather than by binding")
	}
}

func TestOnlyInProcessBindingsAreLoaded(t *testing.T) {
	// A sidecar binding names an image, not a module. Trying to load one would
	// log an error on every snapshot for a component that is working fine.
	registry, digest := registryWith(t, guest(t))

	sidecar := binding("a-sidecar", digest)
	sidecar.Execution = "sidecar"
	registry.Sync(t.Context(), []core.GuardrailBinding{sidecar})

	if _, ok := registry.Guardrail("a-sidecar"); ok {
		t.Fatal("a sidecar binding was loaded as a WASM module")
	}
}

func TestSyncIsIdempotentAndSharesOneCompilationPerDigest(t *testing.T) {
	// Keyed by digest, so a component that changes version without changing
	// its module is not recompiled and two components shipping the same module
	// are compiled once.
	registry, digest := registryWith(t, guest(t))
	bindings := []core.GuardrailBinding{
		binding("first", digest),
		binding("second", digest),
	}

	registry.Sync(t.Context(), bindings)
	registry.Sync(t.Context(), bindings)

	for _, name := range []string{"first", "second"} {
		guardrail, ok := registry.Guardrail(name)
		if !ok {
			t.Fatalf("%s was not loaded", name)
		}
		result, err := guardrail.Inspect(t.Context(), input("harmless"))
		if err != nil {
			t.Fatalf("Inspect: %v", err)
		}
		if result.Verdict != core.VerdictAllow {
			t.Fatalf("%s returned %v", name, result.Verdict)
		}
	}
}

func TestAComponentWhoseModuleIsMissingIsLeftUnbound(t *testing.T) {
	// Refusing the whole configuration would make every rollout wait on file
	// distribution finishing first. The chain already handles a binding it
	// cannot resolve.
	registry, digest := registryWith(t, guest(t))
	absent := "sha256:" + strings.Repeat("e", 64)

	registry.Sync(t.Context(), []core.GuardrailBinding{
		binding("present", digest),
		binding("absent", absent),
	})

	if _, ok := registry.Guardrail("absent"); ok {
		t.Fatal("a component with no module was bound")
	}
	if _, ok := registry.Guardrail("present"); !ok {
		t.Fatal("one missing module prevented a working component from loading")
	}
}

func TestModuleBytesAreVerifiedBeforeCompiling(t *testing.T) {
	// The admission record vouches for these bytes and no others. A worker
	// that compiles whatever is at the expected path runs whatever an attacker
	// with write access to the volume put there.
	module := guest(t)
	sum := sha256.Sum256(module)
	digest := hex.EncodeToString(sum[:])

	dir := t.TempDir()
	// Right name, wrong bytes.
	if err := os.WriteFile(filepath.Join(dir, digest+".wasm"), append(module, 0x00), 0o600); err != nil {
		t.Fatalf("writing the module: %v", err)
	}

	runtime, err := wasm.NewRuntime(t.Context(), wasm.Limits{})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	store, _ := wasm.NewModuleStore(dir)
	registry, _ := wasmguardrail.NewRegistry(runtime, store, quietLogger())

	registry.Sync(t.Context(), []core.GuardrailBinding{binding("swapped", "sha256:"+digest)})

	if _, ok := registry.Guardrail("swapped"); ok {
		t.Fatal("a module whose bytes do not match the admitted digest was loaded")
	}
}

func TestARegistryNeedsARuntimeAndAStore(t *testing.T) {
	store, _ := wasm.NewModuleStore(t.TempDir())

	if _, err := wasmguardrail.NewRegistry(nil, store, nil); err == nil {
		t.Fatal("a registry with no runtime was accepted")
	}
	runtime, err := wasm.NewRuntime(t.Context(), wasm.Limits{})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })

	if _, err := wasmguardrail.NewRegistry(runtime, nil, nil); err == nil {
		t.Fatal("a registry with no module store was accepted")
	}
}
