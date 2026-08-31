package core_test

import (
	"testing"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

func TestParseAPIKey(t *testing.T) {
	tests := []struct {
		name       string
		presented  string
		wantPrefix core.KeyPrefix
		wantSecret string
		wantErr    bool
	}{
		{name: "well formed", presented: "gw_acme_s3cr3tvalue", wantPrefix: "acme", wantSecret: "s3cr3tvalue"},
		{name: "secret may contain separators", presented: "gw_acme_a_b_c", wantPrefix: "acme", wantSecret: "a_b_c"},
		{name: "wrong scheme", presented: "sk-1234", wantErr: true},
		{name: "no secret segment", presented: "gw_acme", wantErr: true},
		{name: "empty secret", presented: "gw_acme_", wantErr: true},
		{name: "empty prefix", presented: "gw__secret", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prefix, secret, err := core.ParseAPIKey(tc.presented)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error for %q", tc.presented)
				}
				if got := core.CodeOf(err); got != core.CodeUnauthenticated {
					t.Fatalf("CodeOf = %q, want %q", got, core.CodeUnauthenticated)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if prefix != tc.wantPrefix || secret != tc.wantSecret {
				t.Fatalf("got (%q, %q), want (%q, %q)", prefix, secret, tc.wantPrefix, tc.wantSecret)
			}
		})
	}
}

func TestComputeKeyLookupIsDeterministicAndPeppered(t *testing.T) {
	pepperA, pepperB := []byte("pepper-a"), []byte("pepper-b")

	first, second := core.ComputeKeyLookup(pepperA, "secret"), core.ComputeKeyLookup(pepperA, "secret")
	if first != second {
		t.Fatal("the same pepper and secret must produce the same lookup")
	}
	if core.ComputeKeyLookup(pepperA, "secret") == core.ComputeKeyLookup(pepperB, "secret") {
		t.Fatal("a different pepper must produce a different lookup, or a stolen snapshot is usable")
	}
	if core.ComputeKeyLookup(pepperA, "secret") == core.ComputeKeyLookup(pepperA, "secreu") {
		t.Fatal("a different secret must produce a different lookup")
	}
}

func TestModelAllowlistDeniesByDefault(t *testing.T) {
	// The zero value must be "allow nothing". A builder that forgets to populate
	// the allowlist should fail safe.
	var zero core.ModelAllowlist
	if zero.Permits("gpt-4") {
		t.Fatal("the zero allowlist must permit nothing")
	}

	named := core.ModelAllowlist{Names: []string{"fast", "cheap"}}
	if !named.Permits("fast") || named.Permits("reasoning") {
		t.Fatal("a named allowlist must permit exactly its names")
	}

	if !(core.ModelAllowlist{AllowAll: true}).Permits("anything") {
		t.Fatal("AllowAll must permit anything")
	}
}

func TestPrincipalExpiry(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

	// A rotated key stays valid while deprecated: two generations overlap so
	// that rotation is a non-event for callers.
	deprecated := core.Principal{Deprecated: true, NotAfter: now.Add(time.Hour)}
	if err := deprecated.Validate(now); err != nil {
		t.Fatalf("a deprecated but unexpired key must still work: %v", err)
	}

	expired := core.Principal{NotAfter: now.Add(-time.Second)}
	if err := expired.Validate(now); err == nil {
		t.Fatal("an expired key must be rejected")
	}

	var noExpiry core.Principal
	if err := noExpiry.Validate(now); err != nil {
		t.Fatalf("a key with no expiry must be valid: %v", err)
	}
}
