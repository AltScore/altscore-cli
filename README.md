# altscore

CLI for the AltScore API. Manages borrowers, identities, documents, deals, executions, packages, and AltData sources.

## Install

Requires `gh` (GitHub CLI) with access to the AltScore org.

```bash
gh release download --repo AltScore/altscore-cli --pattern "altscore-$(uname -s | tr '[:upper:]' '[:lower:]')-$(uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/')" --output /usr/local/bin/altscore --clobber
chmod +x /usr/local/bin/altscore
altscore skill install   # the Claude Code skill that teaches agents this CLI
```

Or build from source (requires Go 1.25+):

```bash
go build -buildvcs=false -o altscore .
```

## Updating

```bash
altscore update
```

Downloads the latest release, verifies its SHA-256 checksum, and replaces the
binary in place.

The CLI also checks for newer releases on its own. At most once every 24 hours it
refreshes a cached version number in a detached background process (it never adds
latency to your command), and prints a one-line "update available" notice on an
interactive terminal when you are behind. To upgrade silently instead of being
notified, set `auto_update = true` under `[defaults]` in `~/.config/altscore/config.toml`
or export `ALTSCORE_AUTO_UPDATE=1`. Silent auto-update is skipped in CI and
non-interactive shells.

Disable the check entirely with `ALTSCORE_NO_UPDATE_CHECK=1`. Dev builds never
check or notify.

## Login

```bash
altscore login
```

Walks you through profile, environment, credentials, and tenant. Tenant is auto-detected after authentication.

## Usage

```bash
altscore borrowers list --per-page 5
altscore borrowers get <id>
altscore borrowers create --body '{"persona": "individual", "label": "Jane Doe"}'
altscore api GET /v1/borrowers/<id>/summary
```

All commands output JSON to stdout. Use `--help` on any command to see fields, filters, and examples.

## Claude Code Skill

The binary embeds the Claude Code skill `altscore-cli` (`internal/skill/assets/altscore-cli/`), which gives agents full access to the API through the CLI. Install it once:

```bash
altscore skill install          # writes ~/.claude/skills/altscore-cli and stamps a marker
altscore skill status           # installed? managed? same version as the binary?
altscore skill install --force  # replace a hand-copied skill directory
```

A managed install is refreshed by `altscore update` and by the first command run after a new version lands, so skill and CLI always carry the same tag. A directory without the marker is a hand copy and is never overwritten without `--force`. Set `CLAUDE_CONFIG_DIR` to target a non-default Claude Code config directory, or `--dir` for a project-local install.

Inside this repo the skill also loads from the checkout: `.claude/skills/altscore-cli` is a symlink to the embedded assets, so editing the skill and running it from here is the same file. Edit the skill under `internal/skill/assets/`, never in `~/.claude/skills/`.

## Release

```bash
./release.sh v0.2.0
```

Builds for darwin/arm64, darwin/amd64, linux/amd64 and publishes to GitHub Releases.
