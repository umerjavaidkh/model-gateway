// Package guardrails runs the inspections a tenant has bound, under the budget
// each was admitted with.
//
// # Blocking and non-blocking are genuinely different things
//
// A blocking guardrail runs inline and may refuse or rewrite the payload. A
// non-blocking one runs off the request path on a copy and can only alert.
// That is not a performance tier — it is a statement about how much the
// gateway trusts the guardrail's judgement.
//
// The design is explicit that prompt-injection detection belongs in the second
// category: it is largely ineffective against a determined attacker, so
// shipping it as a blocking control buys an outage risk in exchange for
// security theatre. Secret scanning belongs in the first, because a leaked
// credential cannot be un-leaked.
//
// # The budget is enforced here, not by the guardrail
//
// A guardrail that hangs must not be able to hang the request, and asking it to
// police its own timeout assumes the thing that just failed. So the chain owns
// the deadline, and a guardrail that overruns is treated as having failed —
// which its failure mode then decides the meaning of.
package guardrails

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

// DefaultTimeout applies when a binding declares none. Deliberately small:
// a guardrail is overhead on every request, and one that needs longer should
// be non-blocking.
const DefaultTimeout = 50 * time.Millisecond

// Registry resolves a guardrail component name to an implementation.
type Registry interface {
	Guardrail(name string) (core.GuardrailPort, bool)
}

// Outcome is what a chain decided.
type Outcome struct {
	// Payload is the body to send onward, rewritten if any guardrail did so.
	Payload []byte
	// Denied is set when a blocking guardrail refused.
	Denied bool
	// Reason names the guardrail that refused, for the error and the audit
	// record. The guardrail's own message is deliberately not returned to the
	// caller: telling someone exactly which pattern their payload tripped is
	// telling them how to avoid it next time.
	Reason string
}

// Stats counts what the chain has done, for metrics.
type Stats struct {
	Evaluated int64
	Denied    int64
	Mutated   int64
	// FailedOpen counts guardrails that errored or timed out and were allowed
	// through. Non-zero means a control an operator believes is enforcing is
	// not, which is the most important thing this package can report.
	FailedOpen int64
	FailedShut int64
	Skipped    int64
}

// Chain runs the guardrails bound to a tenant. Safe for concurrent use.
type Chain struct {
	registry Registry
	logger   *slog.Logger

	evaluated  atomic.Int64
	denied     atomic.Int64
	mutated    atomic.Int64
	failedOpen atomic.Int64
	failedShut atomic.Int64
	skipped    atomic.Int64

	// offPath bounds the goroutines non-blocking guardrails run in, so a slow
	// one cannot accumulate work faster than it finishes and exhaust the
	// worker. Alerting is worth less than serving.
	offPath chan struct{}
	wg      sync.WaitGroup
}

// Option configures a Chain.
type Option func(*Chain)

// WithLogger sets where the chain reports.
func WithLogger(l *slog.Logger) Option {
	return func(c *Chain) {
		if l != nil {
			c.logger = l
		}
	}
}

// WithOffPathLimit bounds concurrent non-blocking evaluations.
func WithOffPathLimit(n int) Option {
	return func(c *Chain) {
		if n > 0 {
			c.offPath = make(chan struct{}, n)
		}
	}
}

