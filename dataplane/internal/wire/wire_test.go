package wire_test

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/wire"
	pb "github.com/umerjavaidkh/model-gateway/dataplane/internal/wire/gatewayv1"
)

var builtAt = time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)

// fullGlobalSpec deliberately populates every field. A round-trip test only
// proves what it exercises, so a field left at its zero value here is a field
// whose loss the test cannot detect.
func fullGlobalSpec() core.GlobalSpec {
	return core.GlobalSpec{
		Version: core.LayerVersion{Number: 12, Digest: "sha256:abc"},
		BuiltAt: builtAt,
		Deployments: []core.Deployment{
			{
				ID:            "vllm-1",
				Key:           core.RoutingKey{BaseModel: "llama-3.3-70b", AdapterID: "triage-v3"},
				Provider:      "vllm",
				Endpoint:      "http://vllm.internal:8000",
				Region:        "me-central-1",
				TrustTier:     core.TrustInternal,
				CredentialRef: "vault://acme/vllm",
				Weight:        100,
				Cost:          core.Cost{InputPer1K: 120, OutputPer1K: 360},
				Capabilities:  []core.Capability{core.CapabilityStreaming, core.CapabilityToolCalling},
			},
		},
		Aliases: []core.ModelAlias{
			{Name: "fast", Targets: []core.RoutingKey{{BaseModel: "llama-3.3-70b", AdapterID: "triage-v3"}}},
		},
		TenantPrefixes:  map[core.KeyPrefix]core.TenantID{"acme": "acme", "globex": "globex"},
		DefaultPlugins:  []core.PluginBinding{{Port: core.PortGuardrail, Component: "regex-pii", Version: "1.0.0", ConfigRef: "cfg://1"}},
		PolicyBundleRef: "bundle-12",
	}
}

func fullTenantSpec() core.TenantSpec {
	lookup := core.ComputeKeyLookup([]byte("pepper"), "secret-1")
	return core.TenantSpec{
		Tenant:  "acme",
		Version: core.LayerVersion{Number: 4, Digest: "sha256:def"},
		BuiltAt: builtAt,
		Tier:    "enterprise",
		Principals: []core.Principal{{
			KeyID: "key-1", Tenant: "acme", Org: "acme-org", Team: "platform",
			User: "u-1", App: "app-1",
			Roles:         []core.Role{"admin", "billing"},
			Models:        core.ModelAllowlist{Names: []string{"fast", "cheap"}},
			Budgets:       []core.BudgetRef{{ID: "monthly", Scope: core.BudgetScopeOrg}},
			DefaultClass:  core.DataClassConfidential,
			MinTrustTier:  core.TrustPrivateCloud,
			MaxConcurrent: 32,
			Deprecated:    true,
			NotAfter:      builtAt.Add(24 * time.Hour),
		}},
		Keys: map[core.KeyLookup]core.KeyID{lookup: "key-1"},
		AliasOverrides: []core.ModelAlias{
			{Name: "fast", Targets: []core.RoutingKey{{BaseModel: "gpt-4o"}}},
		},
		Budgets: []core.BudgetState{{
			ID: "monthly", Scope: core.BudgetScopeOrg,
			LimitMicroUSD: 5_000_000, SpentMicroUSD: 1_250_000,
			Hard: true, HeadroomBasisPoints: 500,
		}},
		Plugins:      []core.PluginBinding{{Port: core.PortGuardrail, Component: "presidio", Version: "2.1.0"}},
		MinTrustTier: core.TrustExternal,
	}
}

func TestGlobalLayerSurvivesARoundTrip(t *testing.T) {
	want := fullGlobalSpec()
	got := wire.DecodeGlobal(wire.EncodeGlobal(want))

	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("global spec changed across the wire (-want +got):\n%s", diff)
	}
}

func TestTenantLayerSurvivesARoundTrip(t *testing.T) {
	want := fullTenantSpec()
	got := wire.DecodeTenant(wire.EncodeTenant(want))

	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("tenant spec changed across the wire (-want +got):\n%s", diff)
	}
}

