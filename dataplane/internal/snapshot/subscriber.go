package snapshot

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

// Defaults chosen against the plan's 30-second propagation ceiling: a 15-second
// interval plus jitter lands a change on every worker inside it, with room for
// one failed attempt.
const (
	DefaultInterval    = 15 * time.Second
	DefaultMinBackoff  = 1 * time.Second
	DefaultMaxBackoff  = 2 * time.Minute
	defaultJitterRatio = 0.2
)

// Subscriber keeps a Holder current by polling a Source.
//
// # The property this exists to protect
//
// A control-plane outage must degrade to "configuration is frozen", not
// "traffic stops". So every failure here is logged, counted and retried; none
// of them touch the snapshot the worker is already serving, and none of them
// stop the loop. The only thing an outage costs is the freshness of the
// configuration.
//
// That is also why a stale or rejected snapshot is not fatal. A watch stream
// redelivers out of order after a reconnect, and the Holder rejects a version
// that moves backwards — the right response is to log it and poll again, not to
// stop and leave the worker serving whatever it had when it gave up.
type Subscriber struct {
	source   Source
	holder   *Holder
	logger   *slog.Logger
	interval time.Duration

	minBackoff time.Duration
	maxBackoff time.Duration
	// jitter is injected so a test can make the schedule deterministic. In
	// production it spreads the fleet: every worker polling on the same second
	// is a thundering herd on the control plane at exactly the moment it is
	// already struggling.
	jitter func(time.Duration) time.Duration

	digest atomic.Pointer[string]

	applied   atomic.Int64
	unchanged atomic.Int64
	failed    atomic.Int64
	rejected  atomic.Int64
	lastError atomic.Pointer[string]

	stop     chan struct{}
	done     sync.WaitGroup
	stopOnce sync.Once
}

// SubscriberOption configures a Subscriber.
type SubscriberOption func(*Subscriber)

// WithInterval sets how often the source is polled when healthy.
func WithInterval(d time.Duration) SubscriberOption {
	return func(s *Subscriber) {
		if d > 0 {
			s.interval = d
		}
	}
}

// WithBackoff sets the retry bounds used after a failure.
func WithBackoff(minBackoff, maxBackoff time.Duration) SubscriberOption {
	return func(s *Subscriber) {
		if minBackoff > 0 {
			s.minBackoff = minBackoff
		}
		if maxBackoff > 0 {
			s.maxBackoff = maxBackoff
		}
	}
}

// WithJitter replaces the jitter function. Pass a function returning its input
// to make polling deterministic in a test.
func WithJitter(fn func(time.Duration) time.Duration) SubscriberOption {
	return func(s *Subscriber) {
		if fn != nil {
			s.jitter = fn
		}
	}
}

// WithLogger sets where the subscriber reports.
func WithLogger(l *slog.Logger) SubscriberOption {
	return func(s *Subscriber) {
		if l != nil {
			s.logger = l
		}
	}
}

