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

### Source credentials (per-version profiles)

Sources that authenticate with the tenant's own provider account (the private bureaus, for example) declare a `credentialsSchema` in `sources-status`. The tenant stores those credentials as **profiles keyed by `(sourceId, version, name)`**. A run of version `v1` reads the `v1` profile named on the node (`default` unless set), never another version's.

There is no dedicated command; use `altscore api`. Writes need the `secrets.write` permission.

```bash
# List every profile of a source (non-secret values + which secrets are set; secrets never come back)
altscore api GET /v2/workflows/sources/<sourceId>/credentials/profiles

# Create or update one profile. Pipe the body through stdin so secrets never land in argv or shell history.
# Secret fields left out of the body keep their stored value; send only what changes.
printf '%s' "$BODY_JSON" | altscore api PUT /v2/workflows/sources/<sourceId>/credentials/profiles/<version>/<name>
#   BODY_JSON = {"values": {"<field>": "<value>", ...}}   field names come from the source's credentialsSchema

# Validate the stored profile against the provider (authenticates; no billable data pull)
altscore api POST /v2/workflows/sources/<sourceId>/credentials/profiles/<version>/<name>/check

# Delete one profile
altscore api DELETE /v2/workflows/sources/<sourceId>/credentials/profiles/<version>/<name>
```

Which profile a run uses:
- `sourcesConfig[].credentialName` on the `altdata-enrichment` node picks the profile; omitted means `default`.
- `sourcesConfig[].testCredentialName` is used instead on test runs (`--test`, or a run flagged as test), e.g. the vendor's UAT credentials; omitted means test runs use `credentialName` too.
- A per-run override goes in the `X-Source-Credential-Overrides` header of `POST /v2/workflows/<id>/execute`: a JSON list of `{"taskAlias", "sourceId", "credentialName"}`. It wins over both fields above.
- A **named** profile (anything but `default`) has no fallback: if it does not exist for that version the source fails instead of quietly using other credentials.

Traps:
- **The version-less `/v2/workflows/sources/<sourceId>/credentials` endpoints are retired.** They wrote a record runs had stopped reading after the move to profiles, so a credential "updated" there changed nothing. Always write `.../credentials/profiles/<version>/<name>`.
- A new source version needs its own profile; credentials are not copied across versions.
- `check` exercises what is **stored**: save first, then check. A `404` from it means no profile exists for that `(version, name)`.
- Sources with `credentialsScope: "endUser"` take the borrower's own credentials (`/v2/workflows/borrowers/<borrowerId>/sources/<sourceId>/credentials`), not tenant profiles.
