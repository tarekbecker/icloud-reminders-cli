# Changelog

## Unreleased

- Encode native text extents in UTF-16 code units, read gzip/zlib archives and
  preserve character IDs, replica clocks and deletion history during edits.
- Create native `Reminder/UUID` records from both CLI and MCP, retain logical
  UUID aliases and reject ordering writes to malformed legacy record names.
- Advance only changed reminder field resolution tokens, including completion;
  isolate unsupported text archives and distinguish explicit missing records.
- Decode titles and notes from their exact protobuf text fields; rebuild old
  decoded caches without rewriting remote content.
- Send numeric completion values accepted by iCloud.
- Confirm every record identity in mutation responses and return structured
  write/retry evidence through MCP without losing safe diagnostics.
- Add durable UUID-keyed creates, including native sections and manual order,
  and recover lost responses without creating a second reminder.

## 1.1.0 — 2026-10-01

- Create/discover native list sections and preserve their JSON metadata assets.
- Read reminders in manual section/hierarchy order, optionally as a tree.
- Move existing reminders under parents or into sections; reorder siblings
  without separating descendants.
- Add `sections`, `move`, `reorder` CLI commands and `list --legend`; document
  priority marks, status marks, indentation and manual drag handles for LLMs.
- Preserve original shared-owner zones and unknown metadata, check current
  permissions, reject cycles and foreign-list references, and rebuild old caches.
- Embed 1.1.0 in local/Docker builds and advertise all fourteen typed MCP tools.
