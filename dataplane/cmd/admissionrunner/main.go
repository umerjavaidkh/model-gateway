// Command admissionrunner runs a port's contract suite against a registered
// component inside a sandbox, and reports the verdict to the control plane.
//
// It is deliberately a separate binary from both the gateway and the control
// plane. The gateway must never execute an unadmitted component; the control
// plane must never execute one at all. This runs on a disposable host, does the
// one dangerous thing, and reports a result it cannot act on.
//
//	admissionrunner \
//	  -control-plane https://control.internal \
//	  -component presidio -version 2.1.0 \
//	  -fixtures ./presidio-fixtures.json \
//	  -evidence s3://admissions/presidio-2.1.0.txt
//
// The fixtures file supplies the payloads only the publisher can know:
//
//	{"benign": "<base64>", "trigger": "<base64>"}
//
// Exit codes distinguish the two failures that matter: 1 means the run could
// not happen, 2 means it happened and the component failed. A CI job that
// treats those the same will retry a genuine failure forever.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/admission"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/sandbox"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/wasm"
)

const (
	exitCouldNotRun = 1
	exitFailedSuite = 2
)

type options struct {
	controlPlane string
	token        string
	component    string
	version      string
	fixtures     string
	evidence     string
	reportDir    string
	runtime      string
	moduleDir    string
	runnerName   string
	timeout      time.Duration
	dryRun       bool
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	opts := parseFlags()
	code, err := run(logger, opts)
	if err != nil {
		logger.Error("admission run failed", slog.String("error", err.Error()))
	}
	os.Exit(code)
}

func parseFlags() options {
	var opts options
	flag.StringVar(&opts.controlPlane, "control-plane", os.Getenv("GATEWAY_CONTROL_PLANE_URL"),
		"control-plane base URL")
	flag.StringVar(&opts.token, "token", os.Getenv("GATEWAY_CONTROL_PLANE_TOKEN"),
		"admin bearer token")
	flag.StringVar(&opts.component, "component", "", "component name")
	flag.StringVar(&opts.version, "version", "", "component version")
	flag.StringVar(&opts.fixtures, "fixtures", "", "JSON file of suite fixtures")
	flag.StringVar(&opts.evidence, "evidence", "",
		"where the report will be stored; recorded on the admission")
	flag.StringVar(&opts.reportDir, "report-dir", ".", "directory to write the report into")
	flag.StringVar(&opts.runtime, "runtime", sandbox.DefaultRuntime,
		"container runtime; use a VM-isolated one for genuinely untrusted code")
	flag.StringVar(&opts.moduleDir, "module-dir", os.Getenv("GATEWAY_WASM_DIR"),
		"directory of WASM modules, named by digest; required for in-process components")
	flag.StringVar(&opts.runnerName, "runner", defaultRunnerName(),
		"how this runner is identified on the admission record")
	flag.DurationVar(&opts.timeout, "timeout", sandbox.DefaultTimeout, "wall clock for the run")
	flag.BoolVar(&opts.dryRun, "dry-run", false,
		"run the suite and print the report without reporting a verdict")
	flag.Parse()
	return opts
}

func defaultRunnerName() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return "admissionrunner@" + host
}

func run(logger *slog.Logger, opts options) (int, error) {
	if opts.component == "" || opts.version == "" {
		return exitCouldNotRun, errors.New("-component and -version are required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fixtures, err := loadFixtures(opts.fixtures)
	if err != nil {
		return exitCouldNotRun, err
	}

	client, err := admission.NewClient(opts.controlPlane, opts.token, 30*time.Second)
	if err != nil {
		return exitCouldNotRun, err
	}

	manifest, err := client.Manifest(ctx, opts.component, opts.version)
	if err != nil {
		return exitCouldNotRun, err
	}
	logger.Info("running the suite",
		slog.String("component", manifest.Name+"@"+manifest.Version),
		slog.String("port", manifest.Port),
		slog.String("image", manifest.Image),
		slog.String("runtime", opts.runtime))

	runnerOptions := []admission.Option{
		admission.WithSandbox(sandbox.New(sandbox.WithRuntime(opts.runtime))),
		admission.WithLimits(sandbox.Limits{Timeout: opts.timeout}),
	}
	if opts.moduleDir != "" {
		store, err := wasm.NewModuleStore(opts.moduleDir)
		if err != nil {
			return exitCouldNotRun, err
		}
		runnerOptions = append(runnerOptions, admission.WithModules(store))
	}

	runner, err := admission.NewRunner(opts.runnerName, runnerOptions...)
	if err != nil {
		return exitCouldNotRun, err
	}

	verdict, err := runner.Run(ctx, manifest, fixtures)
	if err != nil {
		// The run could not happen. Reporting this as a failing verdict would
		// let an infrastructure problem look like a component defect.
		return exitCouldNotRun, err
	}
	verdict.EvidenceRef = opts.evidence

	reportPath, err := writeReport(opts, manifest, verdict)
	if err != nil {
		return exitCouldNotRun, err
	}
	fmt.Print(verdict.Report)
	logger.Info("report written", slog.String("path", reportPath))

	if opts.dryRun {
		logger.Info("dry run; no verdict reported", slog.Bool("passed", verdict.Passed))
		return passExit(verdict.Passed), nil
	}

	if err := client.Report(ctx, opts.component, opts.version, verdict); err != nil {
		return exitCouldNotRun, err
	}
	logger.Info("verdict reported", slog.Bool("passed", verdict.Passed))
	return passExit(verdict.Passed), nil
}

func passExit(passed bool) int {
	if passed {
		return 0
	}
	return exitFailedSuite
}

func loadFixtures(path string) (admission.Fixtures, error) {
	if path == "" {
		return admission.Fixtures{}, errors.New(
			"-fixtures is required: the suite needs a payload the component allows, " +
				"and only the publisher knows what that is")
	}
	raw, err := os.ReadFile(path) //nolint:gosec // an operator-supplied path
	if err != nil {
		return admission.Fixtures{}, fmt.Errorf("reading fixtures: %w", err)
	}
	var fixtures admission.Fixtures
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		return admission.Fixtures{}, fmt.Errorf("parsing fixtures: %w", err)
	}
	return fixtures, nil
}

// writeReport keeps the evidence locally even when reporting fails, because a
// verdict whose evidence was lost to a network error is a verdict nobody can
// check.
func writeReport(opts options, manifest admission.Manifest, verdict admission.Verdict) (string, error) {
	name := fmt.Sprintf("%s-%s.txt", manifest.Name, manifest.Version)
	path := filepath.Join(opts.reportDir, name)

	header := fmt.Sprintf(
		"component: %s@%s\nport: %s\ndigest: %s\nsuite version: %s\nrunner: %s\npassed: %v\n\n",
		manifest.Name, manifest.Version, manifest.Port, manifest.Digest,
		verdict.SuiteVersion, verdict.Runner, verdict.Passed)

	if err := os.WriteFile(path, []byte(header+verdict.Report), 0o600); err != nil {
		return "", fmt.Errorf("writing the report: %w", err)
	}
	return path, nil
}
