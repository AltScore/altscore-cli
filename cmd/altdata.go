package cmd

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/AltScore/altscore-cli/internal/client"
	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

// defaultAltdataPerPage is the default page size for altdata catalog listings.
// The catalog has ~170 sources today; a single call returns everything.
const defaultAltdataPerPage = 200

func init() {
	altdataCmd := &cobra.Command{
		Use:   "altdata",
		Short: "AltData source discovery and data requests",
		Long: `Interact with AltData data sources.

Discovery commands (sources, describe, dictionary, search, sample) query the
Borrower Central module and work in all environments. For agents doing
pre-flight on a specific source, 'altscore altdata describe <id>' is the
canonical one-shot primitive.

Execution commands (request-sync, request-async, request-status, request-collect)
hit the AltData module directly and are only available in production.`,
	}

	altdataCmd.AddCommand(makeAltdataSourcesCmd())
	altdataCmd.AddCommand(makeAltdataDescribeCmd())
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
		Long: fmt.Sprintf(`List available data sources with status and metadata.

Uses the Borrower Central module -- works in all environments.

By default returns up to %d entries (the full catalog today is ~170 sources).
Use --per-page to override. For drilling into a single source, prefer
'altscore altdata describe <sourceId>' which is a one-shot pre-flight.

Available filters (pass via --filter key=value):
  status                Source status (e.g. "active")
  country               Country code (e.g. "USA")
  search                Free-text search across name/description

Response fields:
  sourceId, sourceVersion, status, timeout, inputFields, enabled,
  name, description, stats, outputSchema`, defaultAltdataPerPage),
		Example: `  # List the full catalog (default per-page is 200)
  altscore altdata sources

  # Filter by country
  altscore altdata sources --filter country=USA

  # Search by keyword
  altscore altdata sources --filter search=credit

  # Sort results
  altscore altdata sources --sort-by name --sort-direction asc`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}

			effectivePerPage := perPage
			if effectivePerPage <= 0 {
				effectivePerPage = defaultAltdataPerPage
			}
			params := []string{fmt.Sprintf("per-page=%d", effectivePerPage)}
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

			path := "/v2/workflows/sources-status"
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
	cmd.Flags().IntVar(&perPage, "per-page", 0, fmt.Sprintf("items per page (default %d)", defaultAltdataPerPage))
	cmd.Flags().IntVar(&page, "page", 0, "page number")
	cmd.Flags().StringVar(&sortBy, "sort-by", "", "field to sort by")
	cmd.Flags().StringVar(&sortDirection, "sort-direction", "", "sort direction (asc or desc)")

	return cmd
}

func makeAltdataDictionaryCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "dictionary <source-id> [version]",
		Short: "Get field definitions for a data source",
		Long: `Get the data dictionary (field definitions) for a specific source.

If <version> is omitted, the latest enabled version is auto-resolved via
sources-status.

Uses the Borrower Central module -- works in all environments.

Response fields:
  sourceId, version, field, dataType, country,
  descriptions{en, es}`,
		Example: `  # Auto-resolve latest version
  altscore altdata dictionary USA-PUB-0001

  # Pin a specific version
  altscore altdata dictionary USA-PUB-0001 v1 | jq '.[].field'`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}

			sourceID := args[0]
			version := ""
			if len(args) == 2 {
				version = args[1]
			} else {
				resolved, rerr := resolveLatestSourceVersion(c, sourceID)
				if rerr != nil {
					return rerr
				}
				version = resolved
			}

			path := fmt.Sprintf("/v1/documentation/data-dictionary?source_id=%s&version=%s", sourceID, version)

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
		Use:   "sample <source-id> [version]",
		Short: "Get sample output for a data source",
		Long: `Get sample/example output for a specific source.

If <version> is omitted, the latest enabled version is auto-resolved via
sources-status.

Uses the Borrower Central module -- works in all environments.

Response fields:
  sourceId, version, data (JSON object with example output)`,
		Example: `  altscore altdata sample USA-PUB-0001
  altscore altdata sample USA-PUB-0001 v1 | jq '.data'`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}

			sourceID := args[0]
			version := ""
			if len(args) == 2 {
				version = args[1]
			} else {
				resolved, rerr := resolveLatestSourceVersion(c, sourceID)
				if rerr != nil {
					return rerr
				}
				version = resolved
			}

			path := fmt.Sprintf("/v1/documentation/output-example?source_id=%s&version=%s", sourceID, version)

			data, _, err := c.Do("GET", "borrower_central", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}

