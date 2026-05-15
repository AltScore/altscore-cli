---
name: altscore-api
description: "Interact with the AltScore Borrower Central API using the altscore CLI. Use when the user needs to create, read, update, or delete borrowers, identities, documents, deals, or query executions and packages. Also use for raw API calls and profile management."
user-invocable: false
allowed-tools: Bash, Read, Grep, Glob
---

# AltScore CLI -- Agent Reference

You have access to the `altscore` CLI for interacting with the AltScore Borrower Central API. All commands output JSON to stdout and status messages to stderr. Pipe to `jq` for field extraction.

## Prerequisites

Before using this skill, verify `altscore` is installed:

```bash
which altscore
```

If not found, install it:

```bash
gh release download --repo AltScore/altscore-cli --pattern "altscore-$(uname -s | tr '[:upper:]' '[:lower:]')-$(uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/')" --output /usr/local/bin/altscore --clobber
chmod +x /usr/local/bin/altscore
```

## Authentication

The CLI must be logged in before use. Check with:

```bash
altscore config
```

If no profile exists, log in interactively (requires a terminal):

```bash
altscore login
```

Tokens auto-refresh on 401. No manual refresh needed.

### Exporting credentials for the Python SDK

**WARNING:** `altscore env` prints raw secrets (client_secret, access token) to stdout. NEVER run it bare -- ALWAYS pipe to a file:

```bash
# Write current profile credentials to .env
altscore env > .env

# Export a specific profile
altscore env --profile staging > .env.staging
```

Outputs: `ALTSCORE_CLIENT_ID`, `ALTSCORE_CLIENT_SECRET`, `ALTSCORE_USER_TOKEN`, `ALTSCORE_ENVIRONMENT`, `ALTSCORE_TENANT`. These are the env vars the AltScore Python SDK reads.

### Updating the CLI

```bash
altscore update
```

Downloads the latest release from GitHub, verifies the SHA-256 checksum, and replaces the binary in-place. If the repo is private, set `GITHUB_TOKEN` first.

## Test Mode (UAT)

Most entities support a test mode (`isTest`) for UAT data in production. Test records are **excluded from list results by default**.

### Listing test records

```bash
# Include test records alongside real ones
altscore borrowers list --include-tests

# Show only test records
altscore borrowers list --test-only
```

### Creating test records

```bash
# Create a borrower as test
altscore borrowers create --is-test --body '{"persona": "individual", "label": "Test User"}'
```

### Toggling test mode on existing records

```bash
# Mark as test (cascades to children: identities, documents, fields, etc.)
altscore borrowers set-test <id> --enable

# Clear test flag (safe: won't un-toggle children with independent test sources)
altscore borrowers set-test <id> --disable
```

Entities with full test mode (`set-test` + list filters + `--is-test` on create):
  borrowers, identities, documents, deals, assets, borrower-fields, deal-fields,
  asset-fields, points-of-contact, deal-contacts, authorizations, metrics,
  artifacts, data-models, evaluators, evaluation-rules, policy-rules, rule-trees

Entities with filter-only (no `set-test`): executions, execution-batches

## Resource Commands

Every resource supports `--help` which documents request body fields, response fields, and available filters.

### Borrowers

```bash
# List (with filters and pagination)
altscore borrowers list --per-page 10
altscore borrowers list --filter persona=individual --filter risk-rating=A

# Get by ID
altscore borrowers get <id>

# Create (persona is required: "individual" or "company")
altscore borrowers create --body '{"persona": "individual", "label": "Jane Doe"}'

# Update
altscore borrowers update <id> --body '{"label": "New Name", "tags": ["vip"]}'

# Delete
altscore borrowers delete <id>
```

### Identities

Identities are key-value pairs on borrowers (email, tax-id, phone, etc.).

```bash
# List for a borrower
altscore identities list --filter borrower-id=<borrower-id>

# Create (borrowerId and key are required IN THE BODY)
altscore identities create --body '{"borrowerId": "<id>", "key": "email", "value": "j@example.com"}'

# Delete
altscore identities delete <id>
```

### Documents

Documents store structured data on borrowers or deals.

```bash
# List for a borrower
altscore documents list --filter borrower-id=<borrower-id>

# Create (key is required; at least one of borrowerId/dealId required IN THE BODY)
altscore documents create --body '{"borrowerId": "<id>", "key": "financial-statement", "value": {"revenue": 50000}}'

# Upload file attachment to existing document
altscore documents upload <doc-id> --file ./report.pdf

# Delete
altscore documents delete <id>
```

### Deals

```bash
# List
altscore deals list --filter borrower-id=<borrower-id> --filter status=active

# Get by ID
altscore deals get <id>

# Create (label is required)
altscore deals create --body '{"label": "Credit Line Q1", "description": "Working capital"}'

# Update
altscore deals update <id> --body '{"status": "approved", "riskRating": "B+"}'
```

### Executions (read-only)

```bash
# List executions
altscore executions list --filter borrower-id=<id> --filter status=complete
altscore executions get <id>

# Query execution outputs (result data across executions)
altscore executions query-outputs --filter borrower-id=<id>
altscore executions query-outputs --filter workflow-alias=my-workflow --per-page 10

# Get a single execution's output (workflow envelope: w_* keys + customOutput)
altscore executions get-output <execution-id>
altscore executions get-output <execution-id> | jq '.output'
altscore executions get-output <execution-id> | jq '.customOutput'

# Get per-task outputs (intent-driven: tries /state first, falls back to /output customOutput).
# The CLI hides the two-surface mess so you don't have to know which collection answered.
altscore executions state <execution-id>

# Drilldowns differ by source. Check ._source to branch:
#   "_source" absent          -> primary /state hit, drill via state.data_flow
#   "_source" == "output_..."  -> fallback used, drill via customOutput keyed by task TYPE
altscore executions state <execution-id> | jq 'if has("_source") then .customOutput else .state.data_flow.task_outputs end'
altscore executions state <execution-id> | jq '.customOutput.scorecard_result[0]'              # fallback shape
altscore executions state <execution-id> | jq '.state.data_flow.task_states | to_entries | map({k:.key, status:.value.status})'  # primary shape

# Get attachments from an execution output (returns download URLs)
altscore executions get-output-attachments <execution-id>
altscore executions get-output-attachments <execution-id> | jq '.[].url'
```

> **Per-task vs envelope outputs.** v2 declarative tasks (altdata-enrichment, scorecard, rule-tree, mapping-table, evaluate-rules, conditional, http) do NOT write into the top-level `output` envelope. So `executions get-output | jq '.output'` is `null` and `.customOutput` may look empty for a workflow whose tasks didn't explicitly emit `w_*` keys.
>
> The runtime stores per-task results in two different surfaces depending on the engine variant:
> - `GET /v1/executions/{id}/state` — `state.data_flow.task_outputs.<taskAlias>` (alias-keyed). Hub debug panel reads this. Only present for in-flight or state-persisting executions; often 404 for completed sync v2 runs.
> - `GET /v1/executions/{id}/output` — `customOutput.<task_type>_result[]` (type-keyed, arrays). Always present for completed executions.
>
> `altscore executions state` hides this mess: it tries `/state` first, falls back to `/output` on 404, and stamps `_source: "output_customOutput"` on the fallback response so your `jq` knows which shape to expect. Use `--no-fallback` to opt out.

### Packages (read-only)

```bash
altscore packages list --filter alias=credit-report --per-page 5
altscore packages get <id>
```

### Workflow Tasks

Versioned Python functions that are the atomic units of remote-tasks workflow DAGs.

#### Task code structure

```python
class InputData(BaseModel):
    field_name: float
    optional_field: Optional[float] = 0.0

class OutputData(BaseModel):
    result: float

async def execute(input_data: InputData, context: dict) -> OutputData:
    return OutputData(result=input_data.field_name * 2)
```

The code-eval engine provides these as globals -- do NOT import them in task code:
- `BaseModel`, `Field` (from pydantic)
- `Optional`, `List`, `Dict`, `Any` (from typing)
- `alts_acli` / `alts_cli` (AltScore SDK, when SDK is enabled)
- `context` (dict with token, environment, etc.)

The engine handles Pydantic conversion automatically:
- Input: hydrates `InputData(**input_data)` so your function receives a Pydantic model
- Output: calls `.dict()` if you return a BaseModel, so returning `OutputData(...)` works

The `InputData`/`OutputData` classes also serve as schema sources -- validate-code extracts JSON schemas from them.

#### Commands

