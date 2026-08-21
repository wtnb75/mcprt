package gateway

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestMaskArguments(t *testing.T) {
	cases := []struct {
		name      string
		v         any
		extraKeys []string
		want      any
	}{
		{
			name: "flat object masks a default-pattern key, keeps others",
			v:    json.RawMessage(`{"api_key":"secret123","name":"alice"}`),
			want: map[string]any{"api_key": "***", "name": "alice"},
		},
		{
			name: "nested object masks at any depth",
			v:    json.RawMessage(`{"config":{"password":"hunter2","name":"y"}}`),
			want: map[string]any{"config": map[string]any{"password": "***", "name": "y"}},
		},
		{
			name: "array of objects masks within each element",
			v:    json.RawMessage(`[{"token":"a"},{"note":"b"}]`),
			want: []any{map[string]any{"token": "***"}, map[string]any{"note": "b"}},
		},
		{
			name: "prompt arguments (map[string]string) are masked the same way",
			v:    map[string]string{"authorization": "Bearer xyz", "topic": "go"},
			want: map[string]any{"authorization": "***", "topic": "go"},
		},
		{
			name:      "extraKeys mask in addition to the defaults",
			v:         json.RawMessage(`{"internal_id":"42","name":"alice"}`),
			extraKeys: []string{"internal_id"},
			want:      map[string]any{"internal_id": "***", "name": "alice"},
		},
		{
			name: "case-insensitive substring matching",
			v:    json.RawMessage(`{"APIKey":"x","Credential_ID":"y","Passwd":"z","access_token":"w"}`),
			want: map[string]any{"APIKey": "***", "Credential_ID": "***", "Passwd": "***", "access_token": "***"},
		},
		{
			name: "scalar RawMessage is returned unchanged",
			v:    json.RawMessage(`"hello"`),
			want: "hello",
		},
		{
			name: "malformed RawMessage falls back to its raw string form",
			v:    json.RawMessage(`not json`),
			want: "not json",
		},
		{
			name: "unsupported type falls back to fmt.Sprintf(\"%v\", v)",
			v:    42,
			want: "42",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := maskArguments(c.v, c.extraKeys)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("maskArguments(%#v, %v) = %#v, want %#v", c.v, c.extraKeys, got, c.want)
			}
		})
	}
}
