### Migrating a workflow off the legacy v1 engine

Triggered by anything shaped like "I want to migrate", "port this workflow to v2", "move <tenant> off the old engine", "migrate the workflows in <repo>". The person asking is a delivery engineer who knows the client; they usually do not state the tenant, the workflow list or the repo path, because to them those are obvious. Get them in one intake round instead of guessing, then follow the doctrine the backend serves.

**The doctrine is not in this file.** It is served live:

```bash
altscore workflows-v2 schema-guide v1Migration
```

Fourteen keys, every claim carrying a borrower-central source citation, updated server-side without a CLI release. Read it before you ask anything — it also tells you what NOT to ask, because it already specifies the order of work, the entity mapping, bucket semantics, the parity harness and replay safety. Do not hand-author a parallel playbook; it drifts from the served one.

#### Step 0 — pre-flight, before any question

Silent checks. None of these is a question for the user unless it fails.

```bash
which altscore                  # installed? (SKILL.md has the install line)
altscore config                 # which profile/tenant is active, and is there a token
altscore profiles list          # every tenant this machine can reach
```

`altscore config` printing a `tenant_id`, a `client_id` and `"has_token": true` means you are logged in and the tenant question below becomes a confirmation rather than an open one.

If there is no profile, or the CLI answers 401 and the refresh fails, say this rather than trying to work around it:

> The CLI is not logged in for that tenant. `altscore login` needs a **client_id** and a **client_secret** — the tenant's API credentials, issued per tenant in the Hub under the tenant's API access settings. Ask whoever owns the tenant if you do not have them, then:
>
> ```bash
> altscore login                              # interactive, needs a terminal
> altscore login --profile <tenant-shortname> # keeps it alongside your others
> altscore config                             # confirm tenant_id and has_token
> ```

`altscore login` reads from stdin, so it cannot be driven from a background job or `-p` print mode — the user has to run it themselves. Tokens auto-refresh on 401 afterwards; there is no manual refresh step.

#### Step 1 — infer, so the questions have real options

Every one of these is cheaper to read than to ask, and it turns the intake from an interrogation into a confirmation. Options built from these reads are also how the user catches your wrong assumption in one click.

| What | Where |
|---|---|
| Which tenants are reachable, which is active | `altscore profiles list`, `altscore config` |
| Whether v2 work already exists there | `altscore workflows-v2 list --filter is-latest=true` — an in-flight migration has DRAFTs |
| What v1 is still running | `altscore workflows list`, and the v1 executions behind it (they are the parity corpus) |
| Which workflows the legacy repo defines | the repo itself, once you know where it is: the v1 evaluator modules and their entry points |
| Sources, decision keys, data models available in the target tenant | `sources-status --status active`, `decisions list`, `data-models list` |
| The tenant's house style for output shape and write targets | `workflows-v2 export <closest sibling id> --format apply-spec` |

#### Step 2 — the intake round (AskUserQuestion)

Two rounds, not one, and the split is deliberate: **the legacy repo path has to be answered before the workflow list can be a list.** Asking "which workflows?" with no repo in hand gets you free text and a typo; asking it after gets you checkboxes of real module names.

**Round 1 — at most 3 questions.** Recommended option first, labelled `(Recommended)`.

| Question | Options to build |
|---|---|
| Which tenant is this migration for? | the active profile from `altscore config` first, labelled `(Recommended)`; then the other names from `altscore profiles list`. Skip the question entirely when the request already names a tenant that matches a profile. |
| Where does the legacy v1 repo live? | any checkout you can see on disk near the working directory; the path they last used if it is in the conversation; `(Other)` covers a path you cannot guess. Ask for the repo ROOT, not a file. |
| How much of it is in scope? | one workflow `(Recommended)` — a first port proves the harness before the volume; a named group; the whole engine. |

**Round 2 — after reading the repo.** One question, multi-select, options are the actual entry points you found, each labelled with what it evaluates. Add the v1 alias when you can match it against `altscore workflows list`, so they recognise the name they use in the Hub rather than the module name.

