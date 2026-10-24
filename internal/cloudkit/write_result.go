package cloudkit

import (
	"errors"
	"fmt"
)

// WriteFailure describes only evidence about the mutation, never inferred success.
// Failed means a complete response rejected all records; missing/partial responses
// and transport failures remain unknown. No layer automatically repeats a write.
type WriteFailure struct {
	Status       string
	HTTPStatus   int
	UpstreamCode string
	Cause        error
}

func (e *WriteFailure) Error() string { return "CloudKit write " + e.Status }
func (e *WriteFailure) Unwrap() error { return e.Cause }

func unknownWrite(err error) *WriteFailure {
	failure := &WriteFailure{Status: "unknown", Cause: err}
	var api *APIError
	if errors.As(err, &api) {
		failure.HTTPStatus = api.StatusCode
	}
	return failure
}

func safeServerCode(code string) string {
	switch code {
	case "BAD_REQUEST", "AUTHENTICATION_REQUIRED", "ACCESS_DENIED", "NOT_FOUND", "UNKNOWN_ITEM", "CONFLICT", "SERVER_RECORD_CHANGED", "ZONE_NOT_FOUND", "QUOTA_EXCEEDED", "LIMIT_EXCEEDED", "THROTTLED", "SERVICE_UNAVAILABLE", "INTERNAL_ERROR", "BATCH_REQUEST_FAILED", "INVALID_FIELD_TYPE", "ATOMIC_FAILURE", "VALIDATING_REFERENCE_ERROR":
		return code
	}
	return ""
}

func confirmWrite(result map[string]interface{}, operations []map[string]interface{}) error {
	records, ok := result["records"].([]interface{})
	if !ok || len(records) != len(operations) {
		return unknownWrite(fmt.Errorf("incomplete write response"))
	}
	expected := make(map[string]string, len(operations))
	for _, operation := range operations {
		record, _ := operation["record"].(map[string]interface{})
		id, _ := record["recordName"].(string)
		kind, _ := operation["operationType"].(string)
		if id == "" || expected[id] != "" {
			return unknownWrite(fmt.Errorf("invalid operation identity"))
		}
		expected[id] = kind
	}
	failures, successes, upstream := 0, 0, ""
	for _, value := range records {
		record, ok := value.(map[string]interface{})
		if !ok {
			return unknownWrite(fmt.Errorf("invalid record response"))
		}
		id, _ := record["recordName"].(string)
		kind := expected[id]
		if kind == "" {
			return unknownWrite(fmt.Errorf("unexpected record confirmation"))
		}
		delete(expected, id)
		if code, _ := record["serverErrorCode"].(string); code != "" {
			failures++
			if upstream == "" || upstream == "ATOMIC_FAILURE" {
				upstream = safeServerCode(code)
			}
			continue
		}
		if kind != "delete" {
			tag, _ := record["recordChangeTag"].(string)
			if tag == "" {
				return unknownWrite(fmt.Errorf("missing record confirmation"))
			}
		}
		successes++
	}
	if failures > 0 {
		status := "unknown"
		if successes == 0 {
			status = "failed"
		}
		return &WriteFailure{Status: status, HTTPStatus: 200, UpstreamCode: upstream}
	}
	if len(expected) != 0 {
		return unknownWrite(fmt.Errorf("missing record confirmation"))
	}
	return nil
}
