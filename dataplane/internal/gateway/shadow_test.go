package gateway_test

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/adapters/echo"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/gateway"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/shadow"
)

func silent() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// shadowed records which deployments were mirrored to, and can be made slow.
type shadowed struct {
	mu    sync.Mutex
	seen  []core.DeploymentID
	delay time.Duration
}

func (s *shadowed) call(ctx context.Context, d core.Deployment, _ *shadow.Request) error {
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.mu.Lock()
	s.seen = append(s.seen, d.ID)
	s.mu.Unlock()
	return nil
}

func (s *shadowed) ids() []core.DeploymentID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]core.DeploymentID(nil), s.seen...)
}

func adapterOf(base core.RoutingKey, id string, shadowPercent uint32) core.Deployment {
	return core.Deployment{
		ID:       core.DeploymentID(id),
		Key:      core.RoutingKey{BaseModel: base.BaseModel, AdapterID: id},
		Provider: "echo", TrustTier: core.TrustInternal,
		Weight: 0, ShadowPercent: shadowPercent,
	}
}

func withShadows(t *testing.T, sink *shadowed) (*gateway.Pipeline, *shadow.Mirror) {
	t.Helper()

	mirror, err := shadow.New(sink.call,
		shadow.WithLogger(silent()), shadow.WithDraw(func() float64 { return 0 }))
	if err != nil {
		t.Fatalf("shadow.New: %v", err)
	}
	mirror.Start(t.Context())
	t.Cleanup(mirror.Wait)

	return buildPipeline(t, gateway.WithShadows(mirror)), mirror
}

