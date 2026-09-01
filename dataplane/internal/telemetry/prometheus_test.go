package telemetry_test

import (
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/telemetry"
)

func newSink(t *testing.T) (*telemetry.PrometheusSink, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	sink, err := telemetry.NewPrometheusSink(reg)
	if err != nil {
		t.Fatalf("NewPrometheusSink: %v", err)
	}
	return sink, reg
}

// gather renders the registry as sorted "name{labels} value" lines, which makes
// an assertion read like the metric it is about. Prometheus returns labels in
// alphabetical order, so the expectations below are written that way too.
func gather(t *testing.T, reg *prometheus.Registry) string {
	t.Helper()

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	var lines []string
	for _, f := range families {
		for _, m := range f.GetMetric() {
			labels := make([]string, 0, len(m.GetLabel()))
			for _, l := range m.GetLabel() {
				labels = append(labels, l.GetName()+`="`+l.GetValue()+`"`)
			}
			var value float64
			switch {
			case m.GetCounter() != nil:
				value = m.GetCounter().GetValue()
			case m.GetHistogram() != nil:
				value = float64(m.GetHistogram().GetSampleCount())
			}
			lines = append(lines, f.GetName()+"{"+strings.Join(labels, ",")+"} "+
				strconv.FormatFloat(value, 'f', -1, 64))
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func write(t *testing.T, sink *telemetry.PrometheusSink, events ...core.Event) {
	t.Helper()
	if err := sink.Write(t.Context(), events); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

func TestUsageEventsBecomeMetrics(t *testing.T) {
	sink, reg := newSink(t)
	write(t, sink, core.UsageEvent{
		Tenant: "acme", Tier: "enterprise", Provider: "openai-compatible",
		InputTokens: 100, OutputTokens: 40, CostMicroUSD: 1500,
		LatencyMs: 250, TimeToFirstByte: 80 * time.Millisecond,
	})

	dump := gather(t, reg)
	for _, want := range []string{
		`gateway_requests_total{outcome="ok",provider="openai-compatible",stream="false",tier="enterprise"} 1`,
		`gateway_tokens_total{direction="input",provider="openai-compatible",tier="enterprise"} 100`,
		`gateway_tokens_total{direction="output",provider="openai-compatible",tier="enterprise"} 40`,
		`gateway_cost_micro_usd_total{provider="openai-compatible",tier="enterprise"} 1500`,
		`gateway_time_to_first_byte_seconds{provider="openai-compatible",tier="enterprise"} 1`,
	} {
		if !strings.Contains(dump, want) {
			t.Fatalf("missing series:\n  %s\ngot:\n%s", want, dump)
		}
	}
}

func TestTenantIdIsNeverALabel(t *testing.T) {
	// A per-tenant label is unbounded cardinality: it grows with the customer
	// list and never shrinks, because Prometheus keeps series long after they
	// stop receiving samples. This is the single most common way a gateway
	// kills its own monitoring, so it gets a test rather than a comment.
	sink, reg := newSink(t)
	for _, tenant := range []core.TenantID{"acme", "globex", "initech"} {
		write(t, sink, core.UsageEvent{Tenant: tenant, Tier: "enterprise", Provider: "echo"})
	}

	dump := gather(t, reg)
	for _, tenant := range []string{"acme", "globex", "initech"} {
		if strings.Contains(dump, tenant) {
			t.Fatalf("tenant id %q leaked into a metric label:\n%s", tenant, dump)
		}
	}
	// All three tenants collapse onto one series, which is the point.
	if !strings.Contains(dump, `gateway_requests_total{outcome="ok",provider="echo",stream="false",tier="enterprise"} 3`) {
		t.Fatalf("expected three requests on one series:\n%s", dump)
	}
}

func TestFailedRequestsAreCountedWithTheirOutcome(t *testing.T) {
	// A request rejected at admission still consumed a slot, and the reason is
	// what an operator alerts on.
	sink, reg := newSink(t)
	write(t, sink,
		core.UsageEvent{Tier: "free", Outcome: core.CodeBudgetExhausted},
		core.UsageEvent{Tier: "free", Outcome: core.CodeRateLimited},
	)

	dump := gather(t, reg)
	for _, want := range []string{`outcome="budget_exhausted"`, `outcome="rate_limited"`, `provider="none"`} {
		if !strings.Contains(dump, want) {
			t.Fatalf("missing %s in:\n%s", want, dump)
		}
	}
}

func TestStreamingIsADistinctSeries(t *testing.T) {
	// Streaming and non-streaming have different latency profiles; averaging
	// them together hides both.
	sink, reg := newSink(t)
	write(t, sink,
		core.UsageEvent{Tier: "pro", Provider: "echo", Stream: true},
		core.UsageEvent{Tier: "pro", Provider: "echo", Stream: false},
	)

	dump := gather(t, reg)
	if !strings.Contains(dump, `stream="true"`) || !strings.Contains(dump, `stream="false"`) {
		t.Fatalf("streaming is not separated:\n%s", dump)
	}
}

func TestAuditEventsAreNotAggregated(t *testing.T) {
	// Audit records are individually meaningful; turning them into a counter
	// loses the only thing they are for.
	sink, reg := newSink(t)
	write(t, sink, core.AuditEvent{Action: "key.rotate", Tenant: "acme"})

	if got := gather(t, reg); got != "" {
		t.Fatalf("an audit event reached a metric:\n%s", got)
	}
}

func TestEmptyLabelsGetAReadableFallback(t *testing.T) {
	// An empty label value is a legitimate Prometheus series and shows up in a
	// dashboard as a blank row nobody can explain.
	sink, reg := newSink(t)
	write(t, sink, core.UsageEvent{})

	dump := gather(t, reg)
	if !strings.Contains(dump, `tier="unknown"`) || !strings.Contains(dump, `provider="none"`) {
		t.Fatalf("empty labels were not defaulted:\n%s", dump)
	}
}

func TestZeroValuesDoNotCreateSeries(t *testing.T) {
	// A request that consumed nothing should not add a token or cost series;
	// otherwise every failed request inflates the series count for no signal.
	sink, reg := newSink(t)
	write(t, sink, core.UsageEvent{Tier: "free", Provider: "echo", Outcome: core.CodeUnauthenticated})

	dump := gather(t, reg)
	if strings.Contains(dump, "gateway_tokens_total") || strings.Contains(dump, "gateway_cost_micro_usd_total") {
		t.Fatalf("a zero-usage request created token or cost series:\n%s", dump)
	}
}

func TestRegisteringTwiceOnOneRegistryFails(t *testing.T) {
	// Duplicate registration is a wiring bug, and Prometheus panics on it at
	// scrape time rather than at startup unless it is caught here.
	reg := prometheus.NewRegistry()
	if _, err := telemetry.NewPrometheusSink(reg); err != nil {
		t.Fatalf("first registration: %v", err)
	}
	if _, err := telemetry.NewPrometheusSink(reg); err == nil {
		t.Fatal("a second registration on the same registry must fail")
	}
}
