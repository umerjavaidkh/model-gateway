// Package policy evaluates the compiled decision table a snapshot carries.
//
// # Compiled, never interpreted
//
// Policy is compiled in the control plane and arrives as an ordered list of
// rules over a fixed attribute set. Evaluating it is an array scan over a
// handful of comparisons — no parsing, no allocation on the allow path, and no
// language runtime in the request path.
//
// See docs/adr/0006-compiled-policy-not-a-policy-language.md for why this is a
// decision table rather than Cedar or Rego, and what would change that.
//
// # First match wins
//
// The order an operator wrote is the whole of the conflict-resolution
// semantics. There is no precedence to learn and no most-specific-rule
// calculation to get wrong — the bundle reads top to bottom.
package policy

import (
	"net/netip"
	"slices"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

// Effect is what a rule does when it matches.
type Effect uint8

// The effects a rule may have. Unset is the zero value and is never valid in a
// compiled bundle, so a rule that neither allows nor denies fails at decode
// rather than being skipped silently on every request.
const (
	EffectUnset Effect = iota
	EffectAllow
	EffectDeny
)

// Rule is one row of the table.
//
// Every condition is a set the attribute must be in, and an empty set matches
// anything. All present conditions must hold, so a rule is a conjunction and
// can be read without reference to any other rule.
type Rule struct {
	ID     string
	Effect Effect

	Models    []string
	Endpoints []core.Endpoint
	Roles     []core.Role
	Regions   []string
	// SourceNets are pre-parsed at compile time. Parsing CIDRs per request
	// would be the "interpreting policy in the hot path" this design avoids,
	// in miniature.
	SourceNets      []netip.Prefix
	MaxPayloadBytes uint64

	// Applied when an allow rule matches. Stamping here is what lets policy
	// decide sensitivity, which the router then turns into a destination
	// constraint — the ordering the design requires.
	DataClass    core.DataClass
	MinTrustTier core.TrustTier
	// DeepInspection asks for the statistical detection tier as well as the
	// deterministic one.
	DeepInspection bool

	Reason string
}

// Bundle is a compiled policy.
type Bundle struct {
	ID      string
	Version uint64
	Rules   []Rule
	// DefaultEffect applies when no rule matches.
	DefaultEffect Effect
}

// Empty reports whether the bundle has nothing to say.
func (b Bundle) Empty() bool { return len(b.Rules) == 0 }

// Input is everything a rule can test.
//
// A fixed set, deliberately. Every attribute here is one the control plane can
// promise is present; adding an open-ended attribute bag would mean rules that
// silently never match because the field was spelled differently.
type Input struct {
	Model    string
	Endpoint core.Endpoint
	Roles    []core.Role
	Region   string
	Source   netip.Addr
	Payload  uint64
}

// Decision is the outcome of evaluating a bundle.
type Decision struct {
	Allowed bool
	// RuleID names the rule that decided, or is empty when the default did.
	// An operator asking "why was this refused" needs the rule, not the answer.
	RuleID string
	Reason string
	// DataClass and MinTrustTier are stamped by a matching allow rule and are
	// zero otherwise, meaning the request's existing values stand.
	DataClass      core.DataClass
	MinTrustTier   core.TrustTier
	DeepInspection bool
}

// Evaluate returns the first matching rule's effect, or the bundle default.
//
// An empty bundle allows. Policy is one control among several — the model
// allowlist, budgets and trust tiers already restrict — so a bundle that denied
// by default would make adding the feature an outage.
func Evaluate(bundle Bundle, in Input) Decision {
	for i := range bundle.Rules {
		rule := &bundle.Rules[i]
		if !matches(rule, in) {
			continue
		}
		if rule.Effect == EffectDeny {
			return Decision{RuleID: rule.ID, Reason: rule.Reason}
		}
		return Decision{
			Allowed:        true,
			RuleID:         rule.ID,
			DataClass:      rule.DataClass,
			MinTrustTier:   rule.MinTrustTier,
			DeepInspection: rule.DeepInspection,
		}
	}

	return Decision{Allowed: bundle.DefaultEffect != EffectDeny}
}

// matches reports whether every present condition holds.
func matches(rule *Rule, in Input) bool {
	if len(rule.Models) > 0 && !slices.Contains(rule.Models, in.Model) {
		return false
	}
	if len(rule.Endpoints) > 0 && !slices.Contains(rule.Endpoints, in.Endpoint) {
		return false
	}
	if len(rule.Regions) > 0 && !slices.Contains(rule.Regions, in.Region) {
		return false
	}
	if rule.MaxPayloadBytes > 0 && in.Payload > rule.MaxPayloadBytes {
		return false
	}
	if len(rule.Roles) > 0 && !hasAnyRole(rule.Roles, in.Roles) {
		return false
	}
	if len(rule.SourceNets) > 0 && !inAnyNet(rule.SourceNets, in.Source) {
		return false
	}
	return true
}

// hasAnyRole is an intersection test: a rule listing roles matches a principal
// holding any of them, which is how a role condition reads in every system
// anybody has used.
func hasAnyRole(required, held []core.Role) bool {
	for _, role := range held {
		if slices.Contains(required, role) {
			return true
		}
	}
	return false
}

// inAnyNet reports whether an address falls inside any listed block.
//
// An address the gateway could not determine matches nothing. A rule that
// restricts by network must not be satisfied by "we do not know where this came
// from" — that is the case it exists to catch.
func inAnyNet(nets []netip.Prefix, addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}
	for _, net := range nets {
		if net.Contains(addr) {
			return true
		}
	}
	return false
}
