package reminders

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestLogicalIDAliasesTargetExistingNativeRecords(t *testing.T) {
	f := nativeOrderingFixture(t)
	s := New(f.directory, time.Second)
	ctx := context.Background()
	got, err := s.Get(ctx, IDInput{ID: "A"})
	if err != nil || got.Reminder.ID != "Reminder/A" {
		t.Fatal("bare alias did not resolve to existing native record")
	}
	page, err := s.List(ctx, ListInput{ListID: "List/1", ParentID: "A", IncludeCompleted: true})
	if err != nil || len(page.Reminders) != 3 {
		t.Fatal("parent alias filter failed")
	}
	result, err := s.Reorder(ctx, ReorderInput{ListID: "List/1", ReminderIDs: []string{"C", "A", "B"}})
	if err != nil || result.OrderVerification != "verified" {
		t.Fatal("logical-ID reorder was not verified")
	}
	result, err = s.Move(ctx, MoveInput{ID: "B", BeforeID: "C"})
	if err != nil || result.ID != "Reminder/B" {
		t.Fatal("anchor aliases failed")
	}
	created, err := s.Create(ctx, CreateInput{ListID: "List/1", ParentID: "A", Title: "🧪 Native child"})
	if err != nil || !strings.HasPrefix(created.ID, "Reminder/") {
		t.Fatal("new reminder was created with a non-native name")
	}
	bare := strings.TrimPrefix(created.ID, "Reminder/")
	updated, err := s.Update(ctx, UpdateInput{ID: bare, Title: "🥰 Updated"})
	if err != nil || updated.ID != created.ID {
		t.Fatal("update alias failed")
	}
	done, err := s.Complete(ctx, IDInput{ID: bare})
	if err != nil || done.ID != created.ID {
		t.Fatal("completion alias failed")
	}
	deleted, err := s.Delete(ctx, DeleteInput{ID: bare, Confirm: true})
	if err != nil || deleted.ID != created.ID {
		t.Fatal("delete alias failed")
	}
	if f.records["A"] != nil || f.records["B"] != nil {
		t.Fatal("alias created a second opaque record")
	}
}