```bash
# CRUD
altscore workflow-tasks list --per-page 10
altscore workflow-tasks get <id>
altscore workflow-tasks create --body '{"alias": "my-task", "label": "My Task", "code": "..."}'
altscore workflow-tasks update <id> --body '{"code": "..."}'
altscore workflow-tasks delete <id>

# Publish lifecycle
altscore workflow-tasks publish <id> --version 1
altscore workflow-tasks unpublish <id> --version 1
altscore workflow-tasks versions <id>

# Validate code structure and extract schemas
altscore workflow-tasks validate --body '{"code": "..."}'
altscore workflow-tasks validate --body '{"code": "..."}' --update-task --task-alias my-task

# Execute a saved task directly (for testing)
altscore workflow-tasks execute <id> 1 --body '{"inputData": {"x": 5}, "context": {}}'

# Execute inline code without saving
altscore workflow-tasks lambda --body '{"code": "...", "inputData": {"x": 5}, "context": {}}'

# Lock task before updating (required)
altscore api POST /v1/workflow-tasks/commands/get-and-lock --body '{"taskId": "<id>"}'
```

### Task Tests

Persistent test cases attached to workflow tasks. The runner compares actual vs expected output.

```bash
# CRUD (testType is REQUIRED: "unit_test" or "integration_test")
altscore task-tests create --body '{"taskId": "<id>", "name": "test1", "testType": "unit_test", "inputData": {...}, "expectedOutputData": {...}}'
altscore task-tests list --filter taskId=<task-id>
altscore task-tests get <test-id>
altscore task-tests update <test-id> --body '{"expectedOutputData": {...}}'
altscore task-tests delete <test-id>

# Run a single test
altscore task-tests run <test-id> --version 1

# Run all tests for a task
altscore task-tests run-all <task-id> --version 1

# List tests by task (shorthand)
altscore task-tests by-task <task-id>
```

Test inputData field names must match InputData model fields exactly. Cross-reference against the inputSchema from validate.

### Workflows

DAG definitions that orchestrate workflow tasks.

```bash
# CRUD
altscore workflows create --body '{"alias": "my-wf", "version": "v1", "remoteTasks": true, "flowDefinition": {...}}'
altscore workflows list --per-page 10
altscore workflows get <id>
altscore workflows update <id> --body '{"flowDefinition": {...}}'
altscore workflows delete <id>

# Execute by ID (sync by default)
altscore workflows execute <id> --body '{"income": 5000}'
altscore workflows execute <id> --body '{"income": 5000}' --async --tags "test"

# Execute by alias and version
altscore workflows execute-by-alias my-workflow v1 --body '{"income": 5000}'

# Update input schema separately
altscore workflows update-schema <id> --body '{"inputSchema": "{\"type\":\"object\",...}"}'

# Input schema reference guide (live documentation)
altscore workflows input-schema-guide
altscore workflows input-schema-guide fieldTypes
altscore workflows input-schema-guide customTypes
altscore workflows input-schema-guide examples
```

#### Input Schema Reference

The `inputSchema` field on a workflow defines execution input validation. It uses JSON-Schema-like syntax converted to a dynamic Pydantic model at runtime.

**Field types:**

| Type | Pydantic Type | Available Constraints |
|------|--------------|----------------------|
| `string` | `str` | minLength, maxLength, pattern, enum |
| `integer` | `int` | minimum, maximum, enum |
| `number` | `float` | minimum, maximum, enum |
| `boolean` | `bool` | -- |
| `object` | nested BaseModel | recursive properties |
| `array` | `List[item_type]` | recursive items |

**Format validators** (use with `"type": "string"`):

| Format | Pydantic Type | Example |
|--------|--------------|---------|
| `email` | `EmailStr` | `{"type": "string", "format": "email"}` |
| `date` | `date` | `{"type": "string", "format": "date"}` (YYYY-MM-DD) |
| `date-time` | `datetime` | `{"type": "string", "format": "date-time"}` (ISO) |

**Custom regional types** (use as `"type"` value instead of standard types):

| Type | Description |
|------|-------------|
| `ecu_personal_id` | Ecuador cedula (10 digits, checksum) |
| `bra_personal_id` | Brazil CPF (11 digits, double checksum) |

Non-digit characters are stripped before validation.

**Constraints mapping** (JSON Schema key -> Pydantic Field kwarg):

| JSON Schema | Pydantic | Applies To |
|-------------|----------|-----------|
| `minLength` | `min_length` | string |
| `maxLength` | `max_length` | string |
| `pattern` | `regex` | string |
| `minimum` | `ge` | number, integer |
| `maximum` | `le` | number, integer |
| `enum` | `Literal[values]` | string, number, integer |

**UI hints:** `title` (label) and `description` (help text) control form rendering. Always provide in the end-users' language; JSON property keys stay in English.

**`x-ui-widget` extension:** Set `"x-ui-widget": "deal-contact-borrower"` on a string field to render a dropdown of deal contact borrowers instead of a text input. Requires deal context.

**`contact_flags` pattern:** When the schema has an array property named `contact_flags`, the Hub renders a special per-party toggle dialog instead of the standard form. Each deal party gets listed with boolean toggles. Only works for single execution (not batch/Excel).

```json
{
  "type": "object",
  "properties": {
    "deal_id": {"type": "string"},
    "contact_flags": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "borrowerId": {"type": "string"},
          "bureau_a": {"type": "boolean", "title": "Bureau A"},
          "bureau_b": {"type": "boolean", "title": "Bureau B"}
        },
        "required": ["borrowerId"]
      }
    }
  },
  "required": ["deal_id"]
}
```

**Validation endpoints:**
- `POST /v1/input-validation/` -- single JSON validation
- `POST /v1/input-validation/batch/columns` -- Excel column structure check
- `POST /v1/input-validation/batch/rows` -- Excel row-by-row validation
- `POST /v1/input-validation/replace-column-headers` -- remap Excel columns
- `POST /v1/input-validation/generate-sample` -- generate sample Excel from schema

**Batch note:** Arrays and nested objects only work for single execution. Batch Excel expects flat tabular data.

**Example: minimal schema:**
```json
{"type": "object", "properties": {"borrower_id": {"title": "Borrower ID", "type": "string"}}, "required": ["borrower_id"]}
```

**Example: multi-field with constraints:**
```json
{
  "type": "object",
  "properties": {
    "company_id": {"title": "Company ID", "type": "string", "minLength": 5, "maxLength": 20},
    "industry": {"title": "Industry", "type": "string", "enum": ["retail", "manufacturing", "services"]},
    "revenue": {"title": "Revenue", "type": "number", "minimum": 0},
    "start_date": {"title": "Start Date", "type": "string", "format": "date"},
    "is_active": {"title": "Active", "type": "boolean"}
  },
  "required": ["company_id", "industry", "revenue"]
}
```

#### DAG data flow rules

Each task receives the **merged dict outputs of its direct parents only** (`dict.update()` in edge order). A task does NOT automatically see the original workflow input unless `workflow_args` is wired as a direct parent.

Wrong -- linear chain loses workflow input fields:
```
workflow_args -> task_A -> task_B
```
task_B only sees task_A's output. Original workflow fields are lost.

Correct -- multi-parent merge:
```json
"workflow_args": {"type": "workflow_args", "to": {"task_A": {}, "task_B": {}}, "dynamic": true},
"task_A": {"type": "task-alias-a", "to": {"task_B": {}}},
"task_B": {"type": "task-alias-b", "to": {}}
```
task_B sees workflow input fields merged with task_A's output. task_A's output wins on key collision.

**Rule of thumb:** For each task, ask "does this task need fields that only exist in the original input?" If yes, add `workflow_args` as a parent.

### Workflows V2 (Visual Builder)

> **⚠️ STOP — read this before doing anything.**
>
> If the user asks "create a v2 workflow that does X", run **`altscore workflows-v2 compose`** with a single spec file. Do not call `workflows-v2 create` directly with hand-built nodes — that path produces orphan nodes (no `taskAlias`) that save successfully but break the Hub UI (`GET /v2/tasks/null` 404 for every node). The CLI now rejects orphan-node bodies at write time with an error pointing at compose; if you see that error, you're on the wrong path — switch to compose.
>
> Compose is the **only** recommended greenfield path. Direct `create` is for special cases where you've already created the tasks via `tasks-v2 create` and assembled a body with proper `taskAlias` references on every non-start/non-end node.
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
> 2. **`altdata-enrichment` tasks need `inputKeys` to wire source-required fields.** Each source (e.g. `ECU-PUB-0002`) declares `inputFields` like `personId`, `taxId`. The task must include `inputKeys: {"personId": "{{personId}}", "taxId": "{{taxId}}"}` matched against an `inputSchema` that declares those keys, plus `dataAge` (cache TTL minutes, default 30) and `packageAlias` (where to store results) on each `sourcesConfig` entry. Compose auto-derives `inputKeys` by querying `sources-status` for each source's `inputFields` — use it.
>
> Run `altscore workflows-v2 schema-guide conditions` and `... schema-guide tasks` for the canonical reference.

Workflows V2 is the API surface for the visual graph builder in the Hub. It uses **two collaborating resources**:

