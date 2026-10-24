# Standalone Reminders MCP server

The CLI binary can expose Reminders tools using the official MCP Go SDK.
Python, a gateway and a reverse proxy are not required. HTTP is stateless and
supports current clients as well as the 2025 Streamable HTTP protocol. Both
`/mcp` and `/mcp/` work without redirects.

## Local setup

```bash
bash scripts/build.sh
./scripts/reminders auth
./scripts/reminders serve
```

The default endpoint is `http://127.0.0.1:8081/mcp`. For a client that launches
an MCP process, use `./scripts/reminders serve --transport stdio`. Server logs
go to stderr, keeping stdout reserved for MCP messages.

Use `--data-dir /path/to/private-data` on **both** `auth` and `serve` to choose
another account directory. `ICLOUD_REMINDERS_DATA_DIR` provides the same default
for all commands. The directory holds `session.json`, `ck_cache.json`, and
optionally a `credentials` file. Session and cache writes replace private files
atomically. Each CLI invocation holds an exclusive directory lock: stop a
running server before authentication, session import, or another CLI operation
on that directory. Use a separate directory for each account and one server
per directory. The server also resets cached records if the CloudKit account
owner changes.

The server only reuses or refreshes an existing web session. It never reads
Apple passwords, prompts on stdin, or initiates password/2FA authentication.
Missing or expired sessions produce an MCP tool error beginning with
`auth_required`. Stop the server, run `reminders auth` with the same directory,
then start it again. This is the iCloud web-login flow; CalDAV/IMAP
app-specific-password authentication is separate. Keep Apple passwords and
session files out of MCP arguments, Git, Docker build contexts, and server logs.

