package wasmguardrail

import (
	"context"
	"log/slog"
	"sync"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/wasm"
)

// ExecutionInProcess is the execution mode this registry serves.
const ExecutionInProcess = "in_process"

// Registry supplies WASM guardrails to the chain, compiling them as snapshots
// bind them.
//
// Keyed by digest rather than by component name, so a component that changes
// version without changing its module is not recompiled, and two components
// that happen to ship the same module are compiled once. Compilation is
// hundreds of milliseconds; doing it per request, or per snapshot, would put
// that on the request path.
//
// Nothing is ever evicted. A worker binds a handful of components over its
// life and each compiled module is a few megabytes, so the cost of keeping
// them is small and the cost of getting eviction wrong — recompiling inside a
// request — is not. If that stops being true, the fix is an eviction policy
// with a measurement behind it, not a guess now.
type Registry struct {
	runtime *wasm.Runtime
	store   *wasm.ModuleStore
	logger  *slog.Logger

	mu       sync.RWMutex
	byDigest map[string]*wasm.Module
	byName   map[string]*Guardrail
}

// NewRegistry returns a registry that loads modules from store.
func NewRegistry(runtime *wasm.Runtime, store *wasm.ModuleStore, logger *slog.Logger) (*Registry, error) {
	if runtime == nil {
		return nil, core.New(core.CodeInternal, "a WASM registry needs a runtime")
	}
	if store == nil {
		return nil, core.New(core.CodeInternal, "a WASM registry needs a module store")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Registry{
		runtime:  runtime,
		store:    store,
		logger:   logger,
		byDigest: make(map[string]*wasm.Module),
		byName:   make(map[string]*Guardrail),
	}, nil
}

// Guardrail returns the component bound under name, if this registry has it.
//
// It never compiles: a miss is a miss. Compiling here would mean the first
// request after a configuration change pays hundreds of milliseconds, and the
// request that pays it would be an arbitrary tenant's.
func (r *Registry) Guardrail(name string) (core.GuardrailPort, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	guardrail, ok := r.byName[name]
	if !ok {
		return nil, false
	}
	return guardrail, true
}

// Sync compiles whatever the given bindings need and forgets nothing.
//
// Called when a snapshot is applied, off the request path. A component that
// fails to load is logged and left unbound rather than failing the whole
// snapshot: the guardrail chain already has to handle a binding it cannot
// resolve, and refusing an entire configuration because one component's
// artifact has not landed yet would make a rollout depend on file
// distribution finishing first.
func (r *Registry) Sync(ctx context.Context, bindings []core.GuardrailBinding) {
	for _, binding := range bindings {
		if binding.Execution != ExecutionInProcess {
			continue
		}
		if err := r.ensure(ctx, binding); err != nil {
			r.logger.Error("an in-process guardrail could not be loaded",
				slog.String("component", binding.Component),
				slog.String("module", binding.Module),
				slog.String("error", err.Error()))
		}
	}
}

// ensure compiles a binding's module if it is not already compiled.
func (r *Registry) ensure(ctx context.Context, binding core.GuardrailBinding) error {
	r.mu.RLock()
	existing, bound := r.byName[binding.Component]
	module, compiled := r.byDigest[binding.Module]
	r.mu.RUnlock()

	if bound && existing.module == module && compiled {
		return nil
	}

	if !compiled {
		// Verified against the digest the manifest carries, not trusted from a
		// path: the admission record vouches for these bytes and no others.
		wasmBytes, err := r.store.Load(binding.Module)
		if err != nil {
			return err
		}
		module, err = r.runtime.Compile(ctx, binding.Component, wasmBytes)
		if err != nil {
			return err
		}
	}

	guardrail, err := New(binding.Component, module)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	// Another Sync may have compiled the same digest while this one was
	// working. Keeping the first is arbitrary but consistent, and both are the
	// same bytes by construction.
	if kept, ok := r.byDigest[binding.Module]; ok {
		module = kept
		guardrail, err = New(binding.Component, module)
		if err != nil {
			return err
		}
	} else {
		r.byDigest[binding.Module] = module
	}
	r.byName[binding.Component] = guardrail

	r.logger.Info("in-process guardrail loaded",
		slog.String("component", binding.Component),
		slog.String("module", binding.Module))
	return nil
}

// Close releases every compiled module.
func (r *Registry) Close(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, module := range r.byDigest {
		_ = module.Close(ctx)
	}
	r.byDigest = make(map[string]*wasm.Module)
	r.byName = make(map[string]*Guardrail)
	return nil
}
