package reminders

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"icloud-reminders/internal/cache"
	"icloud-reminders/internal/cloudkit"
	syncengine "icloud-reminders/internal/sync"
	"icloud-reminders/internal/writer"
	"icloud-reminders/pkg/models"
)

type SectionsInput struct {
	ListID string `json:"list_id" jsonschema:"Exact list ID"`
}
type CreateSectionInput struct {
	ListID string `json:"list_id" jsonschema:"Exact list ID"`
	Title  string `json:"title" jsonschema:"Native section heading; not a reminder title"`
}
type SectionsResult struct {
	StructureWarnings []*models.StructureIssue  `json:"structure_warnings,omitempty"`
	ListID            string                    `json:"list_id"`
	Sections          []*models.ReminderSection `json:"sections"`
}
type MoveInput struct {
	ID           string `json:"id" jsonschema:"Exact reminder ID"`
	ParentID     string `json:"parent_id,omitempty" jsonschema:"Indent under this same-list reminder; omit to keep current parent"`
	ClearParent  bool   `json:"clear_parent,omitempty" jsonschema:"Make top-level; mutually exclusive with parent_id"`
	SectionID    string `json:"section_id,omitempty" jsonschema:"Move into this native section; omit to keep current section; children inherit their parent's section"`
	ClearSection bool   `json:"clear_section,omitempty" jsonschema:"Move out of a section; mutually exclusive with section_id"`
	BeforeID     string `json:"before_id,omitempty" jsonschema:"Place before this sibling; mutually exclusive with after_id"`
	AfterID      string `json:"after_id,omitempty" jsonschema:"Place after this sibling; without an anchor append to the target sibling group"`
}
type ReorderInput struct {
	ListID      string   `json:"list_id" jsonschema:"Exact list ID"`
	ReminderIDs []string `json:"reminder_ids" jsonschema:"All sibling IDs exactly once, including completed reminders, in desired order"`
	ParentID    string   `json:"parent_id,omitempty" jsonschema:"Exact parent ID; omit for top-level reminders"`
	SectionID   string   `json:"section_id,omitempty" jsonschema:"Exact section ID; omit for unsectioned top-level reminders"`
}

func StructureLegend() map[string]string {
	return map[string]string{
		"•": "Pending reminder", "✓": "Completed reminder", "↳": "Subtask; parent_ref is authoritative",
		"!": "Low priority (9)", "!!": "Medium priority (5)", "!!!": "High priority (1)", "0": "No priority",
		"≡":          "Drag handle for manual order; not priority. Use move_reminder/reorder_reminders with IDs, never symbols in titles.",
		"sort_index": "Zero-based position in the native list's manual order; -1 when unavailable; may have gaps after filtering",
		"depth":      "Number of ancestors; tree nests only reminders returned on this page",
	}
}

func pointerValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
func exactSection(engine *syncengine.Engine, id, listID string) error {
	section := engine.Cache.Sections[id]
	if section == nil {
		return &Error{"not_found", "Section ID not found. Use list_reminder_sections for an exact ID."}
	}
	if listID != "" && section.ListID != listID {
		return invalid("section_id must belong to list_id")
	}
	if engine.Cache.Scopes[id].Key() != engine.Cache.Scopes[section.ListID].Key() {
		return fmt.Errorf("section belongs to another zone")
	}
	return nil
}

func (s *Service) Sections(ctx context.Context, in SectionsInput) (out SectionsResult, err error) {
	if strings.TrimSpace(in.ListID) == "" {
		return out, invalid("list_id is required")
	}
	out = SectionsResult{ListID: in.ListID, Sections: []*models.ReminderSection{}}
	err = s.run(ctx, false, func(e *syncengine.Engine, _ *writer.Writer) error {
		if err := exactList(e, in.ListID); err != nil {
			return err
		}
		ids := []string{}
		out.StructureWarnings = e.StructureWarnings(in.ListID)
		if st := e.Cache.Structures[in.ListID]; st != nil {
			ids = append(ids, st.SectionIDs...)
		}
		seen := map[string]bool{}
		remaining := []string{}
		for _, id := range ids {
			seen[id] = true
		}
		for id, section := range e.Cache.Sections {
			if section.ListID == in.ListID && !seen[id] {
				remaining = append(remaining, id)
			}
		}
		sort.Strings(remaining)
		ids = append(ids, remaining...)
		seen = map[string]bool{}
		for _, id := range ids {
			if section := e.Cache.Sections[id]; section != nil && section.ListID == in.ListID && !seen[id] {
				if err := exactSection(e, id, in.ListID); err != nil {
					return err
				}
				seen[id] = true
				out.Sections = append(out.Sections, &models.ReminderSection{ID: id, Title: section.Title, ListID: in.ListID, SortIndex: len(out.Sections)})
			}
		}
		return nil
	})
	return
}

