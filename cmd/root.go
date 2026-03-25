package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/AltScore/altscore-cli/internal/client"
	"github.com/AltScore/altscore-cli/internal/config"
	"github.com/AltScore/altscore-cli/internal/version"
	"github.com/spf13/cobra"
)

var (
	flagProfile     string
	flagEnvironment string
	flagTenant      string
	flagVerbose     bool
	flagBaseURLs    []string
)

var rootCmd = &cobra.Command{
	Use:   "altscore",
	Short: "CLI for the AltScore API",
	Long: `altscore is a command-line interface for the AltScore API.

It supports multiple named profiles for different environments and tenants,
automatic token refresh, and JSON output for scripting and LLM tool use.

Authentication uses OAuth2 client credentials. Configure profiles with:
  altscore login --profile <name> --environment <env>

All commands output JSON to stdout. Status messages go to stderr.
Use --help on any command to see available flags and examples.

Quick start:
  altscore login
  altscore borrowers list --per-page 5
  altscore api GET /v1/borrowers?per-page=1`,
	Version:      version.Version,
	SilenceUsage: true,
}

func init() {
	rootCmd.PersistentFlags().StringVar(&flagProfile, "profile", "", "named profile to use (default: from config or \"default\")")
	rootCmd.PersistentFlags().StringVar(&flagEnvironment, "environment", "", "override profile's environment (production, staging, sandbox)")
	rootCmd.PersistentFlags().StringVar(&flagTenant, "tenant", "", "override profile's tenant ID")
	rootCmd.PersistentFlags().BoolVar(&flagVerbose, "verbose", false, "print request details to stderr")
	rootCmd.PersistentFlags().StringArrayVar(&flagBaseURLs, "base-url", nil, `override module base URL (format: module=url, repeatable)
  e.g. --base-url borrower_central=http://localhost:8000`)

	rootCmd.AddCommand(makeSchemaCmd())
	rootCmd.AddCommand(makeToolsCmd())
	registerResources()
}

