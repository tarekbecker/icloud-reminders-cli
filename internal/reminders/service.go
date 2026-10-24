// Package reminders adapts the CLI's CloudKit implementation for server use.
package reminders

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"icloud-reminders/internal/auth"
	"icloud-reminders/internal/cache"
	"icloud-reminders/internal/cloudkit"
	syncengine "icloud-reminders/internal/sync"
	"icloud-reminders/internal/writer"
	"icloud-reminders/pkg/models"
)

// Error exposes only stable, deliberately public error messages.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func invalid(message string) error { return &Error{"invalid_argument", message} }

// PublicError prevents upstream response bodies, URLs and credentials from
// escaping through MCP errors or logs.
func PublicError(err error) *Error {
	var diagnostic *DiagnosticError
	if errors.As(err, &diagnostic) {
		return diagnostic.Public
	}
	var write *cloudkit.WriteFailure
	if errors.As(err, &write) {
		if write.Status == "not_sent" {
			return &Error{"icloud_request_failed", "Reminder metadata could not be refreshed. No write was attempted."}
		}
		if write.Status == "succeeded" {
			return &Error{"local_state_failed", "The write succeeded but local state could not be saved. Inspect current data before retrying."}
		}
		if write.Status == "failed" {
			return &Error{"icloud_write_failed", "iCloud rejected the write."}
		}
		return &Error{"write_result_unknown", "iCloud did not confirm the write. Inspect the reminder before retrying."}
	}
	var public *Error
	if errors.As(err, &public) {
		return public
	}
	var structure *cloudkit.StructureError
	if errors.As(err, &structure) {
		if structure.Issue.RecordType == "Reminder" && (structure.Issue.Field == "TitleDocument" || structure.Issue.Field == "NotesDocument") {
			return &Error{"unsupported_text_document", "This reminder's text document cannot be updated safely. No write was attempted."}
		}
		if structure.Issue.Reason == "metadata_unavailable" {
			return &Error{"icloud_request_failed", "List metadata could not be read. No structure write was attempted."}
		}
		return &Error{"unsupported_structure", "This list uses an unsupported structure format. No structure write was attempted."}
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return &Error{"request_timeout", "Request ended before completion. A write may already have succeeded; inspect the reminder before retrying."}
	}
	if errors.Is(err, auth.ErrAuthRequired) {
		return &Error{"auth_required", "Run reminders auth with the server's data directory, then retry."}
	}
	var api *cloudkit.APIError
	if errors.Is(err, auth.ErrWebAccessDisabled) || (errors.As(err, &api) && auth.IsWebAccessDisabled(api.StatusCode, []byte(api.Body))) {
		return &Error{"icloud_access_denied", "iCloud blocked access to Reminders data. Enable iCloud web data access, then run reminders auth --approve-web-access with the server's data directory and approve on a trusted device."}
	}
	if errors.As(err, &api) && (api.StatusCode == 401 || api.StatusCode == 403) {
		return &Error{"auth_required", "Run reminders auth with the server's data directory, then retry."}
	}
	return &Error{"icloud_request_failed", "iCloud request failed. A write may already have succeeded; inspect the reminder before retrying."}
}

type ListInput struct {
	ListID           string `json:"list_id,omitempty" jsonschema:"Exact list ID returned by list_reminder_lists"`
	ParentID         string `json:"parent_id,omitempty" jsonschema:"Exact parent reminder ID; return only its direct subtasks"`
	Query            string `json:"query,omitempty" jsonschema:"Case-insensitive substring of the reminder title"`
	IncludeCompleted bool   `json:"include_completed,omitempty" jsonschema:"Also return completed reminders"`
	Limit            int    `json:"limit,omitempty" jsonschema:"Page size: 1 to 500; default 100"`
	Offset           int    `json:"offset,omitempty" jsonschema:"Number of matching reminders to skip; default 0"`
	SectionID        string `json:"section_id,omitempty" jsonschema:"Exact native section ID; includes its subtasks"`
	View             string `json:"view,omitempty" jsonschema:"flat (default) or tree; tree nests only reminders returned on this page"`
}

type IDInput struct {
	ID string `json:"id" jsonschema:"Exact reminder ID returned by list_reminders or create_reminder"`
}

