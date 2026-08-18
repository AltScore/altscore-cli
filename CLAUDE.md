# CLAUDE.md - AltScore CLI

## Commands

- Build: `go build -buildvcs=false -o altscore .`
- Run without building: `go run . <args>`
- Run tests: `go test ./...`
- Run single test: `go test ./cmd -run TestName`
- Check compilation: `go build -buildvcs=false ./...`

## Project Structure

`cmd/` holds 76 `.go` files (44 non-test, 32 test). This is a subsystem map, not a full tree.

```
altscore-cli/
├── main.go
├── cmd/
│   ├── root.go                            # registerResources(), rootCmd wiring
│   ├── resource.go                        # ResourceDef + registerResource() generic CRUD
│   ├── workflows.go                       # v1 group: execute, execute-by-alias, input-schema-guide, update-schema; ResourceDef Name:"workflows" in root.go
│   ├── workflows_v2.go                    # 25 of the 36 wfv2 subcommands: all but the 7 graph edits, apply, lint and import
│   ├── workflows_v2_apply.go              # makeWfv2ApplyCmd: composeSpec + build pipeline
│   ├── workflows_v2_apply_verify.go       # post-apply verification
│   ├── workflows_v2_apply_diff.go         # --diff renderer
│   ├── workflows_v2_apply_enforce_type.go # type coercion on apply
│   ├── workflows_v2_import.go             # makeWfv2ImportCmd + the findings it reports
│   ├── workflows_v2_findings.go           # shared finding partition/render (apply + import)
│   ├── workflows_v2_preflight_validate.go # POST /v2/workflows/validate (server oracle)
│   ├── workflows_v2_validate.go           # local spec validation + makeWfv2LintCmd
│   ├── workflows_v2_normalize.go          # normalization + autodefaults
│   ├── workflows_v2_layout.go             # auto graph layout
│   ├── workflows_v2_export_apply_spec.go  # live workflow -> apply spec
│   ├── workflows_v2_helpers.go            # the 7 graph-edit subcommands (add/remove-node, add/remove-edge,
│   │                                      #   set/unset-variable, set-mapping) + wfv2LockInfo
│   ├── tasks_v2.go                        # tasks-v2 group (/v2/tasks)
│   ├── executions.go  altdata.go  analytics.go  decisions.go
│   ├── credit_decisioning.go  credit_accounts.go  payment_orders.go  dpas.go
│   ├── evaluators.go  external_source_configs.go  data_models.go  schemas.go
│   ├── workflow_tasks.go  task_tests.go  tools.go
│   ├── login.go  refresh_token.go  profiles.go  config.go  env.go
│   └── api.go  help.go  update.go  update_check.go  version.go
├── internal/
│   ├── client/{client,auth,urls}.go       # HTTP client, OAuth2, env base URLs
│   ├── config/config.go                   # TOML (~/.config/altscore/config.toml)
│   ├── output/output.go                   # JSON to stdout
│   └── version/version.go                 # feeds rootCmd's Version field in cmd/root.go
└── .claude/skills/altscore-api/
    ├── SKILL.md
    └── references/                        # 12 files incl. workflows-v2.md, workflows-v1.md
```

## workflows-v2 (the CLI's largest surface)

A ResourceDef group (`cmd/root.go`, `Name: "workflows-v2"`, `BasePath: /v2/workflows`, `BodyValidator: validateWorkflowV2Body`) extended with 36 hand-written subcommands (36 `wfv2Group.AddCommand` calls at `cmd/root.go:866-903`).

| Group | Commands |
| --- | --- |
| Authoring | `apply` (alias `compose`), `lint`, `import`, `export`, `duplicate` |
| Graph edits (the 7 helpers) | `add-node`, `remove-node`, `add-edge`, `remove-edge`, `set-variable`, `unset-variable`, `set-mapping` |
| Mapping endpoints | `update-mapping`, `resolve-mappings` |
| Lifecycle | `publish`, `create-draft`, `revert`, `archive`, `restore`, `versions`, `get-version` |
| Locking | `lock` (group), `autosave` |
| Execution | `execute`, `execute-by-alias`, `execute-batch`, `execute-batch-by-alias`, `batch`, `executions`, `download`, `schedule` |
| Introspection | `schema-guide`, `sources-status`, `external-sources-status`, `ai` |

Each of the 7 graph-edit helpers wraps lock + fetch + mutate + autosave + release, via `mutateAndAutosaveV2` in `cmd/workflows_v2_helpers.go`. The two mapping endpoints do not: `makeWfv2UpdateMappingCmd` and `makeWfv2ResolveMappingsCmd` live in `cmd/workflows_v2.go` and are single bare calls (PUT `/v2/workflows/{id}/update_mapping_workflow`, GET `/v2/workflows/{id}/resolve-mappings`) with no lock and no autosave.

`tasks-v2` is a separate top-level group (`cmd/tasks_v2.go`, registered at `cmd/root.go:905`) for `/v2/tasks`: `list`, `get`, `create`, `create-version`, `delete`, `get-soap-methods`.

### Non-negotiables

