package cmd

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

// /v2/tasks holds the executable config for v2 workflow nodes (HTTP urls,
// evaluator aliases, sources_config, branches, etc.). Each task has an
// alias and a version sequence; a workflow node references a task by
// alias (and optionally pinned version).
//
// API endpoints:
//   POST   /v2/tasks                          create new task
//   POST   /v2/tasks/{alias}                  create new version
//   GET    /v2/tasks                          list (paginated, filterable)
//   GET    /v2/tasks/{alias}                  get latest (with version history)
//   GET    /v2/tasks/{alias}/services/methods SOAP method introspection
//   DELETE /v2/tasks/{alias}                  delete every version of a task

func registerTasksV2(parent *cobra.Command) {
	group := &cobra.Command{
		Use:   "tasks-v2",
		Short: "Manage v2 workflow tasks (executable config referenced by graph nodes)",
		Long: `Tasks-v2 are versioned executable building blocks at /v2/tasks. Each v2
workflow node references a task by alias (and optionally pinned version).

Common types: altdata-enrichment, evaluate-rules, http, conditional, wait,
webhook, compute-variables, data-store, pdf-report, end. Each type has
its own config fields (sourcesConfig for altdata, evaluatorAlias for
evaluators, url+method for http, branches for conditional, etc.).

Subcommands:
  list             paginated task list with filters (alias-prefix, type, workflow-alias)
  get              latest version + version history for one task
  create           create a new task (auto-generates alias if omitted)
  create-version   bump a task's version sequence
  delete           hard-delete every version of a task (refuses if referenced)
  get-soap-methods SOAP method introspection for soap-typed tasks

Run 'altscore workflows-v2 schema-guide taskTypes' for the field list per type.`,
	}
	group.AddCommand(makeTv2CreateCmd())
	group.AddCommand(makeTv2CreateVersionCmd())
	group.AddCommand(makeTv2GetCmd())
	group.AddCommand(makeTv2ListCmd())
	group.AddCommand(makeTv2DeleteCmd())
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

func makeTv2ListCmd() *cobra.Command {
	var (
		aliasPrefix   string
		typeFilter    string
		workflowAlias string
		page          int
		perPage       int
		sortBy        string
		sortDirection string
		includeAll    bool
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List v2 tasks (paginated, filterable; GET /v2/tasks)",
		Long: `List task definitions. By default returns one row per task alias
(the latest version). Pass --include-all to dump every historical version.

Useful for cleanup after a failed 'workflows-v2 apply': filter by
--alias-prefix to enumerate orphan tasks left behind by a previous attempt,
then delete each via 'tasks-v2 delete <alias>'.`,
		Example: `  # Find every task whose alias starts with 'co2-' (cleanup workflow)
  altscore tasks-v2 list --alias-prefix co2- --per-page 50

  # List only HTTP and conditional tasks
  altscore tasks-v2 list --type http,conditional

  # Show every task currently referenced by a specific workflow
  altscore tasks-v2 list --workflow-alias kyc-lite

  # Dump every historical version (not just is_latest=true)
  altscore tasks-v2 list --include-all`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			params := url.Values{}
			if aliasPrefix != "" {
				params.Set("alias-prefix", aliasPrefix)
			}
			if typeFilter != "" {
				params.Set("type", typeFilter)
			}
			if workflowAlias != "" {
				params.Set("workflow-alias", workflowAlias)
			}
			if includeAll {
				params.Set("is-latest", "false")
			}
			if page > 0 {
				params.Set("page", fmt.Sprintf("%d", page))
			}
			if perPage > 0 {
				params.Set("per-page", fmt.Sprintf("%d", perPage))
			}
			if sortBy != "" {
				params.Set("sort-by", sortBy)
			}
			if sortDirection != "" {
				params.Set("sort-direction", sortDirection)
			}

			path := "/v2/tasks"
			if encoded := params.Encode(); encoded != "" {
				path = path + "?" + encoded
			}

			data, _, err := c.Do("GET", "borrower_central", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
	cmd.Flags().StringVar(&aliasPrefix, "alias-prefix", "",
		"Filter to tasks whose alias starts with this prefix")
	cmd.Flags().StringVar(&typeFilter, "type", "",
		"Filter by task type (single value or comma-separated list)")
	cmd.Flags().StringVar(&workflowAlias, "workflow-alias", "",
		"Restrict results to tasks referenced by this workflow's latest version")
	cmd.Flags().IntVar(&page, "page", 1, "Page number (1-indexed)")
	cmd.Flags().IntVar(&perPage, "per-page", 50, "Items per page (max 200)")
	cmd.Flags().StringVar(&sortBy, "sort-by", "createdAt",
		"Sort field (e.g. createdAt, alias, type, version)")
	cmd.Flags().StringVar(&sortDirection, "sort-direction", "desc",
		"Sort direction: asc or desc")
	cmd.Flags().BoolVar(&includeAll, "include-all", false,
		"Include every historical version, not just the latest per alias")
	return cmd
}

func makeTv2DeleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <alias>",
		Short: "Delete every version of a v2 task (DELETE /v2/tasks/{alias})",
		Long: `Hard-deletes every version of the task identified by alias. The server
refuses (409) if any non-archived workflow's latest version still has a
node referencing this task -- in that case the response payload lists each
referencing workflow + node so you can detach them before retrying.

Use 'tasks-v2 list --alias-prefix <prefix>' beforehand to enumerate residue
from a failed 'workflows-v2 apply' (the typical cleanup path).`,
		Example: `  altscore tasks-v2 delete co2-orphan-task
  altscore tasks-v2 list --alias-prefix co2- --per-page 50 | jq -r '.[].alias' | \
    xargs -n1 altscore tasks-v2 delete`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			alias := args[0]
			path := fmt.Sprintf("/v2/tasks/%s", alias)
			data, status, err := c.Do("DELETE", "borrower_central", path, nil)
			if err != nil {
				// Surface 409 referencedBy payload as an actionable message so
				// users see which workflows still pin the task without having
				// to re-parse the raw envelope.
				if status == 409 {
					return formatTv2DeleteConflict(alias, err)
				}
				if status == 404 {
					return fmt.Errorf("task %q not found", alias)
				}
				return err
			}
			if len(data) == 0 {
				// 204 (no body) -- mint a minimal confirmation so stdout is
				// always JSON for downstream pipes.
				return output.RawJSON(json.RawMessage(
					fmt.Sprintf(`{"deleted":%q}`, alias),
				))
			}
			return output.RawJSON(data)
		},
	}
	return cmd
}

