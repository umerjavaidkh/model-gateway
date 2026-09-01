package contracts

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

// ProviderFactory builds the implementation under test. It is a factory rather
// than a single instance so the suite can assert on a freshly constructed
// adapter where that matters.
type ProviderFactory func(t *testing.T) core.ProviderPort

// SampleCall is a call the implementation is expected to be able to serve. Each
// adapter supplies its own, because a valid body is provider-shaped.
type SampleCall func(t *testing.T) *core.ProviderCall

// RunProviderSuite asserts the behaviour every ProviderPort must have,
// regardless of which upstream it speaks to.
func RunProviderSuite(t *testing.T, newPort ProviderFactory, sample SampleCall) {
	t.Helper()

	t.Run("reports a stable non-empty name", func(t *testing.T) {
		port := newPort(t)
		if port.Name() == "" {
			t.Fatal("Name must be non-empty: it is the key a snapshot binds the port by")
		}
		first, second := port.Name(), port.Name()
		if first != second {
			t.Fatalf("Name must be stable across calls: got %q then %q", first, second)
		}
	})

	t.Run("declares at least one endpoint", func(t *testing.T) {
		// An adapter serving no surface can never be routed to, which is a
		// wiring bug that would otherwise show up as an unexplained 404.
		port := newPort(t)
		if len(port.Endpoints()) == 0 {
			t.Fatal("Endpoints must name at least one API surface")
		}
		for _, e := range port.Endpoints() {
			if e == "" {
				t.Fatal("Endpoints contains an empty surface")
			}
		}
	})

	t.Run("invoke returns a response", func(t *testing.T) {
		port := newPort(t)
		resp, err := port.Invoke(t.Context(), sample(t))
		if err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		if resp == nil {
			t.Fatal("Invoke returned a nil response and a nil error")
		}
	})

	t.Run("stream terminates with io.EOF", func(t *testing.T) {
		port := newPort(t)
		stream, err := port.Stream(t.Context(), sample(t))
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		defer func() { _ = stream.Close() }()

		// A bounded loop, so a non-terminating implementation fails the suite
		// rather than hanging CI.
		const maxChunks = 10_000
		var sawFinal bool
		for i := 0; ; i++ {
			if i == maxChunks {
				t.Fatalf("stream produced %d chunks without reaching io.EOF", maxChunks)
			}
			chunk, err := stream.Next(t.Context())
			if errors.Is(err, io.EOF) {
				// io.EOF must arrive on its own. Returning a chunk alongside it
				// is the classic Go iterator trap: callers write
				// `if err == io.EOF { break }` and drop the value, which loses
				// whatever the final chunk carried.
				if len(chunk.Body) > 0 || chunk.Final || chunk.Usage != (core.TokenUsage{}) {
					t.Fatal("Next returned a chunk together with io.EOF; the terminating chunk must come first, with a nil error")
				}
				break
			}
			if err != nil {
				t.Fatalf("Next: %v", err)
			}
			sawFinal = sawFinal || chunk.Final
		}
		if !sawFinal {
			t.Fatal("no chunk was marked Final: the response leg cannot tell where usage is reported")
		}
	})

	t.Run("close is idempotent", func(t *testing.T) {
		// The response pipeline closes on its own error paths as well as after
		// io.EOF, so a double Close must not panic or error.
		port := newPort(t)
		stream, err := port.Stream(t.Context(), sample(t))
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		if err := stream.Close(); err != nil {
			t.Fatalf("first Close: %v", err)
		}
		if err := stream.Close(); err != nil {
			t.Fatalf("second Close: %v", err)
		}
	})

	t.Run("respects a cancelled context", func(t *testing.T) {
		// The router shares one deadline budget across every attempt, so an
		// adapter that ignores cancellation can outlive the client's timeout.
		port := newPort(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		if _, err := port.Invoke(ctx, sample(t)); !errors.Is(err, context.Canceled) {
			t.Fatalf("Invoke with a cancelled context returned %v, want context.Canceled", err)
		}
	})

	t.Run("does not exceed a short deadline", func(t *testing.T) {
		port := newPort(t)
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()

		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = port.Invoke(ctx, sample(t))
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Invoke outlived its context deadline")
		}
	})
}
