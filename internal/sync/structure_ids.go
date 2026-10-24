package sync

import (
	"icloud-reminders/internal/cache"
	"icloud-reminders/internal/cloudkit"
	"icloud-reminders/pkg/models"
)

// Ordering and membership documents contain logical IDs, usually without a
// prefix. CloudKit record names are opaque: both UUID and Reminder/UUID exist.
// Resolve metadata against the records in this list/zone without renaming them.
func (e *Engine) ResolveListStructureIDs(listID string, st *cache.ListStructure) error {
	scope := e.Cache.Scopes[listID]
	reminders, sections := map[string]string{}, map[string]string{}
	ambiguous := func(field string) error {
		return &cloudkit.StructureError{Issue: models.StructureIssue{
			ErrorCode: "unsupported_structure", ListID: listID, RecordType: st.RecordType,
			Field: field, Reason: "invalid_entries",
		}}
	}
	for id, record := range e.Cache.Reminders {
		if record.ListRef == nil || *record.ListRef != listID || e.Cache.Scopes[id].Key() != scope.Key() {
			continue
		}
		key := cloudkit.NativeID("Reminder", id)
		if previous := reminders[key]; previous != "" && previous != id {
			return ambiguous("ReminderIDs")
		}
		reminders[key] = id
	}
	for id, section := range e.Cache.Sections {
		if section.ListID != listID || e.Cache.Scopes[id].Key() != scope.Key() {
			continue
		}
		key := cloudkit.NativeID("ListSection", id)
		if previous := sections[key]; previous != "" && previous != id {
			return ambiguous("SectionIDsOrderingAsData")
		}
		sections[key] = id
	}
	resolve := func(index map[string]string, prefix, id string) string {
		if recordName := index[cloudkit.NativeID(prefix, id)]; recordName != "" {
			return recordName
		}
		return id // Preserve historical/unknown entries rather than discarding them.
	}
	for i, id := range st.ReminderIDs {
		st.ReminderIDs[i] = resolve(reminders, "Reminder", id)
	}
	for i, id := range st.SectionIDs {
		st.SectionIDs[i] = resolve(sections, "ListSection", id)
	}
	memberships := map[string]string{}
	for reminder, section := range st.Memberships {
		memberships[resolve(reminders, "Reminder", reminder)] = resolve(sections, "ListSection", section)
	}
	st.Memberships = memberships
	return nil
}
