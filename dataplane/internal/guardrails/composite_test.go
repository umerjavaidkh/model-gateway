package guardrails_test

import (
	"context"
	"testing"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/guardrails"
)

type named string

func (n named) Name() string { return string(n) }

func (n named) Inspect(context.Context, *core.GuardrailInput) (*core.GuardrailResult, error) {
	return &core.GuardrailResult{Verdict: core.VerdictAllow, Reason: string(n)}, nil
}

func registryOf(t *testing.T, names ...string) guardrails.Registry {
	t.Helper()

	ports := make([]core.GuardrailPort, 0, len(names))
	for _, name := range names {
		ports = append(ports, named(name))
	}
	registry, err := guardrails.NewStaticRegistry(ports...)
	if err != nil {
		t.Fatalf("NewStaticRegistry: %v", err)
	}
	return registry
}

func TestACompositeFindsGuardrailsInEveryRegistry(t *testing.T) {
	composite, err := guardrails.NewCompositeRegistry(
		registryOf(t, "builtin-one"), registryOf(t, "wasm-one"))
	if err != nil {
		t.Fatalf("NewCompositeRegistry: %v", err)
	}

	for _, name := range []string{"builtin-one", "wasm-one"} {
		if _, ok := composite.Guardrail(name); !ok {
			t.Fatalf("%s was not found", name)
		}
	}
	if _, ok := composite.Guardrail("nowhere"); ok {
		t.Fatal("an unbound name resolved")
	}
}

func TestTheFirstRegistryWins(t *testing.T) {
	// A published component sharing a name with a built-in must not displace
	// it: the alternative is a registry entry silently overriding a guardrail
	// an operator believes is running.
	composite, err := guardrails.NewCompositeRegistry(
		registryOf(t, "secret-scan"), registryOf(t, "secret-scan"))
	if err != nil {
		t.Fatalf("NewCompositeRegistry: %v", err)
	}

	guardrail, ok := composite.Guardrail("secret-scan")
	if !ok {
		t.Fatal("not found")
	}
	result, err := guardrail.Inspect(t.Context(), &core.GuardrailInput{})
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	// Both are named the same, so the reason is what distinguishes them; the
	// static registry returns the port it was given first.
	if result.Reason != "secret-scan" {
		t.Fatalf("reason = %q", result.Reason)
	}
}

func TestANilRegistryIsSkippedRatherThanCalled(t *testing.T) {
	// A nil registry is how an optional one arrives — no module directory
	// configured — and calling through it would panic on the first lookup.
	composite, err := guardrails.NewCompositeRegistry(nil, registryOf(t, "builtin-one"))
	if err != nil {
		t.Fatalf("NewCompositeRegistry: %v", err)
	}

	if _, ok := composite.Guardrail("builtin-one"); !ok {
		t.Fatal("a nil registry ahead of a real one hid it")
	}
}

func TestACompositeWithNothingInItIsRefused(t *testing.T) {
	if _, err := guardrails.NewCompositeRegistry(); err == nil {
		t.Fatal("an empty composite was accepted")
	}
	if _, err := guardrails.NewCompositeRegistry(nil, nil); err == nil {
		t.Fatal("a composite of nils was accepted")
	}
}
