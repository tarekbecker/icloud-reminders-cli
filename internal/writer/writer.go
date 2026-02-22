// Package writer handles write operations for iCloud Reminders.
package writer

import (
	"fmt"
	"time"

	"icloud-reminders/internal/cache"
	"icloud-reminders/internal/cloudkit"
	"icloud-reminders/internal/logger"
	"icloud-reminders/pkg/models"
	"icloud-reminders/internal/sync"
	"icloud-reminders/internal/utils"
)

// Writer handles creating and modifying reminders.
type Writer struct {
	CK   *cloudkit.Client
	Sync *sync.Engine
}

// New creates a new Writer.
func New(ck *cloudkit.Client, engine *sync.Engine) *Writer {
	return &Writer{CK: ck, Sync: engine}
}

// ownerID returns the cached or fetched owner ID.
func (w *Writer) ownerID() (string, error) {
	if w.Sync.Cache.OwnerID != nil && *w.Sync.Cache.OwnerID != "" {
		return *w.Sync.Cache.OwnerID, nil
	}
	id, err := w.CK.GetOwnerID()
	if err != nil {
		return "", err
	}
	w.Sync.Cache.OwnerID = &id
	return id, nil
}

// AddReminder adds a single reminder.
func (w *Writer) AddReminder(title, listName, dueDate, priority, notes, parentID string) (map[string]interface{}, error) {
	ownerID, err := w.ownerID()
	if err != nil {
		return errResult(err), nil
	}

	listID := ""
	if listName != "" {
		listID = w.Sync.FindListByName(listName)
		if listID == "" {
			return errResult(fmt.Errorf("list '%s' not found", listName)), nil
		}
	}

	parentRef := ""
	if parentID != "" {
		parentRef = w.Sync.FindReminderByID(parentID)
		if parentRef == "" {
			return errResult(fmt.Errorf("parent reminder '%s' not found", parentID)), nil
		}
		// Inherit list from parent if not specified
		if listID == "" {
			if pd := w.Sync.Cache.Reminders[parentRef]; pd != nil && pd.ListRef != nil {
				listID = *pd.ListRef
			}
		}
	}

	priorityVal := models.PriorityMap[priority]

	op, recordName, err := buildCreateOp(title, listID, parentRef, dueDate, priorityVal, notes)
	if err != nil {
		return errResult(err), nil
	}

	logger.Debugf("add: creating record %s in list %s", recordName, listID)
	result, err := w.CK.ModifyRecords(ownerID, []map[string]interface{}{op})
	if err != nil {
		return errResult(err), nil
	}

	if err := checkRecordErrors(result); err != nil {
		return errResult(err), nil
	}

	logger.Infof("Created reminder: %q → %s", title, listName)
	// Update cache
	rd := &cache.ReminderData{
		Title:    title,
		Priority: priorityVal,
	}
	if dueDate != "" {
		rd.Due = &dueDate
	}
	if notes != "" {
		rd.Notes = &notes
	}
	if listID != "" {
		rd.ListRef = &listID
	}
	if parentRef != "" {
		rd.ParentRef = &parentRef
	}
	ts := time.Now().UnixMilli()
	rd.ModifiedTS = &ts
	// Extract recordChangeTag from response so the reminder can be
	// immediately completed/deleted without requiring a sync first.
	if records, ok := result["records"].([]interface{}); ok && len(records) > 0 {
		if rec, ok := records[0].(map[string]interface{}); ok {
			if ct, ok := rec["recordChangeTag"].(string); ok && ct != "" {
				rd.ChangeTag = &ct
			}
		}
	}
	w.Sync.Cache.Reminders[recordName] = rd
	if err := w.Sync.Cache.Save(); err != nil {
		logger.Warnf("cache save failed: %v", err)
	}

	return result, nil
}

