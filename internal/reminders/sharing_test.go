package reminders

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icloud-reminders/internal/cache"
	"icloud-reminders/internal/utils"
)

func newSharingCloud(t *testing.T) *cloudFixture {
	f := newCloud(t)
	field := func(value any) any { return map[string]any{"value": value} }
	person := func(id, role, permission, status string) any {
		return map[string]any{"participantId": id, "type": role, "permission": permission, "acceptanceStatus": status,
			"userIdentity": map[string]any{"nameComponents": map[string]any{"givenName": "Private", "familyName": id}, "lookupInfo": map[string]any{"emailAddress": "private-person@example.invalid"}}}
	}
	share := func(name, actor string, people []any) map[string]any {
		return map[string]any{"recordName": name, "recordType": "cloudkit.share", "participants": people, "currentUserParticipant": map[string]any{"participantId": actor}}
	}
	f.records["List/1"]["share"] = map[string]any{"recordName": "Share/owned"}
	f.records["Share/owned"] = share("Share/owned", "me", []any{
		person("me", "OWNER", "READ_WRITE", "ACCEPTED"), person("other", "USER", "READ_WRITE", "ACCEPTED"),
		person("invited", "USER", "READ_WRITE", "PENDING"), person("removed", "USER", "NONE", "REMOVED"),
	})
	title, err := utils.EncodeTitle("private-shared-reminder")
	if err != nil {
		t.Fatal(err)
	}
	f.sharedRecords = map[string]map[string]any{
		"List/shared": {"recordName": "List/shared", "recordType": "List", "recordChangeTag": "shared-list-tag", "fields": map[string]any{"Name": field("private-shared-list")}, "share": map[string]any{"recordName": "Share/incoming"}},
		"Reminder/shared": {"recordName": "Reminder/shared", "recordType": "Reminder", "recordChangeTag": "shared-tag", "fields": map[string]any{
			"TitleDocument": field(title), "Completed": field(0), "List": field(map[string]any{"recordName": "List/shared"}), "AssignmentIDs": field([]string{"existing"}),
		}},
		"Assignment/existing": {"recordName": "Assignment/existing", "recordType": "Assignment", "recordChangeTag": "assignment-tag", "parent": map[string]any{"recordName": "Reminder/shared"}, "fields": map[string]any{
			"Reminder": field(map[string]any{"recordName": "Reminder/shared"}), "EncryptedAssigneeIdentifier": field("them"), "Status": field(1),
		}},
		"Share/incoming": share("Share/incoming", "me-shared", []any{person("them", "OWNER", "READ_WRITE", "ACCEPTED"), person("me-shared", "USER", "READ_WRITE", "ACCEPTED")}),
	}
	return f
}

