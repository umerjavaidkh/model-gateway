package gateway_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/adapters/echo"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/gateway"
)

var (
	pepper = []byte("a-test-pepper-that-is-long-enough")
	now    = time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

	routeEcho     = core.RoutingKey{BaseModel: "echo-model"}
	routeExternal = core.RoutingKey{BaseModel: "external-model"}
)

// snapshotOpts lets each test bend one thing about the fixture without
// rebuilding the whole world, which keeps the interesting difference visible in
// the test body rather than buried in a copy of the setup.
type snapshotOpts struct {
	principal   func(*core.Principal)
	tenantTier  core.TrustTier
	deployments []core.Deployment
	budgetSpent core.MicroUSD
}

func buildSnapshot(t *testing.T, opts snapshotOpts) *core.Snapshot {
	t.Helper()

	deployments := opts.deployments
	if deployments == nil {
		deployments = []core.Deployment{
			{ID: "echo-1", Key: routeEcho, Provider: "echo", TrustTier: core.TrustInternal, Weight: 100},
			{ID: "ext-1", Key: routeExternal, Provider: "echo", TrustTier: core.TrustExternal, Weight: 100},
		}
	}
	tier := opts.tenantTier
	if tier == core.TrustUnset {
		tier = core.TrustExternal
	}

	// core rejects an alias pointing at a model with no deployment, so the
	// fixture only declares "fast" when the case under test provides one.
	var aliases []core.ModelAlias
	for _, d := range deployments {
		if d.Key == routeEcho {
			aliases = []core.ModelAlias{{Name: "fast", Targets: []core.RoutingKey{routeEcho}}}
			break
		}
	}

	global, err := core.NewGlobalLayer(core.GlobalSpec{
		Version:        core.LayerVersion{Number: 1},
		Deployments:    deployments,
		Aliases:        aliases,
		TenantPrefixes: map[core.KeyPrefix]core.TenantID{"acme": "acme"},
	})
	if err != nil {
		t.Fatalf("NewGlobalLayer: %v", err)
	}

	principal := core.Principal{
		KeyID: "key-1", Tenant: "acme",
		Models:  core.ModelAllowlist{AllowAll: true},
		Budgets: []core.BudgetRef{{ID: "monthly", Scope: core.BudgetScopeOrg}},
	}
	if opts.principal != nil {
		opts.principal(&principal)
	}

	tenant, err := core.NewTenantLayer(core.TenantSpec{
		Tenant:  "acme",
		Version: core.LayerVersion{Number: 1},
		Tier:    "enterprise",
		Budgets: []core.BudgetState{{
			ID: "monthly", Scope: core.BudgetScopeOrg,
			LimitMicroUSD: 1_000_000, SpentMicroUSD: opts.budgetSpent, Hard: true,
		}},
		Principals:   []core.Principal{principal},
		Keys:         map[core.KeyLookup]core.KeyID{core.ComputeKeyLookup(pepper, "secret-1"): "key-1"},
		MinTrustTier: tier,
	})
	if err != nil {
		t.Fatalf("NewTenantLayer: %v", err)
	}

	snap, err := core.Compose(global, tenant)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	return snap
}

