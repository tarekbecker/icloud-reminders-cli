package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"icloud-reminders/internal/cache"
	"icloud-reminders/internal/logger"
	"icloud-reminders/internal/reminders"
)

// TestLiveRemindersReadOnly exercises the real CloudKit backend through MCP.
// Explicit opt-in is required. Only the session is copied; a fresh, temporary
// cache proves that successful reads came from iCloud rather than old records.
func TestLiveRemindersReadOnly(t *testing.T) {
	if os.Getenv("REMINDERS_LIVE_TEST") != "1" {
		t.Skip("set REMINDERS_LIVE_TEST=1 to enable read-only iCloud smoke testing")
	}
	minimum := 0
	if value := os.Getenv("REMINDERS_LIVE_MIN_ACTIVE"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 0 {
			t.Fatal("REMINDERS_LIVE_MIN_ACTIVE must be a nonnegative integer")
		}
		minimum = parsed
	}
	directory := os.Getenv("ICLOUD_REMINDERS_DATA_DIR")
	if directory == "" {
		directory = cache.ConfigDir
	}
	file, err := os.Open(filepath.Join(directory, "session.json"))
	if errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing Reminders session; first run reminders auth with the same data directory (Docker: docker compose run --rm reminders auth)")
	}
	if err != nil {
		t.Fatalf("cannot open Reminders session (%T)", err)
	}
	const maximumSessionBytes = 2 << 20
	session, err := io.ReadAll(io.LimitReader(file, maximumSessionBytes+1))
	_ = file.Close()
	if err != nil {
		t.Fatalf("cannot read Reminders session (%T)", err)
	}
	if len(session) > maximumSessionBytes {
		t.Fatal("Reminders session exceeds the smoke test size limit")
	}
	temporary := t.TempDir()
	if err := os.WriteFile(filepath.Join(temporary, "session.json"), session, 0600); err != nil {
		t.Fatalf("cannot prepare private test session (%T)", err)
	}
	// Do not copy the account cache or credentials, and do not emit CLI logs
	// that can contain upstream response bodies or reminder contents.
	previousLevel := logger.Level()
	logger.SetLevel(-1)
	t.Cleanup(func() { logger.SetLevel(previousLevel) })
	server := New(reminders.New(temporary, 4*time.Minute), "live-smoke", nil)
	httpServer := httptest.NewServer(HTTPHandler(server, HTTPOptions{}))
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "reminders-live-smoke", Version: "1"}, nil)
	connection, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: httpServer.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatalf("MCP connection failed (%T)", err)
	}
	defer connection.Close()
	tools, err := connection.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("MCP discovery failed (%T)", err)
	}
	for _, required := range []string{"sync_reminders", "list_reminder_lists", "list_reminders", "get_reminder", "list_reminder_participants"} {
		found := false
		for _, tool := range tools.Tools {
			if tool.Name == required {
				found = true
				if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
					t.Fatal("smoke test tool is missing its read-only annotation")
				}
				break
			}
		}
		if !found {
			t.Fatal("required read-only MCP tool is missing")
		}
	}
	refreshed := liveRead[reminders.SyncResult](t, ctx, connection, "sync_reminders", map[string]any{"full": true})
	lists := liveRead[reminders.ListsResult](t, ctx, connection, "list_reminder_lists", map[string]any{})
	active := liveRead[reminders.Page](t, ctx, connection, "list_reminders", map[string]any{"include_completed": false, "limit": 5})
	if lists.Lists == nil || active.Reminders == nil || active.Total < len(active.Reminders) {
		t.Fatal("invalid MCP list result")
	}
	if active.Total < minimum {
		t.Fatalf("found %d active reminders; expected at least %d", active.Total, minimum)
	}
	if active.Total > 0 && len(active.Reminders) == 0 {
		t.Fatal("nonempty account returned no sample reminders")
	}
	seen := map[string]bool{}
	for _, item := range active.Reminders {
		if item == nil || item.ID == "" || strings.TrimSpace(item.Title) == "" || item.Completed || seen[item.ID] {
			t.Fatal("invalid or duplicate active reminder in sample")
		}
		seen[item.ID] = true
	}
	if len(active.Reminders) > 0 {
		item := liveRead[reminders.ReminderResult](t, ctx, connection, "get_reminder", map[string]any{"id": active.Reminders[0].ID})
		if item.Reminder == nil || item.Reminder.ID != active.Reminders[0].ID {
			t.Fatal("individual reminder lookup did not match the sampled ID")
		}
	}
	sharedLists, participantCount := 0, 0
	for _, list := range lists.Lists {
		if list == nil || list.ID == "" {
			t.Fatal("invalid list identity")
		}
		people := liveRead[reminders.ParticipantsResult](t, ctx, connection, "list_reminder_participants", map[string]any{"list_id": list.ID})
		if people.ListID != list.ID || people.Participants == nil || (!people.Shared && len(people.Participants) != 0) {
			t.Fatal("invalid participant result")
		}
		identities := make(map[string]bool)
		for _, person := range people.Participants {
			if person == nil || person.ID == "" || identities[person.ID] {
				t.Fatal("invalid or duplicate participant identity")
			}
			identities[person.ID] = true
		}
		if people.Shared {
			sharedLists++
			participantCount += len(people.Participants)
		}
	}
	t.Logf("live read verified: lists=%d shared_lists=%d participants=%d active_reminders=%d sampled=%d synced_reminders=%d", len(lists.Lists), sharedLists, participantCount, active.Total, len(active.Reminders), refreshed.Reminders)
}

func liveRead[T any](t *testing.T, ctx context.Context, connection *mcp.ClientSession, name string, arguments map[string]any) T {
	t.Helper()
	// This allowlist prevents accidental cloud writes if the smoke test grows.
	switch name {
	case "sync_reminders", "list_reminder_lists", "list_reminders", "get_reminder", "list_reminder_participants":
	default:
		t.Fatal("live smoke test only permits read-only tools")
	}
	result, err := connection.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("%s: MCP call failed (%T)", name, err)
	}
	if result.IsError {
		code := "tool_failed"
		for _, content := range result.Content {
			if text, ok := content.(*mcp.TextContent); ok {
				candidate, _, _ := strings.Cut(text.Text, ":")
				switch candidate {
				case "auth_required", "icloud_access_denied", "request_timeout", "icloud_request_failed", "not_found", "invalid_argument":
					code = candidate
				}
			}
		}
		t.Fatalf("%s failed: %s (no account details logged)", name, code)
	}
	if result.StructuredContent == nil {
		t.Fatalf("%s returned no structured content", name)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("%s returned invalid structured content (%T)", name, err)
	}
	var decoded T
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("%s returned an unexpected result shape (%T)", name, err)
	}
	return decoded
}
