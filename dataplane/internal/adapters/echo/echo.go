// Package echo implements ProviderPort against nothing at all: it returns the
// request body back to the caller.
//
// It exists so that the request path can be exercised end to end — auth,
// admission, routing, accounting, streaming — without a network, a credential,
// or a provider account. Every stage after this one is developed against it
// before a real provider adapter exists, and it stays afterwards as the fixture
// the contract suite and the load harness run against.
package echo

import (
	"context"
	"io"
	"sync"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

// ChunkSize is how many bytes of the echoed body each stream chunk carries.
const ChunkSize = 16

// Provider is a stateless ProviderPort. The zero value is ready to use and is
// safe for concurrent use.
type Provider struct{}

// New returns an echo provider.
func New() *Provider { return &Provider{} }

// Name identifies the adapter in a snapshot's deployment records.
func (*Provider) Name() string { return "echo" }

// Invoke returns the request body unchanged.
func (*Provider) Invoke(ctx context.Context, call *core.ProviderCall) (*core.ProviderResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	body := make([]byte, len(call.Body))
	copy(body, call.Body)
	return &core.ProviderResponse{
		StatusCode: 200,
		Body:       body,
		Usage:      estimateUsage(call.Body),
	}, nil
}

// Stream returns the request body in fixed-size chunks.
func (*Provider) Stream(ctx context.Context, call *core.ProviderCall) (core.ChunkStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	body := make([]byte, len(call.Body))
	copy(body, call.Body)
	return &stream{body: body, usage: estimateUsage(call.Body)}, nil
}

// estimateUsage stands in for real provider-reported usage. Four bytes per token
// is the usual rough English ratio; it is a placeholder, and accounting treats
// estimated usage differently from reported usage.
func estimateUsage(body []byte) core.TokenUsage {
	tokens := int64(len(body)+3) / 4
	return core.TokenUsage{Input: tokens, Output: tokens}
}

type stream struct {
	mu     sync.Mutex
	body   []byte
	offset int
	usage  core.TokenUsage
	closed bool
}

// Next returns the next chunk of the echoed body, or io.EOF once it is spent.
func (s *stream) Next(ctx context.Context) (core.Chunk, error) {
	if err := ctx.Err(); err != nil {
		return core.Chunk{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed || s.offset >= len(s.body) {
		return core.Chunk{}, io.EOF
	}

	end := min(s.offset+ChunkSize, len(s.body))
	chunk := core.Chunk{Body: s.body[s.offset:end]}
	s.offset = end

	if s.offset >= len(s.body) {
		chunk.Final = true
		chunk.Usage = s.usage
	}
	return chunk, nil
}

// Close marks the stream spent. It is safe to call more than once.
func (s *stream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}
