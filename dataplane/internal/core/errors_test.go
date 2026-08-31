package core_test

import (
	"errors"
	"testing"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

func TestErrorMatchesSentinelByCode(t *testing.T) {
	err := core.Newf(core.CodeForbidden, "principal %q may not call %q", "k1", "gpt-4")

	if !errors.Is(err, core.ErrForbidden) {
		t.Fatal("expected the error to match the forbidden sentinel")
	}
	if errors.Is(err, core.ErrRateLimited) {
		t.Fatal("expected the error not to match an unrelated sentinel")
	}
}

func TestWrapPreservesCauseAndCode(t *testing.T) {
	cause := errors.New("dial tcp: connection refused")
	err := core.Wrap(core.CodeUpstreamError, cause, "calling bedrock")

	if !errors.Is(err, cause) {
		t.Fatal("expected the cause to stay reachable through the wrap chain")
	}
	if got := core.CodeOf(err); got != core.CodeUpstreamError {
		t.Fatalf("CodeOf = %q, want %q", got, core.CodeUpstreamError)
	}
}

func TestWrapOfNilIsNil(t *testing.T) {
	// Wrap composes with early returns, so a nil cause must stay nil rather than
	// becoming a non-nil error with an empty message.
	if err := core.Wrap(core.CodeInternal, nil, "unreachable"); err != nil {
		t.Fatalf("Wrap(nil) = %v, want nil", err)
	}
}

func TestUnclassifiedErrorReportsAsInternal(t *testing.T) {
	if got := core.CodeOf(errors.New("something from a driver")); got != core.CodeInternal {
		t.Fatalf("CodeOf = %q, want %q", got, core.CodeInternal)
	}
}

func TestAsRetryableDoesNotMutateTheSentinel(t *testing.T) {
	// AsRetryable returns a copy for exactly this reason: marking one error
	// retryable must not make every future upstream error retryable.
	retryable := core.ErrUpstreamError.AsRetryable()

	if !core.IsRetryable(retryable) {
		t.Fatal("expected the copy to be retryable")
	}
	if core.IsRetryable(core.ErrUpstreamError) {
		t.Fatal("the package sentinel was mutated")
	}
}
