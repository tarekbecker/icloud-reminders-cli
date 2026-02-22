# Homebrew Tap Setup

This document describes how the Homebrew tap is configured.

## User Installation

Users can install via Homebrew:

```bash
brew tap tarekbecker/tap
brew install icloud-reminders
```

## Maintainer Setup

### 1. Create the Tap Repository

Create a public repository on GitHub: `tarekbecker/homebrew-tap`

This repo can be empty initially — GoReleaser will create the Formula file on first release.

### 2. Configure GitHub Actions Secret

The workflow needs a token to push to the tap repo. Two options:

#### Option A: Use GITHUB_TOKEN (simpler, same-owner only)

If the tap repo is under the same user/org as the source repo, the built-in `GITHUB_TOKEN` works. The workflow already supports this fallback:

```yaml
TAP_GITHUB_TOKEN: ${{ secrets.TAP_GITHUB_TOKEN || secrets.GITHUB_TOKEN }}
```

#### Option B: Personal Access Token (PAT)

If the tap is under a different owner, create a PAT:

1. Go to GitHub Settings → Developer settings → Personal access tokens → Fine-grained tokens
2. Create a token with:
   - Repository access: `tarekbecker/homebrew-tap`
   - Permissions: `Contents: Read and Write`
3. Add as a repository secret in `icloud-reminders-cli`:
   - Name: `TAP_GITHUB_TOKEN`
   - Value: your PAT

### 3. Release Process

On each Git tag push, GoReleaser will:

1. Build binaries for all platforms
2. Create a GitHub release
3. Update the Formula in `homebrew-tap` with the new version, URL, and SHA256

The Formula will be at: `https://github.com/tarekbecker/homebrew-tap/blob/main/Formula/icloud-reminders.rb`

## How It Works

GoReleaser's `brews` configuration (in `.goreleaser.yaml`) tells it to:

1. Download the built binary from the GitHub release
2. Generate a Homebrew Formula with the correct URL and SHA256
3. Push the Formula to the `homebrew-tap` repository

Users then `brew tap` the repo and `brew install` the formula.

## Troubleshooting

**"Error: Invalid formula"**
- Check that the SHA256 in the Formula matches the release asset
- GoReleaser handles this automatically, but manual edits can break it

**"Error: Tap not found"**
- Ensure the repo `tarekbecker/homebrew-tap` is public
- Check the repo name matches exactly (case-sensitive)

**Formula not updating**
- Check the GitHub Actions log for errors
- Ensure `TAP_GITHUB_TOKEN` has write access to the tap repo
- Verify the `.goreleaser.yaml` syntax is valid
