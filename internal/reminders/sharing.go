package reminders

import (
	"context"
	"fmt"
	"sort"
	"strings"

	syncengine "icloud-reminders/internal/sync"
	"icloud-reminders/internal/writer"
	"icloud-reminders/pkg/models"
)

func stringValue(value map[string]any, key string) string {
	output, _ := value[key].(string)
	return output
}

func participants(engine *syncengine.Engine, listID string) (ParticipantsResult, string, bool, error) {
	output := ParticipantsResult{ListID: listID, Participants: []*models.ReminderParticipant{}}
	if err := exactList(engine, listID); err != nil {
		return output, "", false, err
	}
	scope, ok := engine.Cache.Scopes[listID]
	if !ok {
		return output, "", false, fmt.Errorf("list has no record scope")
	}
	client := engine.CK.ForScope(scope)
	root, err := client.LookupRecords(scope.OwnerRecordName, []string{listID})
	if err != nil {
		return output, "", false, err
	}
	share, ok := root[0]["share"].(map[string]any)
	if !ok || stringValue(share, "recordName") == "" {
		return output, "", false, nil
	}
	metadata, err := client.LookupRecords(scope.OwnerRecordName, []string{stringValue(share, "recordName")})
	if err != nil {
		return output, "", false, err
	}
	if stringValue(metadata[0], "recordType") != "cloudkit.share" {
		return output, "", false, fmt.Errorf("invalid share record")
	}
	values, ok := metadata[0]["participants"].([]any)
	if !ok {
		return output, "", false, fmt.Errorf("share participants unavailable")
	}
	current, _ := metadata[0]["currentUserParticipant"].(map[string]any)
	actor := stringValue(current, "participantId")
	canWrite := false
	seen := make(map[string]bool)
	for _, value := range values {
		person, ok := value.(map[string]any)
		if !ok {
			return output, "", false, fmt.Errorf("invalid share participant")
		}
		if stringValue(person, "acceptanceStatus") != "ACCEPTED" {
			continue
		}
		id := stringValue(person, "participantId")
		if id == "" || seen[id] {
			return output, "", false, fmt.Errorf("invalid participant identity")
		}
		seen[id] = true
		identity, _ := person["userIdentity"].(map[string]any)
		name, _ := identity["nameComponents"].(map[string]any)
		lookup, _ := identity["lookupInfo"].(map[string]any)
		role, permission := stringValue(person, "type"), stringValue(person, "permission")
		item := &models.ReminderParticipant{ID: id, Name: strings.TrimSpace(stringValue(name, "givenName") + " " + stringValue(name, "familyName")),
			Email: stringValue(lookup, "emailAddress"), Phone: stringValue(lookup, "phoneNumber"), Role: role, Permission: permission, IsCurrentUser: id == actor}
		output.Participants = append(output.Participants, item)
		if item.IsCurrentUser && (permission == "READ_WRITE" || role == "OWNER") {
			canWrite = true
		}
	}
	output.Shared = true
	sort.Slice(output.Participants, func(i, j int) bool { return output.Participants[i].ID < output.Participants[j].ID })
	return output, actor, canWrite, nil
}

func (s *Service) Participants(ctx context.Context, input ParticipantsInput) (output ParticipantsResult, err error) {
	if strings.TrimSpace(input.ListID) == "" {
		return output, invalid("list_id is required")
	}
	err = s.run(ctx, false, func(engine *syncengine.Engine, _ *writer.Writer) error {
		var lookupErr error
		output, _, _, lookupErr = participants(engine, input.ListID)
		return lookupErr
	})
	return
}

func (s *Service) Assign(ctx context.Context, input AssignInput) (output MutationResult, err error) {
	if strings.TrimSpace(input.ID) == "" || (input.ParticipantID == "" && !input.Clear) || (input.ParticipantID != "" && input.Clear) || (input.ParticipantID != "" && strings.TrimSpace(input.ParticipantID) == "") {
		return output, invalid("id and exactly one of participant_id or clear=true are required")
	}
	err = s.run(ctx, false, func(engine *syncengine.Engine, writer *writer.Writer) error {
		if err := exactReminder(engine, input.ID); err != nil {
			return err
		}
		reminder := engine.Cache.Reminders[input.ID]
		if reminder.ListRef == nil {
			return &Error{"not_shared", "Only reminders in a shared list can be assigned."}
		}
		people, actor, canWrite, err := participants(engine, *reminder.ListRef)
		if err != nil {
			return err
		}
		if !people.Shared {
			return &Error{"not_shared", "Only reminders in a shared list can be assigned."}
		}
		if !canWrite {
			return &Error{"permission_denied", "The current participant cannot modify this shared list."}
		}
		if engine.Cache.Scopes[input.ID].Key() != engine.Cache.Scopes[*reminder.ListRef].Key() {
			return fmt.Errorf("reminder belongs to another zone")
		}
		if !input.Clear {
			valid := false
			for _, person := range people.Participants {
				if person.ID == input.ParticipantID && (person.Permission == "READ_WRITE" || person.Role == "OWNER") {
					valid = true
				}
			}
			if !valid {
				return invalid("participant_id must identify an accepted collaborator of this reminder's list")
			}
			if len(reminder.AssignmentIDs) == 1 {
				assignment := engine.Cache.Assignments[reminder.AssignmentIDs[0]]
				if assignment != nil && assignment.ReminderID == input.ID && assignment.Status == 1 && assignment.AssigneeID == input.ParticipantID && engine.Cache.Scopes[reminder.AssignmentIDs[0]].Key() == engine.Cache.Scopes[input.ID].Key() {
					output = MutationResult{ID: input.ID, Status: "unchanged"}
					return nil
				}
			}
		} else if len(reminder.AssignmentIDs) == 0 {
			output = MutationResult{ID: input.ID, Status: "unchanged"}
			return nil
		}
		if err := writer.Assign(input.ID, input.ParticipantID, actor, input.Clear); err != nil {
			return err
		}
		status := "assigned"
		if input.Clear {
			status = "unassigned"
		}
		output = MutationResult{ID: input.ID, Status: status}
		return nil
	})
	return
}
