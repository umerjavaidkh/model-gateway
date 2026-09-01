package router_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/router"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// stubProvider serves every endpoint; the tests are about routing, not schemas.
type stubProvider struct{ name string }

func (p stubProvider) Name() string { return p.name }
func (stubProvider) Endpoints() []core.Endpoint {
	return []core.Endpoint{core.EndpointChatCompletions, core.EndpointMessages}
}
func (stubProvider) Invoke(context.Context, *core.ProviderCall) (*core.ProviderResponse, error) {
	return &core.ProviderResponse{StatusCode: 200}, nil
}
func (stubProvider) Stream(context.Context, *core.ProviderCall) (core.ChunkStream, error) {
	return nil, core.New(core.CodeInternal, "not used")
}

type registry map[string]core.ProviderPort

func (r registry) Provider(name string) (core.ProviderPort, bool) {
	p, ok := r[name]
	return p, ok
}

func defaultRegistry() registry {
	return registry{"alpha": stubProvider{name: "alpha"}, "beta": stubProvider{name: "beta"}}
}

func deployment(id string, tier core.TrustTier, provider string) core.Deployment {
	return core.Deployment{
		ID: core.DeploymentID(id), Key: core.RoutingKey{BaseModel: "m"},
		Provider: provider, TrustTier: tier, Weight: 100,
	}
}

func snapshotWith(t *testing.T, deployments ...core.Deployment) *core.Snapshot {
	t.Helper()
	global, err := core.NewGlobalLayer(core.GlobalSpec{
		Version:        core.LayerVersion{Number: 1},
		Deployments:    deployments,
		TenantPrefixes: map[core.KeyPrefix]core.TenantID{"acme": "acme"},
	})
	if err != nil {
		t.Fatalf("NewGlobalLayer: %v", err)
	}
	tenant, err := core.NewTenantLayer(core.TenantSpec{
		Tenant: "acme", Version: core.LayerVersion{Number: 1},
	})
	if err != nil {
		t.Fatalf("NewTenantLayer: %v", err)
	}
	snap, err := core.Compose(global, tenant)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	return snap
}

func newRouter(t *testing.T, reg registry, opts ...router.Option) *router.Router {
	t.Helper()
	r, err := router.New(reg, append([]router.Option{router.WithLogger(quiet())}, opts...)...)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	return r
}

func selectAll(t *testing.T, r *router.Router, snap *core.Snapshot, tier core.TrustTier) []router.Candidate {
	t.Helper()
	candidates, err := r.Select(router.SelectionInput{
		Snapshot: snap, Tenant: "acme", Model: "m",
		Endpoint: core.EndpointChatCompletions, MinTrustTier: tier,
	})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	return candidates
}

// --- selection ---------------------------------------------------------------

func TestSelectionFiltersTheWholeListByTrustTier(t *testing.T) {
	// The property the whole ordering exists for: no execution path can reach a
	// deployment the request was not allowed to use, because such deployments
	// are never in the list. That is a property of the list, not of the loop
	// that walks it — which is the only way a later change to execution cannot
	// break it.
	snap := snapshotWith(t,
		deployment("internal-1", core.TrustInternal, "alpha"),
		deployment("external-1", core.TrustExternal, "beta"),
	)

	candidates := selectAll(t, newRouter(t, defaultRegistry()), snap, core.TrustInternal)
	if len(candidates) != 1 || candidates[0].Deployment.ID != "internal-1" {
		t.Fatalf("candidates = %v, want only the internal deployment", candidates)
	}
}

