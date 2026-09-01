// Package injectionheuristics looks for prompt-injection attempts and says so.
//
// # It cannot block, and that is the point
//
// The design is explicit about this: prompt-injection protection at a gateway
// is largely ineffective against a determined attacker. Any pattern list is
// trivially paraphrased around, so a blocking control built on one refuses
// legitimate requests while stopping nobody who is actually trying.
//
// Shipping it as detection-and-logging is not a weaker version of a real
// control — it is the honest version. It should be bound as non-blocking and
// fail-open, and this package returns Deny only as an *alert* verdict, which
// the chain records rather than acts on.
//
// Stated plainly so nobody later "upgrades" it to blocking and believes the
// gateway is protected.
package injectionheuristics

import (
	"bytes"
	"context"
	"regexp"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

// Name is how a snapshot binds this guardrail.
const Name = "injection-heuristics"

// maxScanBytes bounds the work, as for any per-request inspection.
const maxScanBytes = 256 << 10

// signals are phrasings that commonly precede an injection attempt. They are
// evidence, not proof — which is exactly why the verdict is an alert.
var signals = []*regexp.Regexp{
	regexp.MustCompile(`(?i)ignore (?:all |any )?(?:previous|prior|above) instructions`),
	regexp.MustCompile(`(?i)disregard (?:all |any )?(?:previous|prior|above)`),
	regexp.MustCompile(`(?i)you are now (?:a|an|in) [a-z ]{0,20}(?:mode|assistant|character)`),
	regexp.MustCompile(`(?i)reveal (?:your |the )?(?:system prompt|instructions|initial prompt)`),
	regexp.MustCompile(`(?i)repeat (?:the |your )?(?:system|initial) (?:prompt|message)`),
	regexp.MustCompile(`(?i)\bDAN\b.{0,40}\bjailbreak\b`),
	regexp.MustCompile(`(?i)pretend (?:that )?you (?:are|have) no (?:restrictions|rules|guidelines)`),
}

// Guardrail flags likely injection attempts. Stateless and concurrent-safe.
type Guardrail struct{}

// New returns the guardrail.
func New() *Guardrail { return &Guardrail{} }

// Name identifies the guardrail in a snapshot binding.
func (*Guardrail) Name() string { return Name }

// Inspect reports a signal if one is present.
//
// The verdict is Deny so that a chain running this as blocking would refuse —
// but it is intended to be bound non-blocking, where the chain records the
// alert and the request proceeds. Returning Allow with a reason would make the
// alert indistinguishable from a clean payload in every consumer.
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
	// Lowercased once rather than per pattern; the patterns are already
	// case-insensitive, and this keeps the cost linear in payload size.
	lowered := bytes.ToLower(payload)

	for _, signal := range signals {
		if signal.Match(lowered) {
			return &core.GuardrailResult{
				Verdict: core.VerdictDeny,
				Reason:  "possible prompt injection",
			}, nil
		}
	}
	return &core.GuardrailResult{Verdict: core.VerdictAllow}, nil
}
