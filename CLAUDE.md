# CLAUDE.md - AltScore CLI

## Commands

- Build: `go build -buildvcs=false -o altscore .`
- Run without building: `go run . <args>`
- Run tests: `go test ./...`
- Run single test: `go test ./cmd -run TestName`
- Check compilation: `go build -buildvcs=false ./...`

## Project Structure

```
altscore-cli/
├── main.go                     # Entry point
├── cmd/                        # Cobra command definitions
│   ├── root.go                 # Root command, resource registrations
│   ├── resource.go             # Generic resource CRUD builder (ResourceDef)
│   ├── login.go                # OAuth2 login flow
│   ├── profiles.go             # Profile management (list, show, delete, set-default)
│   ├── config.go               # Show resolved config
│   ├── api.go                  # Raw API passthrough
│   └── help.go                 # Topic-based help system
├── internal/
│   ├── client/
│   │   ├── client.go           # HTTP client with auto token refresh
│   │   ├── auth.go             # OAuth2 token exchange
│   │   └── urls.go             # Environment base URL mapping
│   ├── config/
│   │   └── config.go           # TOML config (~/.config/altscore/config.toml)
│   └── output/
│       └── output.go           # JSON pretty-printing to stdout
└── .claude/skills/altscore-api/
    └── SKILL.md                # Agent skill for API interaction
```

## Architecture

The CLI uses a generic resource builder pattern. `ResourceDef` in `cmd/resource.go` defines a REST resource (name, path, actions, schemas) and `registerResource()` generates Cobra subcommands for each action (list, get, create, update, delete).

Resources are registered in `cmd/root.go` with their API schemas embedded as help text.

### Adding a new resource

1. Add a `registerResource(ResourceDef{...})` call in `cmd/root.go`
2. Fill in `CreateSchema`, `UpdateSchema`, `ResponseSchema`, `FilterHelp` from the API docs
3. Build and test with `--help`

### Key design rules

- **JSON to stdout only.** Status messages, errors, and verbose output go to stderr.
- **No Go structs for API types.** Request/response bodies are `json.RawMessage` passed through as-is.
- **Schemas are documentation only.** They appear in `--help` text, not used for validation.
- **Auto token refresh.** On HTTP 401 the client re-authenticates and retries once.

## Code Style

- **Naming**: Go standard -- `camelCase` unexported, `PascalCase` exported
- **Imports**: Group by standard library, then third-party (`github.com/...`), then local (`internal/...`)
- **Errors**: Return `fmt.Errorf(...)` with context; Cobra handles printing
- **No external test frameworks.** Use stdlib `testing` package only.
- **CLI framework**: Cobra. Use `RunE` (not `Run`) so errors propagate.
