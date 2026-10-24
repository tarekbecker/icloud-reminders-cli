package cloudkit

import (
	"encoding/json"
	"errors"
	"icloud-reminders/internal/auth"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLookupRecognizesExplicitCloudKitMissingCodes(t *testing.T) {
	for _, code := range []string{"NOT_FOUND", "UNKNOWN_ITEM", "ACCESS_DENIED", "INTERNAL_ERROR"} {
		t.Run(code, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"records": []any{map[string]any{"recordName": "Reminder/test", "serverErrorCode": code, "reason": "private upstream details"}}})
			}))
			defer server.Close()
			client, _ := NewFromSession(&auth.SessionData{CKBaseURL: server.URL})
			_, err := client.LookupRecords("owner", []string{"Reminder/test"})
			var missing *RecordNotFoundError
			if err == nil || errors.As(err, &missing) != (code == "NOT_FOUND" || code == "UNKNOWN_ITEM") {
				t.Fatal("missing-item classification differs from explicit server code")
			}
		})
	}
}
