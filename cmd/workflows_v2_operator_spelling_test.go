package cmd

import (
	"strings"
	"testing"
)

// The served guide teaches camelCase spellings the vocabulary only has in snake_case, and the
// backend evaluates an unknown operator to False without an error.
func TestCamelCaseOperatorIsRewrittenToItsSnakeCaseForm(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"isNotEmpty", "is_not_empty"},
		{"isEmpty", "is_empty"},
		{"isTrue", "is_true"},
		{"notContains", "not_contains"},
		{"isNull", "isNull"}, // a compiled alias stays as written
		{"is_not_empty", "is_not_empty"},
	} {
		cond := map[string]any{"field": "inputs.x", "operator": tc.in, "value": nil}
		if err := validateConditionGroup(cond, "conditions"); err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		if cond["operator"] != tc.want {
			t.Errorf("%s: operator became %v, want %s", tc.in, cond["operator"], tc.want)
		}
	}
}

func TestUnknownOperatorSuggestsTheClosestOne(t *testing.T) {
	cond := map[string]any{"field": "inputs.x", "operator": "notEmpty"}
	err := validateConditionGroup(cond, "conditions")
	if err == nil {
		t.Fatal("notEmpty is no operator in either spelling and must be refused")
	}
	if !strings.Contains(err.Error(), `did you mean "is_not_empty"?`) {
		t.Errorf("expected a suggestion, got %v", err)
	}
	if cond["operator"] != "notEmpty" {
		t.Errorf("a refused operator must not be rewritten, got %v", cond["operator"])
	}
	if s := didYouMeanOperator("zzzzzzzz"); s != "" {
		t.Errorf("nothing close means no suggestion, got %q", s)
	}
}
