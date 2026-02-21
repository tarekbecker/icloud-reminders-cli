# iCloud Reminders CLI

A Python CLI for managing Apple iCloud Reminders via the CloudKit API. Features full CRUD operations, hierarchical subtask support, and efficient delta sync.

## Features

- 📝 **Full CRUD** — Create, read, complete, delete reminders
- 🌲 **Subtasks** — Hierarchical reminder trees with `--parent`
- ⚡ **Delta Sync** — Fast incremental updates via CloudKit sync tokens
- 🔍 **Search** — Find reminders by title
- 📋 **Lists** — Filter and organize by reminder lists
- 📅 **Due Dates & Priorities** — Set deadlines and importance levels
- 📤 **JSON Export** — Machine-readable output

## Installation

### Prerequisites

- Python 3.10+
- [pyicloud](https://github.com/picklepete/pyicloud) library

### Setup

1. **Install dependencies:**
   ```bash
   pip install pyicloud requests
   ```

2. **Create config directory:**
   ```bash
   mkdir -p ~/.config/icloud-reminders
   ```

3. **Add credentials** (`~/.config/icloud-reminders/credentials`):
   ```bash
   export ICLOUD_USERNAME="your@apple.id"
   export ICLOUD_PASSWORD="your-app-specific-password"
   ```
   
   > 💡 Use an [App-Specific Password](https://support.apple.com/en-us/HT204397) for better security.

4. **Complete 2FA authentication:**
   ```bash
   ./reminders.sh list
   ```
   Enter your 2FA code when prompted. The session is cached in `~/.config/icloud-reminders/`.

## Usage

### List Reminders

```bash
# All active reminders (with subtask hierarchy)
./reminders.sh list

# Filter by list
./reminders.sh list -l "🛒 Einkauf"

# Include completed items
./reminders.sh list --all
```

### Add Reminders

```bash
# Basic
./reminders.sh add "Buy groceries" -l "Shopping"

# With due date and priority
./reminders.sh add "Submit report" -l "Work" --due 2026-03-01 --priority high

# Add as subtask
./reminders.sh add "Milk" --parent ABC123

# With notes
./reminders.sh add "Call doctor" --notes "Annual checkup"
```

### Manage Reminders

```bash
# Complete a reminder (use ID from list output)
./reminders.sh complete 9593651F

# Delete a reminder
./reminders.sh delete 9593651F

# Search by title
./reminders.sh search "milk"

# Show all lists
./reminders.sh lists
```

### Sync & Export

```bash
# Force full resync (clears cache)
./reminders.sh sync

# Export as JSON
./reminders.sh json
```

## Output Format

```
✅ Reminders: 42 (38 active)

📋 🛒 Einkauf (12)
  • Supermarkt  (9593651F)
    • Butter  (C58C792C)
    • Käse  (AADFFDDB)
    • Milch  (12345678)
  • DM  (72581C3B)
    • Shampoo  (5AF1068B)

📋 Work (8)
  • Review PR  (ABCD1234)
  • Schedule meeting  (EFGH5678)
```

- Parent reminders at base indentation
- Subtasks indented under their parent
- 8-character ID in parentheses (use for `complete`, `delete`, `--parent`)

## Configuration

| File | Purpose |
|------|---------|
| `~/.config/icloud-reminders/credentials` | iCloud credentials (sourced by shell) |
| `~/.config/icloud-reminders/ck_cache.json` | Local cache with sync token |
| `~/.config/icloud-reminders/` | pyicloud session cookies |

## Session Sharing (Optional)

Share iCloud access without sharing your password by exporting session cookies:

```bash
# Export session to file
./reminders.sh export-session my-session.tar.gz

# On another machine: import session
./reminders.sh import-session my-session.tar.gz
```

After importing, you may need to create a credentials file with just the username:
```bash
echo 'export ICLOUD_USERNAME="your@apple.id"' > ~/.config/icloud-reminders/credentials
```

> ⚠️ **Security Warning:** Session files grant full iCloud access to your account (not just Reminders). Only share with trusted parties. Sessions may expire and require re-authentication.

## Troubleshooting

### "Re-auth required"

Your iCloud session has expired. Run `./reminders.sh list` in an interactive terminal and complete the 2FA challenge.

### "Missing change tag"

The local cache is out of sync. Run:
```bash
./reminders.sh sync
```

### "List not found"

List names are case-insensitive but must otherwise match exactly. Check available lists:
```bash
./reminders.sh lists
```

### Sync is slow

Full sync can take **~2 minutes** for accounts with many reminders. Subsequent syncs use delta updates and are much faster.

### Special characters in titles

Titles with emojis and umlauts (ä, ö, ü) work correctly. The CRDT encoder handles Unicode properly.

## Technical Notes

### TitleDocument Format

Apple Reminders stores titles in a CRDT (Conflict-free Replicated Data Type) format called `TitleDocument`:

```
TitleDocument = base64(gzip(protobuf(CRDT)))
```

The protobuf structure contains:
- The title text (UTF-8)
- CRDT operations (insert, position markers)
- Metadata (UUID, clock values, replicas)
- Attribute runs

### Character vs Byte Length

**Important:** Apple's CRDT metadata uses **character length**, not byte length. This matters for Unicode:

```python
title = "Käse"
char_len = len(title)           # 4 characters (correct for CRDT)
byte_len = len(title.encode())  # 5 bytes (wrong for CRDT)
```

The encoder correctly uses character length for:
- CRDT operation lengths
- Clock values
- Attribute run lengths

### Optimistic Concurrency

Write operations (complete, delete) require `recordChangeTag` for CloudKit's optimistic locking. The cache stores these tags; if they're stale, run `sync` to refresh.

### Delta Sync

CloudKit provides `syncToken` for efficient delta synchronization. After initial full sync, only changed records are fetched. The token is stored in the cache file.

## Files

```
skills/icloud-reminders/
├── reminders_ck.py   # Main Python script
├── reminders.sh      # Shell wrapper (loads credentials)
├── SKILL.md          # OpenClaw skill documentation
├── README.md         # This file
└── LICENSE           # MIT License
```

## License

MIT License — see [LICENSE](LICENSE) for details.
