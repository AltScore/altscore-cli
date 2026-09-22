package cmd

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

func TestRewriteRefsInTaskTemplates_EndPdfConfig(t *testing.T) {
	refMap := map[string]string{
		"score": "scorecard-059a48",
		"tree":  "rbol-de-decisi-n-55e977",
	}
	task := map[string]any{
		"type": "end",
		"endConfig": map[string]any{
			"outputJson": "",
			"pdfConfig": map[string]any{
				"enabled": true,
				"sourcesConfig": []any{
					map[string]any{"type": "htmlBlock"},
					map[string]any{"type": "scorecard", "taskAlias": "score"},
					map[string]any{"type": "rule-tree", "taskAlias": "tree"},
					map[string]any{"type": "scorecard", "taskAlias": "scorecard-already-server"},
				},
			},
		},
	}
	if err := rewriteRefsInTaskTemplates(task, refMap); err != nil {
		t.Fatalf("rewriteRefsInTaskTemplates: %v", err)
	}
	endCfg := task["endConfig"].(map[string]any)
	pdfCfg := endCfg["pdfConfig"].(map[string]any)
	sources := pdfCfg["sourcesConfig"].([]any)
	cases := []struct {
		idx  int
		want string
		key  string
	}{
		{1, "scorecard-059a48", "taskAlias"},
		{2, "rbol-de-decisi-n-55e977", "taskAlias"},
		{3, "scorecard-already-server", "taskAlias"},
	}
	for _, c := range cases {
		got, _ := sources[c.idx].(map[string]any)[c.key].(string)
		if got != c.want {
			t.Errorf("sources[%d].%s = %q, want %q", c.idx, c.key, got, c.want)
		}
	}
	if _, has := sources[0].(map[string]any)["taskAlias"]; has {
		t.Errorf("sources[0] should not have gained a taskAlias key")
	}
}

func TestValidateNoResidualSpecRefs(t *testing.T) {
	refMap := map[string]string{
		"score": "scorecard-059a48",
		"fetch": "altdata-enrichment-1234",
	}
	t.Run("clean body passes", func(t *testing.T) {
		body := map[string]any{
			"type":  "end",
			"label": "Fin",
			"endConfig": map[string]any{
				"pdfConfig": map[string]any{
					"sourcesConfig": []any{
						map[string]any{"type": "scorecard", "taskAlias": "scorecard-059a48"},
					},
				},
			},
		}
		if err := validateNoResidualSpecRefs(body, refMap, "test"); err != nil {
			t.Errorf("unexpected error on clean body: %v", err)
		}
	})
	t.Run("residual ref at unknown path fails", func(t *testing.T) {
		body := map[string]any{
			"type":            "end",
			"someNewRefField": "score",
		}
		err := validateNoResidualSpecRefs(body, refMap, "test")
		if err == nil {
			t.Fatal("expected validator to flag residual ref")
		}
		msg := err.Error()
		if !strings.Contains(msg, "someNewRefField") {
			t.Errorf("error should name the offending path; got: %v", err)
		}
		if !strings.Contains(msg, "scorecard-059a48") {
			t.Errorf("error should name the expected server alias; got: %v", err)
		}
	})
	t.Run("excluded user-text fields pass", func(t *testing.T) {
		body := map[string]any{
			"label":       "score",
			"description": "Computes score",
			"comment":     "fetch upstream",
			"endConfig": map[string]any{
				"pdfConfig": map[string]any{
					"title":    "score breakdown",
					"subtitle": "fetch results",
				},
			},
		}
		if err := validateNoResidualSpecRefs(body, refMap, "test"); err != nil {
			t.Errorf("excluded fields should not trip validator: %v", err)
		}
	})
	t.Run("residual ref inside array element fails", func(t *testing.T) {
		body := map[string]any{
			"endConfig": map[string]any{
				"pdfConfig": map[string]any{
					"sourcesConfig": []any{
						map[string]any{"type": "scorecard", "taskAlias": "score"},
					},
				},
			},
		}
		err := validateNoResidualSpecRefs(body, refMap, "test")
		if err == nil {
			t.Fatal("expected validator to flag ref nested in array")
		}
		if !strings.Contains(err.Error(), "sourcesConfig[0].taskAlias") {
			t.Errorf("error should include bracketed path; got: %v", err)
		}
	})
	t.Run("empty refMap is a no-op", func(t *testing.T) {
		body := map[string]any{"taskAlias": "score"}
		if err := validateNoResidualSpecRefs(body, map[string]string{}, "test"); err != nil {
			t.Errorf("empty refMap should short-circuit cleanly: %v", err)
		}
	})
}

