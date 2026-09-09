package config

import (
	"fmt"
	"time"

	"github.com/invopop/jsonschema"
	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that unmarshals from YAML as a Go duration
// string ("5s", "1m30s", ...) instead of yaml.v3's default of decoding
// straight into the underlying int64 nanosecond count.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}

// JSONSchema overrides the schema github.com/invopop/jsonschema would
// otherwise reflect for Duration's underlying int64 -- Duration is only
// ever written/read as a Go duration string ("5s", "1m30s", ...), never as
// a bare number (see UnmarshalYAML/MarshalYAML above), so its schema must
// say "string", not "integer".
//
// The pattern's "0" alternative matters on its own: time.ParseDuration("0")
// succeeds with no unit required, and several TimeoutsConfig fields (e.g.
// BackendKeepAlive) document a bare 0 as their meaningful "disable this"
// value -- a pattern that required a unit on every duration would flag that
// legitimate value as an error in any schema-aware editor.
func (d Duration) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:        "string",
		Pattern:     `^[-+]?(0|([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+)$`,
		Description: `a Go time.ParseDuration string, e.g. "5s", "1m30s", "500ms", or "0"`,
	}
}
