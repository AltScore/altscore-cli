package cmd

import (
	"bytes"
	"strings"
	"testing"
)

func varDef(t string) map[string]any {
	v := map[string]any{"expression": "_o = 1", "returnValue": "_o"}
	if t != "" {
		v["type"] = t
	}
	return v
}

func TestStampEnforceTypeOnlyTouchesNewTypedVariables(t *testing.T) {
	spec := map[string]any{
		"brand_new":  varDef("integer"),
		"untyped":    varDef(""),
		"already":    varDef("number"),
		"explicitly": map[string]any{"type": "string", "enforceType": false},
	}
	existing := map[string]any{"already": varDef("number")}

	var warn bytes.Buffer
	stampEnforceTypeOnNewVariables(spec, existing, &warn)

	if spec["brand_new"].(map[string]any)["enforceType"] != true {
		t.Error("a new typed variable should be enforced")
	}
	if _, ok := spec["untyped"].(map[string]any)["enforceType"]; ok {
		t.Error("a variable with no declared type has nothing to enforce")
	}
	if _, ok := spec["already"].(map[string]any)["enforceType"]; ok {
		t.Error("a variable already live keeps its unenforced history")
	}
	if spec["explicitly"].(map[string]any)["enforceType"] != false {
		t.Error("an explicit enforceType:false must not be overwritten")
	}
	if !strings.Contains(warn.String(), "brand_new") {
		t.Errorf("the stamp should be reported, got %q", warn.String())
	}
	if strings.Contains(warn.String(), "already") {
		t.Errorf("only newly stamped names should be listed, got %q", warn.String())
	}
}

// A re-apply must not weaken a variable's contract: the live enforceType is
// carried forward when the spec is silent, whatever its value.
func TestStampEnforceTypeCarriesLiveEnforcementForward(t *testing.T) {
	spec := map[string]any{
		"enforced":   varDef("number"),
		"relaxed":    varDef("number"),
		"unstamped":  varDef("number"),
		"overridden": map[string]any{"type": "number", "enforceType": false},
	}
	existing := map[string]any{
		"enforced":   map[string]any{"type": "number", "enforceType": true},
		"relaxed":    map[string]any{"type": "number", "enforceType": false},
		"unstamped":  varDef("number"),
		"overridden": map[string]any{"type": "number", "enforceType": true},
	}
	var warn bytes.Buffer
	stampEnforceTypeOnNewVariables(spec, existing, &warn)

	if spec["enforced"].(map[string]any)["enforceType"] != true {
		t.Error("a live enforceType:true must survive a re-apply that omits the field")
	}
	if spec["relaxed"].(map[string]any)["enforceType"] != false {
		t.Error("a live enforceType:false must be carried forward as-is, not upgraded")
	}
	if _, ok := spec["unstamped"].(map[string]any)["enforceType"]; ok {
		t.Error("a live variable that never had enforceType must not gain one")
	}
	if spec["overridden"].(map[string]any)["enforceType"] != false {
		t.Error("an explicit spec value still wins over the live one")
	}
	if warn.Len() != 0 {
		t.Errorf("carrying live state forward is not a stamp and must not be reported, got %q", warn.String())
	}
}

func TestStampEnforceTypeOnCreatePathStampsEverythingTyped(t *testing.T) {
	spec := map[string]any{"a": varDef("number"), "b": varDef("boolean")}
	stampEnforceTypeOnNewVariables(spec, nil, nil)
	for _, k := range []string{"a", "b"} {
		if spec[k].(map[string]any)["enforceType"] != true {
			t.Errorf("%s should be enforced on a brand-new workflow", k)
		}
	}
}

func TestLiveCustomVariablesTolerantOfShape(t *testing.T) {
	if liveCustomVariables(nil) != nil {
		t.Error("nil workflow -> nil")
	}
	if liveCustomVariables(map[string]any{}) != nil {
		t.Error("missing key -> nil")
	}
	if liveCustomVariables(map[string]any{"customVariables": "nope"}) != nil {
		t.Error("wrong shape -> nil, never a panic")
	}
	got := liveCustomVariables(map[string]any{"customVariables": map[string]any{"x": 1}})
	if len(got) != 1 {
		t.Errorf("expected the map through, got %v", got)
	}
}
