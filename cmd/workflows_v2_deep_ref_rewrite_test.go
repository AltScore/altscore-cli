package cmd

import (
	"reflect"
	"strings"
	"testing"
)

// The class this pass closes: a task type or field the typed rewriters have
// never heard of carries a long-form ref, and apply must both order it after
// the task it names and rewrite it to the server alias. Before the generic pass
// each such field was a fix of its own (#18, #20, #21, #106, #111).

func deepRefMap() map[string]string {
	return map[string]string{"q1": "consulta-turso-8f21c3", "fetch": "fetch-a1b2c3"}
}

func TestRewriteTaskRefs_UnlistedFieldLongFormIsRewritten(t *testing.T) {
	task := map[string]any{
		"type":  "brand-new-type",
		"label": "Nuevo",
		"brandNewConfig": map[string]any{
			"source": "task_outputs.q1.rows",
			"steps": []any{
				map[string]any{"tpl": "{{task_outputs.q1.rows[0].uno}} and {{inputs.x}}"},
				"task_outputs.fetch.body",
			},
			"byType": map[string]any{"task_outputs.q1.total": "number"},
		},
	}
	if err := rewriteTaskRefs(task, deepRefMap(), `node ref="n"`); err != nil {
		t.Fatalf("rewrite must succeed on an unlisted field: %v", err)
	}
	cfg := task["brandNewConfig"].(map[string]any)
	if got := cfg["source"]; got != "task_outputs.consulta-turso-8f21c3.rows" {
		t.Errorf("source not rewritten: %v", got)
	}
	steps := cfg["steps"].([]any)
	if got := steps[0].(map[string]any)["tpl"]; got != "{{task_outputs.consulta-turso-8f21c3.rows[0].uno}} and {{inputs.x}}" {
		t.Errorf("nested template not rewritten: %v", got)
	}
	if got := steps[1]; got != "task_outputs.fetch-a1b2c3.body" {
		t.Errorf("array string not rewritten: %v", got)
	}
	wantKeys := map[string]any{"task_outputs.consulta-turso-8f21c3.total": "number"}
	if !reflect.DeepEqual(cfg["byType"], wantKeys) {
		t.Errorf("map key not rewritten: %v", cfg["byType"])
	}
}

// Prose fields are the guard's exclusion list and must stay the author's text:
// a label that mentions a ref is documentation, not wiring.
func TestRewriteTaskRefs_ProseFieldsUntouched(t *testing.T) {
	task := map[string]any{
		"type":        "brand-new-type",
		"label":       "reads task_outputs.q1.rows",
		"description": "binds {{task_outputs.q1.rows[0].uno}} into the table",
		"brandNewConfig": map[string]any{
			"comment": []any{"see task_outputs.q1.rows"},
			"source":  "task_outputs.q1.rows",
		},
	}
	if err := rewriteTaskRefs(task, deepRefMap(), `node ref="n"`); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task["label"] != "reads task_outputs.q1.rows" || task["description"] != "binds {{task_outputs.q1.rows[0].uno}} into the table" {
		t.Errorf("prose rewritten: label=%v description=%v", task["label"], task["description"])
	}
	cfg := task["brandNewConfig"].(map[string]any)
	if got := cfg["comment"].([]any)[0]; got != "see task_outputs.q1.rows" {
		t.Errorf("array under a prose key rewritten: %v", got)
	}
	if got := cfg["source"]; got != "task_outputs.consulta-turso-8f21c3.rows" {
		t.Errorf("non-prose sibling must still be rewritten: %v", got)
	}
}

// Assembly runs the rewrite with an identity map, the post phase with the real
// one; the pass must be a no-op for the first and idempotent for the second.
func TestRewriteTaskOutputsRefsDeep_IdentityAndIdempotent(t *testing.T) {
	build := func() map[string]any {
		return map[string]any{
			"cfg": map[string]any{
				"a":    "task_outputs.q1.rows",
				"keys": map[string]any{"task_outputs.q1.total": "number", "inputs.x": "string"},
			},
		}
	}
	identity := build()
	rewriteTaskOutputsRefsDeep(identity, map[string]string{"q1": "q1"}, nil)
	if !reflect.DeepEqual(identity, build()) {
		t.Errorf("identity map must not change anything: %v", identity)
	}
	once, twice := build(), build()
	rewriteTaskOutputsRefsDeep(once, deepRefMap(), nil)
	rewriteTaskOutputsRefsDeep(twice, deepRefMap(), nil)
	rewriteTaskOutputsRefsDeep(twice, deepRefMap(), nil)
	if !reflect.DeepEqual(once, twice) {
		t.Errorf("not idempotent\nonce  %v\ntwice %v", once, twice)
	}
}