// makeAltdataDescribeCmd builds the canonical one-shot discovery primitive for
// AltData sources. It bundles the metadata, all enabled versions, the input
// fields, and a preview of the outputSchema for the latest version into a
// single JSON document so an agent doing pre-flight on a source does not need
// to chain sources-status + dictionary + sample.
func makeAltdataDescribeCmd() *cobra.Command {
	var version string
	cmd := &cobra.Command{
		Use:   "describe <source-id>",
		Short: "One-shot pre-flight summary for a data source",
		Long: `Pre-flight summary for a single AltData source.

Bundles metadata, available versions, required input fields, and the top-level
outputSchema keys into one JSON document. Use this as the canonical first stop
before composing a workflow that uses the source -- it answers "what versions
exist?", "what does this source need?", and "what does it return?" in one call.

If --version is omitted, the latest enabled version is auto-resolved via
sources-status.

Uses the Borrower Central module -- works in all environments.

Response shape:
  {
    "sourceId":     "USA-PUB-0001",
    "name":         "...",
    "description":  {"en": "...", "es": "..."},
    "country":      "USA",
    "status":       "active",
    "timeout":      60,
    "versions":     ["v1", "v2"],
    "latestVersion":"v2",
    "selectedVersion":"v2",
    "inputFields":  [{"field": "personId", "required": true, ...}],
    "outputKeys":   ["score", "tradeLines", ...],
    "stats":        {...},
    "next": {
      "dictionary": "altscore altdata dictionary USA-PUB-0001 v2",
      "sample":     "altscore altdata sample USA-PUB-0001 v2"
    }
  }`,
		Example: `  # Pre-flight on a single source
  altscore altdata describe USA-PUB-0001

  # Pin a specific version
  altscore altdata describe USA-PUB-0001 --version v1

  # Pipe straight into jq
  altscore altdata describe USA-PUB-0001 | jq '{inputFields, outputKeys}'`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			sourceID := args[0]

			entries, err := fetchSourceEntries(c, sourceID)
			if err != nil {
				return err
			}
			if len(entries) == 0 {
				return fmt.Errorf("source %q not found in catalog", sourceID)
			}

			versions := make([]string, 0, len(entries))
			for _, e := range entries {
				if v, _ := e["sourceVersion"].(string); v != "" {
					versions = append(versions, v)
				}
			}
			latest := pickLatestVersion(versions)

			selected := version
			if selected == "" {
				selected = latest
			}
			var picked map[string]any
			for _, e := range entries {
				if v, _ := e["sourceVersion"].(string); v == selected {
					picked = e
					break
				}
			}
			if picked == nil {
				return fmt.Errorf("source %q has no version %q (available: %s)", sourceID, selected, strings.Join(versions, ", "))
			}

			outputKeys := []string{}
			if outSchema, ok := picked["outputSchema"].(map[string]any); ok {
				if entry, ok := outSchema[sourceID].(map[string]any); ok {
					if props, ok := entry["properties"].(map[string]any); ok {
						for k := range props {
							outputKeys = append(outputKeys, k)
						}
					} else {
						for k := range entry {
							if k == "type" || k == "title" || k == "description" {
								continue
							}
							outputKeys = append(outputKeys, k)
						}
					}
				}
			}
			sort.Strings(outputKeys)

			summary := map[string]any{
				"sourceId":        sourceID,
				"name":            picked["name"],
				"description":     picked["description"],
				"country":         picked["country"],
				"status":          picked["status"],
				"timeout":         picked["timeout"],
				"versions":        dedupeSorted(versions),
				"latestVersion":   latest,
				"selectedVersion": selected,
				"inputFields":     picked["inputFields"],
				"outputKeys":      outputKeys,
				"stats":           picked["stats"],
				"next": map[string]any{
					"dictionary": fmt.Sprintf("altscore altdata dictionary %s %s", sourceID, selected),
					"sample":     fmt.Sprintf("altscore altdata sample %s %s", sourceID, selected),
				},
			}
			out, err := json.Marshal(summary)
			if err != nil {
				return err
			}
			return output.RawJSON(out)
		},
	}

	cmd.Flags().StringVar(&version, "version", "", "pin a specific version (default: latest)")
	return cmd
}

