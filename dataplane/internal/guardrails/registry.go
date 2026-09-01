package guardrails

import "github.com/umerjavaidkh/model-gateway/dataplane/internal/core"

// StaticRegistry is a fixed set of guardrails, keyed by the name a snapshot
// binds them under.
//
// What the worker uses until the component registry can install them at
// runtime. Being an implementation of the interface rather than the interface
// itself means that swap changes one line in main.
type StaticRegistry struct {
	byName map[string]core.GuardrailPort
}

// NewStaticRegistry indexes guardrails by their reported name.
func NewStaticRegistry(guardrails ...core.GuardrailPort) (*StaticRegistry, error) {
	byName := make(map[string]core.GuardrailPort, len(guardrails))
	for _, g := range guardrails {
		name := g.Name()
		if name == "" {
			return nil, core.New(core.CodeInternal, "a guardrail reported an empty name")
		}
		if _, dup := byName[name]; dup {
			return nil, core.Newf(core.CodeInternal, "two guardrails named %q", name)
		}
		byName[name] = g
	}
	return &StaticRegistry{byName: byName}, nil
}

// Guardrail looks one up by name.
func (r *StaticRegistry) Guardrail(name string) (core.GuardrailPort, bool) {
	g, ok := r.byName[name]
	return g, ok
}