func TestSelectionSkipsDeploymentsThatCannotServe(t *testing.T) {
	// Four refusals with four distinct meanings: a capacity problem, a request
	// on a surface nothing serves, and a typo. Collapsing them would report a
	// data-residency violation as a missing feature.
	notServing := deployment("d1", core.TrustInternal, "alpha")
	notServing.Weight = 0
	if _, err := newRouter(t, defaultRegistry()).Select(router.SelectionInput{
		Snapshot: snapshotWith(t, notServing), Tenant: "acme", Model: "m",
		Endpoint: core.EndpointChatCompletions, MinTrustTier: core.TrustExternal,
	}); !errors.Is(err, core.ErrNoCandidates) {
		t.Fatalf("a weight-0 deployment gave %v, want no_candidates", err)
	}

	if _, err := newRouter(t, registry{}).Select(router.SelectionInput{
		Snapshot: snapshotWith(t, deployment("d1", core.TrustInternal, "alpha")),
		Tenant:   "acme", Model: "m",
		Endpoint: core.EndpointChatCompletions, MinTrustTier: core.TrustExternal,
	}); !errors.Is(err, core.ErrEndpointUnsupported) {
		t.Fatalf("an unregistered provider gave %v, want endpoint_unsupported", err)
	}

	if _, err := newRouter(t, defaultRegistry()).Select(router.SelectionInput{
		Snapshot: snapshotWith(t, deployment("d1", core.TrustExternal, "alpha")),
		Tenant:   "acme", Model: "nope",
		Endpoint: core.EndpointChatCompletions, MinTrustTier: core.TrustExternal,
	}); !errors.Is(err, core.ErrModelNotFound) {
		t.Fatalf("an unknown model gave %v, want model_not_found", err)
	}
}

func TestHealthierDeploymentsAreTriedFirst(t *testing.T) {
	snap := snapshotWith(t,
		deployment("sick", core.TrustInternal, "alpha"),
		deployment("healthy", core.TrustInternal, "beta"),
	)
	r := newRouter(t, defaultRegistry(),
		router.WithMaxAttempts(1), router.WithRetryBackoff(0))

	// Fail "sick" enough times to move its score below the warm-up threshold.
	failing := func(_ context.Context, _ core.ProviderPort, d core.Deployment) (
		*core.ProviderResponse, core.ChunkStream, error,
	) {
		if d.ID == "sick" {
			return nil, nil, core.New(core.CodeUpstreamError, "down").AsRetryable()
		}
		return &core.ProviderResponse{StatusCode: 200}, nil, nil
	}
	for range 8 {
		sick := []router.Candidate{{Deployment: deployment("sick", core.TrustInternal, "alpha")}}
		_, _ = r.Execute(t.Context(), sick, time.Time{}, failing)
	}

	candidates := selectAll(t, r, snap, core.TrustInternal)
	if candidates[0].Deployment.ID != "healthy" {
		t.Fatalf("tried %q first; a deployment that keeps failing must fall behind one that works",
			candidates[0].Deployment.ID)
	}
}

// --- execution ---------------------------------------------------------------

func failingCall(err error) router.Call {
	return func(context.Context, core.ProviderPort, core.Deployment) (
		*core.ProviderResponse, core.ChunkStream, error,
	) {
		return nil, nil, err
	}
}

func TestExecutionFailsOverToTheNextCandidate(t *testing.T) {
	r := newRouter(t, defaultRegistry(), router.WithRetryBackoff(0))
	candidates := []router.Candidate{
		{Deployment: deployment("first", core.TrustInternal, "alpha")},
		{Deployment: deployment("second", core.TrustInternal, "beta")},
	}

	var tried atomic.Int64
	result, err := r.Execute(t.Context(), candidates, time.Time{},
		func(_ context.Context, _ core.ProviderPort, d core.Deployment) (
			*core.ProviderResponse, core.ChunkStream, error,
		) {
			tried.Add(1)
			if d.ID == "first" {
				return nil, nil, core.New(core.CodeUpstreamError, "503").AsRetryable()
			}
			return &core.ProviderResponse{StatusCode: 200}, nil, nil
		})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if result.Deployment.ID != "second" {
		t.Fatalf("served by %q, want the fallback", result.Deployment.ID)
	}
	if tried.Load() != 2 {
		t.Fatalf("made %d attempts, want 2", tried.Load())
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("recorded %d attempts, want both", len(result.Attempts))
	}
}

