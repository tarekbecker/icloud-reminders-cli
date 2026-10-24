// Package cache manages the local JSON cache for iCloud Reminders.
package cache

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"icloud-reminders/internal/storage"
	"icloud-reminders/pkg/models"
)

// ConfigDir is the default config/session directory.
var ConfigDir = filepath.Join(os.Getenv("HOME"), ".config", "icloud-reminders")

// CacheFile is the path to the reminders JSON cache file.
var CacheFile = filepath.Join(ConfigDir, "ck_cache.json")

// SessionFile is the path to the auth session JSON file.
var SessionFile = filepath.Join(ConfigDir, "session.json")

// ReminderData holds raw cached data for a single reminder.
type ReminderData struct {
	Title              string   `json:"title"`
	Completed          bool     `json:"completed"`
	CompletionDate     *string  `json:"completion_date,omitempty"`
	Due                *string  `json:"due,omitempty"`
	Priority           int      `json:"priority"`
	Notes              *string  `json:"notes,omitempty"`
	ListRef            *string  `json:"list_ref,omitempty"`
	ParentRef          *string  `json:"parent_ref,omitempty"`
	ModifiedTS         *int64   `json:"modified_ts,omitempty"`
	ChangeTag          *string  `json:"change_tag,omitempty"`
	AssignmentIDs      []string `json:"assignment_ids,omitempty"`
	ResolutionTokenMap string   `json:"resolution_token_map,omitempty"`
}

type AssignmentData struct {
	ReminderID string `json:"reminder_id"`
	AssigneeID string `json:"assignee_id"`
	ChangeTag  string `json:"change_tag"`
	Status     int    `json:"status"`
}

type SectionData struct {
	Title     string `json:"title"`
	ListID    string `json:"list_id"`
	ChangeTag string `json:"change_tag"`
}

type ListStructure struct {
	ReminderIDs      []string          `json:"reminder_ids"`
	SectionIDs       []string          `json:"section_ids"`
	Memberships      map[string]string `json:"memberships"`
	ChangeTag        string            `json:"change_tag"`
	RecordType       string            `json:"record_type"`
	MembershipsData  json.RawMessage   `json:"memberships_data,omitempty"`
	SectionOrderData json.RawMessage   `json:"section_order_data,omitempty"`
}

// Cache holds the local cache of reminders and lists.
type Cache struct {
	directory           string
	Reminders           map[string]*ReminderData          `json:"reminders"`
	Lists               map[string]string                 `json:"lists"`
	SyncToken           *string                           `json:"sync_token,omitempty"`
	OwnerID             *string                           `json:"owner_id,omitempty"`
	UpdatedAt           *string                           `json:"updated_at,omitempty"`
	SchemaVersion       int                               `json:"schema_version,omitempty"`
	ZoneTokens          map[string]string                 `json:"zone_tokens,omitempty"`
	Scopes              map[string]models.RecordScope     `json:"record_scopes,omitempty"`
	ListShares          map[string]string                 `json:"list_shares,omitempty"`
	Assignments         map[string]*AssignmentData        `json:"assignments,omitempty"`
	Sections            map[string]*SectionData           `json:"sections"`
	Structures          map[string]*ListStructure         `json:"structures"`
	StructureIssues     map[string]*models.StructureIssue `json:"structure_issues"`
	PendingReadbacks    map[string]models.RecordScope     `json:"pending_readbacks,omitempty"`
	TitleDecoderVersion int                               `json:"title_decoder_version,omitempty"`
}

// NewCache returns an empty Cache.
func NewCache() *Cache {
	return &Cache{
		Reminders:        make(map[string]*ReminderData),
		Lists:            make(map[string]string),
		ZoneTokens:       make(map[string]string),
		Scopes:           make(map[string]models.RecordScope),
		ListShares:       make(map[string]string),
		Assignments:      make(map[string]*AssignmentData),
		Sections:         make(map[string]*SectionData),
		Structures:       make(map[string]*ListStructure),
		StructureIssues:  make(map[string]*models.StructureIssue),
		PendingReadbacks: make(map[string]models.RecordScope),
	}
}

// Load loads the cache from disk; returns empty cache on error.
func Load() *Cache {
	return LoadFrom(ConfigDir)
}

// LoadFrom loads an account's cache from its configured data directory.
func LoadFrom(directory string) *Cache {
	c := NewCache()
	c.directory = directory
	data, err := os.ReadFile(filepath.Join(directory, "ck_cache.json"))
	if err != nil {
		return c
	}
	if err := json.Unmarshal(data, c); err != nil {
		c = NewCache()
		c.directory = directory
		return c
	}
	if c.TitleDecoderVersion != 1 {
		c = NewCache()
		c.directory = directory
		return c
	}
	if c.Reminders == nil {
		c.Reminders = make(map[string]*ReminderData)
	}
	if c.PendingReadbacks == nil {
		c.PendingReadbacks = make(map[string]models.RecordScope)
	}
	if c.StructureIssues == nil {
		c.StructureIssues = make(map[string]*models.StructureIssue)
		c.SchemaVersion = 0
	}
	if c.Lists == nil {
		c.Lists = make(map[string]string)
	}
	if c.Sections == nil || c.Structures == nil {
		c.SchemaVersion = 0
		c.Sections = make(map[string]*SectionData)
		c.Structures = make(map[string]*ListStructure)
	}
	if c.ZoneTokens == nil || c.Scopes == nil || c.ListShares == nil || c.Assignments == nil {
		c.SchemaVersion = 0
		c.ZoneTokens = make(map[string]string)
		c.Scopes = make(map[string]models.RecordScope)
		c.ListShares = make(map[string]string)
		c.Assignments = make(map[string]*AssignmentData)
	}
	return c
}

// Save writes the cache to disk.
func (c *Cache) Save() error {
	directory := c.directory
	if directory == "" {
		directory = ConfigDir
	}
	c.TitleDecoderVersion = 1
	now := time.Now().Format("2006-01-02T15:04:05")
	c.UpdatedAt = &now
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return storage.WritePrivate(filepath.Join(directory, "ck_cache.json"), data)
}

// Reset clears sync state while retaining the account's storage location.
func (c *Cache) Reset() *Cache {
	fresh := NewCache()
	fresh.directory = c.directory
	return fresh
}

// SetDirectory configures storage for a CLI invocation before any operation.
func SetDirectory(directory string) {
	ConfigDir = directory
	CacheFile = filepath.Join(directory, "ck_cache.json")
	SessionFile = filepath.Join(directory, "session.json")
}
