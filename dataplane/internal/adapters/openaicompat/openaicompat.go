// Package openaicompat implements ProviderPort for anything speaking the
// OpenAI chat-completions schema.
//
// One adapter serves OpenAI, Azure OpenAI, vLLM, TGI, Ollama, Groq, Together,
// Fireworks, DeepInfra and every self-hosted server that adopted the same
// shape — which is most of them, and all of the self-hosted tier. See
// docs/adr/0004-native-provider-adapters.md for why this is written rather
// than adopted.
//
// The adapter is deliberately thin. It rewrites the model field, attaches
// credentials, and normalizes what comes back; it does not parse or validate
// the payload. Anything a provider accepts that we do not understand is
// forwarded untouched, so a provider adding a parameter does not need a
// gateway release.
package openaicompat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

// Name is how a deployment refers to this adapter in a snapshot.
const Name = "openai-compatible"

// DefaultTimeout bounds a single upstream attempt when the request carries no
// deadline of its own. Generous, because a long completion legitimately takes
// minutes.
const DefaultTimeout = 10 * time.Minute

// maxErrorBodyBytes bounds how much of a provider's error response is read.
// A misconfigured endpoint can return a megabyte of HTML, and none of it
// belongs in a log line.
const maxErrorBodyBytes = 8 << 10

// Provider calls an OpenAI-compatible endpoint.
//
// Safe for concurrent use. The HTTP client is shared so connections are pooled
// across requests, which is where most of the latency saving in a proxy lives.
type Provider struct {
	client *http.Client
}

// Option configures a Provider.
type Option func(*Provider)

// WithHTTPClient replaces the client, for tests and for callers that need
// custom transport settings such as a proxy or a client certificate.
func WithHTTPClient(c *http.Client) Option {
	return func(p *Provider) { p.client = c }
}

// New returns a provider with pooled, keep-alive HTTP.
func New(opts ...Option) *Provider {
	p := &Provider{client: &http.Client{Timeout: DefaultTimeout}}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Name identifies the adapter.
func (*Provider) Name() string { return Name }

// Endpoints reports the one surface this adapter speaks. Translating an
// Anthropic-shaped request into this schema is a separate feature, not
// something to do implicitly and get subtly wrong.
func (*Provider) Endpoints() []core.Endpoint {
	return []core.Endpoint{core.EndpointChatCompletions}
}

// Invoke makes a non-streaming call.
func (p *Provider) Invoke(ctx context.Context, call *core.ProviderCall) (*core.ProviderResponse, error) {
	resp, err := p.send(ctx, call, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= http.StatusBadRequest {
		return nil, upstreamError(resp)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, core.Wrap(core.CodeUpstreamError, err, "reading the provider response")
	}
	return &core.ProviderResponse{
		StatusCode: resp.StatusCode,
		Body:       body,
		Usage:      usageFrom(body),
	}, nil
}

// Stream makes a streaming call and returns a cursor over the normalized
// chunks.
func (p *Provider) Stream(ctx context.Context, call *core.ProviderCall) (core.ChunkStream, error) {
	resp, err := p.send(ctx, call, true)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		defer func() { _ = resp.Body.Close() }()
		return nil, upstreamError(resp)
	}
	return newStream(resp.Body), nil
}

func (p *Provider) send(ctx context.Context, call *core.ProviderCall, stream bool) (*http.Response, error) {
	body, err := prepareBody(call, stream)
	if err != nil {
		return nil, err
	}

	url := strings.TrimSuffix(call.Deployment.Endpoint, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, core.Wrap(core.CodeInvalidRequest, err, "building the upstream request")
	}
	req.Header.Set("Content-Type", "application/json")
	if len(call.Credential.Secret) > 0 {
		req.Header.Set("Authorization", "Bearer "+string(call.Credential.Secret))
	}
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}

	resp, err := p.client.Do(req)
	if err != nil {
		// A timeout and a refused connection are both retryable against
		// another deployment; a malformed URL is not. context.DeadlineExceeded
		// is separated so the caller can report a timeout rather than a
		// generic upstream failure.
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, core.WrapRetryable(core.CodeUpstreamTimeout, err, "provider did not respond in time")
		}
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		return nil, core.WrapRetryable(core.CodeUpstreamError, err, "calling the provider")
	}
	return resp, nil
}

