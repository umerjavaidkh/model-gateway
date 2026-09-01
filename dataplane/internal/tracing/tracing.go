// Package tracing configures OpenTelemetry for the worker.
//
// OTel is adopted rather than wrapped. The reference plan is explicit that
// observability is a standard to adopt and never to build, and hiding a
// tracing SDK behind a homemade interface costs the thing tracing is for: the
// context propagation, the semantic conventions, and the ecosystem of
// collectors that already understand them.
//
// internal/core still imports none of it — CI enforces that — so the domain
// model stays free of the dependency even though the request path uses it.
package tracing

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

// ServiceName is what this worker calls itself in a trace.
const ServiceName = "model-gateway"

// InstrumentationName scopes the spans this codebase creates.
const InstrumentationName = "github.com/umerjavaidkh/model-gateway/dataplane"

// DefaultSampleRatio is the fraction of traces started here that are recorded.
//
// Not 1.0: a gateway at any real request rate would drown a collector and its
// storage. Not tiny either, because a rare error is worth catching. Head
// sampling cannot know which traces will turn out interesting — that is what a
// collector's tail sampling is for, and this ratio is what feeds it.
const DefaultSampleRatio = 0.1

// Config describes how to export traces.
type Config struct {
	// Endpoint is the OTLP gRPC collector address. Empty disables tracing
	// entirely, which is the correct default: a worker should not fail or
	// stall because no collector was configured.
	Endpoint string
	// Insecure sends without TLS, for a collector on the same host or pod.
	Insecure bool
	// SampleRatio is the fraction of root traces recorded.
	SampleRatio float64
	Version     string
}

// Shutdown flushes and stops the exporter. It is safe to call when tracing was
// never enabled.
type Shutdown func(context.Context) error

// Setup installs a global tracer provider and propagator.
//
// With no endpoint it installs a no-op provider and returns. That keeps every
// call site unconditional — instrumented code does not have to ask whether
// tracing is on, which is how instrumentation ends up wrapped in branches that
// drift out of step.
func Setup(ctx context.Context, cfg Config) (Shutdown, error) {
	// The propagator is installed either way. A worker with tracing disabled
	// must still pass an incoming traceparent through to the provider, or it
	// silently breaks the trace of every caller that does have tracing on.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	if cfg.Endpoint == "" {
		otel.SetTracerProvider(noop.NewTracerProvider())
		return func(context.Context) error { return nil }, nil
	}

	options := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.Endpoint)}
	if cfg.Insecure {
		options = append(options, otlptracegrpc.WithInsecure())
	}
	exporter, err := otlptracegrpc.New(ctx, options...)
	if err != nil {
		return nil, core.Wrap(core.CodeUnavailable, err, "creating the OTLP exporter")
	}

	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(ServiceName),
		semconv.ServiceVersion(cfg.Version),
	))
	if err != nil {
		return nil, core.Wrap(core.CodeInternal, err, "building the trace resource")
	}

	ratio := cfg.SampleRatio
	if ratio <= 0 || ratio > 1 {
		ratio = DefaultSampleRatio
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter, sdktrace.WithBatchTimeout(5*time.Second)),
		sdktrace.WithResource(res),
		// Parent-based: if a caller decided to sample this trace, honour it.
		// Overriding a caller's decision produces traces with holes in them,
		// which is worse than not tracing the request at all.
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))),
	)
	otel.SetTracerProvider(provider)

	return provider.Shutdown, nil
}

// Tracer returns the tracer this codebase's spans belong to.
func Tracer() trace.Tracer { return otel.Tracer(InstrumentationName) }

// TraceIDFrom returns the trace id in a context, or empty when there is none.
//
// The gateway uses it as the request id, so that the string returned to a
// caller in a header is the same string a trace is filed under. Two different
// correlation ids for one request is a support burden with no benefit.
func TraceIDFrom(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.HasTraceID() {
		return ""
	}
	return sc.TraceID().String()
}

// Attr builds a string attribute. A thin helper so call sites do not each
// import the attribute package for one line.
func Attr(key, value string) attribute.KeyValue { return attribute.String(key, value) }