func TestANonRetryableErrorIsNotRetried(t *testing.T) {
	// A provider rejecting a malformed request will reject it again. Retrying
	// spends the caller's deadline to arrive at the same answer more slowly.
	r := newRouter(t, defaultRegistry(), router.WithRetryBackoff(0))
	candidates := []router.Candidate{
		{Deployment: deployment("first", core.TrustInternal, "alpha")},
		{Deployment: deployment("second", core.TrustInternal, "beta")},
	}

	var tried atomic.Int64
	_, err := r.Execute(t.Context(), candidates, time.Time{},
		func(context.Context, core.ProviderPort, core.Deployment) (
			*core.ProviderResponse, core.ChunkStream, error,
		) {
			tried.Add(1)
			return nil, nil, core.New(core.CodeUpstreamError, "400 bad request")
		})

	if err == nil {
		t.Fatal("expected the error to surface")
	}
	if tried.Load() != 1 {
		t.Fatalf("made %d attempts on a terminal error, want 1", tried.Load())
	}
}

func TestABrokenDeploymentIsSkippedOnceItsBreakerOpens(t *testing.T) {
	// The point of the breaker is not to protect the provider but to stop a
	// dead deployment consuming the caller's deadline.
	r := newRouter(t, defaultRegistry(),
		router.WithRetryBackoff(0),
		router.WithBreakerOptions(router.WithFailureThreshold(2)))
	only := []router.Candidate{{Deployment: deployment("d1", core.TrustInternal, "alpha")}}

	for range 2 {
		_, _ = r.Execute(t.Context(), only, time.Time{},
			failingCall(core.New(core.CodeUpstreamError, "down").AsRetryable()))
	}

	var tried atomic.Int64
	result, err := r.Execute(t.Context(), only, time.Time{},
		func(context.Context, core.ProviderPort, core.Deployment) (
			*core.ProviderResponse, core.ChunkStream, error,
		) {
			tried.Add(1)
			return &core.ProviderResponse{StatusCode: 200}, nil, nil
		})

	if err == nil {
		t.Fatal("expected the open breaker to refuse")
	}
	if tried.Load() != 0 {
		t.Fatal("the provider was called despite an open breaker")
	}
	if len(result.Attempts) != 1 || !result.Attempts[0].Skipped {
		t.Fatalf("attempts = %+v, want one recorded as skipped", result.Attempts)
	}
}

func TestABreakerRecoversAfterItsCooldown(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(d)
	}

	r := newRouter(t, defaultRegistry(),
		router.WithRetryBackoff(0), router.WithClock(clock),
		router.WithBreakerOptions(
			router.WithFailureThreshold(1),
			router.WithCooldown(30*time.Second),
			router.WithBreakerClock(clock),
		))
	only := []router.Candidate{{Deployment: deployment("d1", core.TrustInternal, "alpha")}}

	_, _ = r.Execute(t.Context(), only, time.Time{},
		failingCall(core.New(core.CodeUpstreamError, "down").AsRetryable()))

	if _, err := r.Execute(t.Context(), only, time.Time{},
		failingCall(core.New(core.CodeUpstreamError, "down").AsRetryable())); err == nil {
		t.Fatal("expected the breaker to be open")
	}

	advance(31 * time.Second)
	ok := func(context.Context, core.ProviderPort, core.Deployment) (
		*core.ProviderResponse, core.ChunkStream, error,
	) {
		return &core.ProviderResponse{StatusCode: 200}, nil, nil
	}
	if _, err := r.Execute(t.Context(), only, time.Time{}, ok); err != nil {
		t.Fatalf("the breaker did not probe after its cooldown: %v", err)
	}
}

