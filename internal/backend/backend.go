package backend

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"

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
		var cmd *exec.Cmd
		if cfg.SSH != nil {
			cmd = sshCommand(cfg)
		} else {
			cmd = exec.Command(cfg.Command[0], cfg.Command[1:]...)
			cmd.Dir = cfg.Dir
			cmd.Env = envWithOverrides(cfg.Env)
		}
		transport = &mcp.CommandTransport{Command: cmd}
	case "http":
		base, err := httpBaseTransport(cfg.Proxy)
		if err != nil {
			return nil, fmt.Errorf("backend %q: %w", cfg.Name, err)
		}
		transport = &mcp.StreamableClientTransport{
			Endpoint:   cfg.URL,
			HTTPClient: &http.Client{Transport: headerRoundTripper{headers: cfg.Headers, base: base}},
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

// sshCommand builds the "ssh ... <remote-script>" command that runs cfg.Command
// on cfg.SSH.Host. The local ssh client process inherits the gateway's own
// environment unchanged; cfg.Dir and cfg.Env are instead embedded into the
// remote shell script, since they need to take effect on the remote side.
func sshCommand(cfg config.BackendConfig) *exec.Cmd {
	var args []string
	if cfg.SSH.Port != 0 {
		args = append(args, "-p", strconv.Itoa(cfg.SSH.Port))
	}
	if cfg.SSH.IdentityFile != "" {
		args = append(args, "-i", cfg.SSH.IdentityFile)
	}
	args = append(args, cfg.SSH.Args...)
	args = append(args, cfg.SSH.Host, remoteScript(cfg))
	return exec.Command("ssh", args...)
}

// remoteScript builds a single POSIX shell command string that cds into
// cfg.Dir (if set), exports cfg.Env, then execs cfg.Command.
func remoteScript(cfg config.BackendConfig) string {
	var b strings.Builder
	if cfg.Dir != "" {
		fmt.Fprintf(&b, "cd %s && ", shellQuote(cfg.Dir))
	}

	keys := make([]string, 0, len(cfg.Env))
	for k := range cfg.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "export %s=%s && ", k, shellQuote(cfg.Env[k]))
	}

	quoted := make([]string, len(cfg.Command))
	for i, arg := range cfg.Command {
		quoted[i] = shellQuote(arg)
	}
	b.WriteString("exec ")
	b.WriteString(strings.Join(quoted, " "))
	return b.String()
}

// shellQuote wraps s in single quotes for safe use as one word in a POSIX
// shell command, escaping any single quotes it contains.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// httpBaseTransport returns the RoundTripper an http backend's requests
// should go through, according to proxy:
//   - "" (unset): http.DefaultTransport, which follows HTTP_PROXY,
//     HTTPS_PROXY and NO_PROXY as usual.
//   - "none": a direct connection, ignoring those environment variables.
//   - any other value: a fixed proxy URL.
func httpBaseTransport(proxy string) (http.RoundTripper, error) {
	switch proxy {
	case "":
		return http.DefaultTransport, nil
	case "none":
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.Proxy = nil
		return t, nil
	default:
		proxyURL, err := url.Parse(proxy)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy url: %w", err)
		}
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.Proxy = http.ProxyURL(proxyURL)
		return t, nil
	}
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
