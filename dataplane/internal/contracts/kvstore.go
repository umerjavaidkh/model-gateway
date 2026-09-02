package contracts

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

// KVTarget is a store to run the suite against.
type KVTarget struct {
	Store core.KVStore
	// Prefix isolates this run from anything else in the same store. A prefix
	// rather than a fresh store, because a real Redis is shared: two runs
	// against one instance must not see each other's counters, and flushing
	// would take out whatever else is using it.
	Prefix string
	// Advance moves the store's notion of time forward, for the cases about
	// expiry.
	//
	// It exists because the three implementations disagree about what time is:
	// the in-process store takes an injectable clock, an emulator has to be
	// told explicitly, and a real server only knows wall time. Without this the
	// suite could only test expiry by sleeping, which is slow against a real
	// server and simply wrong against an emulator that never advances on its
	// own — the failure looks identical to a broken TTL.
	//
	// Leave it nil to have the suite sleep instead.
	Advance func(time.Duration)
}

// KVFactory builds the store under test.
type KVFactory func(t T) KVTarget

// RunKVStoreSuite asserts the behaviour every KVStore must have.
//
// It is what keeps the in-process store and Redis interchangeable. The limiter
// leases permits through this interface and cannot tell them apart, so a
// difference between them is a difference in how limits are enforced — which is
// exactly the kind of thing that shows up as a production-only discrepancy.
func RunKVStoreSuite(t T, newStore KVFactory) {
	t.Helper()

	t.Run("a missing key is absent, not an error", func(t T) {
		target := newStore(t)
		store, prefix := target.Store, target.Prefix
		value, found, err := store.Get(t.Context(), prefix+"missing")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if found || value != nil {
			t.Fatalf("got (%q, %v), want absent", value, found)
		}
	})

	t.Run("set then get", func(t T) {
		target := newStore(t)
		store, prefix := target.Store, target.Prefix
		key := prefix + "set"

		if err := store.Set(t.Context(), key, []byte("value"), time.Minute); err != nil {
			t.Fatalf("Set: %v", err)
		}
		value, found, err := store.Get(t.Context(), key)
		if err != nil || !found {
			t.Fatalf("Get = (%q, %v, %v)", value, found, err)
		}
		if string(value) != "value" {
			t.Fatalf("value = %q", value)
		}
	})

	t.Run("incr creates and accumulates", func(t T) {
		target := newStore(t)
		store, prefix := target.Store, target.Prefix
		key := prefix + "incr"

		for i := 1; i <= 3; i++ {
			total, err := store.Incr(t.Context(), key, 5, time.Minute)
			if err != nil {
				t.Fatalf("Incr: %v", err)
			}
			if want := int64(i * 5); total != want {
				t.Fatalf("Incr returned %d, want the running total %d", total, want)
			}
		}
	})

	t.Run("a counter reads back as its decimal text", func(t T) {
		// The limiter reads counters with Get and parses them. If one store
		// wrote binary and another decimal, a window would look empty on one
		// and full on the other.
		target := newStore(t)
		store, prefix := target.Store, target.Prefix
		key := prefix + "text"

		if _, err := store.Incr(t.Context(), key, 42, time.Minute); err != nil {
			t.Fatalf("Incr: %v", err)
		}
		raw, found, err := store.Get(t.Context(), key)
		if err != nil || !found {
			t.Fatalf("Get = (%q, %v, %v)", raw, found, err)
		}
		parsed, err := strconv.ParseInt(string(raw), 10, 64)
		if err != nil {
			t.Fatalf("a counter must be readable as decimal text, got %q", raw)
		}
		if parsed != 42 {
			t.Fatalf("parsed %d, want 42", parsed)
		}
	})

	t.Run("incr does not extend an existing expiry", func(t T) {
		// The property a rate-limit window depends on. If every increment
		// pushed the expiry out, a continuously busy key would never expire and
		// its window would never roll — so the busiest principal, the one the
		// limit exists for, would be the one it stopped applying to.
		target := newStore(t)
		store, prefix := target.Store, target.Prefix
		key := prefix + "ttl"

		const life = 2 * time.Second
		if _, err := store.Incr(t.Context(), key, 1, life); err != nil {
			t.Fatalf("Incr: %v", err)
		}
		// A second increment asking for a much longer life must not get it.
		if _, err := store.Incr(t.Context(), key, 1, time.Hour); err != nil {
			t.Fatalf("Incr: %v", err)
		}

		advance(t, target, life+time.Second)

		if _, found, err := store.Get(t.Context(), key); err != nil {
			t.Fatalf("Get: %v", err)
		} else if found {
			t.Fatal("the key outlived its original TTL, so a busy window would never roll")
		}
	})

	t.Run("delete removes", func(t T) {
		target := newStore(t)
		store, prefix := target.Store, target.Prefix
		key := prefix + "delete"

		if err := store.Set(t.Context(), key, []byte("v"), time.Minute); err != nil {
			t.Fatalf("Set: %v", err)
		}
		if err := store.Delete(t.Context(), key); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, found, _ := store.Get(t.Context(), key); found {
			t.Fatal("the key survived deletion")
		}
		// Deleting something absent is not an error; a caller cleaning up
		// should not have to check first.
		if err := store.Delete(t.Context(), key); err != nil {
			t.Fatalf("second Delete: %v", err)
		}
	})

	t.Run("incr is atomic under concurrency", func(t T) {
		// The limiter leases permits with Incr from every worker at once. A
		// lost update here is over-admission that no amount of tuning explains.
		target := newStore(t)
		store, prefix := target.Store, target.Prefix
		key := prefix + "atomic"

		const goroutines, each = 16, 50
		var wg sync.WaitGroup
		for range goroutines {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range each {
					if _, err := store.Incr(context.Background(), key, 1, time.Minute); err != nil {
						t.Error(err)
					}
				}
			}()
		}
		wg.Wait()

		raw, found, err := store.Get(t.Context(), key)
		if err != nil || !found {
			t.Fatalf("Get = (%q, %v, %v)", raw, found, err)
		}
		total, err := strconv.ParseInt(string(raw), 10, 64)
		if err != nil {
			t.Fatalf("parsing the counter: %v", err)
		}
		if want := int64(goroutines * each); total != want {
			t.Fatalf("counter = %d, want %d; increments were lost", total, want)
		}
	})

	t.Run("a cancelled context is honoured", func(t T) {
		target := newStore(t)
		store, prefix := target.Store, target.Prefix
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		// An in-process store may legitimately succeed, having nothing to wait
		// on. What must not happen is a hang or a panic.
		_, _, err := store.Get(ctx, prefix+"cancelled")
		if err != nil && !errors.Is(err, context.Canceled) && core.CodeOf(err) == "" {
			t.Fatalf("Get returned an unclassified error: %v", err)
		}
	})
}

// advance moves the target's clock forward, sleeping when it has none.
func advance(t T, target KVTarget, d time.Duration) {
	t.Helper()
	if target.Advance != nil {
		target.Advance(d)
		return
	}
	time.Sleep(d)
}