func TestTheDeadlineIsSharedAcrossAttempts(t *testing.T) {
	// Three retries that each get the full timeout give the caller three times
	// the wait they asked for, which is worse than the failure they were
	// trying to avoid.
	r := newRouter(t, defaultRegistry(), router.WithRetryBackoff(0))
	candidates := []router.Candidate{
		{Deployment: deployment("a", core.TrustInternal, "alpha")},
		{Deployment: deployment("b", core.TrustInternal, "beta")},
		{Deployment: deployment("c", core.TrustInternal, "alpha")},
	}

	deadline := time.Now().Add(600 * time.Millisecond)
	started := time.Now()

	_, err := r.Execute(t.Context(), candidates, deadline,
		func(ctx context.Context, _ core.ProviderPort, _ core.Deployment) (
			*core.ProviderResponse, core.ChunkStream, error,
		) {
			<-ctx.Done()
			return nil, nil, core.WrapRetryable(core.CodeUpstreamTimeout, ctx.Err(), "timed out")
		})
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("expected the request to fail")
	}
	// One budget, not one per attempt. A little slack for scheduling.
	if elapsed > time.Second {
		t.Fatalf("took %v against a 600ms budget; the deadline was not shared", elapsed)
	}
}

func TestAnExhaustedDeadlineStopsBeforeAttemptingAgain(t *testing.T) {
	// Starting an attempt with almost no budget converts a useful error into a
	// timeout and tells the caller nothing.
	r := newRouter(t, defaultRegistry(), router.WithRetryBackoff(0))
	candidates := []router.Candidate{
		{Deployment: deployment("a", core.TrustInternal, "alpha")},
		{Deployment: deployment("b", core.TrustInternal, "beta")},
	}

	var tried atomic.Int64
	_, err := r.Execute(t.Context(), candidates, time.Now().Add(10*time.Millisecond),
		func(context.Context, core.ProviderPort, core.Deployment) (
			*core.ProviderResponse, core.ChunkStream, error,
		) {
			tried.Add(1)
			return nil, nil, core.New(core.CodeUpstreamError, "down").AsRetryable()
		})

	if err == nil {
		t.Fatal("expected a failure")
	}
	if tried.Load() != 0 {
		t.Fatalf("made %d attempts with no budget left", tried.Load())
	}
}

func TestAttemptsAreCappedIndependentlyOfCandidateCount(t *testing.T) {
	// A model with ten deployments must not turn one request into ten upstream
	// calls; the cap is on the caller's patience, not on the catalog.
	r := newRouter(t, defaultRegistry(),
		router.WithRetryBackoff(0), router.WithMaxAttempts(2))

	candidates := make([]router.Candidate, 0, 6)
	for i := range 6 {
		candidates = append(candidates,
			router.Candidate{Deployment: deployment(string(rune('a'+i)), core.TrustInternal, "alpha")})
	}

	var tried atomic.Int64
	_, _ = r.Execute(t.Context(), candidates, time.Time{},
		func(context.Context, core.ProviderPort, core.Deployment) (
			*core.ProviderResponse, core.ChunkStream, error,
		) {
			tried.Add(1)
			return nil, nil, core.New(core.CodeUpstreamError, "down").AsRetryable()
		})

	if tried.Load() != 2 {
		t.Fatalf("made %d attempts, want the configured cap of 2", tried.Load())
	}
}

func TestARequestThatSucceedsFirstTimeMakesOneCall(t *testing.T) {
	r := newRouter(t, defaultRegistry(), router.WithRetryBackoff(0))
	candidates := []router.Candidate{
		{Deployment: deployment("a", core.TrustInternal, "alpha")},
		{Deployment: deployment("b", core.TrustInternal, "beta")},
	}

	var tried atomic.Int64
	result, err := r.Execute(t.Context(), candidates, time.Time{},
		func(context.Context, core.ProviderPort, core.Deployment) (
			*core.ProviderResponse, core.ChunkStream, error,
		) {
			tried.Add(1)
			return &core.ProviderResponse{StatusCode: 200}, nil, nil
		})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if tried.Load() != 1 || result.Deployment.ID != "a" {
		t.Fatalf("made %d calls served by %q, want 1 by the first candidate",
			tried.Load(), result.Deployment.ID)
	}
}

func TestNewRejectsAMissingRegistry(t *testing.T) {
	if _, err := router.New(nil); err == nil {
		t.Fatal("a router with no registry could never resolve a provider")
	}
}
