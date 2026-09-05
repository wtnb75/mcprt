package backend

import (
	"net/http"
	"slices"
	"testing"

	"github.com/wtnb75/mcprt/internal/config"
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

func TestRemoteScript(t *testing.T) {
	got := remoteScript(config.BackendConfig{
		Dir:     "/remote/dir",
		Env:     map[string]string{"FOO": "bar", "MSG": "it's a value with spaces"},
		Command: []string{"/usr/bin/mcp-server", "--flag", "arg with space"},
	})
	want := `cd '/remote/dir' && export FOO='bar' && export MSG='it'\''s a value with spaces' && exec '/usr/bin/mcp-server' '--flag' 'arg with space'`
	if got != want {
		t.Fatalf("remoteScript =\n%q\nwant\n%q", got, want)
	}
}

func TestRemoteScript_NoDirNoEnv(t *testing.T) {
	got := remoteScript(config.BackendConfig{Command: []string{"echo", "hi"}})
	want := "exec 'echo' 'hi'"
	if got != want {
		t.Fatalf("remoteScript = %q, want %q", got, want)
	}
}

func TestSSHCommand(t *testing.T) {
	cmd := sshCommand(config.BackendConfig{
		Command: []string{"echo", "hi"},
		SSH: &config.SSHConfig{
			Host:         "user@example.com",
			Port:         2222,
			IdentityFile: "/home/me/.ssh/id_ed25519",
			Args:         []string{"-o", "StrictHostKeyChecking=no"},
		},
	})
	want := []string{
		"ssh",
		"-p", "2222",
		"-i", "/home/me/.ssh/id_ed25519",
		"-o", "StrictHostKeyChecking=no",
		"user@example.com",
		"exec 'echo' 'hi'",
	}
	if !slices.Equal(cmd.Args, want) {
		t.Fatalf("sshCommand args = %v, want %v", cmd.Args, want)
	}
}

func TestHTTPBaseTransport_Default(t *testing.T) {
	rt, err := httpBaseTransport("")
	if err != nil {
		t.Fatalf("httpBaseTransport: %v", err)
	}
	if rt != http.RoundTripper(http.DefaultTransport) {
		t.Fatalf("httpBaseTransport(\"\") = %v, want http.DefaultTransport unchanged", rt)
	}
}

func TestHTTPBaseTransport_None(t *testing.T) {
	rt, err := httpBaseTransport("none")
	if err != nil {
		t.Fatalf("httpBaseTransport: %v", err)
	}
	tr, ok := rt.(*http.Transport)
	if !ok || tr.Proxy != nil {
		t.Fatalf("httpBaseTransport(\"none\") = %+v, want a *http.Transport with Proxy == nil", rt)
	}
}

func TestHTTPBaseTransport_FixedURL(t *testing.T) {
	rt, err := httpBaseTransport("http://proxy.example.com:8080")
	if err != nil {
		t.Fatalf("httpBaseTransport: %v", err)
	}
	tr, ok := rt.(*http.Transport)
	if !ok || tr.Proxy == nil {
		t.Fatalf("httpBaseTransport(url) = %+v, want a *http.Transport with a Proxy func set", rt)
	}
	req, err := http.NewRequest(http.MethodGet, "http://target.example.com/", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	got, err := tr.Proxy(req)
	if err != nil {
		t.Fatalf("Proxy(req): %v", err)
	}
	if got.String() != "http://proxy.example.com:8080" {
		t.Fatalf("Proxy(req) = %q, want %q", got, "http://proxy.example.com:8080")
	}
}

func TestHTTPBaseTransport_InvalidURL(t *testing.T) {
	if _, err := httpBaseTransport("://not-a-url"); err == nil {
		t.Fatal("httpBaseTransport: expected error for invalid proxy url, got nil")
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
	rt := headerRoundTripper{headers: map[string]string{"Authorization": "Bearer xyz"}, host: "example.com", base: base}
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

func TestHeaderRoundTripper_CrossHostRedirect_DoesNotLeakHeaders(t *testing.T) {
	var gotHeader string
	var sawHeader bool
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotHeader = req.Header.Get("Authorization")
		_, sawHeader = req.Header["Authorization"]
		return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
	})
	rt := headerRoundTripper{headers: map[string]string{"Authorization": "Bearer xyz"}, host: "example.com", base: base}
	// Simulates the request net/http builds for a cross-host redirect hop:
	// same RoundTripper, but req.URL now points at a different host, and
	// net/http has already stripped Authorization from req.Header.
	req, err := http.NewRequest(http.MethodGet, "http://attacker.example.com", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if sawHeader {
		t.Fatalf("Authorization header leaked to cross-host redirect target: %q", gotHeader)
	}
}
