package reminders

import (
	"context"
	"github.com/google/uuid"
	"strings"
	"testing"
	"time"
)

func TestRepeatedExactWriteLifecycle(t *testing.T) {
	f := newCloud(t)
	s := New(f.directory, 5*time.Second)
	ctx := context.Background()
	titles := []string{"Rollläden im Wohnzimmer getrennt steuerbar", "Glücklich sein und sich lieben! 🥰",
		"3,43 % Sparkasse Dortmund, 10 Jahre Zinsbindung", "❤️", "ÄÖÜ äöü ß", "!#$%&'()*+,-./:;<=>?@[\\]^_`{|}~"}
	for index := 0; index < 100; index++ {
		title := titles[index%len(titles)]
		created, err := s.Create(ctx, CreateInput{Title: title, ListID: "List/1", Due: "2026-10-15", Priority: "high", Notes: title})
		if err != nil {
			t.Fatal(err)
		}
		read, err := s.Get(ctx, IDInput{ID: created.ID})
		if err != nil || read.Reminder.Title != title || read.Reminder.Notes == nil || *read.Reminder.Notes != title || read.Reminder.Priority != 1 || read.Reminder.Due == nil || *read.Reminder.Due != "2026-10-15" {
			t.Fatalf("roundtrip %d", index)
		}
		if _, err := s.Update(ctx, UpdateInput{ID: created.ID, Title: title + " updated"}); err != nil {
			t.Fatal(err)
		}
		read, err = s.Get(ctx, IDInput{ID: created.ID})
		if err != nil || read.Reminder.Title != title+" updated" {
			t.Fatal("update did not roundtrip")
		}
		if _, err := s.Complete(ctx, IDInput{ID: created.ID}); err != nil {
			t.Fatal(err)
		}
		read, err = s.Get(ctx, IDInput{ID: created.ID})
		if err != nil || !read.Reminder.Completed {
			t.Fatal("complete did not roundtrip")
		}
		if _, err := s.Delete(ctx, DeleteInput{ID: created.ID, Confirm: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Get(ctx, IDInput{ID: created.ID}); err == nil {
			t.Fatal("delete did not roundtrip")
		}
	}
}

func TestKeyedCreationRecoveryAcrossRestart(t *testing.T) {
	f := newCloud(t)
	ctx := context.Background()
	s := New(f.directory, 5*time.Second)
	in := CreateInput{Title: "Exact keyed title 🥰", ListID: "List/1", ClientRequestID: uuid.NewString()}
	// The fake iCloud applies the write but drops the application response.
	f.failure = "lost_create_response"
	if _, err := s.Create(ctx, in); err == nil {
		t.Fatal("lost response unexpectedly confirmed")
	}
	if f.writes != 1 {
		t.Fatal("write automatically retried")
	}
	f.failure = ""
	restarted := New(f.directory, 5*time.Second)
	recovered, err := restarted.Create(ctx, in)
	if err != nil || recovered.Status != "already_created" || recovered.ID == "" {
		t.Fatalf("recovery: %v", err)
	}
	if f.writes != 1 {
		t.Fatal("recovery created a duplicate")
	}
	again, err := New(f.directory, 5*time.Second).Create(ctx, in)
	if err != nil || again.ID != recovered.ID || again.Status != "already_created" || f.writes != 1 {
		t.Fatal("durable replay failed")
	}
	in.Title += " changed"
	if _, err := restarted.Create(ctx, in); err == nil || PublicError(err).Code != "idempotency_conflict" {
		t.Fatal("key reuse with different payload accepted")
	}
}

func TestSafeWriteEvidence(t *testing.T) {
	for _, failure := range []string{"http", "record", "empty", "lost_create_response"} {
		t.Run(failure, func(t *testing.T) {
			f := newCloud(t)
			s := New(f.directory, 5*time.Second)
			f.failure = failure
			_, err := s.Create(context.Background(), CreateInput{Title: "safe title", ListID: "List/1"})
			if err == nil {
				t.Fatal("failure was accepted")
			}
			details := ErrorDetails(err, "create_reminder", "safe-request-id", true)
			expected := "unknown"
			if failure == "record" {
				expected = "failed"
			}
			if details["write_status"] != expected {
				t.Fatalf("incorrect write evidence: %+v", details)
			}
			if strings.Contains(err.Error(), "private-upstream-body") {
				t.Fatal("private response escaped")
			}
			if f.writes != 1 {
				t.Fatal("write retried")
			}
		})
	}
}

func TestKeyedOrganizedCreationKeepsSectionAndOrderAcrossRecovery(t *testing.T) {
	f := structureFixture(t)
	ctx := context.Background()
	in := CreateInput{Title: "Exact title ❤️", ListID: "List/1", SectionID: "ListSection/two", ClientRequestID: uuid.NewString()}
	f.failure = "lost_create_response"
	if _, err := New(f.directory, time.Second).Create(ctx, in); err == nil {
		t.Fatal("lost response unexpectedly confirmed")
	}
	f.failure = ""
	recovered, err := New(f.directory, time.Second).Create(ctx, in)
	if err != nil || recovered.Status != "already_created" || f.writes != 1 {
		t.Fatalf("organized recovery failed: %v", err)
	}
	read, err := New(f.directory, time.Second).Get(ctx, IDInput{ID: recovered.ID})
	if err != nil || read.Reminder.Title != in.Title || read.Reminder.SectionRef == nil || *read.Reminder.SectionRef != in.SectionID {
		t.Fatal("recovered creation lost native section or title")
	}
	root := f.records[in.ListID]["fields"].(map[string]any)
	order := root["ReminderIDs"].(map[string]any)["value"].(string)
	if strings.Count(order, strings.TrimPrefix(recovered.ID, "Reminder/")) != 1 {
		t.Fatal("reserved creation is missing or duplicated in manual order")
	}
	in.SectionID = "ListSection/one"
	if _, err := New(f.directory, time.Second).Create(ctx, in); err == nil || PublicError(err).Code != "idempotency_conflict" || f.writes != 1 {
		t.Fatal("section was excluded from the request payload hash")
	}
}
