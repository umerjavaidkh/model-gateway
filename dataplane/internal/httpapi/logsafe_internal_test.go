package httpapi

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestValidModelName(t *testing.T) {
	valid := []string{"echo-model", "gpt-4o-mini", "llama-3.3-70b", "meta/llama:70b", "ada_002"}
	for _, name := range valid {
		if !validModelName(name) {
			t.Fatalf("%q should be accepted", name)
		}
	}

	invalid := map[string]string{
		"empty":     "",
		"newline":   "a\nb",
		"tab":       "a\tb",
		"null":      "a\x00b",
		"escape":    "a\x1bb",
		"too long":  strings.Repeat("m", maxModelNameBytes+1),
		"bad utf-8": "echo\xff\xfe",
	}
	for reason, name := range invalid {
		if validModelName(name) {
			t.Fatalf("%s should be rejected: %q", reason, name)
		}
	}
}

func TestLogSafe(t *testing.T) {
	// A value that forges a log entry under a text handler must not survive.
	forged := "model\nlevel=INFO msg=\"you have been owned\""
	got := logSafe(forged)
	if strings.ContainsAny(got, "\n\r") {
		t.Fatalf("logSafe left a line break in %q", got)
	}

	if got := logSafe("gpt-4o"); got != "gpt-4o" {
		t.Fatalf("logSafe altered an ordinary value: %q", got)
	}
}

func TestLogSafeBoundsLengthWithoutSplittingARune(t *testing.T) {
	// Truncating mid-rune would put invalid UTF-8 into the log, which some
	// aggregators drop and others mangle.
	long := strings.Repeat("é", maxLoggedValueBytes)
	got := logSafe(long)

	if len(got) > maxLoggedValueBytes+len("…") {
		t.Fatalf("logSafe returned %d bytes, want at most %d", len(got), maxLoggedValueBytes+len("…"))
	}
	if !utf8.ValidString(got) {
		t.Fatal("logSafe produced invalid UTF-8")
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatal("a truncated value must show that it was truncated")
	}
}
