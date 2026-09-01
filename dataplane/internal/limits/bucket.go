// Package limits enforces rate limits at admission.
//
// # Two mechanisms, not one
//
// The design separates rate limits from budgets deliberately, and this package
// is only the first. Rate limits are fast and may be approximate; budgets must
// be correct and may lag. Conflating them produces a system that is both slow
// and wrong: a limiter that insists on exactness pays a round trip per request,
// and a budget that tolerates drift eventually disagrees with an invoice.
//
// So everything here is allowed to over-admit slightly, and says so.
//
// # Why a local lease
//
// A shared counter checked on every request is a network round trip on the hot
// path and a hard dependency on the store being up. Instead each worker leases
// a block of permits from the shared window and spends them locally, refreshing
// before it runs out.
//
// The cost is bounded over-admission: with W workers and a lease of N permits,
// the fleet can admit up to N*W beyond the limit at a window boundary in the
// worst case. That is a documented trade, not an accident, and the lease size
// is what tunes it.
package limits

import (
	"sync"
	"time"
)

// Bucket is a local allowance of permits, refilled from a shared source.
//
// Safe for concurrent use. The mutex is held only for arithmetic; the refill
// itself happens outside it, because holding a lock across a network call is
// how one slow store stalls every request on a worker.
type Bucket struct {
	mu        sync.Mutex
	remaining int64
	expiresAt time.Time
	// refreshing marks a refill in progress, so a burst of requests that all
	// find the bucket empty produces one refill rather than one per request.
	refreshing bool
}

// Take spends one permit if any remain.
//
// It reports whether a permit was taken and whether the caller should refill.
// Refilling is the caller's job rather than the bucket's so that the bucket
// stays pure and testable, and so the caller decides what "ask the store"
// means.
func (b *Bucket) Take(now time.Time) (taken bool, shouldRefill bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if now.After(b.expiresAt) {
		// The lease covered a window that has passed. Permits from an expired
		// window are not carried forward: doing so would let a quiet minute
		// fund a burst in the next one, which is exactly what a rate limit is
		// meant to prevent.
		b.remaining = 0
	}

	if b.remaining > 0 {
		b.remaining--
		// Refill early rather than on empty, so the refill overlaps with
		// serving instead of blocking a request behind it.
		return true, !b.refreshing && b.remaining == 0
	}
	return false, !b.refreshing
}

// BeginRefill marks a refill as started, reporting whether this caller won the
// race. Only the winner should talk to the store.
func (b *Bucket) BeginRefill() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.refreshing {
		return false
	}
	b.refreshing = true
	return true
}

// Grant installs a new lease and ends the refill.
func (b *Bucket) Grant(permits int64, expiresAt time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if permits > 0 {
		b.remaining += permits
		b.expiresAt = expiresAt
	}
	b.refreshing = false
}

// AbandonRefill ends a refill that failed, so the next request retries rather
// than the bucket staying permanently marked as refreshing.
func (b *Bucket) AbandonRefill() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refreshing = false
}

// Remaining reports the permits left, for metrics and tests.
func (b *Bucket) Remaining() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.remaining
}
