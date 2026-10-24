package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// ErrWebAccessDisabled distinguishes a data-access restriction from expired
// authentication. Repeating password/2FA authentication cannot grant access.
var ErrWebAccessDisabled = errors.New("iCloud Reminders data access is blocked; enable iCloud web data access, then run reminders auth --approve-web-access")

// IsWebAccessDisabled recognizes only Apple's explicit private-database denial.
// Other 403 responses can still represent expired or invalid authentication.
func IsWebAccessDisabled(status int, body []byte) bool {
	if status != http.StatusForbidden {
		return false
	}
	var response struct {
		Code   string `json:"serverErrorCode"`
		Reason string `json:"reason"`
	}
	if json.Unmarshal(body, &response) != nil {
		return false
	}
	return response.Code == "ACCESS_DENIED" && strings.EqualFold(strings.TrimSpace(response.Reason), "private db access disabled for this account")
}
