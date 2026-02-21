# iCloud Reminders CLI

Pure Go CLI for Apple iCloud Reminders. No Python, no pyicloud — just native Go with full CloudKit API support.

## Features

- ✅ List, add, complete, delete reminders
- ✅ Hierarchical subtasks
- ✅ Native 2FA support with session caching
- ✅ Cross-platform: Linux, macOS, Windows
- ✅ No dependencies — single binary

## Quick Start

```bash
# Install (macOS/Linux)
curl -sL https://github.com/tarekbecker/icloud-reminders-cli/releases/latest/download/install.sh | bash

# Or download manually from [Releases](https://github.com/tarekbecker/icloud-reminders-cli/releases)
```

## Setup

```bash
# 1. Create credentials file
mkdir -p ~/.config/icloud-reminders
cat > ~/.config/icloud-reminders/credentials << 'EOF'
export ICLOUD_USERNAME="your@apple.id"
export ICLOUD_PASSWORD="your-password"
EOF
chmod 600 ~/.config/icloud-reminders/credentials

# 2. Authenticate (interactive 2FA)
reminders auth

# 3. Use it
reminders list
reminders add "Buy milk" -l "Shopping"
```

## Usage

See [SKILL.md](SKILL.md) for full command reference.

## Building from Source

```bash
git clone https://github.com/tarekbecker/icloud-reminders-cli.git
cd icloud-reminders-cli
bash scripts/build.sh
```

## Release

```bash
git tag -a v1.0.0 -m "Release v1.0.0"
git push origin v1.0.0
# GitHub Actions builds + releases automatically
```

## License

MIT
