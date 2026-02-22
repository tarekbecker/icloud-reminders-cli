// Package cmd provides the CLI commands for iCloud Reminders.
package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"icloud-reminders/internal/auth"
	"icloud-reminders/internal/cache"
	"icloud-reminders/internal/cloudkit"
	"icloud-reminders/internal/logger"
	"icloud-reminders/internal/sync"
	"icloud-reminders/internal/writer"
)

// verbosity is incremented once per -v flag: -v=1 (info), -vv=2 (debug).
var verbosity int

// shared per-invocation state (set in PersistentPreRunE)
var (
	ckClient   *cloudkit.Client
	syncEngine *sync.Engine
	w          *writer.Writer
)

// RootCmd is the root cobra command.
var RootCmd = &cobra.Command{
	Use:   "reminders",
	Short: "iCloud Reminders CLI (CloudKit)",
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		logger.SetLevel(verbosity)

		// Commands that handle their own auth (or none)
		switch cmd.Name() {
		case "auth", "export-session", "import-session":
			return nil
		}

		// Load session (reuse or refresh via accountLogin)
		sess, err := loadSession(false)
		if err != nil {
			return fmt.Errorf("not authenticated: %w\n\nRun: reminders auth", err)
		}

		ckClient, err = cloudkit.NewFromSession(sess)
		if err != nil {
			return fmt.Errorf("cloudkit init: %w", err)
		}
		syncEngine = sync.New(ckClient, cache.SessionFile)
		w = writer.New(ckClient, syncEngine)
		return nil
	},
}

// loadSession ensures a valid CloudKit session.
// If no valid session exists, returns error prompting for auth.
func loadSession(forceReauth bool) (*auth.SessionData, error) {
	a := auth.New()
	return a.EnsureSession(cache.SessionFile, forceReauth)
}

func init() {
	// CountP increments verbosity each time -v is passed: -v=1, -vv=2
	RootCmd.PersistentFlags().CountVarP(&verbosity, "verbose", "v", "Verbosity: -v info, -vv debug")

	RootCmd.AddCommand(
		authCmd,
		listCmd,
		searchCmd,
		listsCmd,
		addCmd,
		addBatchCmd,
		completeCmd,
		deleteCmd,
		jsonCmd,
		syncCmd,
		exportSessionCmd,
		importSessionCmd,
	)
}