func TestDetectLegacySpecShape_RejectsTasksKey(t *testing.T) {
	body := []byte(`{
		"label": "x",
		"category": "EVALUATION",
		"tasks": [{"ref":"fetch","type":"altdata-enrichment"}]
	}`)
	err := detectLegacySpecShape(body)
	if err == nil {
		t.Fatal("expected error rejecting tasks[] input; got nil")
	}
	msg := err.Error()
	for _, want := range []string{"tasks[] (1 entries)", "nodes[]", "before:", "after:"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error missing %q; got: %s", want, msg)
		}
	}
}

func TestDetectLegacySpecShape_RejectsExtraNodesKey(t *testing.T) {
	body := []byte(`{
		"label": "x",
		"extraNodes": [{"ref":"start","type":"start"},{"ref":"end","type":"end"}]
	}`)
	err := detectLegacySpecShape(body)
	if err == nil {
		t.Fatal("expected error rejecting extraNodes[] input; got nil")
	}
	if !strings.Contains(err.Error(), "extraNodes[] (2 entries)") {
		t.Errorf("expected entry count in error; got: %s", err.Error())
	}
}

func TestDetectLegacySpecShape_RejectsBoth(t *testing.T) {
	body := []byte(`{
		"tasks":      [{"ref":"a","type":"altdata-enrichment"}],
		"extraNodes": [{"ref":"start","type":"start"}]
	}`)
	err := detectLegacySpecShape(body)
	if err == nil {
		t.Fatal("expected error; got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "tasks[] (1 entries) and extraNodes[] (1 entries)") {
		t.Errorf("expected combined citation; got: %s", msg)
	}
}

func TestDetectLegacySpecShape_PassesNodesShape(t *testing.T) {
	body := []byte(`{
		"label": "x",
		"nodes": [
			{"ref":"start","type":"start"},
			{"ref":"fetch","type":"altdata-enrichment"},
			{"ref":"end","type":"end"}
		]
	}`)
	if err := detectLegacySpecShape(body); err != nil {
		t.Fatalf("nodes[] spec should pass detector; got: %v", err)
	}
}

func TestDetectLegacySpecShape_IgnoresEmptyLegacyArrays(t *testing.T) {
	body := []byte(`{"tasks": [], "extraNodes": [], "nodes": [{"ref":"start","type":"start"}]}`)
	if err := detectLegacySpecShape(body); err != nil {
		t.Fatalf("empty legacy arrays should be tolerated; got: %v", err)
	}
}

func TestDetectLegacySpecShape_IgnoresMalformedJSON(t *testing.T) {
	if err := detectLegacySpecShape([]byte("not json")); err != nil {
		t.Errorf("malformed JSON should defer to caller's unmarshal; got: %v", err)
	}
}

func TestRewriteRefsInTaskTemplates_EndStandardOutput(t *testing.T) {
	refMap := map[string]string{
		"score": "scorecard-059a48",
		"tree":  "rbol-de-decisi-n-55e977",
	}
	task := map[string]any{
		"type": "end",
		"endConfig": map[string]any{
			"standardOutput": map[string]any{
				"enabled":   true,
				"scorecard": "{{task_outputs.score.rows}}",
				"rules":     "{{task_outputs.tree.rules}}",
				"decision":  "{{task_outputs.score.decision}}",
				"score": map[string]any{
					"key":      "score",
					"label":    "Score",
					"value":    "{{task_outputs.score.total}}",
					"maxValue": float64(1000),
				},
				"fields": map[string]any{
					"tax_id": "{{inputs.tax_id}}",
					"note":   "approved",
				},
			},
		},
	}
	if err := rewriteRefsInTaskTemplates(task, refMap); err != nil {
		t.Fatalf("rewriteRefsInTaskTemplates: %v", err)
	}
	std := task["endConfig"].(map[string]any)["standardOutput"].(map[string]any)
	if got := std["scorecard"]; got != "{{task_outputs.scorecard-059a48.rows}}" {
		t.Errorf("scorecard = %q, want rewritten server alias", got)
	}
	if got := std["rules"]; got != "{{task_outputs.rbol-de-decisi-n-55e977.rules}}" {
		t.Errorf("rules = %q, want rewritten server alias", got)
	}
	if got := std["decision"]; got != "{{task_outputs.scorecard-059a48.decision}}" {
		t.Errorf("decision = %q", got)
	}
	score := std["score"].(map[string]any)
	if got := score["value"]; got != "{{task_outputs.scorecard-059a48.total}}" {
		t.Errorf("score.value = %q, want rewritten", got)
	}
	if got := score["key"]; got != "score" {
		t.Errorf("score.key literal changed: %q", got)
	}
	fields := std["fields"].(map[string]any)
	if got := fields["note"]; got != "approved" {
		t.Errorf("fields.note literal changed: %q", got)
	}
	if got := fields["tax_id"]; got != "{{inputs.tax_id}}" {
		t.Errorf("fields.tax_id = %q, want unchanged", got)
	}
}

func TestRewriteRefsInTaskTemplates_EndStandardOutputUnknownRef(t *testing.T) {
	task := map[string]any{
		"type": "end",
		"endConfig": map[string]any{
			"standardOutput": map[string]any{
				"scorecard": "{{task_outputs.nonexistent.rows}}",
			},
		},
	}
	err := rewriteRefsInTaskTemplates(task, map[string]string{"score": "scorecard-1"})
	if err == nil {
		t.Fatal("expected error for unknown standardOutput ref, got nil")
	}
	if !strings.Contains(err.Error(), "standardOutput.scorecard") {
		t.Errorf("error should name the offending field, got: %v", err)
	}
}

func TestRewriteRefsInTaskTemplates_EndStandardOutputNullIsUnset(t *testing.T) {
	endCfg := map[string]any{
		"outputJson":     `{"s":"{{task_outputs.score.rows}}"}`,
		"standardOutput": nil,
		"decisionConfig": nil,
	}
	task := map[string]any{"type": "end", "endConfig": endCfg}
	if err := rewriteRefsInTaskTemplates(task, map[string]string{"score": "score"}); err != nil {
		t.Fatalf("a null standardOutput must be accepted as unset, got: %v", err)
	}
	if v, present := endCfg["standardOutput"]; !present || v != nil {
		t.Errorf("null standardOutput must survive as null, got present=%v value=%v", present, v)
	}
	if _, err := json.Marshal(task); err != nil {
		t.Fatalf("body must still marshal: %v", err)
	}
}

func TestValidateStandardOutputShape(t *testing.T) {
	cases := []struct {
		name    string
		std     map[string]any
		wantErr string
	}{
		{"valid", map[string]any{"enabled": true, "scorecard": "{{task_outputs.s.rows}}", "score": map[string]any{"value": "{{task_outputs.s.t}}"}, "fields": map[string]any{"a": "b"}}, ""},
		{"enabled not bool", map[string]any{"enabled": "yes"}, "enabled must be a boolean"},
		{"section is list", map[string]any{"scorecard": []any{1, 2}}, "scorecard must be a variable template string"},
		{"unknown top key", map[string]any{"scoreCard": "x"}, `unknown field "scoreCard"`},
		{"score not object", map[string]any{"score": "x"}, "score must be an object"},
		{"score unknown key", map[string]any{"score": map[string]any{"maximum": 1}}, `score has unknown field "maximum"`},
		{"fields not object", map[string]any{"fields": []any{}}, "fields must be an object"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateStandardOutputShape(c.std)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("want error containing %q, got %v", c.wantErr, err)
			}
		})
	}
}

