### Workflows V2 (Visual Builder)

> **⚠️ STOP — read this before doing anything.**
>
> If the user asks "create a v2 workflow that does X" — or "update workflow Y to do Z" — run **`altscore workflows-v2 apply`** with a single spec file. `apply` is declarative: it reconciles the spec against the tenant. If no workflow shares the spec's alias, it creates one (POST tasks + POST workflow); if one already exists, it updates in place (fresh tasks + create-draft + lock + autosave + publish, same workflow id and alias retained). One verb, one validation pipeline, no fork-vs-update branch for the agent.
>
> Do not call `workflows-v2 create` directly with hand-built nodes — that path produces orphan nodes (no `taskAlias`) that save successfully but break the Hub UI (`GET /v2/tasks/null` 404 for every node). The CLI rejects orphan-node bodies at write time with an error pointing at apply; if you see that error, you're on the wrong path — switch to apply.
>
> `apply` is the **only** recommended path for both greenfield and update. Direct `create` is for special cases where you've already created the tasks via `tasks-v2 create` and assembled a body with proper `taskAlias` references on every non-start/non-end node.
>
> **Two more silent traps the API doesn't reject** (the CLI now rejects both, but agents must understand the canonical shape):
>
> 1. **Conditional branches use structured `conditions`, not `expression` strings.** The API stores `expression` as a no-op and the branch never fires. Use:
>    ```json
>    "branches": [
>      {"id": "branch_approve", "label": "Approve", "isElse": false, "order": 0,
>       "conditions": {"operator": "AND",
>         "items": [{"field": "score", "operator": "gte", "value": "700", "valueType": "value"}]}},
>      {"id": "branch-else", "label": "Reject", "isElse": true, "order": 1, "conditions": null}
>    ]
>    ```
>    Operators: `eq, neq, gt, gte, lt, lte, contains, startsWith, endsWith, in, notIn, between, isNull, isNotNull, arrayContainsAny, arrayContainsAll, isAltdataEmpty, isAltdataNotCalculated, isAltdataError, isAltdataNull, isNotAltdataNull`. `valueType` is `"value"` (literal) or `"variable"` (reference to another inputSchema field). Field is `isElse` (camelCase), not `is_else`.
>
> 2. **`altdata-enrichment` tasks need `inputKeys` to wire source-required fields.** Each source (e.g. `ECU-PUB-0002`) declares `inputFields` like `personId`, `taxId`. The task must include `inputKeys: {"personId": "{{personId}}", "taxId": "{{taxId}}"}` matched against an `inputSchema` that declares those keys, plus `dataAge` (cache TTL minutes, default 30) and `packageAlias` (where to store results) on each `sourcesConfig` entry. `apply` auto-derives `inputKeys` by querying `sources-status` for each source's `inputFields` — use it.
>
> Run `altscore workflows-v2 schema-guide conditions` and `... schema-guide tasks` for the canonical reference.

Workflows V2 is the API surface for the visual graph builder in the Hub. It uses **two collaborating resources**:

| Resource | Path | Purpose |
|---|---|---|
| **Tasks** | `/v2/tasks` | Versioned executable units (HTTP url, evaluator alias, sources_config, branches, ...) |
| **Workflows** | `/v2/workflows` | The graph: `nodes[]` + `edges[]` + variables. Each node references a task by `taskAlias` |

This is **not** v1 (`/v1/workflows`). Use v2 for anything created in the visual editor.

**Key insight: tasks first, then workflow.** **Every** graph node — including `start`, `end`, and `conditional` — needs a `taskAlias`. The Hub creates trivial backing tasks (just `type` + `label`) for start/end so it can render them. Verified by inspecting working tenant workflows: every node has a non-null `taskAlias`. Use `apply`, which creates a backing task for every node automatically.

After creating a workflow, run `altscore workflows-v2 lint <id>` to verify there are no orphan nodes, dangling edges, or duplicate ids. The lint command also runs the same checks as the create-time validator and is the fastest way to triage a misbehaving workflow.

For canonical field-by-field reference run:

```bash
altscore workflows-v2 schema-guide               # full guide
altscore workflows-v2 schema-guide architecture  # the tasks-first explanation
altscore workflows-v2 schema-guide nodes         # node shape (camelCase: nodeId, label, taskAlias, ...)
altscore workflows-v2 schema-guide edges         # edge shape (sourceNodeId, targetNodeId)
altscore workflows-v2 schema-guide tasks         # task shape + per-type config fields
altscore workflows-v2 schema-guide examples      # full scoring_pipeline template
```

#### Recommended path: `apply` (declarative create-or-update)

For "Create a workflow that does X" — or "Update workflow Y to do Z" — use `workflows-v2 apply`. It takes a single spec and reconciles it against the tenant. Use `--dry-run` first to inspect what will be sent (the dry-run output also tells you which branch will fire: CREATE or UPDATE).

Use `--diff` to preview changes against the current tenant state before mutating. Pairs well with the UPDATE path — see exactly what version-bump will change before pulling the trigger. The diff renders structural deltas on metadata, nodes (added/removed/changed by stable slugified-label identity), edges, input/customVariables, and any entity-scope conflicts the apply would touch. Read-only: no `/v2/tasks` POSTs, no `/v2/workflows` mutations, no entity PATCHes. Mutually exclusive with `--dry-run` and `--publish`.

**Create vs update semantics.** apply resolves the target by alias:

