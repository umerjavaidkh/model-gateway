// Package wasm runs an untrusted component inside the worker's own process.
//
// This is the second of the two isolation mechanisms in the component
// registry, and the trade against the first is worth stating plainly. A
// sidecar (ADR 0010) is isolated by the operating system, costs a Unix socket
// round trip, and can be written in anything. A WASM module is isolated by the
// runtime, costs a function call, and must be compiled to WASM — which is the
// right shape for the sub-millisecond work the design lists for in-process
// components: routing strategies, deterministic detectors, cost calculators.
//
// # What the boundary is
//
// A WASM module has no ambient authority whatsoever. It cannot open a file,
// dial a socket, read an environment variable, or see the host's memory: it
// addresses only its own linear memory, and the only way anything reaches it
// is through the bytes this package copies in. That is a stronger boundary
// than the container in internal/sandbox, and unlike that one it does not rest
// on a shared kernel.
//
// The runtime is given WASI because a Go-compiled guest's runtime needs clocks
// and entropy to start. Nothing else is granted: no filesystem, no environment,
// no arguments, no standard streams. A guest asking for a file gets the same
// answer as a guest asking for a network — there is nothing there.
//
// # A fresh instance per call
//
// Every call runs in a new instance, so a component cannot carry anything from
// one request into the next. That is what makes running someone else's code in
// the worker's own process defensible: without it, a component could stash one
// tenant's payload and return it to another, and nothing outside the module
// could see that happen.
//
// It costs about 1.5ms for a guest compiled from Go, most of it wazero
// reserving the module's linear memory rather than the guest's own start-up
// (BenchmarkCall measures both). A component that needs to be faster than that
// needs a leaner guest — a smaller initial memory — not a weaker boundary.
// A component whose declared latency budget it cannot meet fails at admission
// rather than in production, which is what the budget is for.
package wasm

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

// The ABI a component must implement.
//
// Two functions and a pair of integers, deliberately. A richer interface would
// mean host functions, and every host function is a capability granted to code
// nobody has vetted — the reason this boundary is worth having is that there
// are none.
const (
	// AllocFunc lets the host place a request in the guest's memory. The guest
	// owns its allocator; the host cannot safely guess a free address.
	AllocFunc = "alloc"
	// HandleFunc receives (ptr, len) of the request and returns the response
	// as ptr<<32|len. Packed into one i64 because a WASM function returns a
	// single value, and an out-parameter would need another allocation.
	HandleFunc = "handle"
	// InitFunc is the reactor entry point. A guest with a runtime — anything
	// compiled from Go — must run it before its exports are usable.
	InitFunc = "_initialize"
)

// Defaults sized for the work in-process components are meant to do. A module
// needing more than this is a module that belongs in a sidecar.
const (
	// DefaultMemoryPages caps the guest's linear memory at 64 MiB.
	DefaultMemoryPages = 1024
	// DefaultMaxOutputBytes caps what one call may return. The guest chooses
	// this length, so without a cap a component could answer a small request
	// by asking the host to read its entire address space.
	DefaultMaxOutputBytes = 8 << 20
	// DefaultCallTimeout bounds one call when the caller sets no deadline.
	DefaultCallTimeout = 5 * time.Second
)

// Runtime compiles and runs modules. Safe for concurrent use.
//
// One per process. Compilation is expensive — hundreds of milliseconds for a
// Go-compiled guest — and a compiled module is reusable across calls, so it
// happens once at load rather than per request.
type Runtime struct {
	runtime wazero.Runtime
	limits  Limits
}

// Limits bound what a module may do.
type Limits struct {
	// MemoryPages caps the guest's linear memory, in 64 KiB pages.
	MemoryPages uint32
	// MaxOutputBytes caps the response of a single call.
	MaxOutputBytes uint32
	// CallTimeout bounds a call when the caller's context has no earlier
	// deadline.
	CallTimeout time.Duration
}

func (l Limits) withDefaults() Limits {
	if l.MemoryPages == 0 {
		l.MemoryPages = DefaultMemoryPages
	}
	if l.MaxOutputBytes == 0 {
		l.MaxOutputBytes = DefaultMaxOutputBytes
	}
	if l.CallTimeout <= 0 {
		l.CallTimeout = DefaultCallTimeout
	}
	return l
}

