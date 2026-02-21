#!/usr/bin/env bash
set -e
cd "$(dirname "$0")/../go"
echo "Building icloud-reminders Go binary..."
go build -o ../scripts/reminders .
echo "✅ Built: $(pwd)/../scripts/reminders"
