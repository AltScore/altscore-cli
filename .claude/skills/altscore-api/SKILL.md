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

## Resource Commands

Ten resources are available. Every resource supports `--help` which documents request body fields, response fields, and available filters.

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
altscore executions list --filter borrower-id=<id> --filter status=complete
altscore executions get <id>
```

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

## AltData

Discovery commands query Borrower Central (work in all environments). Execution commands hit the AltData module (production only).

### Discovery

```bash
# List available data sources
altscore altdata sources --per-page 10
altscore altdata sources --filter country=USA --filter status=active

# Field definitions for a source
altscore altdata dictionary USA-PUB-0001 v1

# Search field definitions across all sources
altscore altdata search "credit score"
altscore altdata search "address" --locale es

# Sample output for a source
altscore altdata sample USA-PUB-0001 v1
```

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
