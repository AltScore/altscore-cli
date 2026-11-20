package cmd

import (
	"strings"
	"testing"
)

func TestSpreadsheetBuilderAndBatchResultsAreValidTaskTypes(t *testing.T) {
	for _, typ := range []string{"spreadsheet-builder", "batch-results"} {
		if !validTaskTypes[typ] {
			t.Errorf("%s must validate offline from the compiled-in validTaskTypes mirror", typ)
		}
		if deprecatedTaskTypes[typ] {
			t.Errorf("%s must not be deprecated", typ)
		}
	}
}

// The flow borrower-central's "create from template" writes
// (app/model/workflows_v2/batch_post_processing_template.py), as an apply spec. One
// difference: `batch` is marked additionalProperties, because apply's object-input lint
// refuses a read of inputs.batch.<field> the declaration does not cover.
func postProcessingTemplateSpec() *composeSpec {
	return &composeSpec{
		Alias:    "post-scoring",
		Label:    "Post-procesamiento Scoring",
		Category: "ACTION",
		InputVariables: map[string]any{
			"batch": map[string]any{"type": "object", "required": false, "title": "Batch", "additionalProperties": true},
		},
		ExtraNodes: []map[string]any{{"ref": "start", "type": "start", "label": "Inicio"}},
		Tasks: []map[string]any{
			{
				"ref": "planilla", "type": "spreadsheet-builder", "label": "Armar planilla",
				"inputMappings": map[string]any{"batch": "inputs.batch"},
				"spreadsheetBuilderConfig": map[string]any{
					"fileName": "resultados-del-batch.xlsx",
					"source":   "blocks",
					"sheets": []any{map[string]any{
						"id": "resultados", "name": "Resultados",
						"blocks": []any{map[string]any{
							"id": "tabla", "type": "batch-results", "batchFrom": "batch",
							"columns": "auto", "rowsFilter": "all", "includeStatus": true,
						}},
					}},
				},
			},
			{
				"ref": "tiene-correo", "type": "conditional", "label": "Tiene correo",
				"inputSchema":   map[string]any{"launcher_email": map[string]any{"type": "string"}},
				"inputMappings": map[string]any{"launcher_email": "inputs.batch.createdByEmail"},
				"branches": []any{
					map[string]any{"label": "Si", "conditions": map[string]any{
						"operator": "AND",
						"items": []any{map[string]any{
							"field": "launcher_email", "operator": "is_not_empty", "value": "", "valueType": "value"}},
					}},
					map[string]any{"label": "No", "isElse": true},
				},
			},
			{
				"ref": "correo", "type": "contact", "label": "Enviar resultados",
				"inputMappings": map[string]any{
					"launcher_email": "inputs.batch.createdByEmail",
					"results_file":   "task_outputs.planilla.fileUrl",
				},
				"contactConfig": map[string]any{
					"channel": "email", "to": "{launcher_email}", "subject": "Resultados", "body": "Adjunto.",
					"attachments": []any{map[string]any{"token": "results_file", "filename": "resultados-del-batch.xlsx"}},
				},
			},
			{
				"ref": "sin-correo", "type": "notices", "label": "Avisar",
				"noticesConfig": map[string]any{"severity": "warning", "message": "No se envio el correo."},
			},
			{"ref": "end", "type": "end", "label": "Fin"},
		},
		Edges: []map[string]any{
			{"from": "start", "to": "planilla"},
			{"from": "planilla", "to": "tiene-correo"},
			{"from": "tiene-correo", "to": "correo", "sourceHandle": "branch_0"},
			{"from": "tiene-correo", "to": "sin-correo", "sourceHandle": "branch-else"},
			{"from": "correo", "to": "end"},
			{"from": "sin-correo", "to": "end"},
		},
	}
}

func TestSpreadsheetBuilderTemplateFlowAssemblesForServerApply(t *testing.T) {
	capture := newComposeCapture()
	wf, err := composeWorkflowBody(nil, postProcessingTemplateSpec(), true, false, true, false, false, true, capture)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	flat, err := buildFlatSpecForServer(wf, capture, "post-scoring")
	if err != nil {
		t.Fatalf("buildFlatSpecForServer: %v", err)
	}
	var planilla map[string]any
	for _, n := range flat["nodes"].([]map[string]any) {
		if n["ref"] == "planilla" {
			planilla = n
		}
	}
	if planilla == nil {
		t.Fatalf("spreadsheet-builder node missing from the flat spec: %v", flat["nodes"])
	}
	cfg, _ := planilla["spreadsheetBuilderConfig"].(map[string]any)
	sheets, _ := cfg["sheets"].([]any)
	if len(sheets) != 1 {
		t.Fatalf("spreadsheetBuilderConfig must reach the server untouched, got: %v", planilla)
	}
	block := sheets[0].(map[string]any)["blocks"].([]any)[0].(map[string]any)
	if block["type"] != "batch-results" || block["batchFrom"] != "batch" {
		t.Errorf("block altered on the way to the server: %v", block)
	}
}