// NewRuntime returns a runtime that modules can be compiled into.
func NewRuntime(ctx context.Context, limits Limits) (*Runtime, error) {
	limits = limits.withDefaults()

	config := wazero.NewRuntimeConfig().
		WithMemoryLimitPages(limits.MemoryPages).
		// Without this a guest loop never returns and the goroutine calling it
		// is stuck for the life of the process. The caller's deadline is what
		// stops it, which is the same rule guardrails already follow: nothing
		// blocks unbudgeted, and the budget is enforced by the caller rather
		// than trusted to the thing being budgeted.
		WithCloseOnContextDone(true)

	runtime := wazero.NewRuntimeWithConfig(ctx, config)
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, runtime); err != nil {
		_ = runtime.Close(ctx)
		return nil, core.Wrap(core.CodeInternal, err, "preparing the WASM runtime")
	}
	return &Runtime{runtime: runtime, limits: limits}, nil
}

// Close releases the runtime and every module compiled into it.
func (r *Runtime) Close(ctx context.Context) error {
	return r.runtime.Close(ctx)
}

// Module is a compiled component, ready to be called.
type Module struct {
	runtime   *Runtime
	compiled  wazero.CompiledModule
	name      string
	needsWASI bool
}

// Compile prepares a module and checks it implements the ABI.
//
// The exports are checked here rather than on first call, so a module that
// cannot possibly work is rejected at load — when an operator is watching a
// deploy — rather than on the first request that reaches it.
func (r *Runtime) Compile(ctx context.Context, name string, wasm []byte) (*Module, error) {
	if name == "" {
		return nil, core.New(core.CodeInvalidRequest, "a module needs a name")
	}
	if len(wasm) == 0 {
		return nil, core.New(core.CodeInvalidRequest, "a module needs bytes")
	}

	compiled, err := r.runtime.CompileModule(ctx, wasm)
	if err != nil {
		return nil, core.Wrapf(core.CodeInvalidRequest, err, "compiling %s", name)
	}

	exports := compiled.ExportedFunctions()
	var missing []string
	for _, required := range []string{AllocFunc, HandleFunc} {
		if _, ok := exports[required]; !ok {
			missing = append(missing, strconv.Quote(required))
		}
	}
	if len(missing) > 0 {
		// All of them, not the first: a publisher fixing an ABI mismatch wants
		// to see what is missing in one go rather than one build at a time.
		_ = compiled.Close(ctx)
		return nil, core.Newf(core.CodeInvalidRequest,
			"module %s does not export %s, so it cannot implement the component ABI",
			name, strings.Join(missing, " or "))
	}

	_, needsInit := exports[InitFunc]

	return &Module{runtime: r, compiled: compiled, name: name, needsWASI: needsInit}, nil
}

// Close releases the module.
func (m *Module) Close(ctx context.Context) error { return m.compiled.Close(ctx) }

// Name reports what the module was compiled as.
func (m *Module) Name() string { return m.name }

