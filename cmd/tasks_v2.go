package cmd

import (
	"fmt"

	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

// /v2/tasks holds the executable config for v2 workflow nodes (HTTP urls,
// evaluator aliases, sources_config, branches, etc.). Each task has an
// alias and a version sequence; a workflow node references a task by
// alias (and optionally pinned version).
//
// API endpoints (no LIST; tasks are discovered via the workflows that use them):
//   POST   /v2/tasks                          create new task
//   POST   /v2/tasks/{alias}                  create new version
//   GET    /v2/tasks/{alias}                  get latest (with version history)
//   GET    /v2/tasks/{alias}/services/methods SOAP method introspection

func registerTasksV2(parent *cobra.Command) {
	group := &cobra.Command{
		Use:   "tasks-v2",
		Short: "Manage v2 workflow tasks (executable config referenced by graph nodes)",
		Long: `Tasks-v2 are versioned executable building blocks at /v2/tasks. Each v2
workflow node references a task by alias (and optionally pinned version).

Active types (palette + backend): altdata-enrichment, http, conditional,
wait, exception, comment, start, end, evaluate-rules, mapping-table,
scorecard, rule-tree, compute-variables, customer, deal, asset,
credit-line, child-workflow, array-router, list-of-similars,
data-store-write, data-store-query.

Each type has its own config fields (sourcesConfig for altdata,
scorecardCode for scorecard, url+method for http, branches for
conditional, entries[] for mapping-table, etc.).

Run 'altscore workflows-v2 schema-guide tasks' for the canonical
per-type field list. The 'tasks.deprecatedTypes' subsection in that
output covers types you may see in legacy workflow bodies but
shouldn't use in new ones (data-store, pdf-report, webhook,
create-identity, create-borrower, update-borrower,
fetch-borrower-entities, fetch-entity, soap).`,
	}
	group.AddCommand(makeTv2CreateCmd())
	group.AddCommand(makeTv2CreateVersionCmd())
	group.AddCommand(makeTv2GetCmd())
	group.AddCommand(makeTv2GetSoapMethodsCmd())
	parent.AddCommand(group)
}

func makeTv2CreateCmd() *cobra.Command {
	var bodyFlag string

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a v2 task (POST /v2/tasks)",
		Long: `Create a new task. The body must include 'label' and 'type'; 'alias' is
optional and auto-generated if omitted. Type-specific fields go in the
same body (e.g. sourcesConfig for altdata-enrichment).

Returns the created task DTO including its id, alias, and version=1.`,
		Example: `  # AltData enrichment task
  altscore tasks-v2 create --body '{
    "alias":"fetch-ecu",
    "label":"Fetch ECU bureau",
    "type":"altdata-enrichment",
    "sourcesConfig":[{"sourceId":"ECU-PUB-0002","version":"v1"}],
    "borrowerIdField":"borrower_id",
    "inputSchema":{"borrower_id":{"type":"string"}},
    "required":["borrower_id"]
  }'

  # Evaluator task
  altscore tasks-v2 create --body '{
    "alias":"score",
    "label":"Score",
    "type":"evaluate-rules",
    "evaluatorTask":"scoring",
    "inputSchema":{"credit_data":{"type":"object"}}
  }'

  # HTTP task with templated url
  altscore tasks-v2 create --body '{
    "alias":"notify-approve",
    "label":"Webhook approve",
    "type":"http",
    "url":"https://example.com/approve/{{inputs.borrower_id}}",
    "method":"POST",
    "headers":"{\"Content-Type\":\"application/json\"}"
  }'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			body, err := readBody(bodyFlag)
			if err != nil {
				return err
			}
			if err := validateTaskV2Body(body); err != nil {
				return err
			}
			data, _, err := c.Do("POST", "borrower_central", "/v2/tasks", body)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
	cmd.Flags().StringVar(&bodyFlag, "body", "", "JSON body (or pipe via stdin)")
	return cmd
}

func makeTv2CreateVersionCmd() *cobra.Command {
	var bodyFlag string

	cmd := &cobra.Command{
		Use:   "create-version <alias>",
		Short: "Create a new version of an existing v2 task (POST /v2/tasks/{alias})",
		Long: `Bumps the task's version sequence; existing workflow nodes pinning a
specific version are unaffected. Body shape is the same as 'create'
minus the alias.`,
		Example: `  altscore tasks-v2 create-version fetch-ecu --body '{
    "label":"Fetch ECU bureau v2",
    "type":"altdata-enrichment",
    "sourcesConfig":[{"sourceId":"ECU-PUB-0002","version":"v1"},{"sourceId":"ECU-PUB-0014","version":"v1"}]
  }'`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			body, err := readBody(bodyFlag)
			if err != nil {
				return err
			}
			if err := validateTaskV2Body(body); err != nil {
				return err
			}
			path := fmt.Sprintf("/v2/tasks/%s", args[0])
			data, _, err := c.Do("POST", "borrower_central", path, body)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
	cmd.Flags().StringVar(&bodyFlag, "body", "", "JSON body (or pipe via stdin)")
	return cmd
}

func makeTv2GetCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "get <alias>",
		Short:   "Get the latest version of a v2 task plus its version history",
		Args:    cobra.ExactArgs(1),
		Example: `  altscore tasks-v2 get fetch-ecu`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/v2/tasks/%s", args[0])
			data, _, err := c.Do("GET", "borrower_central", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}

func makeTv2GetSoapMethodsCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "get-soap-methods <alias>",
		Short:   "Introspect SOAP methods from a task's WSDL URL",
		Long:    `For tasks of type 'soap', returns the available methods + schemas defined by the WSDL.`,
		Args:    cobra.ExactArgs(1),
		Example: `  altscore tasks-v2 get-soap-methods my-soap-task`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/v2/tasks/%s/services/methods", args[0])
			data, _, err := c.Do("GET", "borrower_central", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}
