// Package models contains data types for iCloud Reminders.
package models

// Reminder represents a single iCloud Reminder.
type Reminder struct {
	ID             string  `json:"id"`
	Title          string  `json:"title"`
	Completed      bool    `json:"completed"`
	CompletionDate *string `json:"completion_date,omitempty"`
	Due            *string `json:"due,omitempty"`
	Priority       int     `json:"priority"` // 0=none, 1=high, 5=medium, 9=low
	Notes          *string `json:"notes,omitempty"`
	ListRef        *string `json:"list_ref,omitempty"`
	ListName       string  `json:"list_name"`
	ParentRef      *string `json:"parent_ref,omitempty"`
	ModifiedTS     *int64  `json:"modified_ts,omitempty"`
}

// PriorityLabel returns a human-readable priority string.
func (r *Reminder) PriorityLabel() string {
	switch r.Priority {
	case 1:
		return "high"
	case 5:
		return "medium"
	case 9:
		return "low"
	}
	return ""
}

// ShortID returns the full UUID portion of the reminder ID (strips "Reminder/" or "List/" prefix).
func (r *Reminder) ShortID() string {
	if r.ID == "" {
		return ""
	}
	id := r.ID
	// Strip prefix like "Reminder/" or "List/"
	for i := len(id) - 1; i >= 0; i-- {
		if id[i] == '/' {
			id = id[i+1:]
			break
		}
	}
	return id
}

// ReminderList represents an iCloud Reminders list.
type ReminderList struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// PriorityMap maps string priority names to CloudKit integer values.
var PriorityMap = map[string]int{
	"high":   1,
	"medium": 5,
	"low":    9,
	"none":   0,
}
