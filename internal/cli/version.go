package cli

import (
	"fmt"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// version and commit can be set at build time via
// "-ldflags -X github.com/wtnb75/mcprt/internal/cli.version=v1.2.3
// -X github.com/wtnb75/mcprt/internal/cli.commit=abc1234" (e.g. by a future
// goreleaser config). When left unset, as with `task build`/`go run`/`go
// test`, versionString falls back to runtime/debug.ReadBuildInfo, which Go
// automatically populates with module and VCS metadata for anything built
// inside a git checkout.
var (
	version = ""
	commit  = ""
)

func newVersionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "version",
		Short: "print the mcprt version",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), versionString())
			return err
		},
	}
	return cmd
}

func versionString() string {
	v := version
	rev := commit
	var dirty bool
	var buildTime string

	if v == "" || rev == "" {
		if info, ok := debug.ReadBuildInfo(); ok {
			if v == "" && info.Main.Version != "" && info.Main.Version != "(devel)" {
				v = info.Main.Version
			}
			for _, s := range info.Settings {
				switch s.Key {
				case "vcs.revision":
					if rev == "" {
						rev = s.Value
					}
				case "vcs.modified":
					dirty = s.Value == "true"
				case "vcs.time":
					buildTime = s.Value
				}
			}
		}
	}
	if v == "" {
		v = "dev"
	}

	out := "mcprt " + v
	if rev != "" {
		if len(rev) > 12 {
			rev = rev[:12]
		}
		out += fmt.Sprintf(" (commit %s", rev)
		if dirty {
			out += ", dirty"
		}
		if buildTime != "" {
			out += ", built " + buildTime
		}
		out += ")"
	}
	return out
}
