package httpapi

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxModelNameBytes bounds a caller-supplied model name.
//
// The longest real model identifier is well under a hundred bytes. Anything
// larger is a mistake or an attempt to bloat logs and metrics, and it is
// cheaper to refuse it at the door than to carry it through routing.
const maxModelNameBytes = 256

// maxLoggedValueBytes bounds any caller-controlled string in a log field, so a
// single request cannot produce an unbounded amount of log.
const maxLoggedValueBytes = 256

// validModelName reports whether a caller-supplied model name is one we are
// willing to handle.
//
// Rejecting control characters here is what stops caller input reaching a log,
// a metric label or an error message with a newline in it. Doing it at the
// boundary means every stage downstream can treat the name as ordinary text.
func validModelName(name string) bool {
	if name == "" || len(name) > maxModelNameBytes {
		return false
	}
	if !utf8.ValidString(name) {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// logSafe makes a caller-controlled string safe to place in a log field.
//
// This is defence in depth rather than the primary control: caller input is
// already validated at the boundary. It exists because the log handler is a
// configuration choice — slog's JSON handler escapes control characters, but a
// text handler does not, so a value containing a newline could forge a second
// log entry. Sanitising at the call site keeps the log safe whichever handler
// is installed, and keeps that guarantee from depending on a validation rule
// somewhere else in the file.
func logSafe(s string) string {
	s = truncateRunes(s, maxLoggedValueBytes)

	// Carriage return and line feed are the two characters that actually forge
	// a log entry, and they are replaced explicitly rather than left to the
	// general pass below. The behaviour is identical either way; the difference
	// is that a direct replacement is a form static analysers recognise as a
	// sanitizer. A control a scanner cannot see is one that gets re-reported
	// every release until somebody dismisses it, and dismissed findings are how
	// a real one gets missed.
	s = strings.ReplaceAll(s, "\r", "\\r")
	s = strings.ReplaceAll(s, "\n", "\\n")

	// Everything else non-printable — NUL, terminal escape sequences — becomes
	// a replacement character. These do not forge entries but they do corrupt
	// terminals and log viewers.
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '�'
		}
		return r
	}, s)
}

// truncateRunes cuts a string to at most limit bytes without splitting a rune,
// which would otherwise produce invalid UTF-8 in the log.
func truncateRunes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
