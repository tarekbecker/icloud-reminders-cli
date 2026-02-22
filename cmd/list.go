package cmd

import (
	"fmt"
	"sort"

	"github.com/spf13/cobra"

	"icloud-reminders/pkg/models"
)

var (
	listFilter       string
	listParentFilter string
	listAll          bool
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List reminders",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := syncEngine.Sync(false); err != nil {
			return err
		}
		reminders := syncEngine.GetReminders(listAll)

		// --parent: show only children of a named parent reminder
		if listParentFilter != "" {
			return runListByParent(reminders, listParentFilter)
		}

		// Build lookup maps
		byList := make(map[string][]*models.Reminder)
		childrenByParent := make(map[string][]*models.Reminder)

		for _, r := range reminders {
			if listFilter != "" && toLowerStr(r.ListName) != toLowerStr(listFilter) {
				continue
			}
			if r.ParentRef != nil && *r.ParentRef != "" {
				childrenByParent[*r.ParentRef] = append(childrenByParent[*r.ParentRef], r)
			} else {
				byList[r.ListName] = append(byList[r.ListName], r)
			}
		}

		active := 0
		for _, r := range reminders {
			if !r.Completed {
				active++
			}
		}
		fmt.Printf("\n✅ Reminders: %d (%d active)\n", len(reminders), active)

		listNames := make([]string, 0, len(byList))
		for name := range byList {
			listNames = append(listNames, name)
		}
		sort.Strings(listNames)

		for _, listName := range listNames {
			items := byList[listName]
			total := len(items)
			for _, r := range items {
				total += len(childrenByParent[r.ID])
			}
			fmt.Printf("\n📋 %s (%d)\n", listName, total)

			sort.Slice(items, func(i, j int) bool {
				ti, tj := int64(0), int64(0)
				if items[i].ModifiedTS != nil {
					ti = *items[i].ModifiedTS
				}
				if items[j].ModifiedTS != nil {
					tj = *items[j].ModifiedTS
				}
				return ti > tj
			})

			for _, r := range items {
				printReminder(r, 2, childrenByParent)
			}
		}
		return nil
	},
}

// runListByParent shows only direct children of the named parent reminder.
func runListByParent(reminders []*models.Reminder, parentFilter string) error {
	// Find parent by name (case-insensitive) or short ID prefix
	byID := make(map[string]*models.Reminder)
	for _, r := range reminders {
		byID[r.ID] = r
	}

	filterLower := toLowerStr(parentFilter)
	var parentID string
	for _, r := range reminders {
		if toLowerStr(r.Title) == filterLower || toLowerStr(r.ShortID()) == filterLower {
			parentID = r.ID
			break
		}
	}
	// Also try as short ID prefix
	if parentID == "" {
		parentID = syncEngine.FindReminderByID(parentFilter)
	}
	if parentID == "" {
		return fmt.Errorf("parent reminder %q not found", parentFilter)
	}

	parent := byID[parentID]
	parentTitle := parentFilter
	if parent != nil {
		parentTitle = parent.Title
	}

	var children []*models.Reminder
	for _, r := range reminders {
		if r.ParentRef != nil && *r.ParentRef == parentID {
			children = append(children, r)
		}
	}

	sort.Slice(children, func(i, j int) bool {
		ti, tj := int64(0), int64(0)
		if children[i].ModifiedTS != nil {
			ti = *children[i].ModifiedTS
		}
		if children[j].ModifiedTS != nil {
			tj = *children[j].ModifiedTS
		}
		return ti > tj
	})

	fmt.Printf("\n📋 %s (%d items)\n", parentTitle, len(children))
	for _, r := range children {
		status := "•"
		if r.Completed {
			status = "✓"
		}
		due := ""
		if r.Due != nil && *r.Due != "" {
			due = fmt.Sprintf("  [due %s]", *r.Due)
		}
		prio := ""
		if r.PriorityLabel() != "" {
			prio = fmt.Sprintf("  [%s]", r.PriorityLabel())
		}
		fmt.Printf("  %s %s%s%s  (%s)\n", status, r.Title, due, prio, r.ShortID())
	}
	return nil
}

func printReminder(r *models.Reminder, indent int, childrenByParent map[string][]*models.Reminder) {
	prefix := spaces(indent)
	status := "•"
	if r.Completed {
		status = "✓"
	}
	due := ""
	if r.Due != nil && *r.Due != "" {
		due = fmt.Sprintf("  [due %s]", *r.Due)
	}
	prio := ""
	if r.PriorityLabel() != "" {
		prio = fmt.Sprintf("  [%s]", r.PriorityLabel())
	}
	fmt.Printf("%s%s %s%s%s  (%s)\n", prefix, status, r.Title, due, prio, r.ShortID())

	// Print children recursively
	children := childrenByParent[r.ID]
	sort.Slice(children, func(i, j int) bool {
		ti, tj := int64(0), int64(0)
		if children[i].ModifiedTS != nil {
			ti = *children[i].ModifiedTS
		}
		if children[j].ModifiedTS != nil {
			tj = *children[j].ModifiedTS
		}
		return ti > tj
	})
	for _, child := range children {
		printReminder(child, indent+2, childrenByParent)
	}
}

func spaces(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = ' '
	}
	return string(b)
}

func toLowerStr(s string) string {
	result := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		result[i] = c
	}
	return string(result)
}

func init() {
	listCmd.Flags().StringVarP(&listFilter, "list", "l", "", "Filter by list name")
	listCmd.Flags().StringVar(&listParentFilter, "parent", "", "Show only children of this parent reminder (name or ID)")
	listCmd.Flags().BoolVarP(&listAll, "all", "a", false, "Include completed reminders")
}
