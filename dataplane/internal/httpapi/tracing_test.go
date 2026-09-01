package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/httpapi"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/tracing"
)

// recordSpans installs a recording provider for the duration of a test and
// returns the recorder, so assertions are about spans actually produced rather
// than about the instrumentation calls being present.
func recordSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		_ = provider.Shutdown(t.Context())
	})

	if _, err := tracing.Setup(t.Context(), tracing.Config{}); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	// Setup installs a no-op provider; the recorder must win.
	otel.SetTracerProvider(provider)
	return recorder
}

func TestARequestProducesASpanPerStage(t *testing.T) {
	// Without per-stage spans a slow request shows only a total, and "the
	// gateway was slow" is not a finding.
	recorder := recordSpans(t)
	rec := post(t, newTestServer(t), "gw_acme_secret-1", `{"model":"echo-model","messages":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}

	names := map[string]bool{}
	for _, span := range recorder.Ended() {
		names[span.Name()] = true
	}
	for _, want := range []string{
		"gateway.completion", "gateway.authenticate", "gateway.admit",
		"gateway.route", "gateway.adapt",
	} {
		if !names[want] {
			t.Fatalf("no %q span; got %v", want, names)
		}
	}
}

func TestTheRequestIdIsTheTraceId(t *testing.T) {
	// One correlation string, so a user pastes one value and it finds the
	// trace, the usage record and the log line.
	recordSpans(t)
	rec := post(t, newTestServer(t), "gw_acme_secret-1", `{"model":"echo-model"}`)

	requestID := rec.Header().Get(httpapi.HeaderRequestID)
	if len(requestID) != 32 {
		t.Fatalf("request id %q is not a trace id", requestID)
	}
}

func TestTheGatewayJoinsAnIncomingTrace(t *testing.T) {
	// A gateway that starts its own unrelated trace makes itself a black box in
	// the middle of every caller's distributed trace.
	recordSpans(t)

	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"echo-model"}`))
	req.Header.Set("Authorization", "Bearer gw_acme_secret-1")
	req.Header.Set("traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")

	rec := httptest.NewRecorder()
	newTestServer(t).ServeHTTP(rec, req)

	if got := rec.Header().Get(httpapi.HeaderRequestID); got != traceID {
		t.Fatalf("request id = %q, want the caller's trace id %q", got, traceID)
	}
}

func TestAFailedRequestRecordsTheErrorOnItsSpan(t *testing.T) {
	// The span is what an operator opens when a request failed. An error only
	// in a log is an error they have to go and find.
	recorder := recordSpans(t)
	rec := post(t, newTestServer(t), "gw_acme_secret-1", `{"model":"no-such-model"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}

	var found bool
	for _, span := range recorder.Ended() {
		if span.Name() == "gateway.completion" && len(span.Events()) > 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("the failure was not recorded on the request span")
	}
}
