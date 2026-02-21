#!/usr/bin/env bash
# iCloud Reminders CLI
# First run: ./scripts/reminders.sh auth  (interactive 2FA setup)

set -e
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
GO_BIN="$SCRIPT_DIR/reminders"
CREDS_FILE="$HOME/.config/icloud-reminders/credentials"

# Load credentials into environment
if [[ -f "$CREDS_FILE" ]]; then
  # shellcheck source=/dev/null
  source "$CREDS_FILE"
fi

# Build Go binary if missing
if [[ ! -x "$GO_BIN" ]]; then
  echo "Building Go binary..." >&2
  bash "$SCRIPT_DIR/build.sh" >&2
fi

exec "$GO_BIN" "$@"
