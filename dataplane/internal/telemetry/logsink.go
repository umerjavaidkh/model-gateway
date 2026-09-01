package telemetry

import (
	"context"
	"log/slog"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

// LogSink writes events as structured log records.
//
// It is the sink that always works: no network, no dependency, nothing to
// misconfigure. In a real deployment the stream sink is the source of truth for
// billing and this is a debugging aid, but having one sink that cannot fail
// means a misconfigured stream never means no record at all.
type LogSink struct {
	logger *slog.Logger
}

// NewLogSink returns a sink writing to the given logger.
func NewLogSink(logger *slog.Logger) *LogSink {
	if logger == nil {
		logger = slog.Default()
	}
	return &LogSink{logger: logger}
}

// Name identifies the sink.
func (*LogSink) Name() string { return "log" }

// Write logs each event.
func (s *LogSink) Write(_ context.Context, events []core.Event) error {
	for _, event := range events {
		switch e := event.(type) {
		case core.UsageEvent:
			s.logger.Info("usage",
				slog.String("request_id", e.RequestID),
				slog.String("tenant", string(e.Tenant)),
				slog.String("tier", e.Tier),
				slog.String("key_id", string(e.KeyID)),
				slog.String("deployment", string(e.Deployment)),
				slog.String("route", e.Route.String()),
				slog.Int64("input_tokens", e.InputTokens),
				slog.Int64("output_tokens", e.OutputTokens),
				slog.Int64("cost_micro_usd", int64(e.CostMicroUSD)),
				slog.Int64("latency_ms", e.LatencyMs),
				slog.Duration("ttfb", e.TimeToFirstByte),
				slog.String("outcome", string(e.Outcome)),
				slog.Uint64("snapshot_version", e.SnapshotVersion))
		case core.AuditEvent:
			s.logger.Info("audit",
				slog.String("request_id", e.RequestID),
				slog.String("tenant", string(e.Tenant)),
				slog.String("actor", e.Actor),
				slog.String("action", e.Action),
				slog.String("resource", e.Resource),
				slog.String("outcome", string(e.Outcome)),
				slog.String("prev_hash", e.PrevHash),
				slog.String("hash", e.Hash))
		}
	}
	return nil
}