// formatTv2DeleteConflict re-parses the server's 409 envelope to surface the
// referencedBy details inline. Falls back to the raw error if the envelope
// doesn't match the expected shape.
func formatTv2DeleteConflict(alias string, original error) error {
	// The client formatHTTPError already stringified the response; try to
	// pull the details payload back out of its (key=value) tail. If that
	// fails, just return the original error -- the user still sees the
	// envelope text.
	msg := original.Error()
	if !strings.Contains(msg, "referencedBy") {
		return original
	}
	// Look for the JSON-ish referencedBy=[{...}] fragment and try to parse it.
	start := strings.Index(msg, "referencedBy=")
	if start < 0 {
		return original
	}
	tail := msg[start+len("referencedBy="):]
	depth := 0
	end := -1
	for i, r := range tail {
		switch r {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				end = i + 1
			}
		}
		if end > 0 {
			break
		}
	}
	if end <= 0 {
		return original
	}
	fragment := tail[:end]
	var refs []map[string]string
	if err := json.Unmarshal([]byte(fragment), &refs); err != nil {
		return original
	}
	parts := make([]string, 0, len(refs))
	for _, ref := range refs {
		parts = append(parts, fmt.Sprintf(
			"%s (node %s)", ref["alias"], ref["nodeId"],
		))
	}
	return fmt.Errorf(
		"task %q is referenced by %d workflow(s): %s; detach those nodes before deleting",
		alias, len(refs), strings.Join(parts, ", "),
	)
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
