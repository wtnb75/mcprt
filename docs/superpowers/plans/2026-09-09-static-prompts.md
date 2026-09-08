# Static Prompts (config-defined `prompts/get`) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let mcprt serve `prompts/get` for prompts defined directly in `config.yaml` (name, description, arguments, a `text/template` body), without forwarding to any backend.

**Architecture:** A new `prompts:` config key parses into `config.StaticPromptConfig`. `internal/cli`'s `buildGateway` converts each entry into a `*gateway.StaticPrompt` (parses `text` as a `text/template.Template`). `gateway.New` registers these directly via `mcp.Server.AddPrompt`, and both `New` and `reconcile.go`'s `updatePromptsLocked` skip registering any backend-sourced prompt whose name collides with a static one -- static always wins.

**Tech Stack:** Go, `github.com/modelcontextprotocol/go-sdk/mcp`, Go standard `text/template`, `gopkg.in/yaml.v3`.

**Spec:** `docs/superpowers/specs/2026-09-08-static-prompts-design.md`

## Global Constraints

- `prompts:` entries validate at config-load time (`internal/config`'s `validate`): non-empty unique `name`, non-empty `text` that parses as a `text/template`, non-empty unique argument `name`s. A bad config must fail `mcprt validate` / `mcprt server` startup / SIGHUP reload -- never partially apply.
- `text` templates use `Option("missingkey=zero")` -- Go's `text/template` default (`missingkey=invalid`) would otherwise render a missing key as the literal string `"<no value>"` instead of `""`. This must be set wherever a `*template.Template` used to render an actual response is constructed (`internal/gateway.NewStaticPrompt`).
- A static prompt's `name` always wins a collision with a same-named backend prompt. This is not configurable (unlike `overrides`/`prompt_overrides`). mcprt logs a `Warn` once, at startup (`gateway.New`), when this happens -- never on every `list_changed` reconcile (that would be repeated log noise across reconnects).
- A `required: true` argument missing from the caller's `arguments` makes `prompts/get` return an error, before the template is ever executed.
- A static prompt renders exactly one `role: "user"` `*mcp.PromptMessage` (`Content: &mcp.TextContent{...}`) -- never multiple messages, never other roles.
- Audit logging and OTel tracing for a static prompt call use the fixed sentinel `"(static)"` wherever a real prompt call would log/tag a backend name.
- Every task ends in a state where `go build ./...`, `gofmt -l .`, `go vet ./...`, and `go test ./...` all pass clean, and ends with a commit.

---

## File Structure

- **`internal/config/config.go`** (modify): add `StaticPromptConfig`/`StaticPromptArgument` types, `Config.Prompts` field, `validateStaticPrompts`, wire it into `validate`.
- **`internal/config/config_test.go`** (modify): table-driven parse/validation tests for `prompts:`.
- **`internal/gateway/static_prompt.go`** (create): `StaticPrompt` type, `NewStaticPrompt`, `renderStaticPrompt` (pure render logic), `staticPromptHandler` (the `mcp.PromptHandler` wrapper with tracing/audit logging).
- **`internal/gateway/static_prompt_internal_test.go`** (create, `package gateway`): unit tests for `NewStaticPrompt`/`renderStaticPrompt` that don't need a running `*mcp.Server`.
- **`internal/gateway/gateway.go`** (modify): `NewConfig.StaticPrompts` field, `Server.staticPromptNames` field, `New`'s prompt-registration wiring.
- **`internal/gateway/reconcile.go`** (modify): `updatePromptsLocked` skips a backend prompt shadowed by a static one.
- **`internal/gateway/gateway_test.go`** (modify): end-to-end tests via a real HTTP round trip.
- **`internal/gateway/reconcile_test.go`** (modify): end-to-end test for the `list_changed`/reconcile path.
- **`internal/cli/server.go`** (modify): `buildStaticPrompts` helper, wired into `buildGateway`.
- **`internal/cli/server_internal_test.go`** (modify): unit tests for `buildStaticPrompts`, end-to-end tests via `buildGateway`.
- **`README.md`** (modify): document the `prompts:` key (already has the general client-support caveat from a prior commit; this adds the feature's own docs and folds `prompts:` into that caveat's wording).

---

### Task 1: `internal/config` -- parse and validate `prompts:`

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `config.StaticPromptConfig{Name, Description string; Arguments []StaticPromptArgument; Text string}`, `config.StaticPromptArgument{Name, Description string; Required bool}`, `config.Config.Prompts []StaticPromptConfig`. Task 4 consumes these.

- [ ] **Step 1: Write the failing tests**

Add to `internal/config/config_test.go`, right after `TestParse_PromptOverrideReferencesUnknownBackend` (around line 607) and before `TestParse_LoggingMaskKeys`:

```go
func TestParse_StaticPrompts(t *testing.T) {
	data := []byte(`
backends:
  - name: a
    transport: stdio
    command: ["x"]

prompts:
  - name: greet
    description: "greets someone"
    arguments:
      - name: user
        description: "who to greet"
        required: true
    text: "hello {{.user}}"
`)
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Prompts) != 1 {
		t.Fatalf("len(cfg.Prompts) = %d, want 1", len(cfg.Prompts))
	}
	p := cfg.Prompts[0]
	if p.Name != "greet" || p.Description != "greets someone" || p.Text != "hello {{.user}}" {
		t.Fatalf("Prompts[0] = %+v, want name=greet description=%q text=%q", p, "greets someone", "hello {{.user}}")
	}
	if len(p.Arguments) != 1 || p.Arguments[0].Name != "user" || !p.Arguments[0].Required {
		t.Fatalf("Prompts[0].Arguments = %+v, want one required argument named \"user\"", p.Arguments)
	}
}

func TestParse_StaticPromptEmptyNameRejected(t *testing.T) {
	data := []byte(`
backends:
  - name: a
    transport: stdio
    command: ["x"]

prompts:
  - name: ""
    text: "hi"
`)
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for empty prompts[].name, got nil")
	}
}

func TestParse_StaticPromptDuplicateNameRejected(t *testing.T) {
	data := []byte(`
backends:
  - name: a
    transport: stdio
    command: ["x"]

prompts:
  - name: greet
    text: "hi"
  - name: greet
    text: "hello"
`)
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for duplicate prompts[].name, got nil")
	}
}

func TestParse_StaticPromptEmptyTextRejected(t *testing.T) {
	data := []byte(`
backends:
  - name: a
    transport: stdio
    command: ["x"]

prompts:
  - name: greet
    text: ""
`)
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for empty prompts[].text, got nil")
	}
}

func TestParse_StaticPromptInvalidTemplateRejected(t *testing.T) {
	data := []byte(`
backends:
  - name: a
    transport: stdio
    command: ["x"]

prompts:
  - name: greet
    text: "hello {{.unterminated"
`)
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for unparseable prompts[].text template, got nil")
	}
}

func TestParse_StaticPromptDuplicateArgumentNameRejected(t *testing.T) {
	data := []byte(`
backends:
  - name: a
    transport: stdio
    command: ["x"]

prompts:
  - name: greet
    text: "hi"
    arguments:
      - name: user
      - name: user
`)
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for duplicate prompts[].arguments[].name, got nil")
	}
}

func TestParse_StaticPromptEmptyArgumentNameRejected(t *testing.T) {
	data := []byte(`
backends:
  - name: a
    transport: stdio
    command: ["x"]

prompts:
  - name: greet
    text: "hi"
    arguments:
      - name: ""
`)
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for empty prompts[].arguments[].name, got nil")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config/... -run TestParse_StaticPrompt -v`
Expected: FAIL -- `cfg.Prompts` doesn't exist yet (compile error: `Config` has no field `Prompts`).

- [ ] **Step 3: Add the types and wire in validation**

In `internal/config/config.go`, add `"text/template"` to the import block (alphabetically, between `"regexp"` and `"time"`):

```go
import (
	"fmt"
	"maps"
	"net/url"
	"os"
	"regexp"
	"text/template"
	"time"

	"github.com/joho/godotenv"
	"gopkg.in/yaml.v3"
)
```

Add the `Prompts` field to `Config` (after `PromptOverrides`, before `Logging`):

```go
type Config struct {
	Listen                    ListenConfig         `yaml:"listen"`
	Backends                  []BackendConfig      `yaml:"backends"`
	Overrides                 map[string]string    `yaml:"overrides,omitempty"`
	ResourceOverrides         map[string]string    `yaml:"resource_overrides,omitempty"`
	ResourceTemplateOverrides map[string]string    `yaml:"resource_template_overrides,omitempty"`
	PromptOverrides           map[string]string    `yaml:"prompt_overrides,omitempty"`
	Prompts                   []StaticPromptConfig `yaml:"prompts,omitempty"`
	Logging                   LoggingConfig        `yaml:"logging,omitempty"`
	Timeouts                  TimeoutsConfig       `yaml:"timeouts,omitempty"`
}

// StaticPromptConfig defines a prompt mcprt serves directly from config,
// without forwarding prompts/get to any backend. See
// internal/gateway.StaticPrompt for the runtime representation built from
// this at gateway-construction time (internal/cli's buildStaticPrompts
// parses Text as a template there; validateStaticPrompts below only checks
// it parses, it doesn't keep the *template.Template around).
type StaticPromptConfig struct {
	Name        string                 `yaml:"name"`
	Description string                 `yaml:"description,omitempty"`
	Arguments   []StaticPromptArgument `yaml:"arguments,omitempty"`
	Text        string                 `yaml:"text"`
}

// StaticPromptArgument mirrors mcp.PromptArgument's fields (Name,
// Description, Required) -- kept as a separate config-layer type rather
// than importing the mcp package here, matching how BackendConfig etc.
// don't import mcp either.
type StaticPromptArgument struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`
	Required    bool   `yaml:"required,omitempty"`
}
```

Add `validateStaticPrompts` right after `validate` returns (before the `validateTimeouts` doc comment), and call it from `validate`:

```go
	for promptName, backendName := range cfg.PromptOverrides {
		if !names[backendName] {
			return fmt.Errorf("prompt_overrides %q references unknown backend %q", promptName, backendName)
		}
	}

	if err := validateStaticPrompts(cfg.Prompts); err != nil {
		return err
	}

	if err := validateTimeouts(cfg.Timeouts); err != nil {
		return err
	}

	return nil
}