func TestCheckWorkflowAlias(t *testing.T) {
	cases := []struct {
		name    string
		alias   string
		wantErr string
	}{
		{"omitted -- server slugifies the label", "", ""},
		{"kebab", "over-the-road-v2", ""},
		{"single word", "kyb", ""},
		{"leading digit", "0089-kyb", ""},
		{"exactly 100 chars", strings.Repeat("a", 100), ""},
		{"underscores", "over_the_road_v2", "over-the-road-v2"},
		{"uppercase", "OverTheRoad", "not kebab-case"},
		{"spaces", "over the road", "over-the-road"},
		{"leading dash", "-otr", "not kebab-case"},
		{"slash", "otr/v2", "otr-v2"},
		{"101 chars", strings.Repeat("a", 101), "not kebab-case"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkWorkflowAlias(c.alias)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("checkWorkflowAlias(%q) = %v, want nil", c.alias, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkWorkflowAlias(%q) = nil, want an error", c.alias)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error should contain %q, got: %v", c.wantErr, err)
			}
			if !strings.Contains(err.Error(), "^[a-z0-9][a-z0-9-]*$") {
				t.Errorf("error should quote the alias regex, got: %v", err)
			}
		})
	}
}

func TestPreflightTasks_RejectsNonKebabWorkflowAlias(t *testing.T) {
	spec := &composeSpec{
		Label:      "Over the road v2",
		Alias:      "over_the_road_v2",
		Category:   "EVALUATION",
		ExtraNodes: startNode,
		Tasks: []map[string]any{
			{"ref": "end", "type": "end", "label": "End"},
		},
		Edges: []map[string]any{{"from": "start", "to": "end"}},
	}
	err := preflightTasks(spec)
	if err == nil {
		t.Fatalf("preflight must reject a non-kebab-case workflow alias before any task is POSTed")
	}
	if !strings.Contains(err.Error(), "over_the_road_v2") || !strings.Contains(err.Error(), "over-the-road-v2") {
		t.Errorf("error should name the bad alias and suggest the kebab form, got: %v", err)
	}
}

