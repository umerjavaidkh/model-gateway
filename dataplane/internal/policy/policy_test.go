package policy_test

import (
	"net/netip"
	"testing"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/policy"
)

func addr(s string) netip.Addr {
	parsed, _ := netip.ParseAddr(s)
	return parsed
}

func prefix(s string) netip.Prefix {
	parsed, _ := netip.ParsePrefix(s)
	return parsed
}

func request() policy.Input {
	return policy.Input{
		Model:    "gpt-4o-mini",
		Endpoint: core.EndpointChatCompletions,
		Roles:    []core.Role{"engineer"},
		Region:   "eu",
		Source:   addr("10.0.0.5"),
		Payload:  1024,
	}
}

func TestAnEmptyBundleAllows(t *testing.T) {
	// Policy is one control among several — the allowlist, budgets and trust
	// tiers already restrict — so a bundle that denied by default would make
	// adding the feature an outage.
	if !policy.Evaluate(policy.Bundle{}, request()).Allowed {
		t.Fatal("an empty bundle refused a request")
	}
}

func TestFirstMatchWins(t *testing.T) {
	// The order an operator wrote is the whole of the conflict-resolution
	// semantics: there is no precedence to learn.
	bundle := policy.Bundle{Rules: []policy.Rule{
		{ID: "allow-first", Effect: policy.EffectAllow, Models: []string{"gpt-4o-mini"}},
		{ID: "deny-second", Effect: policy.EffectDeny, Models: []string{"gpt-4o-mini"}},
	}}

	decision := policy.Evaluate(bundle, request())
	if !decision.Allowed || decision.RuleID != "allow-first" {
		t.Fatalf("decision = %+v, want the first rule to decide", decision)
	}
}

func TestEveryConditionMustHold(t *testing.T) {
	// A rule is a conjunction, so it can be read without reference to any
	// other rule.
	rule := policy.Rule{
		ID: "narrow", Effect: policy.EffectDeny,
		Models:    []string{"gpt-4o-mini"},
		Endpoints: []core.Endpoint{core.EndpointChatCompletions},
		Regions:   []string{"us"},
	}
	bundle := policy.Bundle{Rules: []policy.Rule{rule}}

	// Region does not match, so the rule does not apply.
	if !policy.Evaluate(bundle, request()).Allowed {
		t.Fatal("a rule matched despite one condition failing")
	}

	in := request()
	in.Region = "us"
	if policy.Evaluate(bundle, in).Allowed {
		t.Fatal("a rule did not match with every condition satisfied")
	}
}

func TestAnEmptyConditionMatchesAnything(t *testing.T) {
	bundle := policy.Bundle{Rules: []policy.Rule{
		{ID: "catch-all", Effect: policy.EffectDeny, Reason: "no"},
	}}

	decision := policy.Evaluate(bundle, request())
	if decision.Allowed || decision.Reason != "no" {
		t.Fatalf("decision = %+v, want a refusal carrying its reason", decision)
	}
}

func TestNetworkConditions(t *testing.T) {
	bundle := policy.Bundle{Rules: []policy.Rule{
		{ID: "corp-only", Effect: policy.EffectAllow, SourceNets: []netip.Prefix{prefix("10.0.0.0/8")}},
		{ID: "deny-rest", Effect: policy.EffectDeny, Reason: "outside the corporate network"},
	}}

	if !policy.Evaluate(bundle, request()).Allowed {
		t.Fatal("an address inside the block was refused")
	}

	outside := request()
	outside.Source = addr("203.0.113.7")
	if policy.Evaluate(bundle, outside).Allowed {
		t.Fatal("an address outside the block was allowed")
	}
}

func TestAnUnknownSourceMatchesNoNetworkRule(t *testing.T) {
	// A rule that restricts by network must not be satisfied by "we do not
	// know where this came from" — that is the case it exists to catch.
	bundle := policy.Bundle{Rules: []policy.Rule{
		{ID: "corp-only", Effect: policy.EffectAllow, SourceNets: []netip.Prefix{prefix("10.0.0.0/8")}},
		{ID: "deny-rest", Effect: policy.EffectDeny},
	}}

	unknown := request()
	unknown.Source = netip.Addr{}
	if policy.Evaluate(bundle, unknown).Allowed {
		t.Fatal("a request with no known source satisfied a network rule")
	}
}

func TestRolesMatchOnIntersection(t *testing.T) {
	bundle := policy.Bundle{Rules: []policy.Rule{
		{ID: "admins", Effect: policy.EffectAllow, Roles: []core.Role{"admin", "sre"}},
		{ID: "deny-rest", Effect: policy.EffectDeny},
	}}

	if policy.Evaluate(bundle, request()).Allowed {
		t.Fatal("a principal without any listed role matched")
	}

	privileged := request()
	privileged.Roles = []core.Role{"engineer", "sre"}
	if !policy.Evaluate(bundle, privileged).Allowed {
		t.Fatal("a principal holding one listed role did not match")
	}
}

