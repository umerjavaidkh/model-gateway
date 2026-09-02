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
	"fmt"
	"os"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/adapters/guardrailsidecar"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/adapters/wasmguardrail"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/contracts"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/sandbox"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/wasm"
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
	// Module is the sha256 digest of the WASM module, for components that run
	// in the worker's process.
	Module string `json:"module"`
	// Execution decides how the component is isolated, and therefore how this
	// package runs it: a container for a sidecar, the WASM runtime itself for
	// an in-process module.
	Execution string `json:"execution"`
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

// ExecutionInProcess and ExecutionSidecar are the modes this package can run.
//
// Strings rather than an enum: they arrive as JSON from the control plane,
// which owns the vocabulary, and a mode this runner does not recognise must be
// refused rather than mapped onto a default.
const (
	ExecutionInProcess = "in_process"
	ExecutionSidecar   = "sidecar"
)

// Runner runs suites against sandboxed components.
type Runner struct {
	sandbox sandbox.Runner
	modules *wasm.ModuleStore
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

// WithModules lets the runner admit in-process components by telling it where
// their WASM modules are. Without one, an in-process component is refused
// rather than skipped, because a component nobody can test is not a component
// that passed.
func WithModules(store *wasm.ModuleStore) Option {
	return func(r *Runner) { r.modules = store }
}

// WithSandbox lets the runner admit sidecar components by giving it a
// container runtime to launch them in.
func WithSandbox(box sandbox.Runner) Option {
	return func(r *Runner) { r.sandbox = box }
}

// NewRunner returns a Runner.
//
// Both isolation mechanisms are optional, and a runner with neither is refused
// rather than left to fail on its first component. A WASM-only deployment
// genuinely needs no container runtime — not needing one is most of the point
// of running a component in process — and a sidecar-only one needs no module
// store, so requiring either would make one of those deployments carry
// something it has no use for.
func NewRunner(name string, opts ...Option) (*Runner, error) {
	if name == "" {
		return nil, core.New(core.CodeInvalidRequest,
			"a runner must be named, so an admission says what produced it")
	}

	runner := &Runner{name: name}
	for _, opt := range opts {
		opt(runner)
	}
	if runner.sandbox == nil && runner.modules == nil {
		return nil, core.New(core.CodeInvalidRequest,
			"a runner needs a sandbox, a module store, or both; with neither it "+
				"can admit nothing")
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

	// The isolation mechanism differs but the battery does not. Both paths
	// return a core.GuardrailPort and the same suite decides.
	switch manifest.Execution {
	case ExecutionInProcess:
		return r.runInProcess(ctx, manifest, fixtures)
	case ExecutionSidecar, "":
		// Empty means a control plane that predates this field, and sidecar is
		// what it would have meant.
		if r.sandbox == nil {
			return Verdict{}, core.New(core.CodeInvalidRequest,
				"this runner has no sandbox, so it cannot admit a sidecar component")
		}
	default:
		return Verdict{}, core.Newf(core.CodeInvalidRequest,
			"this runner cannot execute a %s component", manifest.Execution)
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

	guardrail, err := guardrailsidecar.New(manifest.Name, handle.SocketPath,
		guardrailsidecar.WithTimeout(callTimeout))
	if err != nil {
		return Verdict{}, err
	}

	// Reachability is checked before the battery, and its failure is an error
	// rather than a verdict. The socket existing only means the component
	// created the file; if nothing accepts a connection on it, every case
	// fails with the same dial error and the report reads as though the
	// component were broken. It might be — but so might the mount, the
	// runtime, or the host, and a runner that cannot tell those apart is
	// exactly the runner this package's exit codes claim not to be.
	readyCtx, cancelReady := context.WithTimeout(ctx, readyTimeout)
	defer cancelReady()
	if err := guardrail.Ping(readyCtx); err != nil {
		return Verdict{}, core.Wrapf(core.CodeUnavailable, err,
			"the component bound %s but does not answer on it", handle.SocketPath)
	}

	recorder := contracts.NewRecorder(ctx, manifest.Name)
	defer recorder.Finish()

	// The suite calls the component with t.Context(), which is the ctx the
	// recorder was built with — contextcheck cannot see through the interface
	// to know the deadline is already threaded.
	contracts.RunGuardrailSuite(recorder, func(contracts.T) contracts.GuardrailTarget { //nolint:contextcheck
		return contracts.GuardrailTarget{
			Guardrail: guardrail,
			Trigger:   fixtures.Trigger,
			Benign:    fixtures.Benign,
		}
	})

	return r.verdictFrom(manifest, recorder.Report()), nil
}

// runInProcess admits a WASM component.
//
// There is no container here, and that is not a gap. A WASM module has no
// ambient authority at all: it cannot open a file, dial a socket, or address
// anything but its own linear memory. That is a stronger boundary than the
// container the sidecar path uses, and unlike that one it does not rest on a
// shared kernel — so the isolation this path skips is isolation it does not
// need.
func (r *Runner) runInProcess(
	ctx context.Context, manifest Manifest, fixtures Fixtures,
) (Verdict, error) {
	if r.modules == nil {
		return Verdict{}, core.New(core.CodeInvalidRequest,
			"this runner has no module store, so it cannot admit an in-process component")
	}

	// Verified against the manifest, not trusted from a path. An admission
	// record vouches for specific bytes.
	wasmBytes, err := r.modules.Load(manifest.Module)
	if err != nil {
		return Verdict{}, err
	}

	runtime, err := wasm.NewRuntime(ctx, wasm.Limits{})
	if err != nil {
		return Verdict{}, err
	}
	defer func() { _ = runtime.Close(context.WithoutCancel(ctx)) }()

	module, err := runtime.Compile(ctx, manifest.Name, wasmBytes)
	if err != nil {
		// A module that will not compile is a component that failed, not a run
		// that could not happen: the bytes are the thing under test and they
		// are right here.
		return r.verdictFor(manifest, false,
			fmt.Sprintf("the module does not compile or does not implement the ABI: %v", err)), nil
	}
	defer func() { _ = module.Close(context.WithoutCancel(ctx)) }()

	guardrail, err := wasmguardrail.New(manifest.Name, module)
	if err != nil {
		return Verdict{}, err
	}

	recorder := contracts.NewRecorder(ctx, manifest.Name)
	defer recorder.Finish()

	contracts.RunGuardrailSuite(recorder, func(contracts.T) contracts.GuardrailTarget { //nolint:contextcheck
		return contracts.GuardrailTarget{
			Guardrail: guardrail,
			Trigger:   fixtures.Trigger,
			Benign:    fixtures.Benign,
		}
	})
	return r.verdictFrom(manifest, recorder.Report()), nil
}

func (r *Runner) verdictFrom(manifest Manifest, report contracts.Report) Verdict {
	verdict := r.verdictFor(manifest, report.Passed(), "")
	verdict.Report = report.String()
	return verdict
}

// verdictFor builds a verdict that did not come from a battery, for the case
// where the component failed before one could run.
func (r *Runner) verdictFor(manifest Manifest, passed bool, note string) Verdict {
	return Verdict{
		Suite:          manifest.Port,
		SuiteVersion:   SuiteVersion,
		ManifestDigest: manifest.Digest,
		Passed:         passed,
		Runner:         r.name,
		Report:         note,
	}
}

// callTimeout bounds one call to the component during a suite run.
//
// Generous compared with a production budget, because a cold sandbox on its
// first request is not what a latency budget is about. The suite's own
// deadline case is what checks the component respects a deadline at all.
const callTimeout = 10 * time.Second

// readyTimeout bounds the reachability check.
//
// Short, because the sandbox has already waited for the socket to appear. What
// remains is one round trip to a process on the same host.
const readyTimeout = 5 * time.Second
