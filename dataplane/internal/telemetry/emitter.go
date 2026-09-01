// Package telemetry carries events off the request path.
//
// # The one rule
//
// Emitting must never block a request. A telemetry sink that applies
// backpressure to callers converts a monitoring outage into an availability
// outage, which is the worst possible trade: the system fails precisely when
// you have least visibility into why.
//
// So Emit hands the event to a bounded ring and returns. A background worker
// drains it. When the ring is full the oldest event is dropped and a counter
// increments, and that counter is itself a metric — "how much did we lose" is
// the question an operator asks first, and it should not require reading logs
// to answer.
//
// # Why drop oldest rather than newest
//
// A backlog means the sink is behind. The events at the front are the stalest
// and the least likely to still matter for alerting; the ones arriving now
// describe the incident in progress. The reference plan specifies drop-oldest
// and is right, but it also claims zero usage-event loss elsewhere. Those two
// statements do not hold together, and this package implements the honest one:
// best-effort with an exact count of what was lost.
package telemetry

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

// Defaults sized so that a sink stalling for a few seconds at a realistic
// request rate does not lose anything.
const (
	DefaultBufferSize   = 4096
	DefaultBatchSize    = 128
	DefaultFlushTimeout = 5 * time.Second
)

// Sink receives batches of events. Implementations are the swappable part:
// a log writer, a Prometheus collector, a Kafka producer, a SIEM forwarder.
type Sink interface {
	Name() string
	// Write delivers a batch. It may block; the emitter calls it from a
	// background goroutine, never from a request.
	Write(ctx context.Context, events []core.Event) error
}

// Stats is what the emitter knows about itself, exposed for metrics and for
// the readiness endpoint.
//
// The counters are named so that one invariant holds and is worth checking:
//
//	Received - Dropped = events that reached, or will reach, a sink
//
// Naming the first one "emitted" was a mistake caught by a test. With
// drop-oldest, a single Emit call can both accept a new event and discard an
// older one, so a counter meaning "accepted" double-counts against a counter
// meaning "discarded" and no invariant holds between them.
type Stats struct {
	// Received counts every event handed to Emit.
	Received int64
	// Dropped counts events that will never reach a sink, whether the arriving
	// one or an older one evicted to make room. Non-zero means a sink cannot
	// keep up; alert on it.
	Dropped int64
	// Failed counts events a sink rejected.
	Failed int64
	Queued int
}

// Emitter is an asynchronous, bounded fan-out to one or more sinks.
//
// It implements core.TelemetryPort. Safe for concurrent use.
type Emitter struct {
	sinks   []Sink
	buffer  chan core.Event
	onError func(sink string, err error)

	batchSize    int
	flushTimeout time.Duration

	received atomic.Int64
	dropped  atomic.Int64
	failed   atomic.Int64

	stop     chan struct{}
	stopped  sync.WaitGroup
	stopOnce sync.Once
}

// Option configures an Emitter.
type Option func(*Emitter)

// WithBufferSize sets how many events may be queued before drops begin.
func WithBufferSize(n int) Option {
	return func(e *Emitter) {
		if n > 0 {
			e.buffer = make(chan core.Event, n)
		}
	}
}

// WithBatchSize sets how many events are handed to a sink at once.
func WithBatchSize(n int) Option {
	return func(e *Emitter) {
		if n > 0 {
			e.batchSize = n
		}
	}
}

// WithErrorHandler receives sink failures. Without one they are counted and
// otherwise silent, because there is nowhere safe for the telemetry system to
// report its own failures except a counter and the process log.
func WithErrorHandler(fn func(sink string, err error)) Option {
	return func(e *Emitter) { e.onError = fn }
}

// NewEmitter starts the background worker. Close must be called to stop it.
func NewEmitter(sinks []Sink, opts ...Option) (*Emitter, error) {
	if len(sinks) == 0 {
		return nil, core.New(core.CodeInternal, "an emitter needs at least one sink")
	}
	for _, s := range sinks {
		if s == nil {
			return nil, core.New(core.CodeInternal, "a nil sink was supplied")
		}
	}

	e := &Emitter{
		sinks:        sinks,
		buffer:       make(chan core.Event, DefaultBufferSize),
		batchSize:    DefaultBatchSize,
		flushTimeout: DefaultFlushTimeout,
		stop:         make(chan struct{}),
	}
	for _, opt := range opts {
		opt(e)
	}

	e.stopped.Add(1)
	go e.run()
	return e, nil
}

// Name identifies the port implementation.
func (*Emitter) Name() string { return "buffered" }

// Emit queues events. It never blocks and never returns an error: there is no
// useful action a request handler can take when telemetry is behind, and
// giving it one invites handling that makes the outage worse.
func (e *Emitter) Emit(_ context.Context, events ...core.Event) error {
	for _, event := range events {
		e.received.Add(1)

		select {
		case e.buffer <- event:
			continue
		default:
		}

		// Full. Discard the oldest to make room, so the buffer holds the events
		// describing the incident in progress rather than the ones from just
		// before it started.
		select {
		case <-e.buffer:
			e.dropped.Add(1)
		default:
		}
		select {
		case e.buffer <- event:
		default:
			// Another goroutine took the slot. Rather than spin, drop this
			// event: under this much contention one more retry changes nothing
			// except how long the request path is delayed.
			e.dropped.Add(1)
		}
	}
	return nil
}

// Stats reports the emitter's counters.
func (e *Emitter) Stats() Stats {
	return Stats{
		Received: e.received.Load(),
		Dropped:  e.dropped.Load(),
		Failed:   e.failed.Load(),
		Queued:   len(e.buffer),
	}
}

// Close drains what is buffered and stops the worker.
//
// It is called during shutdown, after the HTTP server has drained, so the
// events from the last in-flight requests are not lost to a deploy.
func (e *Emitter) Close() error {
	e.stopOnce.Do(func() {
		close(e.stop)
		e.stopped.Wait()
	})
	return nil
}

func (e *Emitter) run() {
	defer e.stopped.Done()

	batch := make([]core.Event, 0, e.batchSize)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		e.write(batch)
		batch = batch[:0]
	}

	for {
		select {
		case event := <-e.buffer:
			batch = append(batch, event)
			if len(batch) >= e.batchSize {
				flush()
			}
		case <-ticker.C:
			// A partial batch must not wait for a full one. An event that sits
			// in a buffer until traffic happens to arrive is useless for
			// alerting on traffic stopping.
			flush()
		case <-e.stop:
			// Drain whatever is queued before exiting.
			for {
				select {
				case event := <-e.buffer:
					batch = append(batch, event)
					if len(batch) >= e.batchSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

func (e *Emitter) write(batch []core.Event) {
	ctx, cancel := context.WithTimeout(context.Background(), e.flushTimeout)
	defer cancel()

	for _, sink := range e.sinks {
		// One sink failing must not stop the others. A SIEM being unreachable
		// should not also cost us our metrics.
		if err := sink.Write(ctx, batch); err != nil {
			e.failed.Add(int64(len(batch)))
			if e.onError != nil {
				e.onError(sink.Name(), err)
			}
		}
	}
}
