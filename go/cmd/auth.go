package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var authCmd = &cobra.Command{
	Use:   "auth",
	Short: "Authenticate with iCloud (interactive — required on first run or session expiry)",
	Long: `Authenticate with iCloud using your Apple ID and password.

On first run or when your session has expired, this command will:
  1. Sign in with ICLOUD_USERNAME / ICLOUD_PASSWORD from credentials file
  2. Prompt for a 2FA code if required
  3. Save the session to ~/.config/icloud-reminders/session.json

Subsequent commands reuse the saved session automatically.
Use --force to re-authenticate even if a valid session exists.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		force, _ := cmd.Flags().GetBool("force")
		sess, err := loadSession(force)
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