func TestPreflightTasks_RefVariableCollision(t *testing.T) {
	cases := []struct {
		name    string
		inputs  map[string]any
		customs map[string]any
		wantErr string
	}{
		{
			name:    "no collision",
			inputs:  map[string]any{"raw_score": map[string]any{"type": "number"}},
			wantErr: "",
		},
		{
			name:    "ref collides with an input variable",
			inputs:  map[string]any{"score": map[string]any{"type": "number"}},
			wantErr: `"score" (workflow.inputVariables)`,
		},
		{
			name:    "ref collides with a custom variable",
			customs: map[string]any{"score": map[string]any{"expression": "result = 1"}},
			wantErr: `"score" (workflow.customVariables)`,
		},
		{
			name:    "collision on a graph-only node ref",
			inputs:  map[string]any{"start": map[string]any{"type": "string"}},
			wantErr: `"start" (workflow.inputVariables)`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec := &composeSpec{
				Label:           "Scoring engine",
				Category:        "EVALUATION",
				InputVariables:  c.inputs,
				CustomVariables: c.customs,
				ExtraNodes:      startNode,
				Tasks: []map[string]any{
					{"ref": "score", "type": "http", "label": "Score", "url": "https://x.test", "method": "GET"},
					{"ref": "end", "type": "end", "label": "End"},
				},
				Edges: []map[string]any{
					{"from": "start", "to": "score"},
					{"from": "score", "to": "end"},
				},
			}
			err := preflightTasks(spec)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("preflight should accept a spec with no ref/variable collision, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("preflight must reject the ref/variable collision before any task is POSTed")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error should name the colliding name and its scope %q, got: %v", c.wantErr, err)
			}
			if !strings.Contains(err.Error(), "Rename one of the two") {
				t.Errorf("error should tell the author how to fix it, got: %v", err)
			}
		})
	}
}

func TestCheckRefVariableCollisions_ReportsAllSorted(t *testing.T) {
	spec := &composeSpec{
		InputVariables:  map[string]any{"zeta": map[string]any{}},
		CustomVariables: map[string]any{"alpha": map[string]any{}},
	}
	refs := map[string]bool{"alpha": true, "zeta": true, "unrelated": true}
	for i := 0; i < 20; i++ {
		err := checkRefVariableCollisions(spec, refs)
		if err == nil {
			t.Fatalf("both collisions must be reported")
		}
		want := `"alpha" (workflow.customVariables), "zeta" (workflow.inputVariables)`
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("collisions must be listed sorted; want %q in: %v", want, err)
		}
	}
}

func TestRewriteTaskRefs_MappingTableInputVariableModes(t *testing.T) {
	refMap := map[string]string{"score": "score-service-49dcec"}
	task := map[string]any{
		"type":          "mapping-table",
		"label":         "Band",
		"inputMappings": map[string]any{"score": "task_outputs.score.weight"},
		"mappingTableConfig": map[string]any{
			"entries": []any{
				map[string]any{"inputVariable": "score", "outputVariable": "risk_band"},
				map[string]any{"inputVariable": "task_outputs.score.weight", "outputVariable": "weight_band"},
			},
		},
	}
	if err := rewriteTaskRefs(task, refMap, `node ref="metrics-a"`); err != nil {
		t.Fatalf("a bare inputVariable is a context-variable name, not a residual ref: %v", err)
	}
	entries := task["mappingTableConfig"].(map[string]any)["entries"].([]any)
	if got := entries[0].(map[string]any)["inputVariable"]; got != "score" {
		t.Errorf("bare inputVariable must survive verbatim, got %v", got)
	}
	if got := entries[1].(map[string]any)["inputVariable"]; got != "task_outputs.score-service-49dcec.weight" {
		t.Errorf("dotted inputVariable must be rewritten to the server alias, got %v", got)
	}
}

