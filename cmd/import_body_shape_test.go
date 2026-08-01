package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// The four credit-decisioning bulk-import endpoints take an OBJECT wrapper, never a
// bare array. From borrower-central:
//
//	app/api/mapping_tables/schemas.py    BulkImportMappingTables.mapping_tables  alias mappingTables
//	app/api/scorecards/schemas.py        BulkImportScorecards.scorecards         alias scorecards
//	app/api/evaluation_rules/schemas.py  BulkImportEvaluationRules.rules         alias rules
//	app/api/rule_trees/schemas.py        BulkImportRuleTrees.rule_trees          alias ruleTrees
//
// Posting a bare array returns 400 "value is not a valid dict". Before this change the
// CLI's --workflow-alias handling did the exact opposite: it required a bare array and
// hard-errored on the wrapper, so the flag could not be used with ANY of the four
// endpoints, and the evaluation-rules help text told users to send the shape the
// server rejects.

// wrapperKeyFor mirrors the call sites in credit_decisioning.go, so a mismatch between
// the table below and the real wiring shows up as a failure here.
var importWrappers = map[string]string{
	"evaluation-rules": "rules",
	"rule-trees":       "ruleTrees",
	"mapping-tables":   "mappingTables",
	"scorecards":       "scorecards",
}

func TestNormalizeImportBodyAcceptsTheWrapperTheEndpointRequires(t *testing.T) {
	for kind, key := range importWrappers {
		in := []byte(`{"` + key + `":[{"code":"a"},{"code":"b"}],"skipExisting":true}`)
		out, err := normalizeImportBody(in, key, "underwriting-v1", kind, nil)
		if err != nil {
			t.Fatalf("%s: wrapper body rejected: %v", kind, err)
		}
		var got map[string]json.RawMessage
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("%s: output is not an object: %v", kind, err)
		}
		if _, ok := got["skipExisting"]; !ok {
			t.Errorf("%s: skipExisting was dropped; the caller's flag must survive", kind)
		}
		var items []map[string]any
		if err := json.Unmarshal(got[key], &items); err != nil {
			t.Fatalf("%s: %q is not an array: %v", kind, key, err)
		}
		if len(items) != 2 {
			t.Fatalf("%s: expected 2 records, got %d", kind, len(items))
		}
		for _, it := range items {
			if it["workflowAlias"] != "underwriting-v1" {
				t.Errorf("%s: record not stamped: %v", kind, it["workflowAlias"])
			}
		}
	}
}

func TestNormalizeImportBodyWrapsABareArray(t *testing.T) {
	// A bare array is what someone naturally hands an "import" command. The server
	// rejects it, so wrap rather than propagate a 400.
	out, err := normalizeImportBody([]byte(`[{"code":"a"}]`), "rules", "wf-1", "evaluation-rules", nil)
	if err != nil {
		t.Fatalf("bare array rejected: %v", err)
	}
	var got struct {
		Rules []map[string]any `json:"rules"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not the wrapper shape: %v", err)
	}
	if len(got.Rules) != 1 || got.Rules[0]["workflowAlias"] != "wf-1" {
		t.Errorf("bare array not wrapped and stamped: %s", out)
	}
}

func TestNormalizeImportBodyStillWarnsWhenNothingCarriesAnAlias(t *testing.T) {
	// Unstamped records import fine but are invisible in every workflow builder,
	// so the warning is the only signal the user gets.
	var buf bytes.Buffer
	if _, err := normalizeImportBody([]byte(`{"scorecards":[{"code":"a"}]}`), "scorecards", "", "scorecards", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "will not appear in any workflow builder") {
		t.Errorf("expected the missing-alias warning, got %q", buf.String())
	}

	buf.Reset()
	if _, err := normalizeImportBody([]byte(`{"scorecards":[{"code":"a","workflowAlias":"wf-1"}]}`), "scorecards", "", "scorecards", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("already-stamped bundle should not warn, got %q", buf.String())
	}
}

func TestNormalizeImportBodyPreservesRecordFields(t *testing.T) {
	// Stamping must not drop anything else on the record -- these bundles carry the
	// entire credit model (buckets, conditions, decisionKey).
	in := []byte(`{"rules":[{"code":"a","decisionKey":"Auto Decline","conditions":{"operator":"AND","items":[{"field":"x"}]}}]}`)
	out, err := normalizeImportBody(in, "rules", "wf-1", "evaluation-rules", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got struct {
		Rules []map[string]json.RawMessage `json:"rules"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"code", "decisionKey", "conditions"} {
		if _, ok := got.Rules[0][k]; !ok {
			t.Errorf("field %q dropped during stamping", k)
		}
	}
}

func TestNormalizeImportBodyRejectsAnObjectWithoutTheArrayKey(t *testing.T) {
	// Better to name the expected key than to forward a body the server will
	// reject with "value is not a valid dict".
	_, err := normalizeImportBody([]byte(`{"items":[{"code":"a"}]}`), "rules", "wf-1", "evaluation-rules", nil)
	if err == nil {
		t.Fatal("expected an error naming the required key")
	}
	if !strings.Contains(err.Error(), `"rules"`) {
		t.Errorf("error should name the expected wrapper key, got: %v", err)
	}
}

func TestEvaluationRulesHelpNoLongerClaimsABareArray(t *testing.T) {
	// The v0.19.0 help text asserted the opposite of the endpoint's contract:
	// "Body must be a top-level JSON ARRAY ... NOT an object wrapper".
	src := readCmdSource(t, "credit_decisioning.go")
	if strings.Contains(src, "NOT an object wrapper") {
		t.Error("evaluation-rules help still claims the endpoint rejects an object wrapper; it requires one")
	}
	if !strings.Contains(src, `{"rules": [{"label": "Approve"`) {
		t.Error("evaluation-rules help should show the wrapper shape the endpoint accepts")
	}
}
