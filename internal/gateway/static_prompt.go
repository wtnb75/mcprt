package gateway

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
