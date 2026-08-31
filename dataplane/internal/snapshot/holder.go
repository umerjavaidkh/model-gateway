// Package snapshot holds the worker side of snapshot distribution: the Holder
// that owns the configuration a worker is currently serving, and the machinery
// that makes replacing it safe while requests are in flight.
//
// # What the Holder is for
//
// Three properties have to hold at once, and they pull against each other:
//
//  1. Reading the current snapshot must be free. It happens on every request,
//     several times, so a mutex on the read path is not acceptable.
//  2. A request must see one consistent configuration for its whole life. A
//     request that authenticated against version N must not route against N+1;
//     that is how a request gets admitted under one budget and billed under
//     another.
//  3. A displaced version must be unloadable, but only once nothing is using
//     it. Plugins bound by a snapshot own resources — a sidecar connection, a
//     WASM instance — and freeing them under an in-flight request is a crash.
//
// The Holder gets all three from one mechanism: snapshots are immutable, the
// current one is swapped by a single atomic pointer store, and a request takes
// a Lease that keeps its version alive until it is done.
//
// # Why the previous version stays loaded
//
// The Holder keeps a reference on the previous version as well as the current
// one, so rollback is instant and needs no rebuild. This is what makes a canary
// halt cheap: reverting to N−1 is a pointer store, not a fetch. The cost is one
// extra snapshot's worth of memory and one extra generation of plugins loaded,
// which is the right trade for a rollback that works when the control plane is
// the thing that is broken.
package snapshot

