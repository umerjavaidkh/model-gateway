package policy_test

import (
	"testing"

	"google.golang.org/protobuf/proto"

	pb "github.com/umerjavaidkh/model-gateway/dataplane/internal/wire/gatewayv1"
)

// The tests build bundles through the wire type, because that is what a
// snapshot actually carries — constructing the decoded form directly would
// skip the decode step several of these cases are about.

type ruleSpec struct {
	id    string
	allow bool
	unset bool
	cidrs []string
}

type bundleSpec struct {
	rules []ruleSpec
}

func encodeBundle(t *testing.T, spec *bundleSpec) []byte {
	t.Helper()

	bundle := &pb.PolicyBundle{Id: "test", Version: 1}
	for _, rule := range spec.rules {
		effect := pb.PolicyEffect_POLICY_EFFECT_DENY
		if rule.allow {
			effect = pb.PolicyEffect_POLICY_EFFECT_ALLOW
		}
		if rule.unset {
			// A rule that neither allows nor denies, for the cases about
			// refusing one.
			effect = pb.PolicyEffect_POLICY_EFFECT_UNSPECIFIED
		}
		bundle.Rules = append(bundle.Rules, &pb.PolicyRule{
			Id: rule.id, Effect: effect, SourceCidrs: rule.cidrs,
		})
	}

	raw, err := proto.Marshal(bundle)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return raw
}
