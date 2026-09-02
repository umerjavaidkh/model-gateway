package wasm_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/wasm"
)

// The guest is compiled once for the package. Building it here rather than
// committing a binary keeps the fixture and its source from drifting — a
// checked-in .wasm is a thing nobody rebuilds and nobody can read.
var (
	guestOnce  sync.Once
	guestBytes []byte
	guestErr   error
)

func guest(t *testing.T) []byte {
	t.Helper()

	guestOnce.Do(func() {
		out := filepath.Join(t.TempDir(), "guest.wasm")
		cmd := exec.CommandContext(context.Background(),
			"go", "build", "-buildmode=c-shared", "-o", out, ".")
		cmd.Dir = "testdata/guest"
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

var (
	defaultOnce   sync.Once
	defaultModule *wasm.Module
	defaultErr    error
)

// compile prepares a module under the given limits.
//
// The default-limits module is compiled once for the package, because that is
// the expensive step — hundreds of milliseconds — and a compiled module is
// reusable across calls, which is exactly how a worker holds one. Tests that
// need different limits pay for their own.
func compile(t *testing.T, limits wasm.Limits) *wasm.Module {
	t.Helper()

	if limits == (wasm.Limits{}) {
		defaultOnce.Do(func() {
			runtime, err := wasm.NewRuntime(context.Background(), limits)
			if err != nil {
				defaultErr = err
				return
			}
			defaultModule, defaultErr = runtime.Compile(context.Background(), "test-guest", guest(t))
		})
		if defaultErr != nil {
			t.Fatalf("preparing the module: %v", defaultErr)
		}
		return defaultModule
	}

	runtime, err := wasm.NewRuntime(t.Context(), limits)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })

	module, err := runtime.Compile(t.Context(), "test-guest", guest(t))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return module
}

func TestACallReachesTheGuestAndComesBack(t *testing.T) {
	output, err := compile(t, wasm.Limits{}).Call(t.Context(), []byte("hello"))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if string(output) != "saw:hello" {
		t.Fatalf("output = %q", output)
	}
}

func TestEachCallGetsAFreshInstance(t *testing.T) {
	// The property that makes running someone else's code in the worker's own
	// process defensible. Without it a component could stash one tenant's
	// payload and return it to another, and nothing outside the module could
	// see that happen.
	module := compile(t, wasm.Limits{})

	first, err := module.Call(t.Context(), []byte("stateful"))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	second, err := module.Call(t.Context(), []byte("stateful"))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if string(first) != "x" || string(second) != "x" {
		t.Fatalf("the guest counted across calls: %q then %q", first, second)
	}
}

func TestAGuestThatNeverReturnsIsStoppedByTheDeadline(t *testing.T) {
	// Nothing blocks unbudgeted. A guest loop would otherwise pin the
	// goroutine calling it for the life of the process.
	module := compile(t, wasm.Limits{})
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := module.Call(ctx, []byte("loop"))
		done <- err
	}()

	select {
	case err := <-done:
		if core.CodeOf(err) != core.CodeUpstreamTimeout {
			t.Fatalf("err = %v (code %s), want a timeout", err, core.CodeOf(err))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the guest was not interrupted; the runtime is not honouring the context")
	}
}

func TestAGuestClaimingAHugeResponseIsRefused(t *testing.T) {
	// The length is the guest's word. Without a cap, a component could answer
	// a small request by asking the host to read its entire address space.
	_, err := compile(t, wasm.Limits{}).Call(t.Context(), []byte("huge"))

	if err == nil {
		t.Fatal("a response over the limit was accepted")
	}
	if !strings.Contains(err.Error(), "over the") {
		t.Fatalf("err = %v, want it to name the limit", err)
	}
}

func TestAGuestPointingOutsideItsMemoryIsRefused(t *testing.T) {
	// It cannot reach the host either way — the read is bounds-checked, which
	// is why this is an error rather than a corruption.
	_, err := compile(t, wasm.Limits{}).Call(t.Context(), []byte("out-of-range"))

	if err == nil {
		t.Fatal("a response outside the guest's memory was accepted")
	}
}

func TestAGuestThatAllocatesWithoutBoundIsStopped(t *testing.T) {
	// Four gigabytes of allocation against a sixteen-megabyte limit. The
	// failure has to be this call's, not the worker's.
	module := compile(t, wasm.Limits{MemoryPages: 256})

	if _, err := module.Call(t.Context(), []byte("grow")); err == nil {
		t.Fatal("a guest exceeded its memory limit without failing")
	}

	// And the module still works afterwards: one greedy request must not take
	// the component out for everyone else.
	output, err := module.Call(t.Context(), []byte("after"))
	if err != nil {
		t.Fatalf("the module was unusable after a memory failure: %v", err)
	}
	if string(output) != "saw:after" {
		t.Fatalf("output = %q", output)
	}
}

func TestAnEmptyResponseIsNotAnError(t *testing.T) {
	// A guardrail with nothing to say is legitimate; the decoder above it is
	// what decides an empty body is unusable.
	output, err := compile(t, wasm.Limits{}).Call(t.Context(), []byte("empty"))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(output) != 0 {
		t.Fatalf("output = %q", output)
	}
}

func TestAModuleMissingTheABIIsRejectedAtCompileTime(t *testing.T) {
	// At load, when an operator is watching a deploy — not on the first
	// request that reaches it.
	runtime, err := wasm.NewRuntime(t.Context(), wasm.Limits{})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer func() { _ = runtime.Close(context.Background()) }()

	// The smallest valid module: a header and nothing else.
	empty := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}

	_, err = runtime.Compile(t.Context(), "empty", empty)
	if core.CodeOf(err) != core.CodeInvalidRequest {
		t.Fatalf("err = %v, want a refusal naming the missing export", err)
	}
	// Both, not the first: a publisher fixing an ABI mismatch wants to see
	// what is missing in one go rather than one build at a time.
	for _, name := range []string{wasm.AllocFunc, wasm.HandleFunc} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("err = %v, want it to name %q", err, name)
		}
	}
}

