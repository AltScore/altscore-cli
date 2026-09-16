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
