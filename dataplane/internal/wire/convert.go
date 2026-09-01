// Package wire converts between the snapshot wire format and the domain types
// the request path uses.
//
// The two are deliberately separate. Wire types are append-only and
// backward-compatible forever, because during a rollout an older worker and a
// newer control plane are live at the same time. Domain types are shaped for
// the request path — indexed, validated, immutable — and are refactored
// freely. This package is the only place that knows both, so a rename in
// internal/core never becomes a wire-compatibility question.
//
// Conversion goes through core's Spec types rather than the built layers.
// Specs are plain exported data; layers are validated and indexed, and their
// internals are intentionally unreachable. That makes the direction of travel
// explicit: bytes -> spec -> validated layer, and never back into bytes from a
// layer that a worker is serving.
package wire

import (
	"math"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	pb "github.com/umerjavaidkh/model-gateway/dataplane/internal/wire/gatewayv1"
)

// DecodeSnapshot converts a wire snapshot into a validated, composed snapshot.
//
// Validation is core's, not ours: this function shapes data and core decides
// whether it is coherent. That keeps one definition of "a valid snapshot"
// rather than a second, weaker one here that drifts.
func DecodeSnapshot(msg *pb.Snapshot) (*core.Snapshot, error) {
	if msg == nil || msg.GetGlobalLayer() == nil {
		return nil, core.New(core.CodeInvalidRequest, "snapshot has no global layer")
	}

	global, err := core.NewGlobalLayer(DecodeGlobal(msg.GetGlobalLayer()))
	if err != nil {
		return nil, err
	}

	tenants := make([]*core.TenantLayer, 0, len(msg.GetTenants()))
	for _, t := range msg.GetTenants() {
		layer, err := core.NewTenantLayer(DecodeTenant(t))
		if err != nil {
			return nil, err
		}
		tenants = append(tenants, layer)
	}
	return core.Compose(global, tenants...)
}

// DecodeGlobal converts a wire global layer into the spec core builds from.
func DecodeGlobal(msg *pb.GlobalLayer) core.GlobalSpec {
	spec := core.GlobalSpec{
		Version:         decodeVersion(msg.GetVersion()),
		BuiltAt:         fromUnixMillis(msg.GetBuiltAtUnixMs()),
		PolicyBundleRef: msg.GetPolicyBundleRef(),
		TenantPrefixes:  make(map[core.KeyPrefix]core.TenantID, len(msg.GetTenantPrefixes())),
	}
	for prefix, tenant := range msg.GetTenantPrefixes() {
		spec.TenantPrefixes[core.KeyPrefix(prefix)] = core.TenantID(tenant)
	}
	for _, d := range msg.GetDeployments() {
		spec.Deployments = append(spec.Deployments, decodeDeployment(d))
	}
	for _, a := range msg.GetAliases() {
		spec.Aliases = append(spec.Aliases, decodeAlias(a))
	}
	for _, p := range msg.GetDefaultPlugins() {
		spec.DefaultPlugins = append(spec.DefaultPlugins, decodePlugin(p))
	}
	for _, g := range msg.GetDefaultGuardrails() {
		spec.DefaultGuardrails = append(spec.DefaultGuardrails, decodeGuardrail(g))
	}
	return spec
}

// DecodeTenant converts a wire tenant layer into the spec core builds from.
func DecodeTenant(msg *pb.TenantLayer) core.TenantSpec {
	spec := core.TenantSpec{
		Tenant:       core.TenantID(msg.GetTenant()),
		Version:      decodeVersion(msg.GetVersion()),
		BuiltAt:      fromUnixMillis(msg.GetBuiltAtUnixMs()),
		Tier:         msg.GetTier(),
		MinTrustTier: decodeTrustTier(msg.GetMinTrustTier()),
		Keys:         make(map[core.KeyLookup]core.KeyID, len(msg.GetKeys())),
	}
	for _, p := range msg.GetPrincipals() {
		spec.Principals = append(spec.Principals, decodePrincipal(p))
	}
	for _, k := range msg.GetKeys() {
		var lookup core.KeyLookup
		// A lookup of the wrong length is dropped rather than truncated into a
		// value that would collide with a real key. core then rejects the layer
		// for mapping a key to a principal it cannot find, which surfaces the
		// corruption instead of quietly authenticating the wrong caller.
		if len(k.GetLookup()) != len(lookup) {
			continue
		}
		copy(lookup[:], k.GetLookup())
		spec.Keys[lookup] = core.KeyID(k.GetKeyId())
	}
	for _, a := range msg.GetAliasOverrides() {
		spec.AliasOverrides = append(spec.AliasOverrides, decodeAlias(a))
	}
	for _, b := range msg.GetBudgets() {
		spec.Budgets = append(spec.Budgets, decodeBudget(b))
	}
	for _, p := range msg.GetPlugins() {
		spec.Plugins = append(spec.Plugins, decodePlugin(p))
	}
	for _, g := range msg.GetGuardrails() {
		spec.Guardrails = append(spec.Guardrails, decodeGuardrail(g))
	}
	return spec
}

