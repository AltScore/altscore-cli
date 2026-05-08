package cmd

import (
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

METHOD must be an HTTP method: GET, POST, PUT, PATCH, DELETE.

The body can be supplied three ways, mirroring the resource subcommands:
  - inline: --body '{"key":"value"}'
  - from file: --body @path/to/body.json
  - from stdin: omit --body and pipe JSON in (only when stdin isn't a tty)`,
	Example: `  # GET request
  altscore api GET /v1/borrowers?per-page=1

  # POST with inline body
  altscore api POST /v1/borrowers --body '{"label": "Test"}'

  # POST with body from file
  altscore api POST /v2/tasks/end-abc123 --body @end-task.json

  # POST with body from stdin
  echo '{"label":"Test"}' | altscore api POST /v1/borrowers

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

	// Read body via the shared resource-command path: handles inline JSON,
	// `@filename`, and stdin uniformly. GET/DELETE are body-less so we
	// silence the "no JSON body provided" error for those methods.
	var body any
	if apiBody != "" || !apiStdinIsTerminal() {
		raw, rerr := readBody(apiBody)
		if rerr != nil {
			if method == "GET" || method == "DELETE" || method == "HEAD" {
				// Treat missing body as no-op for body-less methods.
				body = nil
			} else {
				return rerr
			}
		} else {
			body = raw
		}
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

// apiStdinIsTerminal reports whether stdin is a tty. When piping JSON in,
// it's not -- and we should attempt to read it. When the user is typing,
// it is, and we shouldn't block.
func apiStdinIsTerminal() bool {
	return isStdinTerminal()
}
