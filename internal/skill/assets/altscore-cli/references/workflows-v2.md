### Workflows V2 (Visual Builder)

> Building a KYC, KYB, or onboarding flow? Read [kyc-kyb-habits](kyc-kyb-habits.md) first — tenant/country-agnostic structure (orchestrator vs per-party child, identity & idempotency, two-layer decisioning) that complements the build mechanics below.

> **Create and update v2 workflows with `apply`.**
>
> To create or update a v2 workflow, run the question path ([Discovery](#discovery-before-authoring-a-guided-question-path-infer-first-then-ask-in-short-rounds)) first, then **`altscore workflows-v2 apply`** with a single spec file. Borrower Central reconciles the spec against the tenant in one request: a rejected spec writes nothing, a publish rejection leaves the tasks and a DRAFT in place (`APPLY_PUBLISH_REJECTED`), and a mid-write failure is rolled back, with the error saying when rollback was incomplete. It creates the workflow when no workflow has the spec's alias; otherwise it updates it in place (same id and alias; unchanged tasks are left alone, changed ones are version-bumped).
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
>    Operators, written in snake_case (`apply` rewrites a camelCase spelling to it, because the backend evaluates an unknown operator to False with no error): `equals`/`eq`, `not_equals`/`neq`, `gt`, `gte`, `lt`, `lte`, `contains`, `not_contains`, `starts_with`, `ends_with`, `in`, `not_in`, `between`, `is_null`, `is_not_null`, `is_empty`, `is_not_empty`, `is_true`, `is_false`, `array_contains_any`/`_all`/`_none`, `is_altdata_empty`, `is_altdata_not_calculated`, `is_altdata_error`, `is_altdata_null`, `is_not_altdata_null`. Presence, emptiness and prefixes are operators, so they need no length or prefix variable. `valueType` is `"value"` (literal) or `"variable"` (reference to another inputSchema field). Field is `isElse` (camelCase), not `is_else`.
>
> 2. **`altdata-enrichment` tasks need `inputKeys` to wire source-required fields.** Each source (e.g. `ECU-PUB-0002`) declares `inputFields` like `personId`, `taxId`. The task must include `inputKeys: {"personId": "{{personId}}", "taxId": "{{taxId}}"}` matched against an `inputSchema` that declares those keys, plus `packageAlias` (where to store results) on each `sourcesConfig` entry. Leave `dataAge` unset unless the user chose a freshness window, because an authored value overrides the freshness the source publishes. `apply` auto-derives `inputKeys` by querying `sources-status` for each source's `inputFields` — use it.
>
> Run `altscore workflows-v2 schema-guide conditions` and `... schema-guide tasks <type>` for the canonical reference.

Workflows V2 is the API surface for the visual graph builder in the Hub. It uses **two collaborating resources**:

| Resource | Path | Purpose |
|---|---|---|
| **Tasks** | `/v2/tasks` | Versioned executable units (HTTP url, evaluator alias, sources_config, branches, ...) |
| **Workflows** | `/v2/workflows` | The graph: `nodes[]` + `edges[]` + variables. Each node references a task by `taskAlias` |

This is **not** v1 (`/v1/workflows`). Use v2 for anything created in the visual editor.

**Key insight: tasks first, then workflow.** **Every** graph node — including `start`, `end`, and `conditional` — needs a `taskAlias`. The Hub creates trivial backing tasks (just `type` + `label`) for start/end so it can render them. In an `apply` spec you write none of these: `apply` creates a backing task for every node automatically, `start` included (`{"ref": "start", "type": "start", "label": "Start"}` is enough).

After creating a workflow, run `altscore workflows-v2 lint <id>` to verify there are no orphan nodes, dangling edges, or duplicate ids. The lint command also runs the same checks as the create-time validator and is the fastest way to triage a misbehaving workflow.

For canonical field-by-field reference run:

```bash
altscore workflows-v2 schema-guide                # INDEX: every section, one line + token cost. Start here.
altscore workflows-v2 schema-guide architecture   # the tasks-first explanation
altscore workflows-v2 schema-guide nodes          # node shape (camelCase: nodeId, label, taskAlias, ...)
altscore workflows-v2 schema-guide edges          # edge shape (sourceNodeId, targetNodeId)
altscore workflows-v2 schema-guide tasks deal     # ONE task type: hand-written notes + introspected fields (1-4k tokens)
altscore workflows-v2 schema-guide tasks          # every type, one row each; notes=false means fields only, no narrative
altscore workflows-v2 schema-guide --search extraction   # every place the guide mentions a word, with the command that opens it
altscore workflows-v2 schema-guide examples       # full scoring_pipeline template
```

The whole guide is ~50k tokens; `--full` prints it, and nothing in this file needs it. Fetch the section that answers the question in front of you, and look a word up with `--search`, never `--full | jq`.

#### Discovery before authoring: a guided question path (infer first, then ask in short rounds)

The path governs every spec you write or change: a new workflow, an update, a one-node edit. "Create a workflow that does X" and "change workflow Y so that Z" leave decisions open. Most of them are already written down somewhere in the tenant; some are business policy that only the user knows; and the brief has gaps of its own, even when it looks complete. Read the first group. Walk the user through the rest as a **path of short rounds** with the **AskUserQuestion** tool, so they answer a few concrete choices at a time instead of reviewing a design:

1. **Frame.** Restate in one line what you inferred (who is evaluated, country, sources, decision keys, the closest sibling workflow). Then ask the framing policy and the brief-contract gaps that survive the reads: what ends the application versus goes to a human, what happens when a gating source fails, where the cutoffs come from, what the brief leaves undefined.
2. **Details** (only when round 1 opened them or left questions over). Questions that exist because of an answer: a review lane exists, so which findings go there; the user supplies the cutoffs, so what are they.
3. **Confirm the plan.** Before writing the spec, one question: the node path in one line plus **every choice made on the user's behalf**: each recommended option taken without asking, each question that did not fit a round, each `TODO` placeholder, and the [defaults `apply` fills](#canonical-end-node-pattern-one-end-fed-by-the-rule-tree) that the spec will not set. Options `Build it (Recommended)` and `Change <choice>`. The engineer reacts to a proposal instead of holding the design in their head.

Every round: at most 4 questions, 2 to 4 options each, the option you would pick first labelled `(Recommended)`; at most 8 across the whole path. When more than 4 survive, ask the 4 that change the graph's shape (a node, a branch, a source, an entity) first; the rest go to the next round or into the confirm round's list of choices. Never pack them into one round. Skip any round with nothing left to ask. Never ask what a read answers.

**Order: questions, answers, then the entities the spec references, then the first `apply --dry-run`.** No entity, spec file or dry-run before the answers.

**Infer first. Never ask these; read them.**

| Decision | Where the answer is |
|---|---|
| Who is evaluated, which country, which stage of the customer's process | the request itself. "KYB for Ecuadorian SMEs" is a company, ECU, origination. |
| Which sources, their required inputs, what they can detect | `altscore workflows-v2 sources-status --country <ISO3>` (or `--search <word>`): one compact row per source version with its `requiredInputs`; the unfiltered catalog is about 40 KB. A `--country` filter hides INT (international) sources such as sanctions lists; stderr counts what a filter hid. Then `altdata describe <id>` and `altdata dictionary <id>` for ONE source's field paths |
| Decision vocabulary, whether a review lane exists | `altscore decisions list`: the registered keys are the only ones a run can write. A brief that says `approve/review/reject` while the tenant registers `passes/pending/fails` is mapped BY LABEL, and the mapping is restated to the user in one line |
| Write targets, output shape, input payload, PDF or not, freshness of paid sources | the tenant's closest existing workflow: `workflows-v2 list --filter is-latest=true` (one compact row per workflow), its skeleton (below), then only its `inputSchema`, end node, `decisionConfig` and `sourcesConfig.dataAge`. Mirror it. |
| Fields available on borrower and deal, and what a real value looks like | `altscore data-models list`, then `borrower-fields list --filter borrower-id=<test borrower>` and `packages content <id>` on the test case. A null sample is a question, not a guess |
| Structure: orchestrator plus per-party child, fan-out, two-layer decisioning | [kyc-kyb-habits](kyc-kyb-habits.md) |
| Whether a change is visible to others (update path) | `workflows-v2 schedule get <id>`, and other workflows whose `child-workflow` nodes carry this alias as `executorId` |

Read a sibling or a child with `--format outline` (inputs, graph, variable types, the end node's output keys: a few KB); the apply-spec inlines every task body:

```bash
altscore workflows-v2 export <id> --format outline
altscore workflows-v2 export <id> --format apply-spec | jq '.nodes[] | select(.ref == "<ref>")'   # one node's body
```

Anything the spec can default is yours as well: labels, positions, aliases and refs, branch ids, `inputKeys`, publish policy (DRAFT on create). Never ask which field a task type uses; read `schema-guide tasks <type>`.

**Ask only what survives the reads.** These are business policy and the brief's own contract: nothing in the tenant states them, and guessing wrong costs money or a customer. Pick the ones the flow triggers; the overflow rule above caps a round.

| Question | Ask when | Options to offer |
|---|---|---|
| A source the decision depends on fails or times out mid-run. What happens to the application? | any gating source in the flow | reject and stop; continue on what came back and flag the gap `(Recommended)` for monitoring; park it for manual review; retry later |
| Which findings end the application outright, and which go to a human? (multi-select the outright rejects) | the tenant registers a review-type decision key. Without one everything rejects; do not invent a lane | sanctions or PEP hit; dissolved or suspended entity; identity mismatch; adverse judicial record |
| Where do the cutoffs come from? | the flow scores or gates on a number and no sibling scorecard or rule-tree holds the numbers | reuse the cutoffs of the existing `<scorecard>` `(Recommended)` when one exists; the user supplies them now; a permissive placeholder marked TODO, tuned after test runs. Never invent thresholds silently |
| How fresh must paid bureau or registry data be? | a paid external source is in the flow and no sibling workflow already uses it | pull fresh every run; reuse within 30 days `(Recommended)` for origination; reuse within 90 days or more for monitoring |
| The same identity applies again within the window. Then what? | the flow writes a deal or a decision | re-run everything, writes are idempotent `(Recommended)`; reuse the last decision if younger than N days; reject as a duplicate |
| Are the entity's people screened too? | KYB, and the country has an ownership or legal-representative source. Without the source the question is moot | entity only; entity plus owners and legal representatives `(Recommended)`; entity plus the guarantors named in the input |
| Who reads the reasons? | no sibling workflow shows the output shape | internal only, decision key plus reason codes `(Recommended)`; the partner shows them to the applicant, so reasons go in the output; a human reads a PDF in the Hub |
| Update path: the change renames or removes an output, or the workflow has schedules or child callers. Blast radius? | the read above found a schedule or a caller | go live on the alias now, consumers are updated; keep the old field names too for a transition; apply under a new alias for a trial |
| Update path: a rule got stricter. Does the past get re-evaluated? | the change tightens a gate or a cutoff | new applications only `(Recommended)`; batch re-run the active portfolio in test mode first; re-run and re-decide |
| The brief and the tenant disagree | a field the brief names is missing on the test borrower, a sample value has the opposite shape, an alias the brief names is not in `workflows-v2 list` | the options ARE the disagreement: which side is right, or what the missing piece should be |
| A rule you want to write has no line in the brief | you are about to copy a rule family from a sibling workflow, or add a "sanity" rule the brief never asked for | add it; add it as informative (no decision impact); leave it out `(Recommended)` |
| **Brief contract.** Check these on every brief, create or update, however complete it looks | | |
| An outcome the brief does not cover | it decides some cases but not all: an input that may be missing, a source that answers empty, a value between two stated bands, a party type it never mentions | the candidate outcomes; the conservative one `(Recommended)` when the brief asks for caution |
| Units, currency and scale | a number or cutoff with no unit; an amount that could be local currency or USD, units or thousands; a rate that could be 0-1 or 0-100 | the readings; the one a sample value or the sibling supports `(Recommended)` |
| Reference date and vague time words | "recent", "the last months", "current", an age or a window with no start date | measured from the application date `(Recommended)`, the run date, or the data's own date; for a vague word, a number of months |
| An output or side effect the brief did not ask for | you are about to turn on recording a decision, a PDF, a notification or a write, or the dry-run's `# defaults applied` block lists one | what the closest sibling does `(Recommended)`; on; off. Ask before defaulting it on |

Example path for "create a KYB workflow for Ecuadorian SMEs" in a tenant that already runs a KYC flow. Read: company, ECU and origination from the request; sources from `sources-status` (the ECU rows plus the INT ones a country filter hides); a `review` key from `decisions list`; write targets, output shape and `dataAge` from the KYC export. Round 1 asks three things: what happens when the registry or judicial source fails mid-run; which findings reject outright versus go to review; whether owners and legal representatives are screened, since the country has an ownership source. Round 2 is skipped: nothing new opened. Round 3 shows `start -> sources -> conditional (rejects / review) -> end` with the choices made for the user (30-day reuse of paid data, reason codes in the output) and asks to build. Do not ask who is evaluated, which country, which sources, where the decision lands, or whether to publish.

**Every open point in the brief is a question.** A brief that carries a "pending / to confirm" list (a fiscal cutoff month, how a registry identifies a kind of asset, a catalogue that has not arrived) hands you the question list ready-made. Each unchecked item becomes either a question in the round or an explicit `TODO` placeholder the user has seen; never a silently assumed value. The same goes for a "conservative by default" principle in the brief: when you cannot confirm something the brief says must be confirmed (an asset class against a list that does not exist yet), the conservative outcome is the default and the deviation is a question, not a judgment call you make alone.

**Mid-build unknowns get a second round, not an analogy.** The first round covers what the reads surfaced. New unknowns appear while building (an alias the brief spelled differently from the tenant, a package alias nobody stated, a subflow output the brief does not say whether to consume). Each is a question. Deriving an identifier from a sibling's pattern ("the other ten documents end in `_info`") is the single most expensive shortcut an agent takes: it looks right, publishes, and is wrong in a way only the client notices.

**When AskUserQuestion is unavailable** (print mode `-p`, background jobs, some SDK hosts), the path still runs, through files. Write the exact payload the tool would have taken to `./questions.json`, `{"questions":[{"question","header","options":[{"label","description"}],"multiSelect"}]}`, and **end the turn before building**: no entity, no spec, no dry-run. That is the delivery order, not a conflict with delivering: the spec comes in the turn after the answers, which arrive in `./answers.json` as `{"answers":{"<question text>":"<chosen label or free text>"}}`, the tool's own semantics; continue the path from there. An answer that leaves the choice to you means: take your recommended option and record it as an assumption. Ask this way whenever someone can answer (the user says they are available, or the host relays answers). Only when the host states that nobody will answer, build under the recommended options, run `apply --dry-run` (or `--diff` for an update), list every assumption, and stop; apply only after the user answers.

#### Authoring loop (how to work a tenant without breaking the person next to you)

These rules exist because a correct workflow can still cost the engineer next to you an afternoon: a duplicate workflow under a mistyped alias, alerts and decisions written to a real borrower by live test runs, a task version bumped under a draft being edited in the Hub, and feedback treated as a go-ahead.

**Before the first write.**

- The question path has run and its answers are in (or the host stated nobody will answer).
- `altscore workflows-v2 list --filter is-latest=true` and read every alias and label. If the brief names an alias that is not in that list, STOP and ask which one it meant; a near miss (`validacion-...` vs `validaci-n-...`) is the tenant's real workflow spelled by a human. `apply` refuses this case itself (`APPLY_ALIAS_NEAR_MATCH`, override with `--create-new`); do not reach for the override to make the error go away.
- Every identifier you will write (workflow alias, package alias, borrower-field key, decision key, source id) is either READ from the tenant (`packages list`, `packages content`, `borrower-fields list`, `decisions list`, `altdata describe`) or ASKED. Never derived by analogy.
- Every rule you author cites the line of the brief, or the sibling workflow, it comes from. List the rules that have no source and the decision-key mapping in the question round; they are the ones the user will later call "invented".

**While iterating.**

- Execute in test mode: `workflows-v2 execute <id> --test` (or `execute-by-alias ... --test`). Live runs write alerts and decisions on real borrowers, cannot be deleted (`DELETE /v1/executions` is 405), and are what the client sees in the Hub. A live run happens only when the user asks for one by name.
- `workflows-v2 lock get <alias>` before ANY write to a workflow, or to a task its nodes pin at `taskVersion: null` (a draft always floats to the latest task version, so bumping the task changes the draft under the editor's feet). If `canEdit` is false, stop and report who holds it; never `force-release` a live Hub tab.
- Define before you reference: autosave the custom variable first, then create the task version that selects it. The reverse order leaves the draft pointing at a variable that does not exist and the Hub shows an error to whoever has it open.
- Re-export before you patch (`export <id> --format apply-spec > spec.json`). After the first publish the tenant is the source of truth, not your generator scripts: the engineer renames nodes, moves them, adds nodes in the Hub. Regenerating from your scripts erases that work. `apply` re-adopts Hub-authored nodes by their alias, so a re-exported spec round-trips.
- Human-facing strings (node labels, rule labels and descriptions, PDF section titles, alert messages, HTML headings) are in the client's language with its diacritics: `Cédula`, `Opinión`, `Garantías`, never `Cedula`, `Opinion`, `Garantias`. `lint` and `apply` print a `[diacritics]` advisory when they see the folded form; fixing 72 rule labels afterwards is a script plus 72 PATCHes.
- Group evaluators by business subject (company, legal representative, guarantor, operation), not by which compute node happens to feed them. The PDF section titles are read by a credit committee.
- Mapping tables translate in the direction the DATA dictates: read one real value of the input field first. A borrower field that already holds a code (`corn`) maps code to label, not free text to code. When the sample is null, ask.

**After the first publish.**

- Every further tenant write needs an explicit go in the current message. "Esto es feedback", "what did you do here", "why did you...", a critique or a question is a READ-ONLY turn: answer, propose, wait. Acting on feedback the moment it arrives is what makes an engineer shout STOP while they are editing the same workflow in the Hub.
- Never publish while the user is inside the Hub editor. Ask "are you out of the flow?" before taking the lock, even when `lock get` says it is free (locks expire; tabs do not).
- When the user asks for a table or a block that already exists as data (a package, a task output), read it from the source; do not extend decision logic to carry presentation.

#### Default structure: wire directly, decide in entities, compute only indicators

This is the shape productive tenants run. Write it by default and deviate only for the exceptions in point 3. Two rules are not defaults, `apply` refuses a spec that breaks them:

- Every node `ref` matches `^[a-z0-9][a-z0-9-]*$`: kebab-case (`end-review`, never `end_review`), no underscores, uppercase or spaces.
- Exactly one end node. Every branch converges on it; `decision_key` from the rule-tree carries the outcome, not which end ran.

1. **Wire every input directly in the node that consumes it** (`inputMappings`), by deep path:

   | Value | Path |
   |---|---|
   | workflow input | `inputs.<name>`, `inputs.<object>.<field>` |
   | entity ids | `task_outputs.<customerRef>.borrower_id`, `task_outputs.<dealRef>.deal_id` |
   | altdata | `task_outputs.<altdataRef>.<SOURCE_ID>.data.<field>`; raw nested `....sourceData.<path>`; success gate `....isSuccess` |
   | decision outputs | `task_outputs.<scorecardRef>.total_score`, `task_outputs.<mappingTableRef>.<outputVariable>`, `task_outputs.<ruleTreeRef>.decision_key` (the reason: `....<outputVariable>_rule_label`, `_rule_code`); one breakdown row `task_outputs.<scorecardRef>.score_breakdown[field='<f>'][0].<prop>` |
   | database | `entity.borrower.<group>.<key>` |
   | child workflow | `task_outputs.<childRef>.<key the child's End outputJson emits>`; fan-out `....items[].output`, `....summary.failed` |
   | document extraction | `task_outputs.<ref>.fields.<name>`, `....missingFields`, `....isSuccess` |

   A custom variable that only re-exposes, renames, casts or `.get()`s one of these is never needed. Inputs and outputs of `contact`, `notices`, `document-extraction` and a child workflow: [node-contracts](node-contracts.md).
2. **The decision lives in entities, not in Python.** `decision_key` comes from a rule-tree: hard gates first, then the band. Score bands and code-to-label maps are mapping tables; thresholds are evaluation rules or scorecards, versioned and editable without a republish. Rule-tree, scorecard and evaluate-rules inputs take deep paths directly, so gate on `isSuccess`, a status or a hit count with no variable in between. The usual chain is altdata -> indicator compute -> scorecard -> mapping-table (score to band) -> rule-tree -> End. Create the entities first ([credit-decisioning](credit-decisioning.md)), then reference them from the spec.
   First means after the question path's answers and before the first dry-run.
3. **Python computes indicators, one scalar each.** A variable earns its place when it combines two or more values (ratio, age, date delta, a multi-step formula), aggregates a list, builds a per-party list for a fan-out over several parties (filtering alone is `inputExpression: "inputs.<list>[<field>=<value>]"`; one party is a plain child call, not a one-element batch), parses a raw payload once, normalises text, renders HTML for a PDF block, or is a scoped probe (below). Return a number, string or boolean, never an object; name it `snake_case` after the value; `onError: "null"` and let the rule handle "no data". The sandbox runs the code inside `def execute(input, context)`: `json`, `re` and a `logger` are already there, but `datetime` and `math` are not, so write `import datetime` (or `math`, `unicodedata`) as a statement. It refuses `os`, `sys`, `subprocess`, network and threading modules, and any bare `__import__`, `eval`, `exec`, `compile`, `open` or `print` call, before running anything; one refusal fails every variable of its compute node, and `apply --dry-run` reports it.
4. **Limits productive tenants stay under:** at most 6 variables per compute node, compute nodes at most a third of the graph, no `self.` chain longer than 2, under about 1,000 code chars per variable unless it is HTML, parsing or iteration. Past those the Python is a rules engine; move it into entities.
5. **Never in a variable:** a threshold, a band, a gate, a decision key, a label map, a constant, or a re-check of `isSuccess`.
6. **No data is a code, not a value.** A source that answers without a value puts a sentinel in that field (`-999997` empty, `-999998` not calculated, `-999999` error, as a number or a string, never null), and a failed source puts one in every field. Every comparison misreads it: a negative form (`not_equals "ACTIVE"`, `not_in`) is true, so a gate rejects an applicant nobody could read; a positive form (`greater_than 0`) is false, so a debt gate lets them through; a count or a sum takes it as a number. So:
   - lead each gate with `is_not_altdata_null` on its own field (evaluation rules and conditionals both take it);
   - give absence its own rule, `is_altdata_null` OR `is_null` on the primary field, routed to the review key when the tenant has one, after the gates and before the default, so "could not verify" never falls through to approve;
   - a value outside the field's documented range is missing too, never a number to band; in Python, test for the code before any arithmetic;
   - in a sibling workflow you read, an unguarded gate or count is a finding for the handoff: it turns an unreachable source into a pass.

```json
{"ref": "policy", "type": "rule-tree", "label": "Credit policy",
 "ruleTreeConfig": {"ruleTreeCode": "<policy-tree>", "inputMappings": {
   "bureau_ok":      "task_outputs.bureau.<SOURCE_ID>.isSuccess",
   "score_band":     "task_outputs.score-band.score_band",
   "debt_to_income": "task_outputs.indicators.debt_to_income"}}},
{"ref": "end", "type": "end", "label": "End",
 "inputMappings": {"borrower_id": "task_outputs.applicant.borrower_id",
                   "decision_key": "task_outputs.policy.decision_key"},
 "endConfig": {"decisionConfig": {"enabled": true, "decisionType": "final"}}}
```

`apply` and `lint` print a `[structure]` advisory for each break of these rules, naming the fix.

#### Recommended path: `apply` (declarative create-or-update)

Before writing or changing a spec, run the question path ([Discovery](#discovery-before-authoring-a-guided-question-path-infer-first-then-ask-in-short-rounds)).

For "Create a workflow that does X" — or "Update workflow Y to do Z" — use `workflows-v2 apply`. It takes a single spec and reconciles it against the tenant. Use `--dry-run` first to inspect what will be sent (the dry-run output also tells you which branch will fire: CREATE or UPDATE). It lists every blocking problem at once, each with its fix: fix all of them before the next dry-run.

Use `--diff` to preview changes against the current tenant state before mutating. Pairs well with the UPDATE path — see exactly what version-bump will change before pulling the trigger. The diff renders structural deltas on metadata, nodes (added/removed/changed, matched by task alias), edges, input/customVariables, and any entity-scope conflicts the apply would touch. Read-only: no `/v2/tasks` POSTs, no `/v2/workflows` mutations, no entity PATCHes. Mutually exclusive with `--dry-run` and `--publish`.

**Create vs update semantics.** apply resolves the target by alias:

- `spec.alias` if set, otherwise `slugifyWorkflowLabel(spec.label)`. **Always set `alias`.** Without it the label is the workflow's identity: relabel it and apply creates a second workflow instead of updating the first. apply warns on every run that lacks one and prints the slug to paste in; a future release requires it.
- If **no** workflow has that alias → **CREATE path**: the server creates the tasks and the workflow (DRAFT by default unless `--publish`).
- If a workflow has that alias → **UPDATE path**: the server drafts (adopting an existing DRAFT, or `create-draft --force-recreate` over an ACTIVE version), autosaves the new nodes/edges/variables/config/notes/category/status, and publishes when the target was ACTIVE, all under its own edit lock. Same workflow id, same alias, version increments, schedules and downstream consumers survive. Tasks are matched by `(workflowAlias, specRef)`: an unchanged body is left alone, a changed one is version-bumped under its existing alias.
  - A lock held by an open builder tab makes apply answer 409 naming the holder; the lock apply itself takes lives only for the request. Pass `--force-lock` to take it anyway and discard whatever is unsaved there.
- If NO workflow has the spec's alias but one exists whose alias or label is one typo away (accent folded to a hyphen, one dropped letter), apply refuses with `APPLY_ALIAS_NEAR_MATCH` and lists the candidates with alias, label, status and version. The usual cause is an alias copied from a brief instead of from `workflows-v2 list`; set `spec.alias` to the existing one and apply UPDATES it. `--create-new` is for the rare case where a second, similarly named workflow is really wanted.

Both paths share the same validation + normalize pipeline (`preflightTasks`, per-type normalize, `validateEntityWorkflowAliasMatch`, etc.) so a spec that passes against a fresh tenant also passes when re-applied later.

**apply is server-side (altscore-cli >= v0.34.0; the only path from v0.35.0).** The CLI parses, normalizes and assembles the spec exactly as before, then sends the flat spec ONCE. Borrower Central resolves every spec-local ref to a task alias before writing anything, validates the assembled graph with the same oracle publish uses, creates or version-bumps each task by `(workflowAlias, specRef)`, and creates / drafts+autosaves / publishes the workflow under its own edit lock, all-or-nothing: a rejected spec writes nothing (400 `APPLY_SPEC_INVALID` / 422 `APPLY_VALIDATION_FAILED`, findings printed), a publish-rule rejection leaves the DRAFT and says so (422 `APPLY_PUBLISH_REJECTED`), a lock held by an open builder tab is a 409 naming the holder (`--force-lock` takes it). Re-applying an unchanged spec writes nothing (every task reports `unchanged`); a changed body bumps its task under the SAME alias. stderr shows one line per ref (`ref -> alias (action, vN)`) plus any fields the server dropped. `--dry-run` and `--diff` use the server's plan, so `--diff` matches nodes by their real alias. Write references in the canonical form everywhere (`task_outputs.<ref>...` in strings, a bare ref under `taskAlias`); a spec with an explicit node `alias` is refused before any request (task aliases are server-assigned; `taskAlias` references an existing task). A backend without the endpoint (404/405) is an error; altscore v0.34.x is the last release that still carried a client-side pipeline.

**Long-form refs are rewritten everywhere.** A `task_outputs.<ref>` reference in any non-prose field of a task body (values and map keys, at any depth) is ordered and rewritten to the server alias, including fields the CLI has never heard of. Bare `<ref>.<field>` heads and bare `{{token}}` placeholders are still handled per task type, so a new task type that uses the bare form needs a CLI update; one that uses the long form does not.

**Entity-scope reconciliation (auto-restamp).** After apply succeeds, it walks the spec's referenced credit-decisioning entities and stamps each one's `workflowAlias` to the workflow's alias via `PATCH /v1/{resource}/{id}`. Walked:

- `scorecard` task → scorecard entity → each `rules[*].mappingTableCode` mapping table.
- `rule-tree` task → rule-tree entity → each `rules[*].ruleCode` evaluation rule.
- `evaluate-rules` task → each `rulesConfig[*].ruleCode` evaluation rule.
- `mapping-table` task → each `mappingTableConfig.entries[*].mappingTableCode` mapping table.

Each successful stamp logs to stderr (`# scoped <resource> <code> to <alias>`). Pass `--skip-rescope` to opt out (then a stale scope shows as a hard error in normalize and the agent has to fix it manually).

**Entity ownership rule.** Each v2 workflow owns its credit-decisioning entities 1:1. A workflow's spec must NOT reference an entity whose `workflowAlias` is currently another workflow's alias. If you want the same logic in two workflows, **clone the entity** with a new code (e.g. `kyc-sc` and `kyb-sc` instead of one shared `scoring-sc`). Apply now refuses to silently steal ownership; pass `--allow-steal-ownership` if you really do want to transfer. Codes are unique per workflow, not per tenant: apply resolves a code under the target workflow's alias first (a tree's rules and a scorecard's tables under the parent's alias), exactly as the runtime does, so each workflow may own its own `DR_R001`; only a code no entity under that alias carries falls back to the tenant-wide match and its ownership check.

**Refs vs aliases.** The spec uses `ref` as a stable spec-local key. Edges and inputMappings reference tasks by `ref`, and Borrower Central resolves each ref to its task alias during apply. You can also pass an explicit `alias` on the workflow itself (`spec.alias`) — apply uses it both to resolve the target and to keep `workflowAlias` stable across re-applies. Edges use `from`/`to` as shortcuts for `sourceNodeId`/`targetNodeId`.

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

#### Canonical end-node pattern (one end, fed by the rule-tree)

A spec has exactly one end node (`apply` refuses more). When it contains a `rule-tree` task, feed that end directly from the rule-tree, not through a `conditional` router. BC's end activity promotes four fields from the end task's resolved input context onto the execution record automatically:

| Context key | Execution-record field | Source |
|---|---|---|
| `borrower_id` | `borrowerId` (the customer id) | upstream customer task output, or `inputs.borrower_id` |
| `billable_id` | `billableId` (defaults to `borrowerId`) | upstream customer/deal task output |
| `deal_id` | `dealId` | upstream deal task output |
| `decision_key` | `currentDecision.key` (when `decisionConfig.enabled=true`) | upstream rule-tree task output (`task_outputs.<rule-tree-ref>.decision_key`) |

Wired this way, the rule-tree's per-run decision string flows through to BC's decision recorder, the PDF generates once, and there's no per-branch hand-maintained `outputJson` to drift. Apply ships a non-blocking lint (`lintCanonicalEndNode`) that warns when a spec has both a rule-tree and an end node but the end node lacks `inputMappings.decision_key` or `endConfig.decisionConfig.enabled=true`. A PDF or decision recording explicitly turned off is the user's choice and is not flagged.

> **Defaults (each fills only a field the spec leaves out).** Every mode prints the ones it used in one stderr block, `# defaults applied (confirm with the user or set them explicitly):`. List them in the confirm round before building, and set any the user decides in the spec:
> 1. End `pdfConfig.enabled: true` (also the server's default; set `false` for no report) and `pdfGenerationRequired: true` (a failed render fails the run).
> 2. End `borrower_id` and `billable_id` wired to the single `customer` node's `borrower_id`; with 0 or more than 1 customer nodes it warns and wires nothing.
> 3. Decision recording stays off unless `endConfig.decisionConfig.enabled` is true; enabled without `decisionType` records `final` (server defaults).
> 4. A write task without `persona` writes an `individual`; a deal contact without `identity_value` copies it from its `identity_key` field (default `tax_id`); a source without `packageAlias` stores under its lowercased id (`ECU-PUB-0002` -> `ecu_pub_0002`).
>
> `--no-auto-defaults` turns off 1, 2 and the contact fill; the server's PDF and decision defaults still apply.

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

> **outputJson template syntax.** Bare placeholders like `{{borrower_id}}` and `{{decision_key}}` do NOT resolve in BC's `VariableResolver` -- only `{{inputs.X}}`, `{{task_outputs.X.Y}}`, `{{custom.X}}`, `{{system.X}}`, and bare-alias `{{<alias>.<field>}}` (with a dot). A bare key stays literal, corrupts the rendered JSON, and the runtime silently falls back to the promoted-scope dump (your custom envelope vanishes with no error). The `inputMappings` short-name keys (`borrower_id`, `decision_key`) drive BC's per-context promotion (`result['decision_key']`, `result['borrower_id']` on the execution record) and PDF section enrichment -- but NOT outputJson substitution. Always use the long form (`{{task_outputs.<ref>.decision_key}}`) in outputJson, even when the same key is also in `inputMappings`. To stamp the workflow's own execution id into the output, use `{{system.workflow_execution_id}}` -- the key is `workflow_execution_id`, NOT `execution_id` (see the System scope note under "Variable resolution syntax").

> **Values render as JSON, quoted or not.** Each placeholder becomes `json.dumps` of its value, and a placeholder that fills a whole quoted string drops those quotes. So `"{{task_outputs.<ref>.score}}"` gives the number `720`, a dict gives an object, and `None` (or a path that resolves to nothing) gives a real `null`. To emit null, leave the value `None`: the text `'null'` comes out as the string `"null"`, and `''` stays an empty string. A placeholder inside a longer string (`"Score: {{...}}"`) breaks the JSON when its value is a string, and the output silently falls back to the scope dump.

> **DRAFT vs publish (test before you publish).** apply's CREATE path saves workflows in `status: "DRAFT"` by default — mirrors the Hub's "save-then-publish" editor flow. A DRAFT executes its **full graph faithfully** — node execution is never gated by status (the engine reads nodes straight from the workflow doc), so a DRAFT run is a real, complete test run. Publishing only changes which version the **alias** serves (the alias resolves to the latest `ACTIVE` version). So to validate before going live, execute the DRAFT **by its workflow id** (or by alias in test mode) — you do **not** have to publish untested work to run it. Pass `--publish` on apply when you *do* want it live immediately; the UPDATE path always publishes (apply treats the spec as desired state). `workflows-v2 execute` prints an informational stderr note when the targeted version isn't `ACTIVE`; pass `--skip-status-check` to silence it.

> **Canvas auto-layout (on by default; `--no-layout` to skip).** apply positions nodes in left-to-right columns using the same algorithm as the Hub builder's **Align** button: longest-path leveling (every edge points forward, so a diamond's short arm never draws backwards into the join) plus barycenter row ordering to reduce crossings. Do NOT hand-write `position` on spec nodes — pinning it on *any* node disables auto-layout for the whole graph (apply says so on stderr) and you inherit responsibility for the entire canvas. Positions are recomputed on every apply, including the UPDATE path, so a re-applied spec always opens tidy.

If the agent asks for help building or updating a workflow, run the question path first, then default to apply. Other paths exist for special cases:

- **Clone-and-modify** (highest success when a similar workflow exists): `export <similar-id> > wf.json` → edit JSON → `import --new-label "..."`. Import validates the bundle against the destination tenant before writing and refuses if it references a scorecard/rule-tree/mapping-table/evaluation-rule that is neither there nor in the bundle.
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
  "sourcesConfig":[{"sourceId":"ECU-PUB-0002","version":"v1","packageAlias":"ecu_pub_0002"}],
  "borrowerIdField":"personId",
  "inputSchema":{"personId":{"type":"string"},"taxId":{"type":"string"}},
  "inputKeys":{"personId":"{{personId}}","taxId":"{{taxId}}"},
  "inputMappings":{"personId":"inputs.borrower_id"},
  "mode":"single","savePackages":true,"timeout":60
}'

# Bump version (existing nodes pinning v1 are unaffected). A DRAFT floats on the
# latest version, so this EDITS every draft whose graph points at the task: the
# server applies their edit lock and answers 423 when a Hub tab has one open,
# including your own tab (the CLI identifies itself as a separate editor via
# X-Lock-Client-Id). `workflows-v2 lock get <wf>` first; pass --lock-token to
# write as the holder. Define a custom variable (autosave) BEFORE the version
# that selects it, never after.
altscore tasks-v2 create-version fetch-ecu --body '{
  "label":"Fetch ECU bureau v2",
  "type":"altdata-enrichment",
  "sourcesConfig":[
    {"sourceId":"ECU-PUB-0002","version":"v1","packageAlias":"ecu_pub_0002"},
    {"sourceId":"ECU-PUB-0014","version":"v1","packageAlias":"ecu_pub_0014"}
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

Per-type config lives in `altscore workflows-v2 schema-guide tasks <type>`: the hand-written notes and traps for that type plus its fields introspected from the backend model, served live. This file does not restate it, because a copy drifts from the runtime. Two nesting traps are worth keeping in view because the API accepts the wrong shape with a 201:

- `category`: every field nests INSIDE `categoryConfig`; a top-level `operation` is silently swallowed by the customer/deal/asset field of the same name.
- `end`: PDF generation is `endConfig.pdfConfig` on the end task, not a task type.

> **Retired types — refused for NEW authoring only.** `create-alert`, `create-borrower`, `create-identity`, `data-store`, `end-old`, `fetch-borrower-entities`, `fetch-entity`, `html-template`, `pdf-report`, `soap`, `update-borrower`, `update-borrower-name`, `webhook`. BC still parses workflows that already use them, and it refuses one only when the incoming graph ADDS it — diffed against the stored graph, so a workflow that already carries such a node stays editable and can be migrated off it. Every CLI path mirrors that rule: `apply` refuses a retired type absent from the target and carries forward one the target already holds (warning on stderr, non-fatal); `tasks-v2 create-version` treats a bump that keeps an already-retired type as a carry-forward; `add-node --type` and `tasks-v2 create` are unambiguously new authoring and always refuse. The refusal is compiled in, so it holds offline. Replacements: `create-borrower`/`update-borrower` → `customer` with `operation: "write"`; `fetch-entity`/`fetch-borrower-entities` → `customer` or `deal` with `operation: "read"`; `create-identity` → the entity-specific tasks; `data-store` → `data-store-write`/`data-store-query`; `pdf-report` → `endConfig.pdfConfig` on `end`; `soap` → `http`. The rest have no replacement.

For `customer` / `deal` / `asset` write tasks the `sourcesConfig` entries map each persisted attribute to a context key. The value always comes from `context[entry.key]` (declare it in `inputSchema`, feed it through `inputMappings`); there is no `value` or `template` field. Common entry shapes:

- `{type: "deal_field", key: "<context_key>", label: "..."}` — write one deal_field record per entry.
- `{type: "identity", key: "<identity_key>"}` / `{type: "borrower_field", key: "..."}` for customer writes; `{type: "asset_field", key: "..."}` for asset writes.

Each node's full `type` vocabulary and its silent-drop rules are under `schema-guide tasks <customer|deal|asset>` → `sourcesConfig`.

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
altscore workflows-v2 lock get my-wf                 # FIRST: canEdit false = someone is in the Hub, stop and say so
TOKEN=$(altscore workflows-v2 lock acquire my-wf | jq -r .lockToken)   # --client-id defaults to cli-<profile>-<host>-<pid>

altscore workflows-v2 lock heartbeat my-wf --lock-token "$TOKEN"   # every ~60s during long edits

altscore workflows-v2 autosave <id> --lock-token "$TOKEN" --last-known-version 3 \
  --body '{"label":"Renamed","nodes":[...],"edges":[...]}'

# Publishing while you hold the lock REQUIRES the token, or the backend answers
# 423 LOCKED -- including for the lock's own holder. A successful publish
# releases the lock server-side, so skip the release below in that case.
altscore workflows-v2 publish <draft-id> --lock-token "$TOKEN"

altscore workflows-v2 lock release my-wf --lock-token "$TOKEN"

# Inspect / unstick
altscore workflows-v2 lock get my-wf
altscore workflows-v2 lock force-release my-wf       # destructive, not elevated: workflows.write
```

`autosave` returning 409 means concurrent modification — re-fetch with `get`, merge, retry.

**423 LOCKED on autosave or publish** means a lock exists whose token you did not
present. Either pass `--lock-token`, or find out who holds it with `lock get`.
Read `renewCount`, not `lockId`: a frozen `renewCount` with `expiresAt` at
`lockedAt`+300s is an abandoned lock nothing is renewing, while a climbing one is
a live Hub tab. `lockId` survives a same-tab re-acquire, so it never proves the
lock is the same grant.

All lock subcommands accept an alias **or** an id; an id is resolved to its alias first, because locks are alias-keyed.

#### Lifecycle

```bash
altscore workflows-v2 create-draft <active-id>                   # branch off ACTIVE
altscore workflows-v2 publish <draft-id>                         # DRAFT -> ACTIVE (no lock held)
altscore workflows-v2 publish <draft-id> --lock-token "$TOKEN"    # when you hold the edit lock
altscore workflows-v2 revert my-wf <version-id>                  # restore prior version into a draft
altscore workflows-v2 revert my-wf <version-id> --mode publish   # replace ACTIVE directly
altscore workflows-v2 archive <id>                               # archive all versions
altscore workflows-v2 restore <id>                               # un-archive
altscore workflows-v2 duplicate <id> --new-label "Copy of X"
altscore workflows-v2 set-visibility my-wf --show-in-deal=true    # per-surface flags: --hidden, --show-in-customer (default true), --show-in-deal (default false)
altscore workflows-v2 set-visibility my-wf --hidden=true          # every version of the alias; survives publish/revert
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
altscore workflows-v2 import --body @my-wf.json --new-label "Imported Copy"
altscore workflows-v2 import --body @my-wf.json --new-label "Light" --skip-evaluation-rules --skip-scorecards
```

#### Execute

> **Body shape — sync vs batch differ.** `execute` and `execute-by-alias` take a **flat** object whose keys are the workflow's `inputVariables` directly (`{"borrower_id":"abc"}`). `execute-batch` takes a **wrapped** object (`{"inputs": [{...}, {...}]}`). Wrapping a sync call (`{"inputs": {...}}`) returns HTTP 400 `Required variable '<name>' is missing` because the resolver looks for top-level keys. Despite the runtime variable namespace being `inputs.<name>`, the request body is flat — the namespace is added server-side after parsing.

```bash
altscore workflows-v2 execute <id> --body '{"borrower_id":"abc"}'                          # sync, flat keys
altscore workflows-v2 execute <id> --body '{...}' --execution-mode async --tags smoke      # async returns executionId
altscore workflows-v2 execute-by-alias my-wf latest --body '{...}'

# A sync run blocks until the graph finishes; the CLI waits up to 10 minutes for
# the server to answer. If it still reports "timeout awaiting response headers",
# the execution was ACCEPTED and is running: the error names the command that
# finds it. Prefer --wait for anything that takes more than a few seconds.

# Test mode -- the DEFAULT while authoring. Marks the WHOLE run non-billable +
# hidden from metrics/default lists; --test injects the "test" tag (BC sets
# is_test=true -> is_billable=false). A live run writes alerts and a decision
# on the borrower that the client sees and that cannot be deleted afterwards
# (DELETE /v1/executions is 405). Run live only when the user asks by name.
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
altscore workflows-v2 sources-status --country ECU   # source versions, compact; --full adds outputSchema
altscore workflows-v2 external-sources-status

altscore workflows-v2 ai suggest-mappings --body '{
  "fields":[{"name":"borrower_id","type":"string"}],
  "availableOutputs":[{"source":"taskOutput","type":"string","taskAlias":"fetch","outputName":"id"}]
}'
```

#### Ergonomic builder helpers (for INCREMENTAL EDIT path)

Before changing a workflow this way, run the question path ([Discovery](#discovery-before-authoring-a-guided-question-path-infer-first-then-ask-in-short-rounds)). These mutate an existing workflow in place. Each handles fetch + lock + autosave internally. **Prefer `apply` for any non-trivial change** — apply is declarative, re-runs the full validation pipeline, and reconciles entity scopes; these helpers are best used for one-off tweaks or interactive exploration where you don't want to maintain a spec file.

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

Before writing or changing a spec, run the question path ([Discovery](#discovery-before-authoring-a-guided-question-path-infer-first-then-ask-in-short-rounds)). The workflow body is permissive — it'll save with bad refs and fail at execute time. Verify external references exist first:

```bash
altscore altdata describe <SOURCE_ID>                        # altdata-enrichment refs (canonical)
altscore evaluators list --filter alias=<ALIAS>              # evaluate-rules refs
altscore data-models list --filter key=<KEY>                 # custom field refs
altscore api GET "/v1/rules?alias=<ALIAS>"                   # rule refs
```

`altdata describe` is the one-shot pre-flight: metadata, versions, required input fields and top-level output keys in one call; `altdata dictionary <id>` adds every output field.

#### Variable resolution syntax (templates and mappings)

The runtime resolver accepts these leading namespaces — anything else fails with `Unknown variable namespace`:

| Scope | Syntax | Example |
|---|---|---|
| Workflow input | `inputs.<name>` | `inputs.borrower_id` |
| Task output (top-level) | `task_outputs.<taskAlias>.<field>` | `task_outputs.fetch.sources_output_packages` |
| Task output (deep path) | `task_outputs.<taskAlias>.<deep>.<path>.<to>.<field>` | `task_outputs.fetch.ECU-PUB-0002.data.pdEc_sri_esActivo` |
| Custom variable | `custom.<name>` | `custom.normalized_score` |
| System | `system.<key>` | `system.workflow_execution_id` |
| Indexed by type | `task_outputs_by_type.<taskType>[<idx>].<field>` | `task_outputs_by_type.altdata-enrichment[0].result` |
| Entity (DB read at run time) | `entity.<root>.<group>.<key>[.<subkey>]` | `entity.borrower.identities.tax_id`, `entity.<ref>:relpick-<id>.points_of_contact.email` |

**`entity.*` reads the database, not a task output.** The workflow passes the value through verbatim and the CONSUMING node resolves it when it runs (`StandardActivity.run` -> `EntityResolver`); a null with no mapping default drops the input. Roots: `borrower` / `deal` (the workflow's primary ones) or `<ref>:<handle>` — the contact behind ONE per-item handle of a `relationships` / `deal` node (`rel-<id>` and `relpick-<id>` resolve to that item's `contact_id`, `deal-<id>` and `dealpick-<id>` to its `borrower_id`). Groups: `identities.<key>`, `borrower_fields.<key>[.amount|.currency]`, `points_of_contact.<email|phone>` (the primary one per method), `addresses[.<field>]` (the first address; `street1`, `city`, `zip_code`, `lat`... or the whole dict when keyless), `metrics.<key>`, `step[.<field>]`, `attributes.<persona|label|external_id>` (built-in fields of the borrower record itself, not data models: `persona` is `individual` | `business`, the person type; any other field reads null), `deal_fields.<key>` (deal roots only) and `id` (the entity's own id, no read). A SENSITIVE identity is decrypted here with audit and stored redacted in the execution record; every task output, the customer read node's included, carries `__sensitive__` instead. Write the spec REF in the root: `apply` remaps `entity.<ref>:` to the server alias like `task_outputs.<ref>`. The whole mapping value must be the reference; it is not interpolated inside templates or `compute-variables` Python. Full contract: `altscore workflows-v2 schema-guide mappings` -> `entityReferences`.

**`documents.<key>.base64` (also `.fileName`, `.mimeType`, `.extension`, `.sizeBytes`, `.files`) is not a resolver namespace but an `http`-task second pass:** it is only valid inside that task's `body` for a `<key>` declared in the task's `documents` list, the http activity fills it in late exactly like End's `{{self.pdf_url}}`, and `apply` passes it through untouched.

**System scope keys — the execution id is `workflow_execution_id`, NOT `execution_id`.** There is no `system.execution_id` key; referencing it resolves to `None` and the `{{...}}` token is left in place literally (so `"{{system.execution_id}}"` survives verbatim into `custom_output`). The keys that exist include `workflow_execution_id`, `tenant`, `executed_by`, `execution_batch_id`, `primary_borrower_id`, `primary_deal_id`, and `workflow_start_time`.

**This table resolves in TEMPLATE contexts only — outputJson `{{...}}` placeholders and node `inputMappings`. It does NOT reach `compute-variables` Python.** A `compute-variables` node's `inputs` dict is built solely from its declared `inputs.` / `task_outputs.` / `custom.` dependencies; the runtime has no `system` branch, so a `system.*` dependency passes publish validation but always resolves to `None` at runtime (this is why even a trivially-present key like `system.tenant` reads `None` from `inputs.get(...)`). To use the execution id inside `compute-variables`, wire `{{system.workflow_execution_id}}` into a normal node's `inputMappings` and read it back via `task_outputs.<alias>.<field>`.

**Deep paths into altdata output**: an `altdata-enrichment` task outputs the entire package object on `sources_output_packages`. To map a single field into a downstream conditional/compute-variables task, use the deep form: `task_outputs.<altdataAlias>.<sourceId>.data.<fieldName>`. No intermediate compute-variables required.

**Direct assignment is the default** (see "Default structure" above): wire a reference straight into the consuming node's `inputMappings` instead of re-exposing it through a variable. **Exception — scoped probes are NOT passthroughs:** a single-handle-scoped probe node whose var reads a per-item scalar (`result = inputs.get("task_outputs.attach-deal.deal_contact_id")`) is the *required* way to surface a scoped value (see "The scoped compute-variables pattern" below) — direct assignment would lose the per-item scope.

**Bare `<alias>.<field>` resolves fine** at runtime — the backend resolver accepts the bare-alias form (WITH a dot), and `apply` deliberately emits it for cross-task references. Both `task_outputs.<server-alias>.<rest>` and the bare `<alias>.<rest>` form are valid.

**Multi-dot inputMappings are accepted on create**: a single POST lands the task at version 1, and the workflow node references version 1.

#### Secrets (API keys and stored credentials)

**There is no `secrets.` namespace.** `secrets.MY_KEY` is not a scope — it parses as a bare reference whose first segment is `secrets`, resolves to `None`, and fails silently. Same for `inputs.MY_KEY` unless a workflow input variable of that name actually exists.

Secrets live in a **tenant-wide** store (`/v1/stores/secrets`), not on the workflow. The Hub surfaces it as the "Secrets" card on the workflow *detail* page, which makes it look workflow-scoped — it is not; every workflow in the tenant sees the same entries. Each entry is one `secretId` holding `{"value": "<the secret>"}` (the Hub's dialog always writes the `value` key, and the runtime reads exactly that key).

```bash
altscore api GET  /v1/stores/secrets
altscore api POST /v1/stores/secrets \
  --body '{"id": "openai-api-key", "secret": {"value": "sk-..."}}'
```

`ALTSCORE_CLIENT_ID` and `ALTSCORE_CLIENT_SECRET` are reserved key names and rejected.

**To read a secret in any node, declare it as a `secret`-typed inputSchema field whose `default` is the secretId:**

```json
"inputSchema": {
  "OPENAI_API_KEY": { "type": "secret", "default": "openai-api-key" },
  "prompt":         { "type": "string" }
}
```

At runtime `resolved_secret_inputs()` looks the secretId up in the tenant store and merges the secret's **value** into the context the activity receives, before the node body runs. The field takes no `inputMapping` — its value comes from `default`.

In a `compute-variables` expression, reference it as a **bare** dependency name — never `secrets.` or `inputs.`:

```json
"dependencies": ["OPENAI_API_KEY", "inputs.prompt"],
"expression":   "key = inputs[\"OPENAI_API_KEY\"]\nresult = call(key, inputs[\"inputs.prompt\"])",
"returnValue":  "result"
```

Caveats worth knowing before you reach for this:

- **`http` nodes should not use it.** They have first-class secret fields (`user`, `password`, `token`, `secret`) — set the field to the *secretId* and the runtime dereferences it. That path is the one the Hub UI supports (its editors have a secret picker).
- **No Hub UI can author a `secret`-typed inputSchema field.** Every type dropdown in the builder offers `string|number|integer|boolean|object|array` only, so this shape is CLI/API-authored and the builder will not show a picker for it.
- **The validation oracle flags the bare dependency.** `POST /v2/workflows/validate` reports `CUSTOM_VAR_DEPENDENCY_UNRESOLVED` because the name matches no workflow input, custom variable, or task alias. It still resolves correctly at runtime — treat that one finding as a known false positive.
- **A workflow-level `inputVariables` entry with `type: "secret"` is a dead end.** The type is accepted, but nothing dereferences it against the secret store, so the run sees the literal secretId string. Only the task `inputSchema` path resolves.
- **Secrets are not written to the task-execution record**, but a compute-variables failure logs a preview of each resolved input at error level — so a failing expression can put the first 32 characters of a key in the logs. Don't hold secrets in variables you also log.

#### Dispatch a batch without waiting for it (`dispatchMode: "async-batch"`)

A `child-workflow` node normally dispatches **and awaits** every child inside one activity, so the
parent blocks and is bounded by a 600s activity budget, a 300s heartbeat and a single attempt --
roughly 400 children in practice. When the parent does not need the results in the same run (a
portfolio re-score fed by an ERP, say), `dispatchMode: "async-batch"` hands the whole list to the
platform batch engine and returns the batch id immediately.

```jsonc
{"ref": "rescore-cartera", "type": "child-workflow",
 "executorId": "preaprobacion-v2",
 "runInBatch": true,
 "dispatchMode": "async-batch",
 "invalidRowPolicy": "fail",              // or "skip"
 "inputExpression": "task_outputs.build-rows.items"}
```

Returns a **dispatch receipt**, not results:

```jsonc
{"executionBatchId": "batch_...", "status": "dispatched",
 "total": 4200, "dispatched": 4200,
 "rejectedCount": 0, "rejectedTruncated": false, "rejected": []}
```

`invalidRowPolicy` decides what happens when a row fails validation against the CHILD's
`inputVariables`: `fail` (default) refuses the node and creates **no batch**, so a bad feed cannot
half-run; `skip` dispatches the valid rows and reports the rest in `rejected`. Note the validator
rejects a **missing** required key, not an empty string. An empty list, or every row rejected under
`skip`, creates no batch at all and returns `status: "empty"` / `"rejected"` with a null id.

Follow the batch:

```bash
altscore execution-batches get <executionBatchId>
altscore executions list --filter execution-batch-id=<executionBatchId>
altscore execution-batches list --filter parent-execution-id=<parentExecutionId>
```

#### Gotchas (v2 specific)

- **Tasks first — for every node.** Even `start`/`end` need a backing `/v2/tasks` record. Hub workflows use trivial type-only tasks for those (`{"type":"start","label":"Start"}`). The API saves orphan-node bodies, but the Hub then hits `GET /v2/tasks/null` 404 on render.
- **Field names are camelCase.** `nodeId` not `id`, `sourceNodeId` not `source`, `targetNodeId` not `target`. Snake-case will return 400.
- **Lock first.** `update` works without a lock but races with the Hub UI. Prefer `autosave` with `--lock-token`.
- **`lastKnownVersion`** is the antidote to silent overwrites. Always pass it on `autosave` if you fetched the workflow earlier in the session.
- **Alias is derived from label on create.** Two workflows can't share an alias — 409 on collision. Use `duplicate --new-label` or `import --new-label` to disambiguate.
- **`schedule preview/validate`** don't take a workflow ID. Standalone cron checkers.
- **`execute --execution-mode async`** returns only `executionId`. Poll `executions <id>` for status.
- **`ai suggest-mappings`** returns 503 when the tenant has no LLM configured. Treat as a soft failure.
- **`secrets.<name>` is not a scope.** To read a stored secret in a node, declare a `secret`-typed `inputSchema` field whose `default` is the secretId — see "Secrets" above.
- **`dispatchMode: "async-batch"` needs `inputExpression`.** Async dispatch batches over a list; without one the node resolves a dict and fails at runtime, so preflight refuses it. `maxConcurrency` and `failurePolicy` are INERT in that mode (the platform batch owns concurrency and per-row failure) and preflight warns if you set them. `invalidRowPolicy` is read only in that mode.
- **An async-batch node's children are invisible to the run that launched them.** `executions list --filter parent-execution-id=<parent>` returns NOTHING for one, because the batch engine creates its own rows. Go parent -> batch (`execution-batches list --filter parent-execution-id=`) -> rows (`executions list --filter execution-batch-id=`). Note the executions query name is `execution-batch-id` while the response field is `batchId`; `--filter batch-id=` is not a real filter and is silently ignored, so it returns the whole unfiltered list.
- **The results email of an async-batch dispatch comes from the CHILD workflow's `batchPolicy`**, read live when the batch finishes -- not from the node and not from the parent workflow. A batch can therefore mail recipients the parent's author never configured; the node emits an info notice when that is the case.
- **Preflight warns instead of blocking on an unverifiable enum value.** When a task type / workflow category / relationship kind / inputSchema type is absent from this build's compiled-in list *and* the backend's meta endpoint can't be reached, `apply` prints a warning and proceeds rather than rejecting: the CLI cannot tell "invalid" from "newer than me" without the backend, and the backend validates all four on write. A value a *reachable* backend disowns is still a hard error.



#### Atomic deal write with customer + N guarantors

A single `deal` task can attach the customer plus an arbitrary number of guarantors atomically. Pattern: a single-mode child-workflow KYCs the customer, a batch child-workflow KYCs the guarantors, a `compute-variables` task assembles the full contacts list, and one `deal` task with an inline `contacts` field writes them all in one shot. (Contacts are attached ONLY via the inline `contacts` field — the legacy `deal_contacts` `sourcesConfig` type is no longer supported and is rejected by `apply`.)

```jsonc
"nodes": [
  // ... start node + any upstream tasks ...
  // A parent sees only the child's End outputJson: the reads of verify-customer below need
  // that child to emit borrower_id there.
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

| Source node | Mode | Item field | Handle per item |
|---|---|---|---|
| `relationships` | write (default) | `relationshipsConfig.items` | `rel-<id>` |
| `deal` | write (default) | `contacts` | `deal-<id>` |
| `relationships` | `operation: read` | `readRelationshipsConfig.picks` | `relpick-<id>` |
| `deal` | `operation: read` | `readDealContactsConfig.picks` | `dealpick-<id>` |

The `<id>` is the item's `id` within the array (e.g. the first item → `deal-0` / `rel-0`). A READ-mode **pick** is an author-defined selector (`relationship` + `isLegalRepresentative` + `take: highest|lowest` on relationships; `role_key` + `is_primary` + `take: oldest|newest` on deal) that resolves to ONE existing relationship / deal contact, or to none: an unmatched pick still emits its handle, with `found: false` and null scalars.

**Scoped output keys differ per node type** — a common trap. When `task_outputs.<sourceRef>` is scoped to one item, the keys you can read are:

| Scoped source | Keys available on the scoped item dict |
|---|---|
| `relationships` item (`rel-`) | `contact_id`, `relationship_id`, `relationship`, `ownership_pct`, `is_legal_representative`, `is_active`, `priority` |
| `deal` contact item (`deal-`) | `deal_id`, `borrower_id`, `deal_contact_id`, `role_key`, `is_primary` |
| `relationships` pick (`relpick-`) | `found`, plus the relationship item keys above (null when `found` is false) |
| `deal` contact pick (`dealpick-`) | `found`, plus the deal contact item keys above (null when `found` is false) |

> **No `contact_id` on a scoped deal item.** A deal contact item exposes `borrower_id` (the party being attached) and `deal_contact_id` (the DealContact join-row id) — NOT `contact_id`. Reading `task_outputs.<dealRef>.contact_id` off a scoped deal item returns None. Use `borrower_id` for the party and `deal_contact_id` for the join row. (Relationships items are the inverse: they DO carry `contact_id`, plus `relationship_id`.)

> **The item dict identifies the contact; it does not describe them.** Names, tax ids, email, phone, birth date and address of the matched contact are NOT keys of the item or pick dict, and you do not need a `customer(read)` subflow behind a `compute-variables` glue node to get them. Read them on the consuming node through the entity scope, root `<ref>:<handle>`: `{"lr_curp": "entity.relaciones:relpick-_TcuAUWL.identities.curp", "lr_email": "entity.relaciones:relpick-_TcuAUWL.points_of_contact.email", "lr_birth_date": "entity.relaciones:relpick-_TcuAUWL.borrower_fields.birth_date", "lr_city": "entity.relaciones:relpick-_TcuAUWL.addresses.city"}`. This works on a node scoped through the handle AND on a node that converges from several picks (the End node, typically), because the resolver reads `items_by_handle.<handle>` explicitly — so one End can lay out the legal representative's and the main contact's data side by side. It is also the only path that decrypts a sensitive identity (with audit); the customer read node returns `__sensitive__`. See "Variable resolution syntax" above.

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

**Every custom variable needs `dependencies` and `returnValue`, not just `expression`.** The runtime injects ONLY the declared `dependencies` into the expression's `inputs` dict (it never parses the expression) and wraps the code with `return <returnValue or None>` — a variable stored with only an `expression` runs with `inputs = {}` and outputs null on every execution, silently. BC's pre-flight/publish validation rejects this shape (`CUSTOM_VAR_UNDECLARED_REFERENCE`, `CUSTOM_VAR_MISSING_RETURN_VALUE`, blocking; `CUSTOM_VAR_UNDECLARED_REFERENCE_WITH_DEFAULT` warning) — the error's `params.suggestedDependencies` / `params.suggestedReturnValue` carry the exact fix to write into the spec.

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

For a `relationships` source the only changes are: use `sourceHandle: "rel-0"`, and read the relationships scoped keys (`contact_id`, `relationship_id`, `relationship`, `ownership_pct`) instead of the deal ones. For a READ-mode pick use `relpick-<id>` / `dealpick-<id>` and check `found` first.

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

