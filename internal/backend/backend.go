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
	"go.opentelemetry.io/otel/propagation"

	"github.com/wtnb75/mcprt/internal/config"
)

// Backend is a live connection to one backend MCP server.
type Backend struct {
	Name    string
	Prefix  string
	Session *mcp.ClientSession
}

// ChangeCallbacks are invoked when a connected backend reports that its
// tool/prompt/resource list has changed. Each func takes no arguments: MCP's
// list_changed notifications carry no payload, they only signal "go
// re-list." A nil field means "not interested" and leaves the corresponding
// SDK handler unset.
type ChangeCallbacks struct {
	OnToolsChanged     func()
	OnPromptsChanged   func()
	OnResourcesChanged func() // fires for notifications/resources/list_changed, which covers BOTH resources and resource templates per the MCP spec -- there is no separate resource-template notification
}

// Connect starts (for stdio) or dials (for http) the backend described by
// cfg, and performs the MCP initialize handshake. cb's non-nil fields are
// wired as the corresponding notification handlers on the client, so the
// caller finds out when the backend's tool/prompt/resource list changes.
func Connect(ctx context.Context, cfg config.BackendConfig, cb ChangeCallbacks) (*Backend, error) {
	clientOpts := &mcp.ClientOptions{}
	if cb.OnToolsChanged != nil {
		clientOpts.ToolListChangedHandler = func(context.Context, *mcp.ToolListChangedRequest) { cb.OnToolsChanged() }
	}
	if cb.OnPromptsChanged != nil {
		clientOpts.PromptListChangedHandler = func(context.Context, *mcp.PromptListChangedRequest) { cb.OnPromptsChanged() }
	}
	if cb.OnResourcesChanged != nil {
		clientOpts.ResourceListChangedHandler = func(context.Context, *mcp.ResourceListChangedRequest) { cb.OnResourcesChanged() }
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "mcprt", Version: "v1"}, clientOpts)

	var transport mcp.Transport
	switch cfg.Transport {
	case "stdio":
		var cmd *exec.Cmd
		switch {
		case cfg.SSH != nil:
			cmd = sshCommand(cfg)
		case cfg.Docker != nil:
			cmd = dockerCommand(cfg)
		default:
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
			HTTPClient: &http.Client{Transport: tracingRoundTripper{base: headerRoundTripper{headers: cfg.Headers, base: base}}},
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

// ListResources fetches the backend's full resource list, following
// pagination.
func (b *Backend) ListResources(ctx context.Context) ([]*mcp.Resource, error) {
	var resources []*mcp.Resource
	for r, err := range b.Session.Resources(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("backend %q: list resources: %w", b.Name, err)
		}
		resources = append(resources, r)
	}
	return resources, nil
}

// ListResourceTemplates fetches the backend's full resource template list,
// following pagination.
func (b *Backend) ListResourceTemplates(ctx context.Context) ([]*mcp.ResourceTemplate, error) {
	var templates []*mcp.ResourceTemplate
	for t, err := range b.Session.ResourceTemplates(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("backend %q: list resource templates: %w", b.Name, err)
		}
		templates = append(templates, t)
	}
	return templates, nil
}

// ListPrompts fetches the backend's full prompt list, following pagination.
func (b *Backend) ListPrompts(ctx context.Context) ([]*mcp.Prompt, error) {
	var prompts []*mcp.Prompt
	for p, err := range b.Session.Prompts(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("backend %q: list prompts: %w", b.Name, err)
		}
		prompts = append(prompts, p)
	}
	return prompts, nil
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

// dockerCommand builds the "<bin> run ... <image> <command>" invocation that
// runs cfg.Command inside a container. Unlike sshCommand, the target program
// is exec'd directly via argv (no intermediate shell), so cfg.Dir and
// cfg.Env need no shell quoting: they become "-w" and "-e" flags. The local
// CLI process's own environment comes from cfg.Docker.Env, not cfg.Env,
// since cfg.Env describes the containerized command's environment, not the
// host-side docker/podman/nerdctl process's.
func dockerCommand(cfg config.BackendConfig) *exec.Cmd {
	bin := cfg.Docker.Bin
	if bin == "" {
		bin = "docker"
	}

	args := []string{"run", "-i", "--rm"}
	keys := make([]string, 0, len(cfg.Env))
	for k := range cfg.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "-e", k+"="+cfg.Env[k])
	}
	if cfg.Dir != "" {
		args = append(args, "-w", cfg.Dir)
	}
	args = append(args, cfg.Docker.Args...)
	args = append(args, cfg.Docker.Image)
	args = append(args, cfg.Command...)

	cmd := exec.Command(bin, args...)
	cmd.Env = envWithOverrides(cfg.Docker.Env)
	return cmd
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
	if proxy == "" {
		return http.DefaultTransport, nil
	}

	t := defaultTransportTemplate.Clone()
	if proxy == "none" {
		t.Proxy = nil
		return t, nil
	}
	proxyURL, err := url.Parse(proxy)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy url: %w", err)
	}
	t.Proxy = http.ProxyURL(proxyURL)
	return t, nil
}

// defaultTransportTemplate is the *http.Transport cloned as a base for a
// backend-specific proxy setting. http.DefaultTransport is documented as an
// *http.Transport, but its static type is only http.RoundTripper; fall back
// to a plain zero-value Transport rather than panic if that ever changes.
var defaultTransportTemplate = func() *http.Transport {
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		return t
	}
	return &http.Transport{}
}()

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

// tracingRoundTripper injects the current span's trace context into
// outbound requests to an HTTP backend, so a call that arrived over HTTP
// (and therefore has an active span in ctx) continues the same trace on the
// backend side. It does not start its own span -- the handler's span
// already covers the call's duration; a bare stdio-originated ctx carries
// no span, so Inject is a no-op and no traceparent header is sent. Only
// trace context is forwarded, never baggage: an inbound client fully
// controls the baggage header, and forwarding it to backends unredacted
// would bypass the audit log's key-masking entirely.
type tracingRoundTripper struct {
	base http.RoundTripper
}

func (t tracingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	propagation.TraceContext{}.Inject(req.Context(), propagation.HeaderCarrier(req.Header))
	return t.base.RoundTrip(req)
}