// EncodeGlobal converts a global spec into its wire form.
//
// Encoding exists for tests, fixtures and the load harness. Production
// snapshots are produced by the Python control plane; nothing in the data plane
// writes one.
func EncodeGlobal(spec core.GlobalSpec) *pb.GlobalLayer {
	msg := &pb.GlobalLayer{
		Version:         encodeVersion(spec.Version),
		BuiltAtUnixMs:   toUnixMillis(spec.BuiltAt),
		PolicyBundleRef: spec.PolicyBundleRef,
		TenantPrefixes:  make(map[string]string, len(spec.TenantPrefixes)),
	}
	for prefix, tenant := range spec.TenantPrefixes {
		msg.TenantPrefixes[string(prefix)] = string(tenant)
	}
	for _, d := range spec.Deployments {
		msg.Deployments = append(msg.Deployments, encodeDeployment(d))
	}
	for _, a := range spec.Aliases {
		msg.Aliases = append(msg.Aliases, encodeAlias(a))
	}
	for _, p := range spec.DefaultPlugins {
		msg.DefaultPlugins = append(msg.DefaultPlugins, encodePlugin(p))
	}
	for _, g := range spec.DefaultGuardrails {
		msg.DefaultGuardrails = append(msg.DefaultGuardrails, encodeGuardrail(g))
	}
	return msg
}

// EncodeTenant converts a tenant spec into its wire form.
func EncodeTenant(spec core.TenantSpec) *pb.TenantLayer {
	msg := &pb.TenantLayer{
		Tenant:        string(spec.Tenant),
		Version:       encodeVersion(spec.Version),
		BuiltAtUnixMs: toUnixMillis(spec.BuiltAt),
		Tier:          spec.Tier,
		MinTrustTier:  encodeTrustTier(spec.MinTrustTier),
	}
	for _, p := range spec.Principals {
		msg.Principals = append(msg.Principals, encodePrincipal(p))
	}
	for lookup, keyID := range spec.Keys {
		msg.Keys = append(msg.Keys, &pb.KeyEntry{Lookup: lookup[:], KeyId: string(keyID)})
	}
	for _, a := range spec.AliasOverrides {
		msg.AliasOverrides = append(msg.AliasOverrides, encodeAlias(a))
	}
	for _, b := range spec.Budgets {
		msg.Budgets = append(msg.Budgets, encodeBudget(b))
	}
	for _, p := range spec.Plugins {
		msg.Plugins = append(msg.Plugins, encodePlugin(p))
	}
	for _, g := range spec.Guardrails {
		msg.Guardrails = append(msg.Guardrails, encodeGuardrail(g))
	}
	return msg
}

// --- leaf conversions -------------------------------------------------------

func decodeVersion(v *pb.LayerVersion) core.LayerVersion {
	return core.LayerVersion{Number: v.GetNumber(), Digest: v.GetDigest()}
}

func encodeVersion(v core.LayerVersion) *pb.LayerVersion {
	return &pb.LayerVersion{Number: v.Number, Digest: v.Digest}
}

func decodeRoutingKey(k *pb.RoutingKey) core.RoutingKey {
	return core.RoutingKey{BaseModel: k.GetBaseModel(), AdapterID: k.GetAdapterId()}
}

func encodeRoutingKey(k core.RoutingKey) *pb.RoutingKey {
	return &pb.RoutingKey{BaseModel: k.BaseModel, AdapterId: k.AdapterID}
}

