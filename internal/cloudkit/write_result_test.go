package cloudkit

import (
	"errors"
	"testing"
)

func TestWriteConfirmationRequiresEveryExactRecord(t *testing.T) {
	operations := []map[string]interface{}{
		{"operationType": "create", "record": map[string]interface{}{"recordName": "Reminder/new"}},
		{"operationType": "update", "record": map[string]interface{}{"recordName": "List/root"}},
	}
	rejected := func(id string) any { return map[string]interface{}{"recordName": id, "serverErrorCode": "CONFLICT"} }
	confirmed := func(id string) any { return map[string]interface{}{"recordName": id, "recordChangeTag": "new-tag"} }
	for _, test := range []struct {
		name    string
		records []any
		want    string
	}{
		{"all rejected", []any{rejected("Reminder/new"), rejected("List/root")}, "failed"},
		{"partial rejection", []any{rejected("Reminder/new")}, "unknown"},
		{"duplicate rejection", []any{rejected("Reminder/new"), rejected("Reminder/new")}, "unknown"},
		{"foreign rejection", []any{rejected("Reminder/new"), rejected("List/foreign")}, "unknown"},
		{"mixed outcomes", []any{confirmed("Reminder/new"), rejected("List/root")}, "unknown"},
		{"all confirmed", []any{confirmed("Reminder/new"), confirmed("List/root")}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := confirmWrite(map[string]interface{}{"records": test.records}, operations)
			if test.want == "" {
				if err != nil {
					t.Fatal("complete confirmation rejected")
				}
				return
			}
			var failure *WriteFailure
			if !errors.As(err, &failure) || failure.Status != test.want {
				t.Fatalf("wrong commit evidence: %v", err)
			}
		})
	}
}

func TestWriteRejectionKeepsSpecificReferenceCodeInsteadOfAtomicFailure(t *testing.T) {
	operations := []map[string]interface{}{
		{"operationType": "delete", "record": map[string]interface{}{"recordName": "Reminder/old"}},
		{"operationType": "update", "record": map[string]interface{}{"recordName": "List/root"}},
	}
	result := map[string]interface{}{"records": []interface{}{
		map[string]interface{}{"recordName": "List/root", "serverErrorCode": "ATOMIC_FAILURE"},
		map[string]interface{}{"recordName": "Reminder/old", "serverErrorCode": "VALIDATING_REFERENCE_ERROR", "reason": "private record data"},
	}}
	var failure *WriteFailure
	if err := confirmWrite(result, operations); !errors.As(err, &failure) || failure.Status != "failed" || failure.UpstreamCode != "VALIDATING_REFERENCE_ERROR" {
		t.Fatal("specific native rejection lost")
	}
}