func TestSharedParticipantsAndAssignmentLifecycle(t *testing.T) {
	f := newSharingCloud(t)
	s := New(f.directory, 5*time.Second)
	ctx := context.Background()
	people, err := s.Participants(ctx, ParticipantsInput{ListID: "List/1"})
	if err != nil || !people.Shared || len(people.Participants) != 2 {
		t.Fatalf("participants failed: %v", err)
	}
	if people.Participants[0].ID != "me" || !people.Participants[0].IsCurrentUser || people.Participants[1].ID != "other" {
		t.Fatal("participant identities changed")
	}
	private, err := s.Participants(ctx, ParticipantsInput{ListID: "List/2"})
	if err != nil || private.Shared || len(private.Participants) != 0 {
		t.Fatal("private list exposed collaborators")
	}
	lists, err := s.ListLists(ctx)
	if err != nil || len(lists.Lists) != 3 {
		t.Fatal("incoming shared list was not discovered")
	}
	result, err := s.Assign(ctx, AssignInput{ID: "Reminder/AA1", ParticipantID: "other"})
	if err != nil || result.Status != "assigned" {
		t.Fatalf("assign failed: %v", err)
	}
	item, err := s.Get(ctx, IDInput{ID: "Reminder/AA1"})
	if err != nil || item.Reminder.AssigneeID == nil || *item.Reminder.AssigneeID != "other" {
		t.Fatal("assignment was not returned by reads")
	}
	stored := cache.LoadFrom(f.directory)
	ids := stored.Reminders["Reminder/AA1"].AssignmentIDs
	if len(ids) != 1 {
		t.Fatal("assignment was not linked")
	}
	native := f.records[ids[0]]
	fields := native["fields"].(map[string]any)
	if fields["EncryptedAssigneeIdentifier"].(map[string]any)["isEncrypted"] != true || fields["EncryptedOriginatorIdentifier"].(map[string]any)["isEncrypted"] != true {
		t.Fatal("identity fields were written unencrypted")
	}
	if native["parent"].(map[string]any)["recordName"] != "Reminder/AA1" {
		t.Fatal("assignment has the wrong sharing parent")
	}
	if fields["Reminder"].(map[string]any)["value"].(map[string]any)["action"] != "VALIDATE" {
		t.Fatal("assignment reference does not validate its reminder")
	}
	if f.writeScopes[0] != "private:owner" {
		t.Fatal("owned assignment used the wrong database")
	}
	writes := f.writes
	result, err = s.Assign(ctx, AssignInput{ID: "Reminder/AA1", ParticipantID: "other"})
	if err != nil || result.Status != "unchanged" || f.writes != writes {
		t.Fatal("same assignment was rewritten")
	}
	if _, err := s.Assign(ctx, AssignInput{ID: "Reminder/AA1", ParticipantID: "me"}); err != nil {
		t.Fatal(err)
	}
	if f.records[ids[0]] == nil || len(cache.LoadFrom(f.directory).Reminders["Reminder/AA1"].AssignmentIDs) != 1 {
		t.Fatal("reassignment duplicated a child")
	}
	result, err = s.Assign(ctx, AssignInput{ID: "Reminder/AA1", Clear: true})
	if err != nil || result.Status != "unassigned" {
		t.Fatalf("clear failed: %v", err)
	}
	item, err = s.Get(ctx, IDInput{ID: "Reminder/AA1"})
	if err != nil || item.Reminder.AssigneeID != nil {
		t.Fatal("cleared assignment remained visible")
	}
	writes = f.writes
	if result, err := s.Assign(ctx, AssignInput{ID: "Reminder/AA1", Clear: true}); err != nil || result.Status != "unchanged" || f.writes != writes {
		t.Fatal("empty assignment was rewritten")
	}
}

