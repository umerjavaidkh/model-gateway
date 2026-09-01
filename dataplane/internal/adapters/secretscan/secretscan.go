// Package secretscan refuses payloads containing credentials.
//
// It is deterministic and blocking, and it fails closed. That combination is
// what the design reserves for controls whose failure is not recoverable: a
// credential that reaches a third-party provider has been disclosed, and no
// later action undoes it. Everything else — anything statistical, anything
// whose accuracy does not justify refusing real traffic — belongs off the
// request path.
//
// The patterns are deliberately narrow. A scanner that fires on anything
// resembling a token refuses legitimate requests, and a guardrail that cries
// wolf gets disabled — at which point it protects nothing at all.
package secretscan

import (
	"context"
	"regexp"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

// Name is how a snapshot binds this guardrail.
const Name = "secret-scan"

// maxScanBytes bounds the work. A guardrail on every request must not scale
// with payload size without limit; a credential pasted into a prompt is at the
// start of it far more often than buried megabytes in.
const maxScanBytes = 256 << 10

// pattern is one credential shape worth refusing.
type pattern struct {
	name string
	re   *regexp.Regexp
}

// patterns are anchored on the issuer's own prefix wherever one exists, which
// is what keeps false positives low enough for a blocking control. A generic
// "long random string" rule would refuse half the base64 in the world.
var patterns = []pattern{
	{"aws-access-key", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"github-token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}\b`)},
	{"slack-token", regexp.MustCompile(`\bxox[baprs]-[0-9A-Za-z-]{10,}\b`)},
	{"google-api-key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
	{"private-key", regexp.MustCompile(`-----BEGIN (?:RSA |EC |OPENSSH |PGP )?PRIVATE KEY-----`)},
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`)},
	// The gateway's own keys. A caller pasting one into a prompt would leak
	// the credential to the very provider the gateway exists to stand between
	// them and.
	{"gateway-key", regexp.MustCompile(`\bgw_[A-Za-z0-9]+_[A-Za-z0-9_-]{20,}\b`)},
}

// Guardrail scans for credentials. Stateless and safe for concurrent use.
type Guardrail struct{}

// New returns the guardrail.
func New() *Guardrail { return &Guardrail{} }

// Name identifies the guardrail in a snapshot binding.
func (*Guardrail) Name() string { return Name }

// Inspect refuses a payload containing a recognisable credential.
//
// The reason names the *kind* of credential, never the value or its position.
// A guardrail that echoes what it found writes the secret into whatever logs
// the refusal, which is the outcome it existed to prevent.
func (*Guardrail) Inspect(
	ctx context.Context, in *core.GuardrailInput,
) (*core.GuardrailResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	payload := in.Payload
	if len(payload) > maxScanBytes {
		payload = payload[:maxScanBytes]
	}

	for _, p := range patterns {
		if p.re.Match(payload) {
			return &core.GuardrailResult{
				Verdict: core.VerdictDeny,
				Reason:  "payload contains a credential (" + p.name + ")",
			}, nil
		}
	}
	return &core.GuardrailResult{Verdict: core.VerdictAllow}, nil
}