func sbTask(cfg map[string]any, mappings map[string]any) map[string]any {
	task := map[string]any{"ref": "xls", "label": "Build", "type": "spreadsheet-builder"}
	if cfg != nil {
		task["spreadsheetBuilderConfig"] = cfg
	}
	if mappings != nil {
		task["inputMappings"] = mappings
	}
	return task
}

func sbSheets(sheets ...any) map[string]any {
	return map[string]any{"fileName": "reporte.xlsx", "sheets": sheets}
}

func sbSheet(name string, blocks ...any) map[string]any {
	return map[string]any{"name": name, "blocks": blocks}
}

func TestSpreadsheetBuilderProblems(t *testing.T) {
	table := map[string]any{"type": "table", "rowsFrom": "ventas", "columns": "auto"}
	ventas := map[string]any{"ventas": "task_outputs.fetch.rows"}
	cases := []struct {
		name     string
		cfg      map[string]any
		mappings map[string]any
		want     []string // substrings, one per expected problem; empty = no problems
	}{
		{"valid table", sbSheets(sbSheet("Ventas", table)), ventas, nil},
		{"valid fields with valueFrom", sbSheets(sbSheet("Resumen", map[string]any{
			"type": "fields", "rows": []any{
				map[string]any{"label": "Cliente", "value": "{{inputs.nombre}}"},
				map[string]any{"label": "Score", "valueFrom": "score", "type": "number"}},
		})), map[string]any{"score": "task_outputs.s.score"}, nil},
		{"batchFrom needs no mapping", sbSheets(sbSheet("R", map[string]any{"type": "batch-results", "batchFrom": "batch"})), nil, nil},
		{"snake_case spelling", map[string]any{"sheets": []any{sbSheet("V", map[string]any{"type": "table", "rows_from": "ventas"})}}, ventas, nil},
		{"block type defaults to table", sbSheets(sbSheet("V", map[string]any{"rowsFrom": "ventas"})), ventas, nil},
		{"source code", map[string]any{"source": "code", "code": "ws = wb.add_worksheet('A')"}, nil, nil},
		{"source template", map[string]any{"source": "template", "template": map[string]any{
			"objectName": "attachments/tenant-demo/00000000-0000-0000-0000-000000000000.xlsx",
			"cells":      []any{map[string]any{"cell": "cliente", "valueFrom": "nombre"}},
			"tables":     []any{map[string]any{"sheet": "Ventas", "anchor": "A2", "rowsFrom": "ventas"}},
		}}, map[string]any{"nombre": "inputs.nombre", "ventas": "task_outputs.fetch.rows"}, nil},

		{"missing config", nil, nil, []string{"spreadsheetBuilderConfig is required"}},
		{"no sheets", map[string]any{"fileName": "a.xlsx"}, nil, []string{"add at least one sheet"}},
		{"empty sheet name", sbSheets(sbSheet("", table)), ventas, []string{"a sheet needs a name"}},
		{"long sheet name", sbSheets(sbSheet(strings.Repeat("a", 32), table)), ventas, []string{"longer than 31 characters"}},
		{"31 accented characters fit", sbSheets(sbSheet(strings.Repeat("ñ", 31), table)), ventas, nil},
		{"forbidden character", sbSheets(sbSheet("Ventas/2026", table)), ventas, []string{"contains one of"}},
		{"reserved name", sbSheets(sbSheet("History", table)), ventas, []string{"Excel reserves"}},
		{"duplicate ignoring case", sbSheets(sbSheet("Ventas", table), sbSheet("VENTAS", table)), ventas,
			[]string{"sheets 1 and 2 share the name"}},
		{"sheet without blocks", sbSheets(sbSheet("Vacia")), nil, []string{"sheet 'Vacia': add at least one block"}},
		{"table without rowsFrom", sbSheets(sbSheet("V", map[string]any{"type": "table"})), nil, []string{"rowsFrom"}},
		{"rowsFrom not mapped", sbSheets(sbSheet("V", table)), map[string]any{"otra": "inputs.x"},
			[]string{"'ventas' is not one of this node's inputMappings (mapped: otra)"}},
		{"rowsFrom not mapped, no mappings", sbSheets(sbSheet("V", table)), nil,
			[]string{"'ventas' is not one of this node's inputMappings (none are mapped)"}},
		{"valueFrom not mapped", sbSheets(sbSheet("R", map[string]any{
			"type": "fields", "rows": []any{map[string]any{"label": "Score", "valueFrom": "score"}}})), nil,
			[]string{"'score' is not one of this node's inputMappings"}},
		{"fields without rows", sbSheets(sbSheet("R", map[string]any{"type": "fields"})), nil, []string{"add at least one field"}},
		{"column without field or formula", sbSheets(sbSheet("V", map[string]any{
			"type": "table", "rowsFrom": "ventas", "columns": []any{map[string]any{"header": "Monto"}}})), ventas,
			[]string{"column 1: needs a field or a formula"}},
		{"formula without =", sbSheets(sbSheet("V", map[string]any{
			"type": "table", "rowsFrom": "ventas", "columns": []any{map[string]any{"header": "Doble", "formula": "B{row}*2"}}})), ventas,
			[]string{"a formula starts with '='"}},
		{"field written as a template", sbSheets(sbSheet("V", map[string]any{
			"type": "table", "rowsFrom": "ventas", "columns": []any{map[string]any{"header": "M", "field": "{{inputs.m}}"}}})), ventas,
			[]string{"dotted path INSIDE each row"}},
		{"empty column list", sbSheets(sbSheet("V", map[string]any{"type": "table", "rowsFrom": "ventas", "columns": []any{}})), ventas,
			[]string{"choose at least one column"}},
		{"bad column type and total", sbSheets(sbSheet("V", map[string]any{
			"type": "table", "rowsFrom": "ventas",
			"columns": []any{map[string]any{"field": "monto", "type": "money", "total": "avg"}}})), ventas,
			[]string{"type 'money' is not one of", "total 'avg' is not one of"}},
		{"unknown block type", sbSheets(sbSheet("V", map[string]any{"type": "chart"})), nil, []string{"block type 'chart' is not one of"}},
		{"artifact without alias", sbSheets(sbSheet("V", map[string]any{"type": "artifact"})), nil, []string{"artifactAlias"}},
		{"empty code block", sbSheets(sbSheet("V", map[string]any{"type": "code", "code": "  "})), nil, []string{"the code block is empty"}},
		{"bad rowsFilter", sbSheets(sbSheet("V", map[string]any{"type": "batch-results", "rowsFilter": "ok"})), nil, []string{"rowsFilter"}},
		{"maxRows zero", sbSheets(sbSheet("V", map[string]any{"type": "batch-results", "maxRows": float64(0)})), nil, []string{"maxRows must be at least 1"}},
		{"unknown source", map[string]any{"source": "pdf"}, nil, []string{"source pdf is not one of"}},
		{"code source without code", map[string]any{"source": "code"}, nil, []string{"the code is empty"}},
		{"template without file", map[string]any{"source": "template", "template": map[string]any{}}, nil, []string{"choose the template file"}},
		{"template fileFrom not mapped", map[string]any{"source": "template", "template": map[string]any{"fileFrom": "plantilla"}}, nil,
			[]string{"'plantilla' is not one of this node's inputMappings"}},
		{"template table needs exactly one source", map[string]any{"source": "template", "template": map[string]any{
			"objectName": "attachments/tenant-demo/x.xlsx",
			"tables":     []any{map[string]any{"sheet": "", "rowsFrom": "ventas", "batchFrom": "batch"}}}}, ventas,
			[]string{"name the sheet it is written on", "either a list (rowsFrom) or a batch (batchFrom)"}},
		{"every problem in one pass", sbSheets(sbSheet("A:B"), sbSheet("V", map[string]any{"type": "table"})), nil,
			[]string{"contains one of", "sheet 'A:B': add at least one block", "rowsFrom"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := spreadsheetBuilderProblems(sbTask(tc.cfg, tc.mappings))
			if len(got) != len(tc.want) {
				t.Fatalf("got %d problem(s) %q, want %d %q", len(got), got, len(tc.want), tc.want)
			}
			joined := strings.Join(got, "\n")
			for _, w := range tc.want {
				if !strings.Contains(joined, w) {
					t.Errorf("problems %q do not mention %q", got, w)
				}
			}
		})
	}
}

