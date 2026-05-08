package cmd

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

func makeExQueryOutputsCmd() *cobra.Command {
	var filters []string
	var perPage int
	var page int
	var includeTests bool
	var testOnly bool

	cmd := &cobra.Command{
		Use:   "query-outputs",
		Short: "Query execution outputs",
		Long: `Query execution outputs across all executions. Returns a JSON array
of output records with the execution's result data, custom output, and
attachment flags.

Use --filter for field-based filters, --per-page and --page for pagination.

Available filters (pass via --filter key=value):
  borrower-id           Parent borrower ID
  deal-id               Parent deal ID
  workflow-id           Workflow ID
  workflow-alias        Workflow alias
  billable-id           Billable ID
  status                Execution status
  sort-by               Field to sort by
  sort-direction        "asc" or "desc"

Response fields:
  id, billableId, borrowerId, dealId, workflowId, workflowAlias,
  workflowVersion, workflowRevisionId, workflowType, status,
  isSuccess, output, customOutput, hasAttachments, createdAt`,
		Example: `  # Query outputs for a borrower
  altscore executions query-outputs --filter borrower-id=<id>

  # Query outputs for a specific workflow
  altscore executions query-outputs --filter workflow-alias=my-workflow

  # Check which outputs have attachments
  altscore executions query-outputs --filter borrower-id=<id> | jq '.[] | select(.hasAttachments)'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}

			params := []string{}

			if includeTests && testOnly {
				return fmt.Errorf("--include-tests and --test-only are mutually exclusive")
			}
			if includeTests {
				params = append(params, "include-tests=true")
			}
			if testOnly {
				params = append(params, "test-only=true")
			}

			if perPage > 0 {
				params = append(params, fmt.Sprintf("per-page=%d", perPage))
			} else if c.Config.Defaults.PerPage > 0 {
				params = append(params, fmt.Sprintf("per-page=%d", c.Config.Defaults.PerPage))
			}
			if page > 0 {
				params = append(params, fmt.Sprintf("page=%d", page))
			}

			for _, f := range filters {
				params = append(params, f)
			}

			path := "/v1/executions/outputs"
			if len(params) > 0 {
				path += "?" + strings.Join(params, "&")
			}

			data, _, err := c.Do("GET", "borrower_central", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	cmd.Flags().StringArrayVar(&filters, "filter", nil, "field filter in key=value format (repeatable)")
	cmd.Flags().IntVar(&perPage, "per-page", 0, "items per page (default: from config)")
	cmd.Flags().IntVar(&page, "page", 0, "page number (default: 1)")
	cmd.Flags().BoolVar(&includeTests, "include-tests", false, "include test records in results")
	cmd.Flags().BoolVar(&testOnly, "test-only", false, "return only test records")

	return cmd
}

func makeExGetOutputCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get-output <execution-id>",
		Short: "Get the output of an execution",
		Long: `Retrieve the output data for a single execution by its ID.

Returns the execution's result including output data, custom output,
success status, and whether attachments are available.

Response fields:
  id, billableId, borrowerId, dealId, workflowId, workflowAlias,
  workflowVersion, workflowRevisionId, workflowType, status,
  isSuccess, output, customOutput, hasAttachments, createdAt`,
		Example: `  altscore executions get-output <execution-id>
  altscore executions get-output <execution-id> | jq '.output'
  altscore executions get-output <execution-id> | jq '.customOutput'`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}

			path := fmt.Sprintf("/v1/executions/%s/output", args[0])
			data, _, err := c.Do("GET", "borrower_central", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}

// makeExStateCmd surfaces per-task outputs and per-task statuses for an
// execution, with a primary read of /v1/executions/{id}/state (what the Hub's
// ExecutionDebugPanel uses) and a fallback to /v1/executions/{id}/output's
// customOutput when no state record exists.
//
// We expose a single intent-driven command rather than two endpoint-driven
// ones because agents asking "what did each task produce?" don't care which
// collection answered -- they care that the data is reachable in one call.
//
// State vs Output, in plain terms:
//   - /state has live engine state -- task_outputs, task_states, execution_logs.
//     Populated for in-flight executions and ones that explicitly persist state
//     (matches what the Hub's debug panel shows). Often 404 for completed sync
//     v2 GraphWorkflow runs.
//   - /output is the durable post-execution record. v2 declarative tasks write
//     their results into customOutput, keyed by task type (e.g. customOutput.
//     altdata_enrichment_result[], customOutput.scorecard_result[]).
//
// The fallback fetches /output and returns just the customOutput slice, plus a
// '_source' marker so callers can tell which surface answered. Hub-style
// drilldowns (state.data_flow.task_outputs.<alias>) only work on the first
// path; on the fallback, drill via .customOutput.<task_type>_result.
func makeExStateCmd() *cobra.Command {
	var noFallback bool
	cmd := &cobra.Command{
		Use:   "state <execution-id>",
		Short: "Get per-task outputs and statuses for an execution",
		Long: `Retrieve per-task outputs and per-task statuses for a workflow execution.

Reads two surfaces with a fallback:

  1. GET /v1/executions/{id}/state  -- live engine state (Hub debug panel uses this)
     Response: {state: {data_flow: {task_outputs: {<alias>: ...}, task_states: {<alias>: ...}}}}
     Often 404 for completed sync v2 executions where the engine didn't persist state.

  2. GET /v1/executions/{id}/output -- durable post-execution record (fallback)
     Response: {customOutput: {<task_type>_result: [...], ...}, output, ...}
     v2 declarative tasks write results to customOutput, keyed by task TYPE
     (altdata_enrichment_result, scorecard_result, rule_tree_result, mapping_table_result,
     evaluate_rules_result, conditional_result, ...).

When the fallback is used, the response wraps customOutput and includes
'_source: "output_customOutput"' so you can tell which surface answered.

Common drilldowns:
  # state surface (Hub-style, by task alias)
  altscore executions state <id> | jq '.state.data_flow.task_outputs'
  altscore executions state <id> | jq '.state.data_flow.task_states | to_entries | map({k:.key, status:.value.status})'

  # customOutput fallback (by task type, arrays)
  altscore executions state <id> | jq '.customOutput'
  altscore executions state <id> | jq '.customOutput.scorecard_result[0]'`,
		Example: `  altscore executions state <execution-id>
  altscore executions state <execution-id> --no-fallback   # strict /state, no fallback`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			data, status, err := c.Do("GET", "borrower_central", fmt.Sprintf("/v1/executions/%s/state", args[0]), nil)
			if err == nil && status < 400 {
				return output.RawJSON(data)
			}
			if noFallback || status != 404 {
				return err
			}
			fmt.Fprintln(cmd.OutOrStderr(), "# /state returned 404; falling back to /output customOutput (v2 declarative tasks write results here, keyed by task type)")
			outData, _, oerr := c.Do("GET", "borrower_central", fmt.Sprintf("/v1/executions/%s/output", args[0]), nil)
			if oerr != nil {
				return fmt.Errorf("/state 404 and /output fallback failed: %w", oerr)
			}
			var outResp map[string]any
			if err := json.Unmarshal(outData, &outResp); err != nil {
				return fmt.Errorf("/output returned malformed JSON: %w", err)
			}
			// Surface failure context that lives on /output but not /state. An
			// agent that sees isSuccess=false in `executions state` should
			// immediately see WHY -- the runtime puts the human-readable
			// failure on errorMessage / notices, not on customOutput.
			synthetic := map[string]any{
				"_source":      "output_customOutput",
				"executionId":  outResp["id"],
				"customOutput": outResp["customOutput"],
				"output":       outResp["output"],
				"isSuccess":    outResp["isSuccess"],
				"status":       outResp["status"],
				"statusCode":   outResp["statusCode"],
				"errorMessage": outResp["errorMessage"],
				"notes":        outResp["notes"],
				"notices":      outResp["notices"],
			}
			body, err := json.Marshal(synthetic)
			if err != nil {
				return err
			}
			return output.RawJSON(body)
		},
	}
	cmd.Flags().BoolVar(&noFallback, "no-fallback", false, "fail on 404 instead of falling back to /output customOutput")
	return cmd
}

