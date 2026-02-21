// Package sync implements the CloudKit delta synchronization engine.
package sync

import (
	"fmt"
	"log"

	"icloud-reminders/cache"
	"icloud-reminders/cloudkit"
	"icloud-reminders/models"
	"icloud-reminders/utils"
)

// Engine handles syncing reminders with CloudKit.
type Engine struct {
	CK    *cloudkit.Client
	Cache *cache.Cache
}

// New creates a new sync engine.
func New(ck *cloudkit.Client) *Engine {
	return &Engine{
		CK:    ck,
		Cache: cache.Load(),
	}
}

// Sync performs a delta or full sync from CloudKit.
func (e *Engine) Sync(force bool) error {
	if force {
		e.Cache = cache.NewCache()
		log.Println("Full sync (forced)...")
	} else if e.Cache.SyncToken != nil && *e.Cache.SyncToken != "" {
		log.Println("Delta sync...")
	} else {
		log.Println("Full sync (first run)...")
	}

	// Get owner ID
	if e.Cache.OwnerID == nil || *e.Cache.OwnerID == "" {
		ownerID, err := e.CK.GetOwnerID()
		if err != nil {
			return fmt.Errorf("get owner ID: %w", err)
		}
		e.Cache.OwnerID = &ownerID
	}
	ownerID := *e.Cache.OwnerID

	page := 0
	total := 0

	for {
		page++
		syncToken := ""
		if e.Cache.SyncToken != nil {
			syncToken = *e.Cache.SyncToken
		}
		data, err := e.CK.ChangesZone(ownerID, syncToken)
		if err != nil {
			return fmt.Errorf("changes/zone page %d: %w", page, err)
		}

		zones, _ := data["zones"].([]interface{})
		if len(zones) == 0 {
			break
		}
		zoneResp, _ := zones[0].(map[string]interface{})
		records, _ := zoneResp["records"].([]interface{})
		moreComing, _ := zoneResp["moreComing"].(bool)
		newToken, _ := zoneResp["syncToken"].(string)

		total += len(records)
		if len(records) > 0 {
			log.Printf("  Page %d: +%d records", page, len(records))
		}

		e.processRecords(records)

		if newToken != "" {
			e.Cache.SyncToken = &newToken
		}
		if !moreComing {
			break
		}
	}

	_ = total
	if err := e.Cache.Save(); err != nil {
		return fmt.Errorf("save cache: %w", err)
	}

	active := 0
	for _, r := range e.Cache.Reminders {
		if !r.Completed {
			active++
		}
	}
	log.Printf("  Synced: %d reminders (%d active), %d lists",
		len(e.Cache.Reminders), active, len(e.Cache.Lists))
	return nil
}

// processRecords processes CloudKit records into the local cache.
func (e *Engine) processRecords(records []interface{}) {
	for _, rec := range records {
		r, ok := rec.(map[string]interface{})
		if !ok {
			continue
		}
		rname, _ := r["recordName"].(string)
		rtype, _ := r["recordType"].(string)
		deleted, _ := r["deleted"].(bool)
		fields, _ := r["fields"].(map[string]interface{})
		if fields == nil {
			fields = map[string]interface{}{}
		}

		// Check soft-delete flag
		if getFieldInt(fields, "Deleted") != 0 {
			deleted = true
		}

		switch rtype {
		case "ReminderList", "List":
			if deleted {
				delete(e.Cache.Lists, rname)
			} else {
				title := getFieldString(fields, "Name")
				if title == "" {
					title = utils.ExtractTitle(getFieldString(fields, "TitleDocument"))
				}
				if title != "" {
					e.Cache.Lists[rname] = title
				}
			}

		case "Reminder":
			if deleted {
				delete(e.Cache.Reminders, rname)
			} else {
				title := utils.ExtractTitle(getFieldString(fields, "TitleDocument"))
				if title == "" {
					title = "(untitled)"
				}

				var dueStr, completionStr *string
				if due := getFieldInt64(fields, "DueDate"); due != 0 {
					s := utils.TsToStr(due)
					dueStr = &s
				}
				if cd := getFieldInt64(fields, "CompletionDate"); cd != 0 {
					s := utils.TsToStr(cd)
					completionStr = &s
				}

				listRef := getFieldRefName(fields, "List")
				parentRef := getFieldRefName(fields, "ParentReminder")
				notes := utils.ExtractTitle(getFieldString(fields, "NotesDocument"))
				priority := getFieldInt(fields, "Priority")
				changeTag, _ := r["recordChangeTag"].(string)

				modified, _ := r["modified"].(map[string]interface{})
				var modTS *int64
				if ts, ok := modified["timestamp"].(float64); ok {
					v := int64(ts)
					modTS = &v
				}

				rd := &cache.ReminderData{
					Title:          title,
					Completed:      getFieldInt(fields, "Completed") != 0,
					CompletionDate: completionStr,
					Due:            dueStr,
					Priority:       priority,
					ModifiedTS:     modTS,
				}
				if notes != "" {
					rd.Notes = &notes
				}
				if listRef != "" {
					rd.ListRef = &listRef
				}
				if parentRef != "" {
					rd.ParentRef = &parentRef
				}
				if changeTag != "" {
					rd.ChangeTag = &changeTag
				}
				e.Cache.Reminders[rname] = rd
			}
		}
	}
}

