package writer

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"icloud-reminders/internal/cache"
)

func (w *Writer) modifyInScope(recordID, ownerID string, operations []map[string]interface{}) (map[string]interface{}, error) {
	client := w.CK
	if scope, ok := w.Sync.Cache.Scopes[recordID]; ok {
		client = client.ForScope(scope)
	}
	return client.ModifyRecords(ownerID, operations)
}

// Assign updates an existing assignment or creates the native child record and
// links it atomically. Clear detaches and soft-deletes linked assignments.
func (w *Writer) Assign(reminderID, participantID, originatorID string, clear bool) error {
	reminder := w.Sync.Cache.Reminders[reminderID]
	scope, ok := w.Sync.Cache.Scopes[reminderID]
	if reminder == nil || !ok || reminder.ChangeTag == nil || *reminder.ChangeTag == "" {
		return fmt.Errorf("assignment requires a current reminder")
	}
	field := func(value any) any { return map[string]any{"value": value} }
	update := func(id, kind, tag string, fields map[string]any) map[string]any {
		return map[string]any{"operationType": "update", "record": map[string]any{
			"recordName": id, "recordType": kind, "recordChangeTag": tag, "fields": fields,
		}}
	}
	for _, id := range reminder.AssignmentIDs {
		assignment := w.Sync.Cache.Assignments[id]
		if assignment == nil || assignment.ReminderID != reminderID || assignment.ChangeTag == "" || w.Sync.Cache.Scopes[id].Key() != scope.Key() {
			return fmt.Errorf("assignment state is incomplete")
		}
	}
	if !clear && len(reminder.AssignmentIDs) > 1 {
		return fmt.Errorf("multiple assignments require explicit clearing first")
	}
	var operations []map[string]any
	newIDs := []string{}
	assignedAt := time.Now().UnixMilli()
	if clear {
		for _, id := range reminder.AssignmentIDs {
			operations = append(operations, update(id, "Assignment", w.Sync.Cache.Assignments[id].ChangeTag, map[string]any{"Deleted": field(1)}))
		}
	} else {
		encrypted := func(value string) any { return map[string]any{"value": value, "type": "STRING", "isEncrypted": true} }
		fields := map[string]any{
			"EncryptedAssigneeIdentifier": encrypted(participantID), "EncryptedOriginatorIdentifier": encrypted(originatorID),
			"AssignedDate": map[string]any{"value": assignedAt, "type": "TIMESTAMP"}, "Status": field(1),
		}
		if len(reminder.AssignmentIDs) == 1 {
			id := reminder.AssignmentIDs[0]
			newIDs = append(newIDs, id)
			operations = append(operations, update(id, "Assignment", w.Sync.Cache.Assignments[id].ChangeTag, fields))
		} else {
			id := "Assignment/" + strings.ToUpper(uuid.NewString())
			newIDs = append(newIDs, id)
			fields["Deleted"], fields["Imported"] = field(0), field(0)
			fields["OwningReminderIdentifier"] = field(strings.TrimPrefix(reminderID, "Reminder/"))
			fields["Reminder"] = map[string]any{"value": map[string]any{
				"recordName": reminderID, "action": "VALIDATE", "zoneID": map[string]any{"zoneName": scope.ZoneName, "ownerRecordName": scope.OwnerRecordName},
			}}
			operations = append(operations, map[string]any{"operationType": "create", "record": map[string]any{
				"recordName": id, "recordType": "Assignment", "parent": map[string]any{"recordName": reminderID}, "fields": fields,
			}})
		}
	}
	ids := make([]string, 0, len(newIDs))
	for _, id := range newIDs {
		ids = append(ids, strings.TrimPrefix(id, "Assignment/"))
	}
	operations = append(operations, update(reminderID, "Reminder", *reminder.ChangeTag, map[string]any{"AssignmentIDs": field(ids)}))
	result, err := w.modifyInScope(reminderID, scope.OwnerRecordName, operations)
	if err != nil {
		return err
	}
	if err := checkRecordErrors(result); err != nil {
		return err
	}
	tags := make(map[string]string)
	for _, value := range result["records"].([]any) {
		record := value.(map[string]any)
		id, _ := record["recordName"].(string)
		tag, _ := record["recordChangeTag"].(string)
		if tag == "" {
			return fmt.Errorf("assignment write was not fully confirmed")
		}
		tags[id] = tag
	}
	// Publish local state only after every record in the atomic batch succeeded.
	for _, id := range reminder.AssignmentIDs {
		if clear {
			delete(w.Sync.Cache.Assignments, id)
			delete(w.Sync.Cache.Scopes, id)
		}
	}
	for _, id := range newIDs {
		w.Sync.Cache.Assignments[id] = &cache.AssignmentData{ReminderID: reminderID, AssigneeID: participantID, ChangeTag: tags[id], Status: 1}
		w.Sync.Cache.Scopes[id] = scope
	}
	reminder.AssignmentIDs = newIDs
	tag := tags[reminderID]
	reminder.ChangeTag = &tag
	return w.Sync.Cache.Save()
}