func decodeDeployment(d *pb.Deployment) core.Deployment {
	out := core.Deployment{
		ID:            core.DeploymentID(d.GetId()),
		Key:           decodeRoutingKey(d.GetKey()),
		Provider:      d.GetProvider(),
		Endpoint:      d.GetEndpoint(),
		Region:        d.GetRegion(),
		TrustTier:     decodeTrustTier(d.GetTrustTier()),
		CredentialRef: d.GetCredentialRef(),
		Cost: core.Cost{
			InputPer1K:       core.MicroUSD(d.GetCost().GetInputPer_1KMicroUsd()),
			OutputPer1K:      core.MicroUSD(d.GetCost().GetOutputPer_1KMicroUsd()),
			CachedInputPer1K: core.MicroUSD(d.GetCost().GetCachedInputPer_1KMicroUsd()),
			CacheWritePer1K:  core.MicroUSD(d.GetCost().GetCacheWritePer_1KMicroUsd()),
		},
	}
	out.Weight = d.GetWeight()
	for _, c := range d.GetCapabilities() {
		out.Capabilities = append(out.Capabilities, core.Capability(c))
	}
	return out
}

func encodeDeployment(d core.Deployment) *pb.Deployment {
	msg := &pb.Deployment{
		Id:            string(d.ID),
		Key:           encodeRoutingKey(d.Key),
		Provider:      d.Provider,
		Endpoint:      d.Endpoint,
		Region:        d.Region,
		TrustTier:     encodeTrustTier(d.TrustTier),
		CredentialRef: d.CredentialRef,
		Weight:        d.Weight,
		Cost: &pb.Cost{
			InputPer_1KMicroUsd:       int64(d.Cost.InputPer1K),
			OutputPer_1KMicroUsd:      int64(d.Cost.OutputPer1K),
			CachedInputPer_1KMicroUsd: int64(d.Cost.CachedInputPer1K),
			CacheWritePer_1KMicroUsd:  int64(d.Cost.CacheWritePer1K),
		},
	}
	for _, c := range d.Capabilities {
		msg.Capabilities = append(msg.Capabilities, string(c))
	}
	return msg
}

func decodeAlias(a *pb.ModelAlias) core.ModelAlias {
	out := core.ModelAlias{Name: a.GetName()}
	for _, t := range a.GetTargets() {
		out.Targets = append(out.Targets, decodeRoutingKey(t))
	}
	return out
}

func encodeAlias(a core.ModelAlias) *pb.ModelAlias {
	msg := &pb.ModelAlias{Name: a.Name}
	for _, t := range a.Targets {
		msg.Targets = append(msg.Targets, encodeRoutingKey(t))
	}
	return msg
}

func decodePlugin(p *pb.PluginBinding) core.PluginBinding {
	return core.PluginBinding{
		Port:      decodePort(p.GetPort()),
		Component: p.GetComponent(),
		Version:   p.GetVersion(),
		ConfigRef: p.GetConfigRef(),
	}
}

func encodePlugin(p core.PluginBinding) *pb.PluginBinding {
	return &pb.PluginBinding{
		Port:      encodePort(p.Port),
		Component: p.Component,
		Version:   p.Version,
		ConfigRef: p.ConfigRef,
	}
}

// Failure mode converts through an explicit table, and an unrecognised value
// becomes fail-closed. A guardrail whose mode a worker does not understand
// must not be silently treated as advisory: the safe reading of "I do not know
// what this control does" is to enforce it.
var failureModes = map[pb.FailureMode]core.FailureMode{
	pb.FailureMode_FAILURE_MODE_OPEN:   core.FailOpen,
	pb.FailureMode_FAILURE_MODE_CLOSED: core.FailClosed,
}

func decodeGuardrail(g *pb.GuardrailBinding) core.GuardrailBinding {
	mode, ok := failureModes[g.GetFailureMode()]
	if !ok {
		mode = core.FailClosed
	}

	phases := make([]core.Phase, 0, len(g.GetPhases()))
	for _, name := range g.GetPhases() {
		if name == "response" {
			phases = append(phases, core.PhaseResponse)
		} else {
			phases = append(phases, core.PhaseRequest)
		}
	}

	return core.GuardrailBinding{
		Component: g.GetComponent(),
		Version:   g.GetVersion(),
		ConfigRef: g.GetConfigRef(),
		Budget: core.GuardrailBudget{
			Timeout:  time.Duration(g.GetTimeoutMs()) * time.Millisecond,
			Mode:     mode,
			Blocking: g.GetBlocking(),
			Phases:   phases,
		},
	}
}

