## AltData

Discovery commands query Borrower Central (work in all environments). Execution commands hit the AltData module (production only).

### Discovery flow (canonical order)

> **Use `describe` as the pre-flight.** It is the one-shot primitive: hits sources-status once, auto-resolves the latest version, and returns metadata + versions + inputFields + outputKeys in a single JSON document. Reach for `dictionary` / `sample` / `sources` only when you need something `describe` doesn't surface.
>
> **If `altscore altdata describe` says "unknown command"** your installed CLI is older than the describe primitive. Build and install the latest from the repo:
> ```bash
> cd <path-to-altscore-cli> && go build -buildvcs=false -o "$(which altscore)" .
> ```
> Then re-run. Until you upgrade, fall back to `altscore altdata sources --per-page 200` (filter client-side via `jq 'select(.sourceId == "<X>")'`) + `altscore altdata dictionary <X> <ver>` (latest is auto-resolved when omitted).

> **Valid `--filter` keys for sources are `country`, `status`, and `search` only.** Mirrors the Hub's `useAltDataSources` hook (`altscore-ai-chat/lib/hooks/use-altdata-sources.ts`) which sends `?country=<csv>&locale=<en|es>` and nothing else. Filtering by `sourceId` doesn't work — the backend silently ignores unknown filter keys and returns the full catalog. For a single source use `altdata describe <id>` (which does its own client-side narrowing).

```bash
# 1. Find candidates (default --per-page is 200, returns the full ~170-source catalog in one call)
altscore altdata sources --filter search="credit"
altscore altdata sources --filter country=USA --filter status=active

# 2. Pre-flight a candidate (the canonical step before composing)
altscore altdata describe USA-PUB-0001                       # auto-resolves latest version
altscore altdata describe USA-PUB-0001 --version v1          # pin a specific version
altscore altdata describe USA-PUB-0001 | jq '{inputFields, outputKeys, latestVersion}'

# 3. Drill into specifics only if needed
altscore altdata dictionary USA-PUB-0001                     # field defs (latest version auto-resolved)
altscore altdata dictionary USA-PUB-0001 v1                  # pin version
altscore altdata sample USA-PUB-0001                         # example output (latest)
altscore altdata sample USA-PUB-0001 v1
altscore altdata search "credit score"                       # cross-source field search
altscore altdata search "address" --locale es
```

**Anti-patterns (avoid):**
- Walking pages of `altscore altdata sources` to inspect a single source — use `describe`.
- `altscore altdata sources --filter sourceId=<X>` — silently returns the full catalog (the backend ignores unknown filter keys). Use `describe <X>` instead.
- Reading `.id` off the `sources` list — each list item's `.id` is `null`; the source identifier is `.sourceId` (the value `describe <id>` takes). Filter/extract with `.sourceId`, not `.id`.
- Calling `dictionary` or `sample` without a version — both now auto-resolve the latest, no need to chain a separate sources call first.
- Using `workflows-v2 sources-status` for general discovery — it's the same endpoint as `altdata sources` but lives under workflows-v2 because apply-time normalization needs it; for agents browsing the catalog, prefer `altdata sources` / `altdata describe`.

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
