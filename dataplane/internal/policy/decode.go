package policy

import (
	"net/netip"
	"sync"

	"google.golang.org/protobuf/proto"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	pb "github.com/umerjavaidkh/model-gateway/dataplane/internal/wire/gatewayv1"
)

// Decode turns the bytes a snapshot carries into an evaluable bundle.
//
// CIDRs are parsed here, once per snapshot version, rather than per request.
// Parsing a prefix on every evaluation would be exactly the "interpreting
// policy in the hot path" this design avoids, in miniature — and a malformed
// one would fail at request time rather than at compile time.
func Decode(raw []byte) (Bundle, error) {
	if len(raw) == 0 {
		return Bundle{}, nil
	}

	var msg pb.PolicyBundle
	if err := proto.Unmarshal(raw, &msg); err != nil {
		return Bundle{}, core.Wrap(core.CodeInvalidRequest, err, "parsing the policy bundle")
	}

	bundle := Bundle{
		ID:            msg.GetId(),
		Version:       msg.GetVersion(),
		DefaultEffect: decodeEffect(msg.GetDefaultEffect()),
		Rules:         make([]Rule, 0, len(msg.GetRules())),
	}

	for _, r := range msg.GetRules() {
		rule, err := decodeRule(r)
		if err != nil {
			return Bundle{}, err
		}
		bundle.Rules = append(bundle.Rules, rule)
	}
	return bundle, nil
}

func decodeRule(r *pb.PolicyRule) (Rule, error) {
	rule := Rule{
		ID:              r.GetId(),
		Effect:          decodeEffect(r.GetEffect()),
		Models:          r.GetModels(),
		Regions:         r.GetRegions(),
		MaxPayloadBytes: r.GetMaxPayloadBytes(),
		DataClass:       core.DataClass(r.GetDataClass()),
		MinTrustTier:    decodeTier(r.GetMinTrustTier()),
		DeepInspection:  r.GetDeepInspection(),
		Reason:          r.GetReason(),
	}

	if rule.Effect == EffectUnset {
		// A rule that neither allows nor denies would be skipped silently on
		// every request, which looks exactly like a rule that never matches.
		return Rule{}, core.Newf(core.CodeInvalidRequest,
			"policy rule %q has no effect", rule.ID)
	}

	for _, endpoint := range r.GetEndpoints() {
		rule.Endpoints = append(rule.Endpoints, core.Endpoint(endpoint))
	}
	for _, role := range r.GetRoles() {
		rule.Roles = append(rule.Roles, core.Role(role))
	}
	for _, cidr := range r.GetSourceCidrs() {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			// Rejected at decode rather than skipped. A network rule that
			// silently does not apply is a restriction an operator believes is
			// in force and is not.
			return Rule{}, core.Wrapf(core.CodeInvalidRequest, err,
				"policy rule %q has an unparseable network %q", rule.ID, cidr)
		}
		rule.SourceNets = append(rule.SourceNets, prefix)
	}
	return rule, nil
}

func decodeEffect(e pb.PolicyEffect) Effect {
	switch e {
	case pb.PolicyEffect_POLICY_EFFECT_ALLOW:
		return EffectAllow
	case pb.PolicyEffect_POLICY_EFFECT_DENY:
		return EffectDeny
	default:
		return EffectUnset
	}
}

func decodeTier(t pb.TrustTier) core.TrustTier {
	switch t {
	case pb.TrustTier_TRUST_TIER_EXTERNAL:
		return core.TrustExternal
	case pb.TrustTier_TRUST_TIER_PRIVATE_CLOUD:
		return core.TrustPrivateCloud
	case pb.TrustTier_TRUST_TIER_INTERNAL:
		return core.TrustInternal
	default:
		return core.TrustUnset
	}
}

// Cache decodes a bundle once per snapshot version rather than per request.
//
// Decoding is cheap but not free, and a bundle is identical for every request
// served by one snapshot. The cache is keyed by the bytes' identity via the
// tenant and the snapshot version the caller supplies, so a configuration
// change invalidates it without anything having to remember to.
type Cache struct {
	mu      sync.RWMutex
	version uint64
	bundles map[core.TenantID]Bundle
}

// NewCache returns an empty cache.
func NewCache() *Cache {
	return &Cache{bundles: map[core.TenantID]Bundle{}}
}

// For returns the decoded bundle for a tenant at a snapshot version.
//
// A bundle that fails to decode is returned as empty with the error, and the
// caller decides. It is not cached, so a fixed configuration takes effect at
// the next snapshot rather than at the next restart.
func (c *Cache) For(tenant core.TenantID, version uint64, raw []byte) (Bundle, error) {
	c.mu.RLock()
	if c.version == version {
		if bundle, ok := c.bundles[tenant]; ok {
			c.mu.RUnlock()
			return bundle, nil
		}
	}
	c.mu.RUnlock()

	bundle, err := Decode(raw)
	if err != nil {
		return Bundle{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.version != version {
		// A new snapshot: everything decoded against the old one is stale, and
		// keeping it would mean policy silently lagging configuration.
		c.version = version
		c.bundles = map[core.TenantID]Bundle{}
	}
	c.bundles[tenant] = bundle
	return bundle, nil
}