func encodeGuardrail(g core.GuardrailBinding) *pb.GuardrailBinding {
	mode := pb.FailureMode_FAILURE_MODE_CLOSED
	if g.Budget.Mode == core.FailOpen {
		mode = pb.FailureMode_FAILURE_MODE_OPEN
	}

	phases := make([]string, 0, len(g.Budget.Phases))
	for _, phase := range g.Budget.Phases {
		if phase == core.PhaseResponse {
			phases = append(phases, "response")
		} else {
			phases = append(phases, "request")
		}
	}

	return &pb.GuardrailBinding{
		Component:   g.Component,
		Version:     g.Version,
		ConfigRef:   g.ConfigRef,
		TimeoutMs:   clampMillis(g.Budget.Timeout),
		FailureMode: mode,
		Blocking:    g.Budget.Blocking,
		Phases:      phases,
	}
}

// clampMillis narrows a duration to the wire's uint32 milliseconds.
//
// A guardrail timeout is realistically single-digit milliseconds, but an
// unclamped conversion wraps silently: a nonsensical 50-day budget would
// arrive as a small one and the guardrail would appear to time out constantly.
// Better to carry the largest representable value than a wrong small one.
func clampMillis(d time.Duration) uint32 {
	ms := d.Milliseconds()
	switch {
	case ms <= 0:
		return 0
	case ms > int64(math.MaxUint32):
		return math.MaxUint32
	default:
		return uint32(ms)
	}
}

func decodeBudget(b *pb.BudgetState) core.BudgetState {
	out := core.BudgetState{
		ID:            core.BudgetID(b.GetId()),
		Scope:         decodeBudgetScope(b.GetScope()),
		LimitMicroUSD: core.MicroUSD(b.GetLimitMicroUsd()),
		SpentMicroUSD: core.MicroUSD(b.GetSpentMicroUsd()),
		Hard:          b.GetHard(),
	}
	out.HeadroomBasisPoints = b.GetHeadroomBasisPoints()
	return out
}

func encodeBudget(b core.BudgetState) *pb.BudgetState {
	return &pb.BudgetState{
		Id:                  string(b.ID),
		Scope:               encodeBudgetScope(b.Scope),
		LimitMicroUsd:       int64(b.LimitMicroUSD),
		SpentMicroUsd:       int64(b.SpentMicroUSD),
		Hard:                b.Hard,
		HeadroomBasisPoints: b.HeadroomBasisPoints,
	}
}

func decodePrincipal(p *pb.Principal) core.Principal {
	out := core.Principal{
		KeyID:        core.KeyID(p.GetKeyId()),
		Tenant:       core.TenantID(p.GetTenant()),
		Org:          core.OrgID(p.GetOrg()),
		Team:         core.TeamID(p.GetTeam()),
		User:         core.UserID(p.GetUser()),
		App:          core.AppID(p.GetApp()),
		Models:       core.ModelAllowlist{AllowAll: p.GetModelsAllowAll(), Names: p.GetModels()},
		DefaultClass: core.DataClass(p.GetDefaultDataClass()),
		MinTrustTier: decodeTrustTier(p.GetMinTrustTier()),
		Limits:       decodeLimits(p),
		Deprecated:   p.GetDeprecated(),
		NotAfter:     fromUnixMillis(p.GetNotAfterUnixMs()),
	}
	for _, r := range p.GetRoles() {
		out.Roles = append(out.Roles, core.Role(r))
	}
	for _, b := range p.GetBudgets() {
		out.Budgets = append(out.Budgets, core.BudgetRef{
			ID: core.BudgetID(b.GetId()), Scope: decodeBudgetScope(b.GetScope()),
		})
	}
	return out
}

// decodeLimits reads the limits, falling back to the superseded
// max_concurrent field.
//
// A snapshot compiled before RateLimit existed still carries max_concurrent at
// its old tag. Reading it keeps an older control plane working against a newer
// worker, which is the situation a rolling upgrade is made of.
func decodeLimits(p *pb.Principal) core.RateLimit {
	limits := core.RateLimit{MaxConcurrent: p.GetMaxConcurrent()}
	if l := p.GetLimits(); l != nil {
		limits.RequestsPerMinute = l.GetRequestsPerMinute()
		limits.TokensPerMinute = l.GetTokensPerMinute()
		if l.GetMaxConcurrent() > 0 {
			limits.MaxConcurrent = l.GetMaxConcurrent()
		}
	}
	return limits
}

