package writer

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAllCreationPathsProduceNativeNamesAndFieldClocks(t *testing.T) {
	for _, build := range []func(string, string, string, string, int, string) (map[string]interface{}, string, error){buildCreateOp, CreateReminderOperation} {
		op, id, err := build("🧪 Native", "List/test", "Reminder/parent", "2026-10-15", 1, "🥰 Notes")
		if err != nil || !strings.HasPrefix(id, "Reminder/") {
			t.Fatal("creation produced a non-native record name")
		}
		record := op["record"].(map[string]interface{})
		fields := record["fields"].(map[string]interface{})
		if record["recordName"] != id {
			t.Fatal("creation result identity differs from the operation")
		}
		list := fields["List"].(map[string]interface{})["value"].(map[string]interface{})
		if list["action"] != "VALIDATE" {
			t.Fatal("native list membership does not validate its reference")
		}
		parent := fields["ParentReminder"].(map[string]interface{})["value"].(map[string]interface{})
		if parent["action"] != "VALIDATE" {
			t.Fatal("native parent reference does not validate its target")
		}
		var tokens struct {
			Map map[string]map[string]any `json:"map"`
		}
		_ = json.Unmarshal([]byte(fields["ResolutionTokenMap"].(map[string]interface{})["value"].(string)), &tokens)
		for _, key := range []string{"titleDocument", "notesDocument", "completed", "creationDate", "lastModifiedDate", "list", "parentReminder", "dueDate", "priority"} {
			if tokens.Map[key]["counter"] != float64(1) || tokens.Map[key]["replicaID"] == "" {
				t.Fatal("native creation field clock is missing")
			}
		}
	}
}
