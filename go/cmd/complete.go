package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var completeCmd = &cobra.Command{
	Use:   "complete <id>",
	Short: "Mark a reminder as complete",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := syncEngine.Sync(false); err != nil {
			return err
		}
		result, err := w.CompleteReminder(args[0])
		if err != nil {
			return err
		}
		if errMsg, ok := result["error"].(string); ok {
			fmt.Fprintf(os.Stderr, "❌ %s\n", errMsg)
			os.Exit(1)
		}
		fmt.Printf("✅ Completed: %s\n", args[0])
		return nil
	},
}
