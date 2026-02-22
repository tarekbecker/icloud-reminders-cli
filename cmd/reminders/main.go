// iCloud Reminders CLI (Go implementation)
// Mirrors the Python implementation in ../reminders_ck.py
package main

import (
	"fmt"
	"os"

	"icloud-reminders/cmd"
)

// version is set by GoReleaser at build time via ldflags.
// Default "dev" is used for local builds.
var version = "dev"

func main() {
	cmd.SetVersion(version)
	if err := cmd.RootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
