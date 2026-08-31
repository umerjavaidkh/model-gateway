package snapshot_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/snapshot"
)

// build returns a minimal but valid snapshot at the given layer versions.
func build(t *testing.T, globalVersion, tenantVersion uint64) *core.Snapshot {
	t.Helper()

	global, err := core.NewGlobalLayer(core.GlobalSpec{
		Version: core.LayerVersion{Number: globalVersion},
		Deployments: []core.Deployment{
			{ID: "d1", Key: core.RoutingKey{BaseModel: "m"}, Provider: "echo", TrustTier: core.TrustInternal, Weight: 100},
		},
		TenantPrefixes: map[core.KeyPrefix]core.TenantID{"acme": "acme"},
	})
	if err != nil {
		t.Fatalf("NewGlobalLayer: %v", err)
	}
	tenant, err := core.NewTenantLayer(core.TenantSpec{
		Tenant:  "acme",
		Version: core.LayerVersion{Number: tenantVersion},
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

func newHolder(t *testing.T, opts ...snapshot.Option) *snapshot.Holder {
	t.Helper()
	h, err := snapshot.New(build(t, 1, 1), opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

func TestNewRejectsANilSnapshot(t *testing.T) {
	if _, err := snapshot.New(nil); err == nil {
		t.Fatal("a holder with no snapshot would serve requests against nothing")
	}
}

func TestALeaseKeepsItsVersionAfterASwap(t *testing.T) {
	// The property the whole design exists for: a request that authenticated
	// against version N must not route against N+1.
	h := newHolder(t)

	lease := h.Acquire()
	defer lease.Release()

	if err := h.Swap(build(t, 2, 1)); err != nil {
		t.Fatalf("Swap: %v", err)
	}

	if got := lease.Snapshot().GlobalVersion().Number; got != 1 {
		t.Fatalf("the lease moved to version %d; it must stay on 1", got)
	}
	if got := h.Current().GlobalVersion().Number; got != 2 {
		t.Fatalf("Current = %d, want the new version 2", got)
	}
}

func TestRetireWaitsForTheLastLease(t *testing.T) {
	// Plugins bound by a snapshot are unloaded on retire. Firing it while a
	// request is still using them is a crash, so the count has to be exact.
	var retired atomic.Int64
	h := newHolder(t, snapshot.OnRetire(func(*core.Snapshot) { retired.Add(1) }))

	lease := h.Acquire()

	// Version 1 leaves current on the first swap but stays loaded for rollback.
	if err := h.Swap(build(t, 2, 1)); err != nil {
		t.Fatalf("Swap: %v", err)
	}
	// The second swap pushes it out of the rollback slot, so only the lease
	// still holds it.
	if err := h.Swap(build(t, 3, 1)); err != nil {
		t.Fatalf("Swap: %v", err)
	}
	if got := retired.Load(); got != 0 {
		t.Fatalf("retired %d snapshots while a lease was open, want 0", got)
	}

	lease.Release()

	if got := retired.Load(); got != 1 {
		t.Fatalf("retired %d snapshots after the last lease closed, want 1", got)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	// `defer lease.Release()` has to compose with an explicit early release, or
	// every handler needs a bespoke cleanup path.
	var retired atomic.Int64
	h := newHolder(t, snapshot.OnRetire(func(*core.Snapshot) { retired.Add(1) }))

	lease := h.Acquire()
	lease.Release()
	lease.Release()
	lease.Release()

	if err := h.Swap(build(t, 2, 1)); err != nil {
		t.Fatalf("Swap: %v", err)
	}
	if err := h.Swap(build(t, 3, 1)); err != nil {
		t.Fatalf("Swap: %v", err)
	}
	if got := retired.Load(); got != 1 {
		t.Fatalf("retired %d times, want exactly 1: a double release corrupted the count", got)
	}
}

func TestRollbackRestoresThePreviousVersion(t *testing.T) {
	h := newHolder(t)
	if err := h.Swap(build(t, 2, 1)); err != nil {
		t.Fatalf("Swap: %v", err)
	}

	if err := h.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got := h.Current().GlobalVersion().Number; got != 1 {
		t.Fatalf("after rollback Current = %d, want 1", got)
	}

	// Only one generation is retained, so rollback is one level deep.
	if err := h.Rollback(); err == nil {
		t.Fatal("a second rollback must fail rather than guess")
	}
}

func TestRollbackRetiresTheFailedVersion(t *testing.T) {
	var retired []uint64
	var mu sync.Mutex
	h := newHolder(t, snapshot.OnRetire(func(s *core.Snapshot) {
		mu.Lock()
		defer mu.Unlock()
		retired = append(retired, s.GlobalVersion().Number)
	}))

	if err := h.Swap(build(t, 2, 1)); err != nil {
		t.Fatalf("Swap: %v", err)
	}
	if err := h.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(retired) != 1 || retired[0] != 2 {
		t.Fatalf("retired %v, want just the rolled-back version [2]", retired)
	}
}

func TestSwapRejectsVersionsThatMoveBackwards(t *testing.T) {
	// A watch stream can redeliver out of order after a reconnect. Applying a
	// stale snapshot reinstates deleted keys and refunds spent budget, and looks
	// like nothing at all until an audit.
	tests := []struct {
		name                         string
		globalVersion, tenantVersion uint64
		wantErr                      bool
	}{
		{name: "both advance", globalVersion: 6, tenantVersion: 4},
		{name: "only the tenant layer advances", globalVersion: 5, tenantVersion: 4},
		{name: "only the global layer advances", globalVersion: 6, tenantVersion: 3},
		{name: "nothing changes", globalVersion: 5, tenantVersion: 3},
		{name: "global layer regresses", globalVersion: 4, tenantVersion: 3, wantErr: true},
		{name: "tenant layer regresses", globalVersion: 5, tenantVersion: 2, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, err := snapshot.New(build(t, 5, 3))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			err = h.Swap(build(t, tc.globalVersion, tc.tenantVersion))
			if tc.wantErr && err == nil {
				t.Fatal("expected the stale swap to be rejected")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestSwapRejectsNil(t *testing.T) {
	h := newHolder(t)
	if err := h.Swap(nil); err == nil {
		t.Fatal("expected a nil swap to be rejected")
	}
	if h.Current() == nil {
		t.Fatal("a rejected swap must leave the current snapshot in place")
	}
}

func TestStatsReportsInFlightAndDrain(t *testing.T) {
	h := newHolder(t)

	a, b := h.Acquire(), h.Acquire()
	if got := h.Stats().InFlight; got != 2 {
		t.Fatalf("InFlight = %d, want 2", got)
	}

	if err := h.Swap(build(t, 2, 1)); err != nil {
		t.Fatalf("Swap: %v", err)
	}
	s := h.Stats()
	if s.InFlight != 0 || s.PreviousInFlight != 2 {
		t.Fatalf("after a swap: InFlight = %d, PreviousInFlight = %d; want 0 and 2", s.InFlight, s.PreviousInFlight)
	}
	if !s.PreviousLoaded || s.PreviousVersion.Number != 1 {
		t.Fatalf("previous = (%v, %v), want version 1 loaded", s.PreviousVersion, s.PreviousLoaded)
	}

	a.Release()
	b.Release()
	if got := h.Stats().PreviousInFlight; got != 0 {
		t.Fatalf("PreviousInFlight = %d after draining, want 0", got)
	}
}

// TestConcurrentAcquireAndSwap is the test this package exists for. It runs
// under -race in CI and is the only evidence that the lock-free read path is
// actually correct: readers churning leases while a writer swaps versions
// underneath them, with every retire accounted for.
func TestConcurrentAcquireAndSwap(t *testing.T) {
	const (
		readers = 32
		swaps   = 200
	)

	var retired atomic.Int64
	h := newHolder(t, snapshot.OnRetire(func(*core.Snapshot) { retired.Add(1) }))

	stop := make(chan struct{})
	var wg sync.WaitGroup

	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				lease := h.Acquire()
				// A leased snapshot must stay readable and self-consistent for
				// as long as the lease is held, however many swaps happen.
				if lease.Snapshot().GlobalVersion().Number == 0 {
					t.Error("leased a snapshot with no version")
				}
				lease.Release()
			}
		}()
	}

	for v := uint64(2); v <= swaps+1; v++ {
		if err := h.Swap(build(t, v, 1)); err != nil {
			t.Fatalf("Swap to %d: %v", v, err)
		}
	}
	close(stop)
	wg.Wait()

	// Every version except the current one and the retained previous one must
	// have been retired exactly once.
	if got, want := retired.Load(), int64(swaps-1); got != want {
		t.Fatalf("retired %d versions, want %d", got, want)
	}
	if got := h.Stats().InFlight; got != 0 {
		t.Fatalf("InFlight = %d after every reader stopped, want 0", got)
	}
}

func TestRetireRunsOnceUnderConcurrentRelease(t *testing.T) {
	var retired atomic.Int64
	h := newHolder(t, snapshot.OnRetire(func(*core.Snapshot) {
		retired.Add(1)
		time.Sleep(time.Millisecond) // widen the window for a double fire
	}))

	leases := make([]*snapshot.Lease, 64)
	for i := range leases {
		leases[i] = h.Acquire()
	}
	if err := h.Swap(build(t, 2, 1)); err != nil {
		t.Fatalf("Swap: %v", err)
	}
	if err := h.Swap(build(t, 3, 1)); err != nil {
		t.Fatalf("Swap: %v", err)
	}

	var wg sync.WaitGroup
	for _, lease := range leases {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease.Release()
		}()
	}
	wg.Wait()

	if got := retired.Load(); got != 1 {
		t.Fatalf("retire fired %d times, want exactly 1", got)
	}
}

// BenchmarkAcquireRelease substantiates the claim that the read path is free.
// Every request takes exactly one lease, so this cost is paid once per request
// and must stay far below the noise floor of a provider call.
func BenchmarkAcquireRelease(b *testing.B) {
	h, err := snapshot.New(build(&testing.T{}, 1, 1))
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			h.Acquire().Release()
		}
	})
}
