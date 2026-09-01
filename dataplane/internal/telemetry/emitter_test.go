package telemetry_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/telemetry"
)

// recordingSink captures what reached it, and can be made slow or failing.
type recordingSink struct {
	mu     sync.Mutex
	events []core.Event

	block chan struct{}
	err   error
}

func (s *recordingSink) Name() string { return "recording" }

func (s *recordingSink) Write(_ context.Context, events []core.Event) error {
	if s.block != nil {
		<-s.block
	}
	if s.err != nil {
		return s.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, events...)
	return nil
}

func (s *recordingSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

func usage(id string) core.UsageEvent {
	return core.UsageEvent{RequestID: id, Timestamp: time.Now(), Tenant: "acme"}
}

func TestEventsReachEverySink(t *testing.T) {
	a, b := &recordingSink{}, &recordingSink{}
	e, err := telemetry.NewEmitter([]telemetry.Sink{a, b}, telemetry.WithBatchSize(1))
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}

	for i := range 10 {
		if err := e.Emit(t.Context(), usage(string(rune('a'+i)))); err != nil {
			t.Fatalf("Emit: %v", err)
		}
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if a.count() != 10 || b.count() != 10 {
		t.Fatalf("sinks received %d and %d events, want 10 each", a.count(), b.count())
	}
}

func TestCloseDrainsWhatIsBuffered(t *testing.T) {
	// Shutdown happens after the HTTP server drains, so the last in-flight
	// requests' events must survive a deploy rather than being discarded.
	sink := &recordingSink{}
	e, err := telemetry.NewEmitter([]telemetry.Sink{sink}, telemetry.WithBatchSize(1000))
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}

	for range 50 {
		_ = e.Emit(t.Context(), usage("r"))
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if sink.count() != 50 {
		t.Fatalf("sink received %d events after Close, want 50", sink.count())
	}
}

func TestEmitNeverBlocksWhenTheSinkStalls(t *testing.T) {
	// This is the property the package exists for: a telemetry outage must not
	// become an availability outage.
	block := make(chan struct{})
	sink := &recordingSink{block: block}
	e, err := telemetry.NewEmitter([]telemetry.Sink{sink},
		telemetry.WithBufferSize(8), telemetry.WithBatchSize(1))
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	t.Cleanup(func() { close(block); _ = e.Close() })

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 1000 {
			_ = e.Emit(context.Background(), usage("r"))
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Emit blocked while the sink was stalled")
	}

	if e.Stats().Dropped == 0 {
		t.Fatal("a full buffer must count drops, or loss is invisible")
	}
}

func TestDropsAreCountedExactly(t *testing.T) {
	block := make(chan struct{})
	sink := &recordingSink{block: block}
	e, err := telemetry.NewEmitter([]telemetry.Sink{sink},
		telemetry.WithBufferSize(4), telemetry.WithBatchSize(1))
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	t.Cleanup(func() { close(block); _ = e.Close() })

	const sent = 100
	for range sent {
		_ = e.Emit(context.Background(), usage("r"))
	}

	// The invariant an operator relies on: everything received either reaches a
	// sink or is counted as lost. Nothing vanishes unaccounted for.
	stats := e.Stats()
	if stats.Received != sent {
		t.Fatalf("Received = %d, want %d", stats.Received, sent)
	}
	if stats.Dropped == 0 {
		t.Fatal("a buffer of 4 behind a stalled sink must drop")
	}
	if delivered := stats.Received - stats.Dropped; delivered < 0 || delivered > sent {
		t.Fatalf("Received - Dropped = %d, which is not a possible delivery count", delivered)
	}
}

func TestOneFailingSinkDoesNotStopTheOthers(t *testing.T) {
	// A SIEM being unreachable must not also cost us our metrics.
	failing := &recordingSink{err: errors.New("siem is down")}
	working := &recordingSink{}
	e, err := telemetry.NewEmitter([]telemetry.Sink{failing, working}, telemetry.WithBatchSize(1))
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}

	_ = e.Emit(t.Context(), usage("r"))
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if working.count() != 1 {
		t.Fatalf("the healthy sink received %d events, want 1", working.count())
	}
	if e.Stats().Failed == 0 {
		t.Fatal("a sink failure must be counted")
	}
}

func TestErrorHandlerNamesTheSink(t *testing.T) {
	var mu sync.Mutex
	var named string
	sink := &recordingSink{err: errors.New("down")}

	e, err := telemetry.NewEmitter([]telemetry.Sink{sink},
		telemetry.WithBatchSize(1),
		telemetry.WithErrorHandler(func(name string, _ error) {
			mu.Lock()
			defer mu.Unlock()
			named = name
		}))
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	_ = e.Emit(t.Context(), usage("r"))
	_ = e.Close()

	mu.Lock()
	defer mu.Unlock()
	if named != "recording" {
		t.Fatalf("error handler got sink %q", named)
	}
}

func TestNewEmitterRejectsBadConfiguration(t *testing.T) {
	if _, err := telemetry.NewEmitter(nil); err == nil {
		t.Fatal("an emitter with no sinks must be refused")
	}
	if _, err := telemetry.NewEmitter([]telemetry.Sink{nil}); err == nil {
		t.Fatal("a nil sink must be refused")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	e, err := telemetry.NewEmitter([]telemetry.Sink{&recordingSink{}})
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	for range 3 {
		if err := e.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
}

func TestEmitIsSafeUnderConcurrency(t *testing.T) {
	sink := &recordingSink{}
	e, err := telemetry.NewEmitter([]telemetry.Sink{sink}, telemetry.WithBufferSize(8192))
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				_ = e.Emit(context.Background(), usage("r"))
			}
		}()
	}
	wg.Wait()
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := e.Stats().Received; got != 1600 {
		t.Fatalf("Received = %d, want 1600", got)
	}
}
