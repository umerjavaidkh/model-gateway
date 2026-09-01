// Package pii detects and replaces personal data before it leaves the trust
// boundary, and restores it on the way back.
//
// # Why this cannot be a normal guardrail
//
// Every other inspection can be moved off the request path if it proves
// expensive. This one cannot: raw data must not cross the boundary at all, and
// a substitution is only useful if it can be reversed on the return leg. Both
// legs, blocking, in the hot path.
//
// # Detection is tiered so NER is not paid for on every request
//
// This file is the deterministic tier: regexes with checksums, running in
// microseconds, always on for external destinations. Names, locations and
// organisations need statistical models, cost tens of milliseconds, and run
// out of process only when policy asks for them — an English NER model misses
// Arabic entities almost entirely, which is why that tier is a sidecar with a
// per-language recogniser registry rather than a library call.
package pii

import (
	"regexp"
	"strings"
)

// Kind is a class of personal data. It becomes part of the placeholder, so a
// model that paraphrases around one is at least paraphrasing around something
// meaningful.
type Kind string

// The deterministic tier's kinds. Names, locations and organisations need
// statistical models and are added by the sidecar tier, not here.
const (
	KindEmail      Kind = "EMAIL"
	KindPhone      Kind = "PHONE"
	KindCard       Kind = "CARD"
	KindIBAN       Kind = "IBAN"
	KindEmiratesID Kind = "EMIRATES_ID"
	KindIPAddress  Kind = "IP"
)

// Match is one detected value and where it was found.
type Match struct {
	Kind  Kind
	Start int
	End   int
	Value string
}

// detector is one deterministic rule.
//
// The validate step is what separates this tier from a guess. A sixteen-digit
// number is not a card number; a sixteen-digit number that passes Luhn very
// probably is. Without the checksum the false-positive rate makes redaction
// destroy ordinary text — order numbers, timestamps, identifiers — and a
// redactor that mangles legitimate content gets turned off.
type detector struct {
	kind     Kind
	re       *regexp.Regexp
	validate func(string) bool
}

var detectors = []detector{
	{
		kind: KindEmail,
		re:   regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`),
	},
	{
		// 784-YYYY-NNNNNNN-C. Anchored on the 784 issuer prefix and checked
		// with Luhn, because the shape alone is just fifteen digits.
		kind:     KindEmiratesID,
		re:       regexp.MustCompile(`\b784-?\d{4}-?\d{7}-?\d\b`),
		validate: func(s string) bool { return luhn(digitsOf(s)) },
	},
	{
		kind:     KindCard,
		re:       regexp.MustCompile(`\b(?:\d[ -]?){12,18}\d\b`),
		validate: func(s string) bool { return luhn(digitsOf(s)) },
	},
	{
		kind:     KindIBAN,
		re:       regexp.MustCompile(`\b[A-Z]{2}\d{2}[A-Z0-9]{10,30}\b`),
		validate: validIBAN,
	},
	{
		// E.164-ish with a leading plus. A bare run of digits is not treated as
		// a phone number: without a country prefix the pattern matches far more
		// non-phone text than phone text.
		kind: KindPhone,
		re:   regexp.MustCompile(`\+\d{1,3}[\s\-]?\(?\d{1,4}\)?[\s\-]?\d{3,4}[\s\-]?\d{3,4}\b`),
	},
	{
		kind:     KindIPAddress,
		re:       regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`),
		validate: validIPv4,
	},
}

// Detect returns every match, in order, with overlaps removed.
//
// Overlaps are real: an Emirates ID is fifteen digits and would also match the
// card pattern. The first detector to claim a span wins, and they are ordered
// most-specific first, so a national identifier is not reported as a payment
// card.
func Detect(payload []byte) []Match {
	text := string(payload)

	var matches []Match
	for _, d := range detectors {
		for _, span := range d.re.FindAllStringIndex(text, -1) {
			value := text[span[0]:span[1]]
			if d.validate != nil && !d.validate(value) {
				continue
			}
			matches = append(matches, Match{Kind: d.kind, Start: span[0], End: span[1], Value: value})
		}
	}
	return withoutOverlaps(matches)
}

// withoutOverlaps keeps the earliest match, and among equals the one found by
// the earlier detector, which is the more specific one.
func withoutOverlaps(matches []Match) []Match {
	if len(matches) < 2 {
		return matches
	}

	// Stable sort by start keeps detector order for equal positions, which is
	// what makes "most specific detector wins" true.
	sortByStart(matches)

	kept := make([]Match, 0, len(matches))
	end := -1
	for _, m := range matches {
		if m.Start < end {
			continue
		}
		kept = append(kept, m)
		end = m.End
	}
	return kept
}

func sortByStart(matches []Match) {
	for i := 1; i < len(matches); i++ {
		for j := i; j > 0 && matches[j].Start < matches[j-1].Start; j-- {
			matches[j], matches[j-1] = matches[j-1], matches[j]
		}
	}
}

func digitsOf(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// luhn is the check digit used by payment cards and by several national
// identifiers.
func luhn(digits string) bool {
	if len(digits) < 12 {
		return false
	}
	sum, double := 0, false
	for i := len(digits) - 1; i >= 0; i-- {
		n := int(digits[i] - '0')
		if double {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
		double = !double
	}
	return sum%10 == 0
}

// validIBAN applies the mod-97 check. Without it the pattern matches most
// uppercase alphanumeric strings that happen to start with two letters.
func validIBAN(s string) bool {
	if len(s) < 15 || len(s) > 34 {
		return false
	}

	// Move the first four characters to the end, then map letters to numbers.
	rearranged := s[4:] + s[:4]
	remainder := 0
	for _, r := range rearranged {
		var value int
		switch {
		case r >= '0' && r <= '9':
			value = int(r - '0')
		case r >= 'A' && r <= 'Z':
			value = int(r-'A') + 10
		default:
			return false
		}
		// Two digits at a time for letters, one for digits, kept as a running
		// remainder so no big-integer arithmetic is needed.
		if value > 9 {
			remainder = (remainder*100 + value) % 97
		} else {
			remainder = (remainder*10 + value) % 97
		}
	}
	return remainder == 1
}

func validIPv4(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	for _, part := range parts {
		if len(part) > 1 && part[0] == '0' {
			return false
		}
		n := 0
		for _, r := range part {
			n = n*10 + int(r-'0')
		}
		if n > 255 {
			return false
		}
	}
	return true
}