Then restate what you got in one line — tenant, repo, workflows, scope — and start at step 1 of the served `order`.

**Do not ask** for anything in the Step 1 table, and do not ask the business-policy questions that belong to greenfield authoring. A migration has an answer for all of them already: the v1 repo is the source of truth for thresholds, messages, hard stops and output shape, and inventing a new policy mid-port destroys the parity proof you are about to build.

**When AskUserQuestion is unavailable** (print mode `-p`, background jobs, some SDK hosts): do not guess and mutate. State the tenant, repo and workflow list you are assuming, do the offline work that does not depend on them, stop at `apply --dry-run`, and report the open questions. Apply only after the user answers.

#### Step 3 — the three rules that decide whether the port was worth doing

Parity is necessary and not sufficient. A one-for-one translation of the v1 Python reproduces every historical decision and hands the client a workflow they cannot operate — the v1 engine had exactly ONE primitive, so everything in it is Python by necessity, not by design. Full text and the reasoning in `schema-guide v1Migration` under `variableBudget` and `indicatorVariables`; the general form is `schema-guide variables` -> `whenToUseACustomVariable`.

1. **Fewest custom variables possible.** The count comes DOWN across a port, not across 1:1. Count before and after. For every variable that survives, be able to say what it computes that no entity could — if you cannot, it is not finished.

2. **Complicate the entity, not the Python.** A mapping-table with more entries, a scorecard with more rules, a rule-tree with more branches, an evaluation-rule with a compound condition: all of those stay visible and editable in the Hub, and the client changes them without a republish. The same logic in an expression is a code box they file a ticket against. Prefer the bigger table to the smaller table plus a variable; prefer the extra rule to the extra `if`. The only thing that pushes work back into Python is that no entity can express it — aggregation over nested arrays, date arithmetic, coalescing across sources, output shaping.

3. **`*_indicator` becomes rules and tables, and the 2/1/0/-1 does not survive.** Split each one: the derivation stays a compute-variable and emits the REAL quantity (the amount, the count, the category as the registry spells it); the coding becomes an evaluation-rule when the answer is boolean, or a mapping-table when it has levels — which is also where a scorecard's points belong, since a scorecard rule cannot carry inline buckets. Do not keep the codes as the table's output either: `GRAVE / LEVE / SIN_REGISTRO` reads correctly in the Hub, in the consuming rule and in the PDF; `2/1/0` needs a legend that lives nowhere. A `-1` for missing is an `includeNa` bucket, not a value — encoded as a number, every downstream comparison has to remember to exclude it, and one that forgets treats "no data" as "better than zero".

This is a representation change, not a behaviour change, so it does not touch the parity proof — verify that with the harness rather than assuming it, and expect the diff to be exact. It is NOT the same as fixing a v1 bug in flight, which `knownDivergences` forbids.

#### Step 4 — the advisory that checks rule 3

Both `lint` and `apply` report ordinal-coded variables on stderr, aggregated into one line, never blocking:

```bash
altscore workflows-v2 lint <id>
altscore workflows-v2 apply --body @spec.json --dry-run
# readability advisory (client handoff): 1 finding(s). Advisory only ...
#   [ordinal-codes] 3 of 89 -- custom variable(s) return nothing but a small integer code ...
#      e.g. fiscalia_o_interpol_indicator (0/1), pjex_sri_billing_permission (-1/0/1)
```

It detects by SHAPE — every assignment to the variable's `returnValue` is a bare literal from a small set — and never by name, in both directions. The suffix under-reports: on a real port it caught a variable with the identical `-1/0/1` shape and no `_indicator` in its name. It also over-reports: an `*_indicator` that returns `GRAVE`/`NO TIENE` is a named category, which is what rule 3 asks you to produce, so flagging it would tell you to undo the right thing.

A clean advisory is not a clean port. It says nothing about rules 1 and 2 — count the variables yourself.
