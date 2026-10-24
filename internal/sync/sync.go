// Package sync implements the CloudKit delta synchronization engine.
package sync

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"icloud-reminders/internal/auth"
	"icloud-reminders/internal/cache"
	"icloud-reminders/internal/cloudkit"
	"icloud-reminders/internal/logger"
	"icloud-reminders/internal/utils"
	"icloud-reminders/pkg/models"
)

// Engine handles syncing reminders with CloudKit.
type Engine struct {
	// NonInteractive disables the CLI's forced password/2FA retry on HTTP 503.
	NonInteractive bool
	CK             *cloudkit.Client
	Cache          *cache.Cache
	sessionFile    string // used for 503 re-auth
}

// New creates a new sync engine.
func New(ck *cloudkit.Client, sessionFile string) *Engine {
	return NewWithCache(ck, sessionFile, cache.Load())
}

// NewWithCache uses an explicit account-scoped cache.
func NewWithCache(ck *cloudkit.Client, sessionFile string, cached *cache.Cache) *Engine {
	return &Engine{
		CK:          ck,
		Cache:       cached,
		sessionFile: sessionFile,
	}
}

// Sync performs a delta or full sync from CloudKit.
// On a 503 response it attempts a forced full re-auth once and retries.
// If the 503 persists after re-auth, the call aborts — this indicates an
// implementation bug rather than a transient server error.
func (e *Engine) Sync(force bool) error {
	err := e.doSync(force)
	if err == nil {
		return nil
	}
	if cloudkit.Is503(err) && !e.NonInteractive {
		logger.Warn("Got 503 from iCloud — attempting forced re-auth...")
		sess, reAuthErr := auth.New().EnsureSession(e.sessionFile, true)
		if reAuthErr != nil {
			return fmt.Errorf("re-auth failed after 503: %w", reAuthErr)
		}
		newCK, ckErr := cloudkit.NewFromSession(sess)
		if ckErr != nil {
			return fmt.Errorf("cloudkit reinit after re-auth: %w", ckErr)
		}
		e.CK = newCK
		if retryErr := e.doSync(force); retryErr != nil {
			return fmt.Errorf("503 persists after re-auth (implementation bug): %w", retryErr)
		}
		return nil
	}
	return err
}

