package cmd

import "testing"

// spreadsheet-extraction is a backend task type as of borrower-central #1913.
// Absent from validTaskTypes, preflight falls back to the live-backend lookup
// and warns that the type is newer than this build -- true but noisy, and it
// skips per-type local validation entirely.
func TestSpreadsheetExtractionIsAValidTaskType(t *testing.T) {
	if !validTaskTypes["spreadsheet-extraction"] {
		t.Fatalf("spreadsheet-extraction must be in validTaskTypes")
	}
}

func spreadsheetSpec(cfg map[string]any, mappings map[string]any) *composeSpec {
	task := map[string]any{
		"ref":                         "read",
		"label":                       "Read the agenda",
		"type":                        "spreadsheet-extraction",
		"spreadsheetExtractionConfig": cfg,
	}
	if mappings != nil {
		task["inputMappings"] = mappings
	}
	return &composeSpec{
		Label:      "Agenda de vencimientos",
		Alias:      "agenda-de-vencimientos",
		Category:   "ACTION",
		ExtraNodes: startNode,
		Tasks: []map[string]any{
			{
				"ref":    "download",
				"type":   "http",
				"label":  "Download the workbook",
				"url":    "https://example.com/a.xlsx",
				"method": "GET",
			},
			task,
			{"ref": "end", "type": "end", "label": "End"},
		},
		Edges: []map[string]any{
			{"from": "start", "to": "download"},
			{"from": "download", "to": "read"},
			{"from": "read", "to": "end"},
		},
	}
}

func TestSpreadsheetExtractionPreflightAcceptsAConfiguredFile(t *testing.T) {
	err := preflightTasks(spreadsheetSpec(
		map[string]any{"fileUrl": "https://example.com/a.xlsx"}, nil))
	if err != nil {
		t.Fatalf("preflight should accept a well-formed spreadsheet-extraction task, got: %v", err)
	}
}

// The recommended wiring: an upstream http download feeds fileBase64. Counting
// only config values would report this as a missing source.
func TestSpreadsheetExtractionPreflightAcceptsAMappedFile(t *testing.T) {
	err := preflightTasks(spreadsheetSpec(
		map[string]any{},
		map[string]any{"fileBase64": "task_outputs.download.response"}))
	if err != nil {
		t.Fatalf("preflight should accept a mapped file source, got: %v", err)
	}
}

func TestSpreadsheetExtractionPreflightAcceptsSnakeCaseMapping(t *testing.T) {
	err := preflightTasks(spreadsheetSpec(
		map[string]any{},
		map[string]any{"file_base64": "task_outputs.download.response"}))
	if err != nil {
		t.Fatalf("preflight should accept the snake_case spelling the backend also accepts, got: %v", err)
	}
}

func TestSpreadsheetExtractionPreflightRejectsNoSource(t *testing.T) {
	err := preflightTasks(spreadsheetSpec(map[string]any{"sheet": "Detalle"}, nil))
	if err == nil {
		t.Fatalf("preflight should reject a spreadsheet-extraction task with no file source")
	}
}

func TestSpreadsheetExtractionPreflightRejectsTwoSources(t *testing.T) {
	err := preflightTasks(spreadsheetSpec(map[string]any{
		"fileUrl":    "https://example.com/a.xlsx",
		"fileBase64": "UEsDBA==",
	}, nil))
	if err == nil {
		t.Fatalf("preflight should reject mutually exclusive file sources")
	}
}