When 2FA is required, `auth` explicitly requests a verification code before
prompting for it. Unlock a trusted Apple device and approve the sign-in
notification. Enter the six-digit code in the terminal only. A failed delivery
request reports its HTTP status and stops instead of waiting for an unsent code;
requests are not retried automatically. If the notification does not appear
after a successful request, a trusted Mac can generate a code under System
Settings > your name > Sign-In & Security > Two-Factor Authentication > Get a
Verification Code. See [Apple's instructions](https://support.apple.com/guide/mac-help/mchl8bd4e9c2/mac).
After updating the CLI, cancel an older waiting login and start `auth` again.

`icloud_access_denied` means Apple explicitly blocked access to the private
Reminders database; it is separate from an expired login. Check that iCloud web
data access is enabled: on iOS 26.4 and later, Settings > your name > iCloud >
iCloud.com > Allow Data Access; older versions have an Access iCloud Data on the
Web toggle under iCloud. Advanced Data Protection can require temporary device
approval in addition to 2FA. Successful access in a separate browser does not
authorize the CLI's session. Use the explicit approval command below.
See [Apple's web-access documentation](https://support.apple.com/102630).

## Advanced Data Protection and device approval

Keep Advanced Data Protection enabled and allow iCloud web data access on your
trusted device. Then request approval for this CLI session:

```bash
./scripts/reminders auth --approve-web-access --approval-timeout 3m
# Or, with the persisted Docker account volume:
docker compose stop reminders
docker compose run --rm reminders auth --approve-web-access --approval-timeout 3m
docker compose --profile test run --rm --build smoke
docker compose up -d reminders
```

The command reuses the saved login, including after an `icloud_access_denied`
error. A missing/expired login still requires normal Apple Account login and
2FA; an app-specific password cannot substitute for that login. With a valid
login, approval does not require entering the account password again.

Unlock an online trusted Apple device and approve the web-access notification.
Apple can send a second notification for access to Reminders. The CLI checks
web-access state, requests device consent once when needed, polls the state,
and requests only the Reminders PCS keys. Polling does not resend consent
notifications or mark every key request as a fresh user action. Ctrl-C cancels
the wait; `--approval-timeout` accepts 1 second through 15 minutes (default 3
minutes). Disabled web access, unsupported devices, expired login, unexpected
responses and unsuccessful approval produce bounded, private errors.

Approval is saved only after a real Reminders database read succeeds. Session
cookies retain their domain, path, secure flag, host-only scope and expiration
across restarts; expired or deleted cookies are not revived. Older session
files remain readable. Ordinary MCP requests and the read-only smoke test
never initiate device approval or send device notifications.

Apple's approval is temporary. When it expires, the server returns
`icloud_access_denied`; stop it and rerun the approval command with the same
account directory. This uses Apple's private iCloud web approval endpoints,
which Apple can change. It is not a permanently unattended ADP integration.

## Docker

```bash
docker compose build
# Interactive one-time login and 2FA; credentials are not baked into the image.
docker compose run --rm reminders auth
docker compose up -d
docker compose ps
```

The container runs as a non-root user. Its session and delta cache persist in
the `reminders-data` volume. Compose publishes only `127.0.0.1:8081`; services
on the same private Docker network can use `http://reminders:8080/mcp`.
`/healthz` and the Docker health check report process liveness, not whether
iCloud authentication is valid. Tools can be discovered before authentication.

To renew authentication:

```bash
docker compose stop reminders
docker compose run --rm reminders auth
docker compose up -d reminders
```

Keep the write-capable endpoint on loopback or a private network. Optional
`REMINDERS_MCP_TOKEN` requires `Authorization: Bearer <token>` on `/mcp`;
tokens are not accepted as URL parameters. Set it in the server environment
or an untracked Compose `.env` file. A fixed bearer token is a private-service
access control, not a public OAuth deployment. `/healthz` remains unauthenticated
and contains no account information. Requests with an `Origin` header are
rejected unless explicitly allowed by `--allow-origin https://trusted.example`.

## Tools

| Tool | Behavior |
| --- | --- |
| `list_reminder_lists` | Read existing lists and their exact IDs |
| `list_reminders` | Native manual order; filter by list, parent, section, title and completion; `view=tree` nests the current page |
| `get_reminder` | Read one reminder, including notes and list/parent references |
| `list_reminder_participants` | Read accepted collaborators, their exact participant IDs, available display/contact details, permissions and `is_current_user`; private lists return `shared=false` |
| `assign_reminder` | Assign/reassign with `id` and `participant_id`, or remove the assignment with `id` and `clear=true` |
| `create_reminder` | Create in an existing `list_id`, optionally with a `parent_id`, due date, notes and priority |
| `update_reminder` | Update specified nonempty fields; `priority=none` clears priority |
| `complete_reminder` | Mark complete; an already completed reminder performs no write |
| `delete_reminder` | Permanently delete; requires `confirm=true` |
| `sync_reminders` | Refresh the local delta cache, or perform a full sync with `full=true` |

Use the exact IDs returned by the tools. MCP writes reject partial IDs and
ambiguous list names. Creation requires an existing list; list creation and
deletion are not exposed. Due dates use `YYYY-MM-DD` and priorities are `none`,
`low`, `medium`, or `high`. Clearing an existing due date or notes is not
supported by this initial adapter. Reads refresh the delta cache before
returning results; pagination is ordered by ID and is not a snapshot across
calls. The default page size is 100, with a maximum of 500.

### Shared lists and assignments

Both owned lists and incoming shared lists are synced. Each record retains its
CloudKit database, zone and owner; reads and writes to someone else's list go
to its shared zone. The first sync after upgrading rebuilds the old cache to
include assignment records. Revoked shared zones are removed on the next sync.

To assign a reminder, call `list_reminder_participants` with its `list_id`, then
use an exact accepted participant ID from that list in `assign_reminder`:

```json
{"id": "Reminder/EXACT-ID", "participant_id": "EXACT-PARTICIPANT-ID"}
```

Use `{"id": "Reminder/EXACT-ID", "clear": true}` to remove an assignment.
`participant_id` and `clear=true` are mutually exclusive. The current user and
the selected collaborator must have write access; assigning to yourself is
supported. Unaccepted invitees, foreign participant IDs and private lists are
rejected. Membership is freshly read before every assignment and is not cached
with contact details. `not_shared` and `permission_denied` are safe tool errors.

Reminder reads include `assignee_id` when assigned. Reassignment updates the
existing native assignment record; clearing soft-deletes it and empties the
reminder's assignment link. The assignment and reminder changes are one atomic
CloudKit batch, and local state is published only after all records succeed.
Repeating an unchanged assignment performs no write. These operations can
notify collaborators through iCloud, so obtain the user's approval before
changing a real reminder. The MCP tools do not invite people or edit shares.

All account operations are serialized. `--request-timeout` bounds the total
tool duration, including queue wait; its default is 3 minutes. iCloud HTTP
requests also have a 30-second per-request limit and observe cancellation.
Writes are never automatically retried. A timeout or failed response can leave
a write's outcome uncertain: inspect the current reminders before retrying,
especially before creating another reminder. Logs include tool names, call
IDs, outcomes, durations and counts, never arguments, reminder contents,
credentials, remote URLs or upstream exception text.

## Verification

```bash
go test -race ./...
go vet ./...
docker compose config --quiet
docker compose build
```

Tests use a simulated CloudKit API and exercise MCP discovery, legacy HTTP
compatibility, CRUD, exact IDs, deletion confirmation, account isolation,
serialization, cancellation, shared-zone routing, participant permissions,
assign/reassign/clear, failed-write cache handling and safe errors/logs.
They do not access a real Apple account. Live authentication and writes need
separate verification with a test account.

## Read-only live smoke test

After authenticating, test the real Go backend through its MCP HTTP interface:

```bash
docker compose run --rm reminders auth
docker compose --profile test run --rm --build smoke
```

The `smoke` service mounts the account volume read-only, copies only its session
into a private temporary directory, and performs a fresh full sync there. It
discovers the MCP tools, lists current lists and active reminders, and reads
one sampled reminder when available, and checks participant discovery for each
list. The original session and cache are not modified, and the test cannot call
create/update/complete/delete/assign tools. It logs
only aggregate counts; titles, IDs, notes, cookies and credentials stay private.
Allow several minutes for the first full sync. The source session must already
be valid: missing or expired authentication fails the explicitly enabled test.

An empty account is a valid result. To require at least one active reminder:

```bash
REMINDERS_LIVE_MIN_ACTIVE=1 docker compose --profile test run --rm --build smoke
```

For a local Go installation and session directory:

```bash
ICLOUD_REMINDERS_DATA_DIR=/path/to/private-data REMINDERS_LIVE_TEST=1 \
  go test -v ./internal/mcpserver -run '^TestLiveRemindersReadOnly$' -count=1 -timeout 10m
```

Normal `go test ./...` skips live testing unless `REMINDERS_LIVE_TEST=1` is set.
There is no interactive authentication inside the test. If the standalone
server is already running, stop it before the administrative `auth` command;
the smoke test itself uses a separate temporary cache and can run alongside it.


## Version 1.1.0: native sections and manual ordering

- `list_reminder_sections(list_id)` reads native section headings and exact IDs,
  in section order. `create_reminder_section(list_id, title)` creates a native
  `ListSection` and updates list ordering atomically.
- `create_reminder` accepts `section_id`; children inherit the parent's section.
- `list_reminders` accepts `section_id` and `view="tree"`. The compatible flat
  result remains; the tree nests only reminders on that page. Follow pagination
  and retain `parent_ref` when a parent is absent. `depth`, `section_ref`,
  `section_name` and `sort_index` explain hierarchy and native manual position.
- `move_reminder` changes parent/section or position within the same list.
  Use `parent_id` / `clear_parent`, `section_id` / `clear_section`, and at most
  one `before_id` or `after_id` (a target sibling). Without an anchor it appends
  to the target group. Cycles and foreign-list references are rejected.
- `reorder_reminders(list_id, reminder_ids, parent_id?, section_id?)` requires
  every sibling exactly once, including completed reminders. Each subtree
  stays together; other sibling groups are preserved.

Native JSON metadata assets use `minimumSupportedVersion=20230430`,
`orderedIdentifiers` for section IDs and `memberships` for section membership.
Membership timestamps use Apple's reference date (2001-01-01). Unknown JSON
properties and obsolete memberships are retained. The cache schema is now 3;
older account caches are fully rebuilt. Structure writes reread current list
metadata and shared-list permissions, retain native resolution tokens and use
one atomic record mutation in the original owner zone. Uploaded metadata is
bounded to 2 MiB; signed asset locations are never logged and never receive
account cookies. Unsupported future formats fail before record mutation.

The MCP instructions, descriptions and result legend explain `•` (pending),
`✓` (completed), `↳`/indentation (subtask), `!` (low/9), `!!` (medium/5),
`!!!` (high/1), `0` (no priority) and `≡` (manual drag handle, independent of
priority). Use IDs and typed fields; never insert display symbols into titles.
The Apple app's automatic sorting can show a different order from manual order.

Equivalent CLI commands:

```bash
reminders sections --list 'List/UUID'
reminders sections add --list 'List/UUID' --title 'Planning'
reminders move 'Reminder/UUID' --parent 'Reminder/PARENT_UUID'
reminders move 'Reminder/UUID' --clear-parent --section 'ListSection/UUID'
reminders move 'Reminder/UUID' --before 'Reminder/SIBLING_UUID'
reminders reorder --list 'List/UUID' --section 'ListSection/UUID' 'Reminder/FIRST' 'Reminder/SECOND'
reminders list --legend
```

CLI `list` shows native section/manual order and nested children. `--legend`
explains its symbols. The live smoke test must remain read-only: validate
section discovery and tree reads, never create/move/reorder real account data.

## Mutation confirmation and keyed creation

Confirmed mutations return `write_status=succeeded`. Tool failures retain native
`isError=true` with safe `error_code`, `operation`, `request_id`, `write_status`
(`not_sent`, `failed`, `succeeded` or `unknown`), `retry_class` and available
upstream status/code. Unknown results require inspection; no mutation is
automatically retried. Complete confirmation requires every exact record ID,
including list metadata records written alongside native structure changes.

`create_reminder` accepts an optional UUID `client_request_id`. The account-scoped
ledger reserves the reminder identity before dispatch and hashes all creation
fields, including section. Identical replays return the original ID with
`status=already_created`; changed arguments return `idempotency_conflict`.
A pending replay looks up the reserved record in its original owner zone. Keep
the ledger with the data volume and preserve the key and original arguments.
