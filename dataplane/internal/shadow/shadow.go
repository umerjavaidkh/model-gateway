// Package shadow mirrors real requests to an adapter nobody is serving from,
// and throws the answer away.
//
// It is how a canary earns its first step. The eval gate (ADR 0014) already
// established that an adapter is better on a fixed suite; what it cannot know
// is whether the adapter survives *this tenant's* traffic — the prompts that
// suite never contained, at the concurrency production actually runs at. A
// mirrored copy answers that without a single caller being exposed to it.
//
// # What this measures, and what it does not
//
// Operational signals only: does it error, how long does it take, what does it
// cost. Not quality. Judging whether the shadow's answer was *better* means
// either storing both payloads — which this system deliberately does not do —
// or running a judge over them, which is what the eval suite already is.
//
// Saying that plainly matters, because "we shadow traffic" is easily heard as
// "we compare answers", and a promotion decision resting on that
// misunderstanding would be resting on nothing.
//
// # What it must never do
//
// Touch the caller. Not its latency, not its errors, not its deadline. A
// shadow that can slow a request down is a feature that makes production worse
// to find out whether something might make it better, which is a trade nobody
// would take if it were stated.
//
// So: mirroring happens after the response is on its way, on a context
// detached from the request's, under a bounded worker pool that drops rather
// than queues. Every one of those is load-bearing and each has a test.
package shadow

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

const (
	// DefaultTimeout bounds one mirrored call.
	//
	// Generous next to a real request's deadline, because nobody is waiting:
	// the only reason to bound it at all is that a hung shadow would hold a
	// worker that another mirror could use.
	DefaultTimeout = 30 * time.Second

	// DefaultWorkers is how many mirrors may be in flight at once.
	//
	// A fixed pool, not a goroutine per request. Under load the second is a
	// memory bomb: mirrors are slower than the requests that spawn them, so
	// they accumulate exactly when the system can least afford it.
	DefaultWorkers = 8

	// DefaultQueue is how many mirrors may wait for a worker.
	//
	// Small on purpose. A deep queue turns a slow shadow into stale shadow
	// data — requests measured minutes after they were served, against a
	// snapshot that may no longer be current.
	DefaultQueue = 64
)

// Call performs one mirrored request against a deployment.
//
// Supplied by the pipeline, which owns credential resolution and provider
// selection. This package decides *whether* and *when* to mirror; it does not
// learn how to talk to a provider.
type Call func(ctx context.Context, deployment core.Deployment, req *Request) error

// Request is the copy handed to a shadow.
//
// Already guarded, already transformed: the shadow sees exactly what the
// primary deployment saw, because a shadow measured on a different payload is
// measuring something other than production.
type Request struct {
	Meta core.RequestMeta
	Body []byte
	// Tenant and Principal are carried so the mirrored call's usage event is
	// attributed to whoever's traffic was mirrored, which is who pays for it.
	Tenant    core.TenantID
	Principal core.KeyID
	// Tier and SnapshotVersion are copied out of the snapshot rather than the
	// snapshot being carried. A mirror outlives the request that spawned it,
	// and holding a snapshot pointer past the request's lease would pin that
	// generation — and its plugins — for as long as the queue is deep. Two
	// scalars are all the usage event needs.
	Tier            string
	SnapshotVersion uint64
	// Budgets the mirrored spend belongs to, captured when the request was
	// served for the same reason the primary event captures them.
	BudgetIDs []core.BudgetID
}

// Stats is what a mirror has done, for a metric and for a test.
type Stats struct {
	Mirrored int64
	Dropped  int64
	Failed   int64
}

// Mirror sends copies of real traffic to shadowing adapters.
type Mirror struct {
	call    Call
	logger  *slog.Logger
	timeout time.Duration
	draw    func() float64

	workers   int
	queueSize int

	queue chan job
	wg    sync.WaitGroup
	// stop closes the queue exactly once, so a second Wait cannot panic on a
	// closed channel.
	stopOnce sync.Once

	mirrored atomic.Int64
	dropped  atomic.Int64
	failed   atomic.Int64
}

type job struct {
	deployment core.Deployment
	request    *Request
}

// Option configures a Mirror.
type Option func(*Mirror)

// WithLogger sets where dropped and failed mirrors are reported.
func WithLogger(l *slog.Logger) Option {
	return func(m *Mirror) {
		if l != nil {
			m.logger = l
		}
	}
}

