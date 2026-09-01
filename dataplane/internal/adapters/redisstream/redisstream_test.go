package redisstream_test

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/adapters/redisstream"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	pb "github.com/umerjavaidkh/model-gateway/dataplane/internal/wire/gatewayv1"
)

func newSink(t *testing.T) (*redisstream.Sink, redis.UniversalClient) {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	sink, err := redisstream.New(client)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return sink, client
}

// decodeAll reads the stream back through the client, which is what a consumer
// does — so this exercises the round trip rather than the emulator's internals.
func decodeAll(t *testing.T, client redis.UniversalClient) []*pb.UsageEvent {
	t.Helper()
	entries, err := client.XRange(t.Context(), redisstream.DefaultStream, "-", "+").Result()
	if err != nil {
		t.Fatalf("XRange: %v", err)
	}

	events := make([]*pb.UsageEvent, 0, len(entries))
	for _, entry := range entries {
		raw, ok := entry.Values[redisstream.PayloadField].(string)
		if !ok {
			t.Fatalf("entry %s has no %s field", entry.ID, redisstream.PayloadField)
		}
		var event pb.UsageEvent
		if err := proto.Unmarshal([]byte(raw), &event); err != nil {
			t.Fatalf("the consumer could not decode what the producer wrote: %v", err)
		}
		events = append(events, &event)
	}
	return events
}

func TestUsageEventsArePublished(t *testing.T) {
	sink, client := newSink(t)

	err := sink.Write(t.Context(), []core.Event{core.UsageEvent{
		RequestID:   "req-1",
		Timestamp:   time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC),
		Tenant:      "acme",
		KeyID:       "key-1",
		Route:       core.RoutingKey{BaseModel: "gpt-4o-mini", AdapterID: "triage"},
		Provider:    "openai-compatible",
		InputTokens: 100, CachedInputTokens: 900, CacheWriteTokens: 20, OutputTokens: 50,
		CostMicroUSD: 1500, PriceMicroUSD: 1500,
		Budgets: []core.BudgetID{"monthly", "team-q3"},
	}})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	events := decodeAll(t, client)
	if len(events) != 1 {
		t.Fatalf("published %d events, want 1", len(events))
	}

	got := events[0]
	if got.GetRequestId() != "req-1" || got.GetTenant() != "acme" {
		t.Fatalf("identity lost: %+v", got)
	}
	// Token classes survive, which is the whole reason they exist: the split is
	// unrecoverable once the response is gone.
	if got.GetUsage().GetCachedInput() != 900 || got.GetUsage().GetCacheWrite() != 20 {
		t.Fatalf("token classes lost: %+v", got.GetUsage())
	}
	// The budgets a request must be charged against are captured when it was
	// served, not looked up later.
	if len(got.GetBudgetIds()) != 2 {
		t.Fatalf("budget ids lost: %v", got.GetBudgetIds())
	}
	if got.GetPriceMicroUsd() != 1500 {
		t.Fatalf("price lost: %d", got.GetPriceMicroUsd())
	}
}

func TestAuditEventsAreNotPublishedToTheUsageStream(t *testing.T) {
	// Audit records are hash-chained and have a different retention clock.
	// Interleaving them with measurements that get aggregated and expired
	// would give them the wrong lifetime.
	sink, client := newSink(t)

	if err := sink.Write(t.Context(), []core.Event{core.AuditEvent{Action: "key.rotate"}}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := decodeAll(t, client); len(got) != 0 {
		t.Fatalf("published %d audit events to the usage stream", len(got))
	}
}

func TestAnEmptyBatchDoesNotTouchRedis(t *testing.T) {
	sink, client := newSink(t)
	if err := sink.Write(t.Context(), nil); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := decodeAll(t, client); len(got) != 0 {
		t.Fatalf("an empty batch published %d entries", len(got))
	}
}

func TestNewRejectsANilClient(t *testing.T) {
	if _, err := redisstream.New(nil); err == nil {
		t.Fatal("a sink with no client would drop every event while looking configured")
	}
}
