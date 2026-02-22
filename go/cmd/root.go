// Package cmd provides the CLI commands for iCloud Reminders.
package cmd

import (
	"fmt"
	"log"
	"os"

	"github.com/spf13/cobra"

	"icloud-reminders/auth"
	"icloud-reminders/cache"
	"icloud-reminders/cloudkit"
	"icloud-reminders/sync"
	"icloud-reminders/writer"
)

var verbose bool

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
		if !verbose {
			// Suppress info logs unless -v is given
			log.SetOutput(os.Stderr)
		}
		log.SetFlags(0)

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
	RootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Verbose output")

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
