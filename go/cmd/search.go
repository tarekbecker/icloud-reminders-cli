package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

var searchAll bool

var searchCmd = &cobra.Command{
	Use:   "search <query>",
	Short: "Search reminders by title",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		query := args[0]
		if err := syncEngine.Sync(false); err != nil {
			return err
		}
		reminders := syncEngine.GetReminders(searchAll)

		queryLower := strings.ToLower(query)
		var matches []*struct {
			title    string
			due      string
			shortID  string
			listName string
			done     bool
		}
		for _, r := range reminders {
			if strings.Contains(strings.ToLower(r.Title), queryLower) {
				due := ""
				if r.Due != nil {
					due = *r.Due
				}
				matches = append(matches, &struct {
					title    string
					due      string
					shortID  string
					listName string
					done     bool
				}{r.Title, due, r.ShortID(), r.ListName, r.Completed})
			}
		}

		fmt.Printf("\n🔍 Search: '%s' → %d matches\n", query, len(matches))
		for _, m := range matches {
			status := "•"
			if m.done {
				status = "✓"
			}
			due := ""
			if m.due != "" {
				due = fmt.Sprintf("  [due %s]", m.due)
			}
			fmt.Printf("  %s %s%s  (%s) — %s\n", status, m.title, due, m.shortID, m.listName)
		}
		return nil
	},
}

func init() {
	searchCmd.Flags().BoolVarP(&searchAll, "all", "a", false, "Include completed reminders")
}
