package cmd

import (
	"reflect"
	"testing"
)

// refMap used across these tests. `tabla` and `tablas` both exist on purpose:
// the boundary check has to tell them apart, and a plain substring replace does
// not. `keep` maps to itself, which the rewriter must skip entirely.
func testRefMap() map[string]string {
	return map[string]string{
		"tablas": "tablas-de-pol-tica-22503b",
		"tabla":  "tabla-suelta-9f11a2",
		"ola1":   "ola-1-identidad-c00e51",
		"keep":   "keep",
	}
}

func TestReplaceTaskOutputsRef_Boundaries(t *testing.T) {
	m := testRefMap()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"trailing field", "task_outputs.tablas.gasto", "task_outputs.tablas-de-pol-tica-22503b.gasto"},
		// Everything below this line was missed while a trailing dot was the
		// only boundary.
		{"bare ref", "task_outputs.tablas", "task_outputs.tablas-de-pol-tica-22503b"},
		{"bracket index", "task_outputs.tablas[0].x", "task_outputs.tablas-de-pol-tica-22503b[0].x"},
		{"array map", "task_outputs.ola1.arr[].f", "task_outputs.ola-1-identidad-c00e51.arr[].f"},
		{"dollar prefixed", "SUM($task_outputs.tablas.a, 1)", "SUM($task_outputs.tablas-de-pol-tica-22503b.a, 1)"},
		{"trailing quote", "inputs['task_outputs.tablas.a']", "inputs['task_outputs.tablas-de-pol-tica-22503b.a']"},
		// A shorter ref must not match inside a longer one, in either order.
		{"longer ref wins", "task_outputs.tablas.a", "task_outputs.tablas-de-pol-tica-22503b.a"},
		{"shorter ref alone", "task_outputs.tabla.a", "task_outputs.tabla-suelta-9f11a2.a"},
		{"both in one string", "task_outputs.tabla.a + task_outputs.tablas.b",
			"task_outputs.tabla-suelta-9f11a2.a + task_outputs.tablas-de-pol-tica-22503b.b"},
		// Untouched: identity mapping, unknown refs, other scopes.
		{"identity ref", "task_outputs.keep.a", "task_outputs.keep.a"},
		{"unknown ref", "task_outputs.otro.a", "task_outputs.otro.a"},
		{"other scope", "inputs.tablas + self.tablas", "inputs.tablas + self.tablas"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rewriteTaskOutputsRefsInString(c.in, m); got != c.want {
				t.Errorf("got  %q\nwant %q", got, c.want)
			}
		})
	}
}

// The bug this whole change exists for: the formula the client reads lives in
// simpleConfig.formulaText, which no field-by-name rewriter ever touched.
func TestRewriteCustomVariableRefs_SimpleConfigFormulaText(t *testing.T) {
	v := map[string]any{
		"editorMode": "simple",
		"expression": "result = inputs['task_outputs.tablas.gasto']",
		"simpleConfig": map[string]any{
			"type":        "formula",
			"formulaText": "SUM(OR($task_outputs.tablas.gasto, 0), $task_outputs.ola1.x)",
		},
	}
	rewriteCustomVariableRefs(v, testRefMap())

	sc := v["simpleConfig"].(map[string]any)
	wantFormula := "SUM(OR($task_outputs.tablas-de-pol-tica-22503b.gasto, 0), $task_outputs.ola-1-identidad-c00e51.x)"
	if got := sc["formulaText"].(string); got != wantFormula {
		t.Errorf("formulaText\ngot  %q\nwant %q", got, wantFormula)
	}
	if got, want := sc["type"].(string), "formula"; got != want {
		t.Errorf("simpleConfig.type mangled: got %q want %q", got, want)
	}
	if got, want := v["editorMode"].(string), "simple"; got != want {
		t.Errorf("editorMode mangled: got %q want %q", got, want)
	}
}

// Guard on the extraction: the fields that already worked must keep working.
func TestRewriteCustomVariableRefs_ExistingFields(t *testing.T) {
	v := map[string]any{
		"expression":   "result = inputs['task_outputs.tablas.gasto']",
		"returnValue":  "task_outputs.ola1.x",
		"dependencies": []any{"task_outputs.tablas.gasto", "inputs.precio", "self.otra"},
	}
	rewriteCustomVariableRefs(v, testRefMap())

	if want := "result = inputs['task_outputs.tablas-de-pol-tica-22503b.gasto']"; v["expression"] != want {
		t.Errorf("expression: got %v want %q", v["expression"], want)
	}
	if want := "task_outputs.ola-1-identidad-c00e51.x"; v["returnValue"] != want {
		t.Errorf("returnValue: got %v want %q", v["returnValue"], want)
	}
	wantDeps := []any{"task_outputs.tablas-de-pol-tica-22503b.gasto", "inputs.precio", "self.otra"}
	if !reflect.DeepEqual(v["dependencies"], wantDeps) {
		t.Errorf("dependencies: got %v want %v", v["dependencies"], wantDeps)
	}
}