| Resource | Path | Purpose |
|---|---|---|
| **Tasks** | `/v2/tasks` | Versioned executable units (HTTP url, evaluator alias, sources_config, branches, ...) |
| **Workflows** | `/v2/workflows` | The graph: `nodes[]` + `edges[]` + variables. Each node references a task by `taskAlias` |

This is **not** v1 (`/v1/workflows`). Use v2 for anything created in the visual editor.

**Key insight: tasks first, then workflow.** **Every** graph node — including `start`, `end`, and `conditional` — needs a `taskAlias`. The Hub creates trivial backing tasks (just `type` + `label`) for start/end so it can render them. Verified by inspecting working tenant workflows: every node has a non-null `taskAlias`. Use `compose`, which creates a backing task for every node automatically.

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

#### Recommended path: `compose` (one-shot greenfield)

For "Create a workflow that does X", use `workflows-v2 compose`. It takes a single spec, calls `/v2/tasks` for each task (server picks the alias), then creates the workflow with nodes auto-wired to those server-assigned aliases. Use `--dry-run` first to inspect what will be sent.

**Refs vs aliases.** The spec uses `ref` as a stable spec-local key. Edges and inputMappings reference tasks by `ref`, and compose rewrites them with the server-assigned aliases at create time. You can also still pass an explicit `alias` if you need a specific name (e.g. for cross-workflow reuse) — compose passes that through. Edges use `from`/`to` as shortcuts for `sourceNodeId`/`targetNodeId`.

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
  "tasks": [
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
    }
  ],
  "extraNodes": [
    {"ref": "start", "type": "start", "label": "Start"},
    {"ref": "end",   "type": "end",   "label": "End"}
  ],
  "edges": [
    {"from": "start", "to": "fetch"},
    {"from": "fetch", "to": "score"},
    {"from": "score", "to": "route"},
    {"from": "route", "to": "end", "sourceHandle": "branch_0"}
  ]
}
EOF

altscore workflows-v2 compose --body @/tmp/spec.json --dry-run   # inspect first
altscore workflows-v2 compose --body @/tmp/spec.json --publish   # create + publish (the one you usually want)
altscore workflows-v2 compose --body @/tmp/spec.json             # create only -- leaves workflow in DRAFT
altscore workflows-v2 publish <id>                                # standalone publish step
```

> **DRAFT trap.** Compose creates workflows in `status: "DRAFT"` by default — mirrors the Hub's "save-then-publish" editor flow. **A DRAFT workflow executes successfully but the engine skips every node**: `executions get` returns `status: complete, isSuccess: true`, the envelope output is `null`, and per-task outputs (`executions state <id> | jq '.state.data_flow.task_outputs'`) is `{}`. This looks like the workflow ran when nothing actually happened. Always pass `--publish` (or run `altscore workflows-v2 publish <id>` after) before executing. The CLI's `workflows-v2 execute` now does a pre-flight check and warns to stderr if the workflow isn't `ACTIVE`; pass `--skip-status-check` to suppress.

If the agent asks for help building a workflow, default to compose. Other paths exist for special cases:

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
```

There is **no LIST endpoint** for tasks today. Discover task aliases via the workflows that use them.

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
- `{type: "deal_contact", key: "<context_key>", label: "..."}` — write a single DealContact row from `context[<key>]` (one contact dict).
- `{type: "deal_contacts", key: "<context_key>", label: "..."}` — **plural** form: `context[<key>]` is a list of contact dicts and one DealContact row is created per item. Same idempotency rules apply (see Gotchas).
- `{type: "identity", key: "<identity_key>"}` / `{type: "borrower_field", key: "..."}` for customer/asset writes.

#### Workflow CRUD

```bash
altscore workflows-v2 list --filter status=ACTIVE --filter is-latest=true
altscore workflows-v2 get  <id>
altscore workflows-v2 lint <id>                                    # check for orphan nodes / dangling edges
altscore workflows-v2 update <id> --body '{"label":"Renamed"}'     # prefer autosave for graph edits
altscore workflows-v2 delete <id>

# Direct create is gated by client-side validation -- prefer compose for greenfield.
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

# Test mode
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

These mutate an existing workflow in place. Each handles fetch + lock + autosave internally. **Do not use these for greenfield** — use `compose` instead.

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

**Bare `<alias>.<field>` is broken** at runtime — Pydantic accepts it, but the resolver rejects `<alias>` as an unknown namespace. Always prefix `task_outputs.` for cross-task references. Compose now does this automatically when you write a ref-prefixed mapping; the rewriter outputs `task_outputs.<server-alias>.<rest>`.

**CreateTaskV2 vs CreateTaskVersionV2**: the strict initial-create validator rejects multi-dot mapping values when `inputSchema` is set. The lenient version-bump validator accepts them. **Compose's two-phase create handles this**: it strips `inputMappings`/`inputSchema` from the first POST, then re-posts the full body to `/v2/tasks/{alias}` to land at version 2. The workflow node references `taskVersion: 2` automatically.

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


### Credit Decisioning

Four entity types power the credit-decisioning v2 task surface. They live at `/v1/{evaluation-rules, mapping-tables, scorecards, rule-trees}` and are referenced by alias from v2 tasks. All four have full CRUD + `import` extras; scorecards add `usage`; evaluation-rules add `history`.

> **`workflowAlias` is load-bearing — set it on every entity.**
> The v2 builder filters its rule / rule-tree / mapping-table / scorecard pickers by `workflowAlias`. An entity created without one is invisible to that workflow, even though the entity itself is fine. Always pass `--workflow-alias <alias>` (matches the workflow's `alias`) on `create`, `update`, and `import`. The CLI prints a stderr warning on `create` if neither the flag nor a body field sets it.
>
> **The workflow's alias is server-derived from its label** — `"Customer Onboarding"` slugifies to `customer-onboarding`, `"All 5 types"` to `all-5-types`. The body's `alias` field is silently dropped on `workflows-v2 create`. A common trap: stamping entities with a guess like `customer-onboarding-v1` when the workflow's actual alias becomes `customer-onboarding-v-1`. Run `altscore workflows-v2 compose --body @spec.json --dry-run` first — it prints the predicted alias up front and tells you what to pass to `--workflow-alias` on entity creates. Or compute it locally: lowercase, replace non-`[a-z0-9]+` with `-`, collapse repeated `-`, trim, cap at 100 chars.

#### Mapping tables — `mapping-tables`

```bash
altscore mapping-tables list --filter mapping-type=numerical
altscore mapping-tables get <id>
altscore mapping-tables create --workflow-alias underwriting-v1 --body '{
  "label": "Score band to risk tier",
  "code": "score_to_tier",
  "mappingType": "numerical",
  "outputType": "string",
  "buckets": [
    {"order": 0, "label": "Excellent",  "lowerLimit": 750, "upperLimit": 999, "lowerInclusive": true,  "upperInclusive": true,  "outputValue": "A"},
    {"order": 1, "label": "Good",       "lowerLimit": 700, "upperLimit": 749, "lowerInclusive": true,  "upperInclusive": true,  "outputValue": "B"},
    {"order": 2, "label": "Subprime",   "lowerLimit": 0,   "upperLimit": 699, "lowerInclusive": true,  "upperInclusive": true,  "outputValue": "D"}
  ],
  "defaultValue": "D"
}'
altscore mapping-tables import --body @bundle.json --workflow-alias underwriting-v1
```

`mappingType: "categorical"` swaps `lowerLimit/upperLimit` for `values: [...]` per bucket.

#### Scorecards — `scorecards`

A scorecard is a list of rules; **each rule must link to a `/v1/mapping-tables` entity** (`mappingTableCode`). Buckets on the rule are NOT a substitute — the runtime reads buckets from the linked mapping table. So: create the mapping tables first, then the scorecard.

```bash
altscore scorecards usage <id>          # before deleting / refactoring -- shows workflow refs
altscore scorecards create --workflow-alias underwriting-v1 --body '{
  "label": "Credit base score",
  "code": "credit_base",
  "rules": [
    {"order": 0, "label": "Active",
     "field": "is_active", "fieldType": "categorical",
     "maxPoints": 100,
     "mappingTableCode": "active_to_points"},
    {"order": 1, "label": "Debt",
     "field": "firm_debt", "fieldType": "numerical",
     "maxPoints": 200,
     "mappingTableCode": "debt_band_to_points"}
  ]
}'
```

The Hub auto-slugs `code` from `label` (`label.toLowerCase().replace(/[^a-z0-9]/g,'_').slice(0,50)`); the CLI passes `code` through verbatim. If you submit a rule without a mappingTable reference, the runtime fails with `Rule '<label>' must be linked to a mapping table` — there's no inline-buckets shortcut.

#### Evaluation rules — `evaluation-rules`

```bash
altscore evaluation-rules history <id>            # version history
altscore evaluation-rules import --body @rules.json --workflow-alias underwriting-v1
altscore evaluation-rules create --workflow-alias underwriting-v1 --body '{
  "label": "Active SRI taxpayer required",
  "code": "ecu_active_taxpayer",
  "description": "Borrower must have an active RUC",
  "conditions": {
    "operator": "AND",
    "items": [
      {"field": "is_active", "operator": "eq", "value": "1", "valueType": "value"}
    ]
  },
  "alertLevel":   2,
  "alertMessage": "Borrower has an inactive RUC",
  "decisionKey":  "reject"
}'
```

`conditions` is a `ConditionGroup` — same shape used by v2 conditional task branches. Run `altscore workflows-v2 schema-guide conditions` for the full operator vocabulary (`eq`, `neq`, `gt`, `gte`, `lt`, `lte`, `contains`, `startsWith`, `endsWith`, `in`, `notIn`, `between`, `isNull`, `isNotNull`, `isAltdata*`, `arrayContainsAny`, `arrayContainsAll`).

**`alertLevel` and `decisionKey` are load-bearing.** They look optional in the schema, but the runtime treats absence as silent skip:
- A rule that hits but has no `alertLevel` produces NO entry in the `evaluate-rules` task's `alerts[]` output. Set `alertLevel` (1=info, 2=warning, 3=critical) when you want the alert recorded.
- A rule referenced from a `rule-tree` task with no `decisionKey` leaves the rule-tree's `outputVariable` null even when conditions match. Set `decisionKey` (e.g. `"approve"`, `"reject"`) to populate the decision.

**`decisionKey` must match a tenant-registered decision** (case-sensitive). See the Decisions section below — `altscore decisions list` shows the valid set; `altscore decisions create --key reject --label Reject` registers a new one. Using an unregistered key (or a case mismatch like `"REJECTED"` when the tenant has lowercase `reject`) makes downstream `executions set-decision` calls fail with HTTP 400 `"key not found for entity type: decision"`, and the rule-tree's outputVariable carries a value the platform can't write back as a final decision.

#### Decisions — `decisions`

Decision keys (`approve`, `reject`, `manual_review`, ...) are stored as data-model records with `entityType=decision`. The CLI exposes them as their own group; under the hood it's a thin facade over `/v1/data-models?entity-type=decision` so anything you do here is interchangeable with `data-models` commands using that filter.

```bash
# Discover the tenant's decision vocabulary BEFORE writing decisionKey on rules
altscore decisions list