// WithTimeout bounds one mirrored call.
func WithTimeout(d time.Duration) Option {
	return func(m *Mirror) {
		if d > 0 {
			m.timeout = d
		}
	}
}

// WithDraw replaces the source of randomness used to sample traffic, so a test
// can assert a sample rate rather than sample one.
func WithDraw(draw func() float64) Option {
	return func(m *Mirror) {
		if draw != nil {
			m.draw = draw
		}
	}
}

// WithCapacity sets the worker count and queue depth.
func WithCapacity(workers, queue int) Option {
	return func(m *Mirror) {
		if workers > 0 {
			m.workers = workers
		}
		if queue > 0 {
			m.queueSize = queue
		}
	}
}

// New returns a Mirror that is not yet running. Call Start.
func New(call Call, opts ...Option) (*Mirror, error) {
	if call == nil {
		return nil, core.New(core.CodeInternal, "a mirror needs something to call")
	}
	m := &Mirror{
		call:      call,
		logger:    slog.Default(),
		timeout:   DefaultTimeout,
		draw:      rand.Float64,
		workers:   DefaultWorkers,
		queueSize: DefaultQueue,
	}
	for _, opt := range opts {
		opt(m)
	}
	m.queue = make(chan job, m.queueSize)
	return m, nil
}

// Start launches the workers.
//
// ctx bounds the workers' lifetime, but a mirror already accepted is finished
// rather than abandoned: it has already cost money, and discarding the result
// would mean paying for a measurement nobody records.
func (m *Mirror) Start(ctx context.Context) {
	for range m.workers {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			for j := range m.queue {
				m.run(ctx, j)
			}
		}()
	}
}

// Send mirrors a request to every adapter shadowing the deployment that served
// it. It never blocks and never returns an error.
//
// Both of those are the contract, not an implementation detail: this is called
// on the request path, and a mirror that could block or fail there would be
// able to do exactly what the package exists to prevent.
func (m *Mirror) Send(snap *core.Snapshot, served core.Deployment, req *Request) {
	if snap == nil || req == nil || served.Key.IsAdapter() {
		// A request that was already served by an adapter is not mirrored:
		// shadowing the shadow measures nothing and bills twice.
		return
	}

	for _, adapter := range snap.Shadows(served.Key.BaseModel) {
		if m.draw()*100 >= float64(adapter.ShadowPercent) {
			continue
		}
		select {
		case m.queue <- job{deployment: adapter, request: req}:
		default:
			// Dropped rather than queued. A deep queue turns a slow shadow
			// into stale data, and blocking here would put the shadow on the
			// request path — which is the one thing it must never be.
			m.dropped.Add(1)
			m.logger.Warn("shadow request dropped; the mirror is saturated",
				slog.String("adapter", string(adapter.ID)),
				slog.String("request_id", req.Meta.RequestID))
		}
	}
}

func (m *Mirror) run(ctx context.Context, j job) {
	// Detached from the request's context, which is cancelled the moment the
	// response is written. Inheriting it would mean every mirror was cancelled
	// before it started.
	callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.timeout)
	defer cancel()

	if err := m.call(callCtx, j.deployment, j.request); err != nil {
		// A failing shadow is a finding, not an incident: that an adapter
		// errors under real traffic is exactly what this is for. It is counted
		// and logged, and the caller was never told.
		m.failed.Add(1)
		m.logger.Info("shadow request failed",
			slog.String("adapter", string(j.deployment.ID)),
			slog.String("request_id", j.request.Meta.RequestID),
			slog.String("error", err.Error()))
		return
	}
	m.mirrored.Add(1)
}

// Wait drains in-flight mirrors and stops the workers.
//
// Drained rather than abandoned, for the same reason the guardrail chain is:
// the last requests before a deploy are the ones a rollout decision is about,
// and losing them means a canary looks quiet rather than healthy.
func (m *Mirror) Wait() {
	m.stopOnce.Do(func() { close(m.queue) })
	m.wg.Wait()
}

// Stats reports what has been mirrored.
func (m *Mirror) Stats() Stats {
	return Stats{
		Mirrored: m.mirrored.Load(),
		Dropped:  m.dropped.Load(),
		Failed:   m.failed.Load(),
	}
}
