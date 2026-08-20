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