func TestZeroTimeStaysZero(t *testing.T) {
	// Zero means "unset" on both sides. Encoding it as epoch would turn "no
	// expiry" into "expired in 1970", which fails closed but for the wrong
	// reason and is baffling to debug.
	spec := core.TenantSpec{Tenant: "acme", Version: core.LayerVersion{Number: 1}}
	got := wire.DecodeTenant(wire.EncodeTenant(spec))

	if !got.BuiltAt.IsZero() {
		t.Fatalf("BuiltAt = %v, want the zero time", got.BuiltAt)
	}
}

func TestDecodeSnapshotProducesAServableSnapshot(t *testing.T) {
	msg := &pb.Snapshot{
		GlobalLayer: wire.EncodeGlobal(fullGlobalSpec()),
		Tenants:     []*pb.TenantLayer{wire.EncodeTenant(fullTenantSpec())},
	}

	snap, err := wire.DecodeSnapshot(msg)
	if err != nil {
		t.Fatalf("DecodeSnapshot: %v", err)
	}

	// Spot-check that the decoded snapshot answers the questions the request
	// path actually asks, rather than merely parsing.
	tenant, ok := snap.TenantForPrefix("acme")
	if !ok || tenant != "acme" {
		t.Fatalf("TenantForPrefix = (%q, %v), want acme", tenant, ok)
	}
	principal, ok := snap.Principal(tenant, core.ComputeKeyLookup([]byte("pepper"), "secret-1"))
	if !ok || principal.KeyID != "key-1" {
		t.Fatalf("Principal = (%v, %v), want key-1", principal.KeyID, ok)
	}
	if got := snap.ResolveAlias(tenant, "fast"); len(got) != 1 || got[0].BaseModel != "gpt-4o" {
		t.Fatalf("the tenant alias override did not survive decoding: %v", got)
	}
	if b, _ := snap.PluginBinding(tenant, core.PortGuardrail); b.Component != "presidio" {
		t.Fatalf("plugin binding = %q, want presidio", b.Component)
	}
}

func TestDecodeSnapshotRejectsAnIncoherentPayload(t *testing.T) {
	// Validation is core's, and the wire layer must not weaken it. A deployment
	// with no trust tier would otherwise be routable to by anything.
	global := wire.EncodeGlobal(fullGlobalSpec())
	global.Deployments[0].TrustTier = pb.TrustTier_TRUST_TIER_UNSPECIFIED

	if _, err := wire.DecodeSnapshot(&pb.Snapshot{GlobalLayer: global}); err == nil {
		t.Fatal("expected the decode to fail core's validation")
	}
}

func TestDecodeSnapshotRejectsAMissingGlobalLayer(t *testing.T) {
	if _, err := wire.DecodeSnapshot(&pb.Snapshot{}); err == nil {
		t.Fatal("a snapshot with no global layer must be rejected")
	}
	if _, err := wire.DecodeSnapshot(nil); err == nil {
		t.Fatal("a nil snapshot must be rejected")
	}
}

func TestAnUnknownEnumValueFailsTheLayer(t *testing.T) {
	// A newer control plane can send a trust tier this worker has never heard
	// of. It must fail the layer, not fall back to the most permissive value.
	global := wire.EncodeGlobal(fullGlobalSpec())
	global.Deployments[0].TrustTier = pb.TrustTier(9999)

	if _, err := wire.DecodeSnapshot(&pb.Snapshot{GlobalLayer: global}); err == nil {
		t.Fatal("an unknown trust tier must fail the layer rather than default")
	}
}

func TestAnOutOfRangeWeightFailsTheLayer(t *testing.T) {
	// Weight is uint32 on the wire and uint8 in the domain. A naive cast would
	// turn 256 into 0 and 300 into 44 — both plausible-looking.
	global := wire.EncodeGlobal(fullGlobalSpec())
	global.Deployments[0].Weight = 300

	if _, err := wire.DecodeSnapshot(&pb.Snapshot{GlobalLayer: global}); err == nil {
		t.Fatal("an out-of-range weight must fail the layer")
	}
}