# Register a new decision key
altscore decisions create --key manual-review --label "Manual review"

# Inspect the decision an execution recorded
altscore executions get-decision <execution-id>

# Override or set the decision on an execution (--type defaults to "final")
altscore executions set-decision <execution-id> --key reject --type final --label "Inactive borrower"

# Admin-only clear (does NOT replay the workflow)
altscore executions delete-decision <execution-id>
```

`decisionType` is `"preliminary"` (interim, can be overridden later in the same run) or `"final"` (default, what the rule-tree task usually emits). The endpoint records each write into a `history` array — read with `executions get-decision` to see the audit trail. Submitting a `key` not registered in the tenant's decision data-models returns HTTP 400 `"key not found for entity type: decision"`.

#### Rule trees — `rule-trees`

```bash
altscore rule-trees create --workflow-alias underwriting-v1 --body '{
  "label": "Credit decision tree",
  "code": "credit_decision_tree",
  "description": "Ordered evaluation of approve/reject rules",
  "rules": [
    {"ruleCode": "ecu_active_taxpayer", "order": 0, "isDefault": false},
    {"ruleCode": "ecu_no_firm_debt",    "order": 1, "isDefault": false}
  ]
}'
altscore rule-trees import --body @rule-trees.json --workflow-alias underwriting-v1
```

References evaluation rules by id and/or code in a specific order with an `isDefault` marker.

#### Building a workflow that uses them (compose)

The four matching v2 task types — `evaluate-rules`, `mapping-table`, `scorecard`, `rule-tree` — reference these entities. Compose validates references against the tenant (best-effort warnings) and pre-fills `outputSchema` with the canonical runtime fields so downstream tasks see the right available outputs.

```bash
cat > /tmp/credit-spec.json <<'EOF'
{
  "label": "Credit decisioning pipeline",
  "category": "EVALUATION",
  "inputVariables": {"borrower_id": {"type": "string", "required": true}},
  "tasks": [
    {"ref": "fetch", "label": "Fetch bureau", "type": "altdata-enrichment",
     "borrowerIdField": "personId",
     "sourcesConfig": [{"sourceId": "ECU-PUB-0002", "version": "v2"}],
     "inputMappings": {"personId": "inputs.borrower_id"}},

    {"ref": "rules", "label": "Evaluate Rules", "type": "evaluate-rules",
     "rulesConfig": [{"ruleCode": "ecu_active_taxpayer"}, {"ruleCode": "ecu_no_firm_debt"}]},

    {"ref": "score", "label": "Credit Score", "type": "scorecard",
     "scorecardConfig": {
       "scorecardCode":      "credit_base",
       "totalScoreVariable": "credit_score",
       "breakdownVariable":  "score_breakdown"
     }},

    {"ref": "tree", "label": "Decision Tree", "type": "rule-tree",
     "ruleTreeConfig": {"ruleTreeCode": "credit_decision_tree",
                        "outputVariable": "decision", "outputType": "string"}}
  ],
  "extraNodes": [
    {"ref": "start", "type": "start", "label": "Start"},
    {"ref": "end",   "type": "end",   "label": "End"}
  ],
  "edges": [
    {"from": "start", "to": "fetch"},
    {"from": "fetch", "to": "rules"},
    {"from": "rules", "to": "score"},
    {"from": "score", "to": "tree"},
    {"from": "tree",  "to": "end"}
  ]
}
EOF
altscore workflows-v2 compose --body @/tmp/credit-spec.json --dry-run
altscore workflows-v2 compose --body @/tmp/credit-spec.json
```

Compose auto-fills:
- `evaluate-rules`: `outputSchema = {alerts: array, alerts_count: integer}`
- `mapping-table`: each `entries[].outputVariable` becomes a top-level output field (string by default; refined to `number` when the referenced mapping table's `outputType` is number). **Compose also mints a UUID for each `entries[].id`** — the runtime requires it but agents typically forget.
- `scorecard`: `outputSchema = {<totalScoreVariable>: number, <breakdownVariable>: object}` (breakdown defaults to `score_breakdown`)
- `rule-tree`: `outputSchema = {<outputVariable>: <outputType>}`

#### Common pitfalls (credit-decisioning specific)

- **`workflowAlias` decides picker visibility.** The Hub builder filters its rule / rule-tree / mapping-table / scorecard pickers by `workflowAlias` matching the workflow. Entities created without it exist on the tenant but are invisible to that workflow. Always pass `--workflow-alias <alias>` (matching the workflow's `alias`) to `create`, `update`, and `import`. To re-scope an existing entity: `altscore <resource> update <id> --workflow-alias <alias>`.
- **All four task types are reference-only.** `evaluate-rules` → `rulesConfig: [{ruleCode}]`, `mapping-table` → `mappingTableConfig.entries[].mappingTableCode`, `scorecard` → `scorecardConfig.scorecardCode`, `rule-tree` → `ruleTreeConfig.ruleTreeCode`. Inline rule/scorecard/table definitions on the task body are silently ignored at runtime; create the entity via the matching CRUD command first, then reference by code in compose.
- **Scorecard rules require a mapping table per rule.** When you `altscore scorecards create`, every entry in `rules[]` must include `mappingTableCode` (or `mappingTableId`). Buckets on the rule are NOT a substitute — the runtime reads buckets from the linked mapping table and fails with `Rule '<label>' must be linked to a mapping table` otherwise.
- **`alertLevel` is required to produce alerts.** A matching `evaluate-rules` rule with no `alertLevel` set produces nothing in the task's `alerts[]` output. Set `alertLevel: 1|2|3` (with optional `alertMessage`) on the rule when alerts are wanted.
- **`decisionKey` drives `rule-tree` output.** The first matching rule's `decisionKey` becomes the rule-tree task's `outputVariable` value. Without `decisionKey` on the rule, the rule-tree's decision is null even on a hit.
- **`decisionKey` is case-sensitive and must match a tenant-registered decision.** Run `altscore decisions list` before writing rules; case mismatches (e.g. `"REJECTED"` when the tenant has `"reject"`) compose and lint clean but fail downstream when the run tries to record the decision via `/v1/executions/{id}/decisions` ("key not found for entity type: decision").
- **`rule-tree` `outputVariable` becomes a top-level task output.** Downstream conditionals reference `task_outputs.<rule-tree-alias>.<outputVariable>` (e.g. `task_outputs.tree.decision`). `outputType` must be `string` | `number` | `boolean`.
- **`is-test` toggles on evaluation rules / rule trees** isolate them from production execution. Use `altscore evaluation-rules set-test <id> --enable` while iterating, then `--disable` once stable.

#### Atomic deal write with customer + N guarantors

A single `deal` task can attach the customer plus an arbitrary number of guarantors atomically. Pattern: a single-mode child-workflow KYCs the customer, a batch child-workflow KYCs the guarantors, a `compute-variables` task assembles the full contacts list, and one `deal` task with a `deal_contacts` (plural) source entry writes them all in one shot.

```jsonc
"tasks": [
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
     "label":       {"type": "string"},
     "contacts":    {"type": "object"}
   },
   "inputMappings": {
     "external_id": "task_outputs.verify-customer.borrower_id",
     "label":       "inputs.customer_legal_name",
     "contacts":    "task_outputs.build-contacts.all_contacts"
   },
   "sourcesConfig": [
     {"type": "deal_field",    "key": "label",    "label": "Label"},
     {"type": "deal_contacts", "key": "contacts", "label": "Contacts"}
   ]
  }
]
```

Re-running the deal task with the same `(deal_id, borrower_id, role_key)` triples is idempotent — no duplicate DealContact rows are created.


## AltData

Discovery commands query Borrower Central (work in all environments). Execution commands hit the AltData module (production only).

### Discovery flow (canonical order)

> **Use `describe` as the pre-flight.** It is the one-shot primitive: hits sources-status once, auto-resolves the latest version, and returns metadata + versions + inputFields + outputKeys in a single JSON document. Reach for `dictionary` / `sample` / `sources` only when you need something `describe` doesn't surface.
>
> **If `altscore altdata describe` says "unknown command"** your installed CLI is older than the describe primitive. Build and install the latest from the repo:
> ```bash
> cd <path-to-altscore-cli> && go build -buildvcs=false -o "$(which altscore)" .
> ```
> Then re-run. Until you upgrade, fall back to `altscore altdata sources --per-page 200` (filter client-side via `jq 'select(.sourceId == "<X>")'`) + `altscore altdata dictionary <X> <ver>` (latest is auto-resolved when omitted).

> **Valid `--filter` keys for sources are `country`, `status`, and `search` only.** Mirrors the Hub's `useAltDataSources` hook (`altscore-ai-chat/lib/hooks/use-altdata-sources.ts`) which sends `?country=<csv>&locale=<en|es>` and nothing else. Filtering by `sourceId` doesn't work — the backend silently ignores unknown filter keys and returns the full catalog. For a single source use `altdata describe <id>` (which does its own client-side narrowing).

```bash
# 1. Find candidates (default --per-page is 200, returns the full ~170-source catalog in one call)
altscore altdata sources --filter search="credit"
altscore altdata sources --filter country=USA --filter status=active

