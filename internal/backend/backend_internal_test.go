package backend

import (
	"net/http"
	"slices"
	"testing"
)

func TestEnvWithOverrides(t *testing.T) {
	t.Setenv("MCPRT_TEST_BASE", "base-value")
	got := envWithOverrides(map[string]string{"EXTRA": "extra-value"})
	if !slices.Contains(got, "MCPRT_TEST_BASE=base-value") {
		t.Fatalf("envWithOverrides did not preserve base environment: %v", got)
	}
	if !slices.Contains(got, "EXTRA=extra-value") {
		t.Fatalf("envWithOverrides did not include extra entry: %v", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestHeaderRoundTripper(t *testing.T) {
	var gotHeader string
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotHeader = req.Header.Get("Authorization")
		return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
	})
	rt := headerRoundTripper{headers: map[string]string{"Authorization": "Bearer xyz"}, base: base}
	req, err := http.NewRequest(http.MethodGet, "http://example.com", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if gotHeader != "Bearer xyz" {
		t.Fatalf("Authorization header = %q, want %q", gotHeader, "Bearer xyz")
	}
}
