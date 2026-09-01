package telemetry

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

// PrometheusSink turns usage events into metrics.
//
// # Cardinality is the whole design constraint
//
// Every label value multiplies the number of time series Prometheus stores.
// Tenant ID as a label is unbounded — it grows with the customer list and never
// shrinks, because Prometheus keeps series long after they stop receiving
// samples. A gateway with ten thousand tenants and a handful of other labels
// produces millions of series and takes Prometheus down with it.
//
// So the labels here are all small closed sets: plan tier, outcome code,
// provider name, and whether the request streamed. Tenant ID lives in the usage
// event and the log, which is where per-tenant questions get answered. This is
// the single most common way a gateway kills its own monitoring, and it is
// cheaper to prevent than to migrate away from.
type PrometheusSink struct {
	requests *prometheus.CounterVec
	tokens   *prometheus.CounterVec
	cost     *prometheus.CounterVec
	price    *prometheus.CounterVec
	latency  *prometheus.HistogramVec
	ttfb     *prometheus.HistogramVec
}

// NewPrometheusSink registers the collectors and returns the sink.
func NewPrometheusSink(reg prometheus.Registerer) (*PrometheusSink, error) {
	// tier, outcome, provider and stream are each a handful of values, so the
	// product stays in the low hundreds of series.
	labels := []string{"tier", "outcome", "provider", "stream"}

	s := &PrometheusSink{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_requests_total",
			Help: "Requests served, by tenant tier and outcome.",
		}, labels),
		tokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_tokens_total",
			Help: "Tokens consumed, by direction.",
		}, []string{"tier", "provider", "direction"}),
		cost: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_cost_micro_usd_total",
			Help: "What providers charge us, in millionths of a US dollar.",
		}, []string{"tier", "provider"}),
		// Separate from cost so margin is a query rather than an estimate.
		// Equal to cost until a rate card exists, which is exactly why it is
		// recorded now: a series that starts later has no history.
		price: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_price_micro_usd_total",
			Help: "What tenants are charged, in millionths of a US dollar.",
		}, []string{"tier", "provider"}),
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "gateway_request_duration_seconds",
			Help: "End-to-end request duration.",
			// Buckets span a model call, not a web request: the interesting
			// range is hundreds of milliseconds to a minute, and the default
			// buckets top out at ten seconds.
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60, 120},
		}, labels),
		ttfb: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gateway_time_to_first_byte_seconds",
			Help:    "Time to the first streamed byte, which is what a user perceives as latency.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10},
		}, []string{"tier", "provider"}),
	}

	for _, c := range []prometheus.Collector{
		s.requests, s.tokens, s.cost, s.price, s.latency, s.ttfb,
	} {
		if err := reg.Register(c); err != nil {
			return nil, core.Wrap(core.CodeInternal, err, "registering a metric collector")
		}
	}
	return s, nil
}

// Name identifies the sink.
func (*PrometheusSink) Name() string { return "prometheus" }

// Write records the usage events in a batch. Audit events are ignored: they are
// individually meaningful records, not something to aggregate into a counter.
func (s *PrometheusSink) Write(_ context.Context, events []core.Event) error {
	for _, event := range events {
		usage, ok := event.(core.UsageEvent)
		if !ok {
			continue
		}

		tier := labelOr(usage.Tier, "unknown")
		outcome := labelOr(string(usage.Outcome), "ok")
		provider := labelOr(usage.Provider, "none")
		stream := "false"
		if usage.Stream {
			stream = "true"
		}

		s.requests.WithLabelValues(tier, outcome, provider, stream).Inc()
		s.latency.WithLabelValues(tier, outcome, provider, stream).
			Observe(float64(usage.LatencyMs) / 1000)

		// Each class is its own series: a cache hit rate is the ratio between
		// them, and it is the single biggest lever on what a workload costs.
		for direction, count := range map[string]int64{
			"input":        usage.InputTokens,
			"cached_input": usage.CachedInputTokens,
			"cache_write":  usage.CacheWriteTokens,
			"output":       usage.OutputTokens,
		} {
			if count > 0 {
				s.tokens.WithLabelValues(tier, provider, direction).Add(float64(count))
			}
		}
		if usage.CostMicroUSD > 0 {
			s.cost.WithLabelValues(tier, provider).Add(float64(usage.CostMicroUSD))
		}
		if usage.PriceMicroUSD > 0 {
			s.price.WithLabelValues(tier, provider).Add(float64(usage.PriceMicroUSD))
		}
		if usage.TimeToFirstByte > 0 {
			s.ttfb.WithLabelValues(tier, provider).Observe(usage.TimeToFirstByte.Seconds())
		}
	}
	return nil
}

// labelOr keeps an empty label value out of the metric. An empty string is a
// legitimate series in Prometheus and shows up in dashboards as a blank row
// that nobody can explain.
func labelOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