func TestValidateNoResidualSpecRefs_StillCatchesRealResidues(t *testing.T) {
	refMap := map[string]string{"score": "score-service-49dcec"}
	body := map[string]any{
		"type": "end",
		"endConfig": map[string]any{
			"pdfConfig": map[string]any{
				"someFutureField": "score",
			},
		},
	}
	err := validateNoResidualSpecRefs(body, refMap, `node ref="end"`)
	if err == nil {
		t.Fatalf("an un-rewritten ref at an unknown path must still fail loudly")
	}
	if !strings.Contains(err.Error(), "someFutureField") {
		t.Errorf("error should name the offending path, got: %v", err)
	}
}

var refBearingTaskBodies = map[string]map[string]any{
	"http": {
		"url":     "https://api.test/{{task_outputs.a.tax_id}}",
		"body":    `{"id": "{{task_outputs.a.tax_id}}"}`,
		"headers": `{"X-Token": "{{task_outputs.a.token}}"}`,
	},
	"end": {
		"endConfig": map[string]any{
			"outputJson": `{"score": "{{task_outputs.a.total}}"}`,
			"standardOutput": map[string]any{
				"enabled":   true,
				"scorecard": "{{task_outputs.a.rows}}",
				"score":     map[string]any{"value": "{{task_outputs.a.total}}"},
				"fields":    map[string]any{"tax_id": "{{task_outputs.a.tax_id}}"},
			},
			"pdfConfig": map[string]any{
				"sourcesConfig": []any{
					map[string]any{"type": "scorecard", "taskAlias": "a"},
				},
			},
		},
	},
	"exception": {"errorMessage": "rejected: {{task_outputs.a.reason}}"},
	"conditional": {
		"branches": []any{
			map[string]any{"conditions": map[string]any{
				"operator": "AND",
				"items": []any{
					map[string]any{"field": "score", "operator": "gt",
						"valueType": "variable", "value": "task_outputs.a.total"},
				},
			}},
		},
	},
	"child-workflow": {"inputExpression": "task_outputs.a.cuit_list"},
	"mapping-table": {
		"mappingTableConfig": map[string]any{
			"entries": []any{
				map[string]any{"inputVariable": "task_outputs.a.weight", "outputVariable": "band"},
			},
		},
	},
	"scorecard": {
		"scorecardConfig": map[string]any{
			"inputMappings": map[string]any{"score": "task_outputs.a.total"},
		},
	},
	"rule-tree": {
		"ruleTreeConfig": map[string]any{
			"inputMappings": map[string]any{"score": "task_outputs.a.total"},
		},
	},
	"category": {
		"categoryConfig": map[string]any{
			"operation":     "assign",
			"categoryKey":   "segmentation",
			"inputMappings": map[string]any{"subcanal": "task_outputs.a.subcanal"},
		},
	},
	"artifact": {
		"artifactConfig": map[string]any{
			"artifactAlias": "usuarios",
			"columns":       []any{"email"},
		},
		"inputMappings": map[string]any{"artifactAlias": "task_outputs.a.alias"},
	},
	"data-store-write": {
		"dataStoreWriteConfig": map[string]any{
			"tableName": "rows",
			"columnMappings": []any{
				map[string]any{"columnName": "uno", "valueTemplate": "{{task_outputs.a.rows[0].uno}}"},
				map[string]any{"columnName": "dos", "valueTemplate": "task_outputs.a.rows"},
			},
			"batchMode":   true,
			"batchSource": "task_outputs.a.rows",
		},
	},
	"data-store-query": {
		"dataStoreQueryConfig": map[string]any{
			"queryMode":     "sql",
			"tableName":     "rows",
			"sql":           "SELECT * FROM rows WHERE uno = '{{task_outputs.a.rows[0].uno}}'",
			"sqlParameters": map[string]any{"uno": "task_outputs.a.rows"},
			"filters": []any{
				map[string]any{"column": "uno", "operator": "eq",
					"valueTemplate": "{{task_outputs.a.rows[0].uno}}"},
			},
		},
	},
	"start":                  {},
	"wait":                   {},
	"evaluate-rules":         {},
	"altdata-enrichment":     {},
	"compute-variables":      {},
	"customer":               {},
	"deal":                   {},
	"credit-line":            {},
	"list-of-similars":       {},
	"asset":                  {},
	"relationships":          {},
	"package-io":             {},
	"sftp":                   {},
	"notices":                {},
	"contact":                {},
	"document-extraction":    {},
	"spreadsheet-extraction": {},
}

