---
name: altscore-api
description: "Interact with the AltScore Borrower Central API using the altscore CLI. Use when the user needs to create, read, update, or delete borrowers, identities, documents, deals, or query executions and packages. Also use for raw API calls and profile management."
user-invocable: false
allowed-tools: Bash, Read, Grep, Glob
---

# AltScore CLI -- Agent Reference

You have access to the `altscore` CLI for interacting with the AltScore Borrower Central API. All commands output JSON to stdout and status messages to stderr. Pipe to `jq` for field extraction.

## Authentication

The CLI must be logged in before use. Check with:

```bash
altscore config
```

If no profile exists, log in:

```bash
altscore login --profile <name> --environment staging \
  --client-id <id> --client-secret <secret>
```

Tokens auto-refresh on 401. No manual refresh needed.

## Resource Commands

Six resources are available. Every resource supports `--help` which documents request body fields, response fields, and available filters.

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