// prepareBody rewrites the model field to the deployment's real model id and
// sets the stream flag to match what the gateway is doing.
//
// The caller asked for an alias like "fast"; the provider has never heard of
// it. Rewriting is done by decoding into a map rather than a struct so that
// every other field the caller sent is preserved byte-for-byte in meaning,
// including ones this gateway has never heard of.
func prepareBody(call *core.ProviderCall, stream bool) ([]byte, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(call.Body, &payload); err != nil {
		return nil, core.Wrap(core.CodeInvalidRequest, err, "the request body is not a JSON object")
	}

	model, err := json.Marshal(call.Deployment.Key.BaseModel)
	if err != nil {
		return nil, core.Wrap(core.CodeInternal, err, "encoding the model name")
	}
	payload["model"] = model
	payload["stream"] = json.RawMessage("false")
	if stream {
		payload["stream"] = json.RawMessage("true")
		// Ask for usage on the final chunk. Providers that do not understand
		// the option ignore it; those that do save us estimating token counts.
		payload["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	}

	out, err := json.Marshal(payload)
	if err != nil {
		return nil, core.Wrap(core.CodeInternal, err, "re-encoding the request body")
	}
	return out, nil
}

// upstreamError turns a provider error response into a gateway error.
//
// 429 and 5xx are retryable against another deployment; a 4xx is the caller's
// fault and retrying it just burns the deadline budget on the same rejection.
func upstreamError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	detail := strings.TrimSpace(string(body))
	if detail == "" {
		detail = resp.Status
	}

	err := core.Newf(core.CodeUpstreamError, "provider returned %d: %s", resp.StatusCode, detail)
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError {
		return err.AsRetryable()
	}
	return err
}

// usage is the shape providers report token counts in.
//
// prompt_tokens is the *total* input including anything served from cache, and
// prompt_tokens_details.cached_tokens is a subset of it. Anthropic reports the
// opposite convention, which is exactly why TokenUsage normalizes: downstream
// code should not have to know which provider it came from.
type usage struct {
	Usage struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		PromptDetails    struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

// usageFrom extracts reported usage, returning zero when the provider omits it.
//
// Zero is meaningful: accounting treats reported and estimated usage
// differently, and silently substituting an estimate here would erase that
// distinction at the only point where it is knowable.
func usageFrom(body []byte) core.TokenUsage {
	var u usage
	if err := json.Unmarshal(body, &u); err != nil {
		return core.TokenUsage{}
	}

	cached := u.Usage.PromptDetails.CachedTokens
	// Subtracted because prompt_tokens includes the cached ones. Clamped
	// because a provider reporting more cached than total is malfunctioning,
	// and a negative input count would surface as a credit on an invoice.
	standard := max(u.Usage.PromptTokens-cached, 0)

	return core.TokenUsage{
		Input:       standard,
		CachedInput: cached,
		Output:      u.Usage.CompletionTokens,
	}
}

// --- streaming --------------------------------------------------------------

const (
	ssePrefix = "data: "
	sseDone   = "[DONE]"
	// maxSSELineBytes bounds one event. A single chunk is a few hundred bytes;
	// anything approaching this is a malfunctioning upstream, and without a
	// bound bufio would grow until the worker is killed.
	maxSSELineBytes = 1 << 20
)

// stream reads server-sent events and yields normalized chunks.
type stream struct {
	body    io.ReadCloser
	scanner *bufio.Scanner

	mu sync.Mutex
	// closed is set by Close; finished is set once the terminating chunk has
	// been handed out. They are separate because a caller may Close early.
	closed   bool
	finished bool
	usage    core.TokenUsage
}

func newStream(body io.ReadCloser) *stream {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64<<10), maxSSELineBytes)
	return &stream{body: body, scanner: scanner}
}

// Next returns the next chunk, or io.EOF at the end of the stream.
//
// The gateway relays SSE payloads rather than re-encoding them, so a client
// library that understands the provider's stream understands the gateway's.
// What is normalized is the framing: where the stream ends, and where usage is
// reported.
func (s *stream) Next(ctx context.Context) (core.Chunk, error) {
	if err := ctx.Err(); err != nil {
		return core.Chunk{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed || s.finished {
		return core.Chunk{}, io.EOF
	}

	for s.scanner.Scan() {
		line := strings.TrimSpace(s.scanner.Text())
		// Blank lines separate events and comments begin with a colon; both are
		// SSE framing, not payload.
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		payload, ok := strings.CutPrefix(line, ssePrefix)
		if !ok {
			continue
		}
		if payload == sseDone {
			// The terminating chunk is returned with a nil error, and io.EOF
			// comes on the call after it. Returning both at once is the classic
			// Go iterator trap: callers write `if err == io.EOF { break }` and
			// silently drop the value, which here would lose the token usage.
			s.finished = true
			return core.Chunk{Final: true, Usage: s.usage}, nil
		}
		// Usage arrives on a late chunk whose choices array is empty, so every
		// chunk is inspected and the last reported value wins.
		if u := usageFrom([]byte(payload)); u.Input != 0 || u.Output != 0 {
			s.usage = u
		}
		return core.Chunk{Body: []byte(payload)}, nil
	}

	if err := s.scanner.Err(); err != nil {
		return core.Chunk{}, core.Wrap(core.CodeUpstreamError, err, "reading the provider stream")
	}
	// The upstream closed without sending [DONE]. That is a truncated response,
	// not a clean end: reporting it as EOF would let a half-finished completion
	// look complete to the caller and to accounting.
	return core.Chunk{}, core.New(core.CodeUpstreamError, "provider stream ended without a terminator")
}

// Close releases the upstream connection. Safe to call more than once.
func (s *stream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil
	}
	s.closed = true
	if err := s.body.Close(); err != nil {
		return core.Wrap(core.CodeUpstreamError, err, "closing the provider stream")
	}
	return nil
}
