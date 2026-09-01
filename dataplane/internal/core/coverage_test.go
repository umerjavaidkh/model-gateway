package core_test

import (
	"errors"
	"testing"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

// These cover the parts of the vocabulary that exist ahead of their consumers:
// capability filtering lands with the router, the event accessors with
// telemetry, WithGlobalLayer with the snapshot subscriber. They are in the
// plan rather than speculative, so testing them beats deleting them — and an
// untested accessor is exactly the kind of thing that is quietly wrong on the
// day the module that needs it arrives.

func TestDeploymentSupports(t *testing.T) {
	d := core.Deployment{Capabilities: []core.Capability{core.CapabilityStreaming, core.CapabilityVision}}

	if !d.Supports(core.CapabilityStreaming) {
		t.Fatal("a declared capability must be reported")
	}
	if !d.Supports(core.CapabilityStreaming, core.CapabilityVision) {
		t.Fatal("Supports must accept several at once")
	}
	if d.Supports(core.CapabilityStreaming, core.CapabilityToolCalling) {
		t.Fatal("Supports must be all-of, not any-of: a partial match is a wrong route")
	}
	if !d.Supports() {
		t.Fatal("requiring nothing must be satisfied by anything")
	}

	var none core.Deployment
	if none.Supports(core.CapabilityStreaming) {
		t.Fatal("a deployment declaring nothing must support nothing")
	}
}

func TestRoutingKeyIdentity(t *testing.T) {
	base := core.RoutingKey{BaseModel: "llama-3.3-70b"}
	adapter := core.RoutingKey{BaseModel: "llama-3.3-70b", AdapterID: "triage-v3"}

	if base.IsAdapter() || !adapter.IsAdapter() {
		t.Fatal("IsAdapter must distinguish a base model from a fine-tune")
	}
	if base.String() != "llama-3.3-70b" {
		t.Fatalf("base String = %q", base.String())
	}
	if adapter.String() != "llama-3.3-70b+triage-v3" {
		t.Fatalf("adapter String = %q", adapter.String())
	}
	// The two must not collide as map keys, or one tenant's fine-tune would
	// serve traffic meant for the base model.
	if base == adapter {
		t.Fatal("a base model and its adapter must be distinct routing keys")
	}
}

func TestTrustTierOrdering(t *testing.T) {
	// The ordering is what the router filters on, so it is load-bearing.
	if !core.TrustInternal.AtLeast(core.TrustExternal) {
		t.Fatal("internal must satisfy an external floor")
	}
	if core.TrustExternal.AtLeast(core.TrustInternal) {
		t.Fatal("external must not satisfy an internal floor")
	}
	if !core.TrustExternal.AtLeast(core.TrustExternal) {
		t.Fatal("AtLeast must be inclusive")
	}
	if core.TrustUnset.Valid() || !core.TrustPrivateCloud.Valid() {
		t.Fatal("only the real tiers are valid in a built snapshot")
	}

	for tier, want := range map[core.TrustTier]string{
		core.TrustUnset:        "unset",
		core.TrustExternal:     "external",
		core.TrustPrivateCloud: "private_cloud",
		core.TrustInternal:     "internal",
		core.TrustTier(99):     "unknown",
	} {
		if got := tier.String(); got != want {
			t.Fatalf("TrustTier(%d).String() = %q, want %q", tier, got, want)
		}
	}
}

func TestEventsAreASealedSetWithDistinctKinds(t *testing.T) {
	// Usage and audit are separate types on purpose: different consumers,
	// retention clocks and integrity requirements. The Event interface is what
	// keeps that distinction enforceable rather than conventional.
	at := time.Date(2026, 8, 31, 9, 0, 0, 0, time.UTC)
	events := []core.Event{
		core.UsageEvent{RequestID: "r1", Timestamp: at},
		core.AuditEvent{RequestID: "r1", Timestamp: at},
	}

	if events[0].Kind() != core.EventKindUsage || events[1].Kind() != core.EventKindAudit {
		t.Fatal("each event must report its own kind")
	}
	if events[0].Kind() == events[1].Kind() {
		t.Fatal("usage and audit must not share a kind")
	}
	for _, e := range events {
		if !e.OccurredAt().Equal(at) {
			t.Fatalf("OccurredAt = %v, want %v", e.OccurredAt(), at)
		}
	}
}

func TestErrorFormatsEveryCombination(t *testing.T) {
	// Every log line and every error response goes through this. All four
	// branches are reachable in production: bare sentinels, messages without a
	// cause, wrapped causes without a message, and both.
	cause := errors.New("connection refused")

	tests := []struct {
		name string
		err  *core.Error
		want string
	}{
		{name: "code only", err: core.ErrForbidden, want: "forbidden"},
		{name: "code and message", err: core.New(core.CodeForbidden, "not your model"),
			want: "forbidden: not your model"},
		{name: "code and cause", err: asError(t, core.Wrap(core.CodeUpstreamError, cause, "")),
			want: "upstream_error: connection refused"},
		{name: "code, message and cause", err: asError(t, core.Wrap(core.CodeUpstreamError, cause, "calling bedrock")),
			want: "upstream_error: calling bedrock: connection refused"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Fatalf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

func asError(t *testing.T, err error) *core.Error {
	t.Helper()
	var e *core.Error
	if !errors.As(err, &e) {
		t.Fatalf("expected a *core.Error, got %T", err)
	}
	return e
}

func TestErrorIsRejectsAForeignTarget(t *testing.T) {
	// A plain error whose text happens to read "forbidden" must not match the
	// sentinel: the whole point of matching by code is that it does not depend
	// on wording.
	foreign := errors.New("forbidden")
	if errors.Is(foreign, core.ErrForbidden) {
		t.Fatal("a foreign error must not match a sentinel")
	}
	if core.ErrForbidden.Is(foreign) {
		t.Fatal("matching must be by code, not by message")
	}
}

func TestCodeOfNil(t *testing.T) {
	if got := core.CodeOf(nil); got != "" {
		t.Fatalf("CodeOf(nil) = %q, want empty", got)
	}
}

func TestLayerVersionString(t *testing.T) {
	// This string is what the response header and every log line carry, so it
	// has to stay parseable.
	v := core.LayerVersion{Number: 42, Digest: "sha256:abc"}
	if got := v.String(); got != "42@sha256:abc" {
		t.Fatalf("String = %q", got)
	}
}

func TestLayerAccessors(t *testing.T) {
	snap := mustSnapshot(t)

	global, err := core.NewGlobalLayer(globalSpec())
	if err != nil {
		t.Fatalf("NewGlobalLayer: %v", err)
	}
	tenantLayer, err := core.NewTenantLayer(tenantSpec())
	if err != nil {
		t.Fatalf("NewTenantLayer: %v", err)
	}

	if global.Version().Number != 7 {
		t.Fatalf("GlobalLayer.Version = %v", global.Version())
	}
	if tenantLayer.Tenant() != "acme" || tenantLayer.Version().Number != 3 {
		t.Fatalf("TenantLayer = (%q, %v)", tenantLayer.Tenant(), tenantLayer.Version())
	}

	if snap.PolicyBundleRef() != "bundle-7" {
		t.Fatalf("PolicyBundleRef = %q", snap.PolicyBundleRef())
	}
	// Tier is the bounded-cardinality metrics label. An unknown tenant must
	// still produce a usable label rather than an empty one.
	if snap.Tier("acme") != "enterprise" || snap.Tier("nobody") != "unknown" {
		t.Fatalf("Tier = (%q, %q)", snap.Tier("acme"), snap.Tier("nobody"))
	}
	if ids := snap.TenantIDs(); len(ids) != 1 || ids[0] != "acme" {
		t.Fatalf("TenantIDs = %v", ids)
	}

	if d, ok := snap.Deployment("vllm-1"); !ok || d.Provider != "vllm" {
		t.Fatalf("Deployment = (%v, %v)", d, ok)
	}
	if _, ok := snap.Deployment("nope"); ok {
		t.Fatal("an unknown deployment id must not resolve")
	}
}

func TestWithGlobalLayerCarriesTenantsForward(t *testing.T) {
	// This is how the subscriber applies a new catalog without refetching every
	// tenant layer.
	snap := mustSnapshot(t)

	spec := globalSpec()
	spec.Version.Number = 8
	next, err := core.NewGlobalLayer(spec)
	if err != nil {
		t.Fatalf("NewGlobalLayer: %v", err)
	}
	updated, err := snap.WithGlobalLayer(next)
	if err != nil {
		t.Fatalf("WithGlobalLayer: %v", err)
	}

	if updated.GlobalVersion().Number != 8 {
		t.Fatalf("GlobalVersion = %d, want 8", updated.GlobalVersion().Number)
	}
	if _, ok := updated.TenantVersion("acme"); !ok {
		t.Fatal("the tenant layer must be carried forward, not dropped")
	}
	if snap.GlobalVersion().Number != 7 {
		t.Fatal("the snapshot it was derived from must be untouched")
	}
}

func TestWithGlobalLayerRejectsOneThatStrandsALiveTenant(t *testing.T) {
	// A global layer that drops a serving tenant's key prefix would leave that
	// tenant authenticating nobody. Better to refuse the swap.
	snap := mustSnapshot(t)

	spec := globalSpec()
	spec.Version.Number = 8
	spec.TenantPrefixes = map[core.KeyPrefix]core.TenantID{"other": "other"}
	next, err := core.NewGlobalLayer(spec)
	if err != nil {
		t.Fatalf("NewGlobalLayer: %v", err)
	}
	if _, err := snap.WithGlobalLayer(next); err == nil {
		t.Fatal("expected the swap to be refused")
	}
}

func TestWithTenantLayerRejectsNil(t *testing.T) {
	if _, err := mustSnapshot(t).WithTenantLayer(nil); err == nil {
		t.Fatal("expected a nil tenant layer to be refused")
	}
}
