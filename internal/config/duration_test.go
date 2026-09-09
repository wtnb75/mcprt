package config_test

import (
	"regexp"
	"testing"
	"time"

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

// TestDuration_JSONSchemaPatternMatchesParseDuration checks the schema
// pattern's accept/reject decisions against time.ParseDuration (what
// UnmarshalYAML actually uses) for every case, not just the string "5s" --
// a pattern that's merely non-empty could still reject values
// UnmarshalYAML happily accepts. "0" is a real regression case: several
// TimeoutsConfig fields document a bare 0 as their "disable this" value
// (e.g. TimeoutsConfig.BackendKeepAlive), and time.ParseDuration("0")
// succeeds with no unit required.
func TestDuration_JSONSchemaPatternMatchesParseDuration(t *testing.T) {
	re := regexp.MustCompile(config.Duration(0).JSONSchema().Pattern)

	cases := []struct {
		s    string
		want bool
	}{
		{"0", true},
		{"5s", true},
		{"1m30s", true},
		{"500ms", true},
		{"1.5s", true},
		{"-5s", true},
		{"+5s", true},
		{"", false},
		{"5", false},
		{"5x", false},
		{" 5s", false},
	}
	for _, c := range cases {
		got := re.MatchString(c.s)
		if got != c.want {
			t.Errorf("pattern.MatchString(%q) = %v, want %v", c.s, got, c.want)
		}
		// Cross-check against the real parser for every case in the table,
		// so this test can't drift from what UnmarshalYAML actually accepts.
		_, parseErr := time.ParseDuration(c.s)
		if parseAccepts := parseErr == nil; parseAccepts != c.want {
			t.Errorf("test case %q: time.ParseDuration accepts=%v but table says want=%v (fix the test case)", c.s, parseAccepts, c.want)
		}
	}
}
