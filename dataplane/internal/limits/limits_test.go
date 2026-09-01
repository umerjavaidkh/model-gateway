package limits_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/adapters/memkv"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/limits"
)

var start = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func principal(limit core.RateLimit) *core.Principal {
	return &core.Principal{KeyID: "key-1", Tenant: "acme", Limits: limit}
}

// clock is a hand-wound time source, so window behaviour is exercised rather
// than waited for.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newLimiter(t *testing.T, store core.KVStore, c *clock, opts ...limits.Option) *limits.Limiter {
	t.Helper()
	l, err := limits.New(store, append([]limits.Option{
		limits.WithClock(c.Now), limits.WithLogger(quiet()),
	}, opts...)...)
	if err != nil {
		t.Fatalf("limits.New: %v", err)
	}
	return l
}

func admitN(t *testing.T, l *limits.Limiter, p *core.Principal, n int) int {
	t.Helper()
	allowed := 0
	for range n {
		if d := l.Admit(t.Context(), p); d.Allowed {
			allowed++
			l.Release(p)
		}
	}
	return allowed
}

func TestNoLimitsMeansNoEnforcement(t *testing.T) {
	// Zero must mean unlimited, not denied: a principal that predates a limit
	// must not suddenly be capped at zero, which would turn adding a field
	// into an outage.
	c := &clock{now: start}
	l := newLimiter(t, memkv.New(memkv.WithClock(c.Now)), c)

	if got := admitN(t, l, principal(core.RateLimit{}), 1000); got != 1000 {
		t.Fatalf("admitted %d of 1000 with no limits set", got)
	}
}

func TestRequestsPerMinuteIsEnforced(t *testing.T) {
	c := &clock{now: start}
	l := newLimiter(t, memkv.New(memkv.WithClock(c.Now)), c, limits.WithLeaseSize(5))
	p := principal(core.RateLimit{RequestsPerMinute: 10})

	if got := admitN(t, l, p, 50); got != 10 {
		t.Fatalf("admitted %d, want exactly the limit of 10", got)
	}
	if d := l.Admit(t.Context(), p); d.Allowed || d.Reason != core.CodeRateLimited {
		t.Fatalf("decision = %+v, want a rate-limit refusal", d)
	}
}

func TestTheWindowRolls(t *testing.T) {
	// Permits from an expired window are not carried forward: a quiet minute
	// must not fund a burst in the next one, which is what a rate limit is for.
	c := &clock{now: start}
	l := newLimiter(t, memkv.New(memkv.WithClock(c.Now)), c, limits.WithLeaseSize(5))
	p := principal(core.RateLimit{RequestsPerMinute: 10})

	if got := admitN(t, l, p, 20); got != 10 {
		t.Fatalf("admitted %d in the first window, want 10", got)
	}

	c.advance(limits.Window + time.Second)
	if got := admitN(t, l, p, 20); got != 10 {
		t.Fatalf("admitted %d in the next window, want a fresh 10", got)
	}
}

func TestRefusalSaysWhenToRetry(t *testing.T) {
	// A 429 without a retry hint makes a well-behaved client guess, and a
	// guessing client retries too fast.
	c := &clock{now: start.Add(20 * time.Second)}
	l := newLimiter(t, memkv.New(memkv.WithClock(c.Now)), c)
	p := principal(core.RateLimit{RequestsPerMinute: 1})

	admitN(t, l, p, 5)
	d := l.Admit(t.Context(), p)
	if d.Allowed {
		t.Fatal("expected a refusal")
	}
	if d.RetryAfter <= 0 || d.RetryAfter > limits.Window {
		t.Fatalf("RetryAfter = %v, want a duration within the window", d.RetryAfter)
	}
}

func TestWorkersShareOneWindowThroughTheStore(t *testing.T) {
	// The point of a shared store: two workers must not each get the full
	// limit. Over-admission is bounded by the lease size, which is the
	// documented trade rather than an accident.
	c := &clock{now: start}
	store := memkv.New(memkv.WithClock(c.Now))
	const lease = 5

	a := newLimiter(t, store, c, limits.WithLeaseSize(lease))
	b := newLimiter(t, store, c, limits.WithLeaseSize(lease))
	limit := core.RateLimit{RequestsPerMinute: 20}

	total := admitN(t, a, principal(limit), 40) + admitN(t, b, principal(limit), 40)

	if total < 20 {
		t.Fatalf("admitted %d, fewer than the limit of 20", total)
	}
	if total > 20+lease {
		t.Fatalf("admitted %d, more than the limit plus one lease (%d)", total, 20+lease)
	}
}

func TestConcurrencyIsBoundedAndReleased(t *testing.T) {
	c := &clock{now: start}
	l := newLimiter(t, memkv.New(memkv.WithClock(c.Now)), c)
	p := principal(core.RateLimit{MaxConcurrent: 2})

	first := l.Admit(t.Context(), p)
	second := l.Admit(t.Context(), p)
	third := l.Admit(t.Context(), p)

	if !first.Allowed || !second.Allowed {
		t.Fatal("the first two concurrent requests must be admitted")
	}
	if third.Allowed {
		t.Fatal("a third concurrent request must be refused")
	}

	l.Release(p)
	if d := l.Admit(t.Context(), p); !d.Allowed {
		t.Fatal("releasing a slot must let the next request in")
	}
}