# 2. Pre-flight a candidate (the canonical step before composing)
altscore altdata describe USA-PUB-0001                       # auto-resolves latest version
altscore altdata describe USA-PUB-0001 --version v1          # pin a specific version
altscore altdata describe USA-PUB-0001 | jq '{inputFields, outputKeys, latestVersion}'

# 3. Drill into specifics only if needed
altscore altdata dictionary USA-PUB-0001                     # field defs (latest version auto-resolved)
altscore altdata dictionary USA-PUB-0001 v1                  # pin version
altscore altdata sample USA-PUB-0001                         # example output (latest)
altscore altdata sample USA-PUB-0001 v1
altscore altdata search "credit score"                       # cross-source field search
altscore altdata search "address" --locale es
```

**Anti-patterns (avoid):**
- Walking pages of `altscore altdata sources` to inspect a single source — use `describe`.
- `altscore altdata sources --filter sourceId=<X>` — silently returns the full catalog (the backend ignores unknown filter keys). Use `describe <X>` instead.
- Calling `dictionary` or `sample` without a version — both now auto-resolve the latest, no need to chain a separate sources call first.
- Using `workflows-v2 sources-status` for general discovery — it's the same endpoint as `altdata sources` but lives under workflows-v2 because compose-time normalization needs it; for agents browsing the catalog, prefer `altdata sources` / `altdata describe`.

### Data Requests (production only)

```bash
# Synchronous request (blocks until complete)
altscore altdata request-sync --body '{
  "personId": "borrower-123",
  "sourcesConfig": [{"sourceId": "USA-PUB-0001", "version": "v1"}]
}'

# Asynchronous request (returns requestId immediately)
altscore altdata request-async --body '{
  "personId": "borrower-123",
  "sourcesConfig": [{"sourceId": "USA-PUB-0001", "version": "v1"}]
}'

# Check async request status
altscore altdata request-status <request-id>

# Collect completed request data
altscore altdata request-collect <request-id>
```

## Raw API Escape Hatch

For endpoints not covered by resource commands:

```bash
altscore api GET /v1/borrowers/<id>/summary
altscore api POST /v1/some/endpoint --body '{"key": "value"}'
altscore api GET /v1/content --module cms
```

Modules: `borrower_central` (default), `cms`, `altdata`.

## Key Patterns

- **Body input**: `--body '{...}'` or pipe from stdin: `echo '{}' | altscore borrowers create`
- **Filters**: `--filter key=value` (repeatable). Run `<resource> list --help` to see available filter keys.
- **Pagination**: `--per-page N --page N`
- **Profiles**: `--profile <name>` switches context. `altscore profiles list` shows all.
- **Verbose**: `--verbose` prints HTTP method, URL, status to stderr.
- **All JSON to stdout**: Safe for `| jq`, `> file.json`, etc.

## Schema Introspection

Before writing code that reads or writes BC entities, query the schema registry:

```bash
altscore schema                              # list all resources
altscore schema borrowers                    # full schema (create + update + response + filters)
altscore schema borrowers --action create    # just create body fields
altscore schema identities --action response # identity response shape
```

This returns exact JSON schemas with field names, types, required/optional, and aliases.

## AltScore Python SDK (available inside workflow tasks)

When a workflow task executes, the SDK is pre-initialized and available as global variables:
- `alts_acli` -- async client (use this in async execute functions)
- `alts_cli` -- sync client

Access pattern: `bc = alts_acli.borrower_central`

### Universal CRUD methods (every module)

All modules under `bc = alts_acli.borrower_central` have these methods:

| Method | Signature | Returns |
|--------|-----------|---------|
| `create` | `create(data: dict) -> str` | Resource ID |
| `retrieve` | `retrieve(resource_id: str) -> Resource or None` | Single resource |
| `query` | `query(**filters) -> List[Resource]` | Paginated list |
| `patch` | `patch(resource_id: str, data: dict) -> str` | Resource ID |
| `delete` | `delete(resource_id: str) -> None` | Nothing |
| `retrieve_all` | `retrieve_all(**filters) -> List[Resource]` | All pages auto-fetched |

IMPORTANT: The method is `retrieve`, not `get`. The method is `query`, not `list` or `find`.

Query filter kwargs use snake_case in Python. They are converted to dash-case for the API automatically (e.g., `borrower_id=` becomes `?borrower-id=`).

### Available modules

Access via `alts_acli.borrower_central.<module>`:

| Module | Description |
|--------|-------------|
| `borrowers` | Borrower profiles |
| `identities` | Identity key-value pairs (email, tax-id, phone) |
| `documents` | Structured data documents |
| `deals` | Loan applications / credit facilities |
| `assets` | Collateral / financed items |
| `addresses` | Physical addresses |
| `points_of_contact` | Contact methods |
| `executions` | Workflow execution records |
| `alerts` | Policy alerts |
| `rules` | Policy rules |
| `policies` | Policy definitions |
| `data_models` | Schema definitions |
| `borrower_fields` | Custom borrower fields |
| `metrics` | Borrower metrics |
| `store_packages` | Data packages (enrichment results, stored data) |
| `workflows` | Workflow definitions |
| `forms` | Onboarding forms |

### Borrower resource methods (most commonly used)

After retrieving a borrower: `borrower = await bc.borrowers.retrieve(borrower_id)`

| Method | Returns | Description |
|--------|---------|-------------|
| `get_identities(**kwargs)` | `List[Identity]` | Kwargs: key, priority |
| `get_identity_by_key(key)` | `Identity or None` | Single identity by key |
| `get_documents(**kwargs)` | `List[Document]` | Kwargs: key |
| `get_document_by_key(key)` | `Document or None` | Single document by key |
| `get_addresses(**kwargs)` | `List[Address]` | All addresses |
| `get_main_address()` | `Address or None` | Highest priority |
| `get_points_of_contact(**kwargs)` | `List[PoC]` | Kwargs: contact_method |
| `get_main_point_of_contact(method)` | `PoC or None` | e.g., "email" |
| `get_borrower_fields(**kwargs)` | `List[Field]` | Custom fields |
| `get_borrower_field_by_key(key)` | `Field or None` | Single field by key |
| `get_metrics(**kwargs)` | `List[Metric]` | Borrower metrics |
| `get_metric_by_key(key)` | `Metric or None` | Single metric by key |
| `get_executions(**kwargs)` | `List[Execution]` | Workflow runs |
| `get_alerts(**kwargs)` | `List[Alert]` | Policy alerts |
| `get_risk_rating()` | `RiskRating` | Current risk rating |
| `set_risk_rating(rating, ref_id=None)` | `None` | Update risk rating |
| `get_stage()` | `Stage` | Current lifecycle stage |
| `set_stage(stage, ref_id=None)` | `None` | Update stage |
| `set_flag(flag, ref_id=None)` | `None` | Set borrower flag |
| `set_label(label)` | `None` | Update display name |
| `map_identities_and_fields_onto_dict(mapping)` | `dict` | Map identity/field keys to flat dict. Mapping: `{"out_key": "identity.key_name"}` or `{"out_key": "field.key_name"}` |

### AltData (external data enrichment)

Access via `alts_acli.altdata`:

```python
# InputKeys and SourceConfig are available from the SDK
# Synchronous request (blocks until data returns)
result = await alts_acli.altdata.requests.new_sync(
    input_keys=InputKeys(person_id=borrower_id, tax_id="123456789"),
    sources_config=[
        SourceConfig(source_id="USA-PUB-0001", version="v1"),
        SourceConfig(source_id="USA-PUB-0014", version="v1"),
    ]
)