- `apply` and `compose` are one command (`cmd/workflows_v2_apply.go`, `Aliases: []string{"compose"}`). Never hand-roll `altscore api POST /v2/workflows`: that bypasses validation, normalization, auto-layout and lock handling.
- Spec shape is a flat `nodes[]` plus `edges[]`. The legacy two-bucket `tasks[]` + `extraNodes[]` shape was removed and is caught by `detectLegacySpecShape()`; it used to silently strip or half-apply `inputMappings` / `endConfig` / `htmlSections`.
- `schema-guide [section]` fetches `/v1/meta/workflows-v2-schema` live from the backend and is the authoritative shape reference. Its own `--help` lists 15 sections (architecture, endpoints, nodes, edges, variables, mappings, tasks, taskTypes, composeSpec, conditions, creditDecisioningEntities, examples, gotchas, gotchas_about_branches_and_inputkeys, preflightChecks) but that list is NOT the full set: the CLI itself also fetches `conditionOperators` (`cmd/workflows_v2_normalize.go`) plus `workflowCategories`, `relationshipKinds` and `inputSchemaTypes` (`cmd/workflows_v2_apply.go`). Run the section, do not guess field names from docs.
- `execute --test` injects the literal `test` tag so borrower-central marks the run `is_test=true` (non-billable, hidden from metrics and default lists). Side effects still run: it is NOT a dry run. Matching is on the exact `test` element, so `parity-test` does not trigger it. `--test-task-id` is a different thing: it tests one node in isolation. For a real preview use `apply --dry-run` or `--diff`.
- Local validate and `POST /v2/workflows/validate` both run BEFORE the first task POST. Anything catchable belongs in one of those two, never in the POST loop.
- Every v2 node EXCEPT `type: "start"` gets a backing `/v2/tasks` record and is referenced by alias. `start` is graph-only with no task and no alias; `end` DOES get a task. See the `composeSpec` comment and the split loop in `cmd/workflows_v2_apply.go`: `type=="start" -> ExtraNodes (graph-only); everything else (including end) -> Tasks`. PDF generation is `endConfig.pdfConfig` on the end node, not a task type.

## Architecture

The CLI uses a generic resource builder pattern. `ResourceDef` in `cmd/resource.go` defines a REST resource (name, path, actions, schemas) and `registerResource()` generates Cobra subcommands for each action (list, get, create, update, delete).

There are TWO registration mechanisms and `cmd/root.go` shows only one of them, so grepping root.go for a command name can come up empty even though the command exists.

Mechanism 1, via root.go. `func init()` (`cmd/root.go:44`) sets rootCmd's persistent flags, adds `schema` / `tools` / `version` directly, then calls `registerResources()` (call at `:55`, defined at `cmd/root.go:58`), which holds every ResourceDef plus:
- ResourceDefs that live in their own file: `cmd/external_source_configs.go:17`, wired via `registerExternalSourceConfigs()` at `cmd/root.go:907`.
- Non-CRUD groups built in their own file and only added from root: `registerTasksV2(rootCmd)` (:905), `makeCreditAccountsGroupCmd()` (:910), `makePaymentOrdersGroupCmd("payment-orders")` / `("disbursements")` (:911-912), `makeDpasGroupCmd()` (:913), `makeAnalyticsGroupCmd()` (:916), `makeDecisionsGroupCmd()` (:701).

Mechanism 2, invisible from root.go. 10 files self-register their command from their own `func init()` and are never named in root.go: api.go, altdata.go, config.go, env.go, help.go, login.go, profiles.go, refresh_token.go, update.go, update_check.go. The command name is not always the basename: help.go registers `topics` (cobra supplies `help` itself), update_check.go registers the hidden `__update-check`, refresh_token.go registers `refresh-token`. To tell the two mechanisms apart: `grep -ln "func init()" cmd/*.go` returns 11 files, and the 10 that are not root.go are exactly this list.

### Adding a new resource

1. Add a `registerResource(ResourceDef{...})` call inside `registerResources()` in `cmd/root.go`
2. Fill in `CreateSchema`, `UpdateSchema`, `ResponseSchema`, `FilterHelp` from the API docs
3. Set the behavioral fields: `HasTestMode`, `HasTestFilter`, `WorkflowAlias`, `BodyValidator`
4. Build and test with `--help`

`WorkflowAlias` is load-bearing. Its declaration comment in `cmd/resource.go` says it plainly: without it "the entity will not appear in the workflow builder's pickers". The 3-step recipe used to omit it and shipped silently invisible entities.

### Key design rules

- **JSON to stdout only.** Status messages, errors, and verbose output go to stderr.
- **Raw JSON for generic CRUD, typed structs for workflows-v2.** `registerResource` bodies are `json.RawMessage` passed through as-is. This does NOT hold for workflows-v2: `composeSpec` (`cmd/workflows_v2_apply.go`) is the typed apply spec, alongside `composeCapture` / `capturedTask` / `validationFinding` / `validationResponse` (`cmd/workflows_v2_preflight_validate.go`) and `wfv2LockInfo` (`cmd/workflows_v2_helpers.go`). Adding an apply field means editing `composeSpec`, not passing extra raw JSON. `Description *string` is a pointer on purpose: explicit `""` blanks the description, an omitted field leaves it untouched.
- **Schemas are documentation only.** They appear in `--help` text, not used for validation.
- **Auto token refresh.** On HTTP 401 the client re-authenticates and retries once.

## Code Style

- **Naming**: Go standard -- `camelCase` unexported, `PascalCase` exported
- **Imports**: Group by standard library, then third-party (`github.com/...`), then local (`internal/...`)
- **Errors**: Return `fmt.Errorf(...)` with context; Cobra handles printing
- **No external test frameworks.** Use stdlib `testing` package only.
- **CLI framework**: Cobra. Use `RunE` (not `Run`) so errors propagate.
