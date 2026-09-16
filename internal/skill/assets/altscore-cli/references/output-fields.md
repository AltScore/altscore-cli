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
