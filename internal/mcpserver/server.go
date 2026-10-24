// Package mcpserver exposes a standalone Reminders MCP server.
package mcpserver

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"icloud-reminders/internal/reminders"
)

func New(backend reminders.Backend, version string, logger *slog.Logger) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "icloud-reminders", Version: version}, &mcp.ServerOptions{
		Capabilities: &mcp.ServerCapabilities{},
		Instructions: "Use list_reminder_lists, list_reminder_sections and list_reminders to obtain exact IDs. Native sections are headings, not reminders. Use view=tree for nested subtasks; move_reminder changes parent/section and reorder_reminders changes manual sibling order. Priority: 0=none, 9=low (!), 5=medium (!!), 1=high (!!!). The drag handle ≡ changes manual order, not priority; •=pending, ✓=complete, ↳=subtask. Never insert display symbols into titles. Use list_reminder_participants for the reminder's list before assigning it to an accepted collaborator. Writes change iCloud data; obtain the user's approval before invoking them. Deletion requires confirm=true. Never blindly retry a failed creation or an uncertain write. Authentication is an administrative CLI operation; run reminders auth when auth_required is returned.",
	})
	read := annotations(true, false, true)
	create := annotations(false, false, false)
	update := annotations(false, true, true)
	complete := annotations(false, false, true)
	remove := annotations(false, true, false)
	register(server, "list_reminder_lists", "List iCloud Reminders lists and their exact IDs.", read, logger,
		func(ctx context.Context, _ struct{}) (reminders.ListsResult, error) { return backend.ListLists(ctx) })
	register(server, "list_reminders", "List reminders in native manual order with list, parent, section, title, completion and pagination filters. view=tree nests reminders on this page; parent_ref remains authoritative when a parent is filtered out. The legend explains status, priority and drag handles. Due dates use YYYY-MM-DD.", read, logger, backend.List)
	register(server, "get_reminder", "Read one reminder by its exact ID, including notes and parent/list references.", read, logger, backend.Get)
	register(server, "create_reminder", "Create a reminder in an existing list, optionally as a subtask. Do not automatically retry failed or timed-out creations.", create, logger, backend.Create)
	register(server, "update_reminder", "Update specified nonempty fields of a reminder. Set priority=none to clear priority; clearing notes or due dates is unsupported.", update, logger, backend.Update)
	register(server, "complete_reminder", "Mark a reminder complete. Already completed reminders are left unchanged.", complete, logger, backend.Complete)
	register(server, "delete_reminder", "Permanently delete a reminder. Requires explicit confirm=true.", remove, logger, backend.Delete)
	register(server, "sync_reminders", "Refresh the local Reminders cache. Full sync can take several minutes; this tool does not mutate iCloud.", read, logger, backend.Sync)
	register(server, "list_reminder_participants", "List accepted participants of one shared list, including exact participant IDs, display names and permissions. Private lists return shared=false and no participants.", read, logger, backend.Participants)
	register(server, "assign_reminder", "Assign a reminder to an accepted participant of its shared list using an exact participant_id from list_reminder_participants. Set clear=true without participant_id to remove the assignment. This changes iCloud data; inspect uncertain writes before retrying.", update, logger, backend.Assign)
	register(server, "list_reminder_sections", "List native Apple Reminders sections and exact IDs in section order for one list.", read, logger, backend.Sections)
	register(server, "create_reminder_section", "Create a native section heading in an existing list. Never automatically retry uncertain creations.", create, logger, backend.CreateSection)
	register(server, "move_reminder", "Arrange a reminder in its current list: parent_id indents, clear_parent makes top-level; section_id/clear_section changes native section. Subtasks inherit their parent's section. before_id/after_id anchors must be target siblings; without an anchor append. A subtree stays together; cycles are rejected. Inspect uncertain writes before retrying.", update, logger, backend.Move)
	register(server, "reorder_reminders", "Set manual sibling order by supplying every sibling ID exactly once, including completed reminders. Omit parent_id for top-level and section_id for the unsectioned group. Subtrees stay together; priority is unchanged. Inspect uncertain writes before retrying.", update, logger, backend.Reorder)
	return server
}

