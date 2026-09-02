package wasmguardrail_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/adapters/wasmguardrail"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/contracts"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/wasm"
)

var (
	guestOnce  sync.Once
	guestBytes []byte
	guestErr   error
)

// guest compiles the fixture once for the package. Built rather than committed:
// a checked-in .wasm is a thing nobody rebuilds and nobody can read, so it
// drifts from the source beside it without anyone noticing.
func guest(t *testing.T) []byte {
	t.Helper()

	guestOnce.Do(func() {
		out := filepath.Join(t.TempDir(), "guest.wasm")
		cmd := exec.CommandContext(context.Background(),
			"go", "build", "-buildmode=c-shared", "-o", out, ".")
		cmd.Dir = "testdata/guest"
		cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
		if output, err := cmd.CombinedOutput(); err != nil {
			guestErr = err
			t.Logf("building the guest: %v\n%s", err, output)
			return
		}
		guestBytes, guestErr = os.ReadFile(out)
	})
	if guestErr != nil {
		t.Fatalf("the test guest could not be built: %v", guestErr)
	}
	return guestBytes
}

var (
	sharedOnce      sync.Once
	sharedGuardrail *wasmguardrail.Guardrail
	sharedErr       error
)

// build returns the guardrail every test shares.
//
// One runtime and one compiled module for the package, which is also how a
// worker uses them: compiling costs hundreds of milliseconds and a compiled
// module is reusable, so it happens once at load rather than per call. Sharing
// it here keeps the suite honest about that and takes the package from minutes
// to seconds.
func build(t *testing.T) *wasmguardrail.Guardrail {
	t.Helper()

	sharedOnce.Do(func() {
		runtime, err := wasm.NewRuntime(context.Background(), wasm.Limits{})
		if err != nil {
			sharedErr = err
			return
		}
		module, err := runtime.Compile(context.Background(), "wasm-guard", guest(t))
		if err != nil {
			sharedErr = err
			return
		}
		sharedGuardrail, sharedErr = wasmguardrail.New("wasm-guard", module)
	})
	if sharedErr != nil {
		t.Fatalf("preparing the guardrail: %v", sharedErr)
	}
	return sharedGuardrail
}

func input(payload string) *core.GuardrailInput {
	return &core.GuardrailInput{
		Phase:   core.PhaseRequest,
		Meta:    core.RequestMeta{RequestID: "req-1", Model: "test-model"},
		Class:   core.DataClassInternal,
		Payload: []byte(payload),
	}
}

// TestSatisfiesGuardrailPort is the point of the whole module: the battery that
// gates a built-in guardrail and a sidecar gates a WASM component too, with no
// second copy of it and no allowance for the execution mode.
func TestSatisfiesGuardrailPort(t *testing.T) {
	guardrail := build(t)

	contracts.RunGuardrailSuite(contracts.Adapt(t), func(contracts.T) contracts.GuardrailTarget {
		return contracts.GuardrailTarget{
			Guardrail: guardrail,
			Trigger:   []byte(`{"messages":[{"content":"deploy with AKIAIOSFODNN7EXAMPLE"}]}`),
			Benign:    []byte(`{"messages":[{"content":"summarise this quarter"}]}`),
		}
	})
}

func TestVerdictsAreTranslated(t *testing.T) {
	guardrail := build(t)

	for _, tc := range []struct {
		name    string
		payload string
		want    core.Verdict
	}{
		{"allow", "nothing interesting", core.VerdictAllow},
		{"deny", "AKIAIOSFODNN7EXAMPLE", core.VerdictDeny},
		{"mutate", "BEHAVIOUR:mutate", core.VerdictMutate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := guardrail.Inspect(t.Context(), input(tc.payload))
			if err != nil {
				t.Fatalf("Inspect: %v", err)
			}
			if result.Verdict != tc.want {
				t.Fatalf("verdict = %v, want %v", result.Verdict, tc.want)
			}
		})
	}
}

func TestAMutateReturnsTheRewrittenPayload(t *testing.T) {
	result, err := build(t).Inspect(t.Context(), input("keep BEHAVIOUR:mutate keep"))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}

	if string(result.Payload) != "keep [redacted] keep" {
		t.Fatalf("payload = %q", result.Payload)
	}
}

func TestTheRequestCarriesThePhaseAndModel(t *testing.T) {
	// The wire shape a guest in another language has to implement. Asserted
	// here so a change to it is a change to this test rather than a silent
	// break for every component already compiled against it.
	in := input("BEHAVIOUR:echo-phase")
	in.Phase = core.PhaseResponse

	result, err := build(t).Inspect(t.Context(), in)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}

	if result.Reason != "response/test-model" {
		t.Fatalf("reason = %q, want the phase and model the host sent", result.Reason)
	}
}

func TestAComponentThatBreaksTheProtocolIsRefused(t *testing.T) {
	guardrail := build(t)

	for _, tc := range []struct {
		name    string
		payload string
		why     string
	}{
		{
			"an unknown verdict", "BEHAVIOUR:unknown-verdict",
			"host and guest disagree about the protocol, and resolving that in " +
				"favour of letting the request through is the wrong default",
		},
		{
			"mutate with no payload", "BEHAVIOUR:mutate-without-payload",
			"the chain would forward the original, so the rewrite silently does not happen",
		},
		{
			"a payload on an allow", "BEHAVIOUR:payload-on-allow",
			"it would be returned and never applied",
		},
		{"a response that is not JSON", "BEHAVIOUR:not-json", "nothing can be read from it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := guardrail.Inspect(t.Context(), input(tc.payload)); err == nil {
				t.Fatalf("accepted: %s", tc.why)
			}
		})
	}
}

func TestTheNameComesFromTheBindingNotTheModule(t *testing.T) {
	// A snapshot binds by name. Asking untrusted code what it is called would
	// let it answer with someone else's binding.
	if got := build(t).Name(); got != "wasm-guard" {
		t.Fatalf("Name = %q", got)
	}
}

func TestNewRequiresANameAndAModule(t *testing.T) {
	if _, err := wasmguardrail.New("", nil); err == nil {
		t.Fatal("a nameless guardrail cannot be bound, so it must not construct")
	}
	if _, err := wasmguardrail.New("g", nil); err == nil {
		t.Fatal("a guardrail with no module would fail every call while looking configured")
	}
}

func TestACancelledContextDoesNotReachTheGuest(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := build(t).Inspect(ctx, input("anything"))

	if err == nil {
		t.Fatal("a cancelled call returned a verdict")
	}
	if !strings.Contains(err.Error(), "wasm-guard") {
		t.Fatalf("err = %v, want it to name the module", err)
	}
}
