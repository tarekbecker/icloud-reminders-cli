package reminders

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"
)

func mixedOrderingFixture(t *testing.T) *cloudFixture {
	f := newCloud(t)
	for id := range f.records {
		if id != "List/1" && id != "List/2" {
			delete(f.records, id)
		}
	}
	field := func(v any) any { return map[string]any{"value": v} }
	ref := func(id string) any { return field(map[string]any{"recordName": id}) }
	for _, id := range []string{"A", "B", "Reminder/C", "A1", "Reminder/A2", "A3"} {
		fields := map[string]any{"List": ref("List/1"), "Completed": field(0)}
		if id == "A1" || id == "Reminder/A2" || id == "A3" {
			fields["ParentReminder"] = ref("A")
		}
		if id == "Reminder/A2" {
			fields["Completed"] = field(1)
		}
		f.records[id] = map[string]any{"recordName": id, "recordType": "Reminder", "recordChangeTag": "tag-1", "fields": fields}
	}
	f.records["List/1"]["fields"].(map[string]any)["ReminderIDs"] = field(`["B","A","A2","A1","A3","C","historical","Reminder/A"]`)
	return f
}

func nativeOrderingFixture(t *testing.T) *cloudFixture {
	f := mixedOrderingFixture(t)
	for _, id := range []string{"A", "B", "A1", "A3"} {
		record := f.records[id]
		delete(f.records, id)
		record["recordName"] = "Reminder/" + id
		f.records["Reminder/"+id] = record
	}
	for _, id := range []string{"Reminder/A1", "Reminder/A2", "Reminder/A3"} {
		f.records[id]["fields"].(map[string]any)["ParentReminder"] = map[string]any{"value": map[string]any{"recordName": "Reminder/A"}}
	}
	return f
}

func TestManualOrderingRoundTripsNativeRecords(t *testing.T) {
	f := nativeOrderingFixture(t)
	s := New(f.directory, time.Second)
	ctx := context.Background()
	check := func(parent string, want []string) {
		t.Helper()
		if _, err := s.Sync(ctx, SyncInput{}); err != nil {
			t.Fatal(err)
		}
		page, err := s.List(ctx, ListInput{ListID: "List/1", ParentID: parent, IncludeCompleted: true, View: "tree"})
		if err != nil {
			t.Fatal(err)
		}
		got := []string{}
		for _, item := range page.Reminders {
			if parent != "" || item.ParentRef == nil {
				got = append(got, item.ID)
			}
			if item.SortIndex < 0 {
				t.Fatal("active reminder was not resolved to native manual order")
			}
		}
		if !slices.Equal(got, want) {
			t.Fatalf("wrong sibling order: %v", got)
		}
	}
	check("", []string{"Reminder/B", "Reminder/A", "Reminder/C"})
	result, err := s.Reorder(ctx, ReorderInput{ListID: "List/1", ReminderIDs: []string{"Reminder/C", "Reminder/A", "Reminder/B"}})
	if err != nil || result.OrderVerification != "verified" {
		t.Fatal("mixed top-level reorder was not verified")
	}
	check("", []string{"Reminder/C", "Reminder/A", "Reminder/B"})
	result, err = s.Reorder(ctx, ReorderInput{ListID: "List/1", ParentID: "Reminder/A", ReminderIDs: []string{"Reminder/A3", "Reminder/A1", "Reminder/A2"}})
	if err != nil || result.OrderVerification != "verified" {
		t.Fatal("mixed child reorder was not verified")
	}
	check("Reminder/A", []string{"Reminder/A3", "Reminder/A1", "Reminder/A2"})
	for _, step := range []struct {
		input  MoveInput
		parent string
		want   []string
	}{
		{MoveInput{ID: "Reminder/B", BeforeID: "Reminder/C"}, "", []string{"Reminder/B", "Reminder/C", "Reminder/A"}},
		{MoveInput{ID: "Reminder/B", ClearParent: true}, "", []string{"Reminder/C", "Reminder/A", "Reminder/B"}},
		{MoveInput{ID: "Reminder/A2", BeforeID: "Reminder/A3"}, "Reminder/A", []string{"Reminder/A2", "Reminder/A3", "Reminder/A1"}},
		{MoveInput{ID: "Reminder/A2", AfterID: "Reminder/A1"}, "Reminder/A", []string{"Reminder/A3", "Reminder/A1", "Reminder/A2"}},
	} {
		result, err := s.Move(ctx, step.input)
		if err != nil || result.OrderVerification != "verified" {
			t.Fatal("mixed anchored/append move was not verified")
		}
		check(step.parent, step.want)
	}
	if _, err := s.Sync(ctx, SyncInput{Full: true}); err != nil {
		t.Fatal(err)
	}
	check("", []string{"Reminder/C", "Reminder/A", "Reminder/B"})
	check("Reminder/A", []string{"Reminder/A3", "Reminder/A1", "Reminder/A2"})
	page, _ := s.List(ctx, ListInput{ListID: "List/1", IncludeCompleted: true})
	positions := map[int]bool{}
	for _, item := range page.Reminders {
		if positions[item.SortIndex] {
			t.Fatal("different records share a manual index")
		}
		positions[item.SortIndex] = true
	}
	var ids []string
	root := f.records["List/1"]["fields"].(map[string]any)
	_ = json.Unmarshal([]byte(root["ReminderIDs"].(map[string]any)["value"].(string)), &ids)
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatal("write duplicated logical ordering IDs")
		}
		seen[id] = true
	}
	if !seen["historical"] || f.records["Reminder/A"] == nil {
		t.Fatal("historical metadata lost or opaque record name rewritten")
	}
}

