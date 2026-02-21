package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var (
	addListName string
	addDue      string
	addPriority string
	addNotes    string
	addParent   string
)

var addCmd = &cobra.Command{
	Use:   "add <title>",
	Short: "Add a reminder",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		title := args[0]
		if addListName == "" {
			return fmt.Errorf("list is required (use -l \"<list-name>\")")
		}
		if err := syncEngine.Sync(false); err != nil {
			return err
		}
		result, err := w.AddReminder(title, addListName, addDue, addPriority, addNotes, addParent)
		if err != nil {
			return err
		}
		if errMsg, ok := result["error"].(string); ok {
			fmt.Fprintf(os.Stderr, "❌ %s\n", errMsg)
			os.Exit(1)
		}
		listStr := ""
		if addListName != "" {
			listStr = fmt.Sprintf(" → %s", addListName)
		}
		parentStr := ""
		if addParent != "" {
			parentStr = fmt.Sprintf(" (subtask of %s)", addParent)
		}
		fmt.Printf("✅ Added: '%s'%s%s\n", title, listStr, parentStr)
		return nil
	},
}

var (
	batchListName string
	batchParent   string
)

var addBatchCmd = &cobra.Command{
	Use:   "add-batch <title>...",
	Short: "Add multiple reminders at once",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if batchListName == "" {
			return fmt.Errorf("list is required (use -l \"<list-name>\")")
		}
		if err := syncEngine.Sync(false); err != nil {
			return err
		}
		result, err := w.AddRemindersBatch(args, batchListName, batchParent)
		if err != nil {
			return err
		}
		if errMsg, ok := result["error"].(string); ok {
			fmt.Fprintf(os.Stderr, "❌ %s\n", errMsg)
			os.Exit(1)
		}
		count := len(args)
		if c, ok := result["created_count"].(int); ok {
			count = c
		}
		listStr := ""
		if batchListName != "" {
			listStr = fmt.Sprintf(" → %s", batchListName)
		}
		parentStr := ""
		if batchParent != "" {
			parentStr = fmt.Sprintf(" (subtasks of %s)", batchParent)
		}
		fmt.Printf("✅ Added %d reminders%s%s:\n", count, listStr, parentStr)
		titles := args
		if t, ok := result["titles"].([]string); ok {
			titles = t
		}
		for _, t := range titles {
			fmt.Printf("   • %s\n", t)
		}
		return nil
	},
}

func init() {
	addCmd.Flags().StringVarP(&addListName, "list", "l", "", "List name")
	addCmd.Flags().StringVarP(&addDue, "due", "d", "", "Due date (YYYY-MM-DD)")
	addCmd.Flags().StringVarP(&addPriority, "priority", "p", "", "Priority (high, medium, low)")
	addCmd.Flags().StringVarP(&addNotes, "notes", "n", "", "Notes")
	addCmd.Flags().StringVar(&addParent, "parent", "", "Parent reminder ID (creates subtask)")

	addBatchCmd.Flags().StringVarP(&batchListName, "list", "l", "", "List name")
	addBatchCmd.Flags().StringVar(&batchParent, "parent", "", "Parent reminder ID (creates subtasks)")
}
