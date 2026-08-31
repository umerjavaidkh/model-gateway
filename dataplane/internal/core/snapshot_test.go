package core_test

import (
	"testing"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

// The fixtures below build a small but realistic snapshot: one base model served
// by an internal vLLM pod and an external provider, plus a fine-tuned adapter on
// the internal pod, so that trust tiers and the (baseModel, adapterId) routing
// key are both exercised.

var (
	routeLlama   = core.RoutingKey{BaseModel: "llama-3.3-70b"}
	routeAdapter = core.RoutingKey{BaseModel: "llama-3.3-70b", AdapterID: "support-triage-v3"}
	routeGPT     = core.RoutingKey{BaseModel: "gpt-4o"}
)

func globalSpec() core.GlobalSpec {
	return core.GlobalSpec{
		Version: core.LayerVersion{Number: 7, Digest: "sha256:global"},
		Deployments: []core.Deployment{
			{ID: "vllm-1", Key: routeLlama, Provider: "vllm", TrustTier: core.TrustInternal, Weight: 100},
			{ID: "vllm-1-triage", Key: routeAdapter, Provider: "vllm", TrustTier: core.TrustInternal, Weight: 0},
			{ID: "openai-1", Key: routeGPT, Provider: "openai", TrustTier: core.TrustExternal, Weight: 100,
				Capabilities: []core.Capability{core.CapabilityStreaming, core.CapabilityVision}},
		},
		Aliases: []core.ModelAlias{
			{Name: "fast", Targets: []core.RoutingKey{routeGPT}},
		},
		TenantPrefixes: map[core.KeyPrefix]core.TenantID{"acme": "acme"},
		DefaultPlugins: []core.PluginBinding{
			{Port: core.PortGuardrail, Component: "regex-pii", Version: "1.0.0"},
		},
		PolicyBundleRef: "bundle-7",
	}
}

func tenantSpec() core.TenantSpec {
	return core.TenantSpec{
		Tenant:  "acme",
		Version: core.LayerVersion{Number: 3, Digest: "sha256:acme"},
		Tier:    "enterprise",
		Budgets: []core.BudgetState{
			{ID: "acme-monthly", Scope: core.BudgetScopeOrg, LimitMicroUSD: 1_000_000, Hard: true},
		},
		Principals: []core.Principal{
			{
				KeyID: "key-1", Tenant: "acme", Org: "acme",
				Models:  core.ModelAllowlist{AllowAll: true},
				Budgets: []core.BudgetRef{{ID: "acme-monthly", Scope: core.BudgetScopeOrg}},
			},
		},
		Keys:         map[core.KeyLookup]core.KeyID{lookupFor("secret-1"): "key-1"},
		MinTrustTier: core.TrustExternal,
	}
}

func lookupFor(secret string) core.KeyLookup {
	return core.ComputeKeyLookup([]byte("test-pepper"), secret)
}

func mustSnapshot(t *testing.T) *core.Snapshot {
	t.Helper()
	global, err := core.NewGlobalLayer(globalSpec())
	if err != nil {
		t.Fatalf("NewGlobalLayer: %v", err)
	}
	tenant, err := core.NewTenantLayer(tenantSpec())
	if err != nil {
		t.Fatalf("NewTenantLayer: %v", err)
	}
	snap, err := core.Compose(global, tenant)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	return snap
}

func TestAuthenticationIsThreeMapProbes(t *testing.T) {
	snap := mustSnapshot(t)

	prefix, secret, err := core.ParseAPIKey("gw_acme_secret-1")
	if err != nil {
		t.Fatalf("ParseAPIKey: %v", err)
	}
	tenant, ok := snap.TenantForPrefix(prefix)
	if !ok {
		t.Fatal("the key prefix must route to a tenant")
	}
	principal, ok := snap.Principal(tenant, core.ComputeKeyLookup([]byte("test-pepper"), secret))
	if !ok {
		t.Fatal("the key must resolve to a principal")
	}
	if principal.KeyID != "key-1" {
		t.Fatalf("KeyID = %q, want key-1", principal.KeyID)
	}
}

func TestUnknownKeyDoesNotResolve(t *testing.T) {
	snap := mustSnapshot(t)
	if _, ok := snap.Principal("acme", lookupFor("not-a-real-secret")); ok {
		t.Fatal("an unknown key must not resolve to a principal")
	}
	if _, ok := snap.TenantForPrefix("other"); ok {
		t.Fatal("an unknown prefix must not resolve to a tenant")
	}
}

func TestTenantAliasOverridesGlobal(t *testing.T) {
	// This is the composition rule the layering exists for: same binary, same
	// global catalog, different meaning of "fast" per tenant.
	snap := mustSnapshot(t)

	if got := snap.ResolveAlias("acme", "fast"); len(got) != 1 || got[0] != routeGPT {
		t.Fatalf("global alias resolved to %v, want [%v]", got, routeGPT)
	}

	spec := tenantSpec()
	spec.Version.Number = 4
	spec.AliasOverrides = []core.ModelAlias{{Name: "fast", Targets: []core.RoutingKey{routeLlama}}}
	override, err := core.NewTenantLayer(spec)
	if err != nil {
		t.Fatalf("NewTenantLayer: %v", err)
	}
	next, err := snap.WithTenantLayer(override)
	if err != nil {
		t.Fatalf("WithTenantLayer: %v", err)
	}

	if got := next.ResolveAlias("acme", "fast"); len(got) != 1 || got[0] != routeLlama {
		t.Fatalf("tenant override resolved to %v, want [%v]", got, routeLlama)
	}
	// The original snapshot is untouched: in-flight requests keep the version
	// they started on, which is what makes the swap safe without a lock.
	if got := snap.ResolveAlias("acme", "fast"); got[0] != routeGPT {
		t.Fatal("replacing a layer must not mutate the snapshot it was derived from")
	}
}

func TestUnknownNameResolvesToItself(t *testing.T) {
	// Callers may pass a concrete model instead of an alias.
	snap := mustSnapshot(t)
	got := snap.ResolveAlias("acme", "llama-3.3-70b")
	if len(got) != 1 || got[0] != routeLlama {
		t.Fatalf("resolved to %v, want [%v]", got, routeLlama)
	}
}

func TestAdapterIsASeparateRoutingTarget(t *testing.T) {
	// Multi-LoRA: the base model and its adapter are distinct routing keys served
	// by the same provider, and a pre-canary adapter sits at weight 0.
	snap := mustSnapshot(t)

	base := snap.Deployments(routeLlama)
	if len(base) != 1 || !base[0].Serving() {
		t.Fatalf("base model deployments = %v, want one serving", base)
	}
	adapter := snap.Deployments(routeAdapter)
	if len(adapter) != 1 || adapter[0].Serving() {
		t.Fatalf("adapter deployments = %v, want one not yet serving", adapter)
	}
}

func TestPluginBindingFallsBackToGlobal(t *testing.T) {
	snap := mustSnapshot(t)

	b, ok := snap.PluginBinding("acme", core.PortGuardrail)
	if !ok || b.Component != "regex-pii" {
		t.Fatalf("PluginBinding = (%v, %v), want the global default", b, ok)
	}

	spec := tenantSpec()
	spec.Version.Number = 4
	spec.Plugins = []core.PluginBinding{{Port: core.PortGuardrail, Component: "presidio", Version: "2.1.0"}}
	layer, err := core.NewTenantLayer(spec)
	if err != nil {
		t.Fatalf("NewTenantLayer: %v", err)
	}
	next, _ := snap.WithTenantLayer(layer)

	if b, _ := next.PluginBinding("acme", core.PortGuardrail); b.Component != "presidio" {
		t.Fatalf("tenant binding = %q, want presidio", b.Component)
	}
	// An unbound port is a valid state, not an error.
	if _, ok := next.PluginBinding("acme", core.PortTelemetry); ok {
		t.Fatal("an unbound port must report absent")
	}
}

func TestUnknownTenantGetsTheMostRestrictiveTrustTier(t *testing.T) {
	snap := mustSnapshot(t)
	if got := snap.MinTrustTier("acme"); got != core.TrustExternal {
		t.Fatalf("MinTrustTier = %v, want external", got)
	}
	if got := snap.MinTrustTier("nobody"); got != core.TrustInternal {
		t.Fatalf("MinTrustTier for an unknown tenant = %v, want internal (fail closed)", got)
	}
}

func TestDeniedBudgetWalksThePrincipalChain(t *testing.T) {
	spec := tenantSpec()
	spec.Budgets[0].SpentMicroUSD = spec.Budgets[0].LimitMicroUSD
	layer, err := core.NewTenantLayer(spec)
	if err != nil {
		t.Fatalf("NewTenantLayer: %v", err)
	}
	snap, _ := mustSnapshot(t).WithTenantLayer(layer)

	principal, _ := snap.Principal("acme", lookupFor("secret-1"))
	denied, ok := snap.DeniedBudget(&principal)
	if !ok || denied.ID != "acme-monthly" {
		t.Fatalf("DeniedBudget = (%v, %v), want the exhausted monthly budget", denied, ok)
	}
}

func TestGlobalLayerRejectsIncoherentSpecs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*core.GlobalSpec)
	}{
		{"zero version", func(s *core.GlobalSpec) { s.Version.Number = 0 }},
		{"duplicate deployment id", func(s *core.GlobalSpec) {
			s.Deployments = append(s.Deployments, s.Deployments[0])
		}},
		{"unset trust tier", func(s *core.GlobalSpec) { s.Deployments[0].TrustTier = core.TrustUnset }},
		{"weight above 100", func(s *core.GlobalSpec) { s.Deployments[0].Weight = 101 }},
		{"alias with no targets", func(s *core.GlobalSpec) { s.Aliases[0].Targets = nil }},
		{"alias targeting a model with no deployment", func(s *core.GlobalSpec) {
			s.Aliases[0].Targets = []core.RoutingKey{{BaseModel: "does-not-exist"}}
		}},
		{"two defaults on one port", func(s *core.GlobalSpec) {
			s.DefaultPlugins = append(s.DefaultPlugins, core.PluginBinding{Port: core.PortGuardrail, Component: "other"})
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := globalSpec()
			tc.mutate(&spec)
			if _, err := core.NewGlobalLayer(spec); err == nil {
				t.Fatal("expected the layer build to fail")
			}
		})
	}
}