// doSync is the inner sync implementation used by Sync.
func (e *Engine) doSync(force bool) error {
	defer logger.Timer("sync")()
	if e.Cache.SchemaVersion != 5 {
		force = true
	}
	if force {
		e.Cache = e.Cache.Reset()
		logger.Info("Full sync (forced)...")
	} else if e.Cache.SyncToken != nil && *e.Cache.SyncToken != "" {
		logger.Info("Delta sync...")
	} else {
		logger.Info("Full sync (no cache)...")
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
	shared, err := e.CK.SharedZones()
	if err != nil {
		return err
	}
	sort.Slice(shared, func(i, j int) bool { return shared[i].Key() < shared[j].Key() })
	scopes := append([]models.RecordScope{{Database: "private", ZoneName: cloudkit.Zone, OwnerRecordName: ownerID}}, shared...)
	active := make(map[string]bool)
	total := 0
	for _, scope := range scopes {
		active[scope.Key()] = true
		client := e.CK.ForScope(scope)
		for page := 0; ; page++ {
			if page >= 10000 {
				return fmt.Errorf("sync pagination limit exceeded")
			}
			data, err := client.ChangesZone(scope.OwnerRecordName, e.Cache.ZoneTokens[scope.Key()])
			if err != nil {
				return err
			}
			zones, ok := data["zones"].([]interface{})
			if !ok || len(zones) != 1 {
				return fmt.Errorf("invalid changes response")
			}
			zoneResp, ok := zones[0].(map[string]interface{})
			if !ok {
				return fmt.Errorf("invalid changes zone")
			}
			if code, _ := zoneResp["serverErrorCode"].(string); code != "" {
				return fmt.Errorf("changes zone unavailable")
			}
			if returned, ok := zoneResp["zoneID"].(map[string]interface{}); ok {
				if returned["zoneName"] != scope.ZoneName || returned["ownerRecordName"] != scope.OwnerRecordName {
					return fmt.Errorf("changes returned wrong zone")
				}
			}
			records, ok := zoneResp["records"].([]interface{})
			if !ok {
				return fmt.Errorf("invalid changes records")
			}
			moreComing, _ := zoneResp["moreComing"].(bool)
			newToken, _ := zoneResp["syncToken"].(string)
			total += len(records)
			if err := e.processRecords(records, scope); err != nil {
				return err
			}
			if moreComing && (newToken == "" || newToken == e.Cache.ZoneTokens[scope.Key()]) {
				return fmt.Errorf("changes token did not advance")
			}
			if newToken != "" {
				e.Cache.ZoneTokens[scope.Key()] = newToken
			}
			if scope.Database == "private" && newToken != "" {
				e.Cache.SyncToken = &newToken
			}
			if !moreComing {
				break
			}
		}
	}
	// Confirmed structure writes schedule scoped reads, never mutation retries.
	// Read after the delta so an older delta cannot overwrite a newer read-back.
	for id, scope := range e.Cache.PendingReadbacks {
		if !active[scope.Key()] {
			delete(e.Cache.PendingReadbacks, id)
			continue
		}
		records, err := e.CK.ForScope(scope).LookupRecords(scope.OwnerRecordName, []string{id})
		if err != nil {
			var missing *cloudkit.RecordNotFoundError
			if errors.As(err, &missing) {
				e.removeRecord(id)
				continue
			}
			return err
		}
		if err := e.processRecords([]interface{}{records[0]}, scope); err != nil {
			return err
		}
		delete(e.Cache.PendingReadbacks, id)
	}
	// Revoked shared zones must not remain readable or writable from old cache.
	for id, scope := range e.Cache.Scopes {
		if !active[scope.Key()] {
			e.removeRecord(id)
		}
	}
	for key := range e.Cache.ZoneTokens {
		if !active[key] {
			delete(e.Cache.ZoneTokens, key)
		}
	}
	for id, reminder := range e.Cache.Reminders {
		if reminder.ListRef != nil {
			if _, exists := e.Cache.Lists[*reminder.ListRef]; !exists {
				e.removeRecord(id)
				continue
			}
			if e.Cache.Scopes[id].Key() != e.Cache.Scopes[*reminder.ListRef].Key() {
				return fmt.Errorf("reminder list belongs to another zone")
			}
		}
	}
	// Resolve after every page and scope is processed: ordering documents may
	// arrive before their records, including records with legacy bare names.
	for listID, structure := range e.Cache.Structures {
		if err := e.ResolveListStructureIDs(listID, structure); err != nil {
			var issue *cloudkit.StructureError
			if !errors.As(err, &issue) {
				return err
			}
			delete(e.Cache.Structures, listID)
			e.Cache.StructureIssues[listID] = &issue.Issue
		}
	}
	e.Cache.SchemaVersion = 5

	if err := e.Cache.Save(); err != nil {
		return fmt.Errorf("save cache: %w", err)
	}

	activeCount := 0
	for _, r := range e.Cache.Reminders {
		if !r.Completed {
			activeCount++
		}
	}
	logger.Infof("Synced: %d reminders (%d active), %d lists — %d records fetched",
		len(e.Cache.Reminders), activeCount, len(e.Cache.Lists), total)
	return nil
}

// processRecords processes CloudKit records into the local cache.
func (e *Engine) processRecords(records []interface{}, scope models.RecordScope) error {
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
		if previous, ok := e.Cache.Scopes[rname]; ok && previous.Key() != scope.Key() {
			return fmt.Errorf("record name belongs to multiple zones")
		}
		if deleted {
			e.removeRecord(rname)
			continue
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
					structure, err := e.CK.ForScope(scope).ReadListStructure(r)
					if err != nil {
						var issue *cloudkit.StructureError
						if !errors.As(err, &issue) {
							return err
						}
						// Never reuse stale order or memberships after a parse failure.
						delete(e.Cache.Structures, rname)
						e.Cache.StructureIssues[rname] = &issue.Issue
					} else {
						delete(e.Cache.StructureIssues, rname)
						e.Cache.Structures[rname] = structure
					}
					e.Cache.Lists[rname] = title
					e.Cache.Scopes[rname] = scope
					delete(e.Cache.ListShares, rname)
					if share, ok := r["share"].(map[string]interface{}); ok {
						if id, ok := share["recordName"].(string); ok && id != "" {
							e.Cache.ListShares[rname] = id
						}
					}
				}
			}

		case "ListSection":
			changeTag, _ := r["recordChangeTag"].(string)
			e.Cache.Sections[rname] = &cache.SectionData{Title: getFieldString(fields, "DisplayName"), ListID: getFieldRefName(fields, "List"), ChangeTag: changeTag}
			e.Cache.Scopes[rname] = scope
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
					ResolutionTokenMap: getFieldString(fields, "ResolutionTokenMap"),
					Title:              title,
					Completed:          getFieldInt(fields, "Completed") != 0,
					CompletionDate:     completionStr,
					Due:                dueStr,
					Priority:           priority,
					ModifiedTS:         modTS,
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
				if values, ok := fields["AssignmentIDs"].(map[string]interface{}); ok {
					if ids, ok := values["value"].([]interface{}); ok {
						for _, value := range ids {
							if id, ok := value.(string); ok && id != "" {
								if !strings.HasPrefix(id, "Assignment/") {
									id = "Assignment/" + id
								}
								rd.AssignmentIDs = append(rd.AssignmentIDs, id)
							}
						}
					}
				}
				e.Cache.Reminders[rname] = rd
				e.Cache.Scopes[rname] = scope
			}
		case "Assignment":
			changeTag, _ := r["recordChangeTag"].(string)
			e.Cache.Assignments[rname] = &cache.AssignmentData{
				ReminderID: getFieldRefName(fields, "Reminder"), AssigneeID: getFieldString(fields, "EncryptedAssigneeIdentifier"),
				ChangeTag: changeTag, Status: getFieldInt(fields, "Status"),
			}
			e.Cache.Scopes[rname] = scope
		}
	}
	return nil
}