func annotations(read, destructive, idempotent bool) *mcp.ToolAnnotations {
	closed := false
	return &mcp.ToolAnnotations{ReadOnlyHint: read, DestructiveHint: &destructive, IdempotentHint: idempotent, OpenWorldHint: &closed}
}

func register[In, Out any](server *mcp.Server, name, description string, annotations *mcp.ToolAnnotations, logger *slog.Logger, operation func(context.Context, In) (Out, error)) {
	tool := &mcp.Tool{Name: name, Description: description, Annotations: annotations}
	if name == "list_reminders" {
		// The SDK's type inference rejects recursive Go types. Describe the
		// recursive tree explicitly instead of weakening the result to raw JSON.
		tool.OutputSchema = map[string]interface{}{
			"type": "object", "required": []string{"reminders", "total", "legend"},
			"properties": map[string]interface{}{
				"reminders": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "object"}},
				"total":     map[string]interface{}{"type": "integer"}, "next_offset": map[string]interface{}{"type": "integer"},
				"legend":             map[string]interface{}{"type": "object", "additionalProperties": map[string]interface{}{"type": "string"}},
				"tree":               map[string]interface{}{"type": "array", "items": map[string]interface{}{"$ref": "#/$defs/node"}},
				"structure_warnings": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "object"}},
			},
			"$defs": map[string]interface{}{"node": map[string]interface{}{
				"type": "object", "required": []string{"reminder", "subtasks"}, "properties": map[string]interface{}{
					"reminder": map[string]interface{}{"type": "object"},
					"subtasks": map[string]interface{}{"type": "array", "items": map[string]interface{}{"$ref": "#/$defs/node"}},
				},
			}},
		}
	} else {
		schema, err := jsonschema.For[Out](nil)
		if err != nil {
			panic("invalid tool output schema")
		}
		tool.OutputSchema = schema
	}
	mcp.AddTool(server, tool,
		func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
			started := time.Now()
			callID := uuid.NewString()
			if logger != nil {
				logger.Info("mcp_tool_start", "tool", name, "call_id", callID)
			}
			out, err := operation(ctx, in)
			outcome := "ok"
			var details map[string]any
			if err != nil {
				details = reminders.ErrorDetails(err, name, callID, !annotations.ReadOnlyHint)
				err = reminders.PublicError(err)
				outcome = err.(*reminders.Error).Code
			}
			if logger != nil {
				fields := []any{"tool", name, "call_id", callID, "outcome", outcome, "duration_ms", time.Since(started).Milliseconds()}
				if err == nil {
					fields = append(fields, "result_count", resultCount(out))
					fields = append(fields, "structure_warning_count", structureWarningCount(out))
				}
				if details != nil {
					for _, key := range []string{"write_status", "retry_class", "upstream_status", "upstream_error_code", "structure_field", "structure_reason", "structure_version", "order_verification"} {
						if value, ok := details[key]; ok {
							fields = append(fields, key, value)
						}
					}
				}
				logger.Info("mcp_tool_complete", fields...)
			}
			if err != nil {
				return &mcp.CallToolResult{IsError: true, StructuredContent: details,
					Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, nil, nil
			}
			if mutation, ok := any(out).(reminders.MutationResult); ok {
				mutation.WriteStatus = "succeeded"
				return nil, mutation, nil
			}
			return nil, out, nil
		})
}

func structureWarningCount(out any) int {
	switch v := out.(type) {
	case reminders.ListsResult:
		return len(v.StructureWarnings)
	case reminders.Page:
		return len(v.StructureWarnings)
	case reminders.ReminderResult:
		return len(v.StructureWarnings)
	case reminders.SectionsResult:
		return len(v.StructureWarnings)
	case reminders.SyncResult:
		return len(v.StructureWarnings)
	}
	return 0
}

func resultCount(out any) int {
	switch value := out.(type) {
	case reminders.ListsResult:
		return len(value.Lists)
	case reminders.Page:
		return len(value.Reminders)
	case reminders.SyncResult:
		return value.Reminders
	case reminders.SectionsResult:
		return len(value.Sections)
	case reminders.ParticipantsResult:
		return len(value.Participants)
	default:
		return 1
	}
}
