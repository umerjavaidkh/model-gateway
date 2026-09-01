package secrets_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/secrets"
)

// countingStore records how often the backing store was actually consulted,
// which is the only way to tell caching from correctness.
type countingStore struct {
	calls  atomic.Int64
	secret string
	err    error
}

func (s *countingStore) Fetch(context.Context, string) ([]byte, error) {
	s.calls.Add(1)
	if s.err != nil {
		return nil, s.err
	}
	return []byte(s.secret), nil
}

func TestResolveCachesUntilTheTTLExpires(t *testing.T) {
	// The plan says workers fetch credentials "at startup". That is wrong for a
	// live system: tenants rotate credentials while workers run, and a worker's
	// lifetime is measured in days. Lazy resolution with a TTL means a rotation
	// takes effect without a restart.
	store := &countingStore{secret: "sk-1"}
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	r, err := secrets.NewResolver(store,
		secrets.WithTTL(time.Minute),
		secrets.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	for range 5 {
		got, err := r.Resolve(t.Context(), "env:KEY")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if string(got.Secret) != "sk-1" {
			t.Fatalf("secret = %q", got.Secret)
		}
	}
	if store.calls.Load() != 1 {
		t.Fatalf("store consulted %d times, want 1", store.calls.Load())
	}

	now = now.Add(2 * time.Minute)
	if _, err := r.Resolve(t.Context(), "env:KEY"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if store.calls.Load() != 2 {
		t.Fatalf("store consulted %d times after expiry, want 2", store.calls.Load())
	}
}

func TestInvalidateForcesARefetch(t *testing.T) {
	// This is what turns a rotation from "effective within a TTL" into
	// "effective at the next snapshot".
	store := &countingStore{secret: "sk-1"}
	r, err := secrets.NewResolver(store)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	if _, err := r.Resolve(t.Context(), "env:KEY"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	r.Invalidate("env:KEY")
	if _, err := r.Resolve(t.Context(), "env:KEY"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if store.calls.Load() != 2 {
		t.Fatalf("store consulted %d times, want 2", store.calls.Load())
	}

	r.InvalidateAll()
	if _, err := r.Resolve(t.Context(), "env:KEY"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if store.calls.Load() != 3 {
		t.Fatalf("store consulted %d times after InvalidateAll, want 3", store.calls.Load())
	}
}

func TestAnEmptyReferenceNeedsNoStore(t *testing.T) {
	// A self-hosted deployment inside the network needs no credential, and
	// consulting the store for it would make an unrelated outage break it.
	store := &countingStore{err: errors.New("store is down")}
	r, err := secrets.NewResolver(store)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	got, err := r.Resolve(t.Context(), "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got.Secret) != 0 || store.calls.Load() != 0 {
		t.Fatalf("an empty reference must resolve to nothing without a fetch")
	}
}

func TestAFailedFetchIsNotCached(t *testing.T) {
	// Caching a failure would extend a transient store outage into a TTL-long
	// one for every deployment that happened to be cold.
	store := &countingStore{err: core.New(core.CodeUnavailable, "store is down")}
	r, err := secrets.NewResolver(store)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	for range 3 {
		if _, err := r.Resolve(t.Context(), "env:KEY"); err == nil {
			t.Fatal("expected the failure to surface")
		}
	}
	if store.calls.Load() != 3 {
		t.Fatalf("store consulted %d times, want 3 — a failure must not be cached", store.calls.Load())
	}
}

func TestResolveIsSafeUnderConcurrency(t *testing.T) {
	store := &countingStore{secret: "sk-1"}
	r, err := secrets.NewResolver(store)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				if _, err := r.Resolve(context.Background(), "env:KEY"); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
}

func TestNewResolverRejectsBadConfiguration(t *testing.T) {
	if _, err := secrets.NewResolver(nil); err == nil {
		t.Fatal("a resolver with no store must be refused")
	}
	if _, err := secrets.NewResolver(&countingStore{}, secrets.WithTTL(0)); err == nil {
		t.Fatal("a non-positive TTL must be refused")
	}
}

func TestEnvStore(t *testing.T) {
	store := secrets.NewEnvStoreWithLookup(func(name string) (string, bool) {
		if name == "OPENAI_API_KEY" {
			return "sk-live", true
		}
		return "", false
	})

	got, err := store.Fetch(t.Context(), "env:OPENAI_API_KEY")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(got) != "sk-live" {
		t.Fatalf("secret = %q", got)
	}

	for name, ref := range map[string]string{
		"unknown scheme": "vault://acme/key",
		"no variable":    "env:",
		"unset variable": "env:MISSING",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := store.Fetch(t.Context(), ref); err == nil {
				t.Fatalf("expected %q to fail", ref)
			}
		})
	}
}

func TestAnUnsetVariableNamesItselfInTheError(t *testing.T) {
	// The variable name is operator configuration, not a secret, and a missing
	// credential is otherwise near-impossible to diagnose from a 502.
	store := secrets.NewEnvStoreWithLookup(func(string) (string, bool) { return "", false })

	_, err := store.Fetch(t.Context(), "env:ANTHROPIC_API_KEY")
	if err == nil || !errors.Is(err, core.ErrUnavailable) {
		t.Fatalf("err = %v, want unavailable", err)
	}
	if got := err.Error(); !strings.Contains(got, "ANTHROPIC_API_KEY") {
		t.Fatalf("error does not name the variable: %q", got)
	}
}