func TestAServedRequestIsMirroredToAShadowingAdapter(t *testing.T) {
	sink := &shadowed{}
	p, mirror := withShadows(t, sink)
	snap := buildSnapshot(t, snapshotOpts{
		deployments: []core.Deployment{
			{ID: "echo-1", Key: routeEcho, Provider: "echo",
				TrustTier: core.TrustInternal, Weight: 100},
			adapterOf(routeEcho, "triage", 100),
		},
	})

	if _, err := p.Handle(t.Context(), snap, request("echo-model", "gw_acme_secret-1")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	mirror.Wait()

	if got := sink.ids(); len(got) != 1 || got[0] != "triage" {
		t.Fatalf("mirrored to %v, want the shadowing adapter", got)
	}
}

func TestASlowShadowDoesNotSlowTheCaller(t *testing.T) {
	// The contract the whole package exists for. A shadow that can add latency
	// makes production worse to find out whether something might make it
	// better, which is a trade nobody would take if it were stated.
	sink := &shadowed{delay: 3 * time.Second}
	p, _ := withShadows(t, sink)
	snap := buildSnapshot(t, snapshotOpts{
		deployments: []core.Deployment{
			{ID: "echo-1", Key: routeEcho, Provider: "echo",
				TrustTier: core.TrustInternal, Weight: 100},
			adapterOf(routeEcho, "triage", 100),
		},
	})

	started := time.Now()
	if _, err := p.Handle(t.Context(), snap, request("echo-model", "gw_acme_secret-1")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	elapsed := time.Since(started)

	if elapsed > time.Second {
		t.Fatalf("the request took %s behind a 3s shadow", elapsed)
	}
}

func TestAFailingShadowDoesNotFailTheCaller(t *testing.T) {
	// That an adapter errors under real traffic is the finding. The caller
	// asked the base model and got their answer.
	failing := func(context.Context, core.Deployment, *shadow.Request) error {
		return core.New(core.CodeUpstreamError, "the adapter is broken")
	}
	mirror, err := shadow.New(failing, shadow.WithLogger(silent()),
		shadow.WithDraw(func() float64 { return 0 }))
	if err != nil {
		t.Fatalf("shadow.New: %v", err)
	}
	mirror.Start(t.Context())
	defer mirror.Wait()

	p := buildPipeline(t, gateway.WithShadows(mirror))
	snap := buildSnapshot(t, snapshotOpts{
		deployments: []core.Deployment{
			{ID: "echo-1", Key: routeEcho, Provider: "echo",
				TrustTier: core.TrustInternal, Weight: 100},
			adapterOf(routeEcho, "triage", 100),
		},
	})

	result, err := p.Handle(t.Context(), snap, request("echo-model", "gw_acme_secret-1"))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; a broken shadow reached the caller", result.StatusCode)
	}
}

func TestNothingIsMirroredWithoutAShadowingAdapter(t *testing.T) {
	sink := &shadowed{}
	p, mirror := withShadows(t, sink)
	snap := buildSnapshot(t, snapshotOpts{})

	if _, err := p.Handle(t.Context(), snap, request("echo-model", "gw_acme_secret-1")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	mirror.Wait()

	if got := sink.ids(); len(got) != 0 {
		t.Fatalf("mirrored to %v with no adapter shadowing", got)
	}
}

func TestAPipelineWithNoMirrorServesNormally(t *testing.T) {
	// Shadowing is optional: a deployment running no canaries starts no mirror
	// at all, and the nil must not be a nil-pointer dereference on every
	// request.
	p := buildPipeline(t)
	snap := buildSnapshot(t, snapshotOpts{
		deployments: []core.Deployment{
			{ID: "echo-1", Key: routeEcho, Provider: "echo",
				TrustTier: core.TrustInternal, Weight: 100},
			adapterOf(routeEcho, "triage", 100),
		},
	})

	if _, err := p.Handle(t.Context(), snap, request("echo-model", "gw_acme_secret-1")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
}

func TestAMirroredRequestGetsItsOwnUsageEvent(t *testing.T) {
	// It cost real money and belongs in the record. What it must not do is
	// collide with the request that spawned it: usage records are keyed by
	// request id for idempotency, so reusing it would make the accounting
	// consumer discard one of the two as a duplicate — and which one would
	// depend on arrival order.
	sink := &collector{}
	providers, err := gateway.NewStaticProviders(echo.New())
	if err != nil {
		t.Fatalf("NewStaticProviders: %v", err)
	}

	var p *gateway.Pipeline
	mirror, err := shadow.New(
		func(ctx context.Context, d core.Deployment, req *shadow.Request) error {
			return p.ShadowCall(ctx, d, req)
		},
		shadow.WithLogger(silent()), shadow.WithDraw(func() float64 { return 0 }))
	if err != nil {
		t.Fatalf("shadow.New: %v", err)
	}
	mirror.Start(t.Context())

	p, err = gateway.New(providers, gateway.NoCredentials{}, pepper,
		gateway.WithClock(func() time.Time { return now }),
		gateway.WithTelemetry(sink),
		gateway.WithShadows(mirror))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	snap := buildSnapshot(t, snapshotOpts{
		deployments: []core.Deployment{
			{ID: "echo-1", Key: routeEcho, Provider: "echo",
				TrustTier: core.TrustInternal, Weight: 100},
			adapterOf(routeEcho, "triage", 100),
		},
	})
	if _, err := p.Handle(t.Context(), snap, request("echo-model", "gw_acme_secret-1")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	mirror.Wait()

	events := sink.all()
	if len(events) != 2 {
		t.Fatalf("got %d events, want the request and its mirror", len(events))
	}

	var primary, mirrored core.UsageEvent
	for _, e := range events {
		if e.Shadow {
			mirrored = e
		} else {
			primary = e
		}
	}

	if mirrored.RequestID == "" || mirrored.RequestID == primary.RequestID {
		t.Fatalf("the mirror reused the request id %q", mirrored.RequestID)
	}
	if mirrored.Deployment != "triage" || !mirrored.Route.IsAdapter() {
		t.Fatalf("the mirror was attributed to %q", mirrored.Deployment)
	}
	// Nobody asked for it, so charging a tenant would bill them for an
	// experiment the platform chose to run. The cost is still recorded.
	if mirrored.PriceMicroUSD != 0 {
		t.Fatalf("the mirror was priced at %d, want zero", mirrored.PriceMicroUSD)
	}
}