func buildPipeline(t *testing.T) *gateway.Pipeline {
	t.Helper()
	providers, err := gateway.NewStaticProviders(echo.New())
	if err != nil {
		t.Fatalf("NewStaticProviders: %v", err)
	}
	p, err := gateway.New(providers, gateway.NoCredentials{}, pepper,
		gateway.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func request(model, key string) *gateway.Request {
	return &gateway.Request{
		APIKey: key,
		Body:   []byte(`{"model":"` + model + `","messages":[]}`),
		Meta:   core.RequestMeta{RequestID: "req-1", Model: model, Endpoint: core.EndpointChatCompletions},
	}
}

func TestHappyPath(t *testing.T) {
	result, err := buildPipeline(t).Handle(t.Context(), buildSnapshot(t, snapshotOpts{}), request("echo-model", "gw_acme_secret-1"))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if result.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want 200", result.StatusCode)
	}
	if result.Deployment != "echo-1" {
		t.Fatalf("Deployment = %q, want echo-1", result.Deployment)
	}
	if result.Usage.Input == 0 {
		t.Fatal("usage was not carried back from the provider")
	}
}

func TestAliasResolvesToADeployment(t *testing.T) {
	result, err := buildPipeline(t).Handle(t.Context(), buildSnapshot(t, snapshotOpts{}), request("fast", "gw_acme_secret-1"))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if result.Route != routeEcho {
		t.Fatalf("Route = %v, want %v", result.Route, routeEcho)
	}
}

func TestAuthenticationFailures(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{name: "no key", key: ""},
		{name: "wrong scheme", key: "sk-1234"},
		{name: "no secret segment", key: "gw_acme"},
		{name: "unknown tenant prefix", key: "gw_nobody_secret-1"},
		{name: "wrong secret", key: "gw_acme_not-the-secret"},
	}

	pipeline := buildPipeline(t)
	snap := buildSnapshot(t, snapshotOpts{})
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pipeline.Handle(t.Context(), snap, request("echo-model", tc.key))
			if !errors.Is(err, core.ErrUnauthenticated) {
				t.Fatalf("err = %v, want unauthenticated", err)
			}
		})
	}
}

func TestAnUnknownPrefixIsIndistinguishableFromAWrongSecret(t *testing.T) {
	// Distinguishing them would turn the key prefix into an oracle for which
	// tenants exist on this gateway.
	pipeline, snap := buildPipeline(t), buildSnapshot(t, snapshotOpts{})

	_, unknownTenant := pipeline.Handle(t.Context(), snap, request("echo-model", "gw_nobody_secret-1"))
	_, wrongSecret := pipeline.Handle(t.Context(), snap, request("echo-model", "gw_acme_wrong"))

	if unknownTenant.Error() != wrongSecret.Error() {
		t.Fatalf("errors differ and leak tenant existence:\n  %v\n  %v", unknownTenant, wrongSecret)
	}
}

func TestExpiredKeyIsRejectedButDeprecatedKeyWorks(t *testing.T) {
	pipeline := buildPipeline(t)

	expired := buildSnapshot(t, snapshotOpts{principal: func(p *core.Principal) {
		p.NotAfter = now.Add(-time.Hour)
	}})
	if _, err := pipeline.Handle(t.Context(), expired, request("echo-model", "gw_acme_secret-1")); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("an expired key must be rejected, got %v", err)
	}

	// Two generations overlap during rotation, so the outgoing key still works.
	rotating := buildSnapshot(t, snapshotOpts{principal: func(p *core.Principal) {
		p.Deprecated = true
		p.NotAfter = now.Add(time.Hour)
	}})
	result, err := pipeline.Handle(t.Context(), rotating, request("echo-model", "gw_acme_secret-1"))
	if err != nil {
		t.Fatalf("a deprecated but unexpired key must work: %v", err)
	}
	if !result.Principal.Deprecated {
		t.Fatal("the result must carry the deprecation flag so the transport can warn the caller")
	}
}

func TestModelAllowlistIsEnforced(t *testing.T) {
	snap := buildSnapshot(t, snapshotOpts{principal: func(p *core.Principal) {
		p.Models = core.ModelAllowlist{Names: []string{"fast"}}
	}})

	if _, err := buildPipeline(t).Handle(t.Context(), snap, request("echo-model", "gw_acme_secret-1")); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("err = %v, want forbidden", err)
	}
	if _, err := buildPipeline(t).Handle(t.Context(), snap, request("fast", "gw_acme_secret-1")); err != nil {
		t.Fatalf("the permitted alias must work: %v", err)
	}
}

func TestExhaustedBudgetDenies(t *testing.T) {
	snap := buildSnapshot(t, snapshotOpts{budgetSpent: 1_000_000})
	if _, err := buildPipeline(t).Handle(t.Context(), snap, request("echo-model", "gw_acme_secret-1")); !errors.Is(err, core.ErrBudgetExhausted) {
		t.Fatalf("err = %v, want budget exhausted", err)
	}
}