// AddRemindersBatch adds multiple reminders in a single CloudKit request.
func (w *Writer) AddRemindersBatch(titles []string, listName, parentID string) (map[string]interface{}, error) {
	if len(titles) == 0 {
		return errResult(fmt.Errorf("no titles provided")), nil
	}

	ownerID, err := w.ownerID()
	if err != nil {
		return errResult(err), nil
	}

	listID := ""
	if listName != "" {
		listID = w.Sync.FindListByName(listName)
		if listID == "" {
			return errResult(fmt.Errorf("list '%s' not found", listName)), nil
		}
	}

	parentRef := ""
	if parentID != "" {
		parentRef = w.Sync.FindReminderByID(parentID)
		if parentRef == "" {
			return errResult(fmt.Errorf("parent reminder '%s' not found", parentID)), nil
		}
		if listID == "" {
			if pd := w.Sync.Cache.Reminders[parentRef]; pd != nil && pd.ListRef != nil {
				listID = *pd.ListRef
			}
		}
	}

	type created struct {
		recordName string
		title      string
	}
	var ops []map[string]interface{}
	var createdList []created

	for _, title := range titles {
		op, recordName, err := buildCreateOp(title, listID, parentRef, "", 0, "")
		if err != nil {
			return errResult(err), nil
		}
		ops = append(ops, op)
		createdList = append(createdList, created{recordName, title})
	}

	logger.Debugf("add-batch: creating %d records in list %s", len(ops), listID)
	result, err := w.CK.ModifyRecords(ownerID, ops)
	if err != nil {
		return errResult(err), nil
	}
	if err := checkRecordErrors(result); err != nil {
		return errResult(err), nil
	}

	logger.Infof("Created %d reminders in %q", len(createdList), listName)
	now := time.Now().UnixMilli()
	for _, c := range createdList {
		rd := &cache.ReminderData{
			Title:      c.title,
			ModifiedTS: &now,
		}
		if listID != "" {
			rd.ListRef = &listID
		}
		if parentRef != "" {
			rd.ParentRef = &parentRef
		}
		w.Sync.Cache.Reminders[c.recordName] = rd
	}
	if err := w.Sync.Cache.Save(); err != nil {
		logger.Warnf("cache save failed: %v", err)
	}
	var titleList []string
	for _, c := range createdList {
		titleList = append(titleList, c.title)
	}
	result["created_count"] = len(createdList)
	result["titles"] = titleList

	return result, nil
}

// CompleteReminder marks a reminder as complete.
func (w *Writer) CompleteReminder(reminderID string) (map[string]interface{}, error) {
	ownerID, err := w.ownerID()
	if err != nil {
		return errResult(err), nil
	}

	fullID := w.Sync.FindReminderByID(reminderID)
	if fullID == "" {
		return errResult(fmt.Errorf("reminder '%s' not found", reminderID)), nil
	}

	rd := w.Sync.Cache.Reminders[fullID]
	if rd == nil || rd.ChangeTag == nil || *rd.ChangeTag == "" {
		return errResult(fmt.Errorf("missing change tag for '%s' — try running 'sync' first", reminderID)), nil
	}

	now := time.Now().UnixMilli()
	op := map[string]interface{}{
		"operationType": "update",
		"record": map[string]interface{}{
			"recordType":      "Reminder",
			"recordName":      fullID,
			"recordChangeTag": *rd.ChangeTag,
			"fields": map[string]interface{}{
				"Completed":      map[string]interface{}{"value": true},
				"CompletionDate": map[string]interface{}{"value": now},
			},
		},
	}

	logger.Debugf("complete: updating record %s", fullID)
	result, err := w.CK.ModifyRecords(ownerID, []map[string]interface{}{op})
	if err != nil {
		return errResult(err), nil
	}
	if _, hasErr := result["error"]; !hasErr {
		rd.Completed = true
		nowStr := utils.TsToStr(now)
		rd.CompletionDate = &nowStr
		// Update change tag from response
		if records, ok := result["records"].([]interface{}); ok && len(records) > 0 {
			if rec, ok := records[0].(map[string]interface{}); ok {
				if ct, ok := rec["recordChangeTag"].(string); ok {
					rd.ChangeTag = &ct
				}
			}
		}
		if err := w.Sync.Cache.Save(); err != nil {
		logger.Warnf("cache save failed: %v", err)
	}
		logger.Infof("Completed reminder: %q (%s)", rd.Title, reminderID)
	}
	return result, nil
}

