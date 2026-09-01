// Package router turns a model name into a served response.
//
// # Selection and execution are separate
//
// Selection reads the snapshot and produces an ordered candidate list.
// Execution walks that list. Keeping them apart is what makes the routing
// policy testable without a network and the retry logic testable without a
// snapshot — and it is why "which deployments could serve this" and "what
// happened when we tried" are answerable independently when a request goes
// wrong.
//
// # The deadline is shared across attempts
//
// Every attempt draws from one budget derived from the caller's deadline.
// Three retries that each get the full timeout give the caller three times the
// wait they asked for, which is worse than the failure they were trying to
// avoid.
package router

import (
	"context"
	"log/slog"
	"maps"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

// Execution defaults.
const (
	// DefaultMaxAttempts counts the first try. Two retries is enough to route
	// around one bad deployment; more just spends the caller's deadline.
	DefaultMaxAttempts = 3
	// DefaultRetryBackoff is the base delay between attempts, before jitter.
	DefaultRetryBackoff = 50 * time.Millisecond
	// minAttemptBudget is the least time worth starting an attempt with.
	// Beginning one with almost no budget converts a useful error into a
	// timeout and tells the caller nothing.
	minAttemptBudget = 250 * time.Millisecond
)

// ProviderFor resolves a deployment's adapter.
type ProviderFor interface {
	Provider(name string) (core.ProviderPort, bool)
}

// Candidate is a deployment eligible to serve a request, with the score that
// ordered it.
type Candidate struct {
	Deployment core.Deployment
	Score      float64
}

// Router selects and executes. Safe for concurrent use.
type Router struct {
	providers ProviderFor
	logger    *slog.Logger
	now       func() time.Time

	maxAttempts int
	backoff     time.Duration

	mu       sync.Mutex
	breakers map[core.DeploymentID]*Breaker
	health   map[core.DeploymentID]*Health

	breakerOpts []BreakerOption
	healthOpts  []HealthOption
}

// Option configures a Router.
type Option func(*Router)

// WithLogger sets where the router reports.
func WithLogger(l *slog.Logger) Option {
	return func(r *Router) {
		if l != nil {
			r.logger = l
		}
	}
}

// WithClock replaces the time source, for tests.
func WithClock(now func() time.Time) Option {
	return func(r *Router) {
		if now != nil {
			r.now = now
		}
	}
}

// WithMaxAttempts sets how many attempts one request may make in total.
func WithMaxAttempts(n int) Option {
	return func(r *Router) {
		if n > 0 {
			r.maxAttempts = n
		}
	}
}

// WithRetryBackoff sets the base delay between attempts.
func WithRetryBackoff(d time.Duration) Option {
	return func(r *Router) { r.backoff = d }
}

// WithBreakerOptions configures every breaker the router creates.
func WithBreakerOptions(opts ...BreakerOption) Option {
	return func(r *Router) { r.breakerOpts = opts }
}

// WithHealthOptions configures every health tracker the router creates.
func WithHealthOptions(opts ...HealthOption) Option {
	return func(r *Router) { r.healthOpts = opts }
}

// New returns a router.
func New(providers ProviderFor, opts ...Option) (*Router, error) {
	if providers == nil {
		return nil, core.New(core.CodeInternal, "a router needs a provider registry")
	}
	r := &Router{
		providers:   providers,
		logger:      slog.Default(),
		now:         time.Now,
		maxAttempts: DefaultMaxAttempts,
		backoff:     DefaultRetryBackoff,
		breakers:    map[core.DeploymentID]*Breaker{},
		health:      map[core.DeploymentID]*Health{},
	}
	for _, opt := range opts {
		opt(r)
	}
	return r, nil
}

// SelectionInput is everything selection needs, and nothing more.
//
// A struct rather than a long parameter list, because every field is either a
// string or a number and a positional call would be unreadable and fragile to
// reordering.
type SelectionInput struct {
	Snapshot *core.Snapshot
	Tenant   core.TenantID
	Model    string
	Endpoint core.Endpoint
	// MinTrustTier is computed once by the caller and applies to the whole
	// list. This is the ordering that matters: filtering per attempt would let
	// a fallback move a request to a lower tier than its payload was prepared
	// for.
	MinTrustTier core.TrustTier
	Required     []core.Capability
}

// Select returns the deployments that may serve a request, best first.
//
// The whole list is filtered by trust tier before anything is ordered, so no
// execution path can reach a deployment the request was not allowed to use.
// That is a property of the list rather than of the code that walks it, which
// is the only way to be sure a future change to execution cannot break it.
func (r *Router) Select(in SelectionInput) ([]Candidate, error) {
	targets := in.Snapshot.ResolveAlias(in.Tenant, in.Model)

	var sawAny, sawServing, sawTier bool
	candidates := make([]Candidate, 0, len(targets))

	for _, target := range targets {
		for _, deployment := range in.Snapshot.Deployments(target) {
			sawAny = true
			if !deployment.Serving() {
				continue
			}
			sawServing = true
			if !deployment.TrustTier.AtLeast(in.MinTrustTier) {
				continue
			}
			sawTier = true
			if !deployment.Supports(in.Required...) {
				continue
			}
			provider, ok := r.providers.Provider(deployment.Provider)
			if !ok || !slices.Contains(provider.Endpoints(), in.Endpoint) {
				continue
			}
			candidates = append(candidates, Candidate{
				Deployment: deployment,
				Score:      r.healthFor(deployment.ID).Score(),
			})
		}
	}

	if len(candidates) == 0 {
		return nil, selectionError(in.Model, in.MinTrustTier, sawAny, sawServing, sawTier)
	}

	// Best first, and stable so that equal scores keep catalog order rather
	// than shuffling between requests — which would make routing decisions
	// impossible to reproduce when investigating one.
	slices.SortStableFunc(candidates, func(a, b Candidate) int {
		switch {
		case a.Score > b.Score:
			return -1
		case a.Score < b.Score:
			return 1
		default:
			return 0
		}
	})
	return candidates, nil
}

// selectionError distinguishes why nothing was eligible. The four cases mean
// different things to a caller: a typo, a capacity problem, a policy refusal,
// and a request made on the wrong API surface.
func selectionError(model string, tier core.TrustTier, sawAny, sawServing, sawTier bool) error {
	switch {
	case !sawAny:
		return core.Newf(core.CodeModelNotFound, "no model named %q", model)
	case !sawServing:
		return core.Newf(core.CodeNoCandidates, "no deployment of %q is serving traffic", model)
	case !sawTier:
		return core.Newf(core.CodeTrustTierDenied,
			"no deployment of %q meets the required trust tier %s", model, tier)
	default:
		return core.Newf(core.CodeEndpointUnsupported,
			"%q is not served on this endpoint with the required capabilities", model)
	}
}

// Attempt is what one try against one deployment did.
type Attempt struct {
	Deployment core.DeploymentID
	Latency    time.Duration
	Err        error
	// Skipped is set when the breaker refused before anything was called, so
	// an operator can tell "we tried and it failed" from "we did not try".
	Skipped bool
}

// Result is a completed execution.
type Result struct {
	Deployment core.Deployment
	Response   *core.ProviderResponse
	Stream     core.ChunkStream
	Attempts   []Attempt
}

// Call is one attempt's work, so execution does not need to know whether it is
// running a streaming or a non-streaming request.
type Call func(ctx context.Context, provider core.ProviderPort, d core.Deployment) (*core.ProviderResponse, core.ChunkStream, error)

// Execute walks the candidates until one succeeds or the budget runs out.
//
// Only retryable errors move to the next candidate. A provider rejecting a
// malformed request will reject it again, and retrying spends the caller's
// deadline to arrive at the same answer more slowly.
func (r *Router) Execute(
	ctx context.Context, candidates []Candidate, deadline time.Time, call Call,
) (*Result, error) {
	result := &Result{}
	var lastErr error

	for i, candidate := range candidates {
		if i >= r.maxAttempts {
			break
		}

		remaining := r.remaining(ctx, deadline)
		if remaining < minAttemptBudget {
			// Starting an attempt with almost no budget converts a useful
			// error into a timeout and tells the caller nothing.
			break
		}

		breaker := r.breakerFor(candidate.Deployment.ID)
		if !breaker.Allow() {
			result.Attempts = append(result.Attempts, Attempt{
				Deployment: candidate.Deployment.ID, Skipped: true,
			})
			continue
		}

		if i > 0 {
			if !r.wait(ctx, i) {
				break
			}
		}

		provider, ok := r.providers.Provider(candidate.Deployment.Provider)
		if !ok {
			// Checked at selection, so reaching here means the registry changed
			// underneath us — a bug rather than a caller error.
			lastErr = core.Newf(core.CodeInternal,
				"provider %q vanished between selection and execution", candidate.Deployment.Provider)
			continue
		}

		attemptCtx, cancel := context.WithTimeout(ctx, remaining)
		started := r.now()
		response, stream, err := call(attemptCtx, provider, candidate.Deployment)
		latency := r.now().Sub(started)

		attempt := Attempt{Deployment: candidate.Deployment.ID, Latency: latency, Err: err}
		result.Attempts = append(result.Attempts, attempt)

		if err == nil {
			// The context outlives this call for a stream, which keeps reading
			// from it after Execute returns. Cancelling here would truncate
			// the response the caller is about to relay.
			if stream == nil {
				cancel()
			} else {
				context.AfterFunc(ctx, cancel)
			}
			breaker.Succeed()
			r.healthFor(candidate.Deployment.ID).Observe(false, latency)

			result.Deployment = candidate.Deployment
			result.Response = response
			result.Stream = stream
			return result, nil
		}
		cancel()

		lastErr = err
		// Only failures that say something about the deployment count against
		// it. A malformed request is the caller's problem, and letting it open
		// a breaker would let one bad client take a healthy deployment out of
		// rotation for everyone.
		if core.IsRetryable(err) {
			breaker.Fail()
			r.healthFor(candidate.Deployment.ID).Observe(true, latency)
		} else {
			return result, err
		}

		r.logger.Warn("attempt failed, trying the next candidate",
			slog.String("deployment", string(candidate.Deployment.ID)),
			slog.String("error", err.Error()))
	}

	if lastErr != nil {
		return result, lastErr
	}
	// Every candidate was refused by its breaker, or the budget ran out before
	// any attempt began. Either way nothing was tried, which is a different
	// thing from everything having failed.
	return result, core.New(core.CodeNoCandidates,
		"every candidate deployment is unavailable or the deadline was exhausted")
}

// wait sleeps before a retry, with jitter.
//
// Without jitter every worker retrying a failed provider does so in lockstep,
// arriving together at exactly the moment it is trying to recover.
func (r *Router) wait(ctx context.Context, attempt int) bool {
	if r.backoff <= 0 {
		return true
	}
	delay := r.backoff * time.Duration(1<<uint(attempt-1))
	delay += time.Duration(rand.Float64() * float64(delay)) //nolint:gosec // scheduling spread

	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (r *Router) remaining(ctx context.Context, deadline time.Time) time.Duration {
	// The caller's context deadline wins when it is sooner: a client that hung
	// up should not have work continued on its behalf.
	if ctxDeadline, ok := ctx.Deadline(); ok && (deadline.IsZero() || ctxDeadline.Before(deadline)) {
		deadline = ctxDeadline
	}
	if deadline.IsZero() {
		return time.Hour
	}
	return deadline.Sub(r.now())
}

// DeploymentHealth is what the router believes about one deployment.
type DeploymentHealth struct {
	Deployment   core.DeploymentID
	Score        float64
	Observations int
	BreakerOpen  bool
}

// Stats reports what the router believes about every deployment it has tried.
//
// Exposed on the readiness endpoint because "why is traffic going there" is
// answered by the scores, and an operator asking it during an incident should
// not have to reason backwards from request outcomes.
func (r *Router) Stats() []DeploymentHealth {
	r.mu.Lock()
	ids := make([]core.DeploymentID, 0, len(r.health))
	for id := range r.health {
		ids = append(ids, id)
	}
	health := make(map[core.DeploymentID]*Health, len(r.health))
	maps.Copy(health, r.health)
	breakers := make(map[core.DeploymentID]*Breaker, len(r.breakers))
	maps.Copy(breakers, r.breakers)
	r.mu.Unlock()

	slices.Sort(ids)
	out := make([]DeploymentHealth, 0, len(ids))
	for _, id := range ids {
		entry := DeploymentHealth{Deployment: id}
		if h, ok := health[id]; ok {
			entry.Score = h.Score()
			entry.Observations = h.Observations()
		}
		if b, ok := breakers[id]; ok {
			entry.BreakerOpen = b.Open()
		}
		out = append(out, entry)
	}
	return out
}

func (r *Router) breakerFor(id core.DeploymentID) *Breaker {
	r.mu.Lock()
	defer r.mu.Unlock()

	if b, ok := r.breakers[id]; ok {
		return b
	}
	b := NewBreaker(r.breakerOpts...)
	r.breakers[id] = b
	return b
}

func (r *Router) healthFor(id core.DeploymentID) *Health {
	r.mu.Lock()
	defer r.mu.Unlock()

	if h, ok := r.health[id]; ok {
		return h
	}
	h := NewHealth(r.healthOpts...)
	r.health[id] = h
	return h
}
