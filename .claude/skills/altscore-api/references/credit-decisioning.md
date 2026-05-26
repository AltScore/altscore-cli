### Credit Decisioning

Four entity types power the credit-decisioning v2 task surface. They live at `/v1/{evaluation-rules, mapping-tables, scorecards, rule-trees}` and are referenced by alias from v2 tasks. All four have full CRUD + `import` extras; scorecards add `usage`; evaluation-rules add `history`.

> **`workflowAlias` is load-bearing — set it on every entity.**
> The v2 builder filters its rule / rule-tree / mapping-table / scorecard pickers by `workflowAlias`. An entity created without one is invisible to that workflow, even though the entity itself is fine. Always pass `--workflow-alias <alias>` (matches the workflow's `alias`) on `create`, `update`, and `import`. The CLI prints a stderr warning on `create` if neither the flag nor a body field sets it.
>
> **The workflow's alias is server-derived from its label** — `"Customer Onboarding"` slugifies to `customer-onboarding`, `"All 5 types"` to `all-5-types`. The body's `alias` field is silently dropped on `workflows-v2 create` (but `apply` honors `spec.alias` explicitly). A common trap: stamping entities with a guess like `customer-onboarding-v1` when the workflow's actual alias becomes `customer-onboarding-v-1`. Run `altscore workflows-v2 apply --body @spec.json --dry-run` first — it prints the predicted alias up front and tells you what to pass to `--workflow-alias` on entity creates. Better: skip the manual --workflow-alias stamping entirely and rely on `apply`'s auto-rescope to fix it post-create. Or compute it locally: lowercase, replace non-`[a-z0-9]+` with `-`, collapse repeated `-`, trim, cap at 100 chars.

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

#### Building a workflow that uses them (apply)

The four matching v2 task types — `evaluate-rules`, `mapping-table`, `scorecard`, `rule-tree` — reference these entities. `apply` validates references against the tenant (best-effort warnings) and pre-fills `outputSchema` with the canonical runtime fields so downstream tasks see the right available outputs.

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
altscore workflows-v2 apply --body @/tmp/credit-spec.json --dry-run
altscore workflows-v2 apply --body @/tmp/credit-spec.json --publish
```

`apply` auto-fills:
- `evaluate-rules`: `outputSchema = {alerts: array, alerts_count: integer}`
- `mapping-table`: each `entries[].outputVariable` becomes a top-level output field (string by default; refined to `number` when the referenced mapping table's `outputType` is number). **`apply` also mints a UUID for each `entries[].id`** — the runtime requires it but agents typically forget.
- `scorecard`: `outputSchema = {<totalScoreVariable>: number, <breakdownVariable>: object}` (breakdown defaults to `score_breakdown`)
- `rule-tree`: `outputSchema = {<outputVariable>: <outputType>}`

#### Common pitfalls (credit-decisioning specific)

- **`workflowAlias` decides picker visibility.** The Hub builder filters its rule / rule-tree / mapping-table / scorecard pickers by `workflowAlias` matching the workflow. Entities created without it exist on the tenant but are invisible to that workflow. **`apply` handles this automatically** via the entity-scope reconciler — after a successful apply, every referenced entity (and the rules/mapping-tables nested under scorecards and rule-trees) is stamped to the workflow's alias. If you're scripting outside apply, pass `--workflow-alias <alias>` to `create`, `update`, and `import`. To re-scope an existing entity manually: `altscore <resource> update <id> --workflow-alias <alias>`.
- **All four task types are reference-only.** `evaluate-rules` → `rulesConfig: [{ruleCode}]`, `mapping-table` → `mappingTableConfig.entries[].mappingTableCode`, `scorecard` → `scorecardConfig.scorecardCode`, `rule-tree` → `ruleTreeConfig.ruleTreeCode`. Inline rule/scorecard/table definitions on the task body are silently ignored at runtime; create the entity via the matching CRUD command first, then reference by code in apply.
- **Scorecard rules require a mapping table per rule.** When you `altscore scorecards create`, every entry in `rules[]` must include `mappingTableCode` (or `mappingTableId`). Buckets on the rule are NOT a substitute — the runtime reads buckets from the linked mapping table and fails with `Rule '<label>' must be linked to a mapping table` otherwise.
- **`alertLevel` is required to produce alerts.** A matching `evaluate-rules` rule with no `alertLevel` set produces nothing in the task's `alerts[]` output. Set `alertLevel: 1|2|3` (with optional `alertMessage`) on the rule when alerts are wanted.
- **`decisionKey` drives `rule-tree` output.** The first matching rule's `decisionKey` becomes the rule-tree task's `outputVariable` value. Without `decisionKey` on the rule, the rule-tree's decision is null even on a hit.
- **`decisionKey` is case-sensitive and must match a tenant-registered decision.** Run `altscore decisions list` before writing rules; case mismatches (e.g. `"REJECTED"` when the tenant has `"reject"`) apply and lint clean but fail downstream when the run tries to record the decision via `/v1/executions/{id}/decisions` ("key not found for entity type: decision").
- **`rule-tree` `outputVariable` becomes a top-level task output.** Downstream conditionals reference `task_outputs.<rule-tree-alias>.<outputVariable>` (e.g. `task_outputs.tree.decision`). `outputType` must be `string` | `number` | `boolean`.
- **`is-test` toggles on evaluation rules / rule trees** isolate them from production execution. Use `altscore evaluation-rules set-test <id> --enable` while iterating, then `--disable` once stable.