func registerResources() {
	// --- Core business entities ---

	registerResource(ResourceDef{
		Name:     "borrowers",
		Singular: "borrower",
		BasePath: "/v1/borrowers",
		Module:   "borrower_central",
		Actions:  []string{"list", "get", "create", "update", "delete"},
		HasTestMode: true,
		Description: `Manage borrowers in the AltScore Borrower Central API.

A borrower represents an individual or company that is a customer or
prospect. Borrowers have identities, documents, and can participate in deals.`,
		CreateSchema: `  persona: string       [required] "individual" or "company"
  label: string         Display name
  externalId: string    External system ID
  riskRating: string    Risk rating value
  flag: string          Flag value
  tags: [string]        Tags (default: [])`,
		UpdateSchema: `  label: string         Display name
  tags: [string]        Tags`,
		ResponseSchema: `  id, persona, label, externalId, avatarUrl, tags, flag, isTest,
  riskRating, repaymentRiskRating, currentStep{stepId, order, key, createdAt},
  cmsClientIds, createdAt, updatedAt`,
		FilterHelp: `  persona               "individual" or "company"
  external-id           External system ID
  flag                  Flag value
  risk-rating           Risk rating value
  tags                  Comma-separated tags
  sort-by               Field to sort by
  sort-direction        "asc" or "desc"`,
	})

	registerResource(ResourceDef{
		Name:     "identities",
		Singular: "identity",
		BasePath: "/v1/identities",
		Module:   "borrower_central",
		Actions:  []string{"list", "create", "delete"},
		HasTestMode: true,
		Description: `Manage identities attached to borrowers.

An identity is a key-value pair associated with a borrower, such as
a tax ID, email address, or phone number. Identities are used to
uniquely identify and deduplicate borrowers.`,
		CreateSchema: `  borrowerId: string    [required] Parent borrower ID
  key: string           [required] Identity type (e.g. "email", "tax-id")
  value: string         Identity value
  tags: [string]        Tags (default: [])`,
		ResponseSchema: `  id, borrowerId, key, label, value (masked), priority, tags, isTest,
  hasAttachments, createdAt, updatedAt`,
		FilterHelp: `  borrower-id           Parent borrower ID
  key                   Identity type
  value                 Identity value
  priority              Priority value
  sort-by               Field to sort by
  sort-direction        "asc" or "desc"`,
	})

	docGroup := registerResource(ResourceDef{
		Name:     "documents",
		Singular: "document",
		BasePath: "/v1/documents",
		Module:   "borrower_central",
		Actions:  []string{"list", "create", "delete"},
		HasTestMode: true,
		Description: `Manage documents attached to borrowers.

Documents store structured data and can have file attachments. Use
'documents create' to create a document record and 'documents upload'
to attach a file.`,
		CreateSchema: `  borrowerId: string    Parent borrower ID (at least one of borrowerId/dealId required)
  dealId: string        Parent deal ID
  key: string           [required] Document type/key
  value: any            Document data (JSON object)
  tags: [string]        Tags (default: [])`,
		UpdateSchema: `  key: string           Document type/key
  value: any            Document data
  tags: [string]        Tags`,
		ResponseSchema: `  id, borrowerId, dealId, key, label, value, tags, isTest,
  hasAttachments, createdAt, updatedAt`,
		FilterHelp: `  borrower-id           Parent borrower ID
  deal-id               Parent deal ID
  key                   Document type/key
  sort-by               Field to sort by
  sort-direction        "asc" or "desc"`,
	})
	docGroup.AddCommand(makeDocUploadCmd())

	registerResource(ResourceDef{
		Name:     "deals",
		Singular: "deal",
		BasePath: "/v1/deals",
		Module:   "borrower_central",
		Actions:  []string{"list", "get", "create", "update"},
		HasTestMode: true,
		Description: `Manage deals (loan applications, credit lines, etc.).

A deal represents a financial product application or agreement
associated with a borrower.`,
		CreateSchema: `  label: string         [required] Deal label
  description: string   Description
  status: string        Status
  externalId: string    External system ID
  riskRating: string    Risk rating
  tags: [string]        Tags (default: [])`,
		UpdateSchema: `  label: string         Deal label
  description: string   Description
  status: string        Status
  riskRating: string    Risk rating
  tags: [string]        Tags`,
		ResponseSchema: `  id, label, description, status, externalId, isTest,
  currentStep{stepId, order, key, label, createdAt},
  riskRating, tags, createdAt, updatedAt`,
		FilterHelp: `  borrower-id           Parent borrower ID
  status                Deal status
  external-id           External system ID
  sort-by               Field to sort by
  sort-direction        "asc" or "desc"`,
	})

	registerResource(ResourceDef{
		Name:     "assets",
		Singular: "asset",
		BasePath: "/v1/assets",
		Module:   "borrower_central",
		Actions:  []string{"list", "get", "create", "update", "delete"},
		HasTestMode: true,
		Description: `Manage assets attached to deals.

An asset represents a physical or financial asset associated with a deal,
such as a vehicle, property, or piece of equipment.`,
		CreateSchema: `  dealId: string        [required] Parent deal ID
  key: string           [required] Asset type/key
  label: string         Display name
  tags: [string]        Tags (default: [])`,
		UpdateSchema: `  label: string         Display name
  tags: [string]        Tags`,
		ResponseSchema: `  id, dealId, key, label, tags, isTest, createdAt, updatedAt`,
		FilterHelp: `  deal-id               Parent deal ID
  key                   Asset type/key
  sort-by               Field to sort by
  sort-direction        "asc" or "desc"`,
	})

	registerResource(ResourceDef{
		Name:     "borrower-fields",
		Singular: "borrower-field",
		BasePath: "/v1/borrower-fields",
		Module:   "borrower_central",
		Actions:  []string{"list", "get", "create", "update", "delete"},
		HasTestMode: true,
		Description: `Manage borrower fields (key-value data attached to borrowers).

A borrower field stores a typed value under a data-model key for a borrower.`,
		CreateSchema: `  borrowerId: string    [required] Parent borrower ID
  key: string           [required] Field key (must match a data-model)
  value: any            Field value`,
		UpdateSchema: `  value: any            Updated field value`,
		ResponseSchema: `  id, borrowerId, key, label, value, isTest, createdAt, updatedAt`,
		FilterHelp: `  borrower-id           Parent borrower ID
  key                   Field key
  sort-by               Field to sort by
  sort-direction        "asc" or "desc"`,
	})

	registerResource(ResourceDef{
		Name:     "deal-fields",
		Singular: "deal-field",
		BasePath: "/v1/deal-fields",
		Module:   "borrower_central",
		Actions:  []string{"list", "get", "create", "update", "delete"},
		HasTestMode: true,
		Description: `Manage deal fields (key-value data attached to deals).

A deal field stores a typed value under a data-model key for a deal.`,
		CreateSchema: `  dealId: string        [required] Parent deal ID
  key: string           [required] Field key (must match a data-model)
  value: any            Field value`,
		UpdateSchema: `  value: any            Updated field value`,
		ResponseSchema: `  id, dealId, key, label, value, isTest, createdAt, updatedAt`,
		FilterHelp: `  deal-id               Parent deal ID
  key                   Field key
  sort-by               Field to sort by
  sort-direction        "asc" or "desc"`,
	})

	registerResource(ResourceDef{
		Name:     "asset-fields",
		Singular: "asset-field",
		BasePath: "/v1/asset-fields",
		Module:   "borrower_central",
		Actions:  []string{"list", "get", "create", "update", "delete"},
		HasTestMode: true,
		Description: `Manage asset fields (key-value data attached to assets).

An asset field stores a typed value under a data-model key for an asset.`,
		CreateSchema: `  assetId: string       [required] Parent asset ID
  key: string           [required] Field key (must match a data-model)
  value: any            Field value`,
		UpdateSchema: `  value: any            Updated field value`,
		ResponseSchema: `  id, assetId, key, label, value, isTest, createdAt, updatedAt`,
		FilterHelp: `  asset-id              Parent asset ID
  key                   Field key
  sort-by               Field to sort by
  sort-direction        "asc" or "desc"`,
	})

	registerResource(ResourceDef{
		Name:     "points-of-contact",
		Singular: "point-of-contact",
		BasePath: "/v1/points-of-contact",
		Module:   "borrower_central",
		Actions:  []string{"list", "get", "create", "update", "delete"},
		HasTestMode: true,
		Description: `Manage points of contact for borrowers.

A point of contact is a communication channel (email, phone, etc.)
associated with a borrower.`,
		CreateSchema: `  borrowerId: string    [required] Parent borrower ID
  key: string           [required] Contact type key
  value: string         Contact value (email, phone number, etc.)
  tags: [string]        Tags (default: [])`,
		UpdateSchema: `  value: string         Updated contact value
  tags: [string]        Tags`,
		ResponseSchema: `  id, borrowerId, key, label, value, tags, isTest,
  isVerified, createdAt, updatedAt`,
		FilterHelp: `  borrower-id           Parent borrower ID
  key                   Contact type key
  sort-by               Field to sort by
  sort-direction        "asc" or "desc"`,
	})

	registerResource(ResourceDef{
		Name:     "deal-contacts",
		Singular: "deal-contact",
		BasePath: "/v1/deal-contacts",
		Module:   "borrower_central",
		Actions:  []string{"list", "get", "create", "update", "delete"},
		HasTestMode: true,
		Description: `Manage deal contacts (borrowers linked to deals with a role).

A deal contact associates a borrower with a deal in a specific role
(e.g. co-debtor, guarantor).`,
		CreateSchema: `  dealId: string        [required] Parent deal ID
  borrowerId: string    [required] Linked borrower ID
  role: string          Contact role`,
		UpdateSchema: `  role: string          Updated role`,
		ResponseSchema: `  id, dealId, borrowerId, role, isTest, createdAt, updatedAt`,
		FilterHelp: `  deal-id               Parent deal ID
  borrower-id           Linked borrower ID
  sort-by               Field to sort by
  sort-direction        "asc" or "desc"`,
	})

	registerResource(ResourceDef{
		Name:     "authorizations",
		Singular: "authorization",
		BasePath: "/v1/authorizations",
		Module:   "borrower_central",
		Actions:  []string{"list", "get", "create", "delete"},
		HasTestMode: true,
		Description: `Manage authorizations (consent records for borrowers).

An authorization tracks borrower consent for data access, terms acceptance,
or other legal agreements. Supports digital signatures and OTP verification.`,
		CreateSchema: `  borrowerId: string    [required] Parent borrower ID
  key: string           [required] Authorization type key
  tags: [string]        Tags (default: [])`,
		ResponseSchema: `  id, borrowerId, key, label, tags, isTest,
  isSigned, createdAt, updatedAt`,
		FilterHelp: `  borrower-id           Parent borrower ID
  key                   Authorization type key
  sort-by               Field to sort by
  sort-direction        "asc" or "desc"`,
	})

	registerResource(ResourceDef{
		Name:     "metrics",
		Singular: "metric",
		BasePath: "/v1/metrics",
		Module:   "borrower_central",
		Actions:  []string{"list", "get", "create", "update", "delete"},
		HasTestMode: true,
		Description: `Manage metrics (computed values attached to borrowers).

A metric is a key-value record that stores computed or derived data
for a borrower, such as scores, ratios, or aggregated values.`,
		CreateSchema: `  borrowerId: string    [required] Parent borrower ID
  key: string           [required] Metric key
  value: any            Metric value`,
		UpdateSchema: `  value: any            Updated metric value`,
		ResponseSchema: `  id, borrowerId, key, label, value, isTest, createdAt, updatedAt`,
		FilterHelp: `  borrower-id           Parent borrower ID
  key                   Metric key
  sort-by               Field to sort by
  sort-direction        "asc" or "desc"`,
	})

	registerResource(ResourceDef{
		Name:     "artifacts",
		Singular: "artifact",
		BasePath: "/v1/artifacts",
		Module:   "borrower_central",
		Actions:  []string{"list", "get", "create", "update", "delete"},
		HasTestMode: true,
		Description: `Manage artifacts (versioned document templates and generated content).

An artifact is a versioned template or generated document that can be
associated with borrowers or deals. Supports drafts, publishing, and
version history.`,
		CreateSchema: `  key: string           [required] Artifact key
  borrowerId: string    Parent borrower ID
  dealId: string        Parent deal ID
  tags: [string]        Tags (default: [])`,
		UpdateSchema: `  tags: [string]        Tags`,
		ResponseSchema: `  id, key, label, borrowerId, dealId, tags, isTest,
  createdAt, updatedAt`,
		FilterHelp: `  borrower-id           Parent borrower ID
  deal-id               Parent deal ID
  key                   Artifact key
  sort-by               Field to sort by
  sort-direction        "asc" or "desc"`,
	})

	// --- Workflow execution resources (filter-only, no set-test) ---

	registerResource(ResourceDef{
		Name:     "executions",
		Singular: "execution",
		BasePath: "/v1/executions",
		Module:   "borrower_central",
		Actions:  []string{"list", "get"},
		HasTestFilter: true,
		Description: `View workflow executions.

An execution represents a running or completed workflow step,
such as a scoring model run or data retrieval.`,
		ResponseSchema: `  id, workflowId, workflowAlias, workflowVersion, workflowType,
  borrowerId, dealId, batchId, billableId, status, tags, isTest,
  isSuccess, isBillable, currentDecision, createdAt, executionTime`,
		FilterHelp: `  borrower-id           Parent borrower ID
  deal-id               Parent deal ID
  workflow-id           Workflow ID
  workflow-alias        Workflow alias
  billable-id           Billable ID
  status                Execution status
  sort-by               Field to sort by
  sort-direction        "asc" or "desc"`,
	})

	registerResource(ResourceDef{
		Name:     "execution-batches",
		Singular: "execution-batch",
		BasePath: "/v1/execution-batches",
		Module:   "borrower_central",
		Actions:  []string{"list", "get", "create", "update"},
		HasTestFilter: true,
		Description: `Manage execution batches (bulk workflow runs).

An execution batch runs a workflow across multiple borrowers or deals.
Supports pause, resume, cancel, and retry operations.`,
		CreateSchema: `  workflowId: string    [required] Workflow to execute
  borrowerIds: [string] Borrower IDs to process
  tags: [string]        Tags (default: [])`,
		UpdateSchema: `  tags: [string]        Tags`,
		ResponseSchema: `  id, workflowId, status, totalItems, processedItems, tags, isTest,
  createdAt, updatedAt`,
		FilterHelp: `  workflow-id           Workflow ID
  status                Batch status
  sort-by               Field to sort by
  sort-direction        "asc" or "desc"`,
	})

	registerResource(ResourceDef{
		Name:     "packages",
		Singular: "package",
		BasePath: "/v1/stores/packages",
		Module:   "borrower_central",
		Actions:  []string{"list", "get"},
		Description: `View store packages.

Packages represent installable data source or workflow bundles
available in the AltScore store.`,
		ResponseSchema: `  id, borrowerId, dealId, assetId, sourceId, alias, workflowId,
  label, contentType, tags, createdAt, ttl, hasAttachments, forcedStale`,
		FilterHelp: `  borrower-id           Parent borrower ID
  deal-id               Parent deal ID
  asset-id              Asset ID
  source-id             Source ID
  workflow-id           Workflow ID
  alias                 Package alias
  sort-by               Field to sort by
  sort-direction        "asc" or "desc"`,
	})

	// --- Config/rules entities ---

	dmGroup := registerResource(ResourceDef{
		Name:     "data-models",
		Singular: "data-model",
		BasePath: "/v1/data-models",
		Module:   "borrower_central",
		Actions:  []string{"list", "get", "create", "update", "delete"},
		HasTestMode: true,
		Description: `Manage data-models (schema definitions for all AltScore entities).

A data-model defines a key within an entity type: identity keys, borrower fields,
steps, deal fields, asset groups, etc. Data-models control what fields exist on
borrowers, deals, and assets, and what values those fields accept.

There are 16 entity types grouped into 5 categories:
  core:     identity, contact, document, borrower, point_of_contact,
            authorization, metric, accounting_document
  fields:   borrower_field
  workflow:  step, decision
  deals:    deal_field, deal_step, deal_role
  assets:   asset_field, asset_group

Use 'data-models guide' for detailed documentation on each entity type,
required fields, validation rules, and create examples.`,
		CreateSchema: `  key: string           [required] Unique key within entity type (per tenant)
  label: string         [required] Display name
  entityType: string    [required] One of the 16 entity types
  priority: int         [required for identity] Sort order (>= -1, -1 = append)
  order: int            [required for step, deal_step] Sequence position
  allowedValues: [any]  Restrict values (only: borrower_field, asset_field, deal_field)
  dataType: string      Type hint: "string", "number", "boolean", "date"
  isSensitive: bool     Enable encryption (one-way, identity only)
  isSegmentationField: bool  Enable audience segmentation
  metadata: object      Free-form key-value metadata
  path: string          Optional hierarchical path`,
		UpdateSchema: `  key: string           Updated key
  label: string         Updated display name
  order: int            Updated order (step/deal_step)
  priority: int         Updated priority (identity)
  allowedValues: [any]  Updated allowed values
  dataType: string      Updated type hint
  isSegmentationField: bool  Updated segmentation flag
  metadata: object      Updated metadata
  path: string          Updated path
  Note: isSensitive cannot be changed via update; use make-sensitive`,
		ResponseSchema: `  id, path, key, label, entityType, priority, order, isTest,
  allowedValues, dataType, metadata, isSegmentationField, isSensitive,
  createdAt, updatedAt, deletedAt`,
		FilterHelp: `  key                   Filter by key
  entity-type           Filter by entity type
  search                Text search
  sort-by               Field to sort by
  sort-direction        "asc" or "desc"`,
	})
	dmGroup.AddCommand(makeDmMakeSensitiveCmd())
	dmGroup.AddCommand(makeDmGuideCmd())

	evGroup := registerResource(ResourceDef{
		Name:     "evaluators",
		Singular: "evaluator",
		BasePath: "/v1/evaluators",
		Module:   "borrower_central",
		Actions:  []string{"list", "get", "create", "update", "delete"},
		HasTestMode: true,
		Description: `Manage evaluators (code-based rule engines).

An evaluator is a versioned Python-based evaluation engine that runs business
rules, scorecards, and scoring models. Given an instance (subject + variables)
and optional entities (co-debtors, guarantors), it returns a decision with
score, scorecard breakdown, metrics, and rule hits.`,
		CreateSchema: `  alias: string         [required] Unique evaluator identifier
  version: string       [required] Version string (e.g. "v1")
  label: string         Display name
  description: string   Description
  specs: object         Evaluator specification (rules, scorecard, metrics config)`,
		UpdateSchema: `  label: string         Display name
  description: string   Description
  specs: object         Updated specification`,
		ResponseSchema: `  id, alias, version, label, description, specs, isTest,
  createdAt, updatedAt`,
		FilterHelp: `  sort-by               Field to sort by
  sort-direction        "asc" or "desc"`,
	})
	evGroup.AddCommand(makeEvEvaluateCmd())
	evGroup.AddCommand(makeEvEvaluateByAliasCmd())

	registerResource(ResourceDef{
		Name:     "evaluation-rules",
		Singular: "evaluation-rule",
		BasePath: "/v1/evaluation-rules",
		Module:   "borrower_central",
		Actions:  []string{"list", "get", "create", "update", "delete"},
		HasTestMode: true,
		Description: `Manage evaluation rules (individual business rules).

An evaluation rule defines a single business rule with conditions and actions
that can be composed into evaluators or used standalone.`,
		CreateSchema: `  alias: string         [required] Unique rule identifier
  label: string         Display name
  description: string   Description
  specs: object         Rule specification`,
		UpdateSchema: `  label: string         Display name
  description: string   Description
  specs: object         Updated specification`,
		ResponseSchema: `  id, alias, label, description, specs, isTest, createdAt, updatedAt`,
		FilterHelp: `  sort-by               Field to sort by
  sort-direction        "asc" or "desc"`,
	})

	registerResource(ResourceDef{
		Name:     "policy-rules",
		Singular: "policy-rule",
		BasePath: "/v1/rules",
		Module:   "borrower_central",
		Actions:  []string{"list", "get", "create", "update", "delete"},
		HasTestMode: true,
		Description: `Manage policy rules (automated policy enforcement rules).

A policy rule defines conditions that trigger alerts or actions on borrowers,
deals, or other entities when certain criteria are met.`,
		CreateSchema: `  alias: string         [required] Unique rule identifier
  label: string         Display name
  description: string   Description
  specs: object         Rule specification`,
		UpdateSchema: `  label: string         Display name
  description: string   Description
  specs: object         Updated specification`,
		ResponseSchema: `  id, alias, label, description, specs, isTest, createdAt, updatedAt`,
		FilterHelp: `  sort-by               Field to sort by
  sort-direction        "asc" or "desc"`,
	})

	registerResource(ResourceDef{
		Name:     "rule-trees",
		Singular: "rule-tree",
		BasePath: "/v1/rule-trees",
		Module:   "borrower_central",
		Actions:  []string{"list", "get", "create", "update", "delete"},
		HasTestMode: true,
		Description: `Manage rule trees (hierarchical decision trees).

A rule tree is a tree-structured set of conditions and outcomes used
for complex decision logic with branching paths.`,
		CreateSchema: `  alias: string         [required] Unique tree identifier
  label: string         Display name
  description: string   Description
  specs: object         Tree specification`,
		UpdateSchema: `  label: string         Display name
  description: string   Description
  specs: object         Updated specification`,
		ResponseSchema: `  id, alias, label, description, specs, isTest, createdAt, updatedAt`,
		FilterHelp: `  sort-by               Field to sort by
  sort-direction        "asc" or "desc"`,
	})

	// --- Workflow development resources ---

	wtGroup := registerResource(ResourceDef{
		Name:     "workflow-tasks",
		Singular: "workflow-task",
		BasePath: "/v1/workflow-tasks",
		Module:   "borrower_central",
		Actions:  []string{"list", "get", "create", "update", "delete"},
		Description: `Manage workflow tasks (Python functions for remote-tasks workflows).

A workflow task is a versioned Python function with typed Pydantic input/output
models. Tasks are the atomic units of remote-tasks workflow DAGs.`,
		CreateSchema: `  alias: string         [required] Unique task identifier
  label: string         [required] Display name
  code: string          Python code with InputData, OutputData, execute()`,
		UpdateSchema: `  label: string         Display name
  description: string   Description
  code: string          Python code (creates new version)`,
		ResponseSchema: `  id, alias, label, version, latestVersion, code,
  inputSchema, outputSchema, isPublished, publishedVersion,
  isLocked, lockedBy, lockedAt, createdAt, updatedAt`,
		FilterHelp: `  sort-by               Field to sort by
  sort-direction        "asc" or "desc"`,
	})
	wtGroup.AddCommand(makeWtPublishCmd())
	wtGroup.AddCommand(makeWtUnpublishCmd())
	wtGroup.AddCommand(makeWtVersionsCmd())
	wtGroup.AddCommand(makeWtValidateCmd())
	wtGroup.AddCommand(makeWtExecuteCmd())
	wtGroup.AddCommand(makeWtLambdaCmd())

	ttGroup := registerResource(ResourceDef{
		Name:     "task-tests",
		Singular: "task-test",
		BasePath: "/v1/task-tests",
		Module:   "borrower_central",
		Actions:  []string{"list", "get", "create", "update", "delete"},
		Description: `Manage task test cases for workflow tasks.

Each test case has input data, context, and expected output. The test runner
executes the task code and compares actual vs expected using DeepDiff.`,
		CreateSchema: `  taskId: string        [required] Parent task ID
  name: string          [required] Test name
  testType: string      "unit_test" or "integration_test"
  inputData: object     Input data for the task
  context: object       Context dict (token, tenant, etc.)
  expectedOutputData: object  Expected output to compare against`,
		UpdateSchema: `  name: string          Test name
  testType: string      Test type
  inputData: object     Input data
  context: object       Context
  expectedOutputData: object  Expected output`,
		ResponseSchema: `  id, taskId, name, description, testType,
  inputData, context, expectedOutputData, createdAt, updatedAt`,
		FilterHelp: `  taskId                Filter by parent task ID
  testType              "unit_test" or "integration_test"
  search                Text search
  sort-by               Field to sort by
  sort-direction        "asc" or "desc"`,
	})
	ttGroup.AddCommand(makeTtRunCmd())
	ttGroup.AddCommand(makeTtRunAllCmd())
	ttGroup.AddCommand(makeTtByTaskCmd())

	wfGroup := registerResource(ResourceDef{
		Name:     "workflows",
		Singular: "workflow",
		BasePath: "/v1/workflows",
		Module:   "borrower_central",
		Actions:  []string{"list", "get", "create", "update", "delete"},
		Description: `Manage workflows (DAG definitions for task orchestration).

Create remote-tasks workflows by passing remoteTasks: true. The flow definition
describes the DAG: which tasks run, how data flows between them, and retry behavior.`,
		CreateSchema: `  alias: string         [required] Unique workflow identifier
  version: string       [required] Version string (e.g. "v1")
  label: string         Display name
  remoteTasks: bool     Set true for remote-tasks engine
  inputSchema: string   JSON Schema string defining workflow inputs
  flowDefinition: object  DAG definition with task_instances and connections`,
		UpdateSchema: `  label: string         Display name
  flowDefinition: object  Updated DAG definition
  inputSchema: string   Updated input schema`,
		ResponseSchema: `  id, alias, version, label, type, engine,
  flowDefinition, inputSchema, nodes, edges, metadata,
  createdAt, updatedAt`,
		FilterHelp: `  sort-by               Field to sort by
  sort-direction        "asc" or "desc"`,
	})
	wfGroup.AddCommand(makeWfExecuteCmd())
	wfGroup.AddCommand(makeWfExecuteByAliasCmd())
	wfGroup.AddCommand(makeWfUpdateSchemaCmd())
	wfGroup.AddCommand(makeWfInputSchemaGuideCmd())
}

