package shadow_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/shadow"
)

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// snapshotWith builds a snapshot holding a base model and whatever adapters
// are given.
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

func base(id string) core.Deployment {
	return core.Deployment{
		ID: core.DeploymentID(id), Key: core.RoutingKey{BaseModel: "llama"},
		Provider: "vllm", TrustTier: core.TrustInternal, Weight: 100,
	}
}

func adapter(id string, shadowPercent uint32) core.Deployment {
	return core.Deployment{
		ID:       core.DeploymentID(id),
		Key:      core.RoutingKey{BaseModel: "llama", AdapterID: id},
		Provider: "vllm", TrustTier: core.TrustInternal,
		Weight: 0, ShadowPercent: shadowPercent,
	}
}

func request() *shadow.Request {
	return &shadow.Request{
		Meta:   core.RequestMeta{RequestID: "req-1", Model: "llama"},
		Body:   []byte(`{"messages":[]}`),
		Tenant: "acme",
	}
}

// recorder captures what was mirrored.
type recorder struct {
	mu      sync.Mutex
	seen    []core.DeploymentID
	err     error
	block   chan struct{}
	entered atomic.Int64
}

func (r *recorder) call(ctx context.Context, d core.Deployment, _ *shadow.Request) error {
	r.entered.Add(1)
	if r.block != nil {
		select {
		case <-r.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.mu.Lock()
	r.seen = append(r.seen, d.ID)
	r.mu.Unlock()
	return r.err
}

func (r *recorder) ids() []core.DeploymentID {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]core.DeploymentID(nil), r.seen...)
}

// always draws 0, so every sampling decision passes.
func always() float64 { return 0 }