func (e *Engine) removeRecord(id string) {
	if _, list := e.Cache.Lists[id]; list {
		for sectionID, section := range e.Cache.Sections {
			if section.ListID == id {
				e.removeRecord(sectionID)
			}
		}
		for reminderID, reminder := range e.Cache.Reminders {
			if reminder.ListRef != nil && *reminder.ListRef == id {
				e.removeRecord(reminderID)
			}
		}
	}
	delete(e.Cache.Lists, id)
	delete(e.Cache.ListShares, id)
	delete(e.Cache.Reminders, id)
	delete(e.Cache.Assignments, id)
	delete(e.Cache.Sections, id)
	delete(e.Cache.Structures, id)
	delete(e.Cache.StructureIssues, id)
	delete(e.Cache.Scopes, id)
	delete(e.Cache.PendingReadbacks, id)
}

// ConfirmedStructureWrite invalidates only records confirmed by records/modify.
// Persist the read-back queue before returning so a restart still refreshes them.
func (e *Engine) ConfirmedStructureWrite(scope models.RecordScope, operations []map[string]interface{}) error {
	for _, operation := range operations {
		record, _ := operation["record"].(map[string]interface{})
		id, _ := record["recordName"].(string)
		if operation["operationType"] == "delete" {
			e.removeRecord(id)
		} else {
			e.Cache.PendingReadbacks[id] = scope
		}
	}
	return e.Cache.Save()
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
		for _, id := range data.AssignmentIDs {
			assignment := e.Cache.Assignments[id]
			if assignment != nil && assignment.Status == 1 && assignment.ReminderID == rid && assignment.AssigneeID != "" {
				if e.Cache.Scopes[id].Key() == e.Cache.Scopes[rid].Key() {
					r.AssigneeID = &assignment.AssigneeID
					break
				}
			}
		}
		if data.ListRef != nil {
			r.SortIndex = -1 // No position is claimed when structure is unavailable.
			ancestor := rid
			seen := map[string]bool{}
			for !seen[ancestor] {
				seen[ancestor] = true
				parent := e.Cache.Reminders[ancestor]
				if parent == nil || parent.ParentRef == nil {
					break
				}
				parentData := e.Cache.Reminders[*parent.ParentRef]
				if parentData == nil || parentData.ListRef == nil || *parentData.ListRef != *data.ListRef || e.Cache.Scopes[*parent.ParentRef].Key() != e.Cache.Scopes[rid].Key() {
					break
				}
				ancestor = *parent.ParentRef
				r.Depth++
			}
			if structure := e.Cache.Structures[*data.ListRef]; structure != nil {
				ancestor := rid
				seen := map[string]bool{}
				for !seen[ancestor] {
					seen[ancestor] = true
					if sectionID := structure.Memberships[ancestor]; sectionID != "" {
						if section := e.Cache.Sections[sectionID]; section != nil && section.ListID == *data.ListRef && e.Cache.Scopes[sectionID].Key() == e.Cache.Scopes[rid].Key() {
							r.SectionRef = &sectionID
							r.SectionName = section.Title
						}
					}
					parent := e.Cache.Reminders[ancestor]
					if parent == nil || parent.ParentRef == nil || *parent.ParentRef == "" {
						break
					}
					parentData := e.Cache.Reminders[*parent.ParentRef]
					if parentData == nil || parentData.ListRef == nil || *parentData.ListRef != *data.ListRef || e.Cache.Scopes[*parent.ParentRef].Key() != e.Cache.Scopes[rid].Key() {
						break
					}
					ancestor = *parent.ParentRef
				}
				r.SortIndex = -1
				for i, id := range structure.ReminderIDs {
					if id == rid {
						r.SortIndex = i
						break
					}
				}
			}
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

func (e *Engine) StructureWarnings(listID string) []*models.StructureIssue {
	result := []*models.StructureIssue{}
	for id, issue := range e.Cache.StructureIssues {
		if listID == "" || id == listID {
			result = append(result, issue)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ListID < result[j].ListID })
	return result
}

// FindListByName finds a list ID by name (case-insensitive).
func (e *Engine) FindListByName(name string) string {
	if _, ok := e.Cache.Lists[name]; ok {
		return name
	}
	nameLower := toLower(name)
	match := ""
	for id, n := range e.Cache.Lists {
		if toLower(n) == nameLower {
			if match != "" {
				return ""
			}
			match = id
		}
	}
	return match
}

// FindReminderByID finds a full reminder ID by partial prefix match.
func (e *Engine) FindReminderByID(partialID string) string {
	if _, ok := e.Cache.Reminders[partialID]; ok {
		return partialID
	}
	if partialID == "" {
		return ""
	}
	partial := toLower(partialID)
	match := ""
	for rid := range e.Cache.Reminders {
		uuidPart := rid
		for i := len(rid) - 1; i >= 0; i-- {
			if rid[i] == '/' {
				uuidPart = rid[i+1:]
				break
			}
		}
		if len(uuidPart) >= len(partial) && toLower(uuidPart[:len(partial)]) == partial {
			if match != "" {
				return ""
			}
			match = rid
		}
	}
	return match
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
