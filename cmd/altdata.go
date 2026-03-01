package cmd

import (
	"fmt"
	"strings"

	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

func init() {
	altdataCmd := &cobra.Command{
		Use:   "altdata",
		Short: "AltData source discovery and data requests",
		Long: `Interact with AltData data sources.

Discovery commands (sources, dictionary, search, sample) query the Borrower
Central module and work in all environments.

Execution commands (request-sync, request-async, request-status, request-collect)
hit the AltData module directly and are only available in production.`,
	}

	altdataCmd.AddCommand(makeAltdataSourcesCmd())
	altdataCmd.AddCommand(makeAltdataDictionaryCmd())
	altdataCmd.AddCommand(makeAltdataSearchCmd())
	altdataCmd.AddCommand(makeAltdataSampleCmd())
	altdataCmd.AddCommand(makeAltdataRequestSyncCmd())
	altdataCmd.AddCommand(makeAltdataRequestAsyncCmd())
	altdataCmd.AddCommand(makeAltdataRequestStatusCmd())
	altdataCmd.AddCommand(makeAltdataRequestCollectCmd())

	rootCmd.AddCommand(altdataCmd)
}

func makeAltdataSourcesCmd() *cobra.Command {
	var filters []string
	var perPage int
	var page int
	var sortBy string
	var sortDirection string

	cmd := &cobra.Command{
		Use:   "sources",
		Short: "List available data sources",
		Long: `List available data sources with status and metadata.

Uses the Borrower Central module -- works in all environments.

Available filters (pass via --filter key=value):
  status                Source status (e.g. "active")
  country               Country code (e.g. "USA")
  search                Free-text search across name/description

Response fields:
  sourceId, sourceVersion, status, timeout, inputFields, enabled,
  name, description, stats, outputSchema`,
		Example: `  # List first 5 sources
  altscore altdata sources --per-page 5

  # Filter by country
  altscore altdata sources --filter country=USA --per-page 10

  # Search by keyword
  altscore altdata sources --filter search=credit

  # Sort results
  altscore altdata sources --sort-by name --sort-direction asc`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}

			params := []string{}
			if perPage > 0 {
				params = append(params, fmt.Sprintf("per-page=%d", perPage))
			}
			if page > 0 {
				params = append(params, fmt.Sprintf("page=%d", page))
			}
			if sortBy != "" {
				params = append(params, fmt.Sprintf("sort-by=%s", sortBy))
			}
			if sortDirection != "" {
				params = append(params, fmt.Sprintf("sort-direction=%s", sortDirection))
			}
			for _, f := range filters {
				params = append(params, f)
			}

			path := "/v1/workflows-v2/sources-status"
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
	cmd.Flags().IntVar(&perPage, "per-page", 0, "items per page")
	cmd.Flags().IntVar(&page, "page", 0, "page number")
	cmd.Flags().StringVar(&sortBy, "sort-by", "", "field to sort by")
	cmd.Flags().StringVar(&sortDirection, "sort-direction", "", "sort direction (asc or desc)")

	return cmd
}

func makeAltdataDictionaryCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "dictionary <source-id> <version>",
		Short: "Get field definitions for a data source",
		Long: `Get the data dictionary (field definitions) for a specific source and version.

Uses the Borrower Central module -- works in all environments.

Response fields:
  sourceId, version, field, dataType, country,
  descriptions{en, es}`,
		Example: `  altscore altdata dictionary USA-PUB-0001 v1
  altscore altdata dictionary USA-PUB-0001 v1 | jq '.[].field'`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}

			path := fmt.Sprintf("/v1/documentation/data-dictionary?source_id=%s&version=%s", args[0], args[1])

			data, _, err := c.Do("GET", "borrower_central", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}

func makeAltdataSearchCmd() *cobra.Command {
	var locale string

	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Search field definitions across all sources",
		Long: `Search data dictionary field definitions across all sources.

Uses the Borrower Central module -- works in all environments.

Response fields:
  sourceId, version, field, dataType, country,
  descriptions{en, es}`,
		Example: `  altscore altdata search "credit score"
  altscore altdata search "address" --locale es`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}

			path := fmt.Sprintf("/v1/documentation/data-dictionary/search?locale=%s&query=%s", locale, args[0])

			data, _, err := c.Do("GET", "borrower_central", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	cmd.Flags().StringVar(&locale, "locale", "en", "search locale (en or es)")

	return cmd
}

func makeAltdataSampleCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sample <source-id> <version>",
		Short: "Get sample output for a data source",
		Long: `Get sample/example output for a specific source and version.

Uses the Borrower Central module -- works in all environments.

Response fields:
  sourceId, version, data (JSON object with example output)`,
		Example: `  altscore altdata sample USA-PUB-0001 v1
  altscore altdata sample USA-PUB-0001 v1 | jq '.data'`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}

			path := fmt.Sprintf("/v1/documentation/output-example?source_id=%s&version=%s", args[0], args[1])

			data, _, err := c.Do("GET", "borrower_central", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}

func makeAltdataRequestSyncCmd() *cobra.Command {
	var bodyFlag string

	cmd := &cobra.Command{
		Use:   "request-sync",
		Short: "Execute a synchronous data request",
		Long: `Execute a synchronous data request. Blocks until complete.

Uses the AltData module -- only available in production.

Request body fields:
  personId: string            [required] Identifier for the person/entity
  sourcesConfig: [object]     [required] Sources to query
    sourceId: string          Source ID (e.g. "USA-PUB-0001")
    version: string           Source version (e.g. "v1")
  dateToAnalyze: string       ISO 8601 date (optional)
  timeout: int                Seconds (default: 60)

Response fields:
  requestId, requestedAt, callSummary, data, sourceData, inputs`,
		Example: `  # Inline body
  altscore altdata request-sync --body '{
    "personId": "borrower-123",
    "sourcesConfig": [{"sourceId": "USA-PUB-0001", "version": "v1"}]
  }'

  # From file
  altscore altdata request-sync --body "$(cat request.json)"

  # From stdin
  cat request.json | altscore altdata request-sync`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}

			body, err := readBody(bodyFlag)
			if err != nil {
				return err
			}

			data, _, err := c.Do("POST", "altdata", "/v1/requests/sync", body)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	cmd.Flags().StringVar(&bodyFlag, "body", "", "JSON request body (or pipe via stdin)")

	return cmd
}

func makeAltdataRequestAsyncCmd() *cobra.Command {
	var bodyFlag string

	cmd := &cobra.Command{
		Use:   "request-async",
		Short: "Execute an asynchronous data request",
		Long: `Execute an asynchronous data request. Returns a request ID immediately.

Uses the AltData module -- only available in production.

Request body fields:
  personId: string            [required] Identifier for the person/entity
  sourcesConfig: [object]     [required] Sources to query
    sourceId: string          Source ID (e.g. "USA-PUB-0001")
    version: string           Source version (e.g. "v1")
  dateToAnalyze: string       ISO 8601 date (optional)
  timeout: int                Seconds (default: 60)

Response fields:
  requestId`,
		Example: `  altscore altdata request-async --body '{
    "personId": "borrower-123",
    "sourcesConfig": [{"sourceId": "USA-PUB-0001", "version": "v1"}]
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

			data, _, err := c.Do("POST", "altdata", "/v1/requests/async", body)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	cmd.Flags().StringVar(&bodyFlag, "body", "", "JSON request body (or pipe via stdin)")

	return cmd
}

func makeAltdataRequestStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "request-status <request-id>",
		Short: "Check status of an async data request",
		Long: `Check the status of an asynchronous data request.

Uses the AltData module -- only available in production.`,
		Example: `  altscore altdata request-status abc-123-def`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}

			data, _, err := c.Do("GET", "altdata", "/v1/requests/"+args[0]+"/status", nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}

func makeAltdataRequestCollectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "request-collect <request-id>",
		Short: "Collect data from a completed request",
		Long: `Collect the data from a completed asynchronous data request.

Uses the AltData module -- only available in production.`,
		Example: `  altscore altdata request-collect abc-123-def
  altscore altdata request-collect abc-123-def | jq '.data'`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}

			data, _, err := c.Do("GET", "altdata", "/v1/requests/"+args[0], nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}
