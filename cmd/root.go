// Package cmd provides the CLI commands for iCloud Reminders.
package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
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
var dataDirectory string
var commandLock *flock.Flock

// Execute runs a single CLI invocation and releases its account lease on
// both success and failure. The lease also protects administrative CLI writes
// from a running server using the same session/cache directory.
func Execute() error {
	defer func() {
		if commandLock != nil {
			_ = commandLock.Close()
			commandLock = nil
		}
	}()
	return RootCmd.Execute()
}

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
		if dataDirectory != "" {
			cache.SetDirectory(dataDirectory)
		}
		logger.SetLevel(verbosity)
		if cmd.Name() == "version" {
			return nil
		}
		if err := os.MkdirAll(cache.ConfigDir, 0700); err != nil {
			return fmt.Errorf("cannot create data directory")
		}
		commandLock = flock.New(filepath.Join(cache.ConfigDir, ".lock"))
		locked, err := commandLock.TryLock()
		if err != nil {
			return fmt.Errorf("cannot lock data directory")
		}
		if !locked {
			return fmt.Errorf("data directory is in use; stop the running server before administrative CLI operations")
		}

		// Commands that handle their own auth (or none)
		switch cmd.Name() {
		case "auth", "export-session", "import-session", "drive-export", "serve":
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
	defaultDirectory := os.Getenv("ICLOUD_REMINDERS_DATA_DIR")
	if defaultDirectory == "" {
		defaultDirectory = cache.ConfigDir
	}
	RootCmd.PersistentFlags().StringVar(&dataDirectory, "data-dir", defaultDirectory, "Directory containing private session and cache files")
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
		editCmd,
		jsonCmd,
		syncCmd,
		exportSessionCmd,
		driveExportCmd,
		importSessionCmd,
		serveCmd,
	)
}
