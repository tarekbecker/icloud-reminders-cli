package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"icloud-reminders/models"
)

var jsonCmd = &cobra.Command{
	Use:   "json",
	Short: "Output reminders as JSON",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := syncEngine.Sync(false); err != nil {
			return err
		}
		reminders := syncEngine.GetReminders(true)
		lists := syncEngine.GetLists()

		type output struct {
			Lists     []*models.ReminderList `json:"lists"`
			Active    []*models.Reminder     `json:"active"`
			Completed []*models.Reminder     `json:"completed"`
		}
		var active, completed []*models.Reminder
		for _, r := range reminders {
			if r.Completed {
				completed = append(completed, r)
			} else {
				active = append(active, r)
			}
		}

		out := output{
			Lists:     lists,
			Active:    active,
			Completed: completed,
		}
		data, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	},
}