func TestRefBearingTaskBodies_CoversEveryTaskType(t *testing.T) {
	for typ := range validTaskTypes {
		if _, has := refBearingTaskBodies[typ]; !has {
			t.Errorf("task type %q has no entry in refBearingTaskBodies -- add one with every "+
				"field of that type the runtime resolves as a reference (an empty map is fine "+
				"when inputMappings is the only such field)", typ)
		}
	}
	for typ := range refBearingTaskBodies {
		if !validTaskTypes[typ] {
			t.Errorf("refBearingTaskBodies has %q which is not in validTaskTypes", typ)
		}
	}
}

func TestRewriteTaskRefs_NoResidualTaskOutputRefs(t *testing.T) {
	const specRef = "a"
	const serverAlias = "a-server-1234"
	types := make([]string, 0, len(refBearingTaskBodies))
	for typ := range refBearingTaskBodies {
		types = append(types, typ)
	}
	sort.Strings(types)
	for _, typ := range types {
		t.Run(typ, func(t *testing.T) {
			task := deepCopyJSON(t, refBearingTaskBodies[typ])
			task["type"] = typ
			task["label"] = "Rep"
			task["inputMappings"] = map[string]any{"upstream": "task_outputs." + specRef + ".field"}

			refMap := map[string]string{specRef: serverAlias}
			if err := rewriteTaskRefs(task, refMap, `node ref="rep"`); err != nil {
				t.Fatalf("rewriteTaskRefs(%s): %v", typ, err)
			}

			encoded, err := json.Marshal(task)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			body := string(encoded)
			if strings.Contains(body, "task_outputs."+specRef+".") {
				t.Errorf("%s: composed body still references the spec-local ref "+
					"(task_outputs.%s.) after rewrite -- a ref-bearing field of this task type is "+
					"not walked by rewriteRefsInTaskTemplates. Body: %s", typ, specRef, body)
			}
			if !strings.Contains(body, serverAlias) {
				t.Errorf("%s: no server alias %q anywhere in the body -- the rewrite did not run. "+
					"Body: %s", typ, serverAlias, body)
			}
		})
	}
}

func deepCopyJSON(t *testing.T, in map[string]any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	if out == nil {
		out = map[string]any{}
	}
	return out
}