func writableStructure(e *syncengine.Engine, listID string) (map[string]interface{}, *cache.ListStructure, error) {
	people, _, canWrite, err := participants(e, listID)
	if err != nil {
		return nil, nil, err
	}
	if people.Shared && !canWrite {
		return nil, nil, &Error{"permission_denied", "The current participant cannot modify this shared list."}
	}
	scope, ok := e.Cache.Scopes[listID]
	if !ok {
		return nil, nil, fmt.Errorf("list scope unavailable")
	}
	values, err := e.CK.ForScope(scope).LookupRecords(scope.OwnerRecordName, []string{listID})
	if err != nil {
		return nil, nil, err
	}
	root := values[0]
	st, err := e.CK.ForScope(scope).ReadListStructure(root)
	if err != nil {
		return nil, nil, err
	}
	if err := e.ResolveListStructureIDs(listID, st); err != nil {
		return nil, nil, err
	}
	// Apple constructs Reminder/<logical ID> when resolving the order document.
	// Bare CloudKit record names remain readable for recovery, but a reorder of
	// them cannot describe records that Apple's native clients can load.
	for id, record := range e.Cache.Reminders {
		if record.ListRef != nil && *record.ListRef == listID && e.Cache.Scopes[id].Key() == scope.Key() && !strings.HasPrefix(id, "Reminder/") {
			return nil, nil, &cloudkit.StructureError{Issue: models.StructureIssue{
				ErrorCode: "unsupported_structure", ListID: listID, RecordType: st.RecordType,
				Field: "ReminderIDs", Reason: "non_native_record_id",
			}}
		}
	}
	e.Cache.Structures[listID] = st
	delete(e.Cache.StructureIssues, listID)
	return root, st, nil
}

func encodeSectionOrder(st *cache.ListStructure) ([]byte, error) {
	document, err := cloudkit.DecodeDocument(st.SectionOrderData, "orderedIdentifiers")
	if err != nil {
		return nil, err
	}
	if document == nil {
		document = map[string]json.RawMessage{"minimumSupportedVersion": json.RawMessage("20230430")}
	}
	ids := []string{}
	for _, id := range st.SectionIDs {
		ids = append(ids, cloudkit.BareID(id))
	}
	document["orderedIdentifiers"], err = json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	return json.Marshal(document)
}

func setMemberships(st *cache.ListStructure, ids []string, section string) ([]byte, error) {
	document, err := cloudkit.DecodeDocument(st.MembershipsData, "memberships")
	if err != nil {
		return nil, err
	}
	if document == nil {
		document = map[string]json.RawMessage{"minimumSupportedVersion": json.RawMessage("20230430"), "memberships": json.RawMessage("[]")}
	}
	var entries []map[string]json.RawMessage
	if json.Unmarshal(document["memberships"], &entries) != nil {
		return nil, fmt.Errorf("invalid memberships")
	}
	wanted := map[string]bool{}
	for _, id := range ids {
		wanted[cloudkit.BareID(id)] = true
	}
	modified, _ := json.Marshal(float64(time.Now().UnixNano())/1e9 - 978307200)
	for _, entry := range entries {
		var member string
		if json.Unmarshal(entry["memberID"], &member) != nil {
			return nil, fmt.Errorf("invalid membership")
		}
		if !wanted[member] {
			continue
		}
		delete(wanted, member)
		entry["modifiedOn"] = modified
		if section == "" {
			entry["isObsolete"] = json.RawMessage("true")
		} else {
			entry["groupID"], _ = json.Marshal(cloudkit.BareID(section))
			delete(entry, "isObsolete")
		}
	}
	if section != "" {
		remaining := []string{}
		for id := range wanted {
			remaining = append(remaining, id)
		}
		sort.Strings(remaining)
		for _, id := range remaining {
			member, _ := json.Marshal(id)
			group, _ := json.Marshal(cloudkit.BareID(section))
			entries = append(entries, map[string]json.RawMessage{"memberID": member, "groupID": group, "modifiedOn": modified})
		}
	}
	document["memberships"], err = json.Marshal(entries)
	if err != nil {
		return nil, err
	}
	return json.Marshal(document)
}

