package router

import (
	"math"
	"sync"
	"time"
)

// Health defaults.
const (
	// DefaultHalfLife is how long it takes an observation to lose half its
	// weight. Short enough that recovery is noticed within a minute, long
	// enough that one slow request does not reorder the candidate list.
	DefaultHalfLife = 30 * time.Second

	// warmupObservations is how many samples a deployment needs before its
	// score is trusted. Below this it scores as healthy, so a deployment that
	// has just been added is tried rather than avoided for never having been
	// tried — which would be self-fulfilling.
	warmupObservations = 5
)

// Health tracks how a deployment has been behaving, as an exponentially
// weighted moving average of its error rate and latency.
//
// Passive rather than probed: every real request is already a measurement, and
// a probe measures a synthetic request that may not resemble the traffic. An
// active prober is still worth adding for deployments with no traffic, which
// this cannot say anything about — but it complements this rather than
// replacing it.
//
// Safe for concurrent use.
type Health struct {
	halfLife time.Duration
	now      func() time.Time

	mu       sync.Mutex
	errorAvg float64
	latency  float64
	count    int
	updated  time.Time
}

// HealthOption configures a Health.
type HealthOption func(*Health)

// WithHalfLife sets how quickly observations lose weight.
func WithHalfLife(d time.Duration) HealthOption {
	return func(h *Health) {
		if d > 0 {
			h.halfLife = d
		}
	}
}

// WithHealthClock replaces the time source, for tests.
func WithHealthClock(now func() time.Time) HealthOption {
	return func(h *Health) {
		if now != nil {
			h.now = now
		}
	}
}

// NewHealth returns a tracker with no observations.
func NewHealth(opts ...HealthOption) *Health {
	h := &Health{halfLife: DefaultHalfLife, now: time.Now}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Observe records the outcome and duration of one attempt.
func (h *Health) Observe(failed bool, latency time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()

	weight := h.decay()

	outcome := 0.0
	if failed {
		outcome = 1.0
	}
	h.errorAvg = h.errorAvg*weight + outcome*(1-weight)
	h.latency = h.latency*weight + latency.Seconds()*(1-weight)

	h.count++
	h.updated = h.now()
}

// Score is how good a candidate this deployment looks, from 0 to 1.
//
// Error rate dominates: a fast deployment that fails is worse than a slow one
// that works, and ordering by latency alone would send traffic to whichever
// deployment was failing fastest.
func (h *Health) Score() float64 {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.count < warmupObservations {
		// Untried is not unhealthy. Scoring a new deployment badly for having
		// no history would mean it is never tried and never gets one.
		return 1
	}

	weight := h.decay()
	errorRate := h.errorAvg * weight
	latency := h.latency * weight

	// A second of latency costs about as much as a 10% error rate, which keeps
	// a healthy-but-slow deployment behind a healthy-and-fast one without ever
	// putting it behind a failing one.
	penalty := errorRate + math.Min(latency/10, 0.5)
	return math.Max(0, 1-penalty)
}

// Observations reports how many samples have been taken, for metrics and to
// tell "healthy" from "unmeasured".
func (h *Health) Observations() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.count
}

// decay is the weight the existing average keeps, given how long it has been
// since the last observation.
//
// Applied on read as well as on write, so a deployment that stopped receiving
// traffic drifts back towards neutral instead of staying condemned by the last
// thing that happened to it before it went quiet.
func (h *Health) decay() float64 {
	if h.updated.IsZero() {
		return 0
	}
	elapsed := h.now().Sub(h.updated)
	if elapsed <= 0 {
		return math.Exp2(-1)
	}
	return math.Exp2(-elapsed.Seconds() / h.halfLife.Seconds())
}
