package config_test

import (
	"strings"
	"testing"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/config"
)

func env(pairs map[string]string) config.Getenv {
	return func(k string) string { return pairs[k] }
}

var validPepper = strings.Repeat("p", 32)

func TestLoadAppliesDefaults(t *testing.T) {
	cfg, err := config.Load(env(map[string]string{
		"GATEWAY_SNAPSHOT_FILE": "/etc/gateway/snapshot.pb",
		"GATEWAY_KEY_PEPPER":    validPepper,
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenAddr != config.DefaultListenAddr {
		t.Fatalf("ListenAddr = %q", cfg.ListenAddr)
	}
}

func TestLoadRejectsIncompleteConfiguration(t *testing.T) {
	tests := map[string]map[string]string{
		"no snapshot file": {"GATEWAY_KEY_PEPPER": validPepper},
		"no pepper":        {"GATEWAY_SNAPSHOT_FILE": "/s.pb"},
		// A short pepper is worse than a missing one: it looks configured.
		"short pepper": {"GATEWAY_SNAPSHOT_FILE": "/s.pb", "GATEWAY_KEY_PEPPER": "too-short"},
	}
	for name, vars := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := config.Load(env(vars)); err == nil {
				t.Fatal("expected the worker to refuse to start")
			}
		})
	}
}

func TestStringRedactsThePepper(t *testing.T) {
	// A config dump in a log must not be a credential leak.
	cfg, err := config.Load(env(map[string]string{
		"GATEWAY_SNAPSHOT_FILE": "/s.pb", "GATEWAY_KEY_PEPPER": validPepper,
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if strings.Contains(cfg.String(), validPepper) {
		t.Fatalf("String() leaked the pepper: %s", cfg.String())
	}
}