func TestAModuleTooLargeForTheLimitIsRefusedAtLoad(t *testing.T) {
	// At load, when an operator is watching a deploy — not on the first
	// request that reaches it.
	runtime, err := wasm.NewRuntime(t.Context(), wasm.Limits{MemoryPages: 2})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer func() { _ = runtime.Close(context.Background()) }()

	_, err = runtime.Compile(t.Context(), "cramped", guest(t))

	if core.CodeOf(err) != core.CodeInvalidRequest {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if !strings.Contains(err.Error(), "over limit") {
		t.Fatalf("err = %v, want it to name the limit", err)
	}
}

func TestAModuleThatCannotStartNamesTheLikelyCause(t *testing.T) {
	// A guest that compiles but needs to grow past the limit to initialise
	// fails with an opaque exit code from the runtime. An operator cannot act
	// on that without being told what it probably means.
	runtime, err := wasm.NewRuntime(t.Context(), wasm.Limits{MemoryPages: 32})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer func() { _ = runtime.Close(context.Background()) }()

	module, err := runtime.Compile(t.Context(), "cramped", guest(t))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	_, err = module.Call(t.Context(), []byte("hello"))

	if err == nil {
		t.Fatal("a module that cannot start returned a result")
	}
	if !strings.Contains(err.Error(), "may not be enough") {
		t.Fatalf("err = %v, want it to name the memory limit as a likely cause", err)
	}
}

func TestBytesThatAreNotWasmAreRejected(t *testing.T) {
	runtime, err := wasm.NewRuntime(t.Context(), wasm.Limits{})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer func() { _ = runtime.Close(context.Background()) }()

	if _, err := runtime.Compile(t.Context(), "junk", []byte("not wasm at all")); err == nil {
		t.Fatal("arbitrary bytes compiled")
	}
	if _, err := runtime.Compile(t.Context(), "empty", nil); err == nil {
		t.Fatal("an empty module compiled")
	}
	if _, err := runtime.Compile(t.Context(), "", []byte{0x00, 0x61, 0x73, 0x6d}); err == nil {
		t.Fatal("a nameless module compiled")
	}
}

func TestConcurrentCallsDoNotCollide(t *testing.T) {
	// Instances are anonymous for this reason: two concurrent calls would
	// otherwise collide on the module name and the second would fail.
	module := compile(t, wasm.Limits{})

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			want := "saw:" + strings.Repeat("a", i)
			got, err := module.Call(t.Context(), []byte(strings.Repeat("a", i)))
			if err != nil {
				errs <- err
				return
			}
			if string(got) != want {
				t.Errorf("output = %q, want %q", got, want)
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent call: %v", err)
	}
}

func BenchmarkCall(b *testing.B) {
	// The number that decides whether a fresh instance per call is affordable.
	// Recorded here so a future change to that policy has to argue with a
	// measurement rather than an intuition.
	runtime, err := wasm.NewRuntime(b.Context(), wasm.Limits{})
	if err != nil {
		b.Fatalf("NewRuntime: %v", err)
	}
	defer func() { _ = runtime.Close(context.Background()) }()

	out := filepath.Join(b.TempDir(), "guest.wasm")
	cmd := exec.CommandContext(context.Background(),
		"go", "build", "-buildmode=c-shared", "-o", out, ".")
	cmd.Dir = "testdata/guest"
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	if output, err := cmd.CombinedOutput(); err != nil {
		b.Fatalf("building the guest: %v\n%s", err, output)
	}
	wasmBytes, err := os.ReadFile(out)
	if err != nil {
		b.Fatalf("reading the guest: %v", err)
	}

	module, err := runtime.Compile(b.Context(), "bench", wasmBytes)
	if err != nil {
		b.Fatalf("Compile: %v", err)
	}

	b.ResetTimer()
	for b.Loop() {
		if _, err := module.Call(b.Context(), []byte("bench")); err != nil {
			b.Fatalf("Call: %v", err)
		}
	}
}
