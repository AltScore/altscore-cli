package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/AltScore/altscore-cli/internal/client"
	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

// defaultAltdataPerPage is the page size of 'altdata sources'. The catalog outgrew one
// page, so lookups by id walk every page through fetchSourceCatalog instead.
const defaultAltdataPerPage = 200

func init() {
	altdataCmd := &cobra.Command{
		Use:   "altdata",
		Short: "AltData source discovery and data requests",
		Long: `Interact with AltData data sources.

Discovery commands (sources, describe, dictionary, search) query the
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
	var full bool
	var ignoredSort string

	cmd := &cobra.Command{
		Use:   "sources",
		Short: "List every data source version, one compact row each",
		Long: `List the AltData source catalog (the same rows as 'workflows-v2 sources-status').

Uses the Borrower Central module -- works in all environments.

` + sourceListingHelp + `

Available filters (pass via --filter key=value):
  status                Source status (e.g. "active")
  country               Country of the sourceId (e.g. "ECU"); never returns INT sources
  search                Free-text search across sourceId, version and name`,
		Example: `  # Every source version, compact
  altscore altdata sources
  altscore altdata sources --filter search=credit
  altscore altdata sources --filter country=USA | jq -r '.[] | "\(.sourceId) \(.version) \(.name)"'

  # Then ONE source's fields
  altscore altdata describe <sourceId>

  # Raw rows with outputSchema (hundreds of KB)
  altscore altdata sources --full --filter search=credit`,
		RunE: func(cmd *cobra.Command, args []string) error {
			q, err := sourceFilterValues(filters, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			c, err := loadClient()
			if err != nil {
				return err
			}
			return sourceListing{filters: q, page: page, perPage: perPage, full: full}.run(c, cmd.ErrOrStderr())
		},
	}

	cmd.Flags().StringArrayVar(&filters, "filter", nil, "country, status or search, as key=value (repeatable)")
	cmd.Flags().IntVar(&perPage, "per-page", 0, fmt.Sprintf("page size of the walk, or of --page (default %d)", defaultAltdataPerPage))
	cmd.Flags().IntVar(&page, "page", 0, "read this page only instead of every page")
	cmd.Flags().BoolVar(&full, "full", false, "print the raw rows, outputSchema and stats included")
	for _, name := range []string{"sort-by", "sort-direction"} {
		cmd.Flags().StringVar(&ignoredSort, name, "", "")
		_ = cmd.Flags().MarkDeprecated(name, "sources-status does not sort; rows come in catalog order")
	}

	return cmd
}

const sourceListingHelp = `By default every page is read and each source version is one compact row:
  sourceId, version, name, status, enabled, country and groupId (when set),
  requiredInputs (input field names grouped by requirement: REQUIRED,
  AT_LEAST_ONE, EXCLUSIVE_ONE, REQUIRED_IF_PARENT; optional ones left out)
stderr says how many rows printed out of how many exist, and what a country
or status filter hid. A country filter never returns INT (international)
sources such as sanctions lists, and status=active also hides every down,
failing or retired version.

For ONE source's fields, use 'altscore altdata describe <sourceId>' (inputFields
and top-level outputKeys) and 'altscore altdata dictionary <sourceId>' (every
output field with its type and description).

--full prints the raw rows instead (outputSchema, stats, credentialsSchema):
hundreds of KB per page. --page reads one page only.`

// sourceListing is the one implementation behind 'altdata sources' and
// 'workflows-v2 sources-status', which read the same endpoint.
type sourceListing struct {
	filters url.Values
	page    int
	perPage int
	full    bool
}

func (l sourceListing) run(c *client.Client, stderr io.Writer) error {
	q := url.Values{}
	for k, v := range l.filters {
		q[k] = append([]string(nil), v...)
	}
	perPage := l.perPage
	if perPage <= 0 {
		perPage = defaultAltdataPerPage
	}
	q.Set("per-page", strconv.Itoa(perPage))
	if l.page > 0 {
		q.Set("page", strconv.Itoa(l.page))
		data, _, err := c.Do("GET", "borrower_central", sourcesStatusPath(q), nil)
		if err != nil {
			return err
		}
		var rows []map[string]any
		if err := json.Unmarshal(data, &rows); err != nil {
			return fmt.Errorf("parse sources-status: %w", err)
		}
		fmt.Fprintf(stderr, "# %d sources on page %d; omit --page to read every page\n", len(rows), l.page)
		if l.full {
			return output.RawJSON(data)
		}
		return printCompactRows(compactSourceRows(rows))
	}

	rows, err := fetchSourceCatalog(c, sourcesStatusPath(q))
	if err != nil {
		return err
	}
	total, notes := len(rows), []string(nil)
	if q.Has("country") || q.Has("status") {
		unfiltered := url.Values{}
		for k, v := range q {
			if k != "country" && k != "status" {
				unfiltered[k] = v
			}
		}
		if all, err := fetchSourceCatalog(c, sourcesStatusPath(unfiltered)); err != nil {
			notes = append(notes, fmt.Sprintf("could not count the rows the country/status filter hid: %v", err))
		} else {
			total, notes = len(all), hiddenSourceNotes(q, rows, all)
		}
	}
	shape := "compact rows; --full adds outputSchema"
	if l.full {
		shape = "raw rows"
	}
	fmt.Fprintf(stderr, "# %d of %d sources, every page read (%s)\n", len(rows), total, shape)
	for _, n := range notes {
		fmt.Fprintf(stderr, "# %s\n", n)
	}
	if l.full {
		return output.JSON(rows)
	}
	return printCompactRows(compactSourceRows(rows))
}

func sourcesStatusPath(q url.Values) string {
	return "/v2/workflows/sources-status?" + q.Encode()
}

// The backend ignores a key it does not know, which used to answer "--filter sourceId=X"
// with the whole catalog as if it had matched.
func sourceFilterValues(filters []string, stderr io.Writer) (url.Values, error) {
	q := url.Values{}
	for _, f := range filters {
		key, value, ok := strings.Cut(f, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("--filter %q: expected key=value (country, status or search)", f)
		}
		switch key {
		case "country", "status", "search":
		default:
			fmt.Fprintf(stderr, "# warning: sources-status has no %q filter (only country, status, search) and ignores it; for one source run 'altscore altdata describe <sourceId>'\n", key)
		}
		q.Add(key, value)
	}
	return q, nil
}

// hiddenSourceNotes describes what the country and status filters removed from the unfiltered
// rows, by status and by sourceId prefix: a down version or an INT (international) source that
// a filter hid used to be silently missing. It describes the hidden set and does not guess which
// filter removed a row.
func hiddenSourceNotes(q url.Values, shown, all []map[string]any) []string {
	kept := make(map[string]bool, len(shown))
	for _, row := range shown {
		kept[sourceRowKey(row)] = true
	}
	hidden := 0
	byStatus, byPrefix := map[string]int{}, map[string]int{}
	for _, row := range all {
		if kept[sourceRowKey(row)] {
			continue
		}
		hidden++
		status, _ := row["status"].(string)
		if status == "" {
			status = "null"
		}
		byStatus[status]++
		id, _ := row["sourceId"].(string)
		prefix, _, _ := strings.Cut(id, "-")
		byPrefix[prefix]++
	}
	if hidden == 0 {
		return nil
	}
	var filters, parts []string
	for _, key := range []string{"country", "status"} {
		if q.Has(key) {
			filters = append(filters, key+"="+strings.Join(q[key], ","))
		}
	}
	if q.Has("status") {
		parts = append(parts, "by status "+countsByFrequency(byStatus))
	}
	if q.Has("country") {
		parts = append(parts, "by sourceId prefix "+countsByFrequency(byPrefix))
	}
	notes := []string{fmt.Sprintf("%s hid %d: %s", strings.Join(filters, " "), hidden, strings.Join(parts, "; "))}
	if n := byPrefix["INT"]; n > 0 && q.Has("country") {
		notes = append(notes, fmt.Sprintf("%d of them are INT (international) sources, which no country filter returns", n))
	}
	return notes
}

// "37 USA, 30 MEX, 1 COL": most frequent first.
func countsByFrequency(counts map[string]int) string {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if counts[keys[i]] != counts[keys[j]] {
			return counts[keys[i]] > counts[keys[j]]
		}
		return keys[i] < keys[j]
	})
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d %s", counts[k], k))
	}
	return strings.Join(parts, ", ")
}

type compactSourceRow struct {
	SourceID       string              `json:"sourceId"`
	Version        string              `json:"version"`
	Name           string              `json:"name"`
	Country        string              `json:"country,omitempty"`
	Status         string              `json:"status"`
	Enabled        bool                `json:"enabled"`
	GroupID        string              `json:"groupId,omitempty"`
	RequiredInputs map[string][]string `json:"requiredInputs"`
}

func compactSourceRows(rows []map[string]any) []compactSourceRow {
	out := make([]compactSourceRow, 0, len(rows))
	for _, row := range rows {
		compact := compactSourceRow{RequiredInputs: map[string][]string{}}
		compact.SourceID, _ = row["sourceId"].(string)
		compact.Version, _ = row["sourceVersion"].(string)
		compact.Name = localizedText(row["name"])
		compact.Country, _ = row["country"].(string)
		compact.Status, _ = row["status"].(string)
		compact.Enabled, _ = row["enabled"].(bool)
		compact.GroupID, _ = row["groupId"].(string)
		for _, f := range asSlice(row["inputFields"]) {
			fm, _ := f.(map[string]any)
			name, _ := fm["field"].(string)
			if name == "" || !altdataFieldRequired(fm) {
				continue
			}
			requirement := "REQUIRED"
			if s, ok := fm["required"].(string); ok && !strings.EqualFold(s, "true") {
				requirement = strings.ToUpper(s)
			}
			compact.RequiredInputs[requirement] = append(compact.RequiredInputs[requirement], name)
		}
		out = append(out, compact)
	}
	return out
}

// The catalog names a source as {en, es}; English first.
func localizedText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case map[string]any:
		if en, _ := t["en"].(string); en != "" {
			return en
		}
		es, _ := t["es"].(string)
		return es
	}
	return ""
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
			version := ""
			if len(args) == 2 {
				version = args[1]
			}
			data, err := fetchAltdataDictionary(c, args[0], version)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}

// An empty version resolves to the latest one in the catalog.
func fetchAltdataDictionary(c *client.Client, sourceID, version string) (json.RawMessage, error) {
	if version == "" {
		resolved, err := resolveLatestSourceVersion(c, sourceID)
		if err != nil {
			return nil, err
		}
		version = resolved
	}
	path := fmt.Sprintf("/v1/documentation/data-dictionary?sourceId=%s&version=%s", sourceID, version)
	data, _, err := c.Do("GET", "borrower_central", path, nil)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func makeAltdataSearchCmd() *cobra.Command {
	var country string

	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Search field definitions across all sources",
		Long: `Search data dictionary field definitions across all sources.

Uses the Borrower Central module -- works in all environments.

Response fields:
  sourceId, version, field, dataType, country,
  descriptions{en, es}`,
		Example: `  altscore altdata search "credit score"
  altscore altdata search "address" --country MX`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}

			// BC accepts query/country/sourceId/page/per-page -- there is no
			// `locale`, so the old flag was silently dropped. The query MUST be
			// escaped: a space makes Go emit a malformed request line and the
			// server 400s before the handler runs -- which broke this command's
			// own documented example, `altdata search "credit score"`.
			path := "/v1/documentation/data-dictionary/search?query=" + url.QueryEscape(args[0])
			if country != "" {
				path += "&country=" + url.QueryEscape(country)
			}

			data, _, err := c.Do("GET", "borrower_central", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	cmd.Flags().StringVar(&country, "country", "", "filter by country code (e.g. MX, EC)")

	return cmd
}

// Kept, hidden, so an old invocation gets the replacement instead of "unknown command":
// the backend answers /v1/documentation/output-example with 405 for every source.
func makeAltdataSampleCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "sample <source-id> [version]",
		Short:  "Retired: use 'altdata describe' (outputKeys) and 'altdata dictionary'",
		Hidden: true,
		Args:   cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			sourceID := "<source-id>"
			if len(args) > 0 {
				sourceID = args[0]
			}
			return fmt.Errorf("'altdata sample' is retired: the backend no longer serves example output for any source. "+
				"A source's output field paths come from its outputSchema: 'altscore altdata describe %s' lists its top-level keys as outputKeys, "+
				"and 'altscore altdata dictionary %s' lists every output field with its type and description", sourceID, sourceID)
		},
	}
}

// makeAltdataDescribeCmd builds the canonical one-shot discovery primitive for
// AltData sources. It bundles the metadata, all enabled versions, the input
// fields, and a preview of the outputSchema for the latest version into a
// single JSON document so an agent doing pre-flight on a source does not need
// to chain sources-status + dictionary.
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
sources-status. The id is looked up across every page of the catalog.

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
      "dictionary": "altscore altdata dictionary USA-PUB-0001 v2"
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
			summary, err := describeAltdataSource(c, args[0], version)
			if err != nil {
				return err
			}
			return output.JSON(summary)
		},
	}

	cmd.Flags().StringVar(&version, "version", "", "pin a specific version (default: latest)")
	return cmd
}

// An empty version selects the latest one in the catalog.
func describeAltdataSource(c *client.Client, sourceID, version string) (map[string]any, error) {
	entries, err := fetchSourceEntries(c, sourceID)
	if err != nil {
		return nil, err
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
		return nil, fmt.Errorf("source %q has no version %q (available: %s)", sourceID, selected, strings.Join(versions, ", "))
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

	return map[string]any{
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
		},
	}, nil
}

// fetchSourceEntries returns every sources-status row for sourceID, read from every page of
// the catalog: one page used to make each source sorted past row 200 a false "not found".
func fetchSourceEntries(c *client.Client, sourceID string) ([]map[string]any, error) {
	catalog, err := fetchSourceCatalog(c, altdataSourcesStatusPath)
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for _, s := range catalog {
		if sid, _ := s["sourceId"].(string); sid == sourceID {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		msg := fmt.Sprintf("source %q not found in the sources-status catalog (%d rows, every page read).", sourceID, len(catalog))
		if near := closestSourceIDs(sourceID, catalog); len(near) > 0 {
			msg += fmt.Sprintf(" Did you mean %s?", strings.Join(near, ", "))
		}
		return nil, fmt.Errorf("%s Search with 'altscore altdata sources --filter search=<text>'", msg)
	}
	return out, nil
}

// closestSourceIDs names at most three catalog ids, nearest edit distance first, that extend the
// input or sit within a small edit distance of it, ignoring case; an input unlike every id gets none.
func closestSourceIDs(input string, catalog []map[string]any) []string {
	want := strings.ToUpper(input)
	cutoff := max(2, len(want)/4)
	type candidate struct {
		id   string
		dist int
	}
	var candidates []candidate
	seen := map[string]bool{}
	for _, row := range catalog {
		id, _ := row["sourceId"].(string)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		have := strings.ToUpper(id)
		dist := levenshtein(want, have)
		if dist <= cutoff || (want != "" && strings.HasPrefix(have, want)) {
			candidates = append(candidates, candidate{id, dist})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].dist != candidates[j].dist {
			return candidates[i].dist < candidates[j].dist
		}
		return candidates[i].id < candidates[j].id
	})
	ids := make([]string, 0, 3)
	for _, cand := range candidates {
		if len(ids) == 3 {
			break
		}
		ids = append(ids, cand.id)
	}
	return ids
}

// resolveLatestSourceVersion picks the highest "v<N>" version among enabled
// entries for sourceID. Falls back to any version if none look like "v<N>".
func resolveLatestSourceVersion(c *client.Client, sourceID string) (string, error) {
	entries, err := fetchSourceEntries(c, sourceID)
	if err != nil {
		return "", err
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
