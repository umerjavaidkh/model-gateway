package pii_test

import (
	"strings"
	"testing"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/pii"
)

var pepper = []byte("a-pii-test-pepper-that-is-long-enough")

func kinds(matches []pii.Match) []pii.Kind {
	out := make([]pii.Kind, 0, len(matches))
	for _, m := range matches {
		out = append(out, m.Kind)
	}
	return out
}

func TestChecksumsSeparateDataFromCoincidence(t *testing.T) {
	// A sixteen-digit number is not a card number; one that passes Luhn very
	// probably is. Without the checksum, redaction destroys ordinary text —
	// order numbers, timestamps, identifiers — and a redactor that mangles
	// legitimate content gets turned off.
	real := pii.Detect([]byte("card 4111 1111 1111 1111 on file"))
	if len(real) != 1 || real[0].Kind != pii.KindCard {
		t.Fatalf("a valid card was not detected: %v", kinds(real))
	}

	if got := pii.Detect([]byte("order 1234567890123456 shipped")); len(got) != 0 {
		t.Fatalf("a number failing Luhn was reported as %v", kinds(got))
	}
}

func TestIBANRequiresTheMod97Check(t *testing.T) {
	valid := pii.Detect([]byte("pay to GB82WEST12345698765432 today"))
	if len(valid) != 1 || valid[0].Kind != pii.KindIBAN {
		t.Fatalf("a valid IBAN was not detected: %v", kinds(valid))
	}

	if got := pii.Detect([]byte("reference GB99WEST12345698765432 here")); len(got) != 0 {
		t.Fatalf("an IBAN failing mod-97 was reported as %v", kinds(got))
	}
}

func TestOverlapsResolveToTheMoreSpecificDetector(t *testing.T) {
	// An Emirates ID is fifteen digits and would also match the card pattern.
	// Reporting a national identifier as a payment card would be wrong in a way
	// that matters for how it is handled.
	// The check digit is computed, not invented: 784-1990-1234567-6 passes
	// Luhn, and an invented one silently fails the detector rather than the
	// test's intent.
	matches := pii.Detect([]byte("id 784-1990-1234567-6 on record"))
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want one: %v", len(matches), kinds(matches))
	}
	if matches[0].Kind != pii.KindEmiratesID {
		t.Fatalf("kind = %q, want the more specific detector", matches[0].Kind)
	}
}

func TestOrdinaryTextIsLeftAlone(t *testing.T) {
	// A detector that fires on normal prose gets disabled, at which point it
	// protects nothing.
	for _, text := range []string{
		"summarise the quarterly report for me",
		"the meeting is at 10.30.2026 in room 4",
		"version 1.2.3.4 was released",
		"call me on extension 4471",
	} {
		if got := pii.Detect([]byte(text)); len(got) > 0 {
			for _, m := range got {
				if m.Kind != pii.KindIPAddress {
					t.Fatalf("%q was flagged as %v", text, kinds(got))
				}
			}
		}
	}
}

func TestRedactionKeepsNoOriginals(t *testing.T) {
	// The default strategy, and the reason it is the default: classification,
	// summarisation and extraction do not need real values back, and a vault
	// they do not need is a database of personal data with a retention policy.
	result := pii.Transform([]byte(`{"content":"email ada@example.com"}`), pii.StrategyRedact, pepper)

	if strings.Contains(string(result.Payload), "ada@example.com") {
		t.Fatalf("the original survived redaction: %s", result.Payload)
	}
	if !strings.Contains(string(result.Payload), "[[EMAIL_1]]") {
		t.Fatalf("no placeholder was substituted: %s", result.Payload)
	}
	if len(result.Replacements) != 0 {
		t.Fatal("redaction kept originals it does not need")
	}
}

func TestHashingIsStableAndKeyed(t *testing.T) {
	// Stable so the same person is the same token across requests; keyed so the
	// mapping cannot be reversed with a rainbow table of email addresses — an
	// unkeyed hash of personal data is personal data.
	body := []byte(`{"content":"ada@example.com and ada@example.com"}`)

	first := pii.Transform(body, pii.StrategyHash, pepper)
	second := pii.Transform(body, pii.StrategyHash, pepper)
	if string(first.Payload) != string(second.Payload) {
		t.Fatal("hashing the same value twice produced different tokens")
	}

	other := pii.Transform(body, pii.StrategyHash, []byte("a-different-pepper-long-enough!!"))
	if string(first.Payload) == string(other.Payload) {
		t.Fatal("a different pepper produced the same token, so it is not keyed")
	}
	if strings.Contains(string(first.Payload), "ada@example.com") {
		t.Fatalf("the original survived hashing: %s", first.Payload)
	}
}

