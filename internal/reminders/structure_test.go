package reminders

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"icloud-reminders/internal/cache"
)

func (f *cloudFixture) assetField(name string, value any) any {
	if f.assets == nil {
		f.assets = map[string][]byte{}
	}
	data, _ := json.Marshal(value)
	f.assets[name] = data
	return map[string]any{"type": "ASSETID", "value": map[string]any{"downloadURL": f.server.URL + "/asset-download/" + name, "size": len(data)}}
}

func structureFixture(t *testing.T) *cloudFixture {
	f := newCloud(t)
	field := func(v any) any { return map[string]any{"value": v} }
	ref := func(id string) any { return field(map[string]any{"recordName": id}) }
	root := f.records["List/1"]["fields"].(map[string]any)
	root["ReminderIDs"] = field(`["BB1","AA1","CC1","DD1"]`)
	root["ResolutionTokenMap"] = field(`{"map":{"name":{"counter":8,"modificationTime":123},"reminderIDsMergeableOrdering":{"counter":3,"modificationTime":123},"sectionIDsOrderingAsData":{"counter":4,"modificationTime":123}},"future":"preserved"}`)
	root["SectionIDsOrderingAsData"] = f.assetField("section-order", map[string]any{"minimumSupportedVersion": 20230430, "orderedIdentifiers": []string{"two", "one"}, "future": "preserved"})
	root["MembershipsOfRemindersInSectionsAsData"] = f.assetField("memberships", map[string]any{"minimumSupportedVersion": 20230430, "memberships": []any{
		map[string]any{"memberID": "AA1", "groupID": "one", "modifiedOn": 123.5, "future": "preserved"},
		map[string]any{"memberID": "DD1", "groupID": "two", "modifiedOn": 123.5},
		map[string]any{"memberID": "deleted-member", "groupID": "one", "modifiedOn": 123.5, "isObsolete": true},
	}, "future": "preserved"})
	for _, id := range []string{"one", "two"} {
		f.records["ListSection/"+id] = map[string]any{"recordName": "ListSection/" + id, "recordType": "ListSection", "recordChangeTag": "section-tag", "fields": map[string]any{"DisplayName": field("Heading " + id), "List": ref("List/1")}}
	}
	for _, id := range []string{"CC1", "DD1"} {
		f.records["Reminder/"+id] = map[string]any{"recordName": "Reminder/" + id, "recordType": "Reminder", "recordChangeTag": "tag-1", "fields": map[string]any{"List": ref("List/1"), "Completed": field(0)}}
	}
	f.records["Reminder/CC1"]["fields"].(map[string]any)["ParentReminder"] = ref("Reminder/BB1")
	return f
}

func TestNativeSectionsAndHierarchicalPagination(t *testing.T) {
	f := structureFixture(t)
	s := New(f.directory, time.Second)
	sections, err := s.Sections(context.Background(), SectionsInput{ListID: "List/1"})
	if err != nil || len(sections.Sections) != 2 || sections.Sections[0].ID != "ListSection/two" {
		t.Fatalf("sections: %+v %v", sections, err)
	}
	page, err := s.List(context.Background(), ListInput{ListID: "List/1", IncludeCompleted: true, View: "tree"})
	if err != nil || len(page.Reminders) != 4 || len(page.Tree) != 2 {
		t.Fatalf("tree page: %+v %v", page, err)
	}
	if page.Reminders[0].ID != "Reminder/DD1" || page.Tree[1].Reminder.ID != "Reminder/AA1" || page.Tree[1].Subtasks[0].Reminder.ID != "Reminder/BB1" || page.Tree[1].Subtasks[0].Subtasks[0].Reminder.ID != "Reminder/CC1" {
		t.Fatal("manual section order or nested subtasks lost")
	}
	if page.Reminders[3].Depth != 2 || page.Reminders[3].SectionName != "Heading one" || !strings.Contains(page.Legend["!!!"], "High") {
		t.Fatal("hierarchy/legend missing")
	}
	filtered, err := s.List(context.Background(), ListInput{ListID: "List/1", ParentID: "Reminder/BB1", View: "tree"})
	if err != nil || len(filtered.Tree) != 1 || filtered.Tree[0].Reminder.ID != "Reminder/CC1" || *filtered.Tree[0].Reminder.ParentRef != "Reminder/BB1" {
		t.Fatal("filtered parent identity lost")
	}
	paged, err := s.List(context.Background(), ListInput{ListID: "List/1", IncludeCompleted: true, View: "tree", Limit: 1, Offset: 2})
	if err != nil || paged.Total != 4 || paged.NextOffset == nil || *paged.NextOffset != 3 || len(paged.Tree) != 1 {
		t.Fatal("pagination changed matching counts or hid a subtask")
	}
	sectionPage, err := s.List(context.Background(), ListInput{SectionID: "ListSection/one", IncludeCompleted: true})
	if err != nil || sectionPage.Total != 3 {
		t.Fatal("section filtering lost inherited subtasks")
	}
}

