package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

// statusByCode maps the gateway's error taxonomy onto HTTP.
//
// This table is the only place in the codebase that knows both, which is what
// keeps core free of HTTP. A code missing from the table is a 500 by design:
// an unmapped error is a bug in whoever added the code, and reporting it as a
// server error makes that visible instead of hiding it behind a plausible 4xx.
var statusByCode = map[core.Code]int{
	core.CodeUnauthenticated: http.StatusUnauthorized,
	core.CodeForbidden:       http.StatusForbidden,
	core.CodeTrustTierDenied: http.StatusForbidden,
	core.CodeInvalidRequest:  http.StatusBadRequest,
	core.CodeModelNotFound:   http.StatusNotFound,
	// The model exists, but not on the surface the caller used. 404 rather than
	// 400: from the caller's side this route does not serve that model, which
	// is what a 404 means, and it matches how a missing model already behaves.
	core.CodeEndpointUnsupported: http.StatusNotFound,
	core.CodeNoCandidates:        http.StatusServiceUnavailable,
	core.CodeRateLimited:         http.StatusTooManyRequests,
	core.CodeBudgetExhausted:     http.StatusPaymentRequired,
	core.CodeGuardrailDenied:     http.StatusUnprocessableEntity,
	core.CodeUpstreamError:       http.StatusBadGateway,
	core.CodeUpstreamTimeout:     http.StatusGatewayTimeout,
	core.CodeUnavailable:         http.StatusServiceUnavailable,
	core.CodeInternal:            http.StatusInternalServerError,
}

// errorBody is the OpenAI-compatible error envelope, so existing client
// libraries surface gateway errors the same way they surface provider ones.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
	// RequestID lets a user paste one string into a support request and have it
	// match a trace, a usage record and an audit record.
	RequestID string `json:"request_id,omitempty"`
}

func writeError(w http.ResponseWriter, logger *slog.Logger, requestID string, err error) {
	code := core.CodeOf(err)
	status, ok := statusByCode[code]
	if !ok {
		status = http.StatusInternalServerError
	}

	// The caller sees the code and a message; the operator sees the whole chain.
	// Wrapped causes can carry an upstream URL or a credential reference, so
	// they stay server-side.
	logger.Error("request failed",
		slog.String("request_id", requestID),
		slog.String("code", string(code)),
		slog.Int("status", status),
		slog.String("error", err.Error()))

	// The message alone, not err.Error(): the code has its own field, and the
	// wrap chain can carry an upstream URL or a credential reference.
	message := err.Error()
	var gwErr *core.Error
	if errors.As(err, &gwErr) && gwErr.Message != "" {
		message = gwErr.Message
	}
	if status >= http.StatusInternalServerError {
		message = "internal error"
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Error: errorDetail{
		Message:   message,
		Type:      typeForStatus(status),
		Code:      string(code),
		RequestID: requestID,
	}})
}

func typeForStatus(status int) string {
	if status >= http.StatusInternalServerError {
		return "server_error"
	}
	return "invalid_request_error"
}
