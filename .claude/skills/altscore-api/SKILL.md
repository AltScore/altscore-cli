---
name: altscore-api
description: "Interact with the AltScore Borrower Central API using the altscore CLI. Use when the user needs to create, read, update, or delete borrowers, identities, documents, deals, or query executions and packages. Also use when they want to author, change or MIGRATE a workflow -- 'migrate this off the legacy engine', 'port this workflow to v2', 'move <tenant> to the new repo' all start here. Also use for raw API calls and profile management."
user-invocable: false
allowed-tools: Bash, Read, Grep, Glob
---

# AltScore CLI -- Agent Reference

You have access to the `altscore` CLI for interacting with the AltScore Borrower Central API. All commands output JSON to stdout and status messages to stderr. Pipe to `jq` for field extraction.

This file is the entry point. It covers setup, the command model, the traps that cause silent failures, and an index of detailed references. **Load a reference file only when the task needs it** (see [Reference index](#reference-index)).

## Prerequisites

Verify `altscore` is installed:

```bash
which altscore
```

If not found, install it:

```bash
gh release download --repo AltScore/altscore-cli --pattern "altscore-$(uname -s | tr '[:upper:]' '[:lower:]')-$(uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/')" --output /usr/local/bin/altscore --clobber
chmod +x /usr/local/bin/altscore
```

Update in place with `altscore update` (downloads the latest release, verifies SHA-256, swaps the binary; set `GITHUB_TOKEN` if the repo is private).

## Authentication

The CLI must be logged in before use. Check with `altscore config`. If no profile exists, log in interactively (requires a terminal) with `altscore login`. Tokens auto-refresh on 401 — no manual refresh needed.

**Exporting credentials for the Python SDK** — `altscore env` prints raw secrets (client_secret, access token) to stdout. NEVER run it bare; ALWAYS pipe to a file:

```bash
altscore env > .env                       # current profile
altscore env --profile staging > .env.staging
```

Outputs `ALTSCORE_CLIENT_ID`, `ALTSCORE_CLIENT_SECRET`, `ALTSCORE_USER_TOKEN`, `ALTSCORE_ENVIRONMENT`, `ALTSCORE_TENANT` — the env vars the AltScore Python SDK reads.

## Command model

Every resource follows the same grammar and is self-documenting:

```bash
altscore <resource> <action> [<id>] [--flags]
altscore <resource> <action> --help        # body fields, response fields, filters, examples
```

Key patterns:

- **Body input**: `--body '{...}'` or pipe from stdin: `echo '{}' | altscore borrowers create`. Use `--body @file.json` for large specs.
- **Filters**: `--filter key=value` (repeatable). Run `<resource> list --help` for valid filter keys.
- **Pagination**: `--per-page N --page N`
- **Profiles**: `--profile <name>` switches context; `altscore profiles list` shows all.
- **Verbose**: `--verbose` prints HTTP method, URL, status to stderr.
- **Raw API escape hatch** for uncovered endpoints: `altscore api GET /v1/borrowers/<id>/summary`, `altscore api POST /v1/path --body '{...}'`. Modules: `borrower_central` (default), `cms`, `altdata` (e.g. `--module cms`).

### Schema introspection

Before reading or writing an entity, query the schema registry for exact field names, types, required/optional, and aliases:

```bash
altscore schema                              # list all resources
altscore schema borrowers                    # create + update + response + filters
altscore schema borrowers --action create    # just create body fields
```

### Test mode (UAT)

Most entities support `isTest` for UAT data in production. Test records are **excluded from list results by default**.

```bash
altscore borrowers list --include-tests           # include test records
altscore borrowers list --test-only               # only test records
altscore borrowers create --is-test --body '{...}'
altscore borrowers set-test <id> --enable         # mark as test (cascades to children)
altscore borrowers set-test <id> --disable        # clear (won't un-toggle independent children)
```

Full test mode (`set-test` + filters + `--is-test`): borrowers, identities, documents, deals, assets, borrower-fields, deal-fields, asset-fields, points-of-contact, deal-contacts, authorizations, metrics, artifacts, data-models, evaluators, evaluation-rules, policy-rules, rule-trees. Filter-only (no `set-test`): executions, execution-batches.

## Critical traps (read before mutating)

These are silent-failure modes — the API accepts the request and fails later, or "succeeds" while doing nothing. Each is detailed in the linked reference.

1. **v2 workflows: use `apply`, not `create`.** `workflows-v2 apply --body @spec.json` is the one declarative verb for create-or-update. Hand-built `create` bodies produce orphan nodes that save but break the Hub. See [workflows-v2](references/workflows-v2.md).
2. **DRAFT executes faithfully — publish only sets the alias default.** A DRAFT v2 workflow runs its full graph (node execution is never gated by status), so executing a DRAFT *by id* is a real pre-publish test. Publishing only changes which version the *alias* serves. Test first, `--publish` when you want it live. See [workflows-v2](references/workflows-v2.md).
3. **Execution outputs live on two surfaces.** Per-task outputs come from `/state` (alias-keyed) or `/output` (type-keyed) depending on the engine. Use `altscore executions state <id>` (it auto-falls-back and stamps `._source`). See [resources](references/resources.md).
4. **`decisionKey` is case-sensitive** and must match a tenant-registered decision (`altscore decisions list`). A mismatch passes lint and fails at runtime. See [credit-decisioning](references/credit-decisioning.md).
5. **Conditional branches use structured `conditions`, not `expression` strings** — the API stores `expression` as a no-op. See [workflows-v2](references/workflows-v2.md).
6. **`workflowAlias` is load-bearing** on credit-decisioning entities (rules, scorecards, mapping-tables, rule-trees) — without it they're invisible to the workflow's pickers. `apply` auto-stamps it. See [credit-decisioning](references/credit-decisioning.md).
7. **Required fields that live in the body, not as flags**: identities/documents need `borrowerId`/`key` IN THE BODY; v2 field names are camelCase (`nodeId`, `sourceNodeId`). `execute --execution-mode async` returns only `executionId` (poll for status). See [resources](references/resources.md) and [workflows-v2](references/workflows-v2.md).

## "I want to migrate"

A delivery engineer asking to move a workflow off the legacy v1 engine has not told you the three things you need, because to them they are obvious. Do NOT start reading code and do NOT guess them.

1. Pre-flight silently: `altscore config` (logged in? which tenant?) and `altscore profiles list`. If there is no session, say that `altscore login` needs a **client_id** and a **client_secret** and that it must be run in a terminal — it reads stdin, so you cannot do it for them.
2. Ask, with **AskUserQuestion**, building the options from those reads: which tenant, where the legacy repo lives, how much is in scope. Then, once you can read the repo, which workflows — as a multi-select of the real entry points you found, not free text.
3. Follow `altscore workflows-v2 schema-guide v1Migration`. That section is the doctrine and it is served live; nothing in this repo restates it.
4. Parity is necessary and not sufficient. Fewest custom variables; complicate the ENTITY, not the Python; `*_indicator` becomes evaluation-rules and mapping-tables and the 2/1/0/-1 codes do not survive the port.

Full intake, including the fallback when AskUserQuestion is unavailable: [v1-migration](references/v1-migration.md).

## Reference index

Load the file that matches the task:

| Topic | File | Covers |
|---|---|---|
| Core entities | [references/resources.md](references/resources.md) | borrowers, identities, documents, deals, executions (two-surface outputs), packages |
| Workflows v1 | [references/workflows-v1.md](references/workflows-v1.md) | workflow-tasks, task-tests, v1 workflows, input-schema reference, DAG data-flow rules |
| v1 -> v2 migration | [references/v1-migration.md](references/v1-migration.md) | the intake round (login pre-flight, tenant, legacy repo, workflow list), where the doctrine is served from, and the three porting rules parity does not check |
| Workflows v2 | [references/workflows-v2.md](references/workflows-v2.md) | `apply`, tasks-v2, CRUD, lock dance, lifecycle, schedules, import/export, execute, helpers, variable resolution, atomic deal patterns |
| Credit decisioning | [references/credit-decisioning.md](references/credit-decisioning.md) | mapping-tables, scorecards, evaluation-rules, decisions, rule-trees, pitfalls |
| KYC/KYB good habits | [references/kyc-kyb-habits.md](references/kyc-kyb-habits.md) | tenant/country-agnostic structural habits for onboarding & screening flows — read before authoring a KYC/KYB/onboarding workflow |
| AltData | [references/altdata.md](references/altdata.md) | source discovery (`describe` pre-flight), data requests |
| Python SDK | [references/sdk.md](references/sdk.md) | in-task SDK, modules, borrower methods, macros, evaluators, reading enrichment packages |
| Report generation | [references/reports.md](references/reports.md) | PDF report components and task patterns |
| Data-models | [references/data-models.md](references/data-models.md) | schema definitions: identity keys, fields, steps, decisions |
| Workflow output fields | [references/output-fields.md](references/output-fields.md) | the `w_` prefix envelope (`w_standard_exec_output`, `w_attachments`, ...) |
| Alerts | [references/alerts.md](references/alerts.md) | policy alerts on borrowers |
| Analytics | [references/analytics.md](references/analytics.md) | metrics, widgets, dashboards, filter helpers; provisioning + verify loop |
