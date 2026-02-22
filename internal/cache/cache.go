// Package cache manages the local JSON cache for iCloud Reminders.
package cache

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// ConfigDir is the default config/session directory.
var ConfigDir = filepath.Join(os.Getenv("HOME"), ".config", "icloud-reminders")

// CacheFile is the path to the reminders JSON cache file.
var CacheFile = filepath.Join(ConfigDir, "ck_cache.json")

// SessionFile is the path to the auth session JSON file.
var SessionFile = filepath.Join(ConfigDir, "session.json")

// ReminderData holds raw cached data for a single reminder.
type ReminderData struct {
	Title          string  `json:"title"`
	Completed      bool    `json:"completed"`
	CompletionDate *string `json:"completion_date,omitempty"`
	Due            *string `json:"due,omitempty"`
	Priority       int     `json:"priority"`
	Notes          *string `json:"notes,omitempty"`
	ListRef        *string `json:"list_ref,omitempty"`
	ParentRef      *string `json:"parent_ref,omitempty"`
	ModifiedTS     *int64  `json:"modified_ts,omitempty"`
	ChangeTag      *string `json:"change_tag,omitempty"`
}

// Cache holds the local cache of reminders and lists.
type Cache struct {
	Reminders map[string]*ReminderData `json:"reminders"`
	Lists     map[string]string        `json:"lists"`
	SyncToken *string                  `json:"sync_token,omitempty"`
	OwnerID   *string                  `json:"owner_id,omitempty"`
	UpdatedAt *string                  `json:"updated_at,omitempty"`
}

// NewCache returns an empty Cache.
func NewCache() *Cache {
	return &Cache{
		Reminders: make(map[string]*ReminderData),
		Lists:     make(map[string]string),
	}
}

// Load loads the cache from disk; returns empty cache on error.
func Load() *Cache {
	data, err := os.ReadFile(CacheFile)
	if err != nil {
		return NewCache()
	}
	var c Cache
	if err := json.Unmarshal(data, &c); err != nil {
		return NewCache()
	}
	if c.Reminders == nil {
		c.Reminders = make(map[string]*ReminderData)
	}
	if c.Lists == nil {
		c.Lists = make(map[string]string)
	}
	return &c
}

// Save writes the cache to disk.
func (c *Cache) Save() error {
	if err := os.MkdirAll(ConfigDir, 0700); err != nil {
		return err
	}
	now := time.Now().Format("2006-01-02T15:04:05")
	c.UpdatedAt = &now
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(CacheFile, data, 0600)
}