- `spec.alias` if set, otherwise `slugifyWorkflowLabel(spec.label)`.
- If **no** ACTIVE workflow has that alias → **CREATE path**: POST `/v2/tasks` for every task (server picks each task's alias), POST `/v2/workflows`, optional publish (DRAFT by default unless `--publish`).
- If exactly **one** ACTIVE workflow has that alias → **UPDATE path**: POST `/v2/tasks` for every task in the spec (old tasks orphan, accepted), `create-draft --force-recreate`, acquire a lock (`client-id=apply-<ts>`), autosave the new nodes/edges/variables/config/notes/category/status, then publish. Same workflow id, same alias, version increments, schedules and downstream consumers survive.

Both paths share the same validation + normalize pipeline (`preflightTasks`, per-type normalize, `validateEntityWorkflowAliasMatch`, etc.) so a spec that passes against a fresh tenant also passes when re-applied later.

**Entity-scope reconciliation (auto-restamp).** After apply succeeds, it walks the spec's referenced credit-decisioning entities and stamps each one's `workflowAlias` to the workflow's alias via `PATCH /v1/{resource}/{id}`. Walked:

- `scorecard` task → scorecard entity → each `rules[*].mappingTableCode` mapping table.
- `rule-tree` task → rule-tree entity → each `rules[*].ruleCode` evaluation rule.
- `evaluate-rules` task → each `rulesConfig[*].ruleCode` evaluation rule.
- `mapping-table` task → each `mappingTableConfig.entries[*].mappingTableCode` mapping table.

Each successful stamp logs to stderr (`# scoped <resource> <code> to <alias>`). This is the fix for the "four -v2 sibling workflows all share a scorecard scoped to the original alias" bug — apply makes the workflow→entity scope mapping reflect the spec, automatically. Pass `--skip-rescope` to opt out (then a stale scope shows as a hard error in normalize and the agent has to fix it manually).

**Entity ownership rule.** Each v2 workflow owns its credit-decisioning entities 1:1. A workflow's spec must NOT reference an entity whose `workflowAlias` is currently another workflow's alias. If you want the same logic in two workflows, **clone the entity** with a new code (e.g. `kyc-sc` and `kyb-sc` instead of one shared `scoring-sc`). Apply now refuses to silently steal ownership; pass `--allow-steal-ownership` if you really do want to transfer.

**Refs vs aliases.** The spec uses `ref` as a stable spec-local key. Edges and inputMappings reference tasks by `ref`, and apply rewrites them with the server-assigned aliases at task-create time. You can also pass an explicit `alias` on the workflow itself (`spec.alias`) — apply uses it both to resolve the target and to keep `workflowAlias` stable across re-applies. Edges use `from`/`to` as shortcuts for `sourceNodeId`/`targetNodeId`.

```bash
cat > /tmp/spec.json <<'EOF'
{
  "label": "Scoring pipeline",
  "category": "EVALUATION",
  "description": "Fetch, score, route",
  "inputVariables": {
    "borrower_id": {"type": "string", "required": true},
    "min_score":   {"type": "integer", "default": 700}
  },
  "nodes": [
    {"ref": "start", "type": "start", "label": "Start"},
    {
      "ref": "fetch",
      "label": "Fetch ECU bureau",
      "type": "altdata-enrichment",
      "sourcesConfig": [{"sourceId": "ECU-PUB-0002", "version": "v1"}],
      "borrowerIdField": "personId",
      "inputMappings": {"personId": "inputs.borrower_id"}
    },
    {
      "ref": "score",
      "label": "Score",
      "type": "evaluate-rules",
      "evaluatorTask": "scoring",
      "inputSchema": {"credit_data": {"type": "object"}},
      "inputMappings": {"credit_data": "task_outputs.fetch.sources_output_packages"}
    },
    {
      "ref": "route",
      "label": "Approve or reject",
      "type": "conditional",
      "inputSchema": {
        "score":     {"type": "number"},
        "min_score": {"type": "integer"}
      },
      "inputMappings": {
        "score":     "task_outputs.score.value",
        "min_score": "inputs.min_score"
      },
      "branches": [
        {"label": "Approve",
         "conditions": {"operator": "AND",
           "items": [{"field": "score", "operator": "gte", "value": "min_score", "valueType": "variable"}]}},
        {"label": "Reject", "isElse": true}
      ]
    },
    {"ref": "end", "type": "end", "label": "End"}
  ],
  "edges": [
    {"from": "start", "to": "fetch"},
    {"from": "fetch", "to": "score"},
    {"from": "score", "to": "route"},
    {"from": "route", "to": "end", "sourceHandle": "branch_0"}
  ]
}
EOF
```

**Spec shape.** `nodes[]` is the one accepted input — a flat list with one entry per graph node (start, end, every task type). Apply dispatches each entry by `type` at parse time: `start` is graph-only, everything else (including `end`) gets a backing task created. The legacy `tasks[]` + `extraNodes[]` two-bucket shape was removed; apply rejects it with an inline rewrite suggestion.

```bash
cat > /tmp/spec-minimal.json <<'EOF'
{
  "label": "Score and Route",
  "alias": "score-and-route",
  "category": "EVALUATION",
  "inputVariables": {"borrower_id": {"type": "string", "required": true}},
  "nodes": [
    {"ref": "start", "type": "start", "label": "Start"},
    {"ref": "fetch", "type": "altdata-enrichment", "...": "..."},
    {"ref": "end",   "type": "end", "endConfig": {"...": "..."}}
  ],
  "edges": [{"from": "start", "to": "fetch"}, {"from": "fetch", "to": "end"}]
}
EOF

altscore workflows-v2 apply --body @/tmp/spec.json --dry-run   # inspect first (prints CREATE vs UPDATE branch)
altscore workflows-v2 apply --body @/tmp/spec.json --publish   # create+publish OR update+publish (the one you usually want)
altscore workflows-v2 apply --body @/tmp/spec.json             # create-only -- leaves workflow in DRAFT (CREATE path); UPDATE path always publishes
altscore workflows-v2 apply --body @/tmp/spec.json --skip-rescope  # do not auto-stamp referenced entities
altscore workflows-v2 publish <id>                              # standalone publish step
```

#### Canonical end-node pattern (single end, not N parallel ends)

When a spec contains a `rule-tree` task, the recommended end-node shape is **one** end node fed directly by the rule-tree -- not N parallel ends behind a `conditional` router. BC's `end_activity` already promotes four fields from the end task's resolved input context onto the execution record automatically (see `borrower-central/app/temporal/activities/end_activity.py:339-360`):

| Context key | Execution-record field | Source |
|---|---|---|
| `borrower_id` | `borrowerId` (the customer id) | upstream customer task output, or `inputs.borrower_id` |
| `billable_id` | `billableId` (defaults to `borrowerId`) | upstream customer/deal task output |
| `deal_id` | `dealId` | upstream deal task output |
| `decision_key` | `currentDecision.key` (when `decisionConfig.enabled=true`) | upstream rule-tree task output (`task_outputs.<rule-tree-ref>.decision_key`) |

Wired this way, the rule-tree's per-run decision string flows through to BC's decision recorder, the PDF generates once, and there's no per-branch hand-maintained `outputJson` to drift. Apply ships a non-blocking lint (`lintCanonicalEndNode`) that warns when a spec has both a rule-tree and an end node but the end node is missing any of: `inputMappings.decision_key`, `endConfig.decisionConfig.enabled=true`, `endConfig.pdfConfig.enabled=true`.

Canonical shape:

```json
{
  "ref": "end",
  "type": "end",
  "label": "End",
  "inputSchema": {
    "borrower_id":  {"type": "string"},
    "deal_id":      {"type": "string"},
    "decision_key": {"type": "string"}
  },
  "inputMappings": {
    "borrower_id":  "task_outputs.<customer-task-ref>.borrower_id",
    "deal_id":      "task_outputs.<deal-task-ref>.deal_id",
    "decision_key": "task_outputs.<rule-tree-ref>.decision_key"
  },
  "endConfig": {
    "decisionConfig": {"enabled": true, "decisionType": "final"},
    "pdfConfig":      {"enabled": true, "title": "Credit Decision Report", "filePrefix": "credit-decision", "sourcesConfig": []},
    "outputJson":     "{\"customer_id\":\"{{inputs.<borrower-input-ref>}}\",\"decision\":\"{{task_outputs.<rule-tree-ref>.decision_key}}\"}"
  }
}
```

> **outputJson template syntax.** Bare placeholders like `{{borrower_id}}` and `{{decision_key}}` do NOT resolve in BC's `VariableResolver` -- only `{{inputs.X}}`, `{{task_outputs.X.Y}}`, `{{custom.X}}`, `{{system.X}}`, and bare-alias `{{<alias>.<field>}}` (with a dot). A bare key stays literal, corrupts the rendered JSON, and the runtime silently falls back to the promoted-scope dump (your custom envelope vanishes with no error). The `inputMappings` short-name keys (`borrower_id`, `decision_key`) drive BC's per-context promotion (`result['decision_key']`, `result['borrower_id']` on the execution record) and PDF section enrichment -- but NOT outputJson substitution. Always use the long form (`{{task_outputs.<ref>.decision_key}}`) in outputJson, even when the same key is also in `inputMappings`.

**When multiple end nodes are correct.** Rare, but legal: post-decision tasks differ per branch (one branch hits an external webhook the other doesn't), or per-branch `htmlSections` that aren't expressible as `{decision_key}` substitutions. In those cases keep the conditional + N ends, but still wire `decision_key` on every end's `inputMappings`.

> **DRAFT vs publish (test before you publish).** apply's CREATE path saves workflows in `status: "DRAFT"` by default — mirrors the Hub's "save-then-publish" editor flow. A DRAFT executes its **full graph faithfully** — node execution is never gated by status (the engine reads nodes straight from the workflow doc), so a DRAFT run is a real, complete test run. Publishing only changes which version the **alias** serves (the alias resolves to the latest `ACTIVE` version). So to validate before going live, execute the DRAFT **by its workflow id** (or by alias in test mode) — you do **not** have to publish untested work to run it. Pass `--publish` on apply when you *do* want it live immediately; the UPDATE path always publishes (apply treats the spec as desired state). `workflows-v2 execute` prints an informational stderr note when the targeted version isn't `ACTIVE`; pass `--skip-status-check` to silence it. (Earlier docs claimed a DRAFT "skips every node and emits null output" — that was never true of the engine; corrected May 2026.)

If the agent asks for help building or updating a workflow, default to apply. Other paths exist for special cases:

- **Clone-and-modify** (highest success when a similar workflow exists): `export <similar-id>` → edit JSON → `validate-rules` → `import --new-label "..."`
- **Incremental edit on an existing workflow**: hold a lock + use `add-node`/`add-edge`/`set-mapping`/`set-variable` helpers (see Helpers section below)
- **Manual** (only if you need explicit control): `tasks-v2 create` per task, then `workflows-v2 create --body @workflow.json` with hand-built nodes

#### Tasks (`/v2/tasks`)

```bash
# Create a task (alias optional; auto-generated if omitted).
# altdata-enrichment requires inputKeys when sourcesConfig is non-empty:
altscore tasks-v2 create --body '{
  "alias":"fetch-ecu",
  "label":"Fetch ECU bureau",
  "type":"altdata-enrichment",
  "sourcesConfig":[{"sourceId":"ECU-PUB-0002","version":"v1","dataAge":30,"packageAlias":"ecu_pub_0002"}],
  "borrowerIdField":"personId",
  "inputSchema":{"personId":{"type":"string"},"taxId":{"type":"string"}},
  "inputKeys":{"personId":"{{personId}}","taxId":"{{taxId}}"},
  "inputMappings":{"personId":"inputs.borrower_id"},
  "mode":"single","savePackages":true,"timeout":60
}'

# Bump version (existing nodes pinning v1 are unaffected)
altscore tasks-v2 create-version fetch-ecu --body '{
  "label":"Fetch ECU bureau v2",
  "type":"altdata-enrichment",
  "sourcesConfig":[
    {"sourceId":"ECU-PUB-0002","version":"v1","dataAge":30,"packageAlias":"ecu_pub_0002"},
    {"sourceId":"ECU-PUB-0014","version":"v1","dataAge":30,"packageAlias":"ecu_pub_0014"}
  ],
  "inputKeys":{"personId":"{{personId}}","taxId":"{{taxId}}"}
}'

# Inspect (returns latest + version history)
altscore tasks-v2 get fetch-ecu

# List tasks (paginated; defaults to one row per alias, the latest version)
altscore tasks-v2 list --per-page 50
altscore tasks-v2 list --alias-prefix co2- --per-page 50           # cleanup residue
altscore tasks-v2 list --type http,conditional                      # filter by type
altscore tasks-v2 list --workflow-alias kyc-lite                    # tasks referenced by a workflow
altscore tasks-v2 list --include-all                                # every historical version

# Delete every version of a task (refused with 409 if any non-archived
# workflow's latest version still references it -- error message lists
# {workflowAlias, nodeId} pairs so you know what to detach)
altscore tasks-v2 delete co2-orphan-task
```

**Cleanup-after-failed-apply flow:** when `workflows-v2 apply` leaves orphan tasks behind (typical symptom: agents rotating alias prefixes like `co2- -> co2v- -> co2x-` to dodge their own residue), run:

```bash
# 1. enumerate orphan tasks by the alias prefix the failed apply used
altscore tasks-v2 list --alias-prefix co2- --per-page 100 | jq -r '.[].alias' > /tmp/orphans.txt

# 2. delete each. 409 means the task is still referenced -- the error
#    message tells you which workflow+node still pin it. Detach those
#    nodes (autosave) before retrying the delete.
xargs -n1 altscore tasks-v2 delete < /tmp/orphans.txt
```

Per-type config (full reference: `schema-guide tasks`):

| type | key fields |
|---|---|
| `altdata-enrichment` | `sourcesConfig`, `borrowerIdField`, `mode`, `savePackages` |
| `evaluate-rules` | `evaluatorTask`, optional `rulesConfig`/`scorecardConfig`/`ruleTreeConfig` |
| `http` | `url`, `method`, `headers` (JSON string), `body`, auth fields |
| `conditional` | `branches`: `[{label, expression, is_else}]` |
| `wait` | `seconds` or `untilCondition` |
| `webhook` | `secret` |
| `compute-variables` | `selectedVariables` |
| `data-store` | `dataStoreWriteConfig` or `dataStoreQueryConfig` |
| `pdf-report` / `end` | `endConfig: {title, subtitle, brand_logo}` |
| `customer` / `deal` / `asset` | `operation` (`write`/`read`), `lookupBy`, `key`/`identityKey`, `inputSchema`, `inputMappings`, `sourcesConfig` |

For `customer` / `deal` / `asset` write tasks the `sourcesConfig` entries map each persisted attribute to a context key. Common entry shapes:

- `{type: "deal_field", key: "<context_key>", label: "..."}` — write one deal_field record per entry.
- `{type: "identity", key: "<identity_key>"}` / `{type: "borrower_field", key: "..."}` for customer/asset writes.

> **Attaching contacts to a deal: inline `contacts` ONLY.** The `deal_contact` (singular) and `deal_contacts` (plural) `sourcesConfig` types are **no longer supported**. Attaching deal contacts via `sourcesConfig` is rejected by `apply`. Declare contacts in the **inline `contacts` field** on the deal node instead — it both persists the DealContact rows and emits the per-contact `deal-<id>` output handles needed for scoping. Set `upsertContacts: true` (top-level on the deal task) to let rows resolve/create borrowers by identity. See [Inline `contacts` field](#per-item-output-scoping) below.

#### Workflow CRUD

```bash
altscore workflows-v2 list --filter status=ACTIVE --filter is-latest=true
altscore workflows-v2 get  <id>
altscore workflows-v2 lint <id>                                    # check for orphan nodes / dangling edges
altscore workflows-v2 update <id> --body '{"label":"Renamed"}'     # prefer autosave for graph edits
altscore workflows-v2 delete <id>

# Direct create is gated by client-side validation -- prefer apply for greenfield and updates.
# If you really need to create from a hand-built body (and have already created
# all referenced /v2/tasks), the body must use camelCase (nodeId, sourceNodeId,
# targetNodeId) and every non-start/non-end node must have taskAlias or taskId.
altscore workflows-v2 create --body @already-wired.json
```

`list` returns the **full DTO** with `nodes` and `edges`. Empty arrays = the workflow is genuinely empty, not a summary projection.

After any create or autosave, run `altscore workflows-v2 lint <id>` to confirm no orphan nodes, dangling edges, or duplicate node ids.

#### Lock dance (required before edits)

```bash
TOKEN=$(altscore workflows-v2 lock acquire my-wf --client-id "agent-$(uuidgen)" | jq -r .lockToken)

altscore workflows-v2 lock heartbeat my-wf --lock-token "$TOKEN"   # every ~60s during long edits

altscore workflows-v2 autosave <id> --lock-token "$TOKEN" --last-known-version 3 \
  --body '{"label":"Renamed","nodes":[...],"edges":[...]}'

altscore workflows-v2 lock release my-wf --lock-token "$TOKEN"

# Inspect / unstick
altscore workflows-v2 lock get my-wf
altscore workflows-v2 lock force-release my-wf       # admin only
```

`autosave` returning 409 means concurrent modification — re-fetch with `get`, merge, retry.

#### Lifecycle

```bash
altscore workflows-v2 create-draft <active-id>                   # branch off ACTIVE
altscore workflows-v2 publish <draft-id>                         # DRAFT -> ACTIVE
altscore workflows-v2 revert my-wf <version-id>                  # restore prior version into a draft
altscore workflows-v2 revert my-wf <version-id> --mode publish   # replace ACTIVE directly
altscore workflows-v2 archive <id>                               # archive all versions
altscore workflows-v2 restore <id>                               # un-archive
altscore workflows-v2 duplicate <id> --new-label "Copy of X"
```

#### Versions, executions, mappings

```bash
altscore workflows-v2 versions my-wf --include-changes
altscore workflows-v2 get-version my-wf latest
altscore workflows-v2 executions <id> --per-page 20 --sort-by createdAt --sort-direction desc

altscore workflows-v2 update-mapping <id> --node-id score --previous old --new new
altscore workflows-v2 update-mapping <id> --node-id score --previous old --new ""    # clear
altscore workflows-v2 resolve-mappings <id>                                          # auto-wire unresolved
```

#### Schedules

```bash
# Always dry-run cron expressions before saving
altscore workflows-v2 schedule preview --cron "0 9 * * *" --utc-delta -5 --count 5
altscore workflows-v2 schedule validate --cron "0 9 * * MON" --utc-delta -5

altscore workflows-v2 schedule get    <id>
altscore workflows-v2 schedule create <id> --body '{"schedule":{"cron":"0 9 * * *","utcDeltaHours":-5}}'
altscore workflows-v2 schedule update <id> --body '{"scheduleBatch":{"cron":"0 0 * * SUN","utcDeltaHours":0}}'
altscore workflows-v2 schedule delete <id> --individual    # or --batch, or both
```

`utcDeltaHours` accepts -12 to 14.

#### Import / export

```bash
altscore workflows-v2 export <id> > my-wf.json
altscore workflows-v2 validate-rules --body @my-wf.json     # which referenced rules already exist?
altscore workflows-v2 import --body @my-wf.json --new-label "Imported Copy"
altscore workflows-v2 import --body @my-wf.json --new-label "Light" --skip-evaluation-rules --skip-scorecards
```

#### Execute

> **Body shape — sync vs batch differ.** `execute` and `execute-by-alias` take a **flat** object whose keys are the workflow's `inputVariables` directly (`{"borrower_id":"abc"}`). `execute-batch` takes a **wrapped** object (`{"inputs": [{...}, {...}]}`). Wrapping a sync call (`{"inputs": {...}}`) returns HTTP 400 `Required variable '<name>' is missing` because the resolver looks for top-level keys. Despite the runtime variable namespace being `inputs.<name>`, the request body is flat — the namespace is added server-side after parsing.

```bash
altscore workflows-v2 execute <id> --body '{"borrower_id":"abc"}'                          # sync, flat keys
altscore workflows-v2 execute <id> --body '{...}' --execution-mode async --tags smoke      # async returns executionId
altscore workflows-v2 execute-by-alias my-wf latest --body '{...}'

# Test mode -- mark the WHOLE run non-billable + hidden from metrics/default lists.
# --test injects the "test" tag (BC sets is_test=true -> is_billable=false).
# NOTE: side effects (borrower/deal/package writes) STILL run -- it is not a dry run.
altscore workflows-v2 execute <id> --body '{...}' --test
altscore workflows-v2 execute-batch <id> --body '{"inputs":[...]}' --test   # sets testMode=true

# Single-task test harness (test ONE node in isolation) -- distinct from --test:
altscore workflows-v2 execute <id> --body '{...}' \
  --test-task-id <task-id> --test-timeout-seconds 60 --store-logs true

# Batch -- wrapped under "inputs": [...]
altscore workflows-v2 execute-batch <id> --body '{
  "inputs":[{"borrower_id":"a"},{"borrower_id":"b"}],
  "label":"smoke","parallelExecutions":50,"continueOnFailures":true
}'
altscore workflows-v2 batch pause     <batch-id>
altscore workflows-v2 batch continue  <batch-id>
altscore workflows-v2 batch terminate <batch-id>
```

#### Sources and AI helpers

```bash
altscore workflows-v2 sources-status --country ECU --status active
altscore workflows-v2 external-sources-status

altscore workflows-v2 ai suggest-mappings --body '{
  "fields":[{"name":"borrower_id","type":"string"}],
  "availableOutputs":[{"source":"taskOutput","type":"string","taskAlias":"fetch","outputName":"id"}]
}'
```

#### Ergonomic builder helpers (for INCREMENTAL EDIT path)

These mutate an existing workflow in place. Each handles fetch + lock + autosave internally. **Prefer `apply` for any non-trivial change** — apply is declarative, re-runs the full validation pipeline, and reconciles entity scopes; these helpers are best used for one-off tweaks or interactive exploration where you don't want to maintain a spec file.

```bash
TOKEN=$(altscore workflows-v2 lock acquire my-wf --client-id "agent-$$" | jq -r .lockToken)

altscore workflows-v2 add-node <id> --lock-token "$TOKEN" \
  --type http --node-id notify --label "Webhook" --task-alias notify-approve

altscore workflows-v2 add-edge <id> --lock-token "$TOKEN" \
  --source route --target notify --source-handle branch_0

altscore workflows-v2 set-mapping <id> --lock-token "$TOKEN" \
  --node-id notify --input-name decision --expression "task_outputs.score.decision"

altscore workflows-v2 set-variable <id> --lock-token "$TOKEN" \
  --scope input --name escalation_email --type string --required

altscore workflows-v2 lock release my-wf --lock-token "$TOKEN"
```

| Helper | Purpose |
|---|---|
| `add-node <id>` | Append a node. **Reference an existing task with `--task-alias`** (and optional `--task-version`). Field names: `nodeId`, `label`, `taskAlias`, `taskVersion` |
| `remove-node <id>` | Remove by `--node-id` (also drops incident edges unless `--keep-edges`) |
| `add-edge <id>` | Append an edge. `--source` / `--target` get serialized as `sourceNodeId` / `targetNodeId` |
| `remove-edge <id>` | By `--id` or by `--source` + `--target` |
| `set-variable <id>` | `--scope input\|custom`, `--name`, `--type`, `--default <json>`, `--required` |
| `unset-variable <id>` | Remove by `--scope` + `--name` |
| `set-mapping <id>` | Wire a node input: `--node-id`, `--input-name`, `--expression`. Use `--clear` to remove |

Both `--lock-token` (caller-managed) and `--client-id` (auto-acquire/release) are supported.

#### Pre-flight checklist (before constructing or modifying)

The workflow body is permissive — it'll save with bad refs and fail at execute time. Verify external references exist first:

```bash
altscore altdata describe <SOURCE_ID>                        # altdata-enrichment refs (canonical)
altscore evaluators list --filter alias=<ALIAS>              # evaluate-rules refs
altscore data-models list --filter key=<KEY>                 # custom field refs
altscore api GET "/v1/rules?alias=<ALIAS>"                   # rule refs
```

`altdata describe` is the one-shot pre-flight: it returns the source's metadata, available versions, required input fields, and outputSchema keys in a single call. Prefer it over chaining `altdata sources` + `altdata dictionary` + `altdata sample`.

#### Variable resolution syntax (templates and mappings)

The runtime resolver accepts these leading namespaces — anything else fails with `Unknown variable namespace`:

| Scope | Syntax | Example |
|---|---|---|
| Workflow input | `inputs.<name>` | `inputs.borrower_id` |
| Task output (top-level) | `task_outputs.<taskAlias>.<field>` | `task_outputs.fetch.sources_output_packages` |
| Task output (deep path) | `task_outputs.<taskAlias>.<deep>.<path>.<to>.<field>` | `task_outputs.fetch.ECU-PUB-0002.data.pdEc_sri_esActivo` |
| Custom variable | `custom.<name>` | `custom.normalized_score` |
| System | `system.<key>` | `system.execution_id` |
| Indexed by type | `task_outputs_by_type.<taskType>[<idx>].<field>` | `task_outputs_by_type.altdata-enrichment[0].result` |

**Deep paths into altdata output**: an `altdata-enrichment` task outputs the entire package object on `sources_output_packages`. To map a single field into a downstream conditional/compute-variables task, use the deep form: `task_outputs.<altdataAlias>.<sourceId>.data.<fieldName>`. No intermediate compute-variables required.

**Bare `<alias>.<field>` resolves fine** at runtime — the backend resolver accepts the bare-alias form (WITH a dot), and `apply` deliberately emits it for cross-task references. Both `task_outputs.<server-alias>.<rest>` and the bare `<alias>.<rest>` form are valid.

**Multi-dot inputMappings are accepted on create**: a single POST lands the task at version 1, and the workflow node references version 1.

#### Gotchas (v2 specific)

- **Tasks first — for every node.** Even `start`/`end` need a backing `/v2/tasks` record. Hub workflows use trivial type-only tasks for those (`{"type":"start","label":"Start"}`). The API saves orphan-node bodies, but the Hub then hits `GET /v2/tasks/null` 404 on render.
- **Field names are camelCase.** `nodeId` not `id`, `sourceNodeId` not `source`, `targetNodeId` not `target`. Snake-case will return 400.
- **Lock first.** `update` works without a lock but races with the Hub UI. Prefer `autosave` with `--lock-token`.
- **`lastKnownVersion`** is the antidote to silent overwrites. Always pass it on `autosave` if you fetched the workflow earlier in the session.
- **Alias is derived from label on create.** Two workflows can't share an alias — 409 on collision. Use `duplicate --new-label` or `import --new-label` to disambiguate.
- **`schedule preview/validate`** don't take a workflow ID. Standalone cron checkers.
- **`execute --execution-mode async`** returns only `executionId`. Poll `executions <id>` for status.
- **`ai suggest-mappings`** returns 503 when the tenant has no LLM configured. Treat as a soft failure.
- **No tasks LIST endpoint.** Discover task aliases via the workflows that use them, or via the Hub UI.



#### Atomic deal write with customer + N guarantors

A single `deal` task can attach the customer plus an arbitrary number of guarantors atomically. Pattern: a single-mode child-workflow KYCs the customer, a batch child-workflow KYCs the guarantors, a `compute-variables` task assembles the full contacts list, and one `deal` task with an inline `contacts` field writes them all in one shot. (Contacts are attached ONLY via the inline `contacts` field — the legacy `deal_contacts` `sourcesConfig` type is no longer supported and is rejected by `apply`.)

```jsonc
"nodes": [
  // ... start node + any upstream tasks ...
  {"ref": "verify-customer",   "type": "child-workflow", "executorId": "kyc-individual-ar",
   "inputMappings": {"tax_id": "inputs.customer_tax_id"}},

  {"ref": "verify-guarantors", "type": "child-workflow", "runInBatch": true,
   "inputExpression": "inputs.guarantors", "executorId": "kyc-individual-ar"},

  {"ref": "build-contacts",    "type": "compute-variables",
   "selectedVariables": ["all_contacts"]},

  {"ref": "deal", "type": "deal", "operation": "write",
   "lookupBy": "external_id", "key": "external_id",
   "inputSchema": {
     "external_id": {"type": "string", "required": true},
     "label":       {"type": "string"}
   },
   "inputMappings": {
     "external_id": "task_outputs.verify-customer.borrower_id",
     "label":       "inputs.customer_legal_name"
   },
   "sourcesConfig": [
     {"type": "deal_field", "key": "label", "label": "Label"}
   ],
   // Inline contacts: each row references an existing borrower by borrower_id.
   "contacts": [
     {"borrower_id": "{{task_outputs.verify-customer.borrower_id}}",
      "role_key": "customer", "is_primary": true}
     // ... plus one row per guarantor from the batch KYC ...
   ]
  }
  // ... end node ...
]
```

Re-running the deal task with the same `(deal_id, borrower_id, role_key)` triples is idempotent — no duplicate DealContact rows are created.

#### Atomic deal write with borrower upsert (no per-party child needed)

When the deal task should own borrower creation, set `upsertContacts: true` as a top-level field on the deal task (sibling to `contacts`). The deal write becomes the single source of truth for the deal record, its contacts, AND the borrowers being attached: each inline `contacts` row without `borrower_id` is resolved by looking up `(identity_key, identity_value, tenant)`, and a new borrower is created (`persona` REQUIRED — `"individual"` or `"business"`) when no identity matches. KYC/KYB can run AFTER the deal exists as pure scoring (no longer a hard dependency for contact attachment — a failed KYC no longer drops a contact).

```jsonc
"nodes": [
  // ... start node ...
  {"ref": "create-deal", "type": "deal", "operation": "write",
   "lookupBy": "external_id", "key": "external_id",
   "inputSchema": {
     "external_id": {"type": "string", "required": true}
   },
   "inputMappings": {
     "external_id": "inputs.deal.dealId"
   },
   "upsertContacts": true,
   "contacts": [
     {"tax_id": "30-71234567-1", "persona": "business", "label": "Acme SRL",
      "role_key": "customer", "is_primary": true},
     {"tax_id": "20-25678901-3", "persona": "individual",
      "role_key": "guarantor", "is_primary": false}
   ]
  },

  // Optional: KYC/KYB scoring AFTER the deal exists. Children take borrower_id
  // from the freshly-written contacts, so a failed child no longer drops a
  // contact -- it just leaves that party unscored.
  {"ref": "kyc-fan-out", "type": "child-workflow", "runInBatch": true,
   "inputExpression": "inputs.deal.parties[entityType=natural]",
   "executorAlias": "deal-kyc-scoring-v1", "continueOnFailure": true}
  // ... end node ...
]
```

#### Per-item output scoping

A `deal` or `relationships` node that carries **inline items** exposes one output handle per item. A downstream node whose ONLY inbound path is a single item handle runs **scoped** to that one item: inside that node (and anything reached only through it) `task_outputs.<sourceRef>` resolves to that single item's dict instead of the source's parallel arrays. This is how you fan a downstream data-source / rules / scorecard subgraph out across N contacts and have each branch read THAT contact's values.

**Handle naming** (used in the edge's `sourceHandle`):

| Source node | Inline item field | Handle per item |
|---|---|---|
| `relationships` | (relationship items) | `rel-<id>` |
| `deal` | `contacts` | `deal-<id>` |

The `<id>` is the item's `id` within the inline array (e.g. the first item → `deal-0` / `rel-0`).

**Scoped output keys differ per node type** — a common trap. When `task_outputs.<sourceRef>` is scoped to one item, the keys you can read are:

| Scoped source | Keys available on the scoped item dict |
|---|---|
| `relationships` item | `contact_id`, `relationship_id`, `relationship`, `ownership_pct` |
| `deal` contact item | `borrower_id`, `deal_contact_id`, `role_key`, `is_primary` |

> **No `contact_id` on a scoped deal item.** A deal contact item exposes `borrower_id` (the party being attached) and `deal_contact_id` (the DealContact join-row id) — NOT `contact_id`. Reading `task_outputs.<dealRef>.contact_id` off a scoped deal item returns None. Use `borrower_id` for the party and `deal_contact_id` for the join row. (Relationships items are the inverse: they DO carry `contact_id`, plus `relationship_id`.)

**Inline `contacts` field on a deal task** — the field that drives `deal-<id>` handles. Shape:

```jsonc
"contacts": [
  {"id": "0", "borrower_id": "brw_abc", "role_key": "customer",  "is_primary": true},
  {"id": "1", "borrower_id": "brw_def", "role_key": "guarantor", "is_primary": false}
]
```

The **inline `contacts` field is the ONLY supported way to attach deal contacts** — it both persists the DealContact rows and enables `deal-<id>` per-contact scoping. (The legacy `deal_contact` / `deal_contacts` `sourcesConfig` types are no longer supported and are rejected by `apply`.)

**Inline `contacts` upsert — `upsertContacts` on the deal node.** By default each inline `contacts` row must reference an existing borrower via `borrower_id`. Set `upsertContacts: true` as a **top-level field on the deal task** (sibling to `contacts`) to let a row instead identify the borrower by identity — the deal write resolves `(identity_key, identity_value, tenant)` and **creates the borrower first if no identity matches**, then attaches it. This mirrors `relationshipsConfig.upsertContacts` on a `relationships` node and lets a single deal write own borrower creation. Accepted item shapes:

- `{borrower_id, role_key?, is_primary?}` — existing-borrower path; short-circuits the upsert (no identity/persona needed).
- `{tax_id, persona, role_key?, is_primary?}` — shorthand: `tax_id` doubles as identity_key+identity_value. `persona` REQUIRED (`"individual"` or `"business"`).
- `{identity_key, identity_value, persona, role_key?, is_primary?}` — explicit identity. `persona` REQUIRED.

`apply` preflights this like relationships: when `upsertContacts` is off, a row missing `borrower_id` is rejected (with a hint to flip the flag); when on, a row without `borrower_id` must carry an identity (`identity_value`, or `tax_id` / `identity_key` shorthand) AND `persona`. Rows with `borrower_id` are accepted in either mode.

```jsonc
{"ref": "attach-deal", "type": "deal", "operation": "write",
 "lookupBy": "external_id", "key": "external_id",
 "inputSchema": {"external_id": {"type": "string", "required": true}},
 "inputMappings": {"external_id": "inputs.external_id"},
 "upsertContacts": true,
 "contacts": [
   {"borrower_id": "brw_customer", "role_key": "customer",  "is_primary": true},
   {"tax_id": "20-12345678-9", "persona": "business", "role_key": "guarantor"},
   {"identity_key": "email", "identity_value": "co@example.com", "persona": "business", "role_key": "guarantor"}
 ]}
```

**The scoped compute-variables pattern.** Reading a scoped scalar into the rest of the graph takes four pieces:

1. A **source node** (`deal` or `relationships`) with inline items.
2. An **edge** carrying `sourceHandle: "deal-<id>"` (or `"rel-<id>"`) from the source node to a `compute-variables` "probe" node — that single inbound handle is what scopes the probe.
3. A **workflow-level custom variable** whose expression reads a scoped scalar, and a `compute-variables` node that lists it in `selectedVariables`. The expression uses BC's Python DSL (see the customVariables normalization in `apply`): `result = inputs.get("task_outputs.<sourceRef>.deal_contact_id")`. The dependency string and the `inputs.get(...)` key must agree on the alias (`apply` rewrites the spec `ref` to the server alias).
4. **Downstream nodes** (data source, rules, scorecard) read the scoped scalar via `custom.<name>` — each branch gets THAT item's value.

**Worked example** — a deal node with inline `contacts`, a `deal-0` edge to a compute-variables probe, a custom var reading the scoped `deal_contact_id`, and a downstream node consuming the scoped scalar:

```jsonc
{
  "label": "Per-contact scoping demo",
  "alias": "per-contact-scoping-demo",
  "category": "EVALUATION",
  "inputVariables": {"external_id": {"type": "string", "required": true}},
  "customVariables": {
    // Scoped scalar: when "probe" is reached ONLY through the deal-0 handle,
    // task_outputs.<dealRef> is the single contact dict, so this resolves to
    // that contact's join-row id (NOT the parallel array).
    "scoped_deal_contact_id": {
      "type": "string",
      "expression": "result = inputs.get(\"task_outputs.attach-deal.deal_contact_id\")",
      "returnValue": "result",
      "dependencies": ["task_outputs.attach-deal.deal_contact_id"]
    }
  },
  "nodes": [
    {"ref": "start", "type": "start", "label": "Start"},

    {"ref": "attach-deal", "type": "deal", "operation": "write",
     "lookupBy": "external_id", "key": "external_id",
     "inputSchema": {"external_id": {"type": "string", "required": true}},
     "inputMappings": {"external_id": "inputs.external_id"},
     // Inline contacts -> persists DealContact rows AND emits deal-0 / deal-1 handles.
     "contacts": [
       {"id": "0", "borrower_id": "brw_customer",  "role_key": "customer",  "is_primary": true},
       {"id": "1", "borrower_id": "brw_guarantor", "role_key": "guarantor", "is_primary": false}
     ]},

    // Probe node: its ONLY inbound edge is the deal-0 handle, so it (and the
    // custom var it computes) runs scoped to the first contact.
    {"ref": "probe", "type": "compute-variables",
     "selectedVariables": ["scoped_deal_contact_id"]},

    // Downstream consumer reads the scoped scalar via custom.<name> -- it sees
    // the FIRST contact's deal_contact_id, not the whole array.
    {"ref": "score-contact", "type": "evaluate-rules", "evaluatorTask": "contact-scoring",
     "inputSchema": {"deal_contact_id": {"type": "string"}},
     "inputMappings": {"deal_contact_id": "custom.scoped_deal_contact_id"}},

    {"ref": "end", "type": "end", "label": "End"}
  ],
  "edges": [
    {"from": "start", "to": "attach-deal"},
    // sourceHandle scopes the probe to the first inline contact (id "0").
    {"from": "attach-deal", "to": "probe", "sourceHandle": "deal-0"},
    {"from": "probe", "to": "score-contact"},
    {"from": "score-contact", "to": "end"}
  ]
}
```

For a `relationships` source the only changes are: use `sourceHandle: "rel-0"`, and read the relationships scoped keys (`contact_id`, `relationship_id`, `relationship`, `ownership_pct`) instead of the deal ones.

> **Anti-pattern: extraction probes.** The worked example above shows the *mechanics* of scoping, but the cleaner design is that scoped values flow **directly via `inputMappings`** into the nodes that consume them. Do NOT create a `compute-variables` node plus a custom variable whose expression merely extracts a scoped scalar (a pure pass-through like `result = inputs.get("task_outputs.<alias>.<field>")`). A node reachable only through a `rel-<id>`/`deal-<id>` handle already runs scoped, so the consuming node can reference `task_outputs.<alias>.<field>` in its own `inputMappings` and get THAT item's value — the probe node and the custom variable add nothing but indirection. Reserve custom variables for values a **rule or scorecard actually evaluates** (derived/computed figures), not for plain extraction. The cleaner shape for the example above drops the `probe` compute-variables node and its `scoped_deal_contact_id` custom variable, and wires the downstream node directly:
>
> ```jsonc
> // edge: {"from": "attach-deal", "to": "score-contact", "sourceHandle": "deal-0"}
> {"ref": "score-contact", "type": "evaluate-rules", "evaluatorTask": "contact-scoring",
>  "inputSchema": {"deal_contact_id": {"type": "string"}},
>  "inputMappings": {"deal_contact_id": "task_outputs.attach-deal.deal_contact_id"}}
> ```
>
> The CLI flags extraction-probe custom variables with a **non-blocking advisory** (`# advisory: customVariable "<name>" looks like an extraction probe ...`) in both `altscore workflows-v2 lint` and the `apply` preflight. It is advisory only — it never fails `lint` or blocks `apply` — but it points you at the direct-wire fix above.