func (s *Service) CreateSection(ctx context.Context, in CreateSectionInput) (out MutationResult, err error) {
	if strings.TrimSpace(in.ListID) == "" || strings.TrimSpace(in.Title) == "" {
		return out, invalid("list_id and title are required")
	}
	if err := validateFields(in.Title, "", ""); err != nil {
		return out, err
	}
	err = s.run(ctx, false, func(e *syncengine.Engine, w *writer.Writer) error {
		if err := exactList(e, in.ListID); err != nil {
			return err
		}
		root, st, err := writableStructure(e, in.ListID)
		if err != nil {
			return err
		}
		op, id := writer.CreateSectionOperation(in.Title, in.ListID)
		st.SectionIDs = append(st.SectionIDs, id)
		data, err := encodeSectionOrder(st)
		if err != nil {
			return err
		}
		if err := w.ApplyStructure(in.ListID, root, map[string]interface{}{}, map[string][]byte{"SectionIDsOrderingAsData": data}, []map[string]interface{}{op}); err != nil {
			return err
		}
		out = MutationResult{ID: id, Status: "created"}
		return nil
	})
	return
}

// Traverse the entire hierarchy before filtering/pagination so selected children
// keep their relative position even when their parent is absent from the result.
func OrderReminders(e *syncengine.Engine, selected []*models.Reminder) []*models.Reminder {
	all := e.GetReminders(true)
	byID := map[string]*models.Reminder{}
	children := map[string][]*models.Reminder{}
	roots := []*models.Reminder{}
	positions := map[string]int{}
	sectionPositions := map[string]int{}
	for _, st := range e.Cache.Structures {
		for i, id := range st.ReminderIDs {
			positions[id] = i
		}
		for i, id := range st.SectionIDs {
			sectionPositions[id] = i + 1
		}
	}
	for _, r := range all {
		byID[r.ID] = r
	}
	for _, r := range all {
		parent := byID[pointerValue(r.ParentRef)]
		if parent != nil && pointerValue(parent.ListRef) == pointerValue(r.ListRef) {
			children[parent.ID] = append(children[parent.ID], r)
		} else {
			roots = append(roots, r)
		}
	}
	less := func(a, b *models.Reminder) bool {
		if pointerValue(a.ListRef) != pointerValue(b.ListRef) {
			return pointerValue(a.ListRef) < pointerValue(b.ListRef)
		}
		as, bs := pointerValue(a.SectionRef), pointerValue(b.SectionRef)
		if as != bs {
			ai, bi := sectionPositions[as], sectionPositions[bs]
			if as != "" && ai == 0 {
				ai = 100000
			}
			if bs != "" && bi == 0 {
				bi = 100000
			}
			if ai != bi {
				return ai < bi
			}
			return as < bs
		}
		ai, aok := positions[a.ID]
		bi, bok := positions[b.ID]
		if aok != bok {
			return aok
		}
		if ai != bi {
			return ai < bi
		}
		return a.ID < b.ID
	}
	sort.Slice(roots, func(i, j int) bool { return less(roots[i], roots[j]) })
	for _, items := range children {
		sort.Slice(items, func(i, j int) bool { return less(items[i], items[j]) })
	}
	wanted := map[string]*models.Reminder{}
	for _, r := range selected {
		wanted[r.ID] = r
	}
	out := []*models.Reminder{}
	seen := map[string]bool{}
	var visit func(*models.Reminder)
	visit = func(r *models.Reminder) {
		if seen[r.ID] {
			return
		}
		seen[r.ID] = true
		if selected := wanted[r.ID]; selected != nil {
			out = append(out, selected)
		}
		for _, child := range children[r.ID] {
			visit(child)
		}
	}
	for _, r := range roots {
		visit(r)
	}
	sort.Slice(all, func(i, j int) bool { return less(all[i], all[j]) })
	for _, r := range all {
		visit(r)
	}
	return out
}

