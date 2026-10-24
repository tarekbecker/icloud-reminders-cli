package cloudkit

import (
	"fmt"
	"icloud-reminders/pkg/models"
)

// RecordNotFoundError is returned only for an explicit missing-item response.
type RecordNotFoundError struct{}

func (*RecordNotFoundError) Error() string { return "record not found" }

func (c *Client) ForScope(scope models.RecordScope) *Client {
	copy := *c
	copy.scope = scope
	return &copy
}

func (c *Client) databasePath(operation string) string {
	database := c.scope.Database
	if database == "" {
		database = "private"
	}
	return "database/1/" + Container + "/production/" + database + "/" + operation
}

func (c *Client) SharedZones() ([]models.RecordScope, error) {
	result, err := c.ForScope(models.RecordScope{Database: "shared"}).post("database/1/"+Container+"/production/shared/zones/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	zones, ok := result["zones"].([]any)
	if !ok {
		return nil, fmt.Errorf("invalid shared zones response")
	}
	var scopes []models.RecordScope
	for _, value := range zones {
		zone, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid shared zone")
		}
		if code, _ := zone["serverErrorCode"].(string); code != "" {
			return nil, fmt.Errorf("shared zone unavailable")
		}
		id, _ := zone["zoneID"].(map[string]any)
		name, _ := id["zoneName"].(string)
		owner, _ := id["ownerRecordName"].(string)
		if name == Zone {
			if owner == "" {
				return nil, fmt.Errorf("shared zone has no owner")
			}
			scopes = append(scopes, models.RecordScope{Database: "shared", ZoneName: name, OwnerRecordName: owner})
		}
	}
	return scopes, nil
}

// LookupRecords fetches complete sharing metadata without caching identities.
func (c *Client) LookupRecords(ownerID string, names []string) ([]map[string]any, error) {
	if len(names) == 0 || len(names) > 200 {
		return nil, fmt.Errorf("invalid lookup size")
	}
	zone := ZoneID{ZoneName: Zone, OwnerRecordName: ownerID}
	if c.scope.ZoneName != "" {
		zone = ZoneID{ZoneName: c.scope.ZoneName, OwnerRecordName: c.scope.OwnerRecordName}
	}
	records := make([]map[string]any, 0, len(names))
	expected := make(map[string]bool)
	for _, name := range names {
		if name == "" || expected[name] {
			return nil, fmt.Errorf("invalid record lookup")
		}
		expected[name] = true
		records = append(records, map[string]any{"recordName": name})
	}
	result, err := c.post(c.databasePath("records/lookup"), map[string]any{"zoneID": zone, "records": records})
	if err != nil {
		return nil, err
	}
	values, ok := result["records"].([]any)
	if !ok || len(values) != len(names) {
		return nil, fmt.Errorf("incomplete record lookup")
	}
	output := make([]map[string]any, 0, len(values))
	for _, value := range values {
		record, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid record lookup response")
		}
		name, _ := record["recordName"].(string)
		if !expected[name] {
			return nil, fmt.Errorf("unexpected record lookup response")
		}
		delete(expected, name)
		if code, _ := record["serverErrorCode"].(string); code != "" {
			if code == "UNKNOWN_ITEM" || code == "NOT_FOUND" {
				return nil, &RecordNotFoundError{}
			}
			return nil, fmt.Errorf("record lookup unavailable")
		}
		if record["deleted"] == true {
			return nil, &RecordNotFoundError{}
		}
		if returned, ok := record["zoneID"].(map[string]any); ok {
			if returned["zoneName"] != zone.ZoneName || returned["ownerRecordName"] != zone.OwnerRecordName {
				return nil, fmt.Errorf("record lookup returned wrong zone")
			}
		}
		output = append(output, record)
	}
	return output, nil
}