// dependencyTypes is keyed BY the ref, and had no coverage at all.
func TestRewriteCustomVariableRefs_DependencyTypesKeys(t *testing.T) {
	v := map[string]any{
		"dependencyTypes": map[string]any{
			"task_outputs.tablas.gasto": "number",
			"inputs.precio":             "number",
		},
	}
	rewriteCustomVariableRefs(v, testRefMap())

	want := map[string]any{
		"task_outputs.tablas-de-pol-tica-22503b.gasto": "number",
		"inputs.precio": "number",
	}
	if !reflect.DeepEqual(v["dependencyTypes"], want) {
		t.Errorf("got  %v\nwant %v", v["dependencyTypes"], want)
	}
}

// A key the author already declared under the destination name owns its slot:
// carrying the stale one over would replace a correct declaration with an old
// one. Runs repeatedly because Go map iteration order is random and the
// two-pass design is what makes the outcome deterministic.
func TestRewriteCustomVariableRefs_DependencyTypesCollisionKeepsDeclared(t *testing.T) {
	for i := 0; i < 50; i++ {
		v := map[string]any{
			"dependencyTypes": map[string]any{
				"task_outputs.tablas.gasto":                    "stale",
				"task_outputs.tablas-de-pol-tica-22503b.gasto": "declared",
			},
		}
		rewriteCustomVariableRefs(v, testRefMap())
		got := v["dependencyTypes"].(map[string]any)
		if got["task_outputs.tablas-de-pol-tica-22503b.gasto"] != "declared" {
			t.Fatalf("iteration %d: declared entry was overwritten: %v", i, got)
		}
		if len(got) != 1 {
			t.Fatalf("iteration %d: stale key survived: %v", i, got)
		}
	}
}

func TestRewriteCustomVariableRefs_NilAndMissingFields(t *testing.T) {
	rewriteCustomVariableRefs(nil, testRefMap()) // must not panic

	v := map[string]any{"expression": "result = 1"}
	rewriteCustomVariableRefs(v, testRefMap())
	if v["expression"] != "result = 1" {
		t.Errorf("expression changed unexpectedly: %v", v["expression"])
	}
	if _, present := v["simpleConfig"]; present {
		t.Error("simpleConfig invented on a variable that had none")
	}
}

// The effective site runs after an identity-map pass in apply, so applying the
// rewrite twice must equal applying it once.
func TestRewriteCustomVariableRefs_Idempotent(t *testing.T) {
	build := func() map[string]any {
		return map[string]any{
			"expression":      "inputs['task_outputs.tablas.gasto']",
			"dependencies":    []any{"task_outputs.ola1.x"},
			"dependencyTypes": map[string]any{"task_outputs.ola1.x": "number"},
			"simpleConfig": map[string]any{
				"formulaText": "$task_outputs.tablas.gasto + $task_outputs.ola1.x",
			},
		}
	}
	once, twice := build(), build()
	rewriteCustomVariableRefs(once, testRefMap())
	rewriteCustomVariableRefs(twice, testRefMap())
	rewriteCustomVariableRefs(twice, testRefMap())
	if !reflect.DeepEqual(once, twice) {
		t.Errorf("not idempotent\nonce  %v\ntwice %v", once, twice)
	}
}

// Anything nested under a future field is covered by the value walk, which is
// the point of walking instead of naming fields.
func TestRewriteCustomVariableRefs_NestedUnknownField(t *testing.T) {
	v := map[string]any{
		"algoNuevo": map[string]any{
			"lista": []any{"task_outputs.tablas.gasto", 42, nil},
		},
	}
	rewriteCustomVariableRefs(v, testRefMap())
	lista := v["algoNuevo"].(map[string]any)["lista"].([]any)
	if lista[0] != "task_outputs.tablas-de-pol-tica-22503b.gasto" {
		t.Errorf("nested string not rewritten: %v", lista[0])
	}
	if lista[1] != 42 || lista[2] != nil {
		t.Errorf("non-string values mangled: %v", lista)
	}
}
