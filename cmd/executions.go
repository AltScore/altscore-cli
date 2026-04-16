package cmd

import (
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