// DeleteReminder deletes a reminder.
func (w *Writer) DeleteReminder(reminderID string) (map[string]interface{}, error) {
	ownerID, err := w.ownerID()
	if err != nil {
		return errResult(err), nil
	}

	fullID := w.Sync.FindReminderByID(reminderID)
	if fullID == "" {
		return errResult(fmt.Errorf("reminder '%s' not found", reminderID)), nil
	}

	rd := w.Sync.Cache.Reminders[fullID]
	if rd == nil || rd.ChangeTag == nil || *rd.ChangeTag == "" {
		return errResult(fmt.Errorf("missing change tag for '%s' — try running 'sync' first", reminderID)), nil
	}

	op := map[string]interface{}{
		"operationType": "delete",
		"record": map[string]interface{}{
			"recordName":      fullID,
			"recordChangeTag": *rd.ChangeTag,
		},
	}

	title := ""
	if rd != nil {
		title = rd.Title
	}
	logger.Debugf("delete: removing record %s", fullID)
	result, err := w.CK.ModifyRecords(ownerID, []map[string]interface{}{op})
	if err != nil {
		return errResult(err), nil
	}
	if _, hasErr := result["error"]; !hasErr {
		delete(w.Sync.Cache.Reminders, fullID)
		if err := w.Sync.Cache.Save(); err != nil {
		logger.Warnf("cache save failed: %v", err)
	}
		logger.Infof("Deleted reminder: %q (%s)", title, reminderID)
	}
	return result, nil
}

// buildCreateOp builds a CloudKit create operation for a new reminder.
func buildCreateOp(title, listID, parentRef, dueDate string, priority int, notes string) (map[string]interface{}, string, error) {
	encoded, err := utils.EncodeTitle(title)
	if err != nil {
		return nil, "", fmt.Errorf("encode title: %w", err)
	}

	recordName := utils.NewUUIDString()
	fields := map[string]interface{}{
		"TitleDocument": map[string]interface{}{"value": encoded},
		"Completed":     map[string]interface{}{"value": 0},
	}

	if listID != "" {
		fields["List"] = map[string]interface{}{
			"value": map[string]interface{}{
				"recordName": listID,
				"action":     "NONE",
			},
		}
	}

	if parentRef != "" {
		fields["ParentReminder"] = map[string]interface{}{
			"value": map[string]interface{}{
				"recordName": parentRef,
				"action":     "NONE",
			},
		}
	}

	if dueDate != "" {
		ts, err := utils.StrToTs(dueDate)
		if err == nil {
			fields["DueDate"] = map[string]interface{}{"value": ts}
		}
	}

	if priority != 0 {
		fields["Priority"] = map[string]interface{}{"value": priority}
	}

	if notes != "" {
		encodedNotes, err := utils.EncodeTitle(notes)
		if err != nil {
			return nil, "", fmt.Errorf("encode notes: %w", err)
		}
		fields["NotesDocument"] = map[string]interface{}{"value": encodedNotes}
	}

	op := map[string]interface{}{
		"operationType": "create",
		"record": map[string]interface{}{
			"recordType": "Reminder",
			"recordName": recordName,
			"fields":     fields,
		},
	}
	return op, recordName, nil
}

func errResult(err error) map[string]interface{} {
	return map[string]interface{}{"error": err.Error()}
}

// checkRecordErrors extracts the first record-level error from CloudKit result.
// CloudKit returns errors like {"records": [{"serverErrorCode": "BAD_REQUEST", "reason": "..."}]}
func checkRecordErrors(result map[string]interface{}) error {
	records, ok := result["records"].([]interface{})
	if !ok {
		return nil
	}
	for _, r := range records {
		rec, ok := r.(map[string]interface{})
		if !ok {
			continue
		}
		if code, ok := rec["serverErrorCode"].(string); ok && code != "" {
			reason, _ := rec["reason"].(string)
			return fmt.Errorf("CloudKit error %s: %s", code, reason)
		}
	}
	return nil
}