// fetchSourceEntries returns every sources-status entry whose sourceId matches.
// Used by describe and resolveLatestSourceVersion.
func fetchSourceEntries(c *client.Client, sourceID string) ([]map[string]any, error) {
	q := url.Values{}
	q.Set("per-page", strconv.Itoa(defaultAltdataPerPage))
	q.Set("filter", "sourceId="+sourceID)
	path := "/v2/workflows/sources-status?" + q.Encode()
	data, _, err := c.Do("GET", "borrower_central", path, nil)
	if err != nil {
		return nil, err
	}
	var sources []map[string]any
	if err := json.Unmarshal(data, &sources); err != nil {
		return nil, fmt.Errorf("parse sources-status: %w", err)
	}
	out := sources[:0]
	for _, s := range sources {
		if sid, _ := s["sourceId"].(string); sid == sourceID {
			out = append(out, s)
		}
	}
	return out, nil
}

// resolveLatestSourceVersion picks the highest "v<N>" version among enabled
// entries for sourceID. Falls back to any version if none look like "v<N>".
func resolveLatestSourceVersion(c *client.Client, sourceID string) (string, error) {
	entries, err := fetchSourceEntries(c, sourceID)
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "", fmt.Errorf("source %q not found; can't auto-resolve version (try 'altscore altdata sources --filter search=%s')", sourceID, sourceID)
	}
	versions := make([]string, 0, len(entries))
	for _, e := range entries {
		enabled, _ := e["enabled"].(bool)
		v, _ := e["sourceVersion"].(string)
		if v != "" && enabled {
			versions = append(versions, v)
		}
	}
	if len(versions) == 0 {
		// fall back to any version; user may be on a tenant where nothing is enabled yet
		for _, e := range entries {
			if v, _ := e["sourceVersion"].(string); v != "" {
				versions = append(versions, v)
			}
		}
	}
	if len(versions) == 0 {
		return "", fmt.Errorf("source %q has no versions in catalog", sourceID)
	}
	return pickLatestVersion(versions), nil
}

// pickLatestVersion returns the version with the highest numeric suffix in
// "v<N>" form. Falls back to the lexicographically last one if no entries
// match the "v<N>" pattern.
func pickLatestVersion(versions []string) string {
	if len(versions) == 0 {
		return ""
	}
	bestIdx := -1
	bestNum := -1
	for i, v := range versions {
		if !strings.HasPrefix(v, "v") {
			continue
		}
		n, err := strconv.Atoi(v[1:])
		if err != nil {
			continue
		}
		if n > bestNum {
			bestNum = n
			bestIdx = i
		}
	}
	if bestIdx >= 0 {
		return versions[bestIdx]
	}
	sorted := append([]string{}, versions...)
	sort.Strings(sorted)
	return sorted[len(sorted)-1]
}

// dedupeSorted returns a sorted, deduplicated copy of versions.
func dedupeSorted(versions []string) []string {
	seen := make(map[string]struct{}, len(versions))
	out := make([]string, 0, len(versions))
	for _, v := range versions {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
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
