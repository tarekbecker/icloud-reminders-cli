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
	AssigneeID     *string `json:"assignee_id,omitempty"`
	SectionRef     *string `json:"section_ref,omitempty"`
	SectionName    string  `json:"section_name,omitempty"`
	SortIndex      int     `json:"sort_index"`
	Depth          int     `json:"depth"`
}

// ReminderNode nests only reminders included in the returned page. ParentRef
// remains authoritative when an ancestor was filtered out or is on another page.
type ReminderNode struct {
	Reminder *Reminder       `json:"reminder"`
	Subtasks []*ReminderNode `json:"subtasks"`
}

type ReminderSection struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	ListID    string `json:"list_id"`
	SortIndex int    `json:"sort_index"`
}

// RecordScope keeps records in their original CloudKit database and zone.
type RecordScope struct {
	Database        string `json:"database"`
	ZoneName        string `json:"zone_name"`
	OwnerRecordName string `json:"owner_record_name"`
}

func (s RecordScope) Key() string {
	return s.Database + "\x00" + s.ZoneName + "\x00" + s.OwnerRecordName
}

type ReminderParticipant struct {
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	Email         string `json:"email,omitempty"`
	Phone         string `json:"phone,omitempty"`
	Role          string `json:"role"`
	Permission    string `json:"permission"`
	IsCurrentUser bool   `json:"is_current_user"`
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

// StructureIssue contains identifiers and fixed parser diagnostics only, never
// document contents, asset URLs, titles or participant data.
type StructureIssue struct {
	ErrorCode  string `json:"error_code"`
	ListID     string `json:"list_id"`
	RecordType string `json:"record_type"`
	Field      string `json:"structure_field"`
	Reason     string `json:"structure_reason"`
	Version    *int   `json:"structure_version,omitempty"`
}

// PriorityMap maps string priority names to CloudKit integer values.
var PriorityMap = map[string]int{
	"high":   1,
	"medium": 5,
	"low":    9,
	"none":   0,
}

// ListRefValue is the exact list identity; names need not be unique.
func (r *Reminder) ListRefValue() string {
	if r.ListRef == nil {
		return ""
	}
	return *r.ListRef
}
