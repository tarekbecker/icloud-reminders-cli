package mcpserver

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"icloud-reminders/internal/reminders"
)

func TestMCPHTTPDiscoveryAndErrors(t *testing.T) {
	for _, path := range []string{"/mcp", "/mcp/"} {
		t.Run(path, func(t *testing.T) {
			var logs bytes.Buffer
			server := New(reminders.New(t.TempDir(), time.Second), "test", slog.New(slog.NewJSONHandler(&logs, nil)))
			httpServer := httptest.NewServer(HTTPHandler(server, HTTPOptions{}))
			defer httpServer.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
			session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: httpServer.URL + path}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			tools, err := session.ListTools(ctx, nil)
			if err != nil || len(tools.Tools) != 14 {
				t.Fatalf("tools: %+v, %v", tools, err)
			}
			for _, tool := range tools.Tools {
				if tool.InputSchema == nil || tool.OutputSchema == nil {
					t.Fatalf("missing schema for %s", tool.Name)
				}
				if tool.Name == "delete_reminder" && (tool.Annotations == nil || tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint) {
					t.Fatal("deletion annotation is missing")
				}
				if tool.Name == "list_reminder_participants" && (tool.Annotations == nil || !tool.Annotations.ReadOnlyHint) {
					t.Fatal("participant discovery must be read-only")
				}
				if tool.Name == "assign_reminder" && (tool.Annotations == nil || tool.Annotations.ReadOnlyHint || tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint) {
					t.Fatal("assignment mutation annotations are missing")
				}
			}
			result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_reminder_lists", Arguments: map[string]any{}})
			if err != nil || !result.IsError {
				t.Fatalf("auth result: %+v, %v", result, err)
			}
			details, ok := result.StructuredContent.(map[string]any)
			if !ok || details["error_code"] != "auth_required" || details["request_id"] == nil {
				t.Fatal("structured error evidence was lost")
			}
			if !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "auth_required") {
				t.Fatal("missing auth status")
			}
			result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "delete_reminder", Arguments: map[string]any{"id": "private-reminder-id", "confirm": false}})
			if err != nil || !result.IsError || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "confirm=true") {
				t.Fatalf("delete result: %+v, %v", result, err)
			}
			result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "assign_reminder", Arguments: map[string]any{"id": "private-reminder-id", "participant_id": "private-person-id", "clear": true}})
			if err != nil || !result.IsError || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "invalid_argument") {
				t.Fatal("assignment intent was not rejected before authentication")
			}
			if strings.Contains(logs.String(), "private-reminder-id") || strings.Contains(logs.String(), "private-person-id") || !strings.Contains(logs.String(), "mcp_tool_complete") {
				t.Fatalf("unsafe or missing logs: %s", logs.String())
			}
		})
	}
}

type failingBackend struct{ reminders.Backend }

func (failingBackend) ListLists(context.Context) (reminders.ListsResult, error) {
	return reminders.ListsResult{}, errors.New("private-title password=private-credential https://private-host/")
}

func TestUnexpectedErrorsAreSanitized(t *testing.T) {
	var logs bytes.Buffer
	server := New(failingBackend{}, "test", slog.New(slog.NewJSONHandler(&logs, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	left, right := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, left, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	clientSession, err := client.Connect(ctx, right, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()
	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "list_reminder_lists", Arguments: map[string]any{}})
	if err != nil || !result.IsError {
		t.Fatalf("result: %+v, %v", result, err)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	if strings.Contains(text, "private-") || strings.Contains(logs.String(), "private-") {
		t.Fatal("upstream details leaked")
	}
}

func TestHTTPAccessControlsAndLegacyProtocol(t *testing.T) {
	server := New(reminders.New(t.TempDir(), time.Second), "test", nil)
	handler := HTTPHandler(server, HTTPOptions{Token: "private-token", AllowedOrigins: []string{"https://trusted.example"}})
	for _, tc := range []struct {
		path, token, origin string
		status              int
	}{
		{"/healthz", "", "", 200},
		{"/unknown", "", "", 404},
		{"/mcp", "", "", 401},
		{"/mcp", "wrong", "", 401},
		{"/mcp", "private-token", "https://untrusted.example", 403},
		{"/mcp", "private-token", "null", 403},
	} {
		request := httptest.NewRequest(http.MethodGet, tc.path, nil)
		if tc.token != "" {
			request.Header.Set("Authorization", "Bearer "+tc.token)
		}
		if tc.origin != "" {
			request.Header.Set("Origin", tc.origin)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != tc.status {
			t.Fatalf("%s: got %d, want %d", tc.path, response.Code, tc.status)
		}
	}
	// Existing Python/frontends may still use the 2025 session-era protocol.
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"legacy","version":"1"}}}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Authorization", "Bearer private-token")
	request.Header.Set("Origin", "https://trusted.example")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "2025-11-25") {
		t.Fatalf("legacy handshake: %d %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Mcp-Session-Id") != "" {
		t.Fatal("stateless HTTP allocated a session")
	}
	request = httptest.NewRequest(http.MethodPost, "/mcp", io.NopCloser(strings.NewReader(strings.Repeat("x", 129<<10))))
	request.Header.Set("Authorization", "Bearer private-token")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code < 400 {
		t.Fatal("oversized request accepted")
	}
}