import (
	"sync"
	"sync/atomic"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

// RetireFunc is called once a displaced snapshot has no remaining leases.
//
// This is the drain-then-unload signal: by the time it runs, no request is
// using that configuration, so the plugins it bound can be shut down. It runs
// on whichever goroutine released the last lease, so it must not block. Long
// teardown belongs on a goroutine the callback starts.
type RetireFunc func(*core.Snapshot)

// Holder owns the snapshot a worker is serving.
//
// It is safe for concurrent use. Reads are lock-free; swaps are serialized by a
// mutex that the request path never touches.
type Holder struct {
	current atomic.Pointer[generation]

	// mu serializes swaps and rollbacks only. Acquire never takes it.
	mu       sync.Mutex
	previous *generation

	onRetire RetireFunc
}

// generation is one loaded snapshot together with its reference count.
//
// The count starts at 1: the Holder itself holds a reference for as long as the
// generation is reachable as current or previous. That is what guarantees a
// generation cannot be retired out from under a request that is about to lease
// it.
type generation struct {
	snap     *core.Snapshot
	refs     atomic.Int64
	retire   RetireFunc
	retireOn sync.Once
}

func newGeneration(snap *core.Snapshot, retire RetireFunc) *generation {
	g := &generation{snap: snap, retire: retire}
	g.refs.Store(1)
	return g
}

// tryRef takes a reference, failing if the generation has already been retired.
//
// The check has to be a compare-and-swap rather than a plain increment. A
// generation loaded from the current pointer can be displaced twice — current
// to previous to released — between the load and the increment, and a plain
// increment would resurrect a generation whose plugins are already unloading.
func (g *generation) tryRef() bool {
	for {
		n := g.refs.Load()
		if n <= 0 {
			return false
		}
		if g.refs.CompareAndSwap(n, n+1) {
			return true
		}
	}
}

func (g *generation) unref() {
	if g.refs.Add(-1) > 0 {
		return
	}
	g.retireOn.Do(func() {
		if g.retire != nil {
			g.retire(g.snap)
		}
	})
}

// Option configures a Holder.
type Option func(*Holder)

// OnRetire registers the callback invoked when a displaced snapshot finishes
// draining. Exactly one callback is supported; the last one set wins.
func OnRetire(fn RetireFunc) Option {
	return func(h *Holder) { h.onRetire = fn }
}

// New returns a Holder serving initial.
func New(initial *core.Snapshot, opts ...Option) (*Holder, error) {
	if initial == nil {
		return nil, core.New(core.CodeInvalidRequest, "a holder needs an initial snapshot")
	}
	h := &Holder{}
	for _, opt := range opts {
		opt(h)
	}
	h.current.Store(newGeneration(initial, h.onRetire))
	return h, nil
}

// Lease is a borrowed reference to one snapshot version.
//
// Every request takes exactly one at ingress and releases it at completion,
// including on the error path — a leaked lease pins a generation and its
// plugins for the life of the process. `defer lease.Release()` immediately
// after Acquire is the only pattern that should appear in the request path.
type Lease struct {
	gen  *generation
	once sync.Once
}

// Snapshot returns the leased configuration. It is valid until Release.
func (l *Lease) Snapshot() *core.Snapshot { return l.gen.snap }

// Release returns the lease. It is idempotent, so a deferred Release composes
// with an explicit early one.
func (l *Lease) Release() {
	l.once.Do(l.gen.unref)
}

// Acquire borrows the current snapshot. It never blocks and never fails.
func (h *Holder) Acquire() *Lease {
	// The loop retries only if the generation was retired between the load and
	// the reference. Swaps are serialized, so a live current generation always
	// has the Holder's own reference and the retry is bounded in practice.
	for {
		g := h.current.Load()
		if g.tryRef() {
			return &Lease{gen: g}
		}
	}
}

// Current returns the current snapshot without taking a lease.
//
// It is for observers — metrics, health endpoints, log fields — that read one
// value and do not care if it is replaced a moment later. The request path must
// use Acquire, or it can read a version that retires while it is still working.
func (h *Holder) Current() *core.Snapshot {
	return h.current.Load().snap
}

// Swap installs a new snapshot and starts draining the one it displaces.
//
// It rejects a snapshot that moves any layer's version backwards. A watch
// stream can deliver out of order after a reconnect, and applying a stale
// snapshot silently reinstates deleted keys and refunds spent budget — a
// failure that looks like nothing at all until an audit.
func (h *Holder) Swap(next *core.Snapshot) error {
	if next == nil {
		return core.New(core.CodeInvalidRequest, "cannot swap in a nil snapshot")
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	cur := h.current.Load()
	if err := checkMonotonic(cur.snap, next); err != nil {
		return err
	}

	displaced := h.previous
	h.previous = cur
	h.current.Store(newGeneration(next, h.onRetire))

	// Released last, and outside the read path: the generation now leaving the
	// rollback slot is the only one whose plugins may unload.
	if displaced != nil {
		displaced.unref()
	}
	return nil
}

// Rollback reverts to the previous snapshot, which is still loaded.
//
// This is the canary halt: an adapter regressing in production is reverted by a
// pointer store rather than by asking a possibly-unhealthy control plane to
// rebuild anything. Only one generation is retained, so rollback is one level
// deep and a second call fails until another Swap.
func (h *Holder) Rollback() error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.previous == nil {
		return core.New(core.CodeInvalidRequest, "no previous snapshot to roll back to")
	}

	failed := h.current.Load()
	h.current.Store(h.previous)
	h.previous = nil
	failed.unref()
	return nil
}

// Stats is a point-in-time view of the Holder, for metrics and health output.
type Stats struct {
	Version         core.LayerVersion
	InFlight        int64
	PreviousVersion core.LayerVersion
	PreviousLoaded  bool
	// PreviousInFlight is the number of requests still running against the
	// displaced version. It is the drain gauge: it should fall to zero within
	// one request timeout of a swap, and an alert on it not doing so is how a
	// leaked lease gets found.
	PreviousInFlight int64
}

// Stats reports the Holder's current state.
func (h *Holder) Stats() Stats {
	h.mu.Lock()
	prev := h.previous
	h.mu.Unlock()

	cur := h.current.Load()
	s := Stats{
		Version: cur.snap.GlobalVersion(),
		// One reference is the Holder's own, so in-flight is the count minus it.
		InFlight: cur.refs.Load() - 1,
	}
	if prev != nil {
		s.PreviousLoaded = true
		s.PreviousVersion = prev.snap.GlobalVersion()
		s.PreviousInFlight = prev.refs.Load() - 1
	}
	return s
}

// checkMonotonic rejects a snapshot that would move any layer backwards.
//
// Versions are compared per layer, not per snapshot, because layers are
// versioned independently: a snapshot may legitimately carry an unchanged
// global version with a newer tenant layer, or the reverse. A tenant absent
// from the next snapshot is an offboarding, not a regression, so it is allowed.
func checkMonotonic(current, next *core.Snapshot) error {
	if got, have := next.GlobalVersion().Number, current.GlobalVersion().Number; got < have {
		return core.Newf(core.CodeInvalidRequest,
			"global layer version would move backwards, from %d to %d", have, got)
	}
	for _, tenant := range next.TenantIDs() {
		have, ok := current.TenantVersion(tenant)
		if !ok {
			continue // a tenant appearing for the first time
		}
		got, _ := next.TenantVersion(tenant)
		if got.Number < have.Number {
			return core.Newf(core.CodeInvalidRequest,
				"tenant %q layer version would move backwards, from %d to %d", tenant, have.Number, got.Number)
		}
	}
	return nil
}
