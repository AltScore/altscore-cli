## Alerts

Alerts are policy notifications created on borrowers, typically from evaluator rule hits.

### CLI commands

```bash
# List alerts for a borrower
altscore api GET "/v1/alerts?borrower-id=<id>"

# Create an alert
altscore api POST /v1/alerts --body '{
  "borrowerId": "<id>",
  "ruleCode": "DR-D001",
  "level": 2,
  "message": "Score below threshold: 450",
  "referenceId": "<execution-id>"
}'
```

### SDK usage

```python
bc = alts_acli.borrower_central
alert_id = await bc.alerts.create({
    "borrowerId": borrower_id,
    "ruleCode": "DR-D001",
    "level": 2,
    "message": "Score below threshold",
    "referenceId": execution_id,      # ties alert to workflow execution
})
```

Prefer `macros.create_alerts_from_rules` over manual alert creation -- it handles rule filtering, level mapping, and duplicate suppression automatically.
