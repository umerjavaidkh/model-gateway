package limits

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

const (
	// Window is the period every limit is expressed over. One minute matches
	// how limits are quoted to tenants; a shorter window would be smoother but
	// would mean explaining a number nobody asked for.
	Window = time.Minute

	// DefaultLeaseSize is how many permits a worker takes at once.
	//
	// It is the whole trade: larger means fewer round trips and more possible
	// over-admission at a boundary; smaller is more accurate and chattier. At
	// 25 with a handful of workers the overshoot is small next to any limit
	// worth setting, and the store sees one call per 25 requests instead of
	// one per request.
	DefaultLeaseSize = 25

	// DefaultConcurrencySlack is how long a slot is held after a request
	// finishes without releasing, so a leaked slot cannot pin a principal
	// forever. Nothing should rely on it; it exists because something
	// eventually will.
	DefaultConcurrencySlack = 5 * time.Minute
)

// Decision is why a request was refused, or that it was not.
type Decision struct {
	Allowed bool
	// Reason is empty when allowed.
	Reason core.Code
	// RetryAfter is how long until the window rolls, for the response header.
	RetryAfter time.Duration
}

var allowed = Decision{Allowed: true}

// Limiter enforces per-principal rate limits.
//
// It holds a bucket per principal per dimension, leased from a KVStore. The
// store is always present and required; *which* store decides the scope. An
// in-process store enforces limits per worker, a shared one enforces them
// fleet-wide, and the limiter cannot tell the difference — which is why there
// is no branch here for the case of not having one. An earlier version treated
// a nil store as "per worker" and enforced nothing at all, because the
// per-worker path had no counter to stop refilling against.
type Limiter struct {
	store  core.KVStore
	logger *slog.Logger
	now    func() time.Time
	lease  int64

	mu      sync.Mutex
	buckets map[string]*Bucket

	// concurrent tracks in-flight requests per principal on this worker.
	concurrent sync.Map // string -> *atomic.Int64

	failedOpen atomic.Int64
}

// Option configures a Limiter.
type Option func(*Limiter)

// WithClock replaces the time source, for tests.
func WithClock(now func() time.Time) Option {
	return func(l *Limiter) {
		if now != nil {
			l.now = now
		}
	}
}

// WithLeaseSize sets how many permits are taken from the store at once.
func WithLeaseSize(n int64) Option {
	return func(l *Limiter) {
		if n > 0 {
			l.lease = n
		}
	}
}

// WithLogger sets where the limiter reports.
func WithLogger(logger *slog.Logger) Option {
	return func(l *Limiter) {
		if logger != nil {
			l.logger = logger
		}
	}
}

// New returns a limiter over the given store.
func New(store core.KVStore, opts ...Option) (*Limiter, error) {
	if store == nil {
		return nil, core.New(core.CodeInternal,
			"a limiter needs a store; use an in-process one for per-worker limits")
	}
	l := &Limiter{
		store:   store,
		logger:  slog.Default(),
		now:     time.Now,
		lease:   DefaultLeaseSize,
		buckets: map[string]*Bucket{},
	}
	for _, opt := range opts {
		opt(l)
	}
	return l, nil
}

// FailedOpen counts requests admitted because the store was unreachable.
//
// The plan is explicit that rate limits fail open with an alarm, and this
// counter is the alarm. Rate limiting exists to protect capacity, and refusing
// traffic because the limiter's own store is down converts a limiter outage
// into a gateway outage.
func (l *Limiter) FailedOpen() int64 { return l.failedOpen.Load() }

// Admit checks a request against the principal's limits.
//
// Token limits are not checked here: token counts are only known after a
// response, so they are enforced by the *next* request seeing an already-spent
// window. That lag is inherent — a limit on something not yet measured cannot
// be anything else — and is why budgets exist for the cases that must be exact.
func (l *Limiter) Admit(ctx context.Context, p *core.Principal) Decision {
	if p.Limits.Unlimited() {
		return allowed
	}

	if p.Limits.MaxConcurrent > 0 {
		if !l.acquireSlot(p) {
			return Decision{Reason: core.CodeRateLimited, RetryAfter: time.Second}
		}
	}

	if p.Limits.RequestsPerMinute > 0 {
		if !l.take(ctx, l.key(p, "rpm"), int64(p.Limits.RequestsPerMinute)) {
			l.releaseSlot(p)
			return Decision{Reason: core.CodeRateLimited, RetryAfter: l.untilWindowRolls()}
		}
	}

	if p.Limits.TokensPerMinute > 0 && l.windowSpent(ctx, l.key(p, "tpm"), int64(p.Limits.TokensPerMinute)) {
		l.releaseSlot(p)
		return Decision{Reason: core.CodeRateLimited, RetryAfter: l.untilWindowRolls()}
	}

	return allowed
}

