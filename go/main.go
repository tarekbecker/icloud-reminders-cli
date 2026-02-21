// iCloud Reminders CLI (Go implementation)
// Mirrors the Python implementation in ../reminders_ck.py
package main

import (
	"fmt"
	"os"

	"icloud-reminders/cmd"
)

func main() {
	if err := cmd.RootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
