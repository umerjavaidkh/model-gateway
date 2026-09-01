package core

import (
	"errors"
	"fmt"
)

// Code is the stable, machine-readable classification of a gateway failure.
//
// Codes are part of the gateway's public contract: they appear in usage events,
// in metrics, and (mapped by the transport layer) in HTTP status codes. Adding a
// code is cheap; changing the meaning of one is a breaking change.
//
// The mapping from Code to an HTTP status lives in the transport layer, not here.
// core does not know that HTTP exists.
type Code string

const (
	// CodeUnauthenticated means no usable credential was presented.
	CodeUnauthenticated Code = "unauthenticated"
	// CodeForbidden means a valid principal is not permitted to do this.
	CodeForbidden Code = "forbidden"
	// CodeInvalidRequest means the caller sent something malformed.
	CodeInvalidRequest Code = "invalid_request"
	// CodeModelNotFound means the requested alias or model resolved to nothing.
	CodeModelNotFound Code = "model_not_found"
	// CodeNoCandidates means the model exists but no deployment is eligible —
	// every candidate was filtered out by trust tier, health, or capability.
	CodeNoCandidates Code = "no_candidates"
	// CodeTrustTierDenied means the request's data classification forbids every
	// destination that could otherwise serve it. This is a 403, never a fallback.
	CodeTrustTierDenied Code = "trust_tier_denied"
	// CodeRateLimited means an RPM, TPM, or concurrency limit was hit.
	CodeRateLimited Code = "rate_limited"
	// CodeBudgetExhausted means a hard budget in the principal's chain is spent.
	CodeBudgetExhausted Code = "budget_exhausted"
	// CodeEndpointUnsupported means the model exists but is served by an
	// adapter that does not speak the API surface the caller used.
	CodeEndpointUnsupported Code = "endpoint_unsupported"
	// CodeGuardrailDenied means a blocking guardrail rejected the payload.
	CodeGuardrailDenied Code = "guardrail_denied"
	// CodeUpstreamError means the provider returned an error we are relaying.
	CodeUpstreamError Code = "upstream_error"
	// CodeUpstreamTimeout means the provider did not answer within the deadline
	// budget shared across attempts.
	CodeUpstreamTimeout Code = "upstream_timeout"
	// CodeUnavailable means a dependency the gateway needs is down and the
	// configured failure mode is fail-closed.
	CodeUnavailable Code = "unavailable"
	// CodeInternal is a bug in the gateway. It should never be returned for a
	// condition we anticipated.
	CodeInternal Code = "internal"
)

// Sentinels for errors.Is. Comparison is by Code, so
// errors.Is(err, core.ErrForbidden) matches any *Error carrying CodeForbidden,
// regardless of message or wrapped cause.
var (
	ErrUnauthenticated     = &Error{Code: CodeUnauthenticated}
	ErrForbidden           = &Error{Code: CodeForbidden}
	ErrInvalidRequest      = &Error{Code: CodeInvalidRequest}
	ErrModelNotFound       = &Error{Code: CodeModelNotFound}
	ErrNoCandidates        = &Error{Code: CodeNoCandidates}
	ErrTrustTierDenied     = &Error{Code: CodeTrustTierDenied}
	ErrRateLimited         = &Error{Code: CodeRateLimited}
	ErrBudgetExhausted     = &Error{Code: CodeBudgetExhausted}
	ErrEndpointUnsupported = &Error{Code: CodeEndpointUnsupported}
	ErrGuardrailDenied     = &Error{Code: CodeGuardrailDenied}
	ErrUpstreamError       = &Error{Code: CodeUpstreamError}
	ErrUpstreamTimeout     = &Error{Code: CodeUpstreamTimeout}
	ErrUnavailable         = &Error{Code: CodeUnavailable}
	ErrInternal            = &Error{Code: CodeInternal}
)

// Error is the single error type the gateway's own code produces. Errors from
// outside — a driver, a provider SDK — are wrapped into one at the boundary of
// the adapter that produced them, so that no caller ever has to type-switch on a
// vendor's error type.
type Error struct {
	Code    Code
	Message string
	// Retryable says whether the router may try the next candidate. It is set
	// deliberately per error rather than derived from Code, because the same
	// code covers both retryable and terminal upstream conditions: a provider
	// 503 is retryable, a provider 400 is not.
	Retryable bool

	cause error
}

func (e *Error) Error() string {
	switch {
	case e.Message == "" && e.cause == nil:
		return string(e.Code)
	case e.cause == nil:
		return string(e.Code) + ": " + e.Message
	case e.Message == "":
		return string(e.Code) + ": " + e.cause.Error()
	default:
		return string(e.Code) + ": " + e.Message + ": " + e.cause.Error()
	}
}

// Unwrap exposes the wrapped cause to errors.Is and errors.As.
func (e *Error) Unwrap() error { return e.cause }

// Is matches on Code alone, which is what makes the package sentinels useful.
func (e *Error) Is(target error) bool {
	var t *Error
	if !errors.As(target, &t) {
		return false
	}
	return e.Code == t.Code
}

// AsRetryable returns a copy marked retryable. It returns a copy rather than
// mutating, so marking a package sentinel cannot corrupt it for every caller.
func (e *Error) AsRetryable() *Error {
	c := *e
	c.Retryable = true
	return &c
}

// New builds an error with a fixed message.
func New(code Code, message string) *Error {
	return &Error{Code: code, Message: message}
}

// Newf builds an error with a formatted message.
func Newf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Wrap attaches a code and context to an error from outside the gateway.
// It returns nil when cause is nil, so it composes with early returns.
//
// The result is `error`, not `*Error`, deliberately. A function returning a
// typed nil pointer into an `error` result produces a non-nil interface holding
// a nil pointer — an error that is not nil and has no message. Returning the
// interface type makes the nil case a real nil for every caller.
func Wrap(code Code, cause error, message string) error {
	if cause == nil {
		return nil
	}
	return &Error{Code: code, Message: message, cause: cause}
}

// WrapRetryable is Wrap for a condition the router may retry against another
// candidate — a timeout, a refused connection, a provider 503.
//
// It exists so that adapters do not each write a type assertion back to *Error
// in order to reach AsRetryable.
func WrapRetryable(code Code, cause error, message string) error {
	if cause == nil {
		return nil
	}
	return &Error{Code: code, Message: message, Retryable: true, cause: cause}
}

// Wrapf is Wrap with a formatted message.
func Wrapf(code Code, cause error, format string, args ...any) error {
	if cause == nil {
		return nil
	}
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), cause: cause}
}

// CodeOf reports the Code carried by err, following the wrap chain.
// Errors that did not originate here are CodeInternal: an unclassified error is
// a bug in whoever failed to classify it, and reporting it as such makes that
// visible in metrics instead of hiding it behind a plausible-looking code.
func CodeOf(err error) Code {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return CodeInternal
}

// IsRetryable reports whether the router may fall through to the next candidate.
func IsRetryable(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Retryable
}