type CreateInput struct {
	Title           string `json:"title" jsonschema:"Reminder title"`
	ListID          string `json:"list_id" jsonschema:"Exact list ID returned by list_reminder_lists"`
	Due             string `json:"due,omitempty" jsonschema:"Due date in YYYY-MM-DD format"`
	Priority        string `json:"priority,omitempty" jsonschema:"none, low, medium or high; default none"`
	Notes           string `json:"notes,omitempty" jsonschema:"Optional notes"`
	ParentID        string `json:"parent_id,omitempty" jsonschema:"Exact parent reminder ID in the same list"`
	SectionID       string `json:"section_id,omitempty" jsonschema:"Exact section ID in the same list; subtasks inherit their parent's section"`
	ClientRequestID string `json:"client_request_id,omitempty" jsonschema:"Optional UUID idempotency key; reuse with identical arguments to recover an uncertain creation without duplicates"`
}

type UpdateInput struct {
	ID       string `json:"id" jsonschema:"Exact reminder ID"`
	Title    string `json:"title,omitempty" jsonschema:"New nonempty title; omitted fields remain unchanged"`
	Due      string `json:"due,omitempty" jsonschema:"New due date in YYYY-MM-DD format; clearing a date is unsupported"`
	Priority string `json:"priority,omitempty" jsonschema:"none, low, medium or high"`
	Notes    string `json:"notes,omitempty" jsonschema:"New nonempty notes; clearing notes is unsupported"`
}

type DeleteInput struct {
	ID      string `json:"id" jsonschema:"Exact reminder ID"`
	Confirm bool   `json:"confirm" jsonschema:"Must be true to permanently delete this reminder"`
}

type SyncInput struct {
	Full bool `json:"full,omitempty" jsonschema:"Discard the local delta token and perform a full sync"`
}

type ParticipantsInput struct {
	ListID string `json:"list_id" jsonschema:"Exact shared-list ID returned by list_reminder_lists"`
}

type AssignInput struct {
	ID            string `json:"id" jsonschema:"Exact reminder ID"`
	ParticipantID string `json:"participant_id,omitempty" jsonschema:"Exact accepted participant ID returned by list_reminder_participants; omit when clearing"`
	Clear         bool   `json:"clear,omitempty" jsonschema:"Explicitly remove the assignment; mutually exclusive with participant_id"`
}

type ParticipantsResult struct {
	ListID       string                        `json:"list_id"`
	Shared       bool                          `json:"shared"`
	Participants []*models.ReminderParticipant `json:"participants"`
}

type ListsResult struct {
	Lists             []*models.ReminderList   `json:"lists"`
	StructureWarnings []*models.StructureIssue `json:"structure_warnings,omitempty"`
}
type ReminderResult struct {
	Reminder          *models.Reminder         `json:"reminder"`
	StructureWarnings []*models.StructureIssue `json:"structure_warnings,omitempty"`
}
type MutationResult struct {
	WriteStatus       string `json:"write_status,omitempty"`
	ID                string `json:"id"`
	Status            string `json:"status"`
	OrderVerification string `json:"order_verification,omitempty"`
}
type SyncResult struct {
	Reminders         int                      `json:"reminders"`
	Lists             int                      `json:"lists"`
	StructureWarnings []*models.StructureIssue `json:"structure_warnings,omitempty"`
}
type Page struct {
	Reminders         []*models.Reminder       `json:"reminders"`
	Total             int                      `json:"total"`
	NextOffset        *int                     `json:"next_offset,omitempty"`
	Tree              []*models.ReminderNode   `json:"tree,omitempty"`
	Legend            map[string]string        `json:"legend"`
	StructureWarnings []*models.StructureIssue `json:"structure_warnings,omitempty"`
}

