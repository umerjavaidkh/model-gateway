package pii

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

// Strategy is what to do with detected data.
type Strategy string

const (
	// StrategyNone leaves the payload alone. For destinations inside the trust
	// boundary, where redacting costs accuracy and buys nothing.
	StrategyNone Strategy = "none"
	// StrategyRedact replaces a value with a placeholder and does not keep the
	// original. The right default: classification, summarisation and extraction
	// do not need the real values back, and a vault they do not need is a
	// database of personal data with a retention policy.
	StrategyRedact Strategy = "redact"
	// StrategyHash replaces a value with a stable keyed digest, so the same
	// person is the same token across requests without the original being
	// recoverable. For analytics over pseudonymous data.
	StrategyHash Strategy = "hash"
	// StrategyTokenize replaces a value with a placeholder and stores the
	// original so the response can be restored. Only earns its complexity when
	// the response genuinely needs the real values back.
	StrategyTokenize Strategy = "tokenize"
)

// Placeholders are bracketed uppercase ASCII with an underscore and a digit.
//
// The shape is chosen against a specific failure: models paraphrase, translate
// and reformat. A placeholder built from punctuation or mixed case comes back
// altered and restoration silently fails, leaving the caller a token where they
// expected a name. Uppercase ASCII in double brackets survives almost every
// transformation a model applies, and the round-trip is asserted per provider
// in the test suite rather than assumed.
const (
	placeholderOpen  = "[["
	placeholderClose = "]]"
	// MaxPlaceholderLen bounds how much of a streamed response must be held
	// back to catch a placeholder split across chunks.
	MaxPlaceholderLen = 64
)

var placeholderPattern = regexp.MustCompile(`\[\[[A-Z_]+_\d+\]\]`)

// Result is a transformed payload and what it took to produce.
type Result struct {
	Payload []byte
	// Replacements maps placeholder to original. Empty unless the strategy was
	// tokenize; nothing else needs to be reversible.
	Replacements map[string]string
	// Count is how many values were replaced, for metrics and the audit
	// record. The values themselves never appear in either.
	Count int
}

// Vault stores originals for the duration of a request.
//
// Backed by a KVStore, which in production is Redis. Entries are written under
// a per-tenant prefix with a TTL a little longer than the request deadline, and
// are never written to any durable log — the whole point is that the mapping
// outlives the request by seconds, not by a retention policy.
type Vault struct {
	store core.KVStore
	ttl   time.Duration
}

// NewVault returns a vault over a store.
func NewVault(store core.KVStore, ttl time.Duration) (*Vault, error) {
	if store == nil {
		return nil, core.New(core.CodeInternal, "a token vault needs a store")
	}
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &Vault{store: store, ttl: ttl}, nil
}

// Put stores the originals for one request.
func (v *Vault) Put(
	ctx context.Context, tenant core.TenantID, requestID string, replacements map[string]string,
) error {
	for placeholder, original := range replacements {
		key := vaultKey(tenant, requestID, placeholder)
		if err := v.store.Set(ctx, key, []byte(original), v.ttl); err != nil {
			// Fails closed. A tokenised request whose originals were not stored
			// would return placeholders to the caller, which is a silently
			// wrong answer rather than a visible failure.
			return core.Wrap(core.CodeUnavailable, err, "storing tokenised values")
		}
	}
	return nil
}

// Get retrieves one original.
func (v *Vault) Get(
	ctx context.Context, tenant core.TenantID, requestID, placeholder string,
) (string, bool) {
	raw, found, err := v.store.Get(ctx, vaultKey(tenant, requestID, placeholder))
	if err != nil || !found {
		return "", false
	}
	return string(raw), true
}

// Forget removes a request's originals.
//
// Called when the response has been restored. The TTL is the backstop; this is
// the intent, and the difference matters because the backstop is measured in
// minutes and this is measured in the life of one request.
func (v *Vault) Forget(
	ctx context.Context, tenant core.TenantID, requestID string, replacements map[string]string,
) {
	for placeholder := range replacements {
		_ = v.store.Delete(ctx, vaultKey(tenant, requestID, placeholder))
	}
}

func vaultKey(tenant core.TenantID, requestID, placeholder string) string {
	return "pii:" + string(tenant) + ":" + requestID + ":" + placeholder
}

// Transform replaces detected values according to the strategy.
//
// The pepper keys the hash strategy. It is the same secret that keys API key
// lookup, so a hashed identifier is stable across the fleet and useless to
// anyone holding only the output.
func Transform(payload []byte, strategy Strategy, pepper []byte) Result {
	return TransformMatches(payload, Detect(payload), strategy, pepper)
}

// TransformMatches replaces already-detected values.
//
// Separate from Transform so the statistical tier's findings can be merged with
// the deterministic tier's before anything is replaced. Transforming twice
// would place a placeholder inside a placeholder.
func TransformMatches(payload []byte, matches []Match, strategy Strategy, pepper []byte) Result {
	if strategy == StrategyNone || strategy == "" {
		return Result{Payload: payload}
	}
	if len(matches) == 0 {
		return Result{Payload: payload}
	}

	var out strings.Builder
	out.Grow(len(payload))
	replacements := make(map[string]string, len(matches))
	counters := map[Kind]int{}

	text := string(payload)
	last := 0
	for _, match := range matches {
		out.WriteString(text[last:match.Start])

		switch strategy {
		case StrategyHash:
			out.WriteString(hashValue(pepper, match))
		case StrategyRedact, StrategyTokenize:
			counters[match.Kind]++
			placeholder := fmt.Sprintf("%s%s_%d%s",
				placeholderOpen, match.Kind, counters[match.Kind], placeholderClose)
			out.WriteString(placeholder)
			if strategy == StrategyTokenize {
				replacements[placeholder] = match.Value
			}
		case StrategyNone:
			out.WriteString(match.Value)
		}
		last = match.End
	}
	out.WriteString(text[last:])

	return Result{Payload: []byte(out.String()), Replacements: replacements, Count: len(matches)}
}

// hashValue produces a stable pseudonym.
//
// Keyed, and truncated to twelve hex characters. Keyed so the mapping cannot be
// reversed by anyone holding a rainbow table of email addresses — an unkeyed
// hash of personal data is personal data. Truncated because the token appears
// in prompts, and a 64-character hash costs tokens and confuses models without
// adding meaningful collision resistance at this scale.
func hashValue(pepper []byte, match Match) string {
	mac := hmac.New(sha256.New, pepper)
	mac.Write([]byte(match.Value))
	digest := hex.EncodeToString(mac.Sum(nil))[:12]
	return fmt.Sprintf("%s%s_%s%s", placeholderOpen, match.Kind, digest, placeholderClose)
}

// Restore puts originals back into a completed response.
func Restore(payload []byte, replacements map[string]string) []byte {
	if len(replacements) == 0 {
		return payload
	}

	text := string(payload)
	// Replaced by scanning for the placeholder shape rather than by iterating
	// the map, so a model that emitted a placeholder we never issued is left
	// alone rather than matching some other request's token.
	restored := placeholderPattern.ReplaceAllStringFunc(text, func(placeholder string) string {
		if original, ok := replacements[placeholder]; ok {
			return original
		}
		return placeholder
	})
	return []byte(restored)
}