func TestCreateNativeSectionPreservesMetadataAndPublishesAtomically(t *testing.T) {
	f := structureFixture(t)
	s := New(f.directory, time.Second)
	result, err := s.CreateSection(context.Background(), CreateSectionInput{ListID: "List/1", Title: "New heading"})
	if err != nil || result.Status != "created" || !strings.HasPrefix(result.ID, "ListSection/") {
		t.Fatalf("create: %+v %v", result, err)
	}
	if f.writes != 1 || f.uploads != 1 || f.records[result.ID]["recordType"] != "ListSection" {
		t.Fatal("section was not a native atomic creation")
	}
	persisted := cache.LoadFrom(f.directory)
	if persisted.SchemaVersion != 5 || len(persisted.PendingReadbacks) != 2 || persisted.PendingReadbacks[result.ID].Key() != persisted.Scopes["List/1"].Key() {
		t.Fatal("confirmed structure write did not persist scoped read-backs")
	}
	sectionFields := f.records[result.ID]["fields"].(map[string]any)
	name := sectionFields["DisplayName"].(map[string]any)
	created := sectionFields["CreationDate"].(map[string]any)
	list := sectionFields["List"].(map[string]any)
	if name["type"] != "STRING" || name["isEncrypted"] != true || name["value"] != "New heading" || created["type"] != "TIMESTAMP" || list["type"] != "REFERENCE" {
		t.Fatal("section creation did not match native CloudKit field types")
	}
	root := f.records["List/1"]["fields"].(map[string]any)
	asset := root["SectionIDsOrderingAsData"].(map[string]any)["value"].(map[string]any)
	var document struct {
		Identifiers []string `json:"orderedIdentifiers"`
		Future      string   `json:"future"`
	}
	if json.Unmarshal(f.assets[asset["receipt"].(string)], &document) != nil || len(document.Identifiers) != 3 || document.Identifiers[0] != "two" || document.Future != "preserved" {
		t.Fatal("existing section order/unknown metadata lost")
	}
	var tokens struct {
		Map    map[string]map[string]any `json:"map"`
		Future string                    `json:"future"`
	}
	_ = json.Unmarshal([]byte(root["ResolutionTokenMap"].(map[string]any)["value"].(string)), &tokens)
	if tokens.Map["name"]["counter"] != float64(8) || tokens.Map["sectionIDsOrderingAsData"]["counter"] != float64(5) || tokens.Future != "preserved" {
		t.Fatal("native resolution tokens overwritten")
	}
	sections, err := s.Sections(context.Background(), SectionsInput{ListID: "List/1"})
	if err != nil || len(sections.Sections) != 3 || sections.Sections[2].ID != result.ID {
		t.Fatal("created section did not survive fresh sync")
	}
	if len(cache.LoadFrom(f.directory).PendingReadbacks) != 0 || f.tokens[len(f.tokens)-1] == "" {
		t.Fatal("structure read-back reset account delta tokens or left stale records")
	}
}

func TestCreateSubtaskInheritsSectionAndReorderPreservesSubtrees(t *testing.T) {
	f := structureFixture(t)
	s := New(f.directory, time.Second)
	created, err := s.Create(context.Background(), CreateInput{Title: "Subtask", ListID: "List/1", ParentID: "Reminder/AA1"})
	if err != nil {
		t.Fatal(err)
	}
	read, err := s.Get(context.Background(), IDInput{ID: created.ID})
	if err != nil || *read.Reminder.ParentRef != "Reminder/AA1" || *read.Reminder.SectionRef != "ListSection/one" {
		t.Fatal("creation lost parent/section")
	}
	_, err = s.Reorder(context.Background(), ReorderInput{ListID: "List/1", ParentID: "Reminder/AA1", ReminderIDs: []string{created.ID, "Reminder/BB1"}})
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.List(context.Background(), ListInput{ListID: "List/1", IncludeCompleted: true})
	if err != nil || page.Reminders[2].ID != created.ID || page.Reminders[3].ID != "Reminder/BB1" || page.Reminders[4].ID != "Reminder/CC1" {
		t.Fatal("reorder detached descendant or ignored completed sibling")
	}
}

