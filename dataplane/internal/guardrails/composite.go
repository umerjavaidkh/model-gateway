package guardrails

import "github.com/umerjavaidkh/model-gateway/dataplane/internal/core"

// CompositeRegistry looks a guardrail up in several registries in order.
//
// A worker runs components from more than one place at once: guardrails
// compiled into the binary, WASM modules loaded from disk, and sidecars on
// sockets. They have different lifecycles — one is fixed at build time, one
// changes with a snapshot — so they are different registries, and this is what
// lets the chain see a single set.
//
// First match wins, and the order is the caller's. Putting the built-ins first
// means a component published under the same name as one of ours cannot
// displace it, which is the safer direction: the alternative is a registry
// entry silently overriding a guardrail an operator believes is running.
type CompositeRegistry struct {
	registries []Registry
}

// NewCompositeRegistry returns a registry that consults each in turn.
func NewCompositeRegistry(registries ...Registry) (*CompositeRegistry, error) {
	kept := make([]Registry, 0, len(registries))
	for _, registry := range registries {
		// A nil registry is how an optional one arrives — no module directory
		// configured, for instance — and skipping it here keeps that check out
		// of the caller.
		if registry != nil {
			kept = append(kept, registry)
		}
	}
	if len(kept) == 0 {
		return nil, core.New(core.CodeInternal, "a composite registry needs at least one registry")
	}
	return &CompositeRegistry{registries: kept}, nil
}

// Guardrail returns the first match.
func (c *CompositeRegistry) Guardrail(name string) (core.GuardrailPort, bool) {
	for _, registry := range c.registries {
		if guardrail, ok := registry.Guardrail(name); ok {
			return guardrail, true
		}
	}
	return nil, false
}
