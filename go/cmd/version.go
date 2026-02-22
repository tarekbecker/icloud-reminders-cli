package cmd

import (
	"fmt"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// version is set at build time via ldflags (see .goreleaser.yaml)
var version = "dev"

// SetVersion allows main.go to inject the build-time version
func SetVersion(v string) {
	version = v
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Run: func(cmd *cobra.Command, args []string) {
		v := version
		if v == "dev" {
			if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
				v = info.Main.Version
			}
		}
		fmt.Printf("icloud-reminders %s\n", v)
	},
}

func init() {
	RootCmd.AddCommand(versionCmd)
}