// Call runs the module once against input and returns what it produced.
//
// The instance is created and destroyed here. See the package comment for why
// that is not an optimisation waiting to happen.
func (m *Module) Call(ctx context.Context, input []byte) ([]byte, error) {
	limits := m.runtime.limits

	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, limits.CallTimeout)
		defer cancel()
	}

	config := wazero.NewModuleConfig().
		// Anonymous: two concurrent calls would otherwise collide on the name,
		// and the second would fail rather than get its own instance.
		WithName("").
		// No filesystem, no environment, no arguments, no standard streams.
		// Everything the guest can reach, it was handed.
		WithStartFunctions(startFunctions(m.needsWASI)...)

	instance, err := m.runtime.runtime.InstantiateModule(ctx, m.compiled, config)
	if err != nil {
		// A module whose declared minimum exceeds the limit is refused at
		// compile time with a clear message. What lands here instead is a
		// guest that needs to *grow* past the limit to start, which the
		// runtime reports as an opaque exit code — so the likely cause is
		// named rather than left for an operator to guess.
		return nil, core.Wrapf(core.CodeUnavailable,
			wrapGuestError(ctx, err, m.name, "instantiating"),
			"the module failed to start; %d pages (%d MiB) may not be enough for it",
			limits.MemoryPages, limits.MemoryPages/16)
	}
	defer func() {
		// Closing the instance is what frees its memory. A call that returns
		// without it leaks a linear memory per request, which for a 64 MiB
		// module is measured in seconds of uptime.
		_ = instance.Close(context.WithoutCancel(ctx))
	}()

	pointer, err := m.writeInput(ctx, instance, input)
	if err != nil {
		return nil, err
	}

	results, err := instance.ExportedFunction(HandleFunc).Call(ctx, uint64(pointer), uint64(len(input)))
	if err != nil {
		return nil, wrapGuestError(ctx, err, m.name, "calling")
	}
	if len(results) != 1 {
		return nil, core.Newf(core.CodeInternal,
			"module %s returned %d values from %s, want one", m.name, len(results), HandleFunc)
	}
	return m.readOutput(instance, results[0])
}

// writeInput asks the guest for space and copies the request into it.
func (m *Module) writeInput(
	ctx context.Context, instance api.Module, input []byte,
) (uint32, error) {
	if len(input) == 0 {
		// Nothing to copy, and asking a guest for a zero-length allocation is
		// a needlessly sharp edge to leave in an ABI.
		return 0, nil
	}

	results, err := instance.ExportedFunction(AllocFunc).Call(ctx, uint64(len(input)))
	if err != nil {
		return 0, wrapGuestError(ctx, err, m.name, "allocating in")
	}
	if len(results) != 1 {
		return 0, core.Newf(core.CodeInternal,
			"module %s returned %d values from %s, want one", m.name, len(results), AllocFunc)
	}

	pointer := api.DecodeU32(results[0])
	if !instance.Memory().Write(pointer, input) {
		// The guest returned an address outside its own memory. It cannot
		// reach the host either way — Write is bounds-checked, which is why
		// this is an error rather than a corruption — but it means the guest's
		// allocator is wrong and nothing it says can be relied on.
		return 0, core.Newf(core.CodeInvalidRequest,
			"module %s allocated %d bytes at an address outside its memory", m.name, len(input))
	}
	return pointer, nil
}

// readOutput reads the response the guest packed into one i64.
func (m *Module) readOutput(instance api.Module, packed uint64) ([]byte, error) {
	pointer := uint32(packed >> 32)        //nolint:gosec // the top half by construction
	length := uint32(packed & 0xFFFF_FFFF) //nolint:gosec // the bottom half by construction

	if length > m.runtime.limits.MaxOutputBytes {
		// The guest chooses this length, so it is attacker-controlled in the
		// case that matters: a component answering a small request by asking
		// the host to read its whole address space.
		return nil, core.Newf(core.CodeInvalidRequest,
			"module %s returned %d bytes, over the %d-byte limit",
			m.name, length, m.runtime.limits.MaxOutputBytes)
	}

	output, ok := instance.Memory().Read(pointer, length)
	if !ok {
		return nil, core.Newf(core.CodeInvalidRequest,
			"module %s returned a response outside its memory", m.name)
	}
	// Copied because the instance — and its memory — is closed on return.
	return append([]byte(nil), output...), nil
}

func startFunctions(needsWASI bool) []string {
	if needsWASI {
		return []string{InitFunc}
	}
	// A guest with no runtime to start. Naming a function that does not exist
	// would fail instantiation, so an empty list is the correct thing to pass.
	return nil
}

// wrapGuestError distinguishes a component that misbehaved from one that ran
// out of time, because only the second is the caller's own doing.
func wrapGuestError(ctx context.Context, err error, name, doing string) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return core.Wrapf(core.CodeUpstreamTimeout, ctxErr,
			"module %s exceeded its deadline while %s it", name, doing)
	}
	return core.Wrapf(core.CodeUnavailable, err, "%s module %s", doing, name)
}
