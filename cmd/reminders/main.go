// iCloud Reminders CLI (Go implementation)
// Mirrors the Python implementation in ../reminders_ck.py
package main

import (
	"fmt"
	"os"

	"icloud-reminders/cmd"
)

// version is set by GoReleaser at build time via ldflags.
// The checked-in version is used for local builds.
var version = "1.1.0"

func main() {
	cmd.SetVersion(version)
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