func sbSpec(task map[string]any) *composeSpec {
	return &composeSpec{
		Label: "Reporte", Alias: "reporte", Category: "ACTION",
		ExtraNodes: startNode,
		Tasks:      []map[string]any{task, {"ref": "end", "type": "end", "label": "End"}},
		Edges:      []map[string]any{{"from": "start", "to": "xls"}, {"from": "xls", "to": "end"}},
	}
}

func TestSpreadsheetBuilderPreflightIsAnErrorNamingTheNodeAndThePublishCode(t *testing.T) {
	err := preflightTasks(sbSpec(sbTask(sbSheets(sbSheet("Ventas", map[string]any{"type": "table", "rowsFrom": "ventas"})), nil)))
	if err == nil {
		t.Fatal("an unmapped rowsFrom must fail preflight: publish would refuse it")
	}
	for _, want := range []string{`ref="xls"`, "SPREADSHEET_BUILDER_INVALID", "'ventas' is not one of this node's inputMappings"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestBatchResultsPreflight(t *testing.T) {
	task := func(cfg map[string]any) *composeSpec {
		body := map[string]any{"ref": "xls", "label": "Read batch", "type": "batch-results"}
		if cfg != nil {
			body["batchResultsConfig"] = cfg
		}
		return sbSpec(body)
	}
	for name, cfg := range map[string]map[string]any{
		"no config":    nil,
		"summary":      {"batchFrom": "batch", "mode": "summary"},
		"rows":         {"mode": "rows", "rowsFilter": "failed", "fields": []any{"score"}, "maxRows": float64(50)},
		"over the cap": {"mode": "rows", "maxRows": float64(9000)}, // advisory only: the backend caps
	} {
		if err := preflightTasks(task(cfg)); err != nil {
			t.Errorf("%s: preflight should accept it, got: %v", name, err)
		}
	}
	for name, tc := range map[string]struct {
		cfg  map[string]any
		want string
	}{
		"bad mode":       {map[string]any{"mode": "all"}, "mode"},
		"bad rowsFilter": {map[string]any{"rowsFilter": "ok"}, "rowsFilter"},
		"maxRows zero":   {map[string]any{"maxRows": float64(0)}, "maxRows must be at least 1"},
		"fields string":  {map[string]any{"fields": "score"}, "fields is a list"},
	} {
		err := preflightTasks(task(tc.cfg))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want an error mentioning %q, got: %v", name, tc.want, err)
		}
	}
}

func TestSpreadsheetBuilder_MappingKeysAreNotTreatedAsResidualRefs(t *testing.T) {
	body := map[string]any{
		"type":          "spreadsheet-builder",
		"inputMappings": map[string]any{"ventas": "task_outputs.ventas-server-1234.rows"},
		"spreadsheetBuilderConfig": map[string]any{
			"sheets": []any{map[string]any{
				"id": "ventas", "name": "ventas",
				"blocks": []any{
					map[string]any{"id": "ventas", "type": "table", "rowsFrom": "ventas", "explodeField": "ventas",
						"columns": []any{map[string]any{"header": "ventas", "field": "ventas"}}},
					map[string]any{"type": "batch-results", "batchFrom": "ventas"},
					map[string]any{"type": "artifact", "artifactAlias": "ventas"},
					map[string]any{"type": "fields", "rows": []any{map[string]any{"label": "ventas", "valueFrom": "ventas"}}},
				},
			}},
			"template": map[string]any{
				"fileFrom": "ventas", "fileLabel": "ventas",
				"cells":  []any{map[string]any{"cell": "ventas", "valueFrom": "ventas"}},
				"tables": []any{map[string]any{"sheet": "ventas", "rowsFrom": "ventas", "explodeField": "ventas"}},
			},
		},
	}
	refMap := map[string]string{"ventas": "ventas-server-1234"}
	if err := validateNoResidualSpecRefs(body, refMap, "compose"); err != nil {
		t.Fatalf("an inputMappings key named like the node it reads must not abort apply, got: %v", err)
	}
	batch := map[string]any{"type": "batch-results",
		"batchResultsConfig": map[string]any{"batchFrom": "ventas", "fields": []any{"ventas"}}}
	if err := validateNoResidualSpecRefs(batch, refMap, "compose"); err != nil {
		t.Fatalf("batchResultsConfig literals must not abort apply, got: %v", err)
	}
}

func TestSpreadsheetPreflight_ReviewFindings(t *testing.T) {
	if got := spreadsheetMaxRowsProblem("b", map[string]any{"maxRows": "0"}); len(got) != 1 || !strings.Contains(got[0], "whole number") {
		t.Errorf("a string maxRows must be reported, got %v", got)
	}
	if got := spreadsheetMaxRowsProblem("b", map[string]any{"maxRows": 2.5}); len(got) != 1 {
		t.Errorf("a fractional maxRows must be reported, got %v", got)
	}
	if got := spreadsheetMaxRowsProblem("b", map[string]any{"maxRows": float64(10)}); len(got) != 0 {
		t.Errorf("a whole maxRows is fine, got %v", got)
	}
	var keys []string
	got := spreadsheetBlockProblems("b", map[string]any{"type": "fields", "rows": []any{false, map[string]any{"label": "x", "value": "y"}}}, &keys)
	if len(got) != 1 || !strings.Contains(got[0], "field 1") {
		t.Errorf("a non-object field row must be reported, got %v", got)
	}
	snake := map[string]any{"batch_results_config": map[string]any{"mode": "rows", "max_rows": float64(9000)}}
	if batchResultsRowCapAdvisory(snake) == "" {
		t.Error("the row-cap advisory must read the snake_case spelling too")
	}
}
