package reminders

import (
	"errors"
	"slices"

	"icloud-reminders/internal/cloudkit"
	syncengine "icloud-reminders/internal/sync"
)

// The modify response confirms a commit, not that Apple's stored order matches
// the requested effect. Verify by a fresh lookup in the original owner zone.
func verifyStoredOrder(e *syncengine.Engine, listID string, requested []string) error {
	unavailable := func(cause error) error {
		failure := &DiagnosticError{
			Public:      &Error{"write_verification_failed", "iCloud accepted the write but its saved manual order could not be verified. Inspect current data before retrying."},
			WriteStatus: "succeeded", RetryClass: "retryable_after_read", OrderVerification: "unavailable", Cause: cause,
		}
		var api *cloudkit.APIError
		if errors.As(cause, &api) {
			failure.HTTPStatus = api.StatusCode
		}
		return failure
	}
	scope := e.Cache.Scopes[listID]
	client := e.CK.ForScope(scope)
	records, err := client.LookupRecords(scope.OwnerRecordName, []string{listID})
	if err != nil {
		return unavailable(err)
	}
	stored, err := client.ReadListStructure(records[0])
	if err != nil {
		return unavailable(err)
	}
	if err := e.ResolveListStructureIDs(listID, stored); err != nil {
		return unavailable(err)
	}
	// Historical IDs/tombstones may be retained or removed by Apple. Compare
	// every current reminder, including completed ones, instead of stale entries.
	current := map[string]bool{}
	for id, record := range e.Cache.Reminders {
		if record.ListRef != nil && *record.ListRef == listID && e.Cache.Scopes[id].Key() == scope.Key() {
			current[cloudkit.NativeID("Reminder", id)] = true
		}
	}
	project := func(order []string) []string {
		out := []string{}
		for _, id := range order {
			key := cloudkit.NativeID("Reminder", id)
			if current[key] {
				out = append(out, key)
			}
		}
		return out
	}
	expected := project(requested)
	if len(expected) != len(current) || !slices.Equal(expected, project(stored.ReminderIDs)) {
		return &DiagnosticError{
			Public:      &Error{"upstream_mismatch", "iCloud accepted the write but its saved manual order differs from the requested order. Inspect current data before retrying."},
			WriteStatus: "succeeded", RetryClass: "retryable_after_read", OrderVerification: "mismatch",
		}
	}
	return nil
}
