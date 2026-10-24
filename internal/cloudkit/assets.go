package cloudkit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const MaxStructureBytes = 2 << 20

// Asset URLs are capabilities. Never log them or attach the account cookie jar.
func (c *Client) assetRequest(method, rawURL string, data []byte) ([]byte, error) {
	u, err := url.Parse(rawURL)
	base, _ := url.Parse(c.ckBase)
	if err != nil || u.User != nil || u.Fragment != "" || u.Host == "" {
		return nil, fmt.Errorf("invalid asset location")
	}
	trusted := u.Scheme == "https" && (strings.HasSuffix(u.Hostname(), ".icloud-content.com") || strings.HasSuffix(u.Hostname(), ".icloud.com") || strings.HasSuffix(u.Hostname(), ".apple-cloudkit.com"))
	if !trusted && !(u.Scheme == base.Scheme && u.Host == base.Host) {
		return nil, fmt.Errorf("untrusted asset location")
	}
	request, err := http.NewRequestWithContext(c.ctx, method, rawURL, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("asset request failed")
	}
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/octet-stream")
	}
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		if c.ctx.Err() != nil {
			return nil, c.ctx.Err()
		}
		return nil, fmt.Errorf("asset request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("asset request rejected")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxStructureBytes+1))
	if err != nil && c.ctx.Err() != nil {
		return nil, c.ctx.Err()
	}
	if err != nil || len(body) > MaxStructureBytes {
		return nil, fmt.Errorf("asset response exceeds bounds or is incomplete")
	}
	return body, nil
}

func (c *Client) ReadStructureAsset(asset map[string]interface{}) ([]byte, error) {
	location, _ := asset["downloadURL"].(string)
	if location == "" {
		return nil, fmt.Errorf("structure asset unavailable")
	}
	return c.assetRequest(http.MethodGet, location, nil)
}

// UploadStructureAsset uploads immutable metadata; a subsequent atomic
// records/modify attaches its receipt. This method never retries uploads/writes.
func (c *Client) UploadStructureAsset(owner, recordID, recordType, field string, data []byte) (map[string]interface{}, error) {
	if len(data) > MaxStructureBytes {
		return nil, fmt.Errorf("structure exceeds bounds")
	}
	zone := ZoneID{ZoneName: Zone, OwnerRecordName: owner}
	if c.scope.ZoneName != "" {
		zone = ZoneID{ZoneName: c.scope.ZoneName, OwnerRecordName: c.scope.OwnerRecordName}
	}
	response, err := c.post(c.databasePath("assets/upload"), map[string]interface{}{
		"zoneID": zone, "tokens": []map[string]interface{}{{"recordName": recordID, "recordType": recordType, "fieldName": field}},
	})
	if err != nil {
		return nil, err
	}
	tokens, _ := response["tokens"].([]interface{})
	if len(tokens) != 1 {
		return nil, fmt.Errorf("missing asset upload token")
	}
	token, _ := tokens[0].(map[string]interface{})
	location, _ := token["url"].(string)
	if token["recordName"] != recordID || token["fieldName"] != field || location == "" {
		return nil, fmt.Errorf("invalid asset upload token")
	}
	body, err := c.assetRequest(http.MethodPost, location, data)
	if err != nil {
		return nil, err
	}
	var uploaded struct {
		SingleFile map[string]interface{} `json:"singleFile"`
	}
	if json.Unmarshal(body, &uploaded) != nil || uploaded.SingleFile == nil || uploaded.SingleFile["receipt"] == nil {
		return nil, fmt.Errorf("unconfirmed asset upload")
	}
	return uploaded.SingleFile, nil
}
