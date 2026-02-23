package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var (
	editTitle    string
	editDue      string
	editNotes    string
	editPriority string
)

var editCmd = &cobra.Command{
	Use:   "edit <id>",
	Short: "Edit a reminder (title, due date, notes, or priority)",
	Long: `Update one or more fields on an existing reminder.

At least one flag must be provided. Only specified fields are changed;
unspecified fields are left unchanged.

Examples:
  reminders edit ABC123 --title "New title"
  reminders edit ABC123 --due 2026-03-01 --priority high
  reminders edit ABC123 --notes "Updated notes"
  reminders edit ABC123 --priority none`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := syncEngine.Sync(false); err != nil {
			return err
		}
		result, err := w.EditReminder(args[0], editTitle, editDue, editNotes, editPriority)
		if err != nil {
			return err
		}
		if errMsg, ok := result["error"].(string); ok {
			return fmt.Errorf("%s", errMsg)
		}
		fmt.Printf("✅ Updated: %s\n", args[0])
		return nil
	},
}

func init() {
	editCmd.Flags().StringVar(&editTitle, "title", "", "New title")
	editCmd.Flags().StringVarP(&editDue, "due", "d", "", "New due date (YYYY-MM-DD)")
	editCmd.Flags().StringVarP(&editNotes, "notes", "n", "", "New notes")
	editCmd.Flags().StringVarP(&editPriority, "priority", "p", "", "New priority (high, medium, low, none)")
}
