package echo_test

import (
	"errors"
	"io"
	"testing"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/adapters/echo"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/contracts"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

func sampleCall(t *testing.T) *core.ProviderCall {
	t.Helper()
	return &core.ProviderCall{
		Deployment: core.Deployment{
			ID: "echo-1", Key: core.RoutingKey{BaseModel: "echo"},
			Provider: "echo", TrustTier: core.TrustInternal, Weight: 100,
		},
		Meta: core.RequestMeta{RequestID: "req-1", Model: "echo", Endpoint: core.EndpointChatCompletions},
		Body: []byte(`{"messages":[{"role":"user","content":"hello world"}]}`),
	}
}

func TestEchoSatisfiesProviderPort(t *testing.T) {
	contracts.RunProviderSuite(t,
		func(*testing.T) core.ProviderPort { return echo.New() },
		sampleCall,
	)
}

func TestStreamReassemblesToTheRequestBody(t *testing.T) {
	call := sampleCall(t)
	stream, err := echo.New().Stream(t.Context(), call)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	var got []byte
	var usage core.TokenUsage
	for {
		chunk, err := stream.Next(t.Context())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		got = append(got, chunk.Body...)
		if chunk.Final {
			usage = chunk.Usage
		}
	}

	if string(got) != string(call.Body) {
		t.Fatalf("reassembled %q, want %q", got, call.Body)
	}
	if usage.Input == 0 {
		t.Fatal("the final chunk must carry usage")
	}
}

func TestInvokeDoesNotAliasTheRequestBody(t *testing.T) {
	// The router may retry with the same call against another deployment, so an
	// adapter that hands back a slice of the caller's buffer would let a
	// response mutation corrupt the next attempt.
	call := sampleCall(t)
	resp, err := echo.New().Invoke(t.Context(), call)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	resp.Body[0] = 'X'
	if call.Body[0] == 'X' {
		t.Fatal("the response body aliases the request body")
	}
}
