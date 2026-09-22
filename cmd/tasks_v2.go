package cmd

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/AltScore/altscore-cli/internal/client"
	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

func registerTasksV2(parent *cobra.Command) {
	group := &cobra.Command{
		Use:   "tasks-v2",
		Short: "Manage v2 workflow tasks (executable config referenced by graph nodes)",
		Long: `Tasks-v2 are versioned executable building blocks at /v2/tasks. Each v2
workflow node references a task by alias (and optionally pinned version).

Common types: altdata-enrichment, evaluate-rules, http, conditional, wait,
compute-variables, data-store-write, data-store-query, end.
(PDF generation is endConfig on the end node, not a task type.) Each type has
its own config fields (sourcesConfig for altdata, evaluatorAlias for
evaluators, url+method for http, branches for conditional, etc.).

Subcommands:
  list             paginated task list with filters (alias-prefix, type, workflow-alias)
  get              latest version + version history for one task
  create           create a new task (auto-generates alias if omitted)
  create-version   bump a task's version sequence
  delete           hard-delete every version of a task (refuses if referenced)

Run 'altscore workflows-v2 schema-guide taskTypes' for the field list per type.`,
	}
	group.AddCommand(makeTv2CreateCmd())
	group.AddCommand(makeTv2CreateVersionCmd())
	group.AddCommand(makeTv2GetCmd())
	group.AddCommand(makeTv2ListCmd())
	group.AddCommand(makeTv2DeleteCmd())
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
			// nil target types: a brand-new task carries nothing forward, so a
			// deprecated type here is unambiguously new authoring and refused.
			if err := validateTaskV2Body(body, nil); err != nil {
				return err
			}
			if err := deriveAltdataInputKeysForCreate(c, &body); err != nil {
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

func taskV2BodyType(body json.RawMessage) string {
	var task struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(body, &task); err != nil {
		return ""
	}
	return task.Type
}

// A bump of a task whose type is ALREADY retired is not new authoring. Any lookup failure
// yields nil, so the refusal stands rather than the CLI going looser than the server.
func deprecatedCarryForwardForTaskVersion(c *client.Client, alias string, body json.RawMessage) map[string]bool {
	bodyType := taskV2BodyType(body)
	if !deprecatedTaskTypes[bodyType] {
		return nil
	}
	data, _, err := c.Do("GET", "borrower_central", fmt.Sprintf("/v2/tasks/%s", alias), nil)
	if err != nil {
		return nil
	}
	if taskV2BodyType(data) != bodyType {
		return nil
	}
	return map[string]bool{bodyType: true}
}

func makeTv2CreateVersionCmd() *cobra.Command {
	var bodyFlag, lockToken, clientID string

	cmd := &cobra.Command{
		Use:   "create-version <alias>",
		Short: "Create a new version of an existing v2 task (POST /v2/tasks/{alias})",
		Long: `Bumps the task's version sequence; existing workflow nodes pinning a
specific version are unaffected. Body shape is the same as 'create'
minus the alias.

A DRAFT floats on the latest task version, so this write edits every draft
whose graph points at the task. The server attributes it to those workflows
and applies their edit lock: a draft someone has open in the Hub answers
423 LOCKED, even when that someone is you in another tab, because this CLI
identifies itself as a separate editor (X-Lock-Client-Id). Pass --lock-token
from 'workflows-v2 lock acquire' to write as the lock holder.`,
		Example: `  altscore tasks-v2 create-version fetch-ecu --body '{
    "label":"Fetch ECU bureau v2",
    "type":"altdata-enrichment",
    "sourcesConfig":[{"sourceId":"ECU-PUB-0002","version":"v1"},{"sourceId":"ECU-PUB-0014","version":"v1"}]
  }'
  altscore tasks-v2 create-version fetch-ecu --body @task.json --lock-token "$TOKEN"`,
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
			carriedForward := deprecatedCarryForwardForTaskVersion(c, args[0], body)
			if err := validateTaskV2Body(body, carriedForward); err != nil {
				return err
			}
			for _, typ := range sortedKeys(carriedForward) {
				warnDeprecatedTaskTypeCarriedForward(fmt.Sprintf("task %q", args[0]), typ)
			}
			if err := deriveAltdataInputKeysForCreate(c, &body); err != nil {
				return err
			}
			if clientID == "" {
				clientID = defaultLockClientID(c.ProfileName)
			}
			data, err := postTaskVersion(c, args[0], body, lockToken, clientID)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
	cmd.Flags().StringVar(&bodyFlag, "body", "", "JSON body (or pipe via stdin)")
	cmd.Flags().StringVar(&lockToken, "lock-token", "", "lockToken from 'workflows-v2 lock acquire' on the workflow this task belongs to; sent as X-Lock-Token")
	cmd.Flags().StringVar(&clientID, "client-id", "", "editor identity sent as X-Lock-Client-Id (default: cli-<profile>-<host>-<pid>)")
	return cmd
}

func taskVersionLockHeaders(lockToken, clientID string) map[string]string {
	headers := map[string]string{}
	if clientID != "" {
		headers["X-Lock-Client-Id"] = clientID
	}
	if lockToken != "" {
		headers["X-Lock-Token"] = lockToken
	}
	return headers
}

func postTaskVersion(c *client.Client, alias string, body json.RawMessage, lockToken, clientID string) (json.RawMessage, error) {
	path := fmt.Sprintf("/v2/tasks/%s", alias)
	data, status, err := c.DoWithHeaders("POST", "borrower_central", path, body, taskVersionLockHeaders(lockToken, clientID))
	if err != nil {
		if status == 423 || strings.Contains(err.Error(), "HTTP 423") {
			return nil, fmt.Errorf("%w\n"+
				"# task %q is a node of a workflow whose draft is being edited (a Hub tab, possibly your own).\n"+
				"# Wait for the editor to close the tab, or hold the lock yourself: `altscore workflows-v2 lock acquire <alias>` and re-run with --lock-token.", err, alias)
		}
		return nil, err
	}
	return data, nil
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

// The client's formatHTTPError already stringified the response, so the details payload
// has to be pulled back out of its (key=value) tail.
func formatTv2DeleteConflict(alias string, original error) error {
	msg := original.Error()
	if !strings.Contains(msg, "referencedBy") {
		return original
	}
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
