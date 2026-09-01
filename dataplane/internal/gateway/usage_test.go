package gateway_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/adapters/echo"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/gateway"
)

// collector captures emitted events so a test can assert on what accounting
// would have received.
type collector struct {
	mu     sync.Mutex
	events []core.UsageEvent
}

func (*collector) Name() string { return "collector" }

func (c *collector) Emit(_ context.Context, events ...core.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range events {
		if u, ok := e.(core.UsageEvent); ok {
			c.events = append(c.events, u)
		}
	}
	return nil
}

func (c *collector) only(t *testing.T) core.UsageEvent {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.events) != 1 {
		t.Fatalf("got %d usage events, want exactly one per request", len(c.events))
	}
	return c.events[0]
}

func pipelineWithCollector(t *testing.T) (*gateway.Pipeline, *collector) {
	t.Helper()
	c := &collector{}
	providers, err := gateway.NewStaticProviders(echo.New())
	if err != nil {
		t.Fatalf("NewStaticProviders: %v", err)
	}
	p, err := gateway.New(providers, gateway.NoCredentials{}, pepper,
		gateway.WithClock(func() time.Time { return now }),
		gateway.WithTelemetry(c))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p, c
}

func TestASuccessfulRequestEmitsOneUsageEvent(t *testing.T) {
	p, c := pipelineWithCollector(t)
	if _, err := p.Handle(t.Context(), buildSnapshot(t, snapshotOpts{}), request("echo-model", "gw_acme_secret-1")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	got := c.only(t)
	if got.Tenant != "acme" || got.Tier != "enterprise" {
		t.Fatalf("tenant/tier = %q/%q", got.Tenant, got.Tier)
	}
	if got.Deployment != "echo-1" || got.Provider != "echo" {
		t.Fatalf("deployment/provider = %q/%q", got.Deployment, got.Provider)
	}
	if got.InputTokens == 0 || got.OutputTokens == 0 {
		t.Fatalf("usage was not recorded: %+v", got)
	}
	if got.Outcome != "" {
		t.Fatalf("Outcome = %q, want empty for success", got.Outcome)
	}
	if got.SnapshotVersion != 1 {
		t.Fatalf("SnapshotVersion = %d, want the version that served it", got.SnapshotVersion)
	}
}

func TestEveryFailurePathStillEmits(t *testing.T) {
	// A request rejected at admission consumed a slot worth counting, and one
	// that failed after routing may already have burned tokens a provider will
	// bill for. Emitting only on success is how a cost report quietly
	// disagrees with an invoice.
	tests := []struct {
		name  string
		model string
		key   string
		opts  snapshotOpts
		want  core.Code
	}{
		{name: "bad key", model: "echo-model", key: "gw_acme_wrong", want: core.CodeUnauthenticated},
		{name: "unknown model", model: "nope", key: "gw_acme_secret-1", want: core.CodeModelNotFound},
		{
			name: "budget exhausted", model: "echo-model", key: "gw_acme_secret-1",
			opts: snapshotOpts{budgetSpent: 1_000_000}, want: core.CodeBudgetExhausted,
		},
		{
			name: "model not allowed", model: "echo-model", key: "gw_acme_secret-1",
			opts: snapshotOpts{principal: func(p *core.Principal) {
				p.Models = core.ModelAllowlist{Names: []string{"fast"}}
			}},
			want: core.CodeForbidden,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, c := pipelineWithCollector(t)
			_, err := p.Handle(t.Context(), buildSnapshot(t, tc.opts), request(tc.model, tc.key))
			if err == nil {
				t.Fatal("expected the request to fail")
			}
			if got := c.only(t); got.Outcome != tc.want {
				t.Fatalf("Outcome = %q, want %q", got.Outcome, tc.want)
			}
		})
	}
}