// Execution decisions surface /v1/executions/{id}/decisions, where workflows
// record their final / preliminary outcomes. Decision keys must already be
// registered in the tenant's decision data-models (see 'altscore decisions
// list'); the API returns 400 with "key not found for entity type: decision"
// otherwise. Three thin commands: get to read what an execution decided, set
// to write a decision (or override one), delete to clear it.

func makeExGetDecisionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get-decision <execution-id>",
		Short: "Get the decision an execution recorded",
		Long: `GET /v1/executions/{id}/decisions. Returns the latest decision the workflow
recorded for this execution, including its history.

Response fields:
  id, executionId, key, label, decisionType ("preliminary"|"final"),
  history (audit trail), createdAt, updatedAt`,
		Example: `  altscore executions get-decision <execution-id>
  altscore executions get-decision <execution-id> | jq '.key'`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/v1/executions/%s/decisions", args[0])
			data, _, err := c.Do("GET", "borrower_central", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}

func makeExSetDecisionCmd() *cobra.Command {
	var key string
	var decisionType string
	var label string
	var bodyFlag string

	cmd := &cobra.Command{
		Use:   "set-decision <execution-id>",
		Short: "Record (or override) the decision for an execution",
		Long: `POST /v1/executions/{id}/decisions.

The 'key' must match a decision data-model registered for the tenant -- run
'altscore decisions list' to see the valid set, or 'altscore decisions create
--key <k> --label <l>' to register a new one. Submitting an unregistered key
returns HTTP 400 "key not found for entity type: decision".

decisionType is either "preliminary" (interim, can be overridden) or "final"
(default; usually the rule-tree task's outputVariable). The endpoint records
each write into the decision's history rather than overwriting blindly.`,
		Example: `  # Record a final decision
  altscore executions set-decision <id> --key approve

  # Preliminary decision with a custom label
  altscore executions set-decision <id> --key manual-review --type preliminary --label "Pending document upload"

  # From a body (e.g. when piping from another tool)
  altscore executions set-decision <id> --body '{"key":"reject","decisionType":"final"}'`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}

			body := json.RawMessage("{}")
			if bodyFlag != "" {
				rb, err := readBody(bodyFlag)
				if err != nil {
					return err
				}
				body = rb
			}
			if key != "" {
				body, err = jsonForceSetString(body, "key", key)
				if err != nil {
					return err
				}
			}
			// decisionType is required by the API; default to "final" when neither
			// --type nor a body field provides it. Most rule-tree task outcomes are
			// final; "preliminary" is the rarer path (interim decisions that get
			// overwritten later in the run).
			if decisionType != "" {
				switch decisionType {
				case "preliminary", "final":
					body, err = jsonForceSetString(body, "decisionType", decisionType)
					if err != nil {
						return err
					}
				default:
					return fmt.Errorf("--type must be 'preliminary' or 'final', got %q", decisionType)
				}
			} else if !bodyHasKey(body, "decisionType") {
				body, err = jsonForceSetString(body, "decisionType", "final")
				if err != nil {
					return err
				}
			}
			if label != "" {
				body, err = jsonForceSetString(body, "label", label)
				if err != nil {
					return err
				}
			}
			if !bodyHasKey(body, "key") {
				return fmt.Errorf("set-decision requires --key (or a 'key' field in --body)")
			}

			path := fmt.Sprintf("/v1/executions/%s/decisions", args[0])
			data, _, err := c.Do("POST", "borrower_central", path, body)
			if err != nil {
				return err
			}
			// The POST endpoint returns an empty body on success. Read the
			// decision back so the caller can pipe the result into jq without
			// a follow-up GET (and without making the cmd silently produce
			// nothing on stdout, which had previously broken automation).
			if len(data) == 0 {
				readBack, _, gerr := c.Do("GET", "borrower_central", path, nil)
				if gerr == nil && len(readBack) > 0 {
					return output.RawJSON(readBack)
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "Decision recorded. (POST returned empty body; follow-up GET %s%s)\n",
					path, errSuffix(gerr))
				return nil
			}
			return output.RawJSON(data)
		},
	}
	cmd.Flags().StringVar(&key, "key", "", "decision key (must be a registered decision data-model)")
	cmd.Flags().StringVar(&decisionType, "type", "", "decisionType: preliminary | final (default: final)")
	cmd.Flags().StringVar(&label, "label", "", "optional human-readable label for this decision")
	cmd.Flags().StringVar(&bodyFlag, "body", "", "JSON body (overridden by --key / --type / --label when set)")
	return cmd
}

