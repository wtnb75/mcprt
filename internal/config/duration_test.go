package config_test

import (
	"testing"

	"github.com/wtnb75/mcprt/internal/config"
)

// TestDuration_JSONSchemaIsString locks Duration's custom JSONSchema()
// override: without it, github.com/invopop/jsonschema would reflect
// Duration's underlying int64 type instead, and a config author's editor
// would flag a duration string like "5s" as a type error.
func TestDuration_JSONSchemaIsString(t *testing.T) {
	schema := config.Duration(0).JSONSchema()
	if schema.Type != "string" {
		t.Fatalf("Duration.JSONSchema().Type = %q, want %q", schema.Type, "string")
	}
	if schema.Pattern == "" {
		t.Fatal("Duration.JSONSchema().Pattern is empty, want a pattern matching Go duration strings")
	}
}
