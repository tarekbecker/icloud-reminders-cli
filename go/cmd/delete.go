package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var deleteCmd = &cobra.Command{
	Use:   "delete <id>",
	Short: "Delete a reminder",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := syncEngine.Sync(false); err != nil {
			return err
		}
		result, err := w.DeleteReminder(args[0])
		if err != nil {
			return err
		}
		if errMsg, ok := result["error"].(string); ok {
			fmt.Fprintf(os.Stderr, "❌ %s\n", errMsg)
			os.Exit(1)
		}
		fmt.Printf("✅ Deleted: %s\n", args[0])
		return nil
	},
}
