package gateway

import (
	"context"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

// StaticProviders is a fixed set of providers, keyed by name.
//
// It is what the worker uses until the component registry can bind providers
// per tenant from the snapshot. Being an implementation of the interface rather
// than the interface itself means that swap changes one line in main.
type StaticProviders struct {
	byName map[string]core.ProviderPort
}

// NewStaticProviders indexes the given providers by their reported name.
func NewStaticProviders(providers ...core.ProviderPort) (*StaticProviders, error) {
	byName := make(map[string]core.ProviderPort, len(providers))
	for _, p := range providers {
		name := p.Name()
		if name == "" {
			return nil, core.New(core.CodeInternal, "a provider reported an empty name")
		}
		if _, dup := byName[name]; dup {
			return nil, core.Newf(core.CodeInternal, "two providers named %q", name)
		}
		byName[name] = p
	}
	return &StaticProviders{byName: byName}, nil
}

// Provider looks up a provider by name.
func (s *StaticProviders) Provider(name string) (core.ProviderPort, bool) {
	p, ok := s.byName[name]
	return p, ok
}

// NoCredentials resolves every reference to an empty credential.
//
// It exists so the request path can run end to end before a secret store does.
// It refuses any non-empty reference rather than returning an empty secret for
// one: a deployment that asks for a credential and silently gets none would
// fail upstream with an authentication error that looks like the tenant's
// fault.
type NoCredentials struct{}

// Resolve returns an empty credential, or an error if one was actually wanted.
func (NoCredentials) Resolve(_ context.Context, ref string) (core.Credential, error) {
	if ref != "" {
		return core.Credential{}, core.Newf(core.CodeUnavailable,
			"deployment needs credential %q but no secret store is configured", ref)
	}
	return core.Credential{}, nil
}
