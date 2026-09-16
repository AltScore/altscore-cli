### KYC / KYB good habits

Tenant- and country-agnostic structure for onboarding and screening workflows. These are *habits*, not a template: durable principles about how to shape a KYC/KYB/onboarding flow, distilled from production workflows across tenants. Country specifics — which data sources, which thresholds, which identity formats — live in versioned decision artifacts and altdata source ids, never in these habits.

Read this **before authoring** a KYC/KYB/onboarding workflow. For the mechanics of *how* to build the nodes, see [workflows-v2](workflows-v2.md); for scorecards/rule-trees/mapping-tables, see [credit-decisioning](credit-decisioning.md).

#### Architecture

- **Separate orchestration from screening.** An orchestrator owns entity creation, party fan-out, aggregation, and the final decision; a per-party *child* workflow does the single-subject enrichment + screening. The child stays reusable across products and countries; the orchestrator holds the business logic. (Child via a `child-workflow` node's `executorId`; fan-out via `inputExpression: "inputs.<collection>[<field>=<value>]"` + `runInBatch`.)
- **One child run per party, split by subject type.** Individuals and legal entities need different sources and different rules — branch on persona/entity-type and fan out one child per party. Run the fan-out **best-effort** (`failurePolicy: best-effort`, `continueOnFailure`): one party failing to enrich must not sink the whole application.
- **Model the entity graph explicitly:** the *subject* (customer node), its *related parties* (relationships — UBOs, legal reps, guarantors), and the *application* (deal). Add a relationships node **only when you actually screen those parties** — don't model relationships for decoration. (Canonical entity-level KYB often scores the company directly with no relationships node at all.)

#### Identity & idempotency

- **One stable identity key per subject type, used consistently** — a national tax id for entities, a national person id for individuals. Resolve/upsert by identity, never by name.
- **Make every write idempotent.** Deterministic keys (identity for borrowers, `external_id` for deals) mean re-runs reconcile instead of duplicating — these nodes are all find-or-create. Treat re-running a flow as safe by default.

#### Enrichment

- **Screen sanctions / PEP / adverse-media for *every* onboarded party** — KYC and KYB, primary and related. Beneficial owners and guarantors get screened too, not just the applicant.
- **Distinguish "source failed" from "source said no."** Gate on an explicit success flag (`isSuccess`), not on data-presence — a timeout must not read as a clean result.
- **Keep raw data separate from derived signals.** Extract raw fields in one step, normalize them into indicators/signals in the next. Rules then depend on portable indicators, not on a specific provider's response shape — which is what lets the same logic move across countries. (Two-stage `compute-variables`: `extract-raw` → `derive-indicators`; expressions live in `workflow.customVariables`.)

#### Decisioning

- **Two layers, never one.** A numeric **score** (creditworthiness/risk) *and* a set of **hard gates** (sanctions hit, dissolved entity, identity mismatch, revoked document → reject regardless of score). Don't bury a hard stop inside a score. (Scorecard for the number; rule-tree for the gates.)
- **Keep country/tenant logic inside versioned decision artifacts**, bound by code. The workflow graph stays country-agnostic; locale-specific thresholds live in the scorecard/rule-tree/mapping you can version and audit.
- **Emit a structured, explainable decision** — a `decision_key` *plus* the reason/rule that fired — and write it back onto the deal/entity. Mark only the authoritative step as the final decision (`decisionType: final`); children and dispatchers stay `preliminary`/disabled.

#### KYB adds (beyond KYC)

- **KYB = screen the entity *and* its people.** Run KYC on the beneficial owners and legal representatives; attach them as relationships carrying `ownership_pct` and `is_legal_representative` when you screen them.
- **Entity status is a hard gate.** Active / dissolved / suspended / blacklisted is a reject condition, checked before scoring.

#### Compliance & ops

- **Minimize PII in outputs** — emit ids, decisions, scores, and reason codes, not raw identity documents.
- **Mark test runs as test** (`execute --test`) so proofs don't pollute production entities or metrics. Side effects still run; the run is just non-billable and hidden from metrics.
- **Leave an audit trail:** the decision and the rule that produced it persist on the entity.

#### Quick checklist before publishing

- [ ] Orchestrator and per-party child are separate workflows.
- [ ] Parties fan out by persona/entity-type, best-effort.
- [ ] Consistent identity key per subject type; all writes idempotent.
- [ ] Sanctions/PEP screening on every party, primary and related.
- [ ] Source failure distinguished from a clean negative (`isSuccess`).
- [ ] Score and hard gates are separate layers.
- [ ] Decision is structured (`decision_key` + reason) and written back; `final` only on the decider.
- [ ] KYB: UBOs/legal-reps screened as relationships; entity-status gate present.
- [ ] No raw PII in outputs; test runs marked test.