# Async request (fire and check later)
async_req = await alts_acli.altdata.requests.new_async(
    input_keys=InputKeys(person_id=borrower_id),
    sources_config=[SourceConfig(source_id="USA-PUB-0001", version="v1")]
)
# Later:
result = await async_req.pull()
```

InputKeys fields: person_id, name, first_name, last_name, birth_date, phone, email, address, tax_id, business_id, and more. Use `altscore schema` to verify available fields.

### Example: KYC task using the SDK

```python
class InputData(BaseModel):
    borrower_id: str

class OutputData(BaseModel):
    borrower_label: str
    identity_keys: List[str]
    missing_required: List[str]
    risk_flags: List[str]

async def execute(input_data: InputData, context: dict) -> OutputData:
    bc = alts_acli.borrower_central
    borrower = await bc.borrowers.retrieve(input_data.borrower_id)

    identities = await borrower.get_identities()
    identity_keys = [i.data.key for i in identities]

    required = ["tax-id", "email", "phone"]
    missing = [k for k in required if k not in identity_keys]

    risk_flags = []
    if missing:
        risk_flags.append("incomplete-identity")
    if borrower.data.flag == "review":
        risk_flags.append("manual-review-flagged")

    return OutputData(
        borrower_label=borrower.data.label,
        identity_keys=identity_keys,
        missing_required=missing,
        risk_flags=risk_flags
    )
```

## SDK Macros

Pre-built high-level operations available on `alts_acli.macros` (async) / `alts_cli.macros` (sync). Prefer macros over raw SDK calls for common patterns.

### `macros.find_or_create_borrower`

Idempotent borrower lookup/creation. Queries identities by key+value; creates borrower + identity if not found.

```python
result = await alts_acli.macros.find_or_create_borrower(
    identity_key="person_id",
    identity_value="1234567890",
    persona="individual",      # only used on creation
    label=None,                # defaults to identity_value
)
# {"borrower_id": "abc-123", "created": False}
```

### `macros.enrich_borrower`

Full AltData enrichment cycle with freshness caching. For each source: checks if a fresh package exists, calls AltData for stale/missing ones, stores results as borrower packages.

```python
enrichment = await alts_acli.macros.enrich_borrower(
    borrower_id="abc-123",
    sources=[
        {"sourceId": "ECU-PUB-0002", "version": "v1"},
        {"sourceId": "ECU-PUB-0004", "version": "v1"},
    ],
    input_keys={"personId": "1234567890"},
    data_age_minutes=360,      # skip if fresh package exists
    timeout_seconds=120,
)
# {
#   "source_results": [{"source_slug": "AD_ECU-PUB-0002_v1", "package_id": "...", "status": "created"}, ...],
#   "all_sources_ok": True,
#   "sources_created": 2,
#   "sources_fresh": 0,
#   "sources_failed": 0,
# }
```

### `macros.evaluate`

Run an evaluator (rule engine) with a simplified interface. Builds the EvaluatorInput internally, handles serialization, and returns a plain dict.

```python
result = await alts_acli.macros.evaluate(
    evaluator_alias="scoring",
    evaluator_version="v3",
    reference_id=borrower_id,        # goes into instance.referenceId
    data={                            # the variables the evaluator evaluates
        "equifax_score": 750,
        "sri_debt_indicator": 0,
        "days_since_first_sale": 365,
    },
    entities=[],                      # co-debtors/guarantors (optional, default [])
    execution_id=context.get("execution_id"),  # ties evaluation to the workflow execution
)
```

**Return value** -- a dict with these keys:

```python
{
    "score": {"key": "score", "label": "Score", "value": 720.0, "maxValue": 999},
    "scorecard": [
        {"field": "equifax_score", "order": 1, "value": 750, "bucket": 3,
         "points": 120, "maxPoints": 200, "label": "Bureau Score", "bucketLabel": "Good"}
    ],
    "metrics": [
        {"key": "risk_grade", "label": "Risk Grade", "value": "B", "metadata": None}
    ],
    "rules": [
        {"id": "r1", "order": 1, "code": "DR_D001", "label": "Score below threshold",
         "value": "Score: 720", "alertLevel": 2, "hit": False}
    ],
    "decision": "Aprobar"
}
```

Key fields for downstream tasks:
- `result["decision"]` -- the evaluator's final decision string
- `result["score"]["value"]` -- the computed score
- `result["rules"]` -- list of business rules with `hit` (True/False/None)
- `result["metrics"]` -- derived values (e.g. risk grade letter)
- `result["scorecard"]` -- individual scorecard variable contributions

If the evaluator errors (bad code, missing variable), the macro raises an Exception with the traceback detail.

### `macros.get_borrower_metrics`

Batch-extract borrower metrics with sentinel value handling. Retrieves the borrower once, then loops through metric keys.

```python
metrics = await alts_acli.macros.get_borrower_metrics(
    borrower_id="abc-123",
    metric_keys=[
        "behMean_sales_last_3M",
        "behMean_sales_last_6M",
        "behMean_sales_last_12M",
        "behCredits_daily_dpd_max_90D",
        "days_since_first_sale",
        "days_since_last_sale",
    ],
    default=-999999,                  # value for missing metrics
    none_on_sentinel=[                # these keys get None instead of -999999
        "days_since_first_sale",
        "days_since_last_sale",
    ],
)
# {"behMean_sales_last_3M": 1234.5, "days_since_first_sale": None, ...}
```

### `macros.create_alerts_from_rules`

Create borrower alerts from evaluator rule results. Filters rules where `hit == True`, maps rule code prefixes to alert levels, and calls the alerts API. Swallows duplicate alert errors.

```python
alerts = await alts_acli.macros.create_alerts_from_rules(
    borrower_id="abc-123",
    rules=evaluator_result["rules"],
    execution_id=context.get("execution_id"),  # ties alerts to the workflow execution
    level_mapping={                    # maps rule code prefix to alert level
        "DR_D": 2,                     # prefix "DR_D" -> level 2 (high)
        "DR_R": 1,                     # prefix "DR_R" -> level 1 (medium)
        "DR_AP": 2,                    # prefix "DR_AP" -> level 2
    },
    default_level=0,                   # level when no prefix matches
)
# [{"borrowerId": "abc-123", "ruleCode": "DR-D001", "level": 2, "message": "...", "referenceId": "exec-456"}, ...]
```

Note: rule codes have underscores replaced with dashes in the alert (e.g. `DR_D001` becomes `DR-D001`).

### Composing macros in task code

```python
class InputData(BaseModel):
    person_id: str

class OutputData(BaseModel):
    borrower_id: str
    all_sources_ok: bool

async def execute(input_data: InputData, context: dict) -> OutputData:
    borrower = await alts_acli.macros.find_or_create_borrower(
        identity_key="person_id",
        identity_value=input_data.person_id,
    )
    enrichment = await alts_acli.macros.enrich_borrower(
        borrower_id=borrower["borrower_id"],
        sources=[{"sourceId": "ECU-PUB-0002", "version": "v1"}],
        input_keys={"personId": input_data.person_id},
    )
    return OutputData(
        borrower_id=borrower["borrower_id"],
        all_sources_ok=enrichment["all_sources_ok"],
    )
