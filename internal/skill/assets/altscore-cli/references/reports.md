## Report Generation (PDF)

The report generator creates styled PDF reports from a structured request.

### CLI commands

```bash
# Generate a report (returns signed URL)
altscore tools generate-report --body '{"reportTitle": "...", "sections": [...]}'

# Discover available report components
altscore tools report-components

# Get JSON Schema for a specific component
altscore tools report-components subjectInfo
altscore tools report-components evaluatorResult
altscore tools report-components reportOptions
```

**Always use `altscore tools report-components <name>` to get the exact schema before constructing a component.** The schemas are served live from the report generator service and include all field names, types, and defaults.

### Testing workflow (before writing task code)

The report generator has strict validation. Always dry-run your payload with the CLI before embedding it in task code:

```bash
# 1. Check top-level required fields (logoUrl is required!)
altscore tools report-components reportOptions

# 2. Check required fields for each component you plan to use
altscore tools report-components keyValueTable

# 3. Test the full request -- iterate until it returns a URL
altscore tools generate-report --body '{
  "reportTitle": "My Report",
  "byLine": "",
  "logoUrl": "",
  "sections": [{
    "title": "Section",
    "subtitle": "",
    "components": [
      {"name": "keyValueTable", "title": "Details", "subtitle": "", "items": [
        {"label": "Key", "value": "Value"}
      ]}
    ]
  }]
}'

# 4. Only after the CLI returns a valid URL, copy the payload into task code
```

### SDK usage (inside workflow tasks)

```python
bc = alts_acli.borrower_central
report_url = await bc.report_generator.generate(report_req)
# Returns a URL string pointing to the generated PDF
```

### `report_req` structure

```python
report_req = {
    "reportTitle": "Credit Analysis Report",
    "byLine": "Generated on 2026-03-07",
    "logoUrl": "https://...",           # REQUIRED (use "" for no logo)
    "logoSize": "128px",               # optional
    "sections": [                       # list of section objects
        {
            "title": "Section Title",
            "subtitle": "",
            "page_break": False,        # optional, force page break before section
            "components": [...]         # list of component objects
        }
    ]
}
```

Each component is a dict with `"name"` identifying the type, plus the component's options as sibling keys. Container components (flex, row) also accept a nested `"components"` array.

### Component categories

**Report components** -- the building blocks you construct manually in task code. Use `altscore tools report-components` to list all with descriptions. Key ones:

- `subjectInfo` -- Identity card with name and key-value pairs
- `subjectScore` -- Score display with grades
- `evaluatorResult` -- Full evaluator output with decision and rules tables
- `scorecardResult` -- Scorecard breakdown table
- `ruleTreeResult` -- Decision tree result with rule hits
- `keyValueTable` -- Label-value pairs (supports HTML)
- `complianceTable` -- Alert factors with severity colors
- `customTable` -- Dynamic table from columns/rows
- `card`, `card_v2` -- Metric cards with icon and context color
- `htmlBlock` -- Raw HTML (spacers, custom formatting)
- `flex`, `row` -- Layout containers for child components

**Source components** -- auto-matched by AltData source slug as the `name` field:
```python
{"name": "ECU-PUB-0002_v1", "altdataPackage": source_dict}
```
Version fallback: if the exact version isn't found, the highest available version is used.

**Entity components** -- render entity data (e.g., `ASSET_v1`).

### Typical report generation task pattern

```python
class InputData(BaseModel):
    borrower_id: str
    evaluator_result: Dict[Any, Any]
    source_packages: Dict[str, Any]   # slug -> package dict

class OutputData(BaseModel):
    report_url: str
    w_attachments: List[Dict[str, Any]]

async def execute(input_data: InputData, context: dict) -> OutputData:
    bc = alts_acli.borrower_central

    sections = []

    # Subject info section
    sections.append({
        "title": "", "subtitle": "",
        "components": [
            {"name": "subjectInfo", "subjectName": "...", "numberOfColumns": 1,
             "items": [{"label": "ID", "value": "..."}]},
            {"name": "subjectScore", "label": "Score",
             "scoreValue": input_data.evaluator_result["score"]["value"],
             "scoreMaxValue": "999", "secondaryInfo": [], "grades": []},
        ]
    })

    # Evaluator result section
    sections.append({
        "title": "Credit Evaluation", "subtitle": "",
        "components": [
            {"name": "evaluatorResult", "title": "",
             "displayConfiguration": {
                 "decision": {"key": "decision", "label": "Decision",
                              "type": "categorical",
                              "contextMap": {"Approve": "success", "Reject": "danger"}},
                 "rulesHitTable": True, "allRulesTable": True,
                 "mainCards": [], "secondaryCards": [],
             },
             "evaluatorResult": input_data.evaluator_result},
        ]
    })

    # AltData source sections (only if successful)
    for slug, pkg in input_data.source_packages.items():
        if pkg.get("isSuccess"):
            sections.append({
                "title": slug, "subtitle": "", "page_break": False,
                "components": [{"name": slug, "altdataPackage": pkg}]
            })

    report_url = await bc.report_generator.generate({
        "reportTitle": "Analysis Report",
        "byLine": f"Generated: {datetime.now().strftime('%Y-%m-%d')}",
        "sections": sections,
    })

    return OutputData(
        report_url=report_url,
        w_attachments=[{"url": report_url}],
    )
```
