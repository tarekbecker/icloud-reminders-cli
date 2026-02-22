package cmd

import (
	"fmt"

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

When the session expires, run 'reminders auth' again to re-authenticate.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		force, _ := cmd.Flags().GetBool("force")
		a := auth.New()
		sess, err := a.EnsureSession(cache.SessionFile, force)
		if err != nil {
			return err
		}
		fmt.Printf("✅ Authenticated\n")
		fmt.Printf("   CK base: %s\n", sess.CKBaseURL)
		if sess.TrustToken != "" {
			fmt.Println("   Trust token: saved (won't need 2FA next time)")
		}
		return nil
	},
}

func init() {
	authCmd.Flags().Bool("force", false, "Force re-authentication even if session is valid")
}