func TestRewriteRefsInTaskTemplates_DataStore(t *testing.T) {
	refMap := map[string]string{"q1": "consulta-turso-8f21c3"}

	t.Run("write", func(t *testing.T) {
		task := map[string]any{
			"type": "data-store-write",
			"dataStoreWriteConfig": map[string]any{
				"tableName": "resultados",
				"columnMappings": []any{
					map[string]any{"columnName": "uno", "valueTemplate": "{{task_outputs.q1.rows[0].uno}}"},
					map[string]any{"columnName": "dos", "valueTemplate": "task_outputs.q1.rows"},
					map[string]any{"columnName": "tres", "valueTemplate": "plain_item_key"},
				},
				"batchMode":   true,
				"batchSource": "task_outputs.q1.rows",
			},
		}
		if err := rewriteRefsInTaskTemplates(task, refMap); err != nil {
			t.Fatalf("rewriteRefsInTaskTemplates: %v", err)
		}
		cfg := task["dataStoreWriteConfig"].(map[string]any)
		cols := cfg["columnMappings"].([]any)
		want := []string{
			"{{task_outputs.consulta-turso-8f21c3.rows[0].uno}}",
			"task_outputs.consulta-turso-8f21c3.rows",
			"plain_item_key",
		}
		for i, w := range want {
			if got := cols[i].(map[string]any)["valueTemplate"]; got != w {
				t.Errorf("columnMappings[%d].valueTemplate = %q, want %q", i, got, w)
			}
		}
		if got := cfg["batchSource"]; got != "task_outputs.consulta-turso-8f21c3.rows" {
			t.Errorf("batchSource = %q, want the server alias", got)
		}
	})

	t.Run("query", func(t *testing.T) {
		task := map[string]any{
			"type": "data-store-query",
			"dataStoreQueryConfig": map[string]any{
				"queryMode":     "sql",
				"sql":           "SELECT * FROM t WHERE uno = '{{task_outputs.q1.rows[0].uno}}'",
				"sqlParameters": map[string]any{"uno": "task_outputs.q1.rows", "lit": "acme.inc"},
				"filters": []any{
					map[string]any{"column": "uno", "operator": "eq",
						"valueTemplate": "{{task_outputs.q1.rows[0].uno}}"},
					map[string]any{"column": "dos", "operator": "eq",
						"valueTemplate": "task_outputs.q1.rows"},
				},
			},
		}
		if err := rewriteRefsInTaskTemplates(task, refMap); err != nil {
			t.Fatalf("rewriteRefsInTaskTemplates: %v", err)
		}
		cfg := task["dataStoreQueryConfig"].(map[string]any)
		if got := cfg["sql"]; got != "SELECT * FROM t WHERE uno = '{{task_outputs.consulta-turso-8f21c3.rows[0].uno}}'" {
			t.Errorf("sql = %q, want the server alias interpolated", got)
		}
		params := cfg["sqlParameters"].(map[string]any)
		if got := params["uno"]; got != "task_outputs.consulta-turso-8f21c3.rows" {
			t.Errorf("sqlParameters[uno] = %q, want the server alias", got)
		}
		if got := params["lit"]; got != "acme.inc" {
			t.Errorf("sqlParameters[lit] = %q, want the literal untouched", got)
		}
		filters := cfg["filters"].([]any)
		if got := filters[0].(map[string]any)["valueTemplate"]; got != "{{task_outputs.consulta-turso-8f21c3.rows[0].uno}}" {
			t.Errorf("filters[0].valueTemplate = %q, want rewritten", got)
		}
		if got := filters[1].(map[string]any)["valueTemplate"]; got != "task_outputs.consulta-turso-8f21c3.rows" {
			t.Errorf("filters[1].valueTemplate = %q, want rewritten", got)
		}
	})

	t.Run("unknown ref in a braced data-store template fails loudly", func(t *testing.T) {
		task := map[string]any{
			"type": "data-store-query",
			"dataStoreQueryConfig": map[string]any{
				"sql": "SELECT {{task_outputs.nonexistent.x}}",
			},
		}
		err := rewriteRefsInTaskTemplates(task, refMap)
		if err == nil {
			t.Fatal("expected an error for an unknown ref in dataStoreQueryConfig.sql")
		}
		if !strings.Contains(err.Error(), "dataStoreQueryConfig.sql") {
			t.Errorf("error should name the offending field, got: %v", err)
		}
	})
}

func TestTopologicalTaskOrder_DataStoreTemplateDeps(t *testing.T) {
	cases := []struct {
		name  string
		tasks []map[string]any
	}{
		{
			name: "write batchSource (bare)",
			tasks: []map[string]any{
				{"ref": "write", "type": "data-store-write", "dataStoreWriteConfig": map[string]any{
					"tableName": "t", "batchMode": true, "batchSource": "task_outputs.q1.rows",
				}},
				{"ref": "q1", "type": "data-store-query"},
			},
		},
		{
			name: "write columnMappings valueTemplate (braced)",
			tasks: []map[string]any{
				{"ref": "write", "type": "data-store-write", "dataStoreWriteConfig": map[string]any{
					"tableName": "t", "columnMappings": []any{
						map[string]any{"columnName": "uno", "valueTemplate": "{{task_outputs.q1.rows[0].uno}}"},
					},
				}},
				{"ref": "q1", "type": "data-store-query"},
			},
		},
		{
			name: "query sqlParameters (bare)",
			tasks: []map[string]any{
				{"ref": "q2", "type": "data-store-query", "dataStoreQueryConfig": map[string]any{
					"queryMode": "sql", "sql": "SELECT * FROM t WHERE uno = :uno",
					"sqlParameters": map[string]any{"uno": "task_outputs.q1.rows"},
				}},
				{"ref": "q1", "type": "data-store-query"},
			},
		},
		{
			name: "query filters valueTemplate (braced)",
			tasks: []map[string]any{
				{"ref": "q2", "type": "data-store-query", "dataStoreQueryConfig": map[string]any{
					"tableName": "t", "filters": []any{
						map[string]any{"column": "uno", "operator": "eq",
							"valueTemplate": "{{task_outputs.q1.rows[0].uno}}"},
					},
				}},
				{"ref": "q1", "type": "data-store-query"},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			order, err := topologicalTaskOrder(c.tasks, nil)
			if err != nil {
				t.Fatalf("topologicalTaskOrder: %v", err)
			}
			if len(order) != 2 || order[0] != 1 {
				t.Fatalf("producer must be ordered before the consumer; got %v", order)
			}
		})
	}
}