func TestIncomingSharedAssignmentAndExistingWritesUseOriginalZone(t *testing.T) {
	f := newSharingCloud(t)
	s := New(f.directory, 5*time.Second)
	ctx := context.Background()
	people, err := s.Participants(ctx, ParticipantsInput{ListID: "List/shared"})
	if err != nil || !people.Shared || len(people.Participants) != 2 {
		t.Fatal("incoming participants are unavailable")
	}
	item, err := s.Get(ctx, IDInput{ID: "Reminder/shared"})
	if err != nil || item.Reminder.AssigneeID == nil || *item.Reminder.AssigneeID != "them" {
		t.Fatal("existing shared assignment was not read")
	}
	if _, err := s.Assign(ctx, AssignInput{ID: "Reminder/shared", ParticipantID: "me-shared"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Assign(ctx, AssignInput{ID: "Reminder/shared", Clear: true}); err != nil {
		t.Fatal(err)
	}
	created, err := s.Create(ctx, CreateInput{Title: "shared child", ListID: "List/shared"})
	if err != nil {
		t.Fatal(err)
	}
	if parent, ok := f.sharedRecords[created.ID]["parent"].(map[string]any); !ok || parent["recordName"] != "List/shared" {
		t.Fatal("new reminder does not inherit the shared list")
	}
	if _, err := s.Assign(ctx, AssignInput{ID: created.ID, ParticipantID: "me-shared"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(ctx, UpdateInput{ID: created.ID, Title: "changed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Complete(ctx, IDInput{ID: created.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Delete(ctx, DeleteInput{ID: created.ID, Confirm: true}); err != nil {
		t.Fatal(err)
	}
	for _, scope := range f.writeScopes {
		if scope != "shared:foreign-owner" {
			t.Fatal("shared write was routed to the private database")
		}
	}
	if _, exists := f.records[created.ID]; exists {
		t.Fatal("shared reminder leaked into private records")
	}
}

func TestAssignmentValidationAndPermissionsPreventWrites(t *testing.T) {
	for _, input := range []AssignInput{
		{}, {ID: "Reminder/AA1"}, {ID: "Reminder/AA1", ParticipantID: "other", Clear: true},
		{ID: "AA1", ParticipantID: "other"}, {ID: "Reminder/AA1", ParticipantID: "invited"},
		{ID: "Reminder/AA1", ParticipantID: "removed"}, {ID: "Reminder/AA1", ParticipantID: "me-shared"},
		{ID: "Reminder/AA1", ParticipantID: "private-person@example.invalid"},
		{ID: "Reminder/AA2", ParticipantID: "other"},
	} {
		f := newSharingCloud(t)
		if _, err := New(f.directory, 5*time.Second).Assign(context.Background(), input); err == nil || f.writes != 0 {
			t.Fatal("invalid assignment was allowed")
		}
	}
	for _, mode := range []string{"read-only", "actor-absent", "revoked-share", "broken-link"} {
		t.Run(mode, func(t *testing.T) {
			f := newSharingCloud(t)
			people := f.records["Share/owned"]["participants"].([]any)
			switch mode {
			case "read-only":
				people[0].(map[string]any)["type"], people[0].(map[string]any)["permission"] = "USER", "READ_ONLY"
			case "actor-absent":
				f.records["Share/owned"]["currentUserParticipant"] = map[string]any{}
			case "revoked-share":
				delete(f.records, "Share/owned")
			case "broken-link":
				f.records["Reminder/AA1"]["fields"].(map[string]any)["AssignmentIDs"] = map[string]any{"value": []string{"missing"}}
			}
			_, err := New(f.directory, 5*time.Second).Assign(context.Background(), AssignInput{ID: "Reminder/AA1", ParticipantID: "other"})
			if err == nil || f.writes != 0 || strings.Contains(err.Error(), "private-") {
				t.Fatal("unsafe assignment or error")
			}
		})
	}
}

func TestAssignmentFailuresNeverPublishOptimisticState(t *testing.T) {
	for _, failure := range []string{"http", "record", "empty"} {
		for _, mode := range []string{"assign", "clear"} {
			t.Run(failure+"/"+mode, func(t *testing.T) {
				f := newSharingCloud(t)
				s := New(f.directory, 5*time.Second)
				if mode == "clear" {
					if _, err := s.Assign(context.Background(), AssignInput{ID: "Reminder/AA1", ParticipantID: "other"}); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := s.ListLists(context.Background()); err != nil {
					t.Fatal(err)
				}
				before := cache.LoadFrom(f.directory)
				oldReminders, _ := json.Marshal(before.Reminders)
				oldAssignments, _ := json.Marshal(before.Assignments)
				writes := f.writes
				f.failure = failure
				_, err := s.Assign(context.Background(), AssignInput{ID: "Reminder/AA1", ParticipantID: func() string {
					if mode == "clear" {
						return ""
					}
					return "other"
				}(), Clear: mode == "clear"})
				if err == nil || f.writes != writes+1 || strings.Contains(err.Error(), "private-upstream-body") {
					t.Fatal("missing, unsafe or retried write failure")
				}
				after := cache.LoadFrom(f.directory)
				reminders, _ := json.Marshal(after.Reminders)
				assignments, _ := json.Marshal(after.Assignments)
				if string(oldReminders) != string(reminders) || string(oldAssignments) != string(assignments) {
					t.Fatal("failed assignment changed the local cache")
				}
			})
		}
	}
}

func TestRevokedSharedZoneAndCollidingNamesCannotReuseCachedRecords(t *testing.T) {
	for _, mode := range []string{"revoked", "collision", "failed-discovery"} {
		t.Run(mode, func(t *testing.T) {
			f := newSharingCloud(t)
			s := New(f.directory, 5*time.Second)
			if _, err := s.ListLists(context.Background()); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(filepath.Join(f.directory, "ck_cache.json"))
			switch mode {
			case "revoked":
				f.sharedRecords = nil
			case "collision":
				f.sharedRecords["Reminder/AA1"] = f.records["Reminder/AA1"]
			case "failed-discovery":
				f.failure = "shared503"
			}
			_, err := s.ListLists(context.Background())
			if mode == "revoked" {
				if err != nil {
					t.Fatal(err)
				}
				stored := cache.LoadFrom(f.directory)
				if stored.Reminders["Reminder/shared"] != nil || stored.Assignments["Assignment/existing"] != nil || stored.Lists["List/shared"] != "" {
					t.Fatal("revoked records remained cached")
				}
				if _, err := s.Assign(context.Background(), AssignInput{ID: "Reminder/shared", ParticipantID: "me-shared"}); err == nil || f.writes != 0 {
					t.Fatal("revoked reminder remained writable")
				}
			} else {
				if err == nil {
					t.Fatal("invalid shared state was accepted")
				}
				after, _ := os.ReadFile(filepath.Join(f.directory, "ck_cache.json"))
				if string(before) != string(after) {
					t.Fatal("incomplete sync replaced the last complete snapshot")
				}
			}
		})
	}
}
