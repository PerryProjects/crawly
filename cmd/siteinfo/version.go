package main

import (
	"fmt"
	"runtime/debug"

	"github.com/spf13/cobra"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print build information",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			// Build info is embedded by the Go toolchain; no -ldflags variable needed.
			version, revision, modified := "(devel)", "unknown", ""
			goVersion := "unknown"
			if bi, ok := debug.ReadBuildInfo(); ok {
				goVersion = bi.GoVersion
				if bi.Main.Version != "" {
					version = bi.Main.Version
				}
				for _, s := range bi.Settings {
					switch s.Key {
					case "vcs.revision":
						revision = s.Value
					case "vcs.modified":
						if s.Value == "true" {
							modified = "+dirty"
						}
					}
				}
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "siteinfo %s (commit %s%s, %s)\n", version, revision, modified, goVersion)
		},
	}
}
