package router

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

// Prober defaults.
const (
	// DefaultProbeInterval is how often idle deployments are checked. Long
	// enough that probing a large catalog is negligible traffic, short enough
	// that a recovered deployment is usable again within a minute.
	DefaultProbeInterval = 30 * time.Second
	// DefaultProbeTimeout bounds one probe. A deployment that cannot answer a
	// listing in this long is not one to route a request to.
	DefaultProbeTimeout = 3 * time.Second
	// idleBeforeProbing is how long without traffic makes a deployment worth
	// probing. Deployments that are serving are already being measured, and
	// probing them adds load for information already held.
	idleBeforeProbing = 60 * time.Second
)

// SnapshotSource supplies the deployments to probe.
//
// A function rather than a snapshot, because the catalog changes underneath the
// prober and holding one would mean probing deployments that no longer exist
// while missing the ones that do.
type SnapshotSource func() *core.Snapshot

// CredentialSource resolves a deployment's credential for probing.
type CredentialSource func(ctx context.Context, ref string) (core.Credential, error)

// Prober keeps health scores meaningful for deployments that receive no
// traffic.
//
// Passive health can only measure what is being used, and the deployments
// nobody is sure about are exactly the ones not being used: a newly added one,
// a failover tier that has never been needed, one whose breaker just opened.
// Without probing they are either always tried or never tried, and both are
// wrong — the first makes every request pay to discover an outage, the second
// makes recovery invisible.
type Prober struct {
	router      *Router
	snapshots   SnapshotSource
	credentials CredentialSource
	logger      *slog.Logger
	now         func() time.Time

	interval time.Duration
	timeout  time.Duration

	mu       sync.Mutex
	lastSeen map[core.DeploymentID]time.Time

	stop     chan struct{}
	done     sync.WaitGroup
	stopOnce sync.Once
}

// ProberOption configures a Prober.
type ProberOption func(*Prober)

// WithProbeInterval sets how often idle deployments are checked.
func WithProbeInterval(d time.Duration) ProberOption {
	return func(p *Prober) {
		if d > 0 {
			p.interval = d
		}
	}
}

// WithProbeTimeout bounds a single probe.
func WithProbeTimeout(d time.Duration) ProberOption {
	return func(p *Prober) {
		if d > 0 {
			p.timeout = d
		}
	}
}

// WithProberLogger sets where the prober reports.
func WithProberLogger(l *slog.Logger) ProberOption {
	return func(p *Prober) {
		if l != nil {
			p.logger = l
		}
	}
}

// WithProberClock replaces the time source, for tests.
func WithProberClock(now func() time.Time) ProberOption {
	return func(p *Prober) {
		if now != nil {
			p.now = now
		}
	}
}

// NewProber returns a prober. Call Start to begin.
func NewProber(
	r *Router, snapshots SnapshotSource, credentials CredentialSource, opts ...ProberOption,
) (*Prober, error) {
	if r == nil || snapshots == nil || credentials == nil {
		return nil, core.New(core.CodeInternal,
			"a prober needs a router, a snapshot source and a credential source")
	}

	p := &Prober{
		router:      r,
		snapshots:   snapshots,
		credentials: credentials,
		logger:      slog.Default(),
		now:         time.Now,
		interval:    DefaultProbeInterval,
		timeout:     DefaultProbeTimeout,
		lastSeen:    map[core.DeploymentID]time.Time{},
		stop:        make(chan struct{}),
	}
	for _, opt := range opts {
		opt(p)
	}

	// The prober registers itself rather than the caller wiring both
	// directions. The dependency is genuinely circular — the router reports
	// traffic to the prober, the prober reads health through the router — and
	// resolving it here means a caller cannot construct the pair half-wired.
	r.observe(p)
	return p, nil
}

// Saw records that a deployment served real traffic, so it is not probed.
//
// Called by the router on every attempt. A deployment that is being used is
// already being measured, and probing it adds load to learn what is known.
func (p *Prober) Saw(id core.DeploymentID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastSeen[id] = p.now()
}

// Start begins probing in the background.
func (p *Prober) Start(ctx context.Context) {
	p.done.Add(1)
	go p.run(ctx)
}

// Stop ends probing and waits for the loop to exit.
func (p *Prober) Stop() {
	p.stopOnce.Do(func() {
		close(p.stop)
		p.done.Wait()
	})
}

// RunOnce probes every idle deployment once. Exported so a test can drive the
// loop a step at a time rather than waiting on a ticker.
func (p *Prober) RunOnce(ctx context.Context) int {
	snap := p.snapshots()
	if snap == nil {
		return 0
	}

	probed := 0
	for _, id := range p.idle(snap) {
		deployment, ok := snap.Deployment(id)
		if !ok {
			continue
		}
		p.probe(ctx, deployment)
		probed++
	}
	return probed
}

func (p *Prober) run(ctx context.Context) {
	defer p.done.Done()

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-p.stop:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.RunOnce(ctx)
		}
	}
}

// idle returns the deployments worth probing: those that have not served
// traffic recently.
func (p *Prober) idle(snap *core.Snapshot) []core.DeploymentID {
	p.mu.Lock()
	defer p.mu.Unlock()

	var ids []core.DeploymentID
	for _, id := range snap.DeploymentIDs() {
		if seen, ok := p.lastSeen[id]; ok && p.now().Sub(seen) < idleBeforeProbing {
			continue
		}
		ids = append(ids, id)
	}

	// Shuffled so a fleet of workers does not probe the catalog in the same
	// order and arrive at the same deployment together.
	//nolint:gosec // probe ordering, not a secret
	rand.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
	return ids
}

func (p *Prober) probe(ctx context.Context, d core.Deployment) {
	provider, ok := p.router.providers.Provider(d.Provider)
	if !ok {
		return
	}

	credential, err := p.credentials(ctx, d.CredentialRef)
	if err != nil {
		// A credential that cannot be resolved is a real problem, but it is not
		// this deployment being unhealthy — and scoring it down would move
		// traffic away for a reason the deployment cannot fix.
		p.logger.Warn("skipping probe; credential unavailable",
			slog.String("deployment", string(d.ID)), slog.String("error", err.Error()))
		return
	}

	probeCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	started := p.now()
	err = provider.Probe(probeCtx, d, credential)
	latency := p.now().Sub(started)

	p.router.healthFor(d.ID).Observe(err != nil, latency)
	if err != nil {
		p.logger.Debug("probe failed",
			slog.String("deployment", string(d.ID)), slog.String("error", err.Error()))
		return
	}

	// A successful probe is what lets a recovered deployment come back without
	// a real request having to discover it. The breaker is told directly,
	// because health alone would not reopen it.
	p.router.breakerFor(d.ID).Succeed()
}
