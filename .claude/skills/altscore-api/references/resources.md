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
>
> **Engine-variant drill paths differ — branch on shape, don't assume `data_flow`.** The `state.data_flow.task_outputs` path above is the v2-engine shape. v1-engine executions (e.g. legacy `*-kyc-*` workflows) expose their keys directly under `state` (`state.altdata_requests`, `state.borrower_id`, ...) with no `data_flow` nesting, so the documented path yields `null` for them. Inspect `state | keys` first, then drill.

### Packages (read-only)

```bash
altscore packages list --filter alias=credit-report --per-page 5
altscore packages get <id>
```