```

## Evaluators (Rule Engines)

Evaluators are versioned Python-based rule engines. Given an instance (subject with variables) and optional entities, they return a decision with score, scorecard, metrics, and rule hits.

### CLI commands

```bash
# List evaluators on the tenant
altscore evaluators list --per-page 10

# Get evaluator definition (shows alias, version, specs)
altscore evaluators get <id>

# Run evaluator by ID
altscore evaluators evaluate <id> --body '{
  "instance": {
    "referenceId": "borrower-123",
    "referenceDate": "2026-03-07T12:00:00",
    "data": {"score": 750, "debt_ratio": 0.3}
  },
  "entities": []
}'

# Run evaluator by alias + version
altscore evaluators evaluate-by-alias scoring v3 --body '{...}'
```

### Using evaluators in workflow task code

Prefer the `macros.evaluate` wrapper over raw SDK calls:

```python
# Simple -- use the macro
result = await alts_acli.macros.evaluate(
    evaluator_alias="scoring",
    evaluator_version="v3",
    reference_id=borrower_id,
    data=evaluator_variables,
    execution_id=context.get("execution_id"),
)

# Raw SDK call (if you need lower-level control)
bc = alts_acli.borrower_central
result = await bc.evaluators.evaluate(
    evaluator_input={
        "instance": {
            "referenceId": borrower_id,
            "referenceDate": datetime.now().isoformat(),
            "data": evaluator_variables,
        },
        "entities": [],
    },
    evaluator_alias="scoring",
    evaluator_version="v3",
)
# result is EvaluatorOutput -- call result.dict(by_alias=True) for a plain dict
```

### Evaluator output shape

```python
{
    "score": {"key": str, "label": str, "value": float, "maxValue": float|None},
    "scorecard": [
        {"field": str, "order": int, "value": any, "bucket": int,
         "points": int, "maxPoints": int, "label": str, "bucketLabel": str}
    ],
    "metrics": [
        {"key": str, "label": str, "value": any, "metadata": dict|None}
    ],
    "rules": [
        {"id": str, "order": int, "code": str, "label": str,
         "value": str, "alertLevel": int|None, "hit": bool|None}
    ],
    "decision": str
}
```

`hit` can be `True`, `False`, or `None` (None = missing input field, rule could not be evaluated).

### Common post-evaluator patterns

**Set risk rating from evaluator metrics:**
```python
metrics = result.get("metrics", [])
risk_grade = metrics[0]["value"] if metrics else "N/A"  # e.g. "A", "B", "C"

grade_to_color = {"A": 1, "B": 2, "C": 2, "D": 3, "E": 4, "F": 5}
color = grade_to_color.get(risk_grade, 5)

borrower = await bc.borrowers.retrieve(borrower_id)
if risk_grade in grade_to_color:
    await borrower.set_risk_rating(risk_grade)
```

**Calculate recommended amount from score:**
```python
score = result["score"]["value"]

factor_table = {
    (500, 600): 0.5,
    (601, 700): 0.75,
    (701, 800): 1.0,
    (801, 900): 1.5,
    (901, 1000): 2.0,
}

factor = None
for (lo, hi), f in factor_table.items():
    if lo <= score <= hi:
        factor = f
        break

base_amount = best_available_sales_average  # from metrics
recommended = base_amount + (base_amount * factor) if factor else base_amount
recommended = min(recommended, cap_limit)
recommended = max(recommended, minimum_amount)
```

## Workflow Output Fields (`w_` prefix)

Task output keys prefixed with `w_` are intercepted by the DAG runner and control the final execution output. They are NOT passed to downstream tasks -- they are extracted before merging. Any task in the DAG can set these.

### `w_standard_exec_output`

The primary structured output. Shape follows `ExecutionOutputStandardOutputWhiteBoxDecisioning`:

```python
{
    "w_standard_exec_output": {
        "billable_id": borrower_id,        # for billing attribution
        "borrower_id": borrower_id,
        "deal_id": deal_id,                # optional
        "isSuccess": True,
        "fields": {                        # free-form client-visible fields
            "message": "Evaluation complete",
            "decision": "Approve",
            "score": 750,
            "recommended_amount": 5000,
        },
        "data": [],                        # additional data list (usually empty)
        "score": {"key": "score", "label": "Score", "value": 750, "maxValue": 999},
        "scorecard": evaluator_result.get("scorecard", []),
        "metrics": evaluator_result.get("metrics", []),
        "rules": evaluator_result.get("rules", []),
        "decision": evaluator_result.get("decision"),
        "alerts": formatted_alerts,        # list of alert dicts
    }
}
```

### `w_custom_output`

Free-form dict returned as `ExecutionOutput.custom_output`. Use for flat, client-facing fields that don't fit the standard schema:

```python
{"w_custom_output": {"SCORE": 750, "DECISION": "Approve", "AMOUNT": 5000}}
```

### `w_attachments`

List of file/URL attachments (typically PDF report URLs):

```python
{"w_attachments": [{"url": report_url}]}
```

Each attachment can also have optional `label`, `file_extension`, and `metadata` fields.

### `w_is_success`

Explicitly override execution success/failure. If not set, defaults to True (or False if an error occurred). When False, `w_standard_exec_output`, `w_attachments`, and `w_custom_output` are all cleared from the final output.

```python
{"w_is_success": False}  # marks execution as failed
```

### `w_notes`

List of string notes collected across all tasks and returned in `ExecutionOutput.notes`:

```python
{"w_notes": ["Source ECU-PUB-0002 returned stale data", "Using cached score"]}
```

### `w_notices`

Structured notices with severity levels. Returned in `ExecutionOutput.notices`:

```python
{"w_notices": [
    {"message": "Bureau score below threshold", "severity": "info"},
    {"message": "Missing tax ID data", "severity": "error"},
]}
```

Severity values: `"info"`, `"error"`, `"debug"`. Debug notices are logged but not returned to the client.

### `w_schedule_callback`

Boolean that signals the execution should be scheduled for async callback (retry later). Used for polling-based workflows. Most tasks will never need this:

```python
{"w_schedule_callback": True}  # schedule callback to retry
```

## Data-Models (Schema Definitions)

Data-models define the fields and keys available on all AltScore entities. They control what identity keys, borrower fields, steps, deal fields, and asset groups exist on a tenant.

### Entity type categories

| Category | Entity Types |
|----------|-------------|
| core | identity, contact, document, borrower, point_of_contact, authorization, metric, accounting_document |
| fields | borrower_field |
| workflow | step, decision |
| deals | deal_field, deal_step, deal_role |
| assets | asset_field, asset_group |

### CLI commands

```bash
# CRUD
altscore data-models list --filter entity-type=identity
altscore data-models get <id>
altscore data-models create --body '{"key": "email", "label": "Email", "entityType": "identity", "priority": 2}'
altscore data-models update <id> --body '{"label": "Email Address"}'
altscore data-models delete <id>

# Enable encryption (one-way, identity only)
altscore data-models make-sensitive <id>

# Best-practices guide (live documentation)
altscore data-models guide
altscore data-models guide identity
altscore data-models guide borrower_field
```

### Key rules

- **identity**: `priority` is required (>= -1). Use -1 to append to end. Priorities auto-shift on insert/delete.
- **step / deal_step**: `order` is required. Orders auto-shift on insert/delete.
- **borrower_field / asset_field / deal_field**: `allowedValues` can constrain input to a list. Other types cannot use `allowedValues`.
- **isSensitive**: Can only be set at creation time or via `make-sensitive`. Cannot be undone or changed via update.
- **isSegmentationField**: Makes the field available for audience segmentation in the UI.

### Create examples

```bash
# Identity with priority
altscore data-models create --body '{"key": "tax-id", "label": "Tax ID", "entityType": "identity", "priority": 0, "isSensitive": true}'

# Step with order
altscore data-models create --body '{"key": "application", "label": "Application", "entityType": "step", "order": 0}'

# Borrower field with allowed values
altscore data-models create --body '{"key": "industry", "label": "Industry", "entityType": "borrower_field", "allowedValues": ["retail", "manufacturing", "services"], "isSegmentationField": true}'
```

### SDK usage (inside workflow tasks)

```python
bc = alts_acli.borrower_central

# List data-models by entity type
models = await bc.data_models.query(entity_type="identity")

# Create a data-model
dm_id = await bc.data_models.create({
    "key": "phone",
    "label": "Phone Number",
    "entityType": "identity",
    "priority": 3,
})

# Retrieve
dm = await bc.data_models.retrieve(dm_id)

# Update
await bc.data_models.patch(dm_id, {"label": "Mobile Phone"})

