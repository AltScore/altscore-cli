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
	registerResources()
}

func registerResources() {
	registerResource(ResourceDef{
		Name:     "borrowers",
		Singular: "borrower",
		BasePath: "/v1/borrowers",
		Module:   "borrower_central",
		Actions:  []string{"list", "get", "create", "update", "delete"},
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
		ResponseSchema: `  id, persona, label, externalId, avatarUrl, tags, flag,
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
		Description: `Manage identities attached to borrowers.

An identity is a key-value pair associated with a borrower, such as
a tax ID, email address, or phone number. Identities are used to
uniquely identify and deduplicate borrowers.`,
		CreateSchema: `  borrowerId: string    [required] Parent borrower ID
  key: string           [required] Identity type (e.g. "email", "tax-id")
  value: string         Identity value
  tags: [string]        Tags (default: [])`,
		ResponseSchema: `  id, borrowerId, key, label, value (masked), priority, tags,
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
		ResponseSchema: `  id, borrowerId, dealId, key, label, value, tags,
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
		ResponseSchema: `  id, label, description, status, externalId,
  currentStep{stepId, order, key, label, createdAt},
  riskRating, tags, createdAt, updatedAt`,
		FilterHelp: `  borrower-id           Parent borrower ID
  status                Deal status
  external-id           External system ID
  sort-by               Field to sort by
  sort-direction        "asc" or "desc"`,
	})

	registerResource(ResourceDef{
		Name:     "executions",
		Singular: "execution",
		BasePath: "/v1/executions",
		Module:   "borrower_central",
		Actions:  []string{"list", "get"},
		Description: `View workflow executions.

An execution represents a running or completed workflow step,
such as a scoring model run or data retrieval.`,
		ResponseSchema: `  id, workflowId, workflowAlias, workflowVersion, workflowType,
  borrowerId, dealId, batchId, billableId, status, tags,
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

	// Workflow development resources

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