func TestTrustTierIsPinnedBeforeAnyCandidateIsConsidered(t *testing.T) {
	// The design's most dangerous ordering bug: if a request that requires an
	// internal destination could reach an external one, payload redaction
	// computed for the first destination would be wrong for the second. Pinning
	// the tier for the whole candidate set makes that impossible.
	snap := buildSnapshot(t, snapshotOpts{
		tenantTier: core.TrustInternal,
		deployments: []core.Deployment{
			{ID: "ext-1", Key: routeExternal, Provider: "echo", TrustTier: core.TrustExternal, Weight: 100},
		},
	})

	_, err := buildPipeline(t).Handle(t.Context(), snap, request("external-model", "gw_acme_secret-1"))
	if !errors.Is(err, core.ErrTrustTierDenied) {
		t.Fatalf("err = %v, want trust tier denied — never a fallback to a lower tier", err)
	}
}

func TestPrincipalTrustTierRaisesTheTenantFloorButNeverLowersIt(t *testing.T) {
	// A key may be more restricted than its tenant; it may not be less.
	stricter := buildSnapshot(t, snapshotOpts{
		tenantTier: core.TrustExternal,
		principal:  func(p *core.Principal) { p.MinTrustTier = core.TrustInternal },
		deployments: []core.Deployment{
			{ID: "ext-1", Key: routeExternal, Provider: "echo", TrustTier: core.TrustExternal, Weight: 100},
		},
	})
	if _, err := buildPipeline(t).Handle(t.Context(), stricter, request("external-model", "gw_acme_secret-1")); !errors.Is(err, core.ErrTrustTierDenied) {
		t.Fatalf("a stricter principal must raise the floor, got %v", err)
	}

	looser := buildSnapshot(t, snapshotOpts{
		tenantTier: core.TrustInternal,
		principal:  func(p *core.Principal) { p.MinTrustTier = core.TrustExternal },
		deployments: []core.Deployment{
			{ID: "ext-1", Key: routeExternal, Provider: "echo", TrustTier: core.TrustExternal, Weight: 100},
		},
	})
	if _, err := buildPipeline(t).Handle(t.Context(), looser, request("external-model", "gw_acme_secret-1")); !errors.Is(err, core.ErrTrustTierDenied) {
		t.Fatalf("a looser principal must not lower the tenant floor, got %v", err)
	}
}