// GetReminders returns reminders as typed objects.
func (e *Engine) GetReminders(includeCompleted bool) []*models.Reminder {
	var result []*models.Reminder
	for rid, data := range e.Cache.Reminders {
		if !includeCompleted && data.Completed {
			continue
		}
		r := &models.Reminder{
			ID:             rid,
			Title:          data.Title,
			Completed:      data.Completed,
			CompletionDate: data.CompletionDate,
			Due:            data.Due,
			Priority:       data.Priority,
			Notes:          data.Notes,
			ListRef:        data.ListRef,
			ParentRef:      data.ParentRef,
			ModifiedTS:     data.ModifiedTS,
		}
		if data.ListRef != nil {
			if name, ok := e.Cache.Lists[*data.ListRef]; ok {
				r.ListName = name
			} else {
				r.ListName = "?"
			}
		} else {
			r.ListName = "?"
		}
		result = append(result, r)
	}
	return result
}

// GetLists returns all reminder lists.
func (e *Engine) GetLists() []*models.ReminderList {
	var result []*models.ReminderList
	for id, name := range e.Cache.Lists {
		result = append(result, &models.ReminderList{ID: id, Name: name})
	}
	return result
}

// FindListByName finds a list ID by name (case-insensitive).
func (e *Engine) FindListByName(name string) string {
	nameLower := toLower(name)
	for id, n := range e.Cache.Lists {
		if toLower(n) == nameLower {
			return id
		}
	}
	return ""
}

// FindReminderByID finds a full reminder ID by partial prefix match.
func (e *Engine) FindReminderByID(partialID string) string {
	partial := toLower(partialID)
	for rid := range e.Cache.Reminders {
		uuidPart := rid
		for i := len(rid) - 1; i >= 0; i-- {
			if rid[i] == '/' {
				uuidPart = rid[i+1:]
				break
			}
		}
		if len(uuidPart) >= len(partial) && toLower(uuidPart[:len(partial)]) == partial {
			return rid
		}
	}
	return ""
}

// --- field extraction helpers ---

func getFieldString(fields map[string]interface{}, key string) string {
	f, _ := fields[key].(map[string]interface{})
	v, _ := f["value"].(string)
	return v
}

func getFieldInt(fields map[string]interface{}, key string) int {
	f, _ := fields[key].(map[string]interface{})
	switch v := f["value"].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

func getFieldInt64(fields map[string]interface{}, key string) int64 {
	f, _ := fields[key].(map[string]interface{})
	switch v := f["value"].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	}
	return 0
}

func getFieldRefName(fields map[string]interface{}, key string) string {
	f, _ := fields[key].(map[string]interface{})
	v, _ := f["value"].(map[string]interface{})
	name, _ := v["recordName"].(string)
	return name
}

func toLower(s string) string {
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