func TestTenantLayerRejectsIncoherentSpecs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*core.TenantSpec)
	}{
		{"no tenant id", func(s *core.TenantSpec) { s.Tenant = "" }},
		{"zero version", func(s *core.TenantSpec) { s.Version.Number = 0 }},
		{"principal belongs to another tenant", func(s *core.TenantSpec) { s.Principals[0].Tenant = "other" }},
		{"dangling budget reference", func(s *core.TenantSpec) {
			s.Principals[0].Budgets = []core.BudgetRef{{ID: "ghost"}}
		}},
		{"key maps to an unknown principal", func(s *core.TenantSpec) {
			s.Keys = map[core.KeyLookup]core.KeyID{lookupFor("x"): "no-such-key"}
		}},
		{"two plugins on one port", func(s *core.TenantSpec) {
			s.Plugins = []core.PluginBinding{
				{Port: core.PortGuardrail, Component: "a"},
				{Port: core.PortGuardrail, Component: "b"},
			}
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := tenantSpec()
			tc.mutate(&spec)
			if _, err := core.NewTenantLayer(spec); err == nil {
				t.Fatal("expected the layer build to fail")
			}
		})
	}
}

func TestComposeRejectsATenantNoKeyRoutesTo(t *testing.T) {
	// Such a snapshot would build cleanly and then authenticate nobody, which is
	// far harder to diagnose than a failed build.
	spec := globalSpec()
	spec.TenantPrefixes = map[core.KeyPrefix]core.TenantID{"other": "other"}
	global, err := core.NewGlobalLayer(spec)
	if err != nil {
		t.Fatalf("NewGlobalLayer: %v", err)
	}
	tenant, err := core.NewTenantLayer(tenantSpec())
	if err != nil {
		t.Fatalf("NewTenantLayer: %v", err)
	}
	if _, err := core.Compose(global, tenant); err == nil {
		t.Fatal("expected Compose to reject an unroutable tenant")
	}
}

func TestComposeRejectsDuplicateTenantLayers(t *testing.T) {
	global, _ := core.NewGlobalLayer(globalSpec())
	a, _ := core.NewTenantLayer(tenantSpec())
	b, _ := core.NewTenantLayer(tenantSpec())
	if _, err := core.Compose(global, a, b); err == nil {
		t.Fatal("expected Compose to reject two layers for one tenant")
	}
}

func TestVersionsAreReportedPerLayer(t *testing.T) {
	snap := mustSnapshot(t)

	if got := snap.GlobalVersion().Number; got != 7 {
		t.Fatalf("GlobalVersion = %d, want 7", got)
	}
	v, ok := snap.TenantVersion("acme")
	if !ok || v.Number != 3 {
		t.Fatalf("TenantVersion = (%v, %v), want 3", v, ok)
	}
	if _, ok := snap.TenantVersion("nobody"); ok {
		t.Fatal("an unknown tenant must have no version")
	}
}
