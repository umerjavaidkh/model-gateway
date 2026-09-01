// Command gateway is the data-plane worker.
//
// It loads a snapshot, serves requests from it, and exits cleanly on a
// termination signal. It holds no durable state and writes nothing to disk, so
// it can be killed at any moment.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/adapters/anthropic"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/adapters/echo"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/adapters/openaicompat"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/config"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/gateway"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/httpapi"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/secrets"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/snapshot"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/telemetry"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/tracing"
)

// version identifies this build in traces. Set with -ldflags at release time.
var version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("gateway exited", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

// run holds the wiring, so that main is only signal handling and an exit code.
// Every dependency is constructed here and passed down; nothing below reaches
// for a global.
func run(logger *slog.Logger) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	logger.Info("starting", slog.String("config", cfg.String()))

	// NotifyContext cancels on SIGINT or SIGTERM, which is what a container
	// runtime sends before it kills the pod. Established before the first fetch
	// so a worker stuck waiting on an unreachable control plane still stops.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Installed before anything else that might be traced, and shut down last,
	// so spans buffered at exit are flushed rather than lost.
	shutdownTracing, err := tracing.Setup(ctx, tracing.Config{
		Endpoint:    cfg.OTLPEndpoint,
		Insecure:    cfg.OTLPInsecure,
		SampleRatio: cfg.TraceSampleRatio,
		Version:     version,
	})
	if err != nil {
		return err
	}
	defer func() {
		flush, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTracing(flush); err != nil {
			logger.Error("flushing traces", slog.String("error", err.Error()))
		}
	}()
	if cfg.OTLPEndpoint != "" {
		logger.Info("tracing enabled",
			slog.String("endpoint", cfg.OTLPEndpoint),
			slog.Float64("sample_ratio", cfg.TraceSampleRatio))
	}

	initial, err := bootstrap(ctx, cfg, logger)
	if err != nil {
		return err
	}
	holder, err := snapshot.New(initial, snapshot.OnRetire(func(s *core.Snapshot) {
		logger.Info("snapshot retired", slog.Uint64("version", s.GlobalVersion().Number))
	}))
	if err != nil {
		return err
	}

	// Subscribing is optional: without a control plane the worker serves its
	// bootstrap snapshot and never changes, which is a legitimate deployment
	// for a single-tenant or air-gapped install.
	var subscriber *snapshot.Subscriber
	if cfg.ControlPlaneURL != "" {
		source, err := snapshot.NewHTTPSource(cfg.ControlPlaneURL, cfg.ControlPlaneToken)
		if err != nil {
			return err
		}
		subscriber, err = snapshot.NewSubscriber(source, holder,
			snapshot.WithInterval(cfg.SnapshotInterval),
			snapshot.WithLogger(logger))
		if err != nil {
			return err
		}
		subscriber.Start(ctx)
		defer subscriber.Stop()
	}

	// echo stays registered alongside the real adapter: it is what the demo
	// snapshot and the load harness route to, and it costs nothing to keep.
	providers, err := gateway.NewStaticProviders(echo.New(), openaicompat.New(), anthropic.New())
	if err != nil {
		return err
	}
	credentials, err := secrets.NewResolver(secrets.NewEnvStore())
	if err != nil {
		return err
	}
	registry := prometheus.NewRegistry()
	promSink, err := telemetry.NewPrometheusSink(registry)
	if err != nil {
		return err
	}
	emitter, err := telemetry.NewEmitter(
		[]telemetry.Sink{promSink, telemetry.NewLogSink(logger)},
		telemetry.WithErrorHandler(func(sink string, err error) {
			logger.Error("telemetry sink failed",
				slog.String("sink", sink), slog.String("error", err.Error()))
		}),
	)
	if err != nil {
		return err
	}
	// Closed after the HTTP server drains, so the last in-flight requests'
	// usage events survive a deploy.
	defer func() {
		if err := emitter.Close(); err != nil {
			logger.Error("draining telemetry", slog.String("error", err.Error()))
		}
		if s := emitter.Stats(); s.Dropped > 0 {
			logger.Warn("usage events were dropped", slog.Int64("dropped", s.Dropped))
		}
	}()

	pipeline, err := gateway.New(providers, credentials, cfg.KeyPepper,
		gateway.WithTelemetry(emitter))
	if err != nil {
		return err
	}
	options := httpapi.Options{
		Logger:         logger,
		Metrics:        promhttp.HandlerFor(registry, promhttp.HandlerOpts{}),
		TelemetryStats: emitter.Stats,
	}
	if subscriber != nil {
		// Reported on /readyz because "is this worker still receiving
		// configuration" is the first question asked when a change does not
		// take effect, and it should not need a metrics scrape to answer.
		options.SubscriberStats = subscriber.Stats
	}
	server, err := httpapi.NewServer(holder, pipeline, options)
	if err != nil {
		return err
	}

	httpServer := &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      server.Handler(),
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		IdleTimeout:  cfg.IdleTimeout,
	}

	errs := make(chan error, 1)
	go func() {
		logger.Info("listening",
			slog.String("addr", cfg.ListenAddr),
			slog.Uint64("snapshot_version", initial.GlobalVersion().Number))
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
		close(errs)
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}

	// Drain in-flight requests before exiting. Without this, a rolling deploy
	// returns errors to callers whose requests were mid-flight.
	logger.Info("shutting down", slog.Duration("grace", cfg.ShutdownGrace))
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer cancel()
	return httpServer.Shutdown(shutdownCtx)
}

// bootstrap obtains the snapshot the worker starts with.
//
// A bootstrap file is preferred when present, because it makes startup
// independent of the control plane: a worker restarting during a control-plane
// outage serves last-known configuration rather than failing to start, which is
// what "a control-plane outage freezes configuration" has to mean for a process
// that was not already running.
//
// Without a file, the control plane is the only source and the worker cannot
// start until it answers. That is a real constraint and it is better stated
// than papered over.
func bootstrap(ctx context.Context, cfg config.Config, logger *slog.Logger) (*core.Snapshot, error) {
	source, err := bootstrapSource(cfg)
	if err != nil {
		return nil, err
	}

	fetched, err := source.Fetch(ctx, "")
	if err != nil {
		return nil, err
	}
	logger.Info("bootstrapped",
		slog.String("source", source.Name()),
		slog.Uint64("version", fetched.Snapshot.GlobalVersion().Number),
		slog.String("digest", fetched.Digest))
	return fetched.Snapshot, nil
}

// bootstrapSource picks where the worker's first snapshot comes from.
//
// A bootstrap file wins when present, because it makes startup independent of
// the control plane: a worker restarting during a control-plane outage serves
// last-known configuration rather than failing to start, which is what "a
// control-plane outage freezes configuration" has to mean for a process that
// was not already running.
//
// Without a file the control plane is the only source and the worker cannot
// start until it answers. That is a real constraint, and it is better stated
// than papered over.
func bootstrapSource(cfg config.Config) (snapshot.Source, error) {
	if cfg.SnapshotFile != "" {
		return snapshot.NewFileSource(cfg.SnapshotFile), nil
	}
	return snapshot.NewHTTPSource(cfg.ControlPlaneURL, cfg.ControlPlaneToken)
}