func TestMoveChangesParentAndSectionWithoutDroppingMetadata(t *testing.T) {
	f := structureFixture(t)
	s := New(f.directory, time.Second)
	_, err := s.Move(context.Background(), MoveInput{ID: "Reminder/AA1", SectionID: "ListSection/two", BeforeID: "Reminder/DD1"})
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.List(context.Background(), ListInput{ListID: "List/1", IncludeCompleted: true})
	if err != nil || page.Reminders[0].ID != "Reminder/AA1" || page.Reminders[3].ID != "Reminder/DD1" {
		t.Fatal("anchored move split subtree")
	}
	for _, r := range page.Reminders {
		if *r.SectionRef != "ListSection/two" {
			t.Fatal("descendant remained in old section")
		}
	}
	_, err = s.Move(context.Background(), MoveInput{ID: "Reminder/BB1", ClearParent: true, ClearSection: true})
	if err != nil {
		t.Fatal(err)
	}
	read, err := s.Get(context.Background(), IDInput{ID: "Reminder/BB1"})
	if err != nil || read.Reminder.ParentRef != nil || read.Reminder.SectionRef != nil {
		t.Fatal("clear parent/section failed")
	}
	root := f.records["List/1"]["fields"].(map[string]any)
	asset := root["MembershipsOfRemindersInSectionsAsData"].(map[string]any)["value"].(map[string]any)
	checksum := root["MembershipsOfRemindersInSectionsChecksum"].(map[string]any)
	if checksum["type"] != "STRING" || checksum["isEncrypted"] != true {
		t.Fatal("section membership checksum did not match native encrypted field type")
	}
	var doc map[string]any
	_ = json.Unmarshal(f.assets[asset["receipt"].(string)], &doc)
	if doc["future"] != "preserved" {
		t.Fatal("unknown membership fields lost")
	}
	for _, x := range doc["memberships"].([]any) {
		entry := x.(map[string]any)
		if entry["memberID"] == "AA1" && entry["future"] != "preserved" {
			t.Fatal("untouched membership fields lost")
		}
		if entry["memberID"] == "BB1" && entry["isObsolete"] != true {
			t.Fatal("clear did not retain native membership tombstone")
		}
	}
}

func TestInvalidStructureWritesNeverReachCloudKitModify(t *testing.T) {
	for _, input := range []MoveInput{
		{ID: "Reminder/AA1", ParentID: "Reminder/CC1"},
		{ID: "Reminder/AA1", ParentID: "Reminder/AA2"},
		{ID: "Reminder/AA1", BeforeID: "Reminder/AA2"},
		{ID: "Reminder/AA1", BeforeID: "Reminder/CC1"},
		{ID: "Reminder/BB1", ClearSection: true},
		{ID: "Reminder/AA1", SectionID: "ListSection/missing"},
	} {
		f := structureFixture(t)
		_, err := New(f.directory, time.Second).Move(context.Background(), input)
		if err == nil || f.writes != 0 || f.uploads != 0 {
			t.Fatalf("invalid move wrote data: %+v %v", input, err)
		}
	}
	f := structureFixture(t)
	_, err := New(f.directory, time.Second).Reorder(context.Background(), ReorderInput{ListID: "List/1", ParentID: "Reminder/AA1", ReminderIDs: []string{}})
	if err == nil || f.writes != 0 {
		t.Fatal("reorder omitted completed sibling")
	}
}

func TestStructureAssetAndRecordFailuresDoNotPublishOptimisticCache(t *testing.T) {
	for _, failure := range []string{"asset", "http", "record", "empty"} {
		t.Run(failure, func(t *testing.T) {
			f := structureFixture(t)
			f.failure = failure
			_, err := New(f.directory, time.Second).CreateSection(context.Background(), CreateSectionInput{ListID: "List/1", Title: "New"})
			if err == nil {
				t.Fatal("failure was accepted")
			}
			cached := cache.LoadFrom(f.directory)
			if len(cached.Sections) != 2 || len(cached.Structures["List/1"].SectionIDs) != 2 {
				t.Fatal("uncertain write published optimistic metadata")
			}
			if f.writes > 1 || f.uploads > 1 {
				t.Fatal("write retried")
			}
		})
	}
}

func TestUnknownMetadataVersionIsReadOnlyFailure(t *testing.T) {
	f := structureFixture(t)
	f.assets["section-order"] = []byte(`{"minimumSupportedVersion":99999999,"orderedIdentifiers":[]}`)
	_, err := New(f.directory, time.Second).CreateSection(context.Background(), CreateSectionInput{ListID: "List/1", Title: "New"})
	if err == nil || !strings.Contains(err.Error(), "unsupported_structure") || f.writes != 0 || f.uploads != 0 {
		t.Fatal("unsupported structure was rewritten")
	}
}

func TestSharedStructureWritesRespectPermissionsAndOriginalZone(t *testing.T) {
	f := newSharingCloud(t)
	s := New(f.directory, time.Second)
	_, err := s.CreateSection(context.Background(), CreateSectionInput{ListID: "List/shared", Title: "New"})
	if err != nil || len(f.writeScopes) != 1 || f.writeScopes[0] != "shared:foreign-owner" {
		t.Fatal("section write left incoming owner zone")
	}
	share := f.sharedRecords["Share/incoming"]
	person := share["participants"].([]any)[1].(map[string]any)
	person["permission"] = "READ_ONLY"
	beforeWrites, beforeUploads := f.writes, f.uploads
	_, err = s.Move(context.Background(), MoveInput{ID: "Reminder/shared"})
	if err == nil || !strings.Contains(err.Error(), "permission_denied") || f.writes != beforeWrites || f.uploads != beforeUploads {
		t.Fatal("read-only collaborator changed structure")
	}
}
