package contracts

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// T is the slice of *testing.T the suites use.
//
// The suites exist to be run two ways: by `go test` against our own adapters,
// and by the admission runner against a third-party component in a sandbox.
// Only the first has a *testing.T, so the suites take this instead — and there
// is exactly one battery either way, which is the whole point. A second copy
// maintained for the runner would drift, and the drift would always be in the
// direction of the runner's copy asking for less.
type T interface {
	Helper()
	Name() string
	Context() context.Context
	// Cleanup runs f when the current case ends, however it ends. Factories
	// need it: one that opens a connection per case and cannot close it leaks
	// a connection per case, and the runner drives a sandbox that has to come
	// down whether the case passed or not.
	Cleanup(f func())
	// Logf records context for the report. A failure message says what went
	// wrong; a log line is how a publisher learns what the suite was doing at
	// the time, which is the difference between a report and a verdict.
	Logf(format string, args ...any)
	Error(args ...any)
	Errorf(format string, args ...any)
	Fatal(args ...any)
	Fatalf(format string, args ...any)
	// Run executes a named case. It reports whether the case passed, and never
	// panics out of a failure — a failing case must not abort the battery,
	// because a report listing one failure out of twelve is far less useful
	// than one listing all three.
	Run(name string, f func(T)) bool
}

// Adapt exposes a *testing.T as a T.
func Adapt(t *testing.T) T { return testingT{t} }

// testingT embeds *testing.T so every method of T except Run is the real one.
// Only Run differs, because testing.T.Run takes a func(*testing.T) and the
// suites take a func(T); the explicit method below shadows the promoted one.
type testingT struct{ *testing.T }

// Run implements T by adapting testing's subtest signature.
func (a testingT) Run(name string, f func(T)) bool {
	return a.T.Run(name, func(t *testing.T) { f(testingT{t}) })
}

// Case is what one contract assertion did.
type Case struct {
	Name string
	// Failures is empty when the case passed. Every message is kept rather than
	// only the first, because "it failed" is not actionable and a publisher
	// fixing one assertion wants to see the others in the same run.
	Failures []string
	// Logs is what the suite reported while running, kept whether the case
	// passed or not — a passing case's log is what a publisher compares
	// against when the next version stops passing.
	Logs     []string
	Duration time.Duration
}

// Passed reports whether the case recorded no failure.
func (c Case) Passed() bool { return len(c.Failures) == 0 }

// Report is the outcome of a whole battery.
type Report struct {
	Cases []Case
}

// Passed reports whether every case passed. An empty report has not passed:
// a battery that ran nothing is a battery that proved nothing, and treating
// "no failures" as success would admit a component the runner never reached.
func (r Report) Passed() bool {
	if len(r.Cases) == 0 {
		return false
	}
	for _, c := range r.Cases {
		if !c.Passed() {
			return false
		}
	}
	return true
}

// Summary is a one-line verdict, for a log or an admission record.
func (r Report) Summary() string {
	failed := 0
	for _, c := range r.Cases {
		if !c.Passed() {
			failed++
		}
	}
	return fmt.Sprintf("%d/%d cases passed", len(r.Cases)-failed, len(r.Cases))
}

// String renders the report, failures first.
func (r Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", r.Summary())
	for _, c := range r.Cases {
		if c.Passed() {
			fmt.Fprintf(&b, "  ok    %s (%s)\n", c.Name, c.Duration.Round(time.Millisecond))
			continue
		}
		fmt.Fprintf(&b, "  FAIL  %s (%s)\n", c.Name, c.Duration.Round(time.Millisecond))
		for _, failure := range c.Failures {
			for _, line := range strings.Split(failure, "\n") {
				fmt.Fprintf(&b, "        %s\n", line)
			}
		}
	}
	return b.String()
}

// Recorder runs a battery outside `go test` and collects what happened.
//
// Not safe for concurrent use across cases; the suites are sequential.
type Recorder struct {
	ctx  context.Context
	name string
	// path is this recorder's position in the case tree, empty at the root. It
	// is what a case is named in the report, so a nested case reads as
	// "streaming/final chunk" rather than as a bare leaf name that could
	// belong to any parent.
	path   string
	report *Report

	mu       sync.Mutex
	current  *Case
	cleanups []func()
}

