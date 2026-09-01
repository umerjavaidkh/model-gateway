package router

import (
	"sync"
	"time"
)

// Breaker state. A breaker is per deployment, not per model: a model served by
// three providers should lose only the one that is failing.
type state uint8

const (
	// closed is healthy: everything passes.
	closed state = iota
	// open is failing: nothing passes, so a dead provider stops consuming the
	// deadline budget of every request routed to it.
	open
	// halfOpen is probing: exactly one request passes, and its outcome decides
	// whether the breaker closes or opens again.
	halfOpen
)

// Breaker defaults. Chosen for a proxy in front of model APIs, where a request
// is slow and expensive, so failing fast matters more than tolerating a blip.
const (
	// DefaultFailureThreshold is the consecutive failures that open a breaker.
	// Low, because each failed attempt costs a caller real latency.
	DefaultFailureThreshold = 5
	// DefaultCooldown is how long a breaker stays open before probing.
	DefaultCooldown = 10 * time.Second
	// DefaultHalfOpenSuccesses is how many probes must succeed to close it.
	// More than one, because a single success after an outage is as likely to
	// be luck as recovery.
	DefaultHalfOpenSuccesses = 2
)

// Breaker stops a failing deployment from being tried.
//
// The point is not to protect the provider — it can protect itself — but to
// stop a dead deployment consuming the caller's deadline. A request that spends
// its whole budget timing out against something known to be down has been
// failed twice: once by the provider and once by us.
//
// Safe for concurrent use.
type Breaker struct {
	failureThreshold  int
	cooldown          time.Duration
	halfOpenSuccesses int
	now               func() time.Time

	mu            sync.Mutex
	state         state
	failures      int
	successes     int
	openedAt      time.Time
	probeInFlight bool
}

// BreakerOption configures a Breaker.
type BreakerOption func(*Breaker)

// WithFailureThreshold sets the consecutive failures that open the breaker.
func WithFailureThreshold(n int) BreakerOption {
	return func(b *Breaker) {
		if n > 0 {
			b.failureThreshold = n
		}
	}
}

// WithCooldown sets how long the breaker stays open before probing.
func WithCooldown(d time.Duration) BreakerOption {
	return func(b *Breaker) {
		if d > 0 {
			b.cooldown = d
		}
	}
}

// WithBreakerClock replaces the time source, for tests.
func WithBreakerClock(now func() time.Time) BreakerOption {
	return func(b *Breaker) {
		if now != nil {
			b.now = now
		}
	}
}

// NewBreaker returns a closed breaker.
func NewBreaker(opts ...BreakerOption) *Breaker {
	b := &Breaker{
		failureThreshold:  DefaultFailureThreshold,
		cooldown:          DefaultCooldown,
		halfOpenSuccesses: DefaultHalfOpenSuccesses,
		now:               time.Now,
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// Allow reports whether a request may be attempted.
//
// In the half-open state exactly one request is admitted at a time. Letting
// several through would mean a recovering provider is hit by the whole fleet at
// once, which is how a service that was about to recover goes back down.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case closed:
		return true
	case open:
		if b.now().Sub(b.openedAt) < b.cooldown {
			return false
		}
		b.state = halfOpen
		b.successes = 0
		b.probeInFlight = true
		return true
	case halfOpen:
		if b.probeInFlight {
			return false
		}
		b.probeInFlight = true
		return true
	default:
		return true
	}
}

// Succeed records a successful attempt.
func (b *Breaker) Succeed() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.probeInFlight = false
	b.failures = 0

	if b.state == halfOpen {
		b.successes++
		if b.successes >= b.halfOpenSuccesses {
			b.state = closed
			b.successes = 0
		}
	}
}

// Fail records a failed attempt.
//
// Only failures that say something about the deployment's health count. A
// provider rejecting a malformed request is the caller's problem, and treating
// it as an outage would let one bad client take a healthy deployment out of
// rotation for everyone — the caller decides, by only calling this for
// retryable errors.
func (b *Breaker) Fail() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.probeInFlight = false

	if b.state == halfOpen {
		// A failed probe means it has not recovered. Back to open, and the
		// cooldown starts again rather than continuing from before.
		b.state = open
		b.openedAt = b.now()
		b.successes = 0
		return
	}

	b.failures++
	if b.failures >= b.failureThreshold {
		b.state = open
		b.openedAt = b.now()
		b.failures = 0
	}
}

// Open reports whether the breaker is currently refusing, for metrics.
func (b *Breaker) Open() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state == open && b.now().Sub(b.openedAt) < b.cooldown
}
