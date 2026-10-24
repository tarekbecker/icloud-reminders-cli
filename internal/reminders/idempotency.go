package reminders

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"icloud-reminders/internal/cloudkit"
	"icloud-reminders/internal/storage"
	syncengine "icloud-reminders/internal/sync"
)

type createReservation struct {
	path      string
	ID        string `json:"id"`
	InputHash string `json:"input_hash"`
	Done      bool   `json:"done"`
}

func (r *createReservation) save() error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return storage.WritePrivate(r.path, data)
}

// Reserve before dispatch, under the service's account gate. The ledger never
// stores titles/notes or participant information. Pending retries inspect the
// exact reserved record in its original zone; every retry uses that same ID.
func (s *Service) reserveCreate(engine *syncengine.Engine, in CreateInput) (*createReservation, error) {
	if in.ClientRequestID == "" {
		return nil, nil
	}
	key, err := uuid.Parse(in.ClientRequestID)
	if err != nil {
		return nil, invalid("client_request_id must be a UUID")
	}
	in.ClientRequestID = key.String()
	if engine.Cache.OwnerID == nil {
		return nil, beforeWrite(errors.New("owner unavailable"))
	}
	accountKey := *engine.Cache.OwnerID + "\x00" + in.ClientRequestID
	identityHash := sha256.Sum256([]byte(accountKey))
	payload, err := json.Marshal(in)
	if err != nil {
		return nil, beforeWrite(err)
	}
	inputHash := sha256.Sum256(payload)
	reservation := &createReservation{
		path:      filepath.Join(s.directory, "create-requests", hex.EncodeToString(identityHash[:])+".json"),
		ID:        "Reminder/" + strings.ToUpper(uuid.NewSHA1(uuid.NameSpaceOID, []byte(accountKey)).String()),
		InputHash: hex.EncodeToString(inputHash[:]),
	}
	data, err := os.ReadFile(reservation.path)
	if errors.Is(err, os.ErrNotExist) {
		if err := reservation.save(); err != nil {
			return nil, beforeWrite(err)
		}
		return reservation, nil
	}
	if err != nil {
		return nil, beforeWrite(err)
	}
	var previous createReservation
	if err := json.Unmarshal(data, &previous); err != nil || previous.ID != reservation.ID {
		return nil, beforeWrite(errors.New("invalid request state"))
	}
	if previous.InputHash != reservation.InputHash {
		return nil, &DiagnosticError{Public: &Error{"idempotency_conflict", "client_request_id was already used with different arguments."}, WriteStatus: "not_sent", RetryClass: "not_retryable"}
	}
	if previous.Done {
		reservation.Done = true
		return reservation, nil
	}
	scope, ok := engine.Cache.Scopes[in.ListID]
	if !ok {
		return nil, beforeWrite(errors.New("list scope unavailable"))
	}
	records, err := engine.CK.ForScope(scope).LookupRecords(scope.OwnerRecordName, []string{reservation.ID})
	if err != nil {
		var absent *cloudkit.RecordNotFoundError
		if errors.As(err, &absent) {
			return reservation, nil
		}
		return nil, &DiagnosticError{Public: &Error{"write_result_unknown", "Could not establish the previous creation's state. Retry only with the same client_request_id."}, WriteStatus: "unknown", RetryClass: "retryable_after_read"}
	}
	if len(records) != 1 || stringValue(records[0], "recordType") != "Reminder" {
		return nil, &DiagnosticError{Public: &Error{"write_result_unknown", "Could not verify the reserved reminder."}, WriteStatus: "unknown", RetryClass: "retryable_after_read"}
	}
	reservation.Done = true
	if err := reservation.save(); err != nil {
		return nil, beforeWrite(err)
	}
	return reservation, nil
}