// Backend is the narrow contract consumed by the MCP layer.
type Backend interface {
	ListLists(context.Context) (ListsResult, error)
	List(context.Context, ListInput) (Page, error)
	Get(context.Context, IDInput) (ReminderResult, error)
	Create(context.Context, CreateInput) (MutationResult, error)
	Update(context.Context, UpdateInput) (MutationResult, error)
	Complete(context.Context, IDInput) (MutationResult, error)
	Delete(context.Context, DeleteInput) (MutationResult, error)
	Sync(context.Context, SyncInput) (SyncResult, error)
	Participants(context.Context, ParticipantsInput) (ParticipantsResult, error)
	Assign(context.Context, AssignInput) (MutationResult, error)
	Sections(context.Context, SectionsInput) (SectionsResult, error)
	CreateSection(context.Context, CreateSectionInput) (MutationResult, error)
	Move(context.Context, MoveInput) (MutationResult, error)
	Reorder(context.Context, ReorderInput) (MutationResult, error)
}

// Service owns one account. All sync/read/write operations are serialized;
// queued calls can still be canceled while waiting for the account.
type Service struct {
	directory string
	timeout   time.Duration
	gate      chan struct{}
}

func New(directory string, timeout time.Duration) *Service {
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	return &Service{directory: directory, timeout: timeout, gate: gate}
}

func (s *Service) run(ctx context.Context, full bool, operation func(*syncengine.Engine, *writer.Writer) error) error {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	select {
	case <-ctx.Done():
		return beforeWrite(ctx.Err())
	case <-s.gate:
	}
	defer func() { s.gate <- struct{}{} }()
	if err := ctx.Err(); err != nil {
		return beforeWrite(err)
	}
	sessionFile := filepath.Join(s.directory, "session.json")
	session, err := auth.NewNonInteractive(ctx).EnsureSession(sessionFile, false)
	if err != nil {
		if ctx.Err() != nil {
			return beforeWrite(ctx.Err())
		}
		return beforeWrite(err)
	}
	client, err := cloudkit.NewFromSession(session)
	if err != nil {
		return beforeWrite(err)
	}
	engine := syncengine.NewWithCache(client.WithContext(ctx), sessionFile, cache.LoadFrom(s.directory))
	engine.NonInteractive = true
	// A directory accidentally reused after authenticating another account
	// must never reuse the previous account's records or delta token.
	ownerID, err := engine.CK.GetOwnerID()
	if err != nil {
		return beforeWrite(err)
	}
	if engine.Cache.OwnerID != nil && *engine.Cache.OwnerID != ownerID {
		engine.Cache = engine.Cache.Reset()
	}
	engine.Cache.OwnerID = &ownerID
	if err := engine.Sync(full); err != nil {
		return beforeWrite(err)
	}
	if err := operation(engine, writer.New(engine.CK, engine)); err != nil {
		return operationFailure(err)
	}
	return nil
}

func (s *Service) ListLists(ctx context.Context) (out ListsResult, err error) {
	out.Lists = []*models.ReminderList{}
	err = s.run(ctx, false, func(engine *syncengine.Engine, _ *writer.Writer) error {
		out.Lists = append(out.Lists, engine.GetLists()...)
		out.StructureWarnings = engine.StructureWarnings("")
		sort.Slice(out.Lists, func(i, j int) bool { return out.Lists[i].ID < out.Lists[j].ID })
		return nil
	})
	return
}

