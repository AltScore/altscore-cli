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

## Resource Commands

Nine resources are available. Every resource supports `--help` which documents request body fields, response fields, and available filters.

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