// NewRecorder returns a Recorder whose cases run under ctx.
//
// The context is the runner's deadline for the whole battery. A component that
// hangs on one assertion must not hang the runner, and the suites already take
// their context from T.
func NewRecorder(ctx context.Context, name string) *Recorder {
	return &Recorder{ctx: ctx, name: name, report: &Report{}}
}

// Report returns what has been recorded so far.
func (r *Recorder) Report() Report { return *r.report }

// Helper implements T. There is no caller-line reporting outside `go test`, so
// it does nothing.
func (r *Recorder) Helper() {}

// Cleanup registers f to run when this case ends.
func (r *Recorder) Cleanup(f func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cleanups = append(r.cleanups, f)
}

// Finish runs cleanups registered outside any case.
//
// Case-scoped cleanups run when their case ends; this is for anything the
// caller registered on the root recorder, and it is a separate call because a
// getter that also tears things down is a trap.
func (r *Recorder) Finish() { r.runCleanups() }

// runCleanups runs registered functions in reverse order, as testing does, so
// a teardown can rely on whatever it was registered after still existing.
func (r *Recorder) runCleanups() {
	r.mu.Lock()
	pending := r.cleanups
	r.cleanups = nil
	r.mu.Unlock()

	for i := len(pending) - 1; i >= 0; i-- {
		func() {
			// A panicking cleanup must not lose the report or skip the ones
			// after it. It becomes a failure like anything else.
			defer func() {
				if recovered := recover(); recovered != nil {
					r.fail(fmt.Sprintf("cleanup panicked: %v", recovered))
				}
			}()
			pending[i]()
		}()
	}
}

// Name implements T.
func (r *Recorder) Name() string { return r.name }

// Context implements T, returning the runner's deadline for the battery.
func (r *Recorder) Context() context.Context { return r.ctx }

// Logf records a line against the current case.
func (r *Recorder) Logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current != nil {
		r.current.Logs = append(r.current.Logs, line)
	}
}

// Error implements T, recording a failure and continuing.
func (r *Recorder) Error(args ...any) { r.fail(fmt.Sprint(args...)) }

// Errorf implements T, recording a failure and continuing.
func (r *Recorder) Errorf(f string, args ...any) { r.fail(fmt.Sprintf(f, args...)) }

// Fatal implements T, recording a failure and ending the case.
func (r *Recorder) Fatal(args ...any) { r.fail(fmt.Sprint(args...)); r.abort() }

// Fatalf implements T, recording a failure and ending the case.
func (r *Recorder) Fatalf(f string, args ...any) { r.fail(fmt.Sprintf(f, args...)); r.abort() }

// Run executes one case, isolating a Fatal to that case.
//
// The goroutine is what makes Fatal mean what it means in `go test`: it stops
// the case and nothing else. Without it, a Fatal in the first assertion would
// end the battery and the report would claim eleven cases were never reached
// when in fact they were never tried.
func (r *Recorder) Run(name string, f func(T)) bool {
	path := name
	if r.path != "" {
		path = r.path + "/" + name
	}
	sub := &Recorder{ctx: r.ctx, name: r.name + "/" + name, path: path, report: r.report}
	entry := Case{Name: path}
	sub.current = &entry

	started := time.Now()
	done := make(chan struct{})
	go func() {
		// A panic in a third-party component's suite is a failure of that
		// component, not a crash of the runner. Reporting it as a failed case
		// is more useful than a stack trace nobody asked for.
		defer func() {
			if recovered := recover(); recovered != nil {
				sub.fail(fmt.Sprintf("panicked: %v", recovered))
			}
			close(done)
		}()
		f(sub)
	}()
	<-done
	sub.runCleanups()

	entry.Duration = time.Since(started)
	r.report.Cases = append(r.report.Cases, entry)
	return entry.Passed()
}

func (r *Recorder) fail(message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current == nil {
		// A failure outside any case: the suite reported something before its
		// first Run, so it becomes a case of its own rather than vanishing.
		r.report.Cases = append(r.report.Cases, Case{Name: "setup", Failures: []string{message}})
		return
	}
	r.current.Failures = append(r.current.Failures, message)
}

// abort ends the current case the way testing.T.Fatal does.
func (r *Recorder) abort() { runtime.Goexit() }