func encodePrincipal(p core.Principal) *pb.Principal {
	msg := &pb.Principal{
		KeyId:            string(p.KeyID),
		Tenant:           string(p.Tenant),
		Org:              string(p.Org),
		Team:             string(p.Team),
		User:             string(p.User),
		App:              string(p.App),
		ModelsAllowAll:   p.Models.AllowAll,
		Models:           p.Models.Names,
		DefaultDataClass: string(p.DefaultClass),
		MinTrustTier:     encodeTrustTier(p.MinTrustTier),
		MaxConcurrent:    p.Limits.MaxConcurrent,
		Limits: &pb.RateLimit{
			RequestsPerMinute: p.Limits.RequestsPerMinute,
			TokensPerMinute:   p.Limits.TokensPerMinute,
			MaxConcurrent:     p.Limits.MaxConcurrent,
		},
		Deprecated:     p.Deprecated,
		NotAfterUnixMs: toUnixMillis(p.NotAfter),
	}
	for _, r := range p.Roles {
		msg.Roles = append(msg.Roles, string(r))
	}
	for _, b := range p.Budgets {
		msg.Budgets = append(msg.Budgets, &pb.BudgetRef{Id: string(b.ID), Scope: encodeBudgetScope(b.Scope)})
	}
	return msg
}

// --- enums ------------------------------------------------------------------
//
// Enums are converted through explicit tables rather than numeric casts. The
// wire and domain numbering happen to agree today; a cast would make that
// coincidence load-bearing, so that reordering a domain constant would silently
// change what a snapshot means.

var trustTiers = map[pb.TrustTier]core.TrustTier{
	pb.TrustTier_TRUST_TIER_EXTERNAL:      core.TrustExternal,
	pb.TrustTier_TRUST_TIER_PRIVATE_CLOUD: core.TrustPrivateCloud,
	pb.TrustTier_TRUST_TIER_INTERNAL:      core.TrustInternal,
}

// decodeTrustTier maps an unknown or unspecified tier to TrustUnset, which core
// rejects. A tier added by a newer control plane must fail the layer rather
// than default to something permissive.
func decodeTrustTier(t pb.TrustTier) core.TrustTier { return trustTiers[t] }

func encodeTrustTier(t core.TrustTier) pb.TrustTier {
	for wire, domain := range trustTiers {
		if domain == t {
			return wire
		}
	}
	return pb.TrustTier_TRUST_TIER_UNSPECIFIED
}

var budgetScopes = map[pb.BudgetScope]core.BudgetScope{
	pb.BudgetScope_BUDGET_SCOPE_KEY:      core.BudgetScopeKey,
	pb.BudgetScope_BUDGET_SCOPE_APP:      core.BudgetScopeApp,
	pb.BudgetScope_BUDGET_SCOPE_USER:     core.BudgetScopeUser,
	pb.BudgetScope_BUDGET_SCOPE_TEAM:     core.BudgetScopeTeam,
	pb.BudgetScope_BUDGET_SCOPE_ORG:      core.BudgetScopeOrg,
	pb.BudgetScope_BUDGET_SCOPE_MODEL:    core.BudgetScopeModel,
	pb.BudgetScope_BUDGET_SCOPE_TRAINING: core.BudgetScopeTraining,
}

func decodeBudgetScope(s pb.BudgetScope) core.BudgetScope { return budgetScopes[s] }

func encodeBudgetScope(s core.BudgetScope) pb.BudgetScope {
	for wire, domain := range budgetScopes {
		if domain == s {
			return wire
		}
	}
	return pb.BudgetScope_BUDGET_SCOPE_UNSPECIFIED
}

var ports = map[pb.Port]core.PortName{
	pb.Port_PORT_PROVIDER:  core.PortProvider,
	pb.Port_PORT_GUARDRAIL: core.PortGuardrail,
	pb.Port_PORT_STORE:     core.PortStore,
	pb.Port_PORT_TELEMETRY: core.PortTelemetry,
}

func decodePort(p pb.Port) core.PortName { return ports[p] }

func encodePort(p core.PortName) pb.Port {
	for wire, domain := range ports {
		if domain == p {
			return wire
		}
	}
	return pb.Port_PORT_UNSPECIFIED
}

// --- time -------------------------------------------------------------------
//
// Unix milliseconds, not a timestamp message: these are build stamps and key
// expiries, where millisecond resolution is ample and a plain integer keeps the
// wire format free of a well-known-types dependency. Zero means "unset" in both
// directions, which is why the round trip preserves a zero time exactly.

func fromUnixMillis(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

func toUnixMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