func TestARefusedRequestDoesNotConsumeASlot(t *testing.T) {
	// A request refused on requests-per-minute must give its concurrency slot
	// back, or a burst of refusals permanently wedges the principal.
	c := &clock{now: start}
	l := newLimiter(t, memkv.New(memkv.WithClock(c.Now)), c, limits.WithLeaseSize(1))
	p := principal(core.RateLimit{RequestsPerMinute: 1, MaxConcurrent: 1})

	if d := l.Admit(t.Context(), p); !d.Allowed {
		t.Fatal("the first request must be admitted")
	}
	l.Release(p)

	for range 5 {
		if d := l.Admit(t.Context(), p); d.Allowed {
			t.Fatal("expected the rate limit to refuse")
		}
	}

	c.advance(limits.Window + time.Second)
	if d := l.Admit(t.Context(), p); !d.Allowed {
		t.Fatal("the concurrency slot was not released by the refusals")
	}
}

func TestOverReleasingDoesNotGrantUnlimitedConcurrency(t *testing.T) {
	// A release without a matching acquire is a caller bug, but letting the
	// count go negative would give that principal unlimited concurrency
	// afterwards, which is worse than the bug.
	c := &clock{now: start}
	l := newLimiter(t, memkv.New(memkv.WithClock(c.Now)), c)
	p := principal(core.RateLimit{MaxConcurrent: 1})

	if d := l.Admit(t.Context(), p); !d.Allowed {
		t.Fatal("first request must be admitted")
	}
	for range 5 {
		l.Release(p)
	}

	if d := l.Admit(t.Context(), p); !d.Allowed {
		t.Fatal("expected one slot available")
	}
	if d := l.Admit(t.Context(), p); d.Allowed {
		t.Fatal("the limit was lost after over-releasing")
	}
}

func TestTokenLimitsTakeEffectOnTheNextRequest(t *testing.T) {
	// Token counts only exist after a response, so the limit is necessarily
	// enforced one request late. Asserting the lag documents it.
	c := &clock{now: start}
	l := newLimiter(t, memkv.New(memkv.WithClock(c.Now)), c)
	p := principal(core.RateLimit{TokensPerMinute: 100})

	if d := l.Admit(t.Context(), p); !d.Allowed {
		t.Fatal("the first request cannot be refused; nothing has been spent")
	}
	l.Release(p)
	l.RecordTokens(t.Context(), p, 150)

	if d := l.Admit(t.Context(), p); d.Allowed {
		t.Fatal("the next request must see the spent window")
	}

	c.advance(limits.Window + time.Second)
	if d := l.Admit(t.Context(), p); !d.Allowed {
		t.Fatal("a new window must start clean")
	}
}

// failingStore stands in for an unreachable Redis.
type failingStore struct{}

func (failingStore) Get(context.Context, string) ([]byte, bool, error) {
	return nil, false, core.New(core.CodeUnavailable, "store is down")
}
func (failingStore) Set(context.Context, string, []byte, time.Duration) error {
	return core.New(core.CodeUnavailable, "store is down")
}
func (failingStore) Incr(context.Context, string, int64, time.Duration) (int64, error) {
	return 0, core.New(core.CodeUnavailable, "store is down")
}
func (failingStore) Delete(context.Context, string) error {
	return core.New(core.CodeUnavailable, "store is down")
}

func TestAnUnreachableStoreFailsOpenAndCountsIt(t *testing.T) {
	// The plan is explicit: rate limits fail open with an alarm. Refusing
	// traffic because the limiter's own store is down turns a limiter outage
	// into a gateway outage, and the counter is the alarm.
	c := &clock{now: start}
	l := newLimiter(t, failingStore{}, c, limits.WithLeaseSize(3))
	p := principal(core.RateLimit{RequestsPerMinute: 1})

	if got := admitN(t, l, p, 30); got == 0 {
		t.Fatal("an unreachable store must not refuse every request")
	}
	if l.FailedOpen() == 0 {
		t.Fatal("failing open must be counted, or the outage is invisible")
	}
}

func TestAnInProcessStoreEnforcesLimitsPerWorker(t *testing.T) {
	// A legitimate deployment, not a degraded mode: with no shared store the
	// ceiling is per worker rather than fleet-wide, and that is far better than
	// none at all. It works because the limiter always has a store — an earlier
	// version special-cased a nil one and enforced nothing.
	c := &clock{now: start}
	l := newLimiter(t, memkv.New(memkv.WithClock(c.Now)), c, limits.WithLeaseSize(4))
	p := principal(core.RateLimit{RequestsPerMinute: 4})

	if got := admitN(t, l, p, 20); got != 4 {
		t.Fatalf("admitted %d, want the per-worker limit of 4", got)
	}
}

func TestALimiterWithoutAStoreIsRefused(t *testing.T) {
	if _, err := limits.New(nil); err == nil {
		t.Fatal("a limiter with no store would enforce nothing while looking configured")
	}
}

func TestAdmitIsSafeUnderConcurrency(t *testing.T) {
	c := &clock{now: start}
	l := newLimiter(t, memkv.New(memkv.WithClock(c.Now)), c, limits.WithLeaseSize(10))
	limit := core.RateLimit{RequestsPerMinute: 100}

	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := principal(limit)
			for range 50 {
				if d := l.Admit(context.Background(), p); d.Allowed {
					mu.Lock()
					admitted++
					mu.Unlock()
					l.Release(p)
				}
			}
		}()
	}
	wg.Wait()

	// Bounded over-admission is the documented trade; unbounded is a bug.
	if admitted < 100 || admitted > 100+limits.DefaultLeaseSize*16 {
		t.Fatalf("admitted %d, outside the expected band around 100", admitted)
	}
}