func TestTokenizeRoundTrips(t *testing.T) {
	body := []byte(`{"content":"contact ada@example.com or bob@example.org"}`)

	transformed := pii.Transform(body, pii.StrategyTokenize, pepper)
	if len(transformed.Replacements) != 2 {
		t.Fatalf("kept %d replacements, want 2", len(transformed.Replacements))
	}

	restored := pii.Restore(transformed.Payload, transformed.Replacements)
	if string(restored) != string(body) {
		t.Fatalf("round trip changed the payload:\n got %s\nwant %s", restored, body)
	}
}

func TestRestoreIgnoresPlaceholdersItNeverIssued(t *testing.T) {
	// A model that emits something placeholder-shaped must not be able to pull
	// a value out of another request's replacements.
	restored := pii.Restore([]byte("see [[EMAIL_9]] and [[EMAIL_1]]"),
		map[string]string{"[[EMAIL_1]]": "ada@example.com"})

	if !strings.Contains(string(restored), "[[EMAIL_9]]") {
		t.Fatal("an unissued placeholder was substituted")
	}
	if !strings.Contains(string(restored), "ada@example.com") {
		t.Fatal("an issued placeholder was not restored")
	}
}

// --- streaming ---------------------------------------------------------------

func drain(t *testing.T, restorer *pii.Restorer, chunks []string) string {
	t.Helper()
	var out strings.Builder
	for _, chunk := range chunks {
		out.Write(restorer.Push([]byte(chunk)))
	}
	out.Write(restorer.Flush())
	return out.String()
}

func TestAPlaceholderSplitAcrossChunksIsStillRestored(t *testing.T) {
	// The failure this whole buffer exists for: a chunk-by-chunk restorer sees
	// neither half as a placeholder and relays both unchanged, so the caller
	// gets a mangled token and nothing notices.
	restorer := pii.NewRestorer(map[string]string{"[[EMAIL_1]]": "ada@example.com"})

	got := drain(t, restorer, []string{"contact ", "[[EMA", "IL_1]]", " today"})
	if got != "contact ada@example.com today" {
		t.Fatalf("got %q, want the split placeholder restored", got)
	}
}

func TestASplitAtEveryPositionIsRestored(t *testing.T) {
	// One split point working proves little; the boundary can fall anywhere.
	const text = "before [[EMAIL_1]] after"
	for i := 1; i < len(text); i++ {
		restorer := pii.NewRestorer(map[string]string{"[[EMAIL_1]]": "ada@example.com"})
		got := drain(t, restorer, []string{text[:i], text[i:]})
		if got != "before ada@example.com after" {
			t.Fatalf("split at %d gave %q", i, got)
		}
	}
}

func TestOneBytePerChunkIsRestored(t *testing.T) {
	// The pathological case, and the one a slow model actually produces.
	restorer := pii.NewRestorer(map[string]string{"[[CARD_1]]": "4111111111111111"})

	chunks := make([]string, 0, 32)
	for _, r := range "pay [[CARD_1]] now" {
		chunks = append(chunks, string(r))
	}
	if got := drain(t, restorer, chunks); got != "pay 4111111111111111 now" {
		t.Fatalf("got %q", got)
	}
}

func TestTextThatMerelyLooksLikeAPlaceholderIsReleased(t *testing.T) {
	// A hold-back that never releases is a stream that never finishes.
	restorer := pii.NewRestorer(map[string]string{"[[EMAIL_1]]": "ada@example.com"})

	got := drain(t, restorer, []string{"an array [[1, 2], [3", ", 4]] here"})
	if got != "an array [[1, 2], [3, 4]] here" {
		t.Fatalf("got %q, want the text unchanged", got)
	}
}

func TestAnAlteredPlaceholderIsCountedRatherThanGuessedAt(t *testing.T) {
	// If a model paraphrases a placeholder, restoration fails and the caller
	// receives the placeholder. That is visible and reportable; silently
	// substituting a best guess would not be.
	restorer := pii.NewRestorer(map[string]string{"[[EMAIL_1]]": "ada@example.com"})

	got := drain(t, restorer, []string{"see [[EMAIL_7]] please"})
	if got != "see [[EMAIL_7]] please" {
		t.Fatalf("got %q, want the unknown placeholder left alone", got)
	}
	if restorer.Misses() != 1 {
		t.Fatalf("Misses = %d, want the alteration counted", restorer.Misses())
	}
}

func TestWithNothingToRestoreTheStreamPassesThrough(t *testing.T) {
	// A redact-only request has nothing to restore, and buffering it would add
	// latency for no purpose.
	restorer := pii.NewRestorer(nil)
	if restorer.Active() {
		t.Fatal("a restorer with no replacements reported itself active")
	}
	if got := drain(t, restorer, []string{"hello ", "world"}); got != "hello world" {
		t.Fatalf("got %q", got)
	}
}