// errSuffix returns " (read-back failed: <err>)" when err is non-nil, else "".
// Used by commands that POST then GET so the user sees why the read-back fell
// through without obscuring the primary success message.
func errSuffix(err error) string {
	if err == nil {
		return ""
	}
	return " (read-back failed: " + err.Error() + ")"
}

func makeExDeleteDecisionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete-decision <execution-id>",
		Short: "Delete the decision an execution recorded (admin only)",
		Long: `DELETE /v1/executions/{id}/decisions.

Requires the bc.private.delete scope. Use sparingly: this clears the recorded
decision but does NOT replay the workflow.`,
		Example: `  altscore executions delete-decision <execution-id>`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/v1/executions/%s/decisions", args[0])
			_, status, err := c.Do("DELETE", "borrower_central", path, nil)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStderr(), "Decision deleted (HTTP %d).\n", status)
			return nil
		},
	}
}

func makeExGetOutputAttachmentsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get-output-attachments <execution-id>",
		Short: "Get attachments from an execution output",
		Long: `Retrieve the file attachments associated with an execution's output.

Returns a JSON array of attachment records, each containing a download URL.

Response fields (per attachment):
  id, url, label, fileExtension, metadata, createdAt`,
		Example: `  altscore executions get-output-attachments <execution-id>
  altscore executions get-output-attachments <execution-id> | jq '.[].url'`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}

			path := fmt.Sprintf("/v1/executions/%s/output/attachments", args[0])
			data, _, err := c.Do("GET", "borrower_central", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}
