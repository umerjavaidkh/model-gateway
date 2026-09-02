package admission

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

// Client talks to the control plane's registry API.
//
// One direction only: it fetches a manifest and posts a verdict. The runner has
// no ability to activate anything — activation is the control plane's decision,
// made from the verdict, and keeping that asymmetry is the point of running the
// suite somewhere else at all.
type Client struct {
	base   *url.URL
	token  string
	client *http.Client
}

// NewClient returns a client for the control plane at baseURL.
func NewClient(baseURL, token string, timeout time.Duration) (*Client, error) {
	if baseURL == "" {
		return nil, core.New(core.CodeInvalidRequest, "a control-plane URL is required")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, core.Wrap(core.CodeInvalidRequest, err, "parsing the control-plane URL")
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{base: parsed, token: token, client: &http.Client{Timeout: timeout}}, nil
}

// Manifest fetches a registered component's manifest.
func (c *Client) Manifest(ctx context.Context, name, version string) (Manifest, error) {
	var manifest Manifest
	err := c.do(ctx, http.MethodGet, c.path("v1", "components", name, version), nil, &manifest)
	return manifest, err
}

// Report posts a verdict for a named component version.
//
// The control plane checks the verdict's digest against what it has registered,
// so a run against a stale copy of the manifest is rejected there rather than
// trusted here.
func (c *Client) Report(ctx context.Context, name, version string, verdict Verdict) error {
	return c.do(ctx, http.MethodPost,
		c.path("v1", "components", name, version, "admissions"), verdict, nil)
}

func (c *Client) path(segments ...string) string {
	escaped := make([]string, 0, len(segments))
	for _, segment := range segments {
		escaped = append(escaped, url.PathEscape(segment))
	}
	return c.base.JoinPath(escaped...).String()
}

func (c *Client) do(ctx context.Context, method, target string, body, out any) error {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return core.Wrap(core.CodeInternal, err, "encoding the request")
		}
		payload = bytes.NewReader(encoded)
	}

	request, err := http.NewRequestWithContext(ctx, method, target, payload)
	if err != nil {
		return core.Wrap(core.CodeInternal, err, "building the request")
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}

	response, err := c.client.Do(request)
	if err != nil {
		return core.Wrap(core.CodeUnavailable, err, "calling the control plane")
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode >= http.StatusBadRequest {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return core.Newf(codeFor(response.StatusCode),
			"the control plane reported %d: %s",
			response.StatusCode, strings.TrimSpace(string(detail)))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(out); err != nil {
		return core.Wrap(core.CodeUnavailable, err, "decoding the control-plane response")
	}
	return nil
}

// codeFor maps an HTTP status onto the data plane's vocabulary.
//
// Coarse on purpose: a 404 and a 409 from the registry are both "the runner
// asked for something the control plane will not do", and the status itself is
// already in the message. Adding codes to core for a tool that runs beside the
// control plane would widen a taxonomy the request path shares.
func codeFor(status int) core.Code {
	if status < http.StatusInternalServerError {
		return core.CodeInvalidRequest
	}
	return core.CodeUnavailable
}
