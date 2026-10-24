package reminders

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestUpdateAdvancesOnlyChangedNativeFieldClocks(t *testing.T) {
	f := newCloud(t)
	fields := f.records["Reminder/AA1"]["fields"].(map[string]any)
	fields["ResolutionTokenMap"] = map[string]any{"value": `{"map":{"titleDocument":{"counter":7,"replicaID":"existing-title-replica","modificationTime":123,"future":"preserve"},"priority":{"counter":3,"replicaID":"existing-priority-replica","modificationTime":123},"completed":{"counter":12,"replicaID":"existing-completion-replica","modificationTime":123}},"futureRoot":true}`}
	s := New(f.directory, time.Second)
	for _, want := range []float64{8, 9} {
		_, err := s.Update(context.Background(), UpdateInput{ID: "Reminder/AA1", Title: "🥰 changed", Priority: "high"})
		if err != nil {
			t.Fatal(err)
		}
		var tokens map[string]any
		_ = json.Unmarshal([]byte(fields["ResolutionTokenMap"].(map[string]any)["value"].(string)), &tokens)
		entries := tokens["map"].(map[string]any)
		title := entries["titleDocument"].(map[string]any)
		if title["counter"] != want || title["replicaID"] != "existing-title-replica" || title["future"] != "preserve" || tokens["futureRoot"] != true {
			t.Fatal("title field clock not advanced or extensions lost")
		}
		unchanged := entries["completed"].(map[string]any)
		if unchanged["counter"] != float64(12) || unchanged["modificationTime"] != float64(123) {
			t.Fatal("unrelated field clock changed")
		}
		if entries["priority"].(map[string]any)["counter"] != want-4 {
			t.Fatal("priority field clock not advanced")
		}
	}
	_, err := s.Complete(context.Background(), IDInput{ID: "AA1"})
	if err != nil {
		t.Fatal(err)
	}
	var tokens map[string]any
	_ = json.Unmarshal([]byte(fields["ResolutionTokenMap"].(map[string]any)["value"].(string)), &tokens)
	entries := tokens["map"].(map[string]any)
	if entries["completed"].(map[string]any)["counter"] != float64(13) || entries["titleDocument"].(map[string]any)["counter"] != float64(9) {
		t.Fatal("completion did not advance its own clock or altered the title clock")
	}
}

func TestUnsupportedTextArchiveBlocksOnlyItsUpdateBeforeWrite(t *testing.T) {
	f := newCloud(t)
	f.records["Reminder/AA1"]["fields"].(map[string]any)["TitleDocument"] = map[string]any{"value": "malformed-private-document"}
	s := New(f.directory, time.Second)
	_, err := s.Update(context.Background(), UpdateInput{ID: "AA1", Title: "new"})
	details := ErrorDetails(err, "update_reminder", "safe-request", true)
	if err == nil || f.writes != 0 || details["error_code"] != "unsupported_text_document" || details["write_status"] != "not_sent" || details["structure_field"] != "TitleDocument" {
		t.Fatal("unsupported text was overwritten or lost its diagnostic")
	}
	if _, err := s.ListLists(context.Background()); err != nil {
		t.Fatal("one text document blocked list discovery")
	}
	if _, err := s.List(context.Background(), ListInput{ListID: "List/2"}); err != nil {
		t.Fatal("one text document blocked another list")
	}
}