// validateStaticPrompts rejects an empty/duplicate prompt or argument name,
// an empty text body, and a text body that doesn't parse as a Go
// text/template -- so a broken prompts: entry fails fast at config-load
// time (mcprt validate/server startup/SIGHUP reload), the same as every
// other misconfiguration this file checks.
func validateStaticPrompts(prompts []StaticPromptConfig) error {
	seen := make(map[string]bool, len(prompts))
	for _, p := range prompts {
		if p.Name == "" {
			return fmt.Errorf("prompts: name is required")
		}
		if seen[p.Name] {
			return fmt.Errorf("prompts %q: duplicate name", p.Name)
		}
		seen[p.Name] = true
		if p.Text == "" {
			return fmt.Errorf("prompts %q: text is required", p.Name)
		}
		if _, err := template.New(p.Name).Parse(p.Text); err != nil {
			return fmt.Errorf("prompts %q: parse text template: %w", p.Name, err)
		}
		argSeen := make(map[string]bool, len(p.Arguments))
		for _, a := range p.Arguments {
			if a.Name == "" {
				return fmt.Errorf("prompts %q: argument name is required", p.Name)
			}
			if argSeen[a.Name] {
				return fmt.Errorf("prompts %q: duplicate argument %q", p.Name, a.Name)
			}
			argSeen[a.Name] = true
		}
	}
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/config/... -v`
Expected: PASS, including every `TestParse_StaticPrompt*` case and every pre-existing test.

- [ ] **Step 5: Format, vet, commit**

Run: `gofmt -l internal/config` (expect no output), `go vet ./internal/config/...`

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): parse and validate prompts: (static prompt definitions)"
```

---

### Task 2: `internal/gateway` -- `StaticPrompt` type and render logic

**Files:**
- Create: `internal/gateway/static_prompt.go`
- Test: `internal/gateway/static_prompt_internal_test.go`

**Interfaces:**
- Consumes: `*mcp.Prompt`, `*mcp.PromptArgument`, `*mcp.GetPromptResult`, `*mcp.PromptMessage`, `*mcp.TextContent` (all from `github.com/modelcontextprotocol/go-sdk/mcp`, already a dependency).
- Produces: `gateway.StaticPrompt{Prompt *mcp.Prompt; Template *template.Template}`, `gateway.NewStaticPrompt(name, description string, args []*mcp.PromptArgument, text string) (*StaticPrompt, error)` (exported: Task 4's `buildStaticPrompts` calls it), `renderStaticPrompt(sp *StaticPrompt, args map[string]string) (*mcp.GetPromptResult, error)` (unexported: Task 3's `staticPromptHandler` calls it).

- [ ] **Step 1: Write the failing tests**

Create `internal/gateway/static_prompt_internal_test.go`:

```go
package gateway

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestNewStaticPrompt_Success(t *testing.T) {
	sp, err := NewStaticPrompt("greet", "greets someone", []*mcp.PromptArgument{{Name: "name", Required: true}}, "hello {{.name}}")
	if err != nil {
		t.Fatalf("NewStaticPrompt: %v", err)
	}
	if sp.Prompt.Name != "greet" || sp.Prompt.Description != "greets someone" {
		t.Fatalf("Prompt = %+v, want name=greet description=%q", sp.Prompt, "greets someone")
	}
	if len(sp.Prompt.Arguments) != 1 || sp.Prompt.Arguments[0].Name != "name" {
		t.Fatalf("Prompt.Arguments = %+v, want one argument named \"name\"", sp.Prompt.Arguments)
	}
}

func TestNewStaticPrompt_InvalidTemplateReturnsError(t *testing.T) {
	if _, err := NewStaticPrompt("bad", "", nil, "{{.unterminated"); err == nil {
		t.Fatal("NewStaticPrompt: expected error for unterminated template action, got nil")
	}
}

func TestRenderStaticPrompt_SubstitutesArgument(t *testing.T) {
	sp, err := NewStaticPrompt("greet", "", nil, "hello {{.name}}")
	if err != nil {
		t.Fatalf("NewStaticPrompt: %v", err)
	}
	result, err := renderStaticPrompt(sp, map[string]string{"name": "world"})
	if err != nil {
		t.Fatalf("renderStaticPrompt: %v", err)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("Messages = %+v, want 1 message", result.Messages)
	}
	if result.Messages[0].Role != "user" {
		t.Fatalf("Role = %q, want %q", result.Messages[0].Role, "user")
	}
	text, ok := result.Messages[0].Content.(*mcp.TextContent)
	if !ok || text.Text != "hello world" {
		t.Fatalf("content = %+v, want text %q", result.Messages[0].Content, "hello world")
	}
}

func TestRenderStaticPrompt_MissingRequiredArgumentReturnsError(t *testing.T) {
	sp, err := NewStaticPrompt("greet", "", []*mcp.PromptArgument{{Name: "name", Required: true}}, "hello {{.name}}")
	if err != nil {
		t.Fatalf("NewStaticPrompt: %v", err)
	}
	if _, err := renderStaticPrompt(sp, map[string]string{}); err == nil {
		t.Fatal("renderStaticPrompt: expected error for missing required argument, got nil")
	}
}

func TestRenderStaticPrompt_UnsetOptionalArgumentRendersEmpty(t *testing.T) {
	sp, err := NewStaticPrompt("greet", "", []*mcp.PromptArgument{{Name: "name", Required: false}}, "hello [{{.name}}]")
	if err != nil {
		t.Fatalf("NewStaticPrompt: %v", err)
	}
	result, err := renderStaticPrompt(sp, map[string]string{})
	if err != nil {
		t.Fatalf("renderStaticPrompt: %v", err)
	}
	text, ok := result.Messages[0].Content.(*mcp.TextContent)
	if !ok || text.Text != "hello []" {
		t.Fatalf("content = %+v, want text %q (not the literal \"<no value>\" text/template's default missingkey behavior would insert)", result.Messages[0].Content, "hello []")
	}
}

func TestRenderStaticPrompt_SetsDescriptionOnResult(t *testing.T) {
	sp, err := NewStaticPrompt("greet", "a friendly greeting", nil, "hi")
	if err != nil {
		t.Fatalf("NewStaticPrompt: %v", err)
	}
	result, err := renderStaticPrompt(sp, nil)
	if err != nil {
		t.Fatalf("renderStaticPrompt: %v", err)
	}
	if result.Description != "a friendly greeting" {
		t.Fatalf("Description = %q, want %q", result.Description, "a friendly greeting")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/gateway/... -run 'TestNewStaticPrompt|TestRenderStaticPrompt' -v`
Expected: FAIL with a compile error (`NewStaticPrompt`/`renderStaticPrompt` undefined).

- [ ] **Step 3: Implement `internal/gateway/static_prompt.go`**

```go
package gateway

import (
	"fmt"
	"strings"
	"text/template"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// StaticPrompt is a prompt mcprt serves directly from config, without
// forwarding prompts/get to any backend. Unlike a backend-sourced prompt, it
// always wins a name collision (see New in gateway.go and
// updatePromptsLocked in reconcile.go).
type StaticPrompt struct {
	Prompt   *mcp.Prompt
	Template *template.Template
}

// NewStaticPrompt parses text as a Go text/template and pairs it with the
// mcp.Prompt it renders for. The caller (internal/cli's buildStaticPrompts)
// does this conversion once per (re)build, from config.StaticPromptConfig --
// a bad template here fails the same startup/SIGHUP-reload path a bad
// backend config would. internal/config's validateStaticPrompts already
// rejects an unparseable text before buildGateway ever runs, so this Parse
// should not fail in practice; it is not skipped, since NewStaticPrompt has
// no way to assume that validation ran.
func NewStaticPrompt(name, description string, args []*mcp.PromptArgument, text string) (*StaticPrompt, error) {
	// missingkey=zero: text/template's default behavior for a map key that
	// isn't present is to print the literal string "<no value>", not "".
	// An unset optional argument (or a name the template references but
	// arguments: never declared) must render as an empty string instead --
	// see TestRenderStaticPrompt_UnsetOptionalArgumentRendersEmpty.
	tmpl, err := template.New(name).Option("missingkey=zero").Parse(text)
	if err != nil {
		return nil, fmt.Errorf("prompt %q: parse text template: %w", name, err)
	}
	return &StaticPrompt{
		Prompt:   &mcp.Prompt{Name: name, Description: description, Arguments: args},
		Template: tmpl,
	}, nil
}

// renderStaticPrompt checks sp's required arguments are all present in
// args, then executes sp.Template with args as the template's dot context
// (Go's text/template resolves .field against a map[string]string as a key
// lookup), returning a single role:user text message -- a static prompt
// never expresses multi-message/multi-role content (see the design spec,
// docs/superpowers/specs/2026-09-08-static-prompts-design.md).
func renderStaticPrompt(sp *StaticPrompt, args map[string]string) (*mcp.GetPromptResult, error) {
	for _, a := range sp.Prompt.Arguments {
		if !a.Required {
			continue
		}
		if _, ok := args[a.Name]; !ok {
			return nil, fmt.Errorf("prompt %q: missing required argument %q", sp.Prompt.Name, a.Name)
		}
	}
	var buf strings.Builder
	if err := sp.Template.Execute(&buf, args); err != nil {
		return nil, fmt.Errorf("prompt %q: render: %w", sp.Prompt.Name, err)
	}
	return &mcp.GetPromptResult{
		Description: sp.Prompt.Description,
		Messages: []*mcp.PromptMessage{
			{Role: "user", Content: &mcp.TextContent{Text: buf.String()}},
		},
	}, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/gateway/... -run 'TestNewStaticPrompt|TestRenderStaticPrompt' -v`
Expected: PASS, all 6 cases.

- [ ] **Step 5: Format, vet, full package test, commit**

Run: `gofmt -l internal/gateway`, `go vet ./internal/gateway/...`, `go test ./internal/gateway/... -race`

```bash
git add internal/gateway/static_prompt.go internal/gateway/static_prompt_internal_test.go
git commit -m "feat(gateway): add StaticPrompt type and render logic"
```

---

### Task 3: `internal/gateway` -- wire `StaticPrompt` into `Server`

**Files:**
- Modify: `internal/gateway/static_prompt.go` (append `staticPromptHandler`)
- Modify: `internal/gateway/gateway.go`
- Modify: `internal/gateway/reconcile.go`
- Test: `internal/gateway/gateway_test.go`
- Test: `internal/gateway/reconcile_test.go`

**Interfaces:**
- Consumes: `gateway.StaticPrompt`, `NewStaticPrompt`, `renderStaticPrompt` (Task 2). `startCallSpan`, `recordOutcome` (`internal/gateway/tracing.go`), `logCall` (`internal/gateway/audit.go`) -- all pre-existing, same package.
- Produces: `gateway.NewConfig.StaticPrompts []*StaticPrompt` field, and the guarantee that a static prompt always wins a name collision, for Task 4 (`buildGateway`) to rely on.

- [ ] **Step 1: Write the failing tests**

Add to `internal/gateway/gateway_test.go` (anywhere after the existing prompt tests, e.g. right after `TestGateway_PromptGetOnDeadBackendReturnsError`, which ends around line 923):

```go
func TestGateway_StaticPromptServesConfiguredText(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	sp, err := gateway.NewStaticPrompt("greet", "greets someone", []*mcp.PromptArgument{{Name: "name", Required: true}}, "hello {{.name}}")
	if err != nil {
		t.Fatalf("NewStaticPrompt: %v", err)
	}

	srv := gateway.New(gateway.NewConfig{
		Logger:        logger,
		Backends:      map[string]*backend.Backend{},
		StaticPrompts: []*gateway.StaticPrompt{sp},
	})

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv.MCP() }, nil))
	defer gw.Close()

	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	res, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: "greet", Arguments: map[string]string{"name": "world"}})
	if err != nil {
		t.Fatalf("GetPrompt(greet): %v", err)
	}
	text, ok := res.Messages[0].Content.(*mcp.TextContent)
	if !ok || text.Text != "hello world" {
		t.Fatalf("GetPrompt(greet) content = %+v, want text \"hello world\"", res.Messages[0].Content)
	}
}

func TestGateway_StaticPromptMissingRequiredArgumentReturnsError(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	sp, err := gateway.NewStaticPrompt("greet", "", []*mcp.PromptArgument{{Name: "name", Required: true}}, "hello {{.name}}")
	if err != nil {
		t.Fatalf("NewStaticPrompt: %v", err)
	}

	srv := gateway.New(gateway.NewConfig{
		Logger:        logger,
		Backends:      map[string]*backend.Backend{},
		StaticPrompts: []*gateway.StaticPrompt{sp},
	})

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv.MCP() }, nil))
	defer gw.Close()

	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	if _, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: "greet"}); err == nil {
		t.Fatal("GetPrompt(greet) succeeded, want an error: required argument \"name\" missing")
	}
}

// TestGateway_StaticPromptAlwaysWinsOverBackendPrompt checks that when a
// static config prompt and a backend prompt share a name, the static one is
// the one actually served -- the backend's same-named prompt must not be
// registered at all.
func TestGateway_StaticPromptAlwaysWinsOverBackendPrompt(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	backendServer := newFakePromptBackendServer("backend-a", "review")
	httpBackend := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendServer }, nil))
	defer httpBackend.Close()

	ctx := context.Background()
	conn, err := backend.Connect(ctx, config.BackendConfig{Name: "backend-a", Transport: "http", URL: httpBackend.URL}, backend.ChangeCallbacks{})
	if err != nil {
		t.Fatalf("connect backend-a: %v", err)
	}
	defer func() { _ = conn.Close() }()

	prompts, err := conn.ListPrompts(ctx)
	if err != nil {
		t.Fatalf("list backend-a prompts: %v", err)
	}
	table := router.Resolve([]router.Entry[*mcp.Prompt]{{BackendName: "backend-a", Items: prompts}}, promptNameOf, promptRename, nil)

	sp, err := gateway.NewStaticPrompt("review", "", nil, "static review text")
	if err != nil {
		t.Fatalf("NewStaticPrompt: %v", err)
	}

	srv := gateway.New(gateway.NewConfig{
		Logger:        logger,
		Backends:      map[string]*backend.Backend{"backend-a": conn},
		Tables:        gateway.Tables{Prompts: table},
		StaticPrompts: []*gateway.StaticPrompt{sp},
	})

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv.MCP() }, nil))
	defer gw.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	res, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: "review"})
	if err != nil {
		t.Fatalf("GetPrompt(review): %v", err)
	}
	text, ok := res.Messages[0].Content.(*mcp.TextContent)
	if !ok || text.Text != "static review text" {
		t.Fatalf("GetPrompt(review) content = %+v, want the static prompt's text (config always wins)", res.Messages[0].Content)
	}
}
```

Add to `internal/gateway/reconcile_test.go`, right after `TestUpdatePrompts_AddsRemovesAndChangesItems`:

```go
// TestUpdatePrompts_StaticPromptStillWinsAfterBackendReportsCollidingName
// checks the reconcile path (a backend connecting/reconnecting after New,
// reporting its prompt list via UpdatePrompts): a static prompt must keep
// winning even when a backend's list_changed notification introduces a
// same-named prompt after the gateway was already built.
func TestUpdatePrompts_StaticPromptStillWinsAfterBackendReportsCollidingName(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	sp, err := gateway.NewStaticPrompt("review", "", nil, "static review text")
	if err != nil {
		t.Fatalf("NewStaticPrompt: %v", err)
	}

	srv := gateway.New(gateway.NewConfig{
		Logger:        logger,
		Backends:      map[string]*backend.Backend{"a": {Name: "a"}},
		StaticPrompts: []*gateway.StaticPrompt{sp},
	})

	srv.UpdatePrompts("a", []*mcp.Prompt{{Name: "review", Description: "backend's version"}})

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv.MCP() }, nil))
	defer gw.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	res, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: "review"})
	if err != nil {
		t.Fatalf("GetPrompt(review): %v", err)
	}
	text, ok := res.Messages[0].Content.(*mcp.TextContent)
	if !ok || text.Text != "static review text" {
		t.Fatalf("GetPrompt(review) content = %+v, want the static prompt's text (still wins after UpdatePrompts)", res.Messages[0].Content)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/gateway/... -run 'TestGateway_StaticPrompt|TestUpdatePrompts_StaticPrompt' -v`
Expected: FAIL with a compile error (`NewConfig` has no field `StaticPrompts`).

- [ ] **Step 3: Implement the wiring**

Append to `internal/gateway/static_prompt.go` (update its import block first):

```go
import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"text/template"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"
)
```

```go
// staticPromptHandler wraps renderStaticPrompt with the same
// tracing/audit-logging treatment promptGetHandler (gateway.go) gives a
// backend-forwarded prompts/get -- there is no backend to attribute the
// call to, so both the span's "mcp.backend" attribute and the audit log's
// backend field use the fixed sentinel "(static)".
func staticPromptHandler(logger *slog.Logger, maskKeys []string, sp *StaticPrompt) mcp.PromptHandler {
	return func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		start := time.Now()
		ctx, span := startCallSpan(ctx, req.Extra, "prompts/get",
			attribute.String("mcp.backend", "(static)"),
			attribute.String("mcp.prompt.name", sp.Prompt.Name))
		defer span.End()

		result, err := renderStaticPrompt(sp, req.Params.Arguments)
		recordOutcome(span, err)
		logCall(ctx, logger, "prompt", "prompt", sp.Prompt.Name, "(static)", req.Session, req.Params.Arguments, maskKeys, start, err, nil)
		return result, err
	}
}
```

In `internal/gateway/gateway.go`, add a field to `NewConfig` (after `Relays Relays`, before the `KeepAlive` doc comment):

```go
	// StaticPrompts are prompts New registers directly (see the prompts
	// registration loop below, and updatePromptsLocked in reconcile.go),
	// without forwarding prompts/get to any backend. A static prompt's name
	// always wins a collision with a backend-sourced prompt of the same
	// name.
	StaticPrompts []*StaticPrompt
```

Add a field to `Server` (after `promptOverrides map[string]string`, inside the same struct, before the closing `}`):

```go

	// staticPromptNames is the set of prompt names New registered directly
	// from NewConfig.StaticPrompts. Unlike the reconcile fields above, it is
	// written once (in New) and never mutated afterward, but reading it
	// happens under mu anyway since its only reader (updatePromptsLocked in
	// reconcile.go) is already there for other reasons.
	staticPromptNames map[string]bool
```

Replace the whole `New` function body with:

```go
func New(cfg NewConfig) *Server {
	mcpSrv := mcp.NewServer(&mcp.Implementation{Name: "mcprt", Version: "v1"}, &mcp.ServerOptions{
		Logger:                    cfg.Logger,
		KeepAlive:                 cfg.KeepAlive,
		KeepAliveFailureThreshold: cfg.KeepAliveFailureThreshold,
	})

	staticNames := make(map[string]bool, len(cfg.StaticPrompts))
	for _, sp := range cfg.StaticPrompts {
		staticNames[sp.Prompt.Name] = true
	}

	s := &Server{
		mcp:      mcpSrv,
		logger:   cfg.Logger,
		backends: cfg.Backends,
		maskKeys: cfg.MaskKeys,
		relays:   cfg.Relays,

		toolEntries:   cfg.Entries.Tools,
		toolTable:     emptyTable(cfg.Tables.Tools),
		toolOverrides: cfg.Overrides.Tools,

		resourceEntries:           cfg.Entries.Resources,
		resourceTable:             emptyTable(cfg.Tables.Resources),
		resourceOverrides:         cfg.Overrides.Resources,
		resourceTemplateEntries:   cfg.Entries.ResourceTemplates,
		resourceTemplateTable:     emptyTable(cfg.Tables.ResourceTemplates),
		resourceTemplateOverrides: cfg.Overrides.ResourceTemplates,

		promptEntries:   cfg.Entries.Prompts,
		promptTable:     emptyTable(cfg.Tables.Prompts),
		promptOverrides: cfg.Overrides.Prompts,

		staticPromptNames: staticNames,
	}

	if cfg.Tables.Tools != nil {
		for _, resolved := range cfg.Tables.Tools.Items {
			registerTool(mcpSrv, cfg.Logger, cfg.Backends, resolved, cfg.MaskKeys, cfg.Relays)
		}
	}
	if cfg.Tables.Resources != nil {
		for _, resolved := range cfg.Tables.Resources.Items {
			registerResource(mcpSrv, cfg.Logger, cfg.Backends, resolved, cfg.MaskKeys)
		}
	}
	if cfg.Tables.ResourceTemplates != nil {
		for _, resolved := range cfg.Tables.ResourceTemplates.Items {
			registerResourceTemplate(mcpSrv, cfg.Logger, cfg.Backends, resolved, cfg.MaskKeys)
		}
	}
	if cfg.Tables.Prompts != nil {
		for _, resolved := range cfg.Tables.Prompts.Items {
			if staticNames[resolved.Item.Name] {
				cfg.Logger.Warn("prompt shadowed by static config prompt", "prompt", resolved.Item.Name, "backend", resolved.BackendName)
				continue
			}
			registerPrompt(mcpSrv, cfg.Logger, cfg.Backends, resolved, cfg.MaskKeys)
		}
	}
	for _, sp := range cfg.StaticPrompts {
		mcpSrv.AddPrompt(sp.Prompt, staticPromptHandler(cfg.Logger, cfg.MaskKeys, sp))
	}

	return s
}
```

In `internal/gateway/reconcile.go`, in `updatePromptsLocked`, add the skip check as the first line of the registration loop:

```go
	for name, resolved := range newTable.Items {
		if s.staticPromptNames[name] {
			continue // a static config prompt always wins; never register a backend's version under this name
		}
		old, ok := s.promptTable.Items[name]
		if !touchedBy(resolved, old, ok, backendName) {
			continue
		}
		unchanged := ok && reflect.DeepEqual(old, resolved)
		if unchanged && (!rebind || !boundTo(resolved, backendName)) {
			continue
		}
		registerPrompt(s.mcp, s.logger, s.backends, resolved, s.maskKeys)
	}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/gateway/... -v`
Expected: PASS, every test in the package (old and new).

- [ ] **Step 5: Format, vet, race test, commit**

Run: `gofmt -l internal/gateway`, `go vet ./internal/gateway/...`, `go test ./internal/gateway/... -race`

```bash
git add internal/gateway/static_prompt.go internal/gateway/gateway.go internal/gateway/reconcile.go internal/gateway/gateway_test.go internal/gateway/reconcile_test.go
git commit -m "feat(gateway): register static prompts, always winning a name collision"
```

---

### Task 4: `internal/cli` -- convert config into `gateway.StaticPrompts`

**Files:**
- Modify: `internal/cli/server.go`
- Test: `internal/cli/server_internal_test.go`

**Interfaces:**
- Consumes: `config.StaticPromptConfig`/`config.StaticPromptArgument` (Task 1), `gateway.NewStaticPrompt`, `gateway.StaticPrompt`, `gateway.NewConfig.StaticPrompts` (Tasks 2-3).
- Produces: `buildStaticPrompts(prompts []config.StaticPromptConfig) ([]*gateway.StaticPrompt, error)`, wired into `buildGateway` so `mcprt server`/SIGHUP reload serve `prompts:` from config.

- [ ] **Step 1: Write the failing tests**

Add to `internal/cli/server_internal_test.go`, right after `TestBuildGateway_Success`:

```go
func TestBuildStaticPrompts_ConvertsConfig(t *testing.T) {
	prompts := []config.StaticPromptConfig{
		{
			Name:        "greet",
			Description: "greets someone",
			Arguments:   []config.StaticPromptArgument{{Name: "name", Description: "who to greet", Required: true}},
			Text:        "hello {{.name}}",
		},
	}
	out, err := buildStaticPrompts(prompts)
	if err != nil {
		t.Fatalf("buildStaticPrompts: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("buildStaticPrompts returned %d entries, want 1", len(out))
	}
	sp := out[0]
	if sp.Prompt.Name != "greet" || sp.Prompt.Description != "greets someone" {
		t.Fatalf("Prompt = %+v, want name=greet description=%q", sp.Prompt, "greets someone")
	}
	if len(sp.Prompt.Arguments) != 1 || sp.Prompt.Arguments[0].Name != "name" || !sp.Prompt.Arguments[0].Required {
		t.Fatalf("Prompt.Arguments = %+v, want one required argument named \"name\"", sp.Prompt.Arguments)
	}
}

func TestBuildStaticPrompts_InvalidTemplatePropagatesError(t *testing.T) {
	prompts := []config.StaticPromptConfig{{Name: "bad", Text: "{{.unterminated"}}
	if _, err := buildStaticPrompts(prompts); err == nil {
		t.Fatal("buildStaticPrompts: expected error for invalid template, got nil")
	}
}

func TestBuildGateway_StaticPromptServesConfiguredText(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	cfg := &config.Config{
		Listen: config.ListenConfig{HTTP: "127.0.0.1:0"},
		Prompts: []config.StaticPromptConfig{
			{
				Name:      "greet",
				Text:      "hello {{.name}}",
				Arguments: []config.StaticPromptArgument{{Name: "name", Required: true}},
			},
		},
	}

	srv, err := buildGateway(context.Background(), logger, cfg)
	if err != nil {
		t.Fatalf("buildGateway: %v", err)
	}

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv.MCP() }, nil))
	defer gw.Close()

	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	res, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: "greet", Arguments: map[string]string{"name": "world"}})
	if err != nil {
		t.Fatalf("GetPrompt(greet): %v", err)
	}
	text, ok := res.Messages[0].Content.(*mcp.TextContent)
	if !ok || text.Text != "hello world" {
		t.Fatalf("GetPrompt(greet) content = %+v, want text \"hello world\"", res.Messages[0].Content)
	}
}

func TestBuildGateway_StaticPromptTemplateErrorFailsBuild(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	cfg := &config.Config{
		Listen:  config.ListenConfig{HTTP: "127.0.0.1:0"},
		Prompts: []config.StaticPromptConfig{{Name: "bad", Text: "{{.unterminated"}},
	}

	if _, err := buildGateway(context.Background(), logger, cfg); err == nil {
		t.Fatal("buildGateway: expected error for invalid static prompt template, got nil")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/cli/... -run 'TestBuildStaticPrompts|TestBuildGateway_StaticPrompt' -v`
Expected: FAIL with a compile error (`buildStaticPrompts` undefined).

- [ ] **Step 3: Implement `buildStaticPrompts` and wire it into `buildGateway`**

In `internal/cli/server.go`, add (near `buildGateway`, e.g. right before its doc comment):

```go
// buildStaticPrompts converts each config-level static prompt definition
// into the *gateway.StaticPrompt New actually registers, parsing its text
// template along the way. internal/config's validateStaticPrompts already
// rejects an unparseable template at config-load time, so an error here
// should only happen if that validation was skipped somehow -- still
// handled, not assumed impossible.
func buildStaticPrompts(prompts []config.StaticPromptConfig) ([]*gateway.StaticPrompt, error) {
	out := make([]*gateway.StaticPrompt, 0, len(prompts))
	for _, p := range prompts {
		args := make([]*mcp.PromptArgument, 0, len(p.Arguments))
		for _, a := range p.Arguments {
			args = append(args, &mcp.PromptArgument{Name: a.Name, Description: a.Description, Required: a.Required})
		}
		sp, err := gateway.NewStaticPrompt(p.Name, p.Description, args, p.Text)
		if err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, nil
}
```

In `buildGateway`, add the call right before `srv := gateway.New(...)`:

```go
	staticPrompts, err := buildStaticPrompts(cfg.Prompts)
	if err != nil {
		return nil, err
	}

	srv := gateway.New(gateway.NewConfig{
```

And add `StaticPrompts: staticPrompts,` to that `gateway.NewConfig{...}` literal, right after the `Overrides: gateway.Overrides{...}` block:

```go
		Overrides: gateway.Overrides{
			Tools:             cfg.Overrides,
			Resources:         cfg.ResourceOverrides,
			ResourceTemplates: cfg.ResourceTemplateOverrides,
			Prompts:           cfg.PromptOverrides,
		},
		StaticPrompts:             staticPrompts,
		MaskKeys:                  cfg.Logging.MaskKeys,
		Relays:                    gwH.relays,
		KeepAlive:                 time.Duration(cfg.Timeouts.DownstreamKeepAlive),
		KeepAliveFailureThreshold: cfg.Timeouts.DownstreamKeepAliveFailureThreshold,
	})
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/cli/... -v`
Expected: PASS, every test in the package (old and new).

- [ ] **Step 5: Format, vet, full test suite, commit**

Run: `gofmt -l internal/cli`, `go vet ./internal/cli/...`, `go test ./... -race`

```bash
git add internal/cli/server.go internal/cli/server_internal_test.go
git commit -m "feat(cli): build static prompts from config into the gateway"
```

---

### Task 5: README -- document `prompts:`

**Files:**
- Modify: `README.md`

**Interfaces:**
- None (documentation only). Depends on Tasks 1-4 being merged so the documented behavior is real.

- [ ] **Step 1: Add the `prompts:` example to the config block**

In `README.md`, right after the `prompt_overrides:` example block (`prompt_overrides:\n      code-review: filesystem`) and before `logging:`, insert:

```yaml
    prompts:
      - name: release-notes
        description: "Summarize a diff into release notes"
        arguments:
          - name: diff
            description: "The diff text to summarize"
            required: true
        text: |
          Summarize the following diff as release notes:

          {{.diff}}
```

- [ ] **Step 2: Add the prose describing `prompts:`**

Right after the existing paragraph ending "...the same way `tools/call`/`resources/read`/`prompts/get` are." and before the "Client support for MCP's `prompts` feature..." paragraph, insert:

```markdown
`prompts` defines prompts mcprt serves itself, without forwarding
`prompts/get` to any backend: each entry's `text` is a Go
[`text/template`](https://pkg.go.dev/text/template), rendered with the
caller's `arguments` as the template's `.` context (so `{{.diff}}` above
expands to the caller's `diff` argument). An argument marked
`required: true` that the caller doesn't supply makes `prompts/get` fail; an
argument that's merely unset (optional, or referenced in `text` but never
declared in `arguments`) renders as an empty string. A `prompts` entry's
`name` always wins a collision with a same-named prompt from a backend --
unlike `overrides`/`prompt_overrides`, this isn't configurable, and mcprt
logs a warning at startup when it happens.
```

- [ ] **Step 3: Fold `prompts:` into the existing client-support caveat**

Change:

```markdown
When naming a prompt (on a backend, or via `prompt_overrides`), keep the name to
`[A-Za-z0-9_-]` and keep its argument count low, so it stays usable across
whichever client ends up calling it.
```

to:

```markdown
When naming a prompt (on a backend, via `prompt_overrides`, or in `prompts`),
keep the name to `[A-Za-z0-9_-]` and keep its argument count low, so it
stays usable across whichever client ends up calling it.
```

- [ ] **Step 4: Review the rendered diff**

Run: `git diff README.md`
Expected: the three changes above, nothing else. Read through the new prose once for typos/broken markdown.

- [ ] **Step 5: Commit**

```bash
git add README.md
git commit -m "docs: document prompts: (static prompt definitions)"
```

---

## After All Tasks

Run the full suite once more end to end: `gofmt -l .`, `go vet ./...`, `golangci-lint run ./...` (if available locally), `go test ./... -race`. Then open the PR (base `main`, this branch's 5 commits) summarizing the feature and linking `docs/superpowers/specs/2026-09-08-static-prompts-design.md`.