func TestAnUnauthenticatedRequestEmitsWithoutATenant(t *testing.T) {
	// There is no principal to attribute it to, but the request still happened
	// and a spike in these is exactly what an operator wants to see.
	p, c := pipelineWithCollector(t)
	_, _ = p.Handle(t.Context(), buildSnapshot(t, snapshotOpts{}), request("echo-model", "gw_acme_wrong"))

	got := c.only(t)
	if got.Tenant != "" {
		t.Fatalf("Tenant = %q, want empty", got.Tenant)
	}
	if got.Tier != "unknown" {
		t.Fatalf("Tier = %q, want the unknown-tenant fallback", got.Tier)
	}
}

func TestCostIsComputedFromTheServingSnapshotsPrice(t *testing.T) {
	// Cost is computed here, not by a consumer, because this is the only place
	// holding both the token counts and the price at the version that served
	// the request. Looking the price up later silently re-bills history when a
	// provider changes rates.
	priced := snapshotOpts{deployments: []core.Deployment{{
		ID: "echo-1", Key: routeEcho, Provider: "echo",
		TrustTier: core.TrustInternal, Weight: 100,
		Cost: core.Cost{InputPer1K: 2000, OutputPer1K: 6000},
	}}}

	p, c := pipelineWithCollector(t)
	if _, err := p.Handle(t.Context(), buildSnapshot(t, priced), request("echo-model", "gw_acme_secret-1")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	got := c.only(t)
	want := core.Cost{InputPer1K: 2000, OutputPer1K: 6000}.For(core.TokenUsage{
		Input: got.InputTokens, Output: got.OutputTokens,
	})
	if got.CostMicroUSD != want {
		t.Fatalf("CostMicroUSD = %d, want %d", got.CostMicroUSD, want)
	}
	if got.CostMicroUSD == 0 {
		t.Fatal("a priced deployment produced no cost")
	}
}

func TestStreamingEmitsExactlyOnceAfterTheStreamDrains(t *testing.T) {
	// Token counts only exist once the stream has been read, so the event
	// cannot be emitted when the stream starts.
	p, c := pipelineWithCollector(t)

	result, err := p.HandleStream(t.Context(), buildSnapshot(t, snapshotOpts{}), request("echo-model", "gw_acme_secret-1"))
	if err != nil {
		t.Fatalf("HandleStream: %v", err)
	}
	c.mu.Lock()
	emittedEarly := len(c.events)
	c.mu.Unlock()
	if emittedEarly != 0 {
		t.Fatal("the event was emitted before the stream was drained, so usage cannot be in it")
	}

	var usage core.TokenUsage
	for {
		chunk, err := result.Chunks.Next(t.Context())
		if chunk.Usage.Input != 0 {
			usage = chunk.Usage
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
	}
	_ = result.Chunks.Close()
	result.Finish(usage, 12*time.Millisecond, nil)
	// Finish is deferred in the transport and may run more than once on an
	// awkward exit path; it must still produce one event.
	result.Finish(usage, 12*time.Millisecond, nil)

	got := c.only(t)
	if !got.Stream {
		t.Fatal("the event must record that this was a streamed request")
	}
	if got.InputTokens == 0 {
		t.Fatalf("usage was not carried into the event: %+v", got)
	}
	if got.TimeToFirstByte != 12*time.Millisecond {
		t.Fatalf("TimeToFirstByte = %v", got.TimeToFirstByte)
	}
}

func TestStreamingFailureBeforeTheStreamStartsEmitsOnce(t *testing.T) {
	p, c := pipelineWithCollector(t)

	result, err := p.HandleStream(t.Context(), buildSnapshot(t, snapshotOpts{}), request("nope", "gw_acme_secret-1"))
	if err == nil {
		t.Fatal("expected the request to fail")
	}
	// The transport defers Finish unconditionally, so it runs even on a result
	// that never started streaming. That must not produce a second event.
	result.Finish(core.TokenUsage{}, 0, err)

	if got := c.only(t); got.Outcome != core.CodeModelNotFound {
		t.Fatalf("Outcome = %q", got.Outcome)
	}
}
