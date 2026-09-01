package tracing_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/tracing"
)

func TestSetupWithNoEndpointStillPropagates(t *testing.T) {
	// A worker with tracing disabled must still pass an incoming traceparent
	// through to the provider, or it silently breaks the trace of every caller
	// that does have tracing on.
	shutdown, err := tracing.Setup(t.Context(), tracing.Config{})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	carrier := propagation.MapCarrier{
		"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
	}
	ctx := otel.GetTextMapPropagator().Extract(t.Context(), carrier)

	if got := tracing.TraceIDFrom(ctx); got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("TraceIDFrom = %q, want the caller's trace id", got)
	}
}

func TestSetupWithNoEndpointIsSafeToShutDown(t *testing.T) {
	// Shutdown runs on every exit path, including one where tracing was never
	// configured. It must not be a special case the caller has to guard.
	shutdown, err := tracing.Setup(t.Context(), tracing.Config{})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := shutdown(t.Context()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestTraceIDIsEmptyWithoutASpan(t *testing.T) {
	// The caller falls back to a random request id, so this has to be
	// distinguishable rather than a zero-filled id that looks real.
	if got := tracing.TraceIDFrom(context.Background()); got != "" {
		t.Fatalf("TraceIDFrom = %q, want empty", got)
	}
}
