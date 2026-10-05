## AltData

Discovery commands query Borrower Central (work in all environments). Execution commands hit the AltData module (production only).

### Discovery flow (canonical order)

1. **Which sources are available.** `altscore workflows-v2 sources-status --country <ISO3>` or `--search <word>` (the same rows as `altscore altdata sources`; the unfiltered catalog is about 40 KB) reads every page and prints one compact row per source version: `sourceId`, `version`, `name`, `status`, `enabled`, `requiredInputs`. stderr says `# N of M sources`. Narrow with `--search <text>` (id, version, name), `--country <ISO3>` or `--status active`, and read stderr after a filter: it counts what was hidden. A country filter never returns INT (international) sources such as sanctions lists, and `--status active` drops every down, failing or retired version. `--full` adds `outputSchema` (megabytes for the catalog); picking a source never needs it.
2. **ONE source's fields.** `altscore altdata describe <sourceId>`: versions, `inputFields` with their requirement, top-level `outputKeys`. Then `altscore altdata dictionary <sourceId> [version]` for every output field with its type and description. Both take the version as a second argument, resolve the latest one when it is omitted, and search every catalog page; a miss names the closest ids. A source with no dictionary is answered from its outputSchema (paths and types, no descriptions).
3. **A field across sources.** `altscore altdata search "credit score"` (`--country MX` narrows).

```bash
altscore workflows-v2 sources-status --search credit
altscore altdata describe <sourceId> | jq '{inputFields, outputKeys, latestVersion}'
altscore altdata dictionary <sourceId> v1
```

**Anti-patterns:**
- `--filter sourceId=<X>`, or any key but `country`, `status`, `search`: the backend ignores it (the CLI warns). Use `describe <X>`.
- Reading one source out of `--full`: use `describe`.
- Reading `.id` off a raw row: the identifier is `.sourceId`.

### Data Requests (production only)

```bash
# Synchronous request (blocks until complete)
altscore altdata request-sync --body '{
  "personId": "borrower-123",
  "sourcesConfig": [{"sourceId": "USA-PUB-0001", "version": "v1"}]
}'

# Asynchronous request (returns requestId immediately)
altscore altdata request-async --body '{
  "personId": "borrower-123",
  "sourcesConfig": [{"sourceId": "USA-PUB-0001", "version": "v1"}]
}'

# Check async request status
altscore altdata request-status <request-id>

# Collect completed request data
altscore altdata request-collect <request-id>
```
