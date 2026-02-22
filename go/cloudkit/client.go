// Package cloudkit implements the CloudKit API client for iCloud Reminders.
package cloudkit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"icloud-reminders/auth"
)

// APIError represents a non-2xx HTTP error from the CloudKit API.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("API error %d: %s", e.StatusCode, e.Body)
}

// Is503 reports whether err is a 503 APIError.
func Is503(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == 503
}

const (
	Container = "com.apple.reminders"
	Zone      = "Reminders"
)

// Client manages an authenticated CloudKit HTTP session.
type Client struct {
	http   *http.Client
	ckBase string
}

// NewFromSession creates a CloudKit client from auth session data.
// The session must have a valid CKBaseURL and cookies.
func NewFromSession(sess *auth.SessionData) (*Client, error) {
	if sess.CKBaseURL == "" {
		return nil, fmt.Errorf("session has no ck_base_url")
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}

	// Build http.Cookie slice with unquoted values.
	// Apple cookies use RFC 2109 quoted-string values (e.g. "v=1:t=...").
	// Go's strict net/http parser rejects raw '"' — strip outer quotes here.
	var httpCookies []*http.Cookie
	for _, c := range sess.Cookies {
		exp := time.Time{}
		if c.Expires > 0 {
			exp = time.Unix(c.Expires, 0)
		}
		httpCookies = append(httpCookies, &http.Cookie{
			Name:    c.Name,
			Value:   unquoteCookie(c.Value),
			Domain:  c.Domain,
			Path:    c.Path,
			Expires: exp,
			Secure:  c.Secure,
		})
	}

	// Set cookies against all iCloud-related hosts so Go's jar forwards
	// them to any *.icloud.com subdomain (setup, ckdatabasews, etc.)
	setURLs := []string{
		"https://www.icloud.com",
		"https://setup.icloud.com",
		"https://idmsa.apple.com",
		"https://appleid.apple.com",
		"https://www.apple.com",
		sess.CKBaseURL,
	}
	for _, rawURL := range setURLs {
		if rawURL == "" {
			continue
		}
		u, err := url.Parse(rawURL)
		if err != nil {
			continue
		}
		jar.SetCookies(u, httpCookies)
	}

	ckURL, err := url.Parse(sess.CKBaseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid ck_base_url %q: %w", sess.CKBaseURL, err)
	}
	_ = ckURL

	// Ensure trailing slash on base
	base := sess.CKBaseURL
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}

	return &Client{
		http:   &http.Client{Jar: jar},
		ckBase: base,
	}, nil
}

// post makes a JSON POST request to the CloudKit API.
func (c *Client) post(path string, body interface{}) (map[string]interface{}, error) {
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	apiURL := c.ckBase + path
	log.Printf("[DEBUG] POST %s", apiURL)

	req, err := http.NewRequest("POST", apiURL, bytes.NewReader(bodyJSON))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", "https://www.icloud.com")
	req.Header.Set("Referer", "https://www.icloud.com/")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &APIError{StatusCode: resp.StatusCode, Body: truncate(string(respBody), 500)}
	}

	var result map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("JSON parse error: %w", err)
	}
	return result, nil
}

// GetOwnerID fetches the CloudKit owner record name for the Reminders zone.
func (c *Client) GetOwnerID() (string, error) {
	result, err := c.post("database/1/"+Container+"/production/private/zones/list", map[string]interface{}{})
	if err != nil {
		return "", fmt.Errorf("zones/list failed: %w", err)
	}

	zones, _ := result["zones"].([]interface{})
	for _, z := range zones {
		zone, _ := z.(map[string]interface{})
		zoneID, _ := zone["zoneID"].(map[string]interface{})
		if zoneID["zoneName"] == Zone {
			if owner, ok := zoneID["ownerRecordName"].(string); ok {
				return owner, nil
			}
		}
	}
	// Fallback: use first zone's owner
	if len(zones) > 0 {
		zone := zones[0].(map[string]interface{})
		zoneID, _ := zone["zoneID"].(map[string]interface{})
		if owner, ok := zoneID["ownerRecordName"].(string); ok {
			return owner, nil
		}
	}
	return "", fmt.Errorf("Reminders zone not found")
}

// ChangesZoneRequest is the payload for a zone changes request.
type ChangesZoneRequest struct {
	Zones []ZoneChangesSpec `json:"zones"`
}

// ZoneChangesSpec specifies a zone and optional sync token.
type ZoneChangesSpec struct {
	ZoneID      ZoneID   `json:"zoneID"`
	DesiredKeys []string `json:"desiredKeys,omitempty"`
	SyncToken   string   `json:"syncToken,omitempty"`
}

// ZoneID identifies a CloudKit zone.
type ZoneID struct {
	ZoneName        string `json:"zoneName"`
	OwnerRecordName string `json:"ownerRecordName"`
}

// ChangesZone fetches zone changes for delta or full sync.
func (c *Client) ChangesZone(ownerID string, syncToken string) (map[string]interface{}, error) {
	spec := ZoneChangesSpec{
		ZoneID:      ZoneID{ZoneName: Zone, OwnerRecordName: ownerID},
		DesiredKeys: []string{"TitleDocument", "NotesDocument", "Name", "Completed", "CompletionDate", "DueDate", "List", "Deleted", "Priority", "ParentReminder"},
	}
	if syncToken != "" {
		spec.SyncToken = syncToken
	}
	return c.post("database/1/"+Container+"/production/private/changes/zone",
		ChangesZoneRequest{Zones: []ZoneChangesSpec{spec}})
}

// ModifyRecords creates, updates, or deletes CloudKit records.
func (c *Client) ModifyRecords(ownerID string, operations []map[string]interface{}) (map[string]interface{}, error) {
	payload := map[string]interface{}{
		"zoneID": map[string]interface{}{
			"zoneName":        Zone,
			"ownerRecordName": ownerID,
		},
		"operations": operations,
		"atomic":     true,
	}
	result, err := c.post("database/1/"+Container+"/production/private/records/modify", payload)
	if err != nil {
		return map[string]interface{}{"error": err.Error()}, nil
	}
	return result, nil
}

// unquoteCookie strips RFC 2109 outer double-quotes from a cookie value.
func unquoteCookie(v string) string {
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		return v[1 : len(v)-1]
	}
	return v
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
