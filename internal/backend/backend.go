package backend

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wtnb75/mcprt/internal/config"
)

// Backend is a live connection to one backend MCP server.
type Backend struct {
	Name    string
	Prefix  string
	Session *mcp.ClientSession
}

// Connect starts (for stdio) or dials (for http) the backend described by
// cfg, and performs the MCP initialize handshake.
func Connect(ctx context.Context, cfg config.BackendConfig) (*Backend, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "mcprt", Version: "v1"}, nil)

	var transport mcp.Transport
	switch cfg.Transport {
	case "stdio":
		cmd := exec.Command(cfg.Command[0], cfg.Command[1:]...)
		cmd.Env = envWithOverrides(cfg.Env)
		transport = &mcp.CommandTransport{Command: cmd}
	case "http":
		transport = &mcp.StreamableClientTransport{
			Endpoint:   cfg.URL,
			HTTPClient: &http.Client{Transport: headerRoundTripper{headers: cfg.Headers, base: http.DefaultTransport}},
		}
	default:
		return nil, fmt.Errorf("backend %q: unknown transport %q", cfg.Name, cfg.Transport)
	}

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("backend %q: connect: %w", cfg.Name, err)
	}

	return &Backend{Name: cfg.Name, Prefix: cfg.Prefix, Session: session}, nil
}

// ListTools fetches the backend's full tool list, following pagination.
func (b *Backend) ListTools(ctx context.Context) ([]*mcp.Tool, error) {
	var tools []*mcp.Tool
	for t, err := range b.Session.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("backend %q: list tools: %w", b.Name, err)
		}
		tools = append(tools, t)
	}
	return tools, nil
}

// Close closes the connection to the backend.
func (b *Backend) Close() error {
	return b.Session.Close()
}

// envWithOverrides returns the current process environment plus extra,
// suitable for exec.Cmd.Env (a stdio backend subprocess should inherit the
// gateway's environment, not replace it).
func envWithOverrides(extra map[string]string) []string {
	env := os.Environ()
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

// headerRoundTripper injects fixed headers (e.g. an Authorization token)
// into every outgoing request to a remote HTTP backend.
type headerRoundTripper struct {
	headers map[string]string
	base    http.RoundTripper
}

func (h headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	return h.base.RoundTrip(req)
}
