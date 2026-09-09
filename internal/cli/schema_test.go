package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/wtnb75/mcprt/internal/cli"
)

func TestSchemaCommand_NoOutputPathWritesToStdout(t *testing.T) {
	var out bytes.Buffer
	root := cli.NewRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"schema"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if got["$schema"] == nil {
		t.Fatal(`output missing "$schema"`)
	}
	props, ok := got["properties"].(map[string]any)
	if !ok {
		t.Fatalf(`output["properties"] = %#v, want a JSON object`, got["properties"])
	}
	if _, ok := props["listen"]; !ok {
		t.Fatal(`output["properties"] missing "listen"`)
	}
	if _, ok := props["backends"]; !ok {
		t.Fatal(`output["properties"] missing "backends"`)
	}
}

// TestSchemaCommand_DurationRendersAsString locks Duration's custom
// JSONSchema() override (internal/config/duration.go): without it, the
// reflector would describe Duration by its underlying int64, and a config
// author's editor would flag "5s" as a type error instead of accepting it.
func TestSchemaCommand_DurationRendersAsString(t *testing.T) {
	var out bytes.Buffer
	root := cli.NewRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"schema"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	defs, ok := got["$defs"].(map[string]any)
	if !ok {
		t.Fatalf(`output["$defs"] = %#v, want a JSON object`, got["$defs"])
	}
	duration, ok := defs["Duration"].(map[string]any)
	if !ok {
		t.Fatalf(`output["$defs"]["Duration"] = %#v, want a JSON object`, defs["Duration"])
	}
	if duration["type"] != "string" {
		t.Fatalf(`Duration schema type = %v, want "string"`, duration["type"])
	}
}

func TestSchemaCommand_RefusesToOverwrite(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "config.schema.json")
	if err := os.WriteFile(outPath, []byte("{}"), 0o600); err != nil {
		t.Fatalf("writing existing file: %v", err)
	}

	if err := cli.Execute(context.Background(), []string{"schema", outPath}); err == nil {
		t.Fatal("Execute: expected error when the output file already exists, got nil")
	}

	if err := cli.Execute(context.Background(), []string{"schema", "--force", outPath}); err != nil {
		t.Fatalf("Execute with --force: unexpected error: %v", err)
	}
}

// TestSchemaCommand_MatchesCommittedFile guards config.schema.json (checked
// into the repo root so `# yaml-language-server: $schema=...` comments can
// point a raw GitHub URL at it) against drifting from internal/config.Config:
// any struct/tag change that isn't followed by regenerating the committed
// file (`task schema`) fails this test.
func TestSchemaCommand_MatchesCommittedFile(t *testing.T) {
	committed, err := os.ReadFile(filepath.Join("..", "..", "config.schema.json"))
	if err != nil {
		t.Fatalf("reading config.schema.json: %v", err)
	}

	var out bytes.Buffer
	root := cli.NewRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"schema"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}

	if out.String() != string(committed) {
		t.Fatal("config.schema.json is stale: regenerate it with `task schema` (or `mcprt schema --force config.schema.json`) and commit the result")
	}
}
