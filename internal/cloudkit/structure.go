package cloudkit

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"icloud-reminders/internal/cache"
	"icloud-reminders/internal/utils"
	"icloud-reminders/pkg/models"
)

const StructureVersion = 20230430

type StructureError struct{ Issue models.StructureIssue }

func (*StructureError) Error() string { return "unsupported list structure" }

func FieldValue(fields map[string]interface{}, name string) interface{} {
	field, _ := fields[name].(map[string]interface{})
	return field["value"]
}

func NativeID(prefix, id string) string {
	if id == "" || strings.HasPrefix(id, prefix+"/") {
		return id
	}
	return prefix + "/" + id
}

func BareID(id string) string {
	if index := strings.IndexByte(id, '/'); index >= 0 {
		return id[index+1:]
	}
	return id
}

// Apple's resolution tokens use seconds since 2001, a replica UUID and a
// monotonic per-property counter. Preserve extension fields and replica IDs.
func BumpResolutionTokens(text string, keys []string) (string, error) {
	tokens := map[string]interface{}{"map": map[string]interface{}{}}
	if text != "" && json.Unmarshal([]byte(text), &tokens) != nil {
		return "", &StructureError{}
	}
	entries, ok := tokens["map"].(map[string]interface{})
	if !ok {
		return "", &StructureError{}
	}
	seen := map[string]bool{}
	for _, key := range keys {
		if seen[key] {
			continue
		}
		seen[key] = true
		entry, ok := entries[key].(map[string]interface{})
		if !ok {
			entry = map[string]interface{}{"replicaID": utils.NewUUIDString()}
		}
		counter, _ := entry["counter"].(float64)
		if counter < 0 || counter > 1<<52 {
			return "", &StructureError{}
		}
		if replica, _ := entry["replicaID"].(string); replica == "" {
			entry["replicaID"] = utils.NewUUIDString()
		}
		entry["counter"] = counter + 1
		entry["modificationTime"] = float64(time.Now().UnixMilli()-978307200000) / 1000
		entries[key] = entry
	}
	encoded, err := json.Marshal(tokens)
	return string(encoded), err
}

func DecodeDocument(data []byte, key string) (map[string]json.RawMessage, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var document map[string]json.RawMessage
	var version int
	if len(data) > MaxStructureBytes {
		return nil, structureFailure("too_large", nil)
	}
	if json.Unmarshal(data, &document) != nil || document == nil {
		return nil, structureFailure("invalid_json", nil)
	}
	if json.Unmarshal(document["minimumSupportedVersion"], &version) != nil {
		return nil, structureFailure("invalid_version", nil)
	}
	if version > StructureVersion || version < 1 {
		return nil, structureFailure("unsupported_version", &version)
	}
	if document[key] == nil {
		return nil, structureFailure("missing_entries", &version)
	}
	return document, nil
}

func structureFailure(reason string, version *int) *StructureError {
	return &StructureError{Issue: models.StructureIssue{ErrorCode: "unsupported_structure", Reason: reason, Version: version}}
}

// ReadListStructure loads Apple's bounded JSON asset formats. It keeps original
// documents so mutations can preserve unknown properties and obsolete entries.
func (c *Client) ReadListStructure(record map[string]interface{}) (_ *cache.ListStructure, failure error) {
	fieldName := "ReminderIDs"
	defer func() {
		if failure == nil || errors.Is(failure, context.Canceled) || errors.Is(failure, context.DeadlineExceeded) {
			return
		}
		var issue *StructureError
		if !errors.As(failure, &issue) {
			issue = structureFailure("metadata_unavailable", nil)
		}
		if issue.Issue.Reason == "" {
			issue.Issue.Reason = "invalid_entries"
		}
		issue.Issue.ErrorCode = "unsupported_structure"
		issue.Issue.ListID, _ = record["recordName"].(string)
		issue.Issue.RecordType, _ = record["recordType"].(string)
		issue.Issue.Field = fieldName
		failure = issue
	}()
	fields, _ := record["fields"].(map[string]interface{})
	out := &cache.ListStructure{Memberships: map[string]string{}}
	out.ChangeTag, _ = record["recordChangeTag"].(string)
	out.RecordType, _ = record["recordType"].(string)
	var reminderData []byte
	if text, ok := FieldValue(fields, "ReminderIDs").(string); ok {
		reminderData = []byte(text)
	} else if asset, ok := FieldValue(fields, "ReminderIDsAsset").(map[string]interface{}); ok {
		fieldName = "ReminderIDsAsset"
		var err error
		reminderData, err = c.ReadStructureAsset(asset)
		if err != nil {
			return nil, err
		}
	} else if FieldValue(fields, "ReminderIDs") != nil || FieldValue(fields, "ReminderIDsAsset") != nil {
		return nil, structureFailure("invalid_entries", nil)
	}
	if len(reminderData) > MaxStructureBytes || (len(reminderData) > 0 && (json.Unmarshal(reminderData, &out.ReminderIDs) != nil || out.ReminderIDs == nil)) {
		return nil, &StructureError{}
	}
	seenReminders := map[string]bool{}
	orderedReminders := []string{}
	for _, id := range out.ReminderIDs {
		canonical := NativeID("Reminder", id)
		if id == "" {
			return nil, &StructureError{}
		}
		// Replicated/legacy order documents can repeat the same record. They
		// describe one reminder; retain its first position without changing iCloud.
		if !seenReminders[canonical] {
			orderedReminders = append(orderedReminders, canonical)
		}
		seenReminders[canonical] = true
	}
	out.ReminderIDs = orderedReminders
	for _, name := range []string{"MembershipsOfRemindersInSectionsAsData", "SectionIDsOrderingAsData"} {
		fieldName = name
		asset, ok := FieldValue(fields, name).(map[string]interface{})
		if !ok {
			if FieldValue(fields, name) != nil {
				return nil, &StructureError{}
			}
			continue
		}
		data, err := c.ReadStructureAsset(asset)
		if err != nil {
			return nil, err
		}
		key := "memberships"
		if name == "SectionIDsOrderingAsData" {
			key = "orderedIdentifiers"
		}
		document, err := DecodeDocument(data, key)
		if err != nil {
			return nil, err
		}
		if key == "memberships" {
			var entries []struct {
				MemberID   string  `json:"memberID"`
				GroupID    string  `json:"groupID"`
				Obsolete   bool    `json:"isObsolete"`
				ModifiedOn float64 `json:"modifiedOn"`
			}
			if json.Unmarshal(document[key], &entries) != nil {
				return nil, &StructureError{}
			}
			seen := map[string]bool{}
			for _, entry := range entries {
				if entry.MemberID == "" || seen[entry.MemberID] {
					return nil, &StructureError{}
				}
				seen[entry.MemberID] = true
				if !entry.Obsolete && entry.GroupID != "" {
					out.Memberships[NativeID("Reminder", entry.MemberID)] = NativeID("ListSection", entry.GroupID)
				}
			}
			out.MembershipsData = data
		} else {
			if json.Unmarshal(document[key], &out.SectionIDs) != nil {
				return nil, &StructureError{}
			}
			seenSections := map[string]bool{}
			for i, id := range out.SectionIDs {
				out.SectionIDs[i] = NativeID("ListSection", id)
				if id == "" || seenSections[out.SectionIDs[i]] {
					return nil, &StructureError{}
				}
				seenSections[out.SectionIDs[i]] = true
			}
			out.SectionOrderData = data
		}
	}
	return out, nil
}