func TestMarshalIsDeterministic(t *testing.T) {
	// Map fields serialize in arbitrary order by default. Without deterministic
	// mode the digest differs between producer and verifier at random, and
	// layers are rejected for no reason.
	msg := wire.EncodeGlobal(fullGlobalSpec())

	first, err := wire.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for range 50 {
		next, err := wire.Marshal(msg)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if string(next) != string(first) {
			t.Fatal("marshalling the same layer twice produced different bytes")
		}
	}
}

func TestSealAndVerify(t *testing.T) {
	msg := wire.EncodeGlobal(fullGlobalSpec())
	if err := wire.SealGlobal(msg); err != nil {
		t.Fatalf("SealGlobal: %v", err)
	}
	if err := wire.VerifyGlobal(msg); err != nil {
		t.Fatalf("a freshly sealed layer must verify: %v", err)
	}

	// The digest must cover the payload, not just the header.
	msg.Deployments[0].Weight = 50
	if err := wire.VerifyGlobal(msg); err == nil {
		t.Fatal("a tampered layer must fail verification")
	}
}

func TestSealIsStableAcrossSerializations(t *testing.T) {
	// The digest is computed with the digest field cleared, so sealing an
	// already-sealed layer must produce the same value rather than hashing the
	// previous hash.
	a := wire.EncodeGlobal(fullGlobalSpec())
	if err := wire.SealGlobal(a); err != nil {
		t.Fatalf("SealGlobal: %v", err)
	}
	first := a.GetVersion().GetDigest()

	if err := wire.SealGlobal(a); err != nil {
		t.Fatalf("SealGlobal: %v", err)
	}
	if a.GetVersion().GetDigest() != first {
		t.Fatal("re-sealing changed the digest")
	}
}

func TestVerifyPassesOnAnUnsealedLayer(t *testing.T) {
	// Digests are optional: a control plane that does not stamp them is not
	// broken, it just gives up the corruption check.
	msg := wire.EncodeGlobal(fullGlobalSpec())
	msg.GetVersion().Digest = ""
	if err := wire.VerifyGlobal(msg); err != nil {
		t.Fatalf("an unsealed layer must verify: %v", err)
	}
}

func TestTenantSealAndVerify(t *testing.T) {
	msg := wire.EncodeTenant(fullTenantSpec())
	if err := wire.SealTenant(msg); err != nil {
		t.Fatalf("SealTenant: %v", err)
	}
	if err := wire.VerifyTenant(msg); err != nil {
		t.Fatalf("VerifyTenant: %v", err)
	}
	msg.Tier = "free"
	if err := wire.VerifyTenant(msg); err == nil {
		t.Fatal("a tampered tenant layer must fail verification")
	}
}

func TestUnmarshalRejectsGarbage(t *testing.T) {
	for name, fn := range map[string]func([]byte) error{
		"snapshot": func(b []byte) error { _, err := wire.UnmarshalSnapshot(b); return err },
		"global":   func(b []byte) error { _, err := wire.UnmarshalGlobal(b); return err },
		"tenant":   func(b []byte) error { _, err := wire.UnmarshalTenant(b); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := fn([]byte{0xff, 0xff, 0xff, 0xff}); err == nil {
				t.Fatal("expected a parse error")
			}
		})
	}
}

func TestBytesRoundTripThroughMarshal(t *testing.T) {
	msg := &pb.Snapshot{
		GlobalLayer: wire.EncodeGlobal(fullGlobalSpec()),
		Tenants:     []*pb.TenantLayer{wire.EncodeTenant(fullTenantSpec())},
	}
	b, err := wire.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	parsed, err := wire.UnmarshalSnapshot(b)
	if err != nil {
		t.Fatalf("UnmarshalSnapshot: %v", err)
	}
	if _, err := wire.DecodeSnapshot(parsed); err != nil {
		t.Fatalf("DecodeSnapshot after a byte round trip: %v", err)
	}
}
