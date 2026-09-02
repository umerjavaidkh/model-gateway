package contracts_test

import (
	"context"
	"strings"
	"testing"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/contracts"
)

// The Recorder is what lets one battery serve both `go test` and the admission
// runner. If its semantics differ from testing.T's, the runner is effectively
// running a different suite from the one we test our own adapters with.

func TestFatalEndsOnlyItsOwnCase(t *testing.T) {
	// The whole point of the goroutine in Run. Without it, a Fatal in the first
	// case would end the battery and the report would be missing eleven cases
	// that were never tried.
	recorder := contracts.NewRecorder(t.Context(), "suite")

	recorder.Run("first", func(t contracts.T) {
		t.Fatal("stop here")
		t.Error("unreachable") //nolint:govet // asserting Fatal actually stops
	})
	recorder.Run("second", func(contracts.T) {})

	report := recorder.Report()
	if len(report.Cases) != 2 {
		t.Fatalf("cases = %d, want both to have run", len(report.Cases))
	}
	if report.Cases[0].Passed() || !report.Cases[1].Passed() {
		t.Fatalf("cases = %+v", report.Cases)
	}
	if len(report.Cases[0].Failures) != 1 {
		t.Fatalf("failures = %v, want only the one before Fatal", report.Cases[0].Failures)
	}
}

func TestEveryFailureInACaseIsKept(t *testing.T) {
	// A publisher fixing one assertion wants to see the others in the same run,
	// rather than one per round trip through a sandbox.
	recorder := contracts.NewRecorder(t.Context(), "suite")

	recorder.Run("case", func(t contracts.T) {
		t.Errorf("first: %d", 1)
		t.Errorf("second: %d", 2)
	})

	failures := recorder.Report().Cases[0].Failures
	if len(failures) != 2 || failures[0] != "first: 1" || failures[1] != "second: 2" {
		t.Fatalf("failures = %v", failures)
	}
}

func TestAPanicIsAFailedCaseNotACrash(t *testing.T) {
	// A panic in a third-party component is a failure of that component, not
	// of the runner.
	recorder := contracts.NewRecorder(t.Context(), "suite")

	recorder.Run("case", func(contracts.T) { panic("boom") })

	failures := recorder.Report().Cases[0].Failures
	if len(failures) != 1 || !strings.Contains(failures[0], "boom") {
		t.Fatalf("failures = %v", failures)
	}
}

func TestCleanupRunsWhenACaseEndsIncludingAfterFatal(t *testing.T) {
	// A factory that opens a sandbox connection per case and cannot close it
	// leaks one per case.
	recorder := contracts.NewRecorder(t.Context(), "suite")
	var order []string

	recorder.Run("case", func(t contracts.T) {
		t.Cleanup(func() { order = append(order, "first") })
		t.Cleanup(func() { order = append(order, "second") })
		t.Fatal("stop")
	})

	// Reverse order, as testing does, so a teardown can rely on whatever it was
	// registered after still existing.
	if len(order) != 2 || order[0] != "second" || order[1] != "first" {
		t.Fatalf("cleanups ran %v", order)
	}
}

func TestAnEmptyReportHasNotPassed(t *testing.T) {
	// A battery that ran nothing proved nothing. Treating "no failures" as
	// success would admit a component the runner never actually reached.
	recorder := contracts.NewRecorder(t.Context(), "suite")

	if recorder.Report().Passed() {
		t.Fatal("an empty report claimed to pass")
	}
}

func TestTheContextReachesEveryCase(t *testing.T) {
	// The runner's deadline. A component that hangs on one assertion must not
	// hang the runner.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	recorder := contracts.NewRecorder(ctx, "suite")

	recorder.Run("case", func(t contracts.T) {
		if t.Context().Err() == nil {
			t.Error("the case did not receive the runner's cancelled context")
		}
	})

	if !recorder.Report().Cases[0].Passed() {
		t.Fatalf("case = %+v", recorder.Report().Cases[0])
	}
}

func TestNestedCasesAreNamedByTheirPath(t *testing.T) {
	recorder := contracts.NewRecorder(t.Context(), "suite")

	recorder.Run("outer", func(t contracts.T) {
		t.Run("inner", func(contracts.T) {})
	})

	names := make([]string, 0, 2)
	for _, c := range recorder.Report().Cases {
		names = append(names, c.Name)
	}
	if len(names) != 2 || names[0] != "outer/inner" || names[1] != "outer" {
		t.Fatalf("names = %v", names)
	}
}

func TestTheReportNamesWhatFailed(t *testing.T) {
	recorder := contracts.NewRecorder(t.Context(), "suite")
	recorder.Run("passing", func(contracts.T) {})
	recorder.Run("failing", func(t contracts.T) { t.Error("because") })

	rendered := recorder.Report().String()

	if !strings.Contains(rendered, "1/2 cases passed") {
		t.Fatalf("summary missing from:\n%s", rendered)
	}
	for _, want := range []string{"FAIL  failing", "because", "ok    passing"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("%q missing from:\n%s", want, rendered)
		}
	}
}
