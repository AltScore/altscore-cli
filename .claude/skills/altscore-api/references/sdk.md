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

Prefer the `macros.evaluate` wrapper (see [SDK Macros](#macrosevaluate) above) over raw SDK calls. Use a raw SDK call only when you need lower-level control:

```python
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

## SDK Gotchas


- **`retrieve` not `get`**: SDK method for fetching by ID is `retrieve()`, not `get()`.
- **Always `await` async methods**: All `alts_acli.borrower_central.<module>.<method>()` calls are coroutines. Missing `await` gives `'coroutine' object has no attribute 'data'`.
- **`create` returns an ID string**, not the created object. To get the object, call `retrieve()` after.
- **`identities.create` body needs camelCase `borrowerId`**: The Pydantic alias is `borrowerId`, not `borrower_id`. Write `{"borrowerId": bid, "key": "email", "value": "..."}`.
- **Query kwargs use snake_case**: `bc.identities.query(borrower_id=bid)` auto-converts to `?borrower-id=...`. Don't pass dash-case.
- **Sentinel values**: `-999999` and `-999997` in metrics/fields mean missing data. Always check before using in calculations.
- **Batch `child-workflow` `inputExpression` accepts deep paths.** `inputExpression: "task_outputs.<alias>.<deep>.<path>"` resolves correctly — you don't need to pre-flatten into a top-level task output or an `inputs.*` variable. A typo that references an unknown alias raises a clear `ValidationError` at apply/dispatch time rather than silently fanning out over an empty list.
- **`customVariables` with `type: "object"` is the safe choice for lists.** Arrays now flow through too, but the underlying eval service preserves lists when the variable is typed `object`; the historical `type: "array"` form returned `[]` in some paths. Use `type: "object"` when you're building a list-valued custom variable that downstream tasks consume.
- **`deal` task writes are idempotent on `(deal_id, borrower_id, role_key)`.** Re-running the deal task with the same triple won't duplicate a DealContact row — safe to re-execute workflows that attach contacts via the inline `contacts` field. (The legacy `deal_contact` / `deal_contacts` `sourcesConfig` types are no longer supported.)
