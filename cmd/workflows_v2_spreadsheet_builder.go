package cmd

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// Offline mirror of borrower-central's app/model/workflows_v2/spreadsheet_builder.py
// (validate_config + mapped_keys) and of the publish rule that reads them
// (SPREADSHEET_BUILDER_INVALID). BC SAVES this config unvalidated and refuses it only at
// publish, so without this pass an apply leaves a draft that cannot go live. BC stays
// the authority: a rule added there and missing here is still reported by the server.

const (
	spreadsheetMaxSheetNameLength = 31
	spreadsheetMaxSheets          = 100
	spreadsheetMaxColumns         = 16384
	batchResultsMaxRowsCap        = 5000
)

var (
	spreadsheetSheetNameForbidden = regexp.MustCompile(`[\[\]:*?/\\]`)

	spreadsheetBlockTypes   = []string{"fields", "table", "batch-results", "artifact", "code"}
	spreadsheetSources      = []string{"blocks", "code", "template"}
	spreadsheetColumnTypes  = []string{"auto", "text", "number", "currency", "percent", "date", "boolean"}
	spreadsheetColumnTotals = []string{"sum", "average", "count", "min", "max"}
	batchRowsFilters        = []string{"all", "successful", "failed"}
	batchResultsModes       = []string{"summary", "rows"}
)

