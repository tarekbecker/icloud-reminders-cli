// Package writer handles write operations for iCloud Reminders.
package writer

import (
	"errors"
	"fmt"
	"time"

	"icloud-reminders/internal/cache"
	"icloud-reminders/internal/cloudkit"
	"icloud-reminders/internal/logger"
	"icloud-reminders/internal/sync"
	"icloud-reminders/internal/utils"
	"icloud-reminders/pkg/models"
)

// Writer handles creating and modifying reminders.
type Writer struct {
	CK   *cloudkit.Client
	Sync *sync.Engine
	// CreateRecordID is reserved durably by the server for keyed creations.
	CreateRecordID string
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
		return nil, err
	}

	listID := ""
	if listName != "" {
		listID = w.Sync.FindListByName(listName)
		if listID == "" {
			return nil, fmt.Errorf("list '%s' not found", listName)
		}
	}

	parentRef := ""
	if parentID != "" {
		parentRef = w.Sync.FindReminderByID(parentID)
		if parentRef == "" {
			return nil, fmt.Errorf("parent reminder '%s' not found", parentID)
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
		return nil, err
	}

	if w.CreateRecordID != "" {
		recordName = w.CreateRecordID
		op["record"].(map[string]interface{})["recordName"] = recordName
	}
	logger.Debugf("add: creating record %s in list %s", recordName, listID)
	result, err := w.modifyInScope(listID, ownerID, []map[string]interface{}{op})
	if err != nil {
		return nil, err
	}

	if err := checkRecordErrors(result); err != nil {
		return nil, err
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
	rd.ResolutionTokenMap, _ = cloudkit.FieldValue(op["record"].(map[string]interface{})["fields"].(map[string]interface{}), "ResolutionTokenMap").(string)
	w.Sync.Cache.Reminders[recordName] = rd
	if scope, ok := w.Sync.Cache.Scopes[listID]; ok {
		w.Sync.Cache.Scopes[recordName] = scope
	}
	if err := w.Sync.Cache.Save(); err != nil {
		logger.Warnf("cache save failed: %v", err)
	}

	result["id"] = recordName
	return result, nil
}

// AddRemindersBatch adds multiple reminders in a single CloudKit request.
func (w *Writer) AddRemindersBatch(titles []string, listName, parentID string) (map[string]interface{}, error) {
	if len(titles) == 0 {
		return nil, fmt.Errorf("no titles provided")
	}

	ownerID, err := w.ownerID()
	if err != nil {
		return nil, err
	}

	listID := ""
	if listName != "" {
		listID = w.Sync.FindListByName(listName)
		if listID == "" {
			return nil, fmt.Errorf("list '%s' not found", listName)
		}
	}

	parentRef := ""
	if parentID != "" {
		parentRef = w.Sync.FindReminderByID(parentID)
		if parentRef == "" {
			return nil, fmt.Errorf("parent reminder '%s' not found", parentID)
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
			return nil, err
		}
		ops = append(ops, op)
		createdList = append(createdList, created{recordName, title})
	}

	logger.Debugf("add-batch: creating %d records in list %s", len(ops), listID)
	result, err := w.modifyInScope(listID, ownerID, ops)
	if err != nil {
		return nil, err
	}
	if err := checkRecordErrors(result); err != nil {
		return nil, err
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
		if scope, ok := w.Sync.Cache.Scopes[listID]; ok {
			w.Sync.Cache.Scopes[c.recordName] = scope
		}
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
		return nil, err
	}

	fullID := w.Sync.FindReminderByID(reminderID)
	if fullID == "" {
		return nil, fmt.Errorf("reminder '%s' not found", reminderID)
	}

	rd := w.Sync.Cache.Reminders[fullID]
	if rd == nil || rd.ChangeTag == nil || *rd.ChangeTag == "" {
		return nil, fmt.Errorf("missing change tag for '%s' — try running 'sync' first", reminderID)
	}

	now := time.Now().UnixMilli()
	tokens, err := cloudkit.BumpResolutionTokens(rd.ResolutionTokenMap, []string{"completed", "completionDate"})
	if err != nil {
		return nil, err
	}
	op := map[string]interface{}{
		"operationType": "update",
		"record": map[string]interface{}{
			"recordType":      "Reminder",
			"recordName":      fullID,
			"recordChangeTag": *rd.ChangeTag,
			"fields": map[string]interface{}{
				"Completed":          map[string]interface{}{"value": 1},
				"CompletionDate":     map[string]interface{}{"value": now},
				"ResolutionTokenMap": map[string]interface{}{"value": tokens},
			},
		},
	}

	logger.Debugf("complete: updating record %s", fullID)
	result, err := w.modifyInScope(fullID, ownerID, []map[string]interface{}{op})
	if err != nil {
		return nil, err
	}
	if err := checkRecordErrors(result); err != nil {
		return nil, err
	}
	if _, hasErr := result["error"]; !hasErr {
		rd.Completed = true
		rd.ResolutionTokenMap = tokens
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
		return nil, err
	}

	fullID := w.Sync.FindReminderByID(reminderID)
	if fullID == "" {
		return nil, fmt.Errorf("reminder '%s' not found", reminderID)
	}

	rd := w.Sync.Cache.Reminders[fullID]
	if rd == nil || rd.ChangeTag == nil || *rd.ChangeTag == "" {
		return nil, fmt.Errorf("missing change tag for '%s' — try running 'sync' first", reminderID)
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
	result, err := w.modifyInScope(fullID, ownerID, []map[string]interface{}{op})
	if err != nil {
		return nil, err
	}
	if err := checkRecordErrors(result); err != nil {
		return nil, err
	}
	if _, hasErr := result["error"]; !hasErr {
		delete(w.Sync.Cache.Reminders, fullID)
		delete(w.Sync.Cache.Scopes, fullID)
		if err := w.Sync.Cache.Save(); err != nil {
			logger.Warnf("cache save failed: %v", err)
		}
		logger.Infof("Deleted reminder: %q (%s)", title, reminderID)
	}
	return result, nil
}

// EditReminder updates one or more fields on an existing reminder.
// Pass non-empty values only for fields you want to change.
func (w *Writer) EditReminder(reminderID, title, dueDate, notes, priority string) (map[string]interface{}, error) {
	ownerID, err := w.ownerID()
	if err != nil {
		return nil, err
	}

	fullID := w.Sync.FindReminderByID(reminderID)
	if fullID == "" {
		return nil, fmt.Errorf("reminder '%s' not found", reminderID)
	}

	rd := w.Sync.Cache.Reminders[fullID]
	if rd == nil || rd.ChangeTag == nil || *rd.ChangeTag == "" {
		return nil, fmt.Errorf("missing change tag for '%s' — try running 'sync' first", reminderID)
	}

	if title == "" && dueDate == "" && notes == "" && priority == "" {
		return nil, fmt.Errorf("no changes specified — use --title, --due, --notes, or --priority")
	}

	fields := map[string]interface{}{}
	var nativeFields map[string]interface{}
	if title != "" || notes != "" {
		client := w.CK
		if scope, ok := w.Sync.Cache.Scopes[fullID]; ok {
			client = client.ForScope(scope)
		}
		records, err := client.LookupRecords(ownerID, []string{fullID})
		if err != nil {
			return nil, &cloudkit.WriteFailure{Status: "not_sent", Cause: err}
		}
		nativeFields, _ = records[0]["fields"].(map[string]interface{})
		freshTag, _ := records[0]["recordChangeTag"].(string)
		if nativeFields == nil || freshTag == "" {
			return nil, &cloudkit.WriteFailure{Status: "not_sent", Cause: fmt.Errorf("fresh reminder metadata unavailable")}
		}
		rd.ChangeTag = &freshTag
		rd.ResolutionTokenMap, _ = cloudkit.FieldValue(nativeFields, "ResolutionTokenMap").(string)
	}
	textError := func(field string) error {
		return &cloudkit.StructureError{Issue: models.StructureIssue{ErrorCode: "unsupported_structure", RecordType: "Reminder", Field: field, Reason: "invalid_entries"}}
	}

	if title != "" {
		oldDocument, _ := cloudkit.FieldValue(nativeFields, "TitleDocument").(string)
		encoded, err := utils.UpdateTitleDocument(oldDocument, title)
		if err != nil {
			return nil, textError("TitleDocument")
		}
		fields["TitleDocument"] = map[string]interface{}{"value": encoded}
	}

	if dueDate != "" {
		ts, err := utils.StrToTs(dueDate)
		if err == nil {
			fields["DueDate"] = map[string]interface{}{"value": ts}
		} else {
			return nil, fmt.Errorf("invalid due date %q (expected YYYY-MM-DD): %w", dueDate, err)
		}
	}

	if notes != "" {
		oldDocument, _ := cloudkit.FieldValue(nativeFields, "NotesDocument").(string)
		if oldDocument == "" && cloudkit.FieldValue(nativeFields, "NotesDocumentAsset") != nil {
			return nil, textError("NotesDocument")
		}
		var encodedNotes string
		var err error
		if oldDocument != "" {
			encodedNotes, err = utils.UpdateTitleDocument(oldDocument, notes)
		} else {
			encodedNotes, err = utils.EncodeTitle(notes)
		}
		if err != nil {
			return nil, textError("NotesDocument")
		}
		fields["NotesDocument"] = map[string]interface{}{"value": encodedNotes}
	}

	if priority != "" {
		priorityVal, ok := models.PriorityMap[priority]
		if !ok {
			return nil, fmt.Errorf("invalid priority %q (use: high, medium, low, none)", priority)
		}
		fields["Priority"] = map[string]interface{}{"value": priorityVal}
	}

	// Native clients resolve scalar field changes using this map. Updating the
	// CloudKit record alone does not advance the title/due/priority field clocks.
	keys := []string{}
	for field, key := range map[string]string{"TitleDocument": "titleDocument", "NotesDocument": "notesDocument", "DueDate": "dueDate", "Priority": "priority"} {
		if fields[field] != nil {
			keys = append(keys, key)
		}
	}
	tokens, err := cloudkit.BumpResolutionTokens(rd.ResolutionTokenMap, keys)
	if err != nil {
		return nil, err
	}
	fields["ResolutionTokenMap"] = map[string]interface{}{"value": tokens}

	op := map[string]interface{}{
		"operationType": "update",
		"record": map[string]interface{}{
			"recordType":      "Reminder",
			"recordName":      fullID,
			"recordChangeTag": *rd.ChangeTag,
			"fields":          fields,
		},
	}

	logger.Debugf("edit: updating record %s", fullID)
	result, err := w.modifyInScope(fullID, ownerID, []map[string]interface{}{op})
	if err != nil {
		return nil, err
	}
	if err := checkRecordErrors(result); err != nil {
		return nil, err
	}

	// Update local cache
	rd.ResolutionTokenMap = tokens
	if title != "" {
		rd.Title = title
	}
	if dueDate != "" {
		rd.Due = &dueDate
	}
	if notes != "" {
		rd.Notes = &notes
	}
	if priority != "" {
		rd.Priority = models.PriorityMap[priority]
	}
	// Update change tag from response
	if records, ok := result["records"].([]interface{}); ok && len(records) > 0 {
		if rec, ok := records[0].(map[string]interface{}); ok {
			if ct, ok := rec["recordChangeTag"].(string); ok && ct != "" {
				rd.ChangeTag = &ct
			}
		}
	}
	now := time.Now().UnixMilli()
	rd.ModifiedTS = &now
	if err := w.Sync.Cache.Save(); err != nil {
		logger.Warnf("cache save failed: %v", err)
	}
	logger.Infof("Edited reminder: %q (%s)", rd.Title, reminderID)
	return result, nil
}

// buildCreateOp builds a CloudKit create operation for a new reminder.
func buildCreateOp(title, listID, parentRef, dueDate string, priority int, notes string) (map[string]interface{}, string, error) {
	encoded, err := utils.EncodeTitle(title)
	if err != nil {
		return nil, "", fmt.Errorf("encode title: %w", err)
	}

	recordName := "Reminder/" + utils.NewUUIDString()
	now := time.Now().UnixMilli()
	fields := map[string]interface{}{
		"TitleDocument":    map[string]interface{}{"value": encoded},
		"Completed":        map[string]interface{}{"value": 0},
		"CreationDate":     map[string]interface{}{"value": now, "type": "TIMESTAMP"},
		"LastModifiedDate": map[string]interface{}{"value": now, "type": "TIMESTAMP"},
	}

	if listID != "" {
		fields["List"] = map[string]interface{}{
			"value": map[string]interface{}{
				"recordName": listID,
				"action":     "VALIDATE",
			},
		}
	}

	if parentRef != "" {
		fields["ParentReminder"] = map[string]interface{}{
			"value": map[string]interface{}{
				"recordName": parentRef,
				"action":     "VALIDATE",
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
	keys := []string{"titleDocument", "completed", "creationDate", "lastModifiedDate"}
	for field, key := range map[string]string{"List": "list", "ParentReminder": "parentReminder", "DueDate": "dueDate", "Priority": "priority", "NotesDocument": "notesDocument"} {
		if fields[field] != nil {
			keys = append(keys, key)
		}
	}
	tokens, err := cloudkit.BumpResolutionTokens("", keys)
	if err != nil {
		return nil, "", err
	}
	fields["ResolutionTokenMap"] = map[string]interface{}{"value": tokens}

	op := map[string]interface{}{
		"operationType": "create",
		"record": map[string]interface{}{
			"recordType": "Reminder",
			"recordName": recordName,
			"fields":     fields,
		},
	}
	if listID != "" {
		// CloudKit children inherit the list's share through the record parent.
		op["record"].(map[string]interface{})["parent"] = map[string]interface{}{"recordName": listID}
	}
	return op, recordName, nil
}

func errResult(err error) map[string]interface{} {
	return map[string]interface{}{"error": err.Error()}
}

// checkRecordErrors extracts the first record-level error from CloudKit result.
// CloudKit returns errors like {"records": [{"serverErrorCode": "BAD_REQUEST", "reason": "..."}]}
func checkRecordErrors(result map[string]interface{}) error {
	if message, ok := result["error"].(string); ok && message != "" {
		return errors.New(message)
	}
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