func TestOrderVerificationRejectsAcknowledgedButIgnoredWrites(t *testing.T) {
	for _, failure := range []string{"ordering_ignored", "verification_lookup"} {
		for _, operation := range []string{"reorder", "move"} {
			t.Run(failure+"/"+operation, func(t *testing.T) {
				f := nativeOrderingFixture(t)
				f.failure = failure
				s := New(f.directory, time.Second)
				var result MutationResult
				var err error
				if operation == "move" {
					result, err = s.Move(context.Background(), MoveInput{ID: "Reminder/A", BeforeID: "Reminder/B"})
				} else {
					result, err = s.Reorder(context.Background(), ReorderInput{ListID: "List/1", ReminderIDs: []string{"Reminder/C", "Reminder/A", "Reminder/B"}})
				}
				if err == nil || result.Status != "" || f.writes != 1 {
					t.Fatal("ignored/unverified mutation returned success or retried")
				}
				details := ErrorDetails(err, operation, "safe-request", true)
				code, verification := "upstream_mismatch", "mismatch"
				if failure == "verification_lookup" {
					code, verification = "write_verification_failed", "unavailable"
				}
				if details["error_code"] != code || details["write_status"] != "succeeded" || details["order_verification"] != verification || details["retry_class"] != "retryable_after_read" {
					t.Fatal("known commit confused with unverified effect")
				}
			})
		}
	}
}

func TestAmbiguousLogicalIDsAreIsolatedAndNeverWritten(t *testing.T) {
	f := mixedOrderingFixture(t)
	f.records["Reminder/A"] = map[string]any{"recordName": "Reminder/A", "recordType": "Reminder", "recordChangeTag": "tag-1", "fields": f.records["A"]["fields"]}
	s := New(f.directory, time.Second)
	page, err := s.List(context.Background(), ListInput{ListID: "List/1", IncludeCompleted: true})
	if err != nil || len(page.Reminders) != 7 || len(page.StructureWarnings) != 1 {
		t.Fatal("distinct opaque records merged or account blocked")
	}
	_, err = s.Move(context.Background(), MoveInput{ID: "A", BeforeID: "B"})
	if err == nil || f.writes != 0 || ErrorDetails(err, "move", "safe-request", true)["write_status"] != "not_sent" {
		t.Fatal("ambiguous ordering identity was written")
	}
}

func TestMissingOrderEntryReportsUnavailablePosition(t *testing.T) {
	f := mixedOrderingFixture(t)
	f.records["List/1"]["fields"].(map[string]any)["ReminderIDs"] = map[string]any{"value": `["B"]`}
	page, err := New(f.directory, time.Second).List(context.Background(), ListInput{ListID: "List/1", IncludeCompleted: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range page.Reminders {
		if r.ID == "B" && r.SortIndex != 0 || r.ID != "B" && r.SortIndex != -1 {
			t.Fatal("invented position for a missing native order entry")
		}
	}
}

func TestBareReminderAndSectionNamesResolveNativeMemberships(t *testing.T) {
	f := structureFixture(t)
	for _, id := range []string{"AA1", "BB1", "CC1"} {
		record := f.records["Reminder/"+id]
		delete(f.records, "Reminder/"+id)
		record["recordName"] = id
		f.records[id] = record
	}
	ref := func(id string) any { return map[string]any{"value": map[string]any{"recordName": id}} }
	f.records["BB1"]["fields"].(map[string]any)["ParentReminder"] = ref("AA1")
	f.records["CC1"]["fields"].(map[string]any)["ParentReminder"] = ref("BB1")
	section := f.records["ListSection/one"]
	delete(f.records, "ListSection/one")
	section["recordName"] = "one"
	f.records["one"] = section
	s := New(f.directory, time.Second)
	page, err := s.List(context.Background(), ListInput{SectionID: "one", IncludeCompleted: true})
	if err != nil || len(page.Reminders) != 3 {
		t.Fatal("bare section/reminder membership was not resolved")
	}
	for _, r := range page.Reminders {
		if r.SectionRef == nil || *r.SectionRef != "one" {
			t.Fatal("opaque section identity was rewritten")
		}
	}
	_, err = s.Move(context.Background(), MoveInput{ID: "AA1", SectionID: "ListSection/two", BeforeID: "Reminder/DD1"})
	if err == nil || f.writes != 0 || ErrorDetails(err, "move", "safe-request", true)["structure_reason"] != "non_native_record_id" {
		t.Fatal("non-native records were reported as writable native ordering")
	}
}

func TestBareRecordNamesRemainReadableButCannotReportNativeOrderSuccess(t *testing.T) {
	for _, operation := range []string{"move", "reorder", "create"} {
		t.Run(operation, func(t *testing.T) {
			f := mixedOrderingFixture(t)
			s := New(f.directory, time.Second)
			page, err := s.List(context.Background(), ListInput{ListID: "List/1", IncludeCompleted: true})
			if err != nil || len(page.Reminders) != 6 {
				t.Fatal("legacy recovery reads failed")
			}
			switch operation {
			case "move":
				_, err = s.Move(context.Background(), MoveInput{ID: "A", BeforeID: "B"})
			case "reorder":
				_, err = s.Reorder(context.Background(), ReorderInput{ListID: "List/1", ReminderIDs: []string{"A", "B", "Reminder/C"}})
			case "create":
				_, err = s.Create(context.Background(), CreateInput{ListID: "List/1", Title: "new"})
			}
			d := ErrorDetails(err, operation, "safe-request", true)
			if err == nil || f.writes != 0 || d["write_status"] != "not_sent" || d["structure_reason"] != "non_native_record_id" {
				t.Fatal("non-native write was not blocked before dispatch")
			}
			other, err := s.List(context.Background(), ListInput{ListID: "List/2"})
			if err != nil || other.Total != 0 {
				t.Fatal("legacy list blocked other lists")
			}
		})
	}
}
