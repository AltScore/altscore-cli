## Analytics

Manage a tenant's analytics config: **metrics** (ClickHouse SQL templates),
**widgets** (visualizations bound to a metric), and **dashboards** (grid
layouts of widgets). Config lives in MongoDB; the numbers come from ClickHouse.

Command group: `altscore analytics <metrics|widgets|dashboards|field-values|terms>`.

### Metrics (SQL templates)

```bash
altscore analytics metrics list                       # tenant's metrics (bc.private.read)
altscore analytics metrics create --body @metric.json # create/overwrite (bc.private.write)
altscore analytics metrics run --metric-key <key>     # execute it (borrowers.read)
altscore analytics metrics run --body '{"metricKey":"<key>","cohortDate":{"from":"2025-01-01","to":"2025-06-30"}}'
altscore analytics metrics delete <key>
```

Create body:
```json
{
  "metricKey": "active_borrowers",
  "queryTemplate": "SELECT status, count() FROM borrower_snapshot WHERE tenant='{tenant}' AND {BORROWER_WHERE_CLAUSE} GROUP BY status",
  "columns": ["status", "count"],
  "metadataCols": ["status"],
  "queryMetadata": {"dataType": "count", "intl": {"es": {"label": "...", "description": "..."}, "en": {"label": "...", "description": "..."}}}
}
```

`queryTemplate` placeholders (str.format): `{tenant}`, `{BORROWER_WHERE_CLAUSE}`,
`{BORROWER_FIELDS_WHERE_CLAUSE}`, `{COHORT_DATE_CLAUSE}`,
`{PAYMENT_COHORT_DATE_CLAUSE}`, `{ANALYSIS_DATE_CLAUSE}`, `{FILTER_QNT}`.
Escape any literal brace as `{{`/`}}`. A bad template returns
`400 INVALID_METRIC_TEMPLATE` at create and run time. `columns` map onto each
result row in SELECT order; `metadataCols` (subset) route into `row.metadata`.

### Widgets (visualizations)

```bash
altscore analytics widgets list
altscore analytics widgets create --body '{"widgetId":"w_active","queryId":"active_borrowers","displayName":"Active","widgetType":"bar-chart","labelAxis":"status","dataAxis":["count"]}'
altscore analytics widgets update <widget-id> --body '{"displayName":"Active borrowers"}'   # partial
altscore analytics widgets delete <widget-id>
```

`queryId` == a metric's `metricKey`. `widgetType`: `pie | kpi | percentage |
money | bar-chart | table | waterfall` (waterfall exempts labelAxis/dataAxis).
All widget ops need `bc.private.write` (list is readable). `create` with an
existing `widgetId` overwrites (full replace); `update` is a partial PUT.

### Dashboards (layouts)

```bash
altscore analytics dashboards list
altscore analytics dashboards get <layout-id>
altscore analytics dashboards default
altscore analytics dashboards create --body @layout.json          # returns {id}
altscore analytics dashboards update <layout-id> --body '{"displayName":"Overview"}'
altscore analytics dashboards set-default <layout-id>
altscore analytics dashboards unset-default <layout-id>
altscore analytics dashboards delete <layout-id>
```

Create/update body: `{ "displayName": "...", "layoutSetup": { "lg": [ {"i":"<widgetId>","x":0,"y":0,"w":6,"h":4} ], "md": [...], "sm": [...] } }` — each `i` is a widgetId. Dashboard ops need `borrowers.write`.

### Filter helpers (for get-metrics filters)

```bash
altscore analytics field-values --key riskRating   # distinct borrower-field values
altscore analytics terms                           # distinct debt durations + interest rates
```

### Provisioning + verify loop

1. `metrics create` → 2. `metrics run --metric-key <key>` to prove the SQL
   executes (a 200 with a `result` array). 3. `widgets create` (queryId = the
   key). 4. `dashboards create` then `dashboards set-default`. Confirm data is
   landing with `altscore api POST /v1/internal/clickhouse-health` if a metric
   returns empty.

### Gotchas

- **Metric upsert is keyed on `metricKey` only** (not tenant): re-creating a key
  overwrites it, even across tenants. Use tenant-unique keys or a deliberate
  `system` metric.
- **`system` sharing is asymmetric**: `system` metrics *run* everywhere but are
  not in `metrics list`; `system` widgets *appear* in `widgets list` for every
  tenant (but can't be edited/deleted per-tenant); dashboards are strictly
  per-tenant.
- A fresh tenant renders empty until the ClickHouse migration has run over its
  data window — the metric/widget exist but have no rows yet.

Full model: `borrower-central/docs/analytics-provisioning.md`.
