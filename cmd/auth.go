package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"icloud-reminders/internal/auth"
	"icloud-reminders/internal/cache"
)

var authCmd = &cobra.Command{
	Use:   "auth",
	Short: "Authenticate with iCloud (required on first run or session expiry)",
	Long: `Authenticate with iCloud using your Apple ID and password.

Credentials are resolved in this order:
  1. ICLOUD_USERNAME / ICLOUD_PASSWORD environment variables
  2. ~/.config/icloud-reminders/credentials file (export KEY=value format)
  3. Interactive prompt (fallback)

The password is used for SRP authentication (never sent to servers in plain text)
and is not persisted. On success, a session token is saved to:
  ~/.config/icloud-reminders/session.json

Subsequent commands reuse the saved session automatically.
Use --force to re-authenticate even if a valid session exists.

For Advanced Data Protection or missing Reminders web approval, use:
  reminders auth --approve-web-access
Approve the device notifications on your trusted Apple device. Existing login
sessions are reused; successful approval is saved in the same data directory.
--approval-timeout bounds the wait (default 3 minutes).

When the session expires, run 'reminders auth' again to re-authenticate.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		force, _ := cmd.Flags().GetBool("force")
		approve, _ := cmd.Flags().GetBool("approve-web-access")
		timeout, _ := cmd.Flags().GetDuration("approval-timeout")
		if approve && (timeout < time.Second || timeout > 15*time.Minute) {
			return fmt.Errorf("approval timeout must be between 1 second and 15 minutes")
		}
		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		a := auth.NewWithContext(ctx)
		sess, err := a.EnsureSession(cache.SessionFile, force)
		if err != nil && !(approve && errors.Is(err, auth.ErrWebAccessDisabled)) {
			return err
		}
		if approve {
			sess, err = a.ApproveWebAccess(cache.SessionFile, timeout, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
		}
		fmt.Printf("✅ Authenticated\n")
		if sess.TrustToken != "" {
			fmt.Println("   Trust token: saved (won't need 2FA next time)")
		}
		return nil
	},
}

func init() {
	authCmd.Flags().Bool("force", false, "Force re-authentication even if session is valid")
	authCmd.Flags().Bool("approve-web-access", false, "Request trusted-device approval for temporary Reminders web access")
	authCmd.Flags().Duration("approval-timeout", 3*time.Minute, "Maximum wait for device approval")
}
