package cmd

import (
	"fmt"
	"sort"

	"github.com/spf13/cobra"

	"icloud-reminders/pkg/models"
)

var listsCmd = &cobra.Command{
	Use:   "lists",
	Short: "Show all reminder lists",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := syncEngine.Sync(false); err != nil {
			return err
		}
		lists := syncEngine.GetLists()

		sort.Slice(lists, func(i, j int) bool {
			return lists[i].Name < lists[j].Name
		})

		fmt.Printf("\n📋 Lists (%d)\n", len(lists))
		for _, lst := range lists {
			count := activeCountForList(lst)
			fmt.Printf("  • %s (%d active)  [%s]\n", lst.Name, count, shortID(lst.ID))
		}
		return nil
	},
}

func activeCountForList(lst *models.ReminderList) int {
	count := 0
	for _, r := range syncEngine.Cache.Reminders {
		if r.ListRef != nil && *r.ListRef == lst.ID && !r.Completed {
			count++
		}
	}
	return count
}

func shortID(id string) string {
	for i := len(id) - 1; i >= 0; i-- {
		if id[i] == '/' {
			id = id[i+1:]
			break
		}
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