func reminderTree(items []*models.Reminder) []*models.ReminderNode {
	nodes := map[string]*models.ReminderNode{}
	for _, r := range items {
		nodes[r.ID] = &models.ReminderNode{Reminder: r, Subtasks: []*models.ReminderNode{}}
	}
	roots := []*models.ReminderNode{}
	for _, r := range items {
		parent := nodes[pointerValue(r.ParentRef)]
		seen := map[string]bool{r.ID: true}
		cursor := parent
		cycle := false
		for cursor != nil {
			if seen[cursor.Reminder.ID] {
				cycle = true
				break
			}
			seen[cursor.Reminder.ID] = true
			cursor = nodes[pointerValue(cursor.Reminder.ParentRef)]
		}
		if parent == nil || cycle {
			roots = append(roots, nodes[r.ID])
		} else {
			parent.Subtasks = append(parent.Subtasks, nodes[r.ID])
		}
	}
	return roots
}

func subtree(e *syncengine.Engine, id string) []string {
	out := []string{}
	seen := map[string]bool{}
	var visit func(string)
	visit = func(id string) {
		if seen[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
		children := []string{}
		for child, r := range e.Cache.Reminders {
			if pointerValue(r.ParentRef) == id {
				children = append(children, child)
			}
		}
		sort.Strings(children)
		for _, child := range children {
			visit(child)
		}
	}
	visit(id)
	return out
}

func reminderByID(e *syncengine.Engine, id string) *models.Reminder {
	for _, r := range e.GetReminders(true) {
		if r.ID == id {
			return r
		}
	}
	return nil
}
func listOrder(e *syncengine.Engine, listID string, st *cache.ListStructure) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, id := range st.ReminderIDs {
		if !seen[id] {
			out = append(out, id)
			seen[id] = true
		}
	}
	missing := []string{}
	for id, r := range e.Cache.Reminders {
		if pointerValue(r.ListRef) == listID && !seen[id] {
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	return append(out, missing...)
}
func orderFields(ids []string) map[string]interface{} {
	bare := []string{}
	for _, id := range ids {
		bare = append(bare, cloudkit.BareID(id))
	}
	encoded, _ := json.Marshal(bare)
	return map[string]interface{}{"ReminderIDs": map[string]interface{}{"value": string(encoded)}}
}

func createOrganized(e *syncengine.Engine, w *writer.Writer, in CreateInput) (MutationResult, error) {
	root, st, err := writableStructure(e, in.ListID)
	if err != nil {
		return MutationResult{}, err
	}
	section := in.SectionID
	if in.ParentID != "" {
		parent := reminderByID(e, in.ParentID)
		if section != "" && section != pointerValue(parent.SectionRef) {
			return MutationResult{}, invalid("subtasks inherit their parent's section")
		}
		section = pointerValue(parent.SectionRef)
	}
	if section != "" {
		if err := exactSection(e, section, in.ListID); err != nil {
			return MutationResult{}, err
		}
	}
	op, id, err := writer.CreateReminderOperation(in.Title, in.ListID, in.ParentID, in.Due, models.PriorityMap[in.Priority], in.Notes)
	if err != nil {
		return MutationResult{}, err
	}
	if w.CreateRecordID != "" {
		id = w.CreateRecordID
		op["record"].(map[string]interface{})["recordName"] = id
	}
	order := listOrder(e, in.ListID, st)
	order = append(order, id)
	docs := map[string][]byte{}
	if section != "" {
		data, err := setMemberships(st, []string{id}, section)
		if err != nil {
			return MutationResult{}, err
		}
		docs["MembershipsOfRemindersInSectionsAsData"] = data
	}
	if err := w.ApplyStructure(in.ListID, root, orderFields(order), docs, []map[string]interface{}{op}); err != nil {
		return MutationResult{}, err
	}
	return MutationResult{ID: id, Status: "created"}, nil
}

func (s *Service) Move(ctx context.Context, in MoveInput) (out MutationResult, err error) {
	if strings.TrimSpace(in.ID) == "" || (in.ParentID != "" && in.ClearParent) || (in.SectionID != "" && in.ClearSection) || (in.BeforeID != "" && in.AfterID != "") {
		return out, invalid("id is required; parent/section clearing and before/after anchors are mutually exclusive")
	}
	err = s.run(ctx, false, func(e *syncengine.Engine, w *writer.Writer) error {
		in.ID = resolveReminderID(e, in.ID)
		in.ParentID = resolveReminderID(e, in.ParentID)
		in.BeforeID = resolveReminderID(e, in.BeforeID)
		in.AfterID = resolveReminderID(e, in.AfterID)
		if err := exactReminder(e, in.ID); err != nil {
			return err
		}
		r := reminderByID(e, in.ID)
		listID := pointerValue(r.ListRef)
		if err := exactList(e, listID); err != nil {
			return err
		}
		root, st, err := writableStructure(e, listID)
		if err != nil {
			return err
		}
		r = reminderByID(e, in.ID)
		parent := pointerValue(r.ParentRef)
		section := pointerValue(r.SectionRef)
		if in.ClearParent {
			parent = ""
		} else if in.ParentID != "" {
			parent = in.ParentID
		}
		moving := subtree(e, in.ID)
		movingSet := map[string]bool{}
		for _, id := range moving {
			child := e.Cache.Reminders[id]
			if child == nil || pointerValue(child.ListRef) != listID || e.Cache.Scopes[id].Key() != e.Cache.Scopes[listID].Key() {
				return invalid("every descendant must belong to the same list and zone")
			}
			movingSet[id] = true
		}
		if movingSet[parent] {
			return invalid("a reminder cannot be moved under itself or a descendant")
		}
		if in.ClearSection {
			section = ""
		} else if in.SectionID != "" {
			section = in.SectionID
		}
		if parent != "" {
			if err := exactReminder(e, parent); err != nil {
				return err
			}
			pr := reminderByID(e, parent)
			if pointerValue(pr.ListRef) != listID {
				return invalid("parent_id must be in the same list")
			}
			ps := pointerValue(pr.SectionRef)
			if (in.SectionID != "" || in.ClearSection) && section != ps {
				return invalid("subtasks inherit their parent's section")
			}
			section = ps
		}
		if section != "" {
			if err := exactSection(e, section, listID); err != nil {
				return err
			}
		}
		anchor := in.BeforeID
		if anchor == "" {
			anchor = in.AfterID
		}
		if anchor != "" {
			if movingSet[anchor] {
				return invalid("anchor cannot be the moved reminder or its descendant")
			}
			if err := exactReminder(e, anchor); err != nil {
				return err
			}
			ar := reminderByID(e, anchor)
			if pointerValue(ar.ListRef) != listID || pointerValue(ar.ParentRef) != parent || pointerValue(ar.SectionRef) != section {
				return invalid("anchor must be a sibling in the target parent and section")
			}
		}
		order := listOrder(e, listID, st)
		block := []string{}
		remaining := []string{}
		for _, id := range order {
			if movingSet[id] {
				block = append(block, id)
			} else {
				remaining = append(remaining, id)
			}
		}
		index := len(remaining)
		if anchor != "" {
			boundary := anchor
			if in.AfterID != "" {
				aSet := map[string]bool{}
				for _, id := range subtree(e, anchor) {
					aSet[id] = true
				}
				for _, id := range remaining {
					if aSet[id] {
						boundary = id
					}
				}
			}
			for i, id := range remaining {
				if id == boundary {
					index = i
					if in.AfterID != "" {
						index++
					}
					break
				}
			}
		} else {
			last := ""
			for _, item := range OrderReminders(e, e.GetReminders(true)) {
				if !movingSet[item.ID] && pointerValue(item.ListRef) == listID && pointerValue(item.ParentRef) == parent && pointerValue(item.SectionRef) == section {
					last = item.ID
				}
			}
			if last != "" {
				lastSet := map[string]bool{}
				for _, id := range subtree(e, last) {
					lastSet[id] = true
				}
				for i, id := range remaining {
					if lastSet[id] {
						index = i + 1
					}
				}
			} else if parent != "" {
				for i, id := range remaining {
					if id == parent {
						index = i + 1
						break
					}
				}
			}
		}
		newOrder := append([]string{}, remaining[:index]...)
		newOrder = append(newOrder, block...)
		newOrder = append(newOrder, remaining[index:]...)
		docs := map[string][]byte{}
		if section != pointerValue(r.SectionRef) {
			data, err := setMemberships(st, moving, section)
			if err != nil {
				return err
			}
			docs["MembershipsOfRemindersInSectionsAsData"] = data
		}
		ops := []map[string]interface{}{}
		if parent != pointerValue(r.ParentRef) {
			rd := e.Cache.Reminders[in.ID]
			if rd.ChangeTag == nil {
				return fmt.Errorf("missing reminder change tag")
			}
			var ref interface{}
			if parent != "" {
				ref = map[string]interface{}{"recordName": parent, "action": "VALIDATE"}
			}
			ops = append(ops, map[string]interface{}{"operationType": "update", "record": map[string]interface{}{"recordName": in.ID, "recordType": "Reminder", "recordChangeTag": *rd.ChangeTag, "fields": map[string]interface{}{"ParentReminder": map[string]interface{}{"value": ref}}}})
		}
		if err := w.ApplyStructure(listID, root, orderFields(newOrder), docs, ops); err != nil {
			return err
		}
		if err := verifyStoredOrder(e, listID, newOrder); err != nil {
			return err
		}
		out = MutationResult{ID: in.ID, Status: "moved", OrderVerification: "verified"}
		return nil
	})
	return
}

func (s *Service) Reorder(ctx context.Context, in ReorderInput) (out MutationResult, err error) {
	if strings.TrimSpace(in.ListID) == "" || in.ReminderIDs == nil || len(in.ReminderIDs) > 10000 {
		return out, invalid("list_id and a bounded reminder_ids array are required")
	}
	seen := map[string]bool{}
	for _, id := range in.ReminderIDs {
		if strings.TrimSpace(id) == "" || seen[id] {
			return out, invalid("reminder_ids must contain unique exact IDs")
		}
		seen[id] = true
	}
	err = s.run(ctx, false, func(e *syncengine.Engine, w *writer.Writer) error {
		in.ParentID = resolveReminderID(e, in.ParentID)
		in.ReminderIDs = append([]string(nil), in.ReminderIDs...)
		for i, id := range in.ReminderIDs {
			in.ReminderIDs[i] = resolveReminderID(e, id)
		}
		if err := exactList(e, in.ListID); err != nil {
			return err
		}
		root, st, err := writableStructure(e, in.ListID)
		if err != nil {
			return err
		}
		section := in.SectionID
		if in.ParentID != "" {
			if err := exactReminder(e, in.ParentID); err != nil {
				return err
			}
			p := reminderByID(e, in.ParentID)
			if pointerValue(p.ListRef) != in.ListID {
				return invalid("parent_id must belong to list_id")
			}
			if section != "" && section != pointerValue(p.SectionRef) {
				return invalid("subtasks inherit their parent's section")
			}
			section = pointerValue(p.SectionRef)
		}
		if section != "" {
			if err := exactSection(e, section, in.ListID); err != nil {
				return err
			}
		}
		siblings := map[string]bool{}
		for _, r := range e.GetReminders(true) {
			if pointerValue(r.ListRef) == in.ListID && pointerValue(r.ParentRef) == in.ParentID && pointerValue(r.SectionRef) == section {
				siblings[r.ID] = true
			}
		}
		if len(siblings) != len(in.ReminderIDs) {
			return invalid("provide every sibling exactly once, including completed reminders")
		}
		for _, id := range in.ReminderIDs {
			if !siblings[id] {
				return invalid("every ID must be a sibling in the target group")
			}
		}
		if len(in.ReminderIDs) == 0 {
			out = MutationResult{ID: in.ListID, Status: "unchanged"}
			return nil
		}
		order := listOrder(e, in.ListID, st)
		blocks := map[string][]string{}
		owner := map[string]string{}
		for _, id := range in.ReminderIDs {
			for _, child := range subtree(e, id) {
				owner[child] = id
			}
		}
		first := -1
		remaining := []string{}
		for _, id := range order {
			if root := owner[id]; root != "" {
				if first < 0 {
					first = len(remaining)
				}
				blocks[root] = append(blocks[root], id)
			} else {
				remaining = append(remaining, id)
			}
		}
		if first < 0 {
			return fmt.Errorf("sibling order unavailable")
		}
		newOrder := append([]string{}, remaining[:first]...)
		for _, id := range in.ReminderIDs {
			newOrder = append(newOrder, blocks[id]...)
		}
		newOrder = append(newOrder, remaining[first:]...)
		if err := w.ApplyStructure(in.ListID, root, orderFields(newOrder), nil, nil); err != nil {
			return err
		}
		if err := verifyStoredOrder(e, in.ListID, newOrder); err != nil {
			return err
		}
		out = MutationResult{ID: in.ListID, Status: "reordered", OrderVerification: "verified"}
		return nil
	})
	return
}
