package reminders

import (
	"context"
	"icloud-reminders/internal/cache"
	"testing"
	"time"
)

func TestUnsupportedListDoesNotBlockAccountOrLoseLegacyData(t *testing.T) {
	f := structureFixture(t)
	ctx := context.Background()
	s := New(f.directory, time.Second)
	if _, err := s.Sync(ctx, SyncInput{}); err != nil {
		t.Fatal(err)
	}
	f.assets["section-order"] = []byte(`{"minimumSupportedVersion":99999999,"orderedIdentifiers":[]}`)
	lists, err := s.ListLists(ctx)
	if err != nil || len(lists.Lists) != 2 || len(lists.StructureWarnings) != 1 {
		t.Fatal("unsupported list blocked discovery")
	}
	issue := lists.StructureWarnings[0]
	if issue.ListID != "List/1" || issue.Field != "SectionIDsOrderingAsData" || issue.Reason != "unsupported_version" || issue.Version == nil || *issue.Version != 99999999 {
		t.Fatal("parser diagnostic lost")
	}
	page, err := s.List(ctx, ListInput{ListID: "List/2", IncludeCompleted: true})
	if err != nil || len(page.Reminders) != 1 || len(page.StructureWarnings) != 0 {
		t.Fatal("other list blocked")
	}
	legacy, err := s.List(ctx, ListInput{ListID: "List/1", IncludeCompleted: true, View: "tree"})
	if err != nil || len(legacy.Reminders) != 4 || len(legacy.StructureWarnings) != 1 {
		t.Fatal("legacy reminders disappeared")
	}
	for _, r := range legacy.Reminders {
		if r.SortIndex != -1 || r.SectionRef != nil {
			t.Fatal("stale native metadata reused")
		}
		if r.ID == "Reminder/CC1" && (r.Depth != 2 || r.ParentRef == nil) {
			t.Fatal("base parent hierarchy lost")
		}
	}
	one, err := s.Get(ctx, IDInput{ID: "Reminder/BB1"})
	if err != nil || !one.Reminder.Completed || len(one.StructureWarnings) != 1 {
		t.Fatal("legacy completed reminder blocked")
	}
	result, err := s.Sync(ctx, SyncInput{Full: true})
	if err != nil || result.Lists != 2 || result.Reminders != 5 || len(result.StructureWarnings) != 1 {
		t.Fatal("full sync blocked")
	}
	cached := cache.LoadFrom(f.directory)
	if cached.Structures["List/1"] != nil || cached.StructureIssues["List/1"] == nil {
		t.Fatal("warning not persisted or stale structure survived")
	}
	before := f.writes
	_, err = s.CreateSection(ctx, CreateSectionInput{ListID: "List/1", Title: "Blocked"})
	details := ErrorDetails(err, "create_reminder_section", "request", true)
	if err == nil || details["write_status"] != "not_sent" || details["structure_field"] != "SectionIDsOrderingAsData" || f.writes != before {
		t.Fatal("unsafe unsupported-list write")
	}
	if _, err = s.Create(ctx, CreateInput{ListID: "List/2", Title: "Unaffected"}); err != nil {
		t.Fatal("unaffected write blocked")
	}
	f.assets["section-order"] = []byte(`{"minimumSupportedVersion":20230430,"orderedIdentifiers":["two","one"]}`)
	restored, err := s.Sync(ctx, SyncInput{Full: true})
	if err != nil || len(restored.StructureWarnings) != 0 {
		t.Fatal("warning survived repair")
	}
}

func TestReplicatedOrderingDuplicatesRemainReadableAndWritable(t *testing.T) {
	f := newCloud(t)
	root := f.records["List/1"]["fields"].(map[string]any)
	root["ReminderIDs"] = map[string]any{"value": `["BB1","AA1","Reminder/BB1","AA1"]`}
	s := New(f.directory, time.Second)
	ctx := context.Background()
	lists, err := s.ListLists(ctx)
	if err != nil || len(lists.StructureWarnings) != 0 {
		t.Fatal("duplicate order still rejected")
	}
	page, err := s.List(ctx, ListInput{ListID: "List/1", IncludeCompleted: true})
	if err != nil || len(page.Reminders) != 2 {
		t.Fatal("duplicate order duplicated objects")
	}
	if cached := cache.LoadFrom(f.directory); len(cached.Structures["List/1"].ReminderIDs) != 2 || cached.Structures["List/1"].ReminderIDs[0] != "Reminder/BB1" {
		t.Fatal("first occurrence order not preserved")
	}
	if _, err = s.Move(ctx, MoveInput{ID: "Reminder/BB1", ClearParent: true}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Reorder(ctx, ReorderInput{ListID: "List/1", ReminderIDs: []string{"Reminder/AA1", "Reminder/BB1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Delete(ctx, DeleteInput{ID: "Reminder/BB1", Confirm: true}); err != nil {
		t.Fatal(err)
	}
}

func TestUnavailableMetadataAssetIsIsolatedAndCancellationStillAborts(t *testing.T) {
	f := structureFixture(t)
	delete(f.assets, "section-order")
	ctx := context.Background()
	s := New(f.directory, time.Second)
	lists, err := s.ListLists(ctx)
	if err != nil || len(lists.Lists) != 2 || len(lists.StructureWarnings) != 1 || lists.StructureWarnings[0].Reason != "metadata_unavailable" {
		t.Fatal("asset outage blocked discovery")
	}
	_, err = s.CreateSection(ctx, CreateSectionInput{ListID: "List/1", Title: "Blocked"})
	details := ErrorDetails(err, "create_reminder_section", "request", true)
	if err == nil || details["write_status"] != "not_sent" || details["retry_class"] != "retryable_safe" || f.writes != 0 {
		t.Fatal("metadata outage classified as an uncertain write")
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = s.Sync(cancelCtx, SyncInput{Full: true}); err == nil {
		t.Fatal("canceled sync continued")
	}
}
