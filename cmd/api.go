package cmd

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

var (
	apiBody   string
	apiModule string
)

var apiCmd = &cobra.Command{
	Use:   "api <METHOD> <path>",
	Short: "Raw API passthrough",
	Long: `Execute a raw HTTP request against the AltScore API.

This is an escape hatch for accessing any API endpoint, including ones
not covered by the built-in resource commands. The path is appended to
the base URL for the selected module.

The module determines which base URL is used:
  borrower_central  (default) Borrower Central API
  cms               CMS API
  altdata           AltData API

METHOD must be an HTTP method: GET, POST, PUT, PATCH, DELETE.`,
	Example: `  # GET request
  altscore api GET /v1/borrowers?per-page=1

  # POST with body
  altscore api POST /v1/borrowers --body '{"label": "Test"}'

  # Use a different module
  altscore api GET /v1/content --module cms

  # PATCH request
  altscore api PATCH /v1/borrowers/<id> --body '{"status": "active"}'`,
	Args: cobra.ExactArgs(2),
	RunE: runAPI,
}

func init() {
	apiCmd.Flags().StringVar(&apiBody, "body", "", "JSON request body")
	apiCmd.Flags().StringVar(&apiModule, "module", "borrower_central", "API module (borrower_central, cms, altdata)")
	rootCmd.AddCommand(apiCmd)
}

func runAPI(cmd *cobra.Command, args []string) error {
	method := strings.ToUpper(args[0])
	path := args[1]

	c, err := loadClient()
	if err != nil {
		return err
	}

	var body any
	if apiBody != "" {
		var raw json.RawMessage
		if err := json.Unmarshal([]byte(apiBody), &raw); err != nil {
			return fmt.Errorf("invalid JSON in --body: %w", err)
		}
		body = raw
	}

	data, status, err := c.Do(method, apiModule, path, body)
	if err != nil {
		return err
	}

	if data != nil {
		return output.RawJSON(data)
	}

	fmt.Fprintf(cmd.ErrOrStderr(), "HTTP %d (empty body)\n", status)
	return nil
}
