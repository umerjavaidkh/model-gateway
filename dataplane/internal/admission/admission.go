// Package admission runs a port's contract suite against a component in a
// sandbox and reports what happened.
//
// It is the gate the control plane refuses to be. The control plane holds
// database credentials, the key pepper, and the network position of the thing
// that configures every worker; this runs somewhere disposable, executes the
// component, and sends back a verdict it has no authority to overrule.
//
// The division matters more than it looks. This package can only *report*: it
// produces a record naming the suite, the manifest digest it examined and the
// result, and the control plane decides what that means. A runner that could
// activate a component directly would be a runner whose compromise is an
// activation.
package admission

import (
	"context"
	"os"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/adapters/guardrailsidecar"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/contracts"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/sandbox"
)

// SuiteVersion identifies the battery that ran.
//
// Bumped whenever a suite gains or changes a case. Components admitted under
// an older version stay admitted and are visibly admitted under an older bar,
// rather than silently grandfathered — the control plane stores this, so
// "which of these passed the current suite" is answerable.
const SuiteVersion = "1"

// Manifest is the part of a component's registration this package needs.
//
// A local struct rather than a shared type: the runner reads a manifest from
// the control plane's JSON, and coupling it to the control plane's Python
// dataclass through a generated type would make the sandbox host depend on the
// snapshot schema, which it has nothing to do with.
type Manifest struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Port    string `json:"port"`
	Digest  string `json:"digest"`
	Image   string `json:"image"`
	// LatencyBudgetMS is what the component claims it needs. The suite does not
	// assert against it — a contract suite runs on a cold sandbox and would
	// measure the sandbox — but it is recorded so a binding can be checked.
	LatencyBudgetMS int `json:"latency_budget_ms"`
}

// Verdict is what a run produces. It maps directly onto the admission record
// the control plane stores.
type Verdict struct {
	Suite          string `json:"suite"`
	SuiteVersion   string `json:"suite_version"`
	ManifestDigest string `json:"manifest_digest"`
	Passed         bool   `json:"passed"`
	Runner         string `json:"runner"`
	EvidenceRef    string `json:"evidence_ref"`
	// Report is the full battery output. Not sent to the control plane — that
	// stores a reference — but written wherever the evidence lives, because a
	// verdict with no evidence is an opinion.
	Report string `json:"-"`
}

// Fixtures are the payloads a suite needs from the publisher.
//
// Supplied per component because only the publisher knows what their component
// is for: a secret scanner and a content policy share nothing but the
// interface. A component that supplies no trigger is taken at its word that it
// only ever allows, and the suite says so in the report rather than asserting
// a behaviour that was never claimed.
type Fixtures struct {
	Trigger []byte `json:"trigger"`
	Benign  []byte `json:"benign"`
}

// Runner runs suites against sandboxed components.
type Runner struct {
	sandbox sandbox.Runner
	// name identifies this runner in the record. An auditor needs to be able
	// to tell one sandbox host from another, and from the control plane.
	name   string
	limits sandbox.Limits
}

// Option configures a Runner.
type Option func(*Runner)

// WithLimits sets the resource bounds sandboxed components run under.
func WithLimits(limits sandbox.Limits) Option {
	return func(r *Runner) { r.limits = limits }
}

// NewRunner returns a Runner that launches components with the given sandbox.
func NewRunner(name string, box sandbox.Runner, opts ...Option) (*Runner, error) {
	if name == "" {
		return nil, core.New(core.CodeInvalidRequest,
			"a runner must be named, so an admission says what produced it")
	}
	if box == nil {
		return nil, core.New(core.CodeInvalidRequest, "a runner needs a sandbox")
	}

	runner := &Runner{sandbox: box, name: name}
	for _, opt := range opts {
		opt(runner)
	}
	return runner, nil
}

// Run executes the suite for the manifest's port and returns a verdict.
//
// An error means the run could not happen — the image would not start, the
// port has no suite. That is deliberately not a failing verdict: "we could not
// test this" and "this failed its tests" are different facts, and recording
// the first as the second would let an infrastructure problem look like a
// component defect.
func (r *Runner) Run(ctx context.Context, manifest Manifest, fixtures Fixtures) (Verdict, error) {
	if manifest.Digest == "" {
		return Verdict{}, core.New(core.CodeInvalidRequest,
			"a manifest digest is required; a verdict must say which bytes it examined")
	}
	if manifest.Port != "guardrail" {
		// Provider and KV suites exist but need a live upstream or a store,
		// which is a different sandbox shape. Refusing is better than running
		// an empty battery and reporting a pass.
		return Verdict{}, core.Newf(core.CodeInvalidRequest,
			"no sandboxed suite for the %s port yet", manifest.Port)
	}
	if len(fixtures.Benign) == 0 {
		return Verdict{}, core.New(core.CodeInvalidRequest,
			"a benign fixture is required; without it a guardrail that denies "+
				"everything passes every assertion about denying")
	}

	socketDir, err := os.MkdirTemp("", "gwadm")
	if err != nil {
		return Verdict{}, core.Wrap(core.CodeInternal, err, "creating the socket directory")
	}
	defer func() { _ = os.RemoveAll(socketDir) }()
	// The container runs as an unprivileged user, so the mounted directory has
	// to be writable by it.
	if err := os.Chmod(socketDir, 0o777); err != nil { //nolint:gosec // mounted into an isolated, networkless container
		return Verdict{}, core.Wrap(core.CodeInternal, err, "preparing the socket directory")
	}

	handle, err := r.sandbox.Start(ctx, sandbox.Spec{
		Image:      manifest.Image,
		SocketDir:  socketDir,
		SocketName: "component.sock",
		Limits:     r.limits,
	})
	if err != nil {
		return Verdict{}, err
	}
	defer func() { _ = handle.Close() }()

	recorder := contracts.NewRecorder(ctx, manifest.Name)
	defer recorder.Finish()

	// The suite calls the component with t.Context(), which is the ctx the
	// recorder was built with — contextcheck cannot see through the interface
	// to know the deadline is already threaded.
	contracts.RunGuardrailSuite(recorder, func(t contracts.T) contracts.GuardrailTarget { //nolint:contextcheck
		guardrail, err := guardrailsidecar.New(manifest.Name, handle.SocketPath,
			guardrailsidecar.WithTimeout(callTimeout))
		if err != nil {
			t.Fatalf("connecting to the component: %v", err)
		}
		return contracts.GuardrailTarget{
			Guardrail: guardrail,
			Trigger:   fixtures.Trigger,
			Benign:    fixtures.Benign,
		}
	})

	report := recorder.Report()
	return Verdict{
		Suite:          manifest.Port,
		SuiteVersion:   SuiteVersion,
		ManifestDigest: manifest.Digest,
		Passed:         report.Passed(),
		Runner:         r.name,
		Report:         report.String(),
	}, nil
}

// callTimeout bounds one call to the component during a suite run.
//
// Generous compared with a production budget, because a cold sandbox on its
// first request is not what a latency budget is about. The suite's own
// deadline case is what checks the component respects a deadline at all.
const callTimeout = 10 * time.Second
