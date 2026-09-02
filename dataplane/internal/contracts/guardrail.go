package contracts

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

// GuardrailTarget is a guardrail to run the suite against.
type GuardrailTarget struct {
	Guardrail core.GuardrailPort
	// Trigger is a payload this guardrail is expected to act on: deny it or
	// rewrite it. Supplied by the publisher, because only they know what their
	// guardrail is for — a secret scanner and a content policy have nothing in
	// common except this interface.
	//
	// Leave it nil for a guardrail that only ever allows. The suite then skips
	// the cases about acting, and says so, rather than asserting a behaviour
	// the component never claimed.
	Trigger []byte
	// Benign is a payload this guardrail must not act on. Required: a
	// guardrail that denies everything passes every assertion about denying
	// and is useless, and this is the case that catches it.
	Benign []byte
}

// GuardrailFactory builds the guardrail under test.
type GuardrailFactory func(t T) GuardrailTarget

// RunGuardrailSuite asserts the behaviour every GuardrailPort must have.
//
// The chain runs these things inline, on real traffic, with the power to refuse
// it. That is a lot of authority to hand to a component the gateway did not
// write, so the battery is mostly about what a guardrail must *not* do: mutate
// without saying so, hold a request past its budget, return a payload it was
// not asked to return, or behave differently on the same input twice.
func RunGuardrailSuite(t T, newGuardrail GuardrailFactory) {
	t.Helper()

	t.Run("reports a stable non-empty name", func(t T) {
		// The name is how a snapshot binds to it. One that changes between
		// constructions silently unbinds the component on the next worker
		// restart.
		first := newGuardrail(t).Guardrail
		second := newGuardrail(t).Guardrail

		if first.Name() == "" {
			t.Fatal("a guardrail with no name cannot be bound")
		}
		if first.Name() != second.Name() {
			t.Fatalf("name is not stable: %q then %q", first.Name(), second.Name())
		}
	})

	t.Run("allows a benign payload unchanged", func(t T) {
		target := newGuardrail(t)
		result := inspect(t, target.Guardrail, input(target.Benign))

		if result.Verdict != core.VerdictAllow {
			t.Fatalf("verdict = %v on a benign payload (%q), want allow", result.Verdict, result.Reason)
		}
	})

	t.Run("an allow verdict carries no payload", func(t T) {
		// The chain substitutes the payload only on mutate. A guardrail
		// returning one on allow is either wasting a copy or expecting a
		// rewrite that will never be applied — and the second is a control
		// that silently does nothing.
		target := newGuardrail(t)
		result := inspect(t, target.Guardrail, input(target.Benign))

		if result.Verdict == core.VerdictAllow && result.Payload != nil {
			t.Fatalf("allow returned a payload of %d bytes; only mutate may", len(result.Payload))
		}
	})

	t.Run("is deterministic on the same input", func(t T) {
		// A guardrail that answers differently on identical input makes every
		// refusal unreproducible, and a refusal nobody can reproduce is one
		// nobody can appeal.
		target := newGuardrail(t)
		in := input(target.Benign)

		first := inspect(t, target.Guardrail, in)
		second := inspect(t, target.Guardrail, in)

		if first.Verdict != second.Verdict {
			t.Fatalf("same input gave %v then %v", first.Verdict, second.Verdict)
		}
	})

	t.Run("does not modify the payload it was given", func(t T) {
		// The chain passes a slice of the live request. A guardrail writing
		// through it edits the request for every stage after it, including the
		// ones that already ran.
		target := newGuardrail(t)
		payload := append([]byte(nil), target.Benign...)
		before := append([]byte(nil), payload...)

		inspect(t, target.Guardrail, input(payload))

		if !bytes.Equal(payload, before) {
			t.Fatalf("the input buffer was modified in place: %q became %q", before, payload)
		}
	})

	t.Run("acts on a payload it is meant to act on", func(t T) {
		target := newGuardrail(t)
		if target.Trigger == nil {
			t.Logf("no trigger payload supplied; this guardrail claims only to allow")
			return
		}

		result := inspect(t, target.Guardrail, input(target.Trigger))
		switch result.Verdict {
		case core.VerdictDeny:
			if strings.TrimSpace(result.Reason) == "" {
				// A refusal with no reason is one an operator cannot explain to
				// the caller it just refused.
				t.Fatal("denied without a reason")
			}
		case core.VerdictMutate:
			if result.Payload == nil {
				t.Fatal("mutate returned no payload, so the chain would forward the original")
			}
			if bytes.Equal(result.Payload, target.Trigger) {
				t.Fatal("mutate returned the input unchanged")
			}
		case core.VerdictAllow:
			t.Fatalf("allowed the payload it declared it acts on: %q", truncate(target.Trigger))
		}
	})

	t.Run("respects a cancelled context", func(t T) {
		// The chain cancels when the budget is spent. A guardrail that ignores
		// it keeps running after the request it was inspecting has gone.
		target := newGuardrail(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err := target.Guardrail.Inspect(ctx, input(target.Benign))
		if err == nil {
			return // Answering instantly from a cheap check is legitimate.
		}
		if !errors.Is(err, context.Canceled) && core.CodeOf(err) != core.CodeUnavailable {
			t.Fatalf("err = %v (code %s), want a cancellation", err, core.CodeOf(err))
		}
	})

	t.Run("returns within a short deadline", func(t T) {
		// The chain enforces the timeout, so a slow guardrail cannot hang a
		// request. What it cannot do is make a guardrail that ignores its
		// deadline stop consuming a worker's goroutines, which is why this is
		// part of admission rather than left to the caller.
		target := newGuardrail(t)
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()

		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = target.Guardrail.Inspect(ctx, input(target.Benign))
		}()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("did not return within 5s on a 2s deadline")
		}
	})

	t.Run("handles an empty payload without failing", func(t T) {
		// A request body can legitimately be empty, and the chain does not
		// special-case it. A guardrail that panics or errors here refuses real
		// traffic for a reason unrelated to what it inspects.
		target := newGuardrail(t)

		result, err := target.Guardrail.Inspect(t.Context(), input(nil))
		if err != nil {
			t.Fatalf("Inspect on an empty payload: %v", err)
		}
		if result == nil {
			t.Fatal("returned no result and no error")
		}
	})
}

func input(payload []byte) *core.GuardrailInput {
	return &core.GuardrailInput{
		Phase:   core.PhaseRequest,
		Meta:    core.RequestMeta{RequestID: "contract-1", Model: "contract-model"},
		Class:   core.DataClassInternal,
		Tier:    core.TrustExternal,
		Payload: payload,
	}
}

func inspect(t T, guardrail core.GuardrailPort, in *core.GuardrailInput) *core.GuardrailResult {
	t.Helper()

	result, err := guardrail.Inspect(t.Context(), in)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if result == nil {
		t.Fatal("returned no result and no error")
	}
	return result
}

func truncate(payload []byte) string {
	const limit = 64
	if len(payload) <= limit {
		return string(payload)
	}
	return string(payload[:limit]) + "..."
}