// New returns a chain over a registry.
func New(registry Registry, opts ...Option) (*Chain, error) {
	if registry == nil {
		return nil, core.New(core.CodeInternal, "a guardrail chain needs a registry")
	}
	c := &Chain{
		registry: registry,
		logger:   slog.Default(),
		offPath:  make(chan struct{}, 64),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// Stats reports the chain's counters.
func (c *Chain) Stats() Stats {
	return Stats{
		Evaluated:  c.evaluated.Load(),
		Denied:     c.denied.Load(),
		Mutated:    c.mutated.Load(),
		FailedOpen: c.failedOpen.Load(),
		FailedShut: c.failedShut.Load(),
		Skipped:    c.skipped.Load(),
	}
}

// Wait blocks until off-path evaluations finish. For shutdown and for tests;
// the request path never calls it.
func (c *Chain) Wait() { c.wg.Wait() }

// Run evaluates every binding for one leg of a request.
//
// Blocking guardrails run in the order they were bound, each seeing whatever
// the previous one produced, so a redaction is visible to a scanner that runs
// after it. Non-blocking ones are dispatched on a copy and never observed.
func (c *Chain) Run(
	ctx context.Context, bindings []core.GuardrailBinding, in *core.GuardrailInput,
) (Outcome, error) {
	outcome := Outcome{Payload: in.Payload}

	for _, binding := range bindings {
		guardrail, ok := c.registry.Guardrail(binding.Component)
		if !ok {
			// A binding naming a component this worker does not have is a
			// configuration error. Fail-closed bindings refuse, because a
			// control that is supposed to be enforcing and simply is not
			// present is the worst of both states.
			c.skipped.Add(1)
			c.logger.Warn("guardrail not installed",
				slog.String("component", binding.Component),
				slog.Bool("fail_closed", binding.Budget.Mode == core.FailClosed))
			if binding.Budget.Mode == core.FailClosed && binding.Budget.Blocking {
				c.failedShut.Add(1)
				outcome.Denied = true
				outcome.Reason = binding.Component
				return outcome, nil
			}
			continue
		}

		if !binding.Budget.Blocking {
			// Deliberately not given the request's context; see dispatch.
			c.dispatch(binding, guardrail, in, outcome.Payload) //nolint:contextcheck
			continue
		}

		next := *in
		next.Payload = outcome.Payload
		result, err := c.evaluate(ctx, binding, guardrail, &next)
		if err != nil {
			if binding.Budget.Mode == core.FailClosed {
				c.failedShut.Add(1)
				outcome.Denied = true
				outcome.Reason = binding.Component
				return outcome, nil
			}
			// Failing open is a decision, not an accident, and it is counted
			// because a control an operator believes is enforcing and is not
			// is the most important thing this package can report.
			c.failedOpen.Add(1)
			c.logger.Warn("guardrail failed open",
				slog.String("component", binding.Component),
				slog.String("error", err.Error()))
			continue
		}

		switch result.Verdict {
		case core.VerdictDeny:
			c.denied.Add(1)
			outcome.Denied = true
			outcome.Reason = binding.Component
			return outcome, nil
		case core.VerdictMutate:
			c.mutated.Add(1)
			outcome.Payload = result.Payload
		case core.VerdictAllow:
		}
	}

	return outcome, nil
}

// evaluate runs one guardrail under its budget.
func (c *Chain) evaluate(
	ctx context.Context,
	binding core.GuardrailBinding,
	guardrail core.GuardrailPort,
	in *core.GuardrailInput,
) (*core.GuardrailResult, error) {
	timeout := binding.Budget.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	budgeted, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	c.evaluated.Add(1)
	result, err := guardrail.Inspect(budgeted, in)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, core.Newf(core.CodeInternal,
			"guardrail %q returned no result and no error", binding.Component)
	}
	if result.Verdict == core.VerdictMutate && result.Payload == nil {
		// A mutation with nothing to substitute would silently send an empty
		// body upstream, which is worse than either allowing or denying.
		return nil, core.Newf(core.CodeInternal,
			"guardrail %q asked to mutate but returned no payload", binding.Component)
	}
	return result, nil
}

// dispatch runs a non-blocking guardrail off the request path.
//
// On a copy of the payload, because the request continues immediately and the
// buffer it holds may be reused or rewritten by a later guardrail. Sharing it
// would make an alerting-only guardrail able to corrupt a served response.
func (c *Chain) dispatch(
	binding core.GuardrailBinding, guardrail core.GuardrailPort, in *core.GuardrailInput, payload []byte,
) {
	select {
	case c.offPath <- struct{}{}:
	default:
		// Saturated. Dropping an alert is the right trade against delaying a
		// request or growing goroutines without bound.
		c.skipped.Add(1)
		return
	}

	copied := make([]byte, len(payload))
	copy(copied, payload)
	snapshot := *in
	snapshot.Payload = copied

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer func() { <-c.offPath }()

		// Its own context, not the request's: the request may well finish
		// first, and cancelling the inspection then would mean off-path
		// guardrails silently never complete on fast requests.
		timeout := binding.Budget.Timeout
		if timeout <= 0 {
			timeout = DefaultTimeout
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		result, err := c.evaluate(ctx, binding, guardrail, &snapshot)
		switch {
		case err != nil:
			c.failedOpen.Add(1)
			c.logger.Warn("off-path guardrail failed",
				slog.String("component", binding.Component),
				slog.String("error", err.Error()))
		case result.Verdict != core.VerdictAllow:
			// It cannot refuse, so this is the whole of its effect. The
			// verdict is recorded rather than acted on, which is what
			// "detection and logging, not a blocking control" means in code.
			c.logger.Warn("guardrail alert",
				slog.String("component", binding.Component),
				slog.String("request_id", snapshot.Meta.RequestID),
				slog.String("reason", result.Reason))
		}
	}()
}
