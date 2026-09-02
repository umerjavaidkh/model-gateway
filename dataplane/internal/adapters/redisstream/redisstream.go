// Package redisstream publishes usage events to a Redis stream.
//
// The plan offers Kafka or Redis Streams, and Redis is already running here for
// rate limits — so this adds no infrastructure. A Kafka sink fills the same
// interface when the operational surface is worth it.
//
// The stream is the source of truth for cost, which is why events are
// serialized with the shared protobuf schema rather than as ad-hoc JSON: the
// producer is Go and the consumer is Python, and a hand-maintained format
// between two languages drifts.
package redisstream

import (
	"context"
	"math"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	pb "github.com/umerjavaidkh/model-gateway/dataplane/internal/wire/gatewayv1"
)

const (
	// DefaultStream is the key the events are appended to.
	DefaultStream = "gateway:usage"

	// DefaultMaxLen bounds the stream so an unread backlog cannot fill Redis.
	//
	// Trimming loses the oldest events, which is the same trade the worker's
	// local buffer makes and for the same reason: when a consumer is behind,
	// the events describing the current incident matter more than the ones
	// from before it. Sized so a consumer can be down for hours at a realistic
	// rate before anything is dropped.
	DefaultMaxLen = 1_000_000

	// PayloadField is the stream entry field the encoded event lives under.
	PayloadField = "event"

	defaultTimeout = 2 * time.Second
)

// Sink publishes events to a Redis stream. Safe for concurrent use.
//
// It implements telemetry.Sink rather than core.EventStream because the
// emitter already provides the batching and the bounded buffer that keep
// publishing off the request path — adding a second mechanism for that would
// mean two places to get backpressure wrong.
type Sink struct {
	client  redis.UniversalClient
	stream  string
	maxLen  int64
	timeout time.Duration
}

// Option configures a Sink.
type Option func(*Sink)

// WithStream sets the stream key.
func WithStream(name string) Option {
	return func(s *Sink) {
		if name != "" {
			s.stream = name
		}
	}
}

// WithMaxLen bounds the stream length.
func WithMaxLen(n int64) Option {
	return func(s *Sink) {
		if n > 0 {
			s.maxLen = n
		}
	}
}

// New returns a sink over an existing client.
func New(client redis.UniversalClient, opts ...Option) (*Sink, error) {
	if client == nil {
		return nil, core.New(core.CodeInternal, "a usage stream needs a redis client")
	}
	s := &Sink{
		client:  client,
		stream:  DefaultStream,
		maxLen:  DefaultMaxLen,
		timeout: defaultTimeout,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// Name identifies the sink.
func (*Sink) Name() string { return "redis-stream" }

// Write appends a batch of usage events.
//
// Audit events are skipped: they are hash-chained records with a different
// retention clock and belong in their own stream, not interleaved with
// measurements that get aggregated and expired.
func (s *Sink) Write(ctx context.Context, events []core.Event) error {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	pipe := s.client.Pipeline()
	queued := 0

	for _, event := range events {
		usage, ok := event.(core.UsageEvent)
		if !ok {
			continue
		}
		payload, err := proto.Marshal(encode(usage))
		if err != nil {
			return core.Wrap(core.CodeInternal, err, "encoding a usage event")
		}

		pipe.XAdd(ctx, &redis.XAddArgs{
			Stream: s.stream,
			// Approximate trimming: exact trimming makes Redis scan, and the
			// point is a bound rather than an exact length.
			MaxLen: s.maxLen,
			Approx: true,
			Values: map[string]any{PayloadField: payload},
		})
		queued++
	}

	if queued == 0 {
		return nil
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return core.Wrap(core.CodeUnavailable, err, "publishing usage events")
	}
	return nil
}

func encode(e core.UsageEvent) *pb.UsageEvent {
	budgets := make([]string, 0, len(e.Budgets))
	for _, id := range e.Budgets {
		budgets = append(budgets, string(id))
	}

	return &pb.UsageEvent{
		RequestId:       e.RequestID,
		TimestampUnixMs: e.Timestamp.UnixMilli(),
		Tenant:          string(e.Tenant),
		KeyId:           string(e.KeyID),
		Tier:            e.Tier,
		Deployment:      string(e.Deployment),
		BaseModel:       e.Route.BaseModel,
		AdapterId:       e.Route.AdapterID,
		Shadow:          e.Shadow,
		Stages:          encodeStages(e.Stages),
		Provider:        e.Provider,
		Stream:          e.Stream,
		Usage: &pb.TokenUsage{
			Input:       e.InputTokens,
			CachedInput: e.CachedInputTokens,
			CacheWrite:  e.CacheWriteTokens,
			Output:      e.OutputTokens,
		},
		CostMicroUsd:      int64(e.CostMicroUSD),
		PriceMicroUsd:     int64(e.PriceMicroUSD),
		LatencyMs:         e.LatencyMs,
		TimeToFirstByteMs: e.TimeToFirstByte.Milliseconds(),
		Outcome:           string(e.Outcome),
		SnapshotVersion:   e.SnapshotVersion,
		BudgetIds:         budgets,
	}
}

// encodeStages narrows stage durations to the wire's milliseconds.
//
// Clamped rather than converted: a negative duration cannot come from a
// monotonic clock, but an unchecked conversion would wrap it into an enormous
// unsigned value and put a stage that took four billion milliseconds on
// somebody's dashboard.
func encodeStages(stages []core.StageTiming) []*pb.StageTiming {
	if len(stages) == 0 {
		return nil
	}
	out := make([]*pb.StageTiming, 0, len(stages))
	for _, stage := range stages {
		ms := stage.Duration.Milliseconds()
		if ms < 0 {
			ms = 0
		}
		if ms > math.MaxUint32 {
			ms = math.MaxUint32
		}
		out = append(out, &pb.StageTiming{
			Name:       stage.Name,
			DurationMs: uint32(ms),
			Outcome:    string(stage.Outcome),
		})
	}
	return out
}
