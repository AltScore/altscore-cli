## Data-Models (Schema Definitions)

Data-models define the fields and keys available on all AltScore entities. They control what identity keys, borrower fields, steps, deal fields, and asset groups exist on a tenant.

### Entity type categories

| Category | Entity Types |
|----------|-------------|
| core | identity, contact, document, borrower, point_of_contact, authorization, metric, accounting_document |
| fields | borrower_field |
| workflow | step, decision |
| deals | deal_field, deal_step, deal_role |
| assets | asset_field, asset_group |

### CLI commands

```bash
# CRUD
altscore data-models list --filter entity-type=identity
altscore data-models get <id>
altscore data-models create --body '{"key": "email", "label": "Email", "entityType": "identity", "priority": 2}'
altscore data-models update <id> --body '{"label": "Email Address"}'
altscore data-models delete <id>

# Enable encryption (one-way, identity only)
altscore data-models make-sensitive <id>

# Best-practices guide (live documentation)
altscore data-models guide
altscore data-models guide identity
altscore data-models guide borrower_field
```

### Key rules

- **identity**: `priority` is required (>= -1). Use -1 to append to end. Priorities auto-shift on insert/delete.
- **step / deal_step**: `order` is required. Orders auto-shift on insert/delete.
- **borrower_field / asset_field / deal_field**: `allowedValues` can constrain input to a list. Other types cannot use `allowedValues`.
- **isSensitive**: Can only be set at creation time or via `make-sensitive`. Cannot be undone or changed via update.
- **isSegmentationField**: Makes the field available for audience segmentation in the UI.

### Create examples

```bash
# Identity with priority
altscore data-models create --body '{"key": "tax-id", "label": "Tax ID", "entityType": "identity", "priority": 0, "isSensitive": true}'

# Step with order
altscore data-models create --body '{"key": "application", "label": "Application", "entityType": "step", "order": 0}'

# Borrower field with allowed values
altscore data-models create --body '{"key": "industry", "label": "Industry", "entityType": "borrower_field", "allowedValues": ["retail", "manufacturing", "services"], "isSegmentationField": true}'
```

### SDK usage (inside workflow tasks)

```python
bc = alts_acli.borrower_central

# List data-models by entity type
models = await bc.data_models.query(entity_type="identity")

# Create a data-model
dm_id = await bc.data_models.create({
    "key": "phone",
    "label": "Phone Number",
    "entityType": "identity",
    "priority": 3,
})

# Retrieve
dm = await bc.data_models.retrieve(dm_id)

# Update
await bc.data_models.patch(dm_id, {"label": "Mobile Phone"})

# Delete
await bc.data_models.delete(dm_id)
```

### Live guide

Use `altscore data-models guide` to get the full best-practices reference including required fields per entity type, validation rules, special behaviors (priority shifting, order shifting), and annotated create examples. Filter by entity type with `altscore data-models guide <type>`.