// Execute runs the root command.
func Execute() error {
	return rootCmd.Execute()
}

// loadClient resolves the active profile and returns a configured API client.
// Used by commands that need to make API calls.
func loadClient() (*client.Client, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}

	profileName := config.ResolveProfile(cfg, flagProfile)
	profile := config.GetProfile(cfg, profileName)

	if flagEnvironment != "" {
		profile.Environment = flagEnvironment
	}
	if flagTenant != "" {
		profile.TenantID = flagTenant
	}

	if profile.Environment == "" {
		return nil, fmt.Errorf("no environment configured for profile %q. Run: altscore login --profile %s --environment <env>", profileName, profileName)
	}

	if profile.AccessToken == "" {
		fmt.Fprintf(os.Stderr, "warning: no access token for profile %q. Run: altscore login --profile %s\n", profileName, profileName)
	}

	c := client.New(cfg, profileName, &profile, flagVerbose)

	// Parse --base-url overrides
	if len(flagBaseURLs) > 0 {
		c.BaseURLOverrides = make(map[string]string)
		for _, entry := range flagBaseURLs {
			parts := strings.SplitN(entry, "=", 2)
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				return nil, fmt.Errorf("invalid --base-url format %q (expected module=url)", entry)
			}
			c.BaseURLOverrides[parts[0]] = parts[1]
		}
	}

	return c, nil
}
