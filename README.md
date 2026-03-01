# altscore

CLI for the AltScore API. Manages borrowers, identities, documents, deals, executions, packages, and AltData sources.

## Install

Requires `gh` (GitHub CLI) with access to the AltScore org.

```bash
gh release download --repo AltScore/altscore-cli --pattern "altscore-$(uname -s | tr '[:upper:]' '[:lower:]')-$(uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/')" --output /usr/local/bin/altscore --clobber
chmod +x /usr/local/bin/altscore
```

Or build from source (requires Go 1.25+):

```bash
go build -buildvcs=false -o altscore .
```

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

This repo includes a Claude Code skill at `.claude/skills/altscore-api/SKILL.md` that gives agents full access to the API through the CLI.

## Release

```bash
./release.sh v0.2.0
```

Builds for darwin/arm64, darwin/amd64, linux/amd64 and publishes to GitHub Releases.