func (s *Service) List(ctx context.Context, in ListInput) (out Page, err error) {
	if in.View != "" && in.View != "flat" && in.View != "tree" {
		return out, invalid("view must be flat or tree")
	}
	if in.Limit == 0 {
		in.Limit = 100
	}
	if in.Limit < 1 || in.Limit > 500 || in.Offset < 0 {
		return out, invalid("limit must be 1..500 and offset must be nonnegative")
	}
	out.Reminders = []*models.Reminder{}
	out.Legend = StructureLegend()
	err = s.run(ctx, false, func(engine *syncengine.Engine, _ *writer.Writer) error {
		in.ParentID = resolveReminderID(engine, in.ParentID)
		if in.ListID != "" {
			if err := exactList(engine, in.ListID); err != nil {
				return err
			}
		}
		if in.ParentID != "" {
			if err := exactReminder(engine, in.ParentID); err != nil {
				return err
			}
		}
		if in.SectionID != "" {
			if err := exactSection(engine, in.SectionID, in.ListID); err != nil {
				return err
			}
			if issue := engine.Cache.StructureIssues[engine.Cache.Sections[in.SectionID].ListID]; issue != nil {
				return &cloudkit.StructureError{Issue: *issue}
			}
		}
		matches := []*models.Reminder{}
		out.StructureWarnings = engine.StructureWarnings(in.ListID)
		for _, item := range engine.GetReminders(in.IncludeCompleted) {
			if in.ListID != "" && (item.ListRef == nil || *item.ListRef != in.ListID) {
				continue
			}
			if in.ParentID != "" && (item.ParentRef == nil || *item.ParentRef != in.ParentID) {
				continue
			}
			if in.Query != "" && !strings.Contains(strings.ToLower(item.Title), strings.ToLower(in.Query)) {
				continue
			}
			if in.SectionID != "" && (item.SectionRef == nil || *item.SectionRef != in.SectionID) {
				continue
			}
			matches = append(matches, item)
		}
		matches = OrderReminders(engine, matches)
		out.Total = len(matches)
		if in.Offset >= len(matches) {
			return nil
		}
		end := min(in.Offset+in.Limit, len(matches))
		out.Reminders = matches[in.Offset:end]
		if in.View == "tree" {
			out.Tree = reminderTree(out.Reminders)
		}
		if end < len(matches) {
			out.NextOffset = &end
		}
		return nil
	})
	return
}

func (s *Service) Get(ctx context.Context, in IDInput) (out ReminderResult, err error) {
	if strings.TrimSpace(in.ID) == "" {
		return out, invalid("id is required")
	}
	err = s.run(ctx, false, func(engine *syncengine.Engine, _ *writer.Writer) error {
		in.ID = resolveReminderID(engine, in.ID)
		if err := exactReminder(engine, in.ID); err != nil {
			return err
		}
		for _, item := range engine.GetReminders(true) {
			if item.ID == in.ID {
				out.Reminder = item
				out.StructureWarnings = engine.StructureWarnings(item.ListRefValue())
				break
			}
		}
		return nil
	})
	return
}

func validateFields(title, due, priority string) error {
	if title != "" && (strings.TrimSpace(title) == "" || len(title) > 4096) {
		return invalid("title must contain text and be at most 4096 bytes")
	}
	if due != "" {
		if _, err := time.Parse("2006-01-02", due); err != nil {
			return invalid("due must be a valid YYYY-MM-DD date")
		}
	}
	if priority != "" {
		if _, ok := models.PriorityMap[priority]; !ok {
			return invalid("priority must be none, low, medium or high")
		}
	}
	return nil
}

func (s *Service) Create(ctx context.Context, in CreateInput) (out MutationResult, err error) {
	if strings.TrimSpace(in.Title) == "" || in.ListID == "" {
		return out, invalid("title and list_id are required")
	}
	if err := validateFields(in.Title, in.Due, in.Priority); err != nil {
		return out, err
	}
	if in.Priority == "" {
		in.Priority = "none"
	}
	err = s.run(ctx, false, func(engine *syncengine.Engine, w *writer.Writer) error {
		in.ParentID = resolveReminderID(engine, in.ParentID)
		if err := exactList(engine, in.ListID); err != nil {
			return err
		}
		if in.ParentID != "" {
			if err := exactReminder(engine, in.ParentID); err != nil {
				return err
			}
			parent := engine.Cache.Reminders[in.ParentID]
			if parent.ListRef == nil || *parent.ListRef != in.ListID {
				return invalid("parent_id must belong to list_id")
			}
		}
		reservation, err := s.reserveCreate(engine, in)
		if err != nil {
			return err
		}
		if reservation != nil {
			if reservation.Done {
				out = MutationResult{ID: reservation.ID, Status: "already_created"}
				return nil
			}
			w.CreateRecordID = reservation.ID
		}
		var createErr error
		out, createErr = createOrganized(engine, w, in)
		if createErr != nil {
			return createErr
		}
		if reservation != nil {
			reservation.Done = true
			if err := reservation.save(); err != nil {
				return &DiagnosticError{Public: &Error{"local_state_failed", "The creation succeeded but local request state could not be saved. Retry only with the same client_request_id."}, WriteStatus: "succeeded", RetryClass: "retryable_after_read"}
			}
		}
		return nil
	})
	return
}