func TestRoutingFailuresAreDistinguished(t *testing.T) {
	// A typo, a capacity problem and a policy refusal mean different things to
	// the caller and map to different HTTP statuses.
	tests := []struct {
		name  string
		model string
		opts  snapshotOpts
		want  error
	}{
		{
			name: "unknown model", model: "no-such-model",
			want: core.ErrModelNotFound,
		},
		{
			name: "registered but not serving", model: "echo-model",
			opts: snapshotOpts{deployments: []core.Deployment{
				{ID: "echo-1", Key: routeEcho, Provider: "echo", TrustTier: core.TrustInternal, Weight: 0},
			}},
			want: core.ErrNoCandidates,
		},
		{
			name: "no deployment meets the trust tier", model: "external-model",
			opts: snapshotOpts{tenantTier: core.TrustInternal, deployments: []core.Deployment{
				{ID: "ext-1", Key: routeExternal, Provider: "echo", TrustTier: core.TrustExternal, Weight: 100},
			}},
			want: core.ErrTrustTierDenied,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts
			if opts.deployments == nil && tc.model != "no-such-model" {
				opts = snapshotOpts{}
			}
			_, err := buildPipeline(t).Handle(t.Context(), buildSnapshot(t, opts), request(tc.model, "gw_acme_secret-1"))
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestADeploymentWithNoRegisteredProviderIsSkipped(t *testing.T) {
	snap := buildSnapshot(t, snapshotOpts{deployments: []core.Deployment{
		{ID: "ghost", Key: routeEcho, Provider: "not-installed", TrustTier: core.TrustInternal, Weight: 100},
		{ID: "echo-1", Key: routeEcho, Provider: "echo", TrustTier: core.TrustInternal, Weight: 100},
	}})

	result, err := buildPipeline(t).Handle(t.Context(), snap, request("echo-model", "gw_acme_secret-1"))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if result.Deployment != "echo-1" {
		t.Fatalf("Deployment = %q, want the installed provider", result.Deployment)
	}
}

func TestNewRejectsAnEmptyPepper(t *testing.T) {
	// Starting with an empty pepper would make every key lookup computable by
	// anyone holding a snapshot.
	providers, _ := gateway.NewStaticProviders(echo.New())
	if _, err := gateway.New(providers, gateway.NoCredentials{}, nil); err == nil {
		t.Fatal("expected an empty pepper to be refused")
	}
}

func TestNoCredentialsRefusesADeploymentThatNeedsOne(t *testing.T) {
	// Returning an empty secret would surface as an upstream auth failure that
	// looks like the tenant's fault.
	_, err := gateway.NoCredentials{}.Resolve(context.Background(), "vault://acme/openai")
	if err == nil {
		t.Fatal("expected a deployment needing a credential to be refused")
	}
}

// providerOnly is a ProviderPort that serves exactly one API surface, so a test
// can exercise routing's endpoint filter without a real adapter.
type providerOnly struct {
	name     string
	endpoint core.Endpoint
}

func (p providerOnly) Name() string               { return p.name }
func (p providerOnly) Endpoints() []core.Endpoint { return []core.Endpoint{p.endpoint} }

func (providerOnly) Probe(context.Context, core.Deployment, core.Credential) error { return nil }

func (providerOnly) Invoke(context.Context, *core.ProviderCall) (*core.ProviderResponse, error) {
	return &core.ProviderResponse{StatusCode: 200, Body: []byte(`{}`)}, nil
}

func (providerOnly) Stream(context.Context, *core.ProviderCall) (core.ChunkStream, error) {
	return nil, core.New(core.CodeInternal, "not used in this test")
}

func TestRoutingSkipsAnAdapterThatCannotSpeakTheEndpoint(t *testing.T) {
	// Forwarding an Anthropic body to an OpenAI endpoint produces a confusing
	// upstream 400. Filtering here turns it into a gateway error that names the
	// real problem.
	providers, err := gateway.NewStaticProviders(
		providerOnly{name: "chat-only", endpoint: core.EndpointChatCompletions},
	)
	if err != nil {
		t.Fatalf("NewStaticProviders: %v", err)
	}
	p, err := gateway.New(providers, gateway.NoCredentials{}, pepper,
		gateway.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	snap := buildSnapshot(t, snapshotOpts{deployments: []core.Deployment{
		{ID: "chat-1", Key: routeEcho, Provider: "chat-only", TrustTier: core.TrustInternal, Weight: 100},
	}})

	req := request("echo-model", "gw_acme_secret-1")
	if _, err := p.Handle(t.Context(), snap, req); err != nil {
		t.Fatalf("the chat-completions surface must work: %v", err)
	}

	req = request("echo-model", "gw_acme_secret-1")
	req.Meta.Endpoint = core.EndpointMessages
	_, err = p.Handle(t.Context(), snap, req)
	if !errors.Is(err, core.ErrEndpointUnsupported) {
		t.Fatalf("err = %v, want endpoint_unsupported", err)
	}
}

func TestEndpointFilteringDoesNotMaskATrustTierRefusal(t *testing.T) {
	// The two refusals mean different things, and collapsing them would report
	// a data-residency violation as a missing feature.
	providers, err := gateway.NewStaticProviders(
		providerOnly{name: "chat-only", endpoint: core.EndpointChatCompletions},
	)
	if err != nil {
		t.Fatalf("NewStaticProviders: %v", err)
	}
	p, err := gateway.New(providers, gateway.NoCredentials{}, pepper,
		gateway.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	snap := buildSnapshot(t, snapshotOpts{
		tenantTier: core.TrustInternal,
		deployments: []core.Deployment{
			{ID: "ext-1", Key: routeExternal, Provider: "chat-only", TrustTier: core.TrustExternal, Weight: 100},
		},
	})

	req := request("external-model", "gw_acme_secret-1")
	req.Meta.Endpoint = core.EndpointMessages
	if _, err := p.Handle(t.Context(), snap, req); !errors.Is(err, core.ErrTrustTierDenied) {
		t.Fatalf("err = %v, want trust_tier_denied to win", err)
	}
}