// Release returns a concurrency slot. The caller defers it immediately after
// Admit returns allowed; a leaked slot pins a principal until the slack expires.
func (l *Limiter) Release(p *core.Principal) {
	if p.Limits.MaxConcurrent > 0 {
		l.releaseSlot(p)
	}
}

// RecordTokens adds a request's token usage to the principal's window.
//
// Called after a response, which is the earliest the count exists. The window
// it lands in is the one current at that moment, not the one the request was
// admitted under — an approximation that only matters for a request spanning a
// window boundary, which is exactly the case where the answer is arbitrary.
func (l *Limiter) RecordTokens(ctx context.Context, p *core.Principal, tokens int64) {
	if p.Limits.TokensPerMinute == 0 || tokens <= 0 {
		return
	}
	if _, err := l.store.Incr(ctx, l.key(p, "tpm"), tokens, Window*2); err != nil {
		l.logger.Warn("recording token usage failed",
			slog.String("key_id", string(p.KeyID)), slog.String("error", err.Error()))
	}
}

// take spends a permit, refilling the local lease from the store when needed.
func (l *Limiter) take(ctx context.Context, key string, limit int64) bool {
	bucket := l.bucket(key)

	taken, shouldRefill := bucket.Take(l.now())
	if !shouldRefill {
		return taken
	}
	if !bucket.BeginRefill() {
		// Another goroutine is already refilling. Rather than wait behind it,
		// this request uses whatever the bucket had — which is the answer that
		// keeps a refill from becoming a latency spike for everyone.
		return taken
	}

	granted := l.leaseFrom(ctx, key, limit)
	bucket.Grant(granted, l.windowEnd())
	if taken {
		return true
	}
	// This request found the bucket empty and did the refill, so it spends one
	// of the permits it just fetched rather than being refused for the work.
	again, _ := bucket.Take(l.now())
	return again
}

// leaseFrom claims a block of permits from the shared window.
func (l *Limiter) leaseFrom(ctx context.Context, key string, limit int64) int64 {
	used, err := l.store.Incr(ctx, key, l.lease, Window*2)
	if err != nil {
		// Fail open, with a count. Refusing traffic because the limiter's own
		// store is unreachable turns a limiter outage into a gateway outage.
		l.failedOpen.Add(1)
		l.logger.Warn("rate limit store unavailable; failing open",
			slog.String("key", key), slog.String("error", err.Error()))
		return l.lease
	}

	// Incr returns the total after adding, so the permits actually available
	// are whatever of this lease fell inside the limit.
	overshoot := used - limit
	if overshoot >= l.lease {
		return 0
	}
	if overshoot > 0 {
		return l.lease - overshoot
	}
	return l.lease
}

// windowSpent reports whether a counter has already passed its limit.
func (l *Limiter) windowSpent(ctx context.Context, key string, limit int64) bool {
	raw, found, err := l.store.Get(ctx, key)
	if err != nil {
		l.failedOpen.Add(1)
		return false
	}
	if !found {
		return false
	}
	used, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return false
	}
	return used >= limit
}

func (l *Limiter) bucket(key string) *Bucket {
	l.mu.Lock()
	defer l.mu.Unlock()

	if b, ok := l.buckets[key]; ok {
		return b
	}
	b := &Bucket{}
	l.buckets[key] = b
	return b
}

func (l *Limiter) acquireSlot(p *core.Principal) bool {
	counter, _ := l.concurrent.LoadOrStore(string(p.KeyID), &atomic.Int64{})
	slots, ok := counter.(*atomic.Int64)
	if !ok {
		return true
	}

	if slots.Add(1) > int64(p.Limits.MaxConcurrent) {
		slots.Add(-1)
		return false
	}
	return true
}

func (l *Limiter) releaseSlot(p *core.Principal) {
	counter, ok := l.concurrent.Load(string(p.KeyID))
	if !ok {
		return
	}
	if slots, ok := counter.(*atomic.Int64); ok && slots.Add(-1) < 0 {
		// A release without a matching acquire is a bug in the caller, but
		// letting the count go negative would give that principal unlimited
		// concurrency afterwards, which is worse than the bug.
		slots.Store(0)
	}
}

// key namespaces a counter by principal, dimension and window.
//
// The window is in the key rather than being reset on a timer, so expiry is the
// store's job and a worker that was down for a window does not resurrect an old
// count when it comes back.
func (l *Limiter) key(p *core.Principal, dimension string) string {
	window := l.now().Truncate(Window).Unix()
	return "rl:" + string(p.Tenant) + ":" + string(p.KeyID) + ":" + dimension + ":" +
		strconv.FormatInt(window, 10)
}

func (l *Limiter) windowEnd() time.Time { return l.now().Truncate(Window).Add(Window) }

func (l *Limiter) untilWindowRolls() time.Duration {
	remaining := l.windowEnd().Sub(l.now())
	if remaining < time.Second {
		return time.Second
	}
	return remaining
}
