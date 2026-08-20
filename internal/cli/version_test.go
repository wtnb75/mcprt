package cli_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/wtnb75/mcprt/internal/cli"
)

func TestVersionCommand_PrintsMcprtPrefixedVersion(t *testing.T) {
	var out bytes.Buffer
	root := cli.NewRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"version"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}

	got := strings.TrimSpace(out.String())
	if got == "" {
		t.Fatal("version command printed nothing")
	}
	if !strings.HasPrefix(got, "mcprt ") {
		t.Fatalf("output = %q, want it to start with \"mcprt \"", got)
	}
}