func stringIn(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// BC's models populate by alias OR field name, so either spelling reaches the worker.
func sbValue(m map[string]any, camel string) (any, bool) {
	if v, ok := m[camel]; ok {
		return v, true
	}
	v, ok := m[camelToSnake(camel)]
	return v, ok
}

func sbString(m map[string]any, camel string) string {
	v, _ := sbValue(m, camel)
	s, _ := v.(string)
	return s
}

func spreadsheetSheetNameProblem(name string) string {
	if strings.TrimSpace(name) == "" {
		return "a sheet needs a name"
	}
	if utf8.RuneCountInString(name) > spreadsheetMaxSheetNameLength {
		return fmt.Sprintf("sheet name '%s' is longer than %d characters", name, spreadsheetMaxSheetNameLength)
	}
	if spreadsheetSheetNameForbidden.MatchString(name) {
		return fmt.Sprintf(`sheet name '%s' contains one of [ ] : * ? / \`, name)
	}
	if strings.HasPrefix(name, "'") || strings.HasSuffix(name, "'") {
		return fmt.Sprintf("sheet name '%s' cannot start or end with an apostrophe", name)
	}
	if strings.ToLower(strings.TrimSpace(name)) == "history" {
		return "'History' is a name Excel reserves"
	}
	return ""
}

func spreadsheetRowsFilterProblem(where string, m map[string]any) []string {
	raw, present := sbValue(m, "rowsFilter")
	if !present || raw == nil {
		return nil
	}
	if s, ok := raw.(string); ok && stringIn(batchRowsFilters, s) {
		return nil
	}
	return []string{fmt.Sprintf("%s: rowsFilter %v is not one of %s", where, raw, strings.Join(batchRowsFilters, ", "))}
}

func spreadsheetColumnProblems(where string, m map[string]any) []string {
	raw, present := m["columns"]
	if !present || raw == nil {
		return nil
	}
	if s, ok := raw.(string); ok {
		if s == "auto" {
			return nil
		}
		return []string{fmt.Sprintf(`%s: columns is "auto" or a list of columns, not %q`, where, s)}
	}
	columns, ok := raw.([]any)
	if !ok {
		return []string{fmt.Sprintf(`%s: columns is "auto" or a list of columns`, where)}
	}
	if len(columns) == 0 {
		return []string{fmt.Sprintf("%s: choose at least one column, or let them be automatic", where)}
	}
	var problems []string
	if len(columns) > spreadsheetMaxColumns {
		problems = append(problems, fmt.Sprintf("%s: more than %d columns", where, spreadsheetMaxColumns))
	}
	for i, rawColumn := range columns {
		column, ok := rawColumn.(map[string]any)
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"%s, column %d: must be an object {header, field | formula, type?, format?, width?, total?}", where, i+1))
			continue
		}
		field, _ := column["field"].(string)
		formula, _ := column["formula"].(string)
		if field == "" && formula == "" {
			problems = append(problems, fmt.Sprintf("%s, column %d: needs a field or a formula", where, i+1))
		}
		if formula != "" && !strings.HasPrefix(strings.TrimSpace(formula), "=") {
			problems = append(problems, fmt.Sprintf("%s, column %d: a formula starts with '='", where, i+1))
		}
		if strings.Contains(field, "{{") {
			problems = append(problems, fmt.Sprintf(
				"%s, column %d: field %q is a {{...}} template, but field is a dotted path INSIDE each row (e.g. \"persona.nombre\")",
				where, i+1, field))
		}
		if t, _ := column["type"].(string); t != "" && !stringIn(spreadsheetColumnTypes, t) {
			problems = append(problems, fmt.Sprintf("%s, column %d: type '%s' is not one of %s",
				where, i+1, t, strings.Join(spreadsheetColumnTypes, ", ")))
		}
		if t, _ := column["total"].(string); t != "" && !stringIn(spreadsheetColumnTotals, t) {
			problems = append(problems, fmt.Sprintf("%s, column %d: total '%s' is not one of %s",
				where, i+1, t, strings.Join(spreadsheetColumnTotals, ", ")))
		}
	}
	return problems
}

func spreadsheetMaxRowsProblem(where string, m map[string]any) []string {
	raw, present := sbValue(m, "maxRows")
	if !present || raw == nil {
		return nil
	}
	n, ok := raw.(float64)
	if !ok || n != float64(int64(n)) {
		return []string{fmt.Sprintf("%s: maxRows must be a whole number (got %v)", where, raw)}
	}
	if n < 1 {
		return []string{fmt.Sprintf("%s: maxRows must be at least 1", where)}
	}
	return nil
}

// keys collects the inputMappings keys the block reads (BC's mapped_keys).
func spreadsheetBlockProblems(where string, block map[string]any, keys *[]string) []string {
	blockType, _ := block["type"].(string)
	if blockType == "" {
		blockType = "table"
	}
	if !stringIn(spreadsheetBlockTypes, blockType) {
		return []string{fmt.Sprintf("%s: block type '%s' is not one of %s", where, blockType, strings.Join(spreadsheetBlockTypes, ", "))}
	}
	var problems []string
	switch blockType {
	case "fields":
		rows := asSlice(block["rows"])
		if len(rows) == 0 {
			problems = append(problems, fmt.Sprintf("%s: add at least one field (rows: [{label, value | valueFrom}])", where))
		}
		for i, rawRow := range rows {
			row, ok := rawRow.(map[string]any)
			if !ok {
				problems = append(problems, fmt.Sprintf("%s, field %d: must be an object {label, value | valueFrom}", where, i+1))
				continue
			}
			if key := sbString(row, "valueFrom"); key != "" {
				*keys = append(*keys, key)
			}
		}
	case "table":
		if key := sbString(block, "rowsFrom"); key == "" {
			problems = append(problems, fmt.Sprintf(
				"%s: choose the list the table shows (rowsFrom = the inputMappings key that resolves to it; rows never go in the config)", where))
		} else {
			*keys = append(*keys, key)
		}
		problems = append(problems, spreadsheetColumnProblems(where, block)...)
	case "batch-results":
		problems = append(problems, spreadsheetColumnProblems(where, block)...)
		problems = append(problems, spreadsheetRowsFilterProblem(where, block)...)
	case "artifact":
		if sbString(block, "artifactAlias") == "" {
			problems = append(problems, fmt.Sprintf("%s: choose the artifact table (artifactAlias)", where))
		}
		problems = append(problems, spreadsheetColumnProblems(where, block)...)
	case "code":
		if code, _ := block["code"].(string); strings.TrimSpace(code) == "" {
			problems = append(problems, fmt.Sprintf("%s: the code block is empty", where))
		}
	}
	return append(problems, spreadsheetMaxRowsProblem(where, block)...)
}

// Every problem of one spreadsheet-builder task, as sentences; empty means publishable
// as far as an offline pass can tell.
func spreadsheetBuilderProblems(task map[string]any) []string {
	rawConfig, present := sbValue(task, "spreadsheetBuilderConfig")
	config, isObject := rawConfig.(map[string]any)
	if !present || rawConfig == nil || !isObject {
		return []string{"spreadsheetBuilderConfig is required for a spreadsheet-builder node " +
			"({fileName, source: 'blocks'|'code'|'template', sheets: [{name, blocks: [...]}]})"}
	}
	source := "blocks"
	if raw, has := config["source"]; has && raw != nil {
		s, _ := raw.(string)
		if !stringIn(spreadsheetSources, s) {
			return []string{fmt.Sprintf("source %v is not one of %s", raw, strings.Join(spreadsheetSources, ", "))}
		}
		source = s
	}

	var problems []string
	var keys []string
	switch source {
	case "code":
		if code, _ := config["code"].(string); strings.TrimSpace(code) == "" {
			problems = append(problems, "the file is drawn with code, and the code is empty")
		}
		return problems
	case "template":
		template := asMap(config["template"])
		fileFrom := sbString(template, "fileFrom")
		if fileFrom == "" && sbString(template, "objectName") == "" {
			return []string{"choose the template file to fill (template.objectName, or template.fileFrom = an inputMappings key resolving to a link)"}
		}
		if fileFrom != "" {
			keys = append(keys, fileFrom)
		}
		for i, rawCell := range asSlice(template["cells"]) {
			cell, _ := rawCell.(map[string]any)
			if name, _ := cell["cell"].(string); strings.TrimSpace(name) == "" {
				problems = append(problems, fmt.Sprintf("template cell %d: name the cell to fill", i+1))
			}
			if key := sbString(cell, "valueFrom"); key != "" {
				keys = append(keys, key)
			}
		}
		for i, rawTable := range asSlice(template["tables"]) {
			table, _ := rawTable.(map[string]any)
			where := fmt.Sprintf("template table %d", i+1)
			if sheet, _ := table["sheet"].(string); strings.TrimSpace(sheet) == "" {
				problems = append(problems, fmt.Sprintf("%s: name the sheet it is written on", where))
			}
			rowsFrom := sbString(table, "rowsFrom")
			if (rowsFrom != "") == (sbString(table, "batchFrom") != "") {
				problems = append(problems, fmt.Sprintf("%s: choose either a list (rowsFrom) or a batch (batchFrom)", where))
			}
			if rowsFrom != "" {
				keys = append(keys, rowsFrom)
			}
			problems = append(problems, spreadsheetColumnProblems(where, table)...)
			problems = append(problems, spreadsheetRowsFilterProblem(where, table)...)
		}
	default:
		sheets := asSlice(config["sheets"])
		if len(sheets) == 0 {
			return []string{"add at least one sheet (sheets: [{name, blocks: [...]}])"}
		}
		if len(sheets) > spreadsheetMaxSheets {
			problems = append(problems, fmt.Sprintf("a workbook holds at most %d sheets", spreadsheetMaxSheets))
		}
		seen := map[string]int{}
		for i, rawSheet := range sheets {
			position := i + 1
			sheet, _ := rawSheet.(map[string]any)
			name, _ := sheet["name"].(string)
			if problem := spreadsheetSheetNameProblem(name); problem != "" {
				problems = append(problems, fmt.Sprintf("sheet %d: %s", position, problem))
			}
			key := strings.ToLower(strings.TrimSpace(name))
			if first, dup := seen[key]; dup && key != "" {
				problems = append(problems, fmt.Sprintf(
					"sheets %d and %d share the name '%s' (sheet names are unique ignoring case)", first, position, name))
			} else if !dup {
				seen[key] = position
			}
			shown := name
			if shown == "" {
				shown = fmt.Sprintf("%d", position)
			}
			blocks := asSlice(sheet["blocks"])
			if len(blocks) == 0 {
				problems = append(problems, fmt.Sprintf("sheet '%s': add at least one block", shown))
			}
			for bi, rawBlock := range blocks {
				where := fmt.Sprintf("sheet '%s', block %d", shown, bi+1)
				block, ok := rawBlock.(map[string]any)
				if !ok {
					problems = append(problems, fmt.Sprintf("%s: must be an object with a type (%s)", where, strings.Join(spreadsheetBlockTypes, ", ")))
					continue
				}
				problems = append(problems, spreadsheetBlockProblems(where, block, &keys)...)
			}
		}
	}

	mappings, _ := task["inputMappings"].(map[string]any)
	mapped := make([]string, 0, len(mappings))
	for k := range mappings {
		mapped = append(mapped, k)
	}
	sort.Strings(mapped)
	have := "none are mapped"
	if len(mapped) > 0 {
		have = "mapped: " + strings.Join(mapped, ", ")
	}
	reported := map[string]bool{}
	for _, key := range keys {
		if _, ok := mappings[key]; ok || reported[key] {
			continue
		}
		reported[key] = true
		problems = append(problems, fmt.Sprintf(
			"'%s' is not one of this node's inputMappings (%s). rowsFrom / valueFrom / template.fileFrom name an "+
				"inputMappings KEY, e.g. inputMappings {\"%s\": \"task_outputs.<ref>.rows\"}", key, have, key))
	}
	return problems
}

func batchResultsProblems(task map[string]any) []string {
	rawConfig, present := sbValue(task, "batchResultsConfig")
	if !present || rawConfig == nil {
		return nil // every field has a default: a summary of the `batch` input
	}
	config, ok := rawConfig.(map[string]any)
	if !ok {
		return []string{"batchResultsConfig must be an object {batchFrom?, mode, rowsFilter, fields?, includeInput?, maxRows?}"}
	}
	var problems []string
	if raw, has := config["mode"]; has && raw != nil {
		if s, _ := raw.(string); !stringIn(batchResultsModes, s) {
			problems = append(problems, fmt.Sprintf("batchResultsConfig.mode %v is not one of %s", raw, strings.Join(batchResultsModes, ", ")))
		}
	}
	problems = append(problems, spreadsheetRowsFilterProblem("batchResultsConfig", config)...)
	problems = append(problems, spreadsheetMaxRowsProblem("batchResultsConfig", config)...)
	if raw, has := config["fields"]; has && raw != nil {
		if _, isList := raw.([]any); !isList {
			problems = append(problems, "batchResultsConfig.fields is a list of dotted paths inside each row, or null")
		}
	}
	return problems
}

// Advisory only: the backend caps silently instead of refusing.
func batchResultsRowCapAdvisory(task map[string]any) string {
	rawConfig, _ := sbValue(task, "batchResultsConfig")
	config := asMap(rawConfig)
	raw, _ := sbValue(config, "maxRows")
	if n, ok := raw.(float64); ok && n > batchResultsMaxRowsCap {
		return fmt.Sprintf("batchResultsConfig.maxRows=%v is above the %d-row cap: the node returns at most %d rows "+
			"(and at most 1 MB) with truncated=true. A sheet with EVERY row is a spreadsheet-builder node's "+
			"'batch-results' block, which streams them into the file", raw, batchResultsMaxRowsCap, batchResultsMaxRowsCap)
	}
	return ""
}
