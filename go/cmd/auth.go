package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"icloud-reminders/auth"
	"icloud-reminders/cache"
)

var authCmd = &cobra.Command{
	Use:   "auth",
	Short: "Authenticate with iCloud (interactive — required on first run or session expiry)",
	Long: `Authenticate with iCloud using your Apple ID and password.

On first run or when your session has expired, this command will:
  1. Prompt for Apple ID and password (not stored, only used for authentication)
  2. Perform SRP authentication with Apple servers
  3. Prompt for a 2FA code if required
  4. Save the session to ~/.config/icloud-reminders/session.json

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
