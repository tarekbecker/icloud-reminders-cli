package cmd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"icloud-reminders/internal/auth"
	"icloud-reminders/internal/cache"
)

var driveExportCmd = &cobra.Command{
	Use:   "drive-export --output <private/session.json>",
	Short: "Reuse the saved Go login and export a read-only Drive runtime session",
	Long:  "Reuse the existing Reminders login without password/2FA prompts. Stop the Go server first. --approve-web-access explicitly requests Drive approval on the trusted device; ordinary exports never request notifications.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		output, _ := cmd.Flags().GetString("output")
		approve, _ := cmd.Flags().GetBool("approve-web-access")
		timeout, _ := cmd.Flags().GetDuration("approval-timeout")
		if output == "" || timeout < time.Second || timeout > 15*time.Minute {
			return errors.New("output is required and approval timeout must be 1 second..15 minutes")
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), timeout+time.Minute)
		defer cancel()
		a := auth.NewWithContext(ctx)
		if _, err := a.PrepareDriveSession(cache.SessionFile); err != nil {
			return err
		}
		if approve {
			if _, err := a.ApproveDriveAccess(cache.SessionFile, timeout, cmd.ErrOrStderr()); err != nil {
				return err
			}
		}
		if err := a.ExportDriveSession(cache.SessionFile, output); err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Existing Go login reused; private Drive session exported. Check drive_status on the common MCP.")
		return nil
	},
}

func init() {
	driveExportCmd.Flags().String("output", "", "Private session.json destination for the Drive reader")
	driveExportCmd.Flags().Bool("approve-web-access", false, "Explicitly request Drive approval without repeating login")
	driveExportCmd.Flags().Duration("approval-timeout", 3*time.Minute, "Maximum wait for Drive device approval")
}