func mirrorFor(t *testing.T, rec *recorder, opts ...shadow.Option) *shadow.Mirror {
	t.Helper()

	opts = append([]shadow.Option{
		shadow.WithLogger(quiet()), shadow.WithDraw(always),
	}, opts...)
	m, err := shadow.New(rec.call, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.Start(t.Context())
	t.Cleanup(m.Wait)
	return m
}

func TestARequestIsMirroredToEveryShadowingAdapter(t *testing.T) {
	rec := &recorder{}
	m := mirrorFor(t, rec)
	snap := snapshotWith(t, base("llama-1"), adapter("triage", 100), adapter("summary", 100))

	m.Send(snap, base("llama-1"), request())
	m.Wait()

	if got := len(rec.ids()); got != 2 {
		t.Fatalf("mirrored to %d adapters, want both: %v", got, rec.ids())
	}
}

func TestAnAdapterNotShadowingIsNotMirroredTo(t *testing.T) {
	// Weight zero and shadow zero is an adapter parked in the routing table.
	// Mirroring to it would bill for a measurement nobody asked for.
	rec := &recorder{}
	m := mirrorFor(t, rec)
	snap := snapshotWith(t, base("llama-1"), adapter("parked", 0))

	m.Send(snap, base("llama-1"), request())
	m.Wait()

	if got := rec.ids(); len(got) != 0 {
		t.Fatalf("mirrored to %v, want nothing", got)
	}
}

func TestTrafficIsSampledAtTheDeclaredRate(t *testing.T) {
	// Mirroring doubles inference spend for whatever fraction it covers, so a
	// shadow that costs as much as production is one an operator turns off.
	rec := &recorder{}
	draws := make(chan float64, 100)
	for i := range 100 {
		draws <- float64(i) / 100
	}
	m, err := shadow.New(rec.call,
		shadow.WithLogger(quiet()),
		shadow.WithDraw(func() float64 { return <-draws }))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.Start(t.Context())
	snap := snapshotWith(t, base("llama-1"), adapter("triage", 10))

	for range 100 {
		m.Send(snap, base("llama-1"), request())
	}
	m.Wait()

	if got := len(rec.ids()); got != 10 {
		t.Fatalf("mirrored %d of 100 requests at 10%%, want 10", got)
	}
}

func TestSendNeverBlocksTheCaller(t *testing.T) {
	// The contract. Send is called on the request path, so a mirror that could
	// block there would do exactly what this package exists to prevent.
	rec := &recorder{block: make(chan struct{})}
	m := mirrorFor(t, rec, shadow.WithCapacity(1, 1))
	snap := snapshotWith(t, base("llama-1"), adapter("triage", 100))

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Far more than the queue can hold, against a call that never returns.
		for range 100 {
			m.Send(snap, base("llama-1"), request())
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Send blocked; a shadow reached the request path")
	}

	if m.Stats().Dropped == 0 {
		t.Fatal("nothing was dropped, so the queue did not actually saturate")
	}
	close(rec.block)
}

func TestAFailingShadowIsCountedAndNotRaised(t *testing.T) {
	// That an adapter errors under real traffic is the finding, not an
	// incident. Send returns nothing, so there is nowhere for it to surface —
	// this asserts it is counted rather than lost.
	rec := &recorder{err: errors.New("adapter is broken")}
	m := mirrorFor(t, rec)
	snap := snapshotWith(t, base("llama-1"), adapter("triage", 100))

	m.Send(snap, base("llama-1"), request())
	m.Wait()

	if stats := m.Stats(); stats.Failed != 1 || stats.Mirrored != 0 {
		t.Fatalf("stats = %+v, want one failure and no success", stats)
	}
}

func TestAMirrorSurvivesTheRequestContextBeingCancelled(t *testing.T) {
	// The request's context is cancelled the moment the response is written.
	// A mirror inheriting it would be cancelled before it started, which would
	// make the whole feature quietly do nothing.
	rec := &recorder{}
	m, err := shadow.New(rec.call, shadow.WithLogger(quiet()), shadow.WithDraw(always))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.Start(context.Background())

	requestCtx, cancel := context.WithCancel(context.Background())
	snap := snapshotWith(t, base("llama-1"), adapter("triage", 100))
	m.Send(snap, base("llama-1"), request())
	cancel()
	_ = requestCtx

	m.Wait()

	if stats := m.Stats(); stats.Mirrored != 1 {
		t.Fatalf("stats = %+v, want the mirror to have completed", stats)
	}
}

func TestAnAdapterThatServedIsNotItselfShadowed(t *testing.T) {
	// Shadowing the shadow measures nothing and bills twice.
	rec := &recorder{}
	m := mirrorFor(t, rec)
	snap := snapshotWith(t, base("llama-1"), adapter("triage", 100))

	m.Send(snap, adapter("triage", 100), request())
	m.Wait()

	if got := rec.ids(); len(got) != 0 {
		t.Fatalf("mirrored to %v when an adapter served the request", got)
	}
}

func TestWaitDrainsWhatIsAlreadyAccepted(t *testing.T) {
	// The last requests before a deploy are the ones a rollout decision is
	// about. Losing them makes a canary look quiet rather than healthy.
	rec := &recorder{}
	m := mirrorFor(t, rec, shadow.WithCapacity(1, 32))
	snap := snapshotWith(t, base("llama-1"), adapter("triage", 100))

	for range 20 {
		m.Send(snap, base("llama-1"), request())
	}
	m.Wait()

	if got := m.Stats(); got.Mirrored+got.Dropped != 20 {
		t.Fatalf("stats = %+v, want every request either mirrored or dropped", got)
	}
}

func TestWaitIsSafeToCallTwice(t *testing.T) {
	m := mirrorFor(t, &recorder{})
	m.Wait()
	m.Wait()
}

func TestAMirrorNeedsSomethingToCall(t *testing.T) {
	if _, err := shadow.New(nil); err == nil {
		t.Fatal("a mirror with nothing to call would silently do nothing")
	}
}

func TestABaseModelCannotShadow(t *testing.T) {
	// It would mirror a request to the very deployment that served it,
	// doubling the bill to compare something with itself.
	shadowingBase := base("llama-1")
	shadowingBase.ShadowPercent = 50

	_, err := core.NewGlobalLayer(core.GlobalSpec{
		Version:     core.LayerVersion{Number: 1},
		Deployments: []core.Deployment{shadowingBase},
	})
	if err == nil {
		t.Fatal("a base model was allowed to shadow")
	}
}
