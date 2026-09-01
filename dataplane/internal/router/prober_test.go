package router_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/router"
)

// probeProvider records probe calls and can be made to fail.
type probeProvider struct {
	stubProvider
	probes atomic.Int64
	fail   atomic.Bool
}

func (p *probeProvider) Probe(context.Context, core.Deployment, core.Credential) error {
	p.probes.Add(1)
	if p.fail.Load() {
		return core.New(core.CodeUnavailable, "unreachable")
	}
	return nil
}

func noCredentials(context.Context, string) (core.Credential, error) {
	return core.Credential{}, nil
}

func TestIdleDeploymentsAreProbed(t *testing.T) {
	// Passive health can only measure what receives traffic, and the
	// deployments nobody is sure about are exactly the ones not receiving any.
	provider := &probeProvider{stubProvider: stubProvider{name: "alpha"}}
	snap := snapshotWith(t, deployment("idle-1", core.TrustInternal, "alpha"))
	r := newRouter(t, registry{"alpha": provider})

	prober, err := router.NewProber(r, func() *core.Snapshot { return snap }, noCredentials,
		router.WithProberLogger(quiet()))
	if err != nil {
		t.Fatalf("NewProber: %v", err)
	}

	if probed := prober.RunOnce(t.Context()); probed != 1 {
		t.Fatalf("probed %d deployments, want 1", probed)
	}
	if provider.probes.Load() != 1 {
		t.Fatalf("provider saw %d probes", provider.probes.Load())
	}
}

func TestDeploymentsServingTrafficAreNotProbed(t *testing.T) {
	// They are already being measured; probing them adds load to learn what is
	// already known.
	provider := &probeProvider{stubProvider: stubProvider{name: "alpha"}}
	snap := snapshotWith(t, deployment("busy-1", core.TrustInternal, "alpha"))
	r := newRouter(t, registry{"alpha": provider})

	prober, err := router.NewProber(r, func() *core.Snapshot { return snap }, noCredentials,
		router.WithProberLogger(quiet()))
	if err != nil {
		t.Fatalf("NewProber: %v", err)
	}

	prober.Saw("busy-1")
	if probed := prober.RunOnce(t.Context()); probed != 0 {
		t.Fatalf("probed %d deployments that are already serving traffic", probed)
	}
}

func TestASuccessfulProbeCanCloseABreaker(t *testing.T) {
	// The point of probing: a recovered deployment comes back without a real
	// request having to discover it.
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

	provider := &probeProvider{stubProvider: stubProvider{name: "alpha"}}
	snap := snapshotWith(t, deployment("d1", core.TrustInternal, "alpha"))
	r := newRouter(t, registry{"alpha": provider},
		router.WithRetryBackoff(0), router.WithClock(clock),
		router.WithBreakerOptions(
			router.WithFailureThreshold(1),
			router.WithCooldown(30*time.Second),
			router.WithBreakerClock(clock),
		))

	only := []router.Candidate{{Deployment: deployment("d1", core.TrustInternal, "alpha")}}
	_, _ = r.Execute(t.Context(), only, time.Time{},
		failingCall(core.New(core.CodeUpstreamError, "down").AsRetryable()))

	prober, err := router.NewProber(r, func() *core.Snapshot { return snap }, noCredentials,
		router.WithProberLogger(quiet()), router.WithProberClock(clock))
	if err != nil {
		t.Fatalf("NewProber: %v", err)
	}

	// Two successful probes after the cooldown, matching the half-open
	// threshold, must close the breaker.
	advance(31 * time.Second)
	prober.RunOnce(t.Context())
	prober.RunOnce(t.Context())

	ok := func(context.Context, core.ProviderPort, core.Deployment) (
		*core.ProviderResponse, core.ChunkStream, error,
	) {
		return &core.ProviderResponse{StatusCode: 200}, nil, nil
	}
	if _, err := r.Execute(t.Context(), only, time.Time{}, ok); err != nil {
		t.Fatalf("a probed-healthy deployment is still refused: %v", err)
	}
}

func TestAFailingProbeLowersTheScore(t *testing.T) {
	provider := &probeProvider{stubProvider: stubProvider{name: "alpha"}}
	provider.fail.Store(true)
	snap := snapshotWith(t, deployment("d1", core.TrustInternal, "alpha"))
	r := newRouter(t, registry{"alpha": provider})

	prober, err := router.NewProber(r, func() *core.Snapshot { return snap }, noCredentials,
		router.WithProberLogger(quiet()))
	if err != nil {
		t.Fatalf("NewProber: %v", err)
	}
	for range 8 {
		prober.RunOnce(t.Context())
	}

	stats := r.Stats()
	if len(stats) != 1 {
		t.Fatalf("stats = %v", stats)
	}
	if stats[0].Score >= 1 {
		t.Fatalf("score = %v after repeated probe failures, want it lowered", stats[0].Score)
	}
}

func TestAnUnresolvableCredentialDoesNotScoreTheDeploymentDown(t *testing.T) {
	// A credential that cannot be resolved is a real problem, but it is not
	// this deployment being unhealthy — and moving traffic away for a reason
	// the deployment cannot fix would be the wrong response.
	provider := &probeProvider{stubProvider: stubProvider{name: "alpha"}}
	snap := snapshotWith(t, deployment("d1", core.TrustInternal, "alpha"))
	r := newRouter(t, registry{"alpha": provider})

	prober, err := router.NewProber(r, func() *core.Snapshot { return snap },
		func(context.Context, string) (core.Credential, error) {
			return core.Credential{}, core.New(core.CodeUnavailable, "vault is down")
		},
		router.WithProberLogger(quiet()))
	if err != nil {
		t.Fatalf("NewProber: %v", err)
	}

	prober.RunOnce(t.Context())
	if provider.probes.Load() != 0 {
		t.Fatal("probed without a credential")
	}
	for _, stat := range r.Stats() {
		if stat.Observations != 0 {
			t.Fatal("a credential failure was recorded against the deployment's health")
		}
	}
}

func TestStopEndsTheLoopAndIsIdempotent(t *testing.T) {
	provider := &probeProvider{stubProvider: stubProvider{name: "alpha"}}
	snap := snapshotWith(t, deployment("d1", core.TrustInternal, "alpha"))
	r := newRouter(t, registry{"alpha": provider})

	prober, err := router.NewProber(r, func() *core.Snapshot { return snap }, noCredentials,
		router.WithProberLogger(quiet()), router.WithProbeInterval(time.Millisecond))
	if err != nil {
		t.Fatalf("NewProber: %v", err)
	}

	prober.Start(t.Context())
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && provider.probes.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	prober.Stop()
	prober.Stop()

	if provider.probes.Load() == 0 {
		t.Fatal("the loop never probed")
	}
}

func TestNewProberRejectsMissingDependencies(t *testing.T) {
	r := newRouter(t, defaultRegistry())
	snap := func() *core.Snapshot { return nil }

	if _, err := router.NewProber(nil, snap, noCredentials); err == nil {
		t.Fatal("a prober with no router must be refused")
	}
	if _, err := router.NewProber(r, nil, noCredentials); err == nil {
		t.Fatal("a prober with no snapshot source must be refused")
	}
	if _, err := router.NewProber(r, snap, nil); err == nil {
		t.Fatal("a prober with no credential source must be refused")
	}
}
