---
name: icloud-reminders
description: Manage Apple iCloud Reminders via CloudKit API. Use for listing, adding, completing, deleting reminders, managing lists, and hierarchical subtasks. Works with 2FA-protected accounts via cached sessions.
---

# iCloud Reminders (Go)

Access and manage Apple iCloud Reminders via CloudKit API. Full CRUD with hierarchical subtask support.

**Pure Go — no Python or pyicloud required.** Authentication, 2FA, session management and CloudKit API calls are all implemented natively in Go.

## Setup

1. **Build the binary** (once):
   ```bash
   bash scripts/build.sh
   ```

2. **Create credentials file** (`~/.config/icloud-reminders/credentials`):
   ```bash
   export ICLOUD_USERNAME="your@apple.id"
   export ICLOUD_PASSWORD="your-password"
   ```

3. **Authenticate** (interactive — required on first run):
   ```bash
   scripts/reminders.sh auth
   ```
   Enter your 2FA code when prompted. Session is saved to
   `~/.config/icloud-reminders/session.json` and reused automatically.
   Re-authentication is only needed when the session expires.

## Commands

```bash
# First-time setup / force re-auth
scripts/reminders.sh auth
scripts/reminders.sh auth --force

# List all active reminders (hierarchical)
scripts/reminders.sh list

# Filter by list name
scripts/reminders.sh list -l "🛒 Einkauf"

# Include completed
scripts/reminders.sh list --all

# Search by title
scripts/reminders.sh search "milk"

# Show all lists
scripts/reminders.sh lists

# Add reminder
scripts/reminders.sh add "Buy milk" -l "Einkauf"

# Add with due date and priority
scripts/reminders.sh add "Call mom" --due 2026-02-25 --priority high

# Add with notes
scripts/reminders.sh add "Buy milk" -l "Einkauf" --notes "Get the organic 2% stuff"

# Add as subtask
scripts/reminders.sh add "Butter" --parent ABC123

# Add multiple at once (batch)
scripts/reminders.sh add-batch "Butter" "Käse" "Milch" -l "Einkauf"

# Complete reminder
scripts/reminders.sh complete abc123

# Delete reminder
scripts/reminders.sh delete abc123

# Export as JSON
scripts/reminders.sh json

# Force full resync
scripts/reminders.sh sync

# Export session cookies (share without password)
scripts/reminders.sh export-session session.tar.gz

# Import session from export
scripts/reminders.sh import-session session.tar.gz
```

## Session Management

The binary handles sessions automatically:

- **On each run:** tries `accountLogin` with saved cookies to get a fresh CloudKit URL
- **On failure / first run:** triggers full interactive signin + 2FA
- **Trust token:** saved after 2FA so subsequent logins don't require a code
- **Session file:** `~/.config/icloud-reminders/session.json`

## Output Format

```
✅ Reminders: 101 (101 active)

📋 Shopping (12)
  • Supermarket  (ABC123DE)
    • Butter  (FGH456IJ)
    • Cheese  (KLM789NO)
  • Drugstore  (PQR012ST)
    • Baking paper  (UVW345XY)
```

IDs (8-char) in parentheses — use for `complete`, `delete`, `--parent`.

## Cache & Sync

- **Cache:** `~/.config/icloud-reminders/ck_cache.json` (same JSON format as Python version — shared/compatible)
- **Delta sync:** Fast incremental updates (default)
- **Full sync:** `scripts/reminders.sh sync` — can take ~2 min for large accounts

## Architecture

```
scripts/
├── reminders.sh            # Entry point wrapper
├── build.sh                # Build script
└── reminders               # Compiled Go binary (generated)

go/
├── main.go                 # Entry point
├── auth/auth.go            # Native iCloud auth (signin, 2FA, trust, accountLogin)
├── cloudkit/client.go      # CloudKit HTTP API client
├── sync/sync.go            # Delta sync engine
├── writer/writer.go        # Write ops (add/complete/delete)
├── cache/cache.go          # Local JSON cache
├── models/models.go        # Data types
├── utils/utils.go          # CRDT title encoding, timestamps
└── cmd/                    # Cobra CLI commands
    ├── auth.go             # reminders auth
    ├── list.go             # reminders list
    ├── add.go              # reminders add / add-batch
    └── ...
```

## Troubleshooting

| Issue | Solution |
|-------|----------|
| "not authenticated" | Run `scripts/reminders.sh auth` |
| "invalid Apple ID or password" | Check credentials file |
| "2FA failed" | Re-run `auth`, enter a fresh code |
| "Missing change tag" | Run `scripts/reminders.sh sync` |
| "List not found" | Check name with `scripts/reminders.sh lists` |
| Binary not found | Run `bash scripts/build.sh` |