// A key already declared under the destination name owns its slot, whatever
// order Go iterates the map in.
func TestRewriteTaskOutputsRefsDeep_KeyCollisionKeepsDeclared(t *testing.T) {
	for i := 0; i < 50; i++ {
		m := map[string]any{
			"task_outputs.q1.total":                    "stale",
			"task_outputs.consulta-turso-8f21c3.total": "declared",
		}
		rewriteTaskOutputsRefsDeep(m, deepRefMap(), nil)
		if len(m) != 1 || m["task_outputs.consulta-turso-8f21c3.total"] != "declared" {
			t.Fatalf("iteration %d: %v", i, m)
		}
	}
}

func TestTaskOutputsHeads(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"task_outputs.q1.rows", []string{"q1"}},
		{"{{task_outputs.q1.rows[0].uno}} + task_outputs.fetch", []string{"q1", "fetch"}},
		{"task_outputs_by_type.http.x", nil},
		{"inputs.q1", nil},
		{"$task_outputs.a-b_c.d", []string{"a-b_c"}},
		{"task_outputs.", nil},
	}
	for _, c := range cases {
		if got := taskOutputsHeads(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("taskOutputsHeads(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestDeepTaskOutputsRefs_SkipsProseAndDedupes(t *testing.T) {
	body := map[string]any{
		"label": "task_outputs.prose.x",
		"cfg": map[string]any{
			"a":    "task_outputs.q1.rows",
			"b":    []any{"task_outputs.q1.total", "task_outputs.fetch.body"},
			"keys": map[string]any{"task_outputs.zeta.k": 1},
		},
	}
	got := deepTaskOutputsRefs(body, residualSpecRefExcludedFields)
	want := []string{"q1", "fetch", "zeta"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}

// A forward reference in a field no typed scanner names must still order the
// consumer after its producer; otherwise refMap lacks the alias at post time
// and the rewrite silently leaves the spec ref in place.
func TestTopologicalTaskOrder_DeepLongFormRefOrdersConsumerLast(t *testing.T) {
	tasks := []map[string]any{
		{"ref": "consumer", "type": "brand-new-type", "label": "C",
			"brandNewConfig": map[string]any{"source": "task_outputs.producer.rows"}},
		{"ref": "producer", "type": "http", "label": "P", "url": "https://x"},
	}
	order, err := topologicalTaskOrder(tasks, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(order, []int{1, 0}) {
		t.Errorf("producer must come first; got order %v", order)
	}

	// A mention in prose is not a dependency and must not manufacture a cycle.
	tasks[1]["description"] = "feeds task_outputs.consumer.x"
	if _, err := topologicalTaskOrder(tasks, nil); err != nil {
		t.Errorf("prose must not create a dependency: %v", err)
	}
}

// End-to-end through the split: the assembled body keeps the spec ref, the
// posted body carries the alias, for a field the allowlist has never seen.
func TestRewriteTaskRefs_UnlistedFieldSurvivesResidualGuard(t *testing.T) {
	task := map[string]any{
		"type":           "brand-new-type",
		"label":          "Nuevo",
		"brandNewConfig": map[string]any{"source": "{{task_outputs.q1.rows}}"},
	}
	if err := rewriteTaskRefs(task, map[string]string{"q1": "q1"}, "assembly"); err != nil {
		t.Fatalf("identity pass: %v", err)
	}
	if err := rewriteTaskRefs(task, deepRefMap(), "post"); err != nil {
		t.Fatalf("post pass: %v", err)
	}
	got := task["brandNewConfig"].(map[string]any)["source"].(string)
	if !strings.Contains(got, "consulta-turso-8f21c3") || strings.Contains(got, "q1.") {
		t.Errorf("posted body must carry the alias only: %q", got)
	}
}