func TestPayloadCeiling(t *testing.T) {
	bundle := policy.Bundle{Rules: []policy.Rule{
		{ID: "small-only", Effect: policy.EffectAllow, MaxPayloadBytes: 2048},
		{ID: "deny-large", Effect: policy.EffectDeny, Reason: "payload too large"},
	}}

	if !policy.Evaluate(bundle, request()).Allowed {
		t.Fatal("a request under the ceiling was refused")
	}

	large := request()
	large.Payload = 4096
	if policy.Evaluate(bundle, large).Allowed {
		t.Fatal("a request over the ceiling was allowed")
	}
}

func TestAnAllowRuleStampsSensitivity(t *testing.T) {
	// Policy decides sensitivity and the router turns it into a destination
	// constraint. That ordering is what keeps a restricted request from being
	// cost-optimised onto an external provider.
	bundle := policy.Bundle{Rules: []policy.Rule{{
		ID: "sensitive", Effect: policy.EffectAllow,
		Models:       []string{"gpt-4o-mini"},
		DataClass:    core.DataClassRestricted,
		MinTrustTier: core.TrustInternal,
	}}}

	decision := policy.Evaluate(bundle, request())
	if !decision.Allowed {
		t.Fatal("the rule refused")
	}
	if decision.DataClass != core.DataClassRestricted {
		t.Fatalf("DataClass = %q, want the stamp", decision.DataClass)
	}
	if decision.MinTrustTier != core.TrustInternal {
		t.Fatalf("MinTrustTier = %v, want the stamp", decision.MinTrustTier)
	}
}

func TestTheDefaultEffectAppliesWhenNothingMatches(t *testing.T) {
	bundle := policy.Bundle{
		Rules:         []policy.Rule{{ID: "other", Effect: policy.EffectAllow, Models: []string{"other"}}},
		DefaultEffect: policy.EffectDeny,
	}

	decision := policy.Evaluate(bundle, request())
	if decision.Allowed {
		t.Fatal("a deny-by-default bundle allowed an unmatched request")
	}
	// No rule decided, so none is named — which is itself the answer to "why".
	if decision.RuleID != "" {
		t.Fatalf("RuleID = %q, want empty for a default decision", decision.RuleID)
	}
}

func TestDecodeRejectsARuleWithNoEffect(t *testing.T) {
	// A rule that neither allows nor denies would be skipped silently on every
	// request, which looks exactly like a rule that never matches.
	raw := encodeBundle(t, &bundleSpec{rules: []ruleSpec{{id: "broken"}}})
	if _, err := policy.Decode(raw); err == nil {
		t.Fatal("a rule with no effect was accepted")
	}
}

func TestDecodeRejectsAnUnparseableNetwork(t *testing.T) {
	// Rejected at decode rather than skipped: a network rule that silently
	// does not apply is a restriction an operator believes is in force.
	raw := encodeBundle(t, &bundleSpec{rules: []ruleSpec{
		{id: "bad-net", allow: true, cidrs: []string{"not-a-cidr"}},
	}})
	if _, err := policy.Decode(raw); err == nil {
		t.Fatal("an unparseable CIDR was accepted")
	}
}

func TestDecodeOfNothingIsAnEmptyBundle(t *testing.T) {
	bundle, err := policy.Decode(nil)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !bundle.Empty() {
		t.Fatal("decoding nothing produced rules")
	}
}

func TestTheCacheIsInvalidatedByASnapshotVersion(t *testing.T) {
	// Keeping a bundle decoded against an older snapshot would mean policy
	// silently lagging configuration.
	cache := policy.NewCache()
	first := encodeBundle(t, &bundleSpec{rules: []ruleSpec{{id: "a", allow: true}}})
	second := encodeBundle(t, &bundleSpec{rules: []ruleSpec{{id: "b", allow: true}}})

	if _, err := cache.For("acme", 1, first); err != nil {
		t.Fatalf("For: %v", err)
	}
	bundle, err := cache.For("acme", 2, second)
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if bundle.Rules[0].ID != "b" {
		t.Fatalf("rule = %q, want the new snapshot's bundle", bundle.Rules[0].ID)
	}
}

func TestTheCacheReturnsTheSameBundleWithinAVersion(t *testing.T) {
	cache := policy.NewCache()
	raw := encodeBundle(t, &bundleSpec{rules: []ruleSpec{{id: "a", allow: true}}})

	first, err := cache.For("acme", 1, raw)
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	second, err := cache.For("acme", 1, raw)
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if first.Rules[0].ID != second.Rules[0].ID {
		t.Fatal("the cache returned a different bundle within one version")
	}
}