func (s *Service) Update(ctx context.Context, in UpdateInput) (out MutationResult, err error) {
	if in.ID == "" {
		return out, invalid("id is required")
	}
	if in.Title == "" && in.Due == "" && in.Priority == "" && in.Notes == "" {
		return out, invalid("provide at least one nonempty field to update")
	}
	if err := validateFields(in.Title, in.Due, in.Priority); err != nil {
		return out, err
	}
	err = s.run(ctx, false, func(engine *syncengine.Engine, w *writer.Writer) error {
		in.ID = resolveReminderID(engine, in.ID)
		if err := exactReminder(engine, in.ID); err != nil {
			return err
		}
		result, err := w.EditReminder(in.ID, in.Title, in.Due, in.Notes, in.Priority)
		if err := writeError(result, err); err != nil {
			return err
		}
		out = MutationResult{ID: in.ID, Status: "updated"}
		return nil
	})
	return
}

func (s *Service) Complete(ctx context.Context, in IDInput) (out MutationResult, err error) {
	if in.ID == "" {
		return out, invalid("id is required")
	}
	err = s.run(ctx, false, func(engine *syncengine.Engine, w *writer.Writer) error {
		in.ID = resolveReminderID(engine, in.ID)
		if err := exactReminder(engine, in.ID); err != nil {
			return err
		}
		if !engine.Cache.Reminders[in.ID].Completed {
			result, err := w.CompleteReminder(in.ID)
			if err := writeError(result, err); err != nil {
				return err
			}
		}
		out = MutationResult{ID: in.ID, Status: "completed"}
		return nil
	})
	return
}

func (s *Service) Delete(ctx context.Context, in DeleteInput) (out MutationResult, err error) {
	if !in.Confirm {
		return out, invalid("confirm=true is required to delete a reminder")
	}
	if in.ID == "" {
		return out, invalid("id is required")
	}
	err = s.run(ctx, false, func(engine *syncengine.Engine, w *writer.Writer) error {
		in.ID = resolveReminderID(engine, in.ID)
		if err := exactReminder(engine, in.ID); err != nil {
			return err
		}
		result, err := w.DeleteReminder(in.ID)
		if err := writeError(result, err); err != nil {
			return err
		}
		out = MutationResult{ID: in.ID, Status: "deleted"}
		return nil
	})
	return
}

func (s *Service) Sync(ctx context.Context, in SyncInput) (out SyncResult, err error) {
	err = s.run(ctx, in.Full, func(engine *syncengine.Engine, _ *writer.Writer) error {
		out = SyncResult{Reminders: len(engine.Cache.Reminders), Lists: len(engine.Cache.Lists)}
		out.StructureWarnings = engine.StructureWarnings("")
		return nil
	})
	return
}

func exactList(engine *syncengine.Engine, id string) error {
	if _, ok := engine.Cache.Lists[id]; !ok {
		return &Error{"not_found", "List ID not found. Use list_reminder_lists to obtain an exact ID."}
	}
	return nil
}

// Resolve a bare logical-ID alias only when its native record actually exists.
// Existing malformed bare records keep their identity for read-only recovery;
// never merge two CloudKit records or invent a successful lookup.
func resolveReminderID(engine *syncengine.Engine, id string) string {
	if id == "" || engine.Cache.Reminders[id] != nil {
		return id
	}
	native := cloudkit.NativeID("Reminder", id)
	if engine.Cache.Reminders[native] != nil {
		return native
	}
	return id
}

func exactReminder(engine *syncengine.Engine, id string) error {
	if _, ok := engine.Cache.Reminders[id]; !ok {
		return &Error{"not_found", "Reminder ID not found. Use list_reminders to obtain an exact ID."}
	}
	return nil
}

func writeError(result map[string]interface{}, err error) error {
	if err != nil {
		return err
	}
	if result == nil {
		return &Error{"write_result_unknown", "The write result is unknown. Inspect the reminder before retrying."}
	}
	if _, failed := result["error"]; failed {
		return &Error{"icloud_write_failed", "iCloud did not confirm the write. Inspect the reminder before retrying."}
	}
	return nil
}