// NewSubscriber returns a subscriber. Call Start to begin polling.
func NewSubscriber(source Source, holder *Holder, opts ...SubscriberOption) (*Subscriber, error) {
	if source == nil || holder == nil {
		return nil, core.New(core.CodeInternal, "a subscriber needs a source and a holder")
	}

	s := &Subscriber{
		source:     source,
		holder:     holder,
		logger:     slog.Default(),
		interval:   DefaultInterval,
		minBackoff: DefaultMinBackoff,
		maxBackoff: DefaultMaxBackoff,
		jitter:     jitterUpTo(defaultJitterRatio),
		stop:       make(chan struct{}),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// SubscriberStats is what the subscriber knows about itself.
//
// Exposed on the readiness endpoint, because "is this worker still receiving
// configuration" is a question that has to be answerable without a metrics
// scrape — it is the first thing asked when a change does not take effect.
type SubscriberStats struct {
	Digest string
	// Applied counts snapshots installed. Unchanged counts polls that found
	// nothing new, which is the healthy steady state.
	Applied   int64
	Unchanged int64
	Failed    int64
	// Rejected counts snapshots the holder refused, which almost always means
	// a version moved backwards. Non-zero is worth an alert: it means the
	// control plane is serving something older than the worker holds.
	Rejected  int64
	LastError string
}

// Stats reports the subscriber's counters.
func (s *Subscriber) Stats() SubscriberStats {
	stats := SubscriberStats{
		Applied:   s.applied.Load(),
		Unchanged: s.unchanged.Load(),
		Failed:    s.failed.Load(),
		Rejected:  s.rejected.Load(),
	}
	if d := s.digest.Load(); d != nil {
		stats.Digest = *d
	}
	if e := s.lastError.Load(); e != nil {
		stats.LastError = *e
	}
	return stats
}

// Refresh polls once and applies whatever it finds.
//
// Exported so that startup can take the first snapshot synchronously — a worker
// should not report ready while still serving nothing — and so a test can drive
// the loop a step at a time.
func (s *Subscriber) Refresh(ctx context.Context) error {
	known := ""
	if d := s.digest.Load(); d != nil {
		known = *d
	}

	fetched, err := s.source.Fetch(ctx, known)
	if err != nil {
		s.failed.Add(1)
		s.recordError(err)
		return err
	}

	if fetched.Unchanged || fetched.Snapshot == nil {
		s.unchanged.Add(1)
		return nil
	}

	if err := s.holder.Swap(fetched.Snapshot); err != nil {
		// Not a failure of the subscriber: the holder rejecting a version that
		// moves backwards is it doing its job. Counted separately so an
		// operator can tell "cannot reach the control plane" from "the control
		// plane is serving something stale".
		s.rejected.Add(1)
		s.recordError(err)
		s.logger.Warn("snapshot rejected",
			slog.String("source", s.source.Name()),
			slog.String("digest", fetched.Digest),
			slog.String("error", err.Error()))
		return err
	}

	digest := fetched.Digest
	s.digest.Store(&digest)
	s.applied.Add(1)
	s.lastError.Store(nil)
	s.logger.Info("snapshot applied",
		slog.String("source", s.source.Name()),
		slog.Uint64("version", fetched.Snapshot.GlobalVersion().Number),
		slog.String("digest", digest))
	return nil
}

// Start begins polling in the background. Stop ends it.
func (s *Subscriber) Start(ctx context.Context) {
	s.done.Add(1)
	go s.run(ctx)
}

// Stop ends polling and waits for the loop to exit.
func (s *Subscriber) Stop() {
	s.stopOnce.Do(func() {
		close(s.stop)
		s.done.Wait()
	})
}

func (s *Subscriber) run(ctx context.Context) {
	defer s.done.Done()

	backoff := s.minBackoff
	for {
		wait := s.jitter(s.interval)

		timer := time.NewTimer(wait)
		select {
		case <-s.stop:
			timer.Stop()
			return
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		if err := s.Refresh(ctx); err != nil {
			// Back off, but keep going. Giving up would leave the worker
			// serving whatever it held when it stopped, with nothing to say so.
			s.logger.Warn("snapshot refresh failed",
				slog.String("source", s.source.Name()),
				slog.Duration("retry_in", backoff),
				slog.String("error", err.Error()))

			timer := time.NewTimer(s.jitter(backoff))
			select {
			case <-s.stop:
				timer.Stop()
				return
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}

			backoff = min(backoff*2, s.maxBackoff)
			continue
		}
		backoff = s.minBackoff
	}
}

func (s *Subscriber) recordError(err error) {
	message := err.Error()
	s.lastError.Store(&message)
}

// jitterUpTo spreads a delay by up to ratio, so a fleet restarted together does
// not converge on one polling instant and stay there.
func jitterUpTo(ratio float64) func(time.Duration) time.Duration {
	return func(d time.Duration) time.Duration {
		if d <= 0 {
			return d
		}
		spread := float64(d) * ratio
		return d + time.Duration(rand.Float64()*spread) //nolint:gosec // scheduling spread, not a secret
	}
}
