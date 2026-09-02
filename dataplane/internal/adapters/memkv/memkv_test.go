package memkv_test

import (
	"sync"
	"testing"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/adapters/memkv"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/contracts"
)

func TestSatisfiesKVStore(t *testing.T) {
	contracts.RunKVStoreSuite(contracts.Adapt(t), func(contracts.T) contracts.KVTarget {
		// A fresh store per case, and no prefix needed: nothing else shares it.
		// The clock is hand-wound so expiry is exercised rather than waited for.
		now := time.Now()
		var mu sync.Mutex
		read := func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		}

		return contracts.KVTarget{
			Store: memkv.New(memkv.WithClock(read)),
			Advance: func(d time.Duration) {
				mu.Lock()
				defer mu.Unlock()
				now = now.Add(d)
			},
		}
	})
}

func TestSweepRemovesExpiredEntries(t *testing.T) {
	// Lazy expiry on read is enough for correctness, but a rate-limit key is
	// per window and is never read again once the window rolls — so the stale
	// set grows with time rather than with traffic.
	store := memkv.New()
	if err := store.Set(t.Context(), "a", []byte("v"), -1); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := store.Set(t.Context(), "b", []byte("v"), 0); err != nil {
		t.Fatalf("Set: %v", err)
	}

	if store.Len() != 2 {
		t.Fatalf("Len = %d, want 2 before sweeping", store.Len())
	}
	if removed := store.Sweep(); removed == 0 {
		t.Fatal("Sweep removed nothing")
	}
}
