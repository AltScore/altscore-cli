# CLAUDE.md - AltScore CLI

## Commands

- Build: `go build -buildvcs=false -o altscore .`
- Run without building: `go run . <args>`
- Run tests: `go test ./...`
- Run single test: `go test ./cmd -run TestName`
- Check compilation: `go build -buildvcs=false ./...`

## Project Structure

`cmd/` holds 94 `.go` files (54 non-test, 40 test; ~21.3k source lines, ~8.7k test lines). This is a subsystem map, not a full tree. The workflows-v2 files are two thirds of the source; the `workflows_v2_apply*.go` stage files are about 8k lines of it.

```
altscore-cli/
├── main.go
├── cmd/
│   ├── root.go                            # registerResources(), rootCmd wiring
│   ├── resource.go                        # ResourceDef + registerResource() generic CRUD
│   ├── workflows.go                       # v1 group: execute, execute-by-alias, input-schema-guide, update-schema; ResourceDef Name:"workflows" in root.go
│   ├── workflows_v2.go                    # 25 of the 36 wfv2 subcommands: all but the 7 graph edits, apply, lint and import
│   ├── workflows_v2_apply.go              # makeWfv2ApplyCmd: flags + RunE (parse -> target -> assemble -> POST /v2/workflows/apply)
│   ├── workflows_v2_apply_spec.go         # composeSpec, detectLegacySpecShape, per-node helpers (localRef, edgeEndpoints)
│   ├── workflows_v2_apply_target.go       # findWorkflowByAlias, slugifyWorkflowLabel
│   ├── workflows_v2_apply_assemble.go     # composeWorkflowBody, applyAutoEndDefaults, htmlSections
│   ├── workflows_v2_apply_refs.go         # ref rewriters, the residual-ref safety net, topologicalTaskOrder
│   ├── workflows_v2_apply_preflight_tasks.go # preflightTasks + the offline lints it runs
│   ├── workflows_v2_apply_vocab.go        # compiled-in vocabularies + live fallback (task types, categories, rel kinds, inputSchema types)
│   ├── workflows_v2_apply_rescope.go      # reconcileEntityScopes
│   ├── workflows_v2_apply_util.go         # camelToSnake, humanizeKey, isServerAlias, sortedKeys, ...
│   ├── workflows_v2_apply_server.go       # POST /v2/workflows/apply: flat-spec builder, request, result rendering
│   ├── workflows_v2_apply_diff.go         # --diff renderer
│   ├── workflows_v2_apply_enforce_type.go # enforceType stamping on new custom variables, carry-forward on live ones
│   ├── workflows_v2_diff.go               # `diff <a> <b>`: two EXISTING versions (task bodies + specRef census); not `apply --diff`
│   ├── workflows_v2_import.go             # makeWfv2ImportCmd + the findings it reports
│   ├── workflows_v2_findings.go           # shared finding partition/render (apply + import)
│   ├── workflows_v2_validation.go         # composeCapture + validation finding types shared by apply, lint and import
│   ├── workflows_v2_validate.go           # local spec validation + makeWfv2LintCmd
│   ├── workflows_v2_readability.go        # lint's handoff readability advisories
│   ├── workflows_v2_vocabulary_cache.go   # 24h disk cache of the meta vocabulary sections (fetchMetaSection)
│   ├── workflows_v2_normalize.go          # normalization + autodefaults
│   ├── workflows_v2_layout.go             # auto graph layout
│   ├── workflows_v2_export_apply_spec.go  # live workflow -> apply spec
│   ├── workflows_v2_helpers.go            # the 7 graph-edit subcommands (add/remove-node, add/remove-edge,
│   │                                      #   set/unset-variable, set-mapping) + mutateAndAutosaveV2
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
| Authoring | `apply` (alias `compose`), `diff`, `lint`, `import`, `export`, `duplicate` |
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
- Local validate (`preflightTasks` and the normalizers) runs before the request; the server oracle runs inside `POST /v2/workflows/apply` before any write. Anything catchable locally belongs in the local pass, never after the request.
- Every v2 node, `start` and `end` included, gets a backing `/v2/tasks` record and is referenced by alias: the runtime rejects a node without a task reference. The split loop in `cmd/workflows_v2_apply.go` (`type=="start" -> ExtraNodes; everything else -> Tasks`) only decides which assembly loop builds the body; the ExtraNodes loop mints a trivial `{label, type}` task for start unless the spec supplies `taskAlias`. PDF generation is `endConfig.pdfConfig` on the end node, not a task type.

### How `apply` works

Read this before touching the `cmd/workflows_v2_apply*.go` files. `makeWfv2ApplyCmd` (`cmd/workflows_v2_apply.go`) runs ONE pipeline; `--dry-run` and `--diff` leave it before the first write. Function names below are the anchors to grep for.

1. **Parse.** `composeSpec` (typed), `detectLegacySpecShape`, then the `nodes[]` split above.
2. **Target.** `spec.alias`, else the label slug via `slugifyWorkflowLabel` with a WARNING on stderr. A relabel without an alias creates a second workflow; a future release requires the field. `findWorkflowByAlias` prefers ACTIVE, else the latest DRAFT.
3. **`stampEnforceTypeOnNewVariables`.** A new typed custom variable gets `enforceType: true`; a live one carries its live value forward (autosave sends `customVariables` wholesale, so a silent spec used to strip it).
4. **Assemble, posting nothing, exactly once.** `composeWorkflowBody`: `preflightTasks` (offline structural checks), per-type `normalizeTaskBody` (memoized live lookups: sources, entities by code, latest child workflow), `applyAutoEndDefaults` (off with `--no-auto-defaults`), `topologicalTaskOrder`, `rewriteTaskRefs` per task with the identity map, `autoLayoutNodes` (off with `--no-layout`). Each task body is recorded in `composeCapture` with a substitution closure. compose mutates the spec, so both paths below reuse this one assembly.
5. **Server-side apply (`cmd/workflows_v2_apply_server.go`).** `buildFlatSpecForServer` rebuilds the flat spec from the assembled graph plus the captured bodies (refs kept, canonical `task_outputs.<ref>` form, server-assigned keys stripped); a spec with an explicit node `alias` is refused here, before any request, with the `taskAlias` hint. `applyViaServer` sends it ONCE to `POST /v2/workflows/apply` (`dryRun` for `--dry-run` and `--diff`, `publish` only when `--publish`, `forceLock` for `--force-lock`). Borrower Central resolves every ref to a task alias before writing, validates with the oracle publish uses, creates or version-bumps tasks by `(workflowAlias, specRef)`, and creates / drafts+autosaves / publishes under its own lock, all-or-nothing. The response's `tasks[]` says what happened per ref (created / bumped / unchanged / referenced, dropped fields); `finishServerApply` renders it. A 400/422 prints the server's findings via `describeServerApplyError` and creates nothing. A backend without the endpoint (404/405 with no `APPLY_*` subcode) is an error naming v0.34.x as the last release that carried a client-side pipeline; there is no fallback.
6. **After.** `reconcileEntityScopes` stamps referenced decisioning entities (off with `--skip-rescope`). It is the one write the CLI still makes itself. Fields the server dropped come back per task in the apply response.

**Refs.** A spec-local `ref` becomes a server alias. Two rewrite layers, split by grammar, run once during assembly with the identity map; the server performs the real ref-to-alias substitution:
- The typed allowlist, `rewriteRefsInTaskTemplates` and `rewriteRefsInMappings`, handles what only a typed case can: a bare `<ref>.<field>` head, a bare `{{token}}` expanded through `inputMappings`, an unknown head rejected as a typo. A new task type that uses the BARE form needs a case there and in `templateDependencyRefs`.
- The generic pass, `rewriteTaskOutputsRefsDeep`, handles the unambiguous long form `task_outputs.<ref>` in every non-prose string and map key of a body (prose is `residualSpecRefExcludedFields`); `deepTaskOutputsRefs` feeds `topologicalTaskOrder` the same surface. A new field that uses the long form needs no CLI change.
- `validateNoResidualSpecRefs` is the safety net for bare identifiers. An embedded residual after the generic pass is a bug in that pass, not a missing allowlist entry.
- Every task carries `specRef` and `workflowAlias`; Borrower Central's stable-alias path version-bumps a match instead of minting a new alias. A ref removed from the spec orphans its task (there is no per-version delete).

**`--diff`.** The `dryRun` plan carries real aliases for every node (existing tasks keep theirs), so `diffWorkflow` matches nodes by alias (`nodeAliasKey`) and a relabel is `~`, never `-`/`+`. Known gap: a spec that pins `status: DRAFT` diffs dirty against an ACTIVE workflow even though the update path publishes regardless.

**Vocabularies.** Task types, condition operators, categories, relationship kinds and inputSchema types are compiled-in mirrors consulted first. On a miss the live section comes through `fetchMetaSection` (24h disk cache under the config dir, keyed by backend URL; a failed fetch is never cached). Live-known: warn and accept. Live-unknown: reject. Unreachable: warn and proceed, EXCEPT condition operators, which stay strict because the backend evaluates an unknown operator to False silently.

**Locks.** apply's lock lives only for the request (the server acquires and releases it); a 409 names the holder and `--force-lock` becomes the request's `forceLock`. The 7 graph-edit helpers still take the lock themselves (`acquireWfv2Lock` / `releaseWfv2Lock` in `mutateAndAutosaveV2`). Publish carries the lock token, because the backend's guard is token-strict.

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
- **Raw JSON for generic CRUD, typed structs for workflows-v2.** `registerResource` bodies are `json.RawMessage` passed through as-is. This does NOT hold for workflows-v2: `composeSpec` (`cmd/workflows_v2_apply_spec.go`) is the typed apply spec, alongside `composeCapture` / `validationFinding` / `validationResponse` (`cmd/workflows_v2_validation.go`). Adding an apply field means editing `composeSpec`, not passing extra raw JSON. `Description *string` is a pointer on purpose: explicit `""` blanks the description, an omitted field leaves it untouched.
- **Schemas are documentation only.** They appear in `--help` text, not used for validation.
- **Auto token refresh.** On HTTP 401 the client re-authenticates and retries once.

## Code Style

- **Naming**: Go standard -- `camelCase` unexported, `PascalCase` exported
- **Imports**: Group by standard library, then third-party (`github.com/...`), then local (`internal/...`)
- **Errors**: Return `fmt.Errorf(...)` with context; Cobra handles printing
- **No external test frameworks.** Use stdlib `testing` package only.
- **CLI framework**: Cobra. Use `RunE` (not `Run`) so errors propagate.
