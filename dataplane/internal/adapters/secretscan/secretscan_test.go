package secretscan_test

import (
	"testing"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/adapters/secretscan"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/contracts"
)

func TestSatisfiesGuardrailPort(t *testing.T) {
	contracts.RunGuardrailSuite(contracts.Adapt(t), func(contracts.T) contracts.GuardrailTarget {
		return contracts.GuardrailTarget{
			Guardrail: secretscan.New(),
			// A syntactically valid AWS key that was never issued. Real-looking
			// on purpose: a trigger the scanner would miss makes the suite
			// assert nothing.
			Trigger: []byte(`{"messages":[{"content":"use AKIAIOSFODNN7EXAMPLE to deploy"}]}`),
			Benign:  []byte(`{"messages":[{"content":"summarise this quarter's revenue"}]}`),
		}
	})
}