func TestTemplateDependencyRefs_NoReservedScopeDeps(t *testing.T) {
	task := map[string]any{
		"type": "data-store-query",
		"dataStoreQueryConfig": map[string]any{
			"queryMode":     "sql",
			"sql":           "SELECT t.uno FROM tabla t WHERE t.dos = :dos",
			"sqlParameters": map[string]any{"dos": "acme.inc", "tres": "plain", "inp": "inputs.tax_id"},
		},
	}
	for _, r := range templateDependencyRefs(task) {
		if r == "" || reservedMappingScopes[r] {
			t.Errorf("templateDependencyRefs emitted an invalid dep %q", r)
		}
	}
}

func TestValidateNoResidualSpecRefs_EmbeddedTemplate(t *testing.T) {
	refMap := map[string]string{"q1": "consulta-turso-8f21c3"}

	t.Run("braced residual is flagged", func(t *testing.T) {
		body := map[string]any{
			"type": "data-store-write",
			"dataStoreWriteConfig": map[string]any{
				"columnMappings": []any{
					map[string]any{"columnName": "uno", "valueTemplate": "{{task_outputs.q1.rows[0].uno}}"},
				},
			},
		}
		err := validateNoResidualSpecRefs(body, refMap, "test")
		if err == nil {
			t.Fatal("an embedded spec-local ref must be flagged")
		}
		for _, want := range []string{"columnMappings[0].valueTemplate", "task_outputs.q1.", "consulta-turso-8f21c3"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error should mention %q; got: %v", want, err)
			}
		}
	})

	t.Run("bare dotted residual is flagged", func(t *testing.T) {
		body := map[string]any{"someFutureField": "task_outputs.q1.rows"}
		if err := validateNoResidualSpecRefs(body, refMap, "test"); err == nil {
			t.Fatal("a bare dotted residual must be flagged")
		}
	})

	t.Run("already-rewritten server alias passes", func(t *testing.T) {
		body := map[string]any{
			"dataStoreWriteConfig": map[string]any{
				"columnMappings": []any{
					map[string]any{"valueTemplate": "{{task_outputs.consulta-turso-8f21c3.rows[0].uno}}"},
				},
				"batchSource": "task_outputs.consulta-turso-8f21c3.rows",
			},
			"inputMappings": map[string]any{"x": "task_outputs.consulta-turso-8f21c3.field"},
		}
		if err := validateNoResidualSpecRefs(body, refMap, "test"); err != nil {
			t.Errorf("a rewritten body must not trip the validator: %v", err)
		}
	})

	t.Run("identity refMap is a no-op", func(t *testing.T) {
		body := map[string]any{
			"dataStoreQueryConfig": map[string]any{"sql": "SELECT {{task_outputs.q1.x}}"},
		}
		if err := validateNoResidualSpecRefs(body, map[string]string{"q1": "q1"}, "test"); err != nil {
			t.Errorf("identity refMap must not report a residual: %v", err)
		}
	})

	t.Run("ref boundary is the trailing dot", func(t *testing.T) {
		body := map[string]any{"someFutureField": "{{task_outputs.q1-extended.rows}}"}
		if err := validateNoResidualSpecRefs(body, refMap, "test"); err != nil {
			t.Errorf("a longer alias sharing the ref's prefix must not match: %v", err)
		}
	})

	t.Run("excluded prose fields still pass", func(t *testing.T) {
		body := map[string]any{
			"label":       "reads task_outputs.q1.rows",
			"description": "binds {{task_outputs.q1.rows[0].uno}} into the table",
		}
		if err := validateNoResidualSpecRefs(body, refMap, "test"); err != nil {
			t.Errorf("excluded prose fields must not trip the validator: %v", err)
		}
	})

	t.Run("error names one ref deterministically", func(t *testing.T) {
		multi := map[string]string{"zeta": "zeta-1", "alpha": "alpha-1"}
		body := map[string]any{"someFutureField": "{{task_outputs.zeta.x}} {{task_outputs.alpha.y}}"}
		for i := 0; i < 20; i++ {
			err := validateNoResidualSpecRefs(body, multi, "test")
			if err == nil {
				t.Fatal("expected a residual error")
			}
			if !strings.Contains(err.Error(), `"alpha"`) {
				t.Fatalf("refs must be checked in sorted order so the message is stable; got: %v", err)
			}
		}
	})
}