# Delete
await bc.data_models.delete(dm_id)
```

### Live guide

Use `altscore data-models guide` to get the full best-practices reference including required fields per entity type, validation rules, special behaviors (priority shifting, order shifting), and annotated create examples. Filter by entity type with `altscore data-models guide <type>`.

## Report Generation (PDF)

The report generator creates styled PDF reports from a structured request.

### CLI commands

```bash
# Generate a report (returns signed URL)
altscore tools generate-report --body '{"reportTitle": "...", "sections": [...]}'

# Discover available report components
altscore tools report-components

# Get JSON Schema for a specific component
altscore tools report-components subjectInfo
altscore tools report-components evaluatorResult
altscore tools report-components reportOptions
```

**Always use `altscore tools report-components <name>` to get the exact schema before constructing a component.** The schemas are served live from the report generator service and include all field names, types, and defaults.

### Testing workflow (before writing task code)

The report generator has strict validation. Always dry-run your payload with the CLI before embedding it in task code:

```bash
# 1. Check top-level required fields (logoUrl is required!)
altscore tools report-components reportOptions

# 2. Check required fields for each component you plan to use
altscore tools report-components keyValueTable

# 3. Test the full request -- iterate until it returns a URL
altscore tools generate-report --body '{
  "reportTitle": "My Report",
  "byLine": "",
  "logoUrl": "",
  "sections": [{
    "title": "Section",
    "subtitle": "",
    "components": [
      {"name": "keyValueTable", "title": "Details", "subtitle": "", "items": [
        {"label": "Key", "value": "Value"}
      ]}
    ]
  }]
}'

# 4. Only after the CLI returns a valid URL, copy the payload into task code
```

### SDK usage (inside workflow tasks)

```python
bc = alts_acli.borrower_central
report_url = await bc.report_generator.generate(report_req)
# Returns a URL string pointing to the generated PDF
```

### `report_req` structure

```python
report_req = {
    "reportTitle": "Credit Analysis Report",
    "byLine": "Generated on 2026-03-07",
    "logoUrl": "https://...",           # REQUIRED (use "" for no logo)
    "logoSize": "128px",               # optional
    "sections": [                       # list of section objects
        {
            "title": "Section Title",
            "subtitle": "",
            "page_break": False,        # optional, force page break before section
            "components": [...]         # list of component objects
        }
    ]
}
```

Each component is a dict with `"name"` identifying the type, plus the component's options as sibling keys. Container components (flex, row) also accept a nested `"components"` array.

### Component categories

**Report components** -- the building blocks you construct manually in task code. Use `altscore tools report-components` to list all with descriptions. Key ones:

- `subjectInfo` -- Identity card with name and key-value pairs
- `subjectScore` -- Score display with grades
- `evaluatorResult` -- Full evaluator output with decision and rules tables
- `scorecardResult` -- Scorecard breakdown table
- `ruleTreeResult` -- Decision tree result with rule hits
- `keyValueTable` -- Label-value pairs (supports HTML)
- `complianceTable` -- Alert factors with severity colors
- `customTable` -- Dynamic table from columns/rows
- `card`, `card_v2` -- Metric cards with icon and context color
- `htmlBlock` -- Raw HTML (spacers, custom formatting)
- `flex`, `row` -- Layout containers for child components

**Source components** -- auto-matched by AltData source slug as the `name` field:
```python
{"name": "ECU-PUB-0002_v1", "altdataPackage": source_dict}
```
Version fallback: if the exact version isn't found, the highest available version is used.

**Entity components** -- render entity data (e.g., `ASSET_v1`).

### Typical report generation task pattern

```python
class InputData(BaseModel):
    borrower_id: str
    evaluator_result: Dict[Any, Any]
    source_packages: Dict[str, Any]   # slug -> package dict

class OutputData(BaseModel):
    report_url: str
    w_attachments: List[Dict[str, Any]]

async def execute(input_data: InputData, context: dict) -> OutputData:
    bc = alts_acli.borrower_central

    sections = []

    # Subject info section
    sections.append({
        "title": "", "subtitle": "",
        "components": [
            {"name": "subjectInfo", "subjectName": "...", "numberOfColumns": 1,
             "items": [{"label": "ID", "value": "..."}]},
            {"name": "subjectScore", "label": "Score",
             "scoreValue": input_data.evaluator_result["score"]["value"],
             "scoreMaxValue": "999", "secondaryInfo": [], "grades": []},
        ]
    })

    # Evaluator result section
    sections.append({
        "title": "Credit Evaluation", "subtitle": "",
        "components": [
            {"name": "evaluatorResult", "title": "",
             "displayConfiguration": {
                 "decision": {"key": "decision", "label": "Decision",
                              "type": "categorical",
                              "contextMap": {"Approve": "success", "Reject": "danger"}},
                 "rulesHitTable": True, "allRulesTable": True,
                 "mainCards": [], "secondaryCards": [],
             },
             "evaluatorResult": input_data.evaluator_result},
        ]
    })

    # AltData source sections (only if successful)
    for slug, pkg in input_data.source_packages.items():
        if pkg.get("isSuccess"):
            sections.append({
                "title": slug, "subtitle": "", "page_break": False,
                "components": [{"name": slug, "altdataPackage": pkg}]
            })

    report_url = await bc.report_generator.generate({
        "reportTitle": "Analysis Report",
        "byLine": f"Generated: {datetime.now().strftime('%Y-%m-%d')}",
        "sections": sections,
    })

    return OutputData(
        report_url=report_url,
        w_attachments=[{"url": report_url}],
    )
```

## Alerts

Alerts are policy notifications created on borrowers, typically from evaluator rule hits.

### CLI commands

```bash
# List alerts for a borrower
altscore api GET "/v1/alerts?borrower-id=<id>"

# Create an alert
altscore api POST /v1/alerts --body '{
  "borrowerId": "<id>",
  "ruleCode": "DR-D001",
  "level": 2,
  "message": "Score below threshold: 450",
  "referenceId": "<execution-id>"
}'
```

### SDK usage

```python
bc = alts_acli.borrower_central
alert_id = await bc.alerts.create({
    "borrowerId": borrower_id,
    "ruleCode": "DR-D001",
    "level": 2,
    "message": "Score below threshold",
    "referenceId": execution_id,      # ties alert to workflow execution
})
```

Prefer `macros.create_alerts_from_rules` over manual alert creation -- it handles rule filtering, level mapping, and duplicate suppression automatically.

## Reading Enrichment Results (store_packages)

After enrichment, downstream tasks read package content using the canonical slug `AD_{sourceId}_{version}`:

```python
bc = alts_acli.borrower_central

# Query package by slug
pkgs = await bc.store_packages.query(source_id="AD_ECU-PUB-0002_v1", borrower_id=bid)
if pkgs:
    await pkgs[0].get_content_json()
    data = pkgs[0].content  # dict with source data

# Or use retrieve_source_package (returns None if not found or stale)
# timedelta is available as a global
pkg = await bc.store_packages.retrieve_source_package(
    source_id="AD_ECU-PUB-0002_v1",
    borrower_id=bid,
    data_age=timedelta(minutes=360),
)
if pkg:
    await pkg.get_content_json()
    data = pkg.content
```

The slug convention is `AD_{sourceId}_{version}`, e.g. `AD_ECU-PUB-0002_v1`.

## Gotchas

- **`retrieve` not `get`**: SDK method for fetching by ID is `retrieve()`, not `get()`.
- **Always `await` async methods**: All `alts_acli.borrower_central.<module>.<method>()` calls are coroutines. Missing `await` gives `'coroutine' object has no attribute 'data'`.
- **`create` returns an ID string**, not the created object. To get the object, call `retrieve()` after.
- **`identities.create` body needs camelCase `borrowerId`**: The Pydantic alias is `borrowerId`, not `borrower_id`. Write `{"borrowerId": bid, "key": "email", "value": "..."}`.
- **Query kwargs use snake_case**: `bc.identities.query(borrower_id=bid)` auto-converts to `?borrower-id=...`. Don't pass dash-case.
- **Sentinel values**: `-999999` and `-999997` in metrics/fields mean missing data. Always check before using in calculations.
- **Batch `child-workflow` `inputExpression` accepts deep paths.** `inputExpression: "task_outputs.<alias>.<deep>.<path>"` resolves correctly — you don't need to pre-flatten into a top-level task output or an `inputs.*` variable. A typo that references an unknown alias raises a clear `ValidationError` at compose/dispatch time rather than silently fanning out over an empty list.
- **`customVariables` with `type: "object"` is the safe choice for lists.** Arrays now flow through too, but the underlying eval service preserves lists when the variable is typed `object`; the historical `type: "array"` form returned `[]` in some paths. Use `type: "object"` when you're building a list-valued custom variable that downstream tasks consume.
- **`deal` task writes are idempotent on `(deal_id, borrower_id, role_key)`.** Re-running the deal task with the same triple won't duplicate a DealContact row — safe to re-execute workflows that include a `deal` task with `sourcesConfig` entries of type `deal_contact` or `deal_contacts`.
