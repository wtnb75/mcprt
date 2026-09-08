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
