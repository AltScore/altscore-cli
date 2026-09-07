package cmd

import "testing"

// documents.<key>.<attr> is the http task's OWN late-resolved input. The
// optional `documents` list on the task names borrower documents by key, and
// BC substitutes {{documents.<key>.base64}} (also .fileName, .mimeType,
// .sizeBytes, .files) in a second pass inside http_activity, exactly like
// End's {{self.pdf_url}}. The bytes do not exist at graph time and never reach
// task_outputs, so apply must treat the head as a reserved scope: never
// rewritten to a server alias, never rejected as an unknown ref.

func TestDocumentsIsAReservedMappingScope(t *testing.T) {
	if !reservedMappingScopes["documents"] {
		t.Fatal("documents must be a reserved mapping scope; otherwise apply rejects {{documents.<key>.base64}} in an http body as an unknown head")
	}
}

func TestDocumentsRefSurvivesHttpBodyRewrite(t *testing.T) {
	refMap := map[string]string{"scoring": "scoring-engine-score-30402a"}
	task := map[string]any{
		"type":   "http",
		"method": "POST",
		"url":    "https://ocr.example.test/parse",
		"body":   `{"file": "{{documents.ine.base64}}", "name": "{{documents.ine.fileName}}", "score": "{{task_outputs.scoring.score}}"}`,
	}
	if err := rewriteRefsInTaskTemplates(task, refMap); err != nil {
		t.Fatalf("rewriteRefsInTaskTemplates must accept {{documents.*}} in an http body, got: %v", err)
	}
	want := `{"file": "{{documents.ine.base64}}", "name": "{{documents.ine.fileName}}", "score": "{{task_outputs.scoring-engine-score-30402a.score}}"}`
	if got := task["body"]; got != want {
		t.Errorf("body:\n got  %q\n want %q", got, want)
	}
}
