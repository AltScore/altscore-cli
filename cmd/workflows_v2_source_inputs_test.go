package cmd

import "testing"

func TestAltdataFieldRequired(t *testing.T) {
	cases := []struct {
		name string
		fm   map[string]any
		want bool
	}{
		{"string REQUIRED", map[string]any{"required": "REQUIRED"}, true},
		{"string OPTIONAL", map[string]any{"required": "OPTIONAL"}, false},
		{"string optional lowercase", map[string]any{"required": "optional"}, false},
		{"bool true", map[string]any{"required": true}, true},
		{"bool false", map[string]any{"required": false}, false},
		{"absent defaults to required", map[string]any{}, true},
	}
	for _, c := range cases {
		if got := altdataFieldRequired(c.fm); got != c.want {
			t.Errorf("%s: altdataFieldRequired = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAltdataRequiredFieldSatisfied(t *testing.T) {
	cases := []struct {
		name          string
		field         string
		inputKeys     map[string]any
		inputMappings map[string]any
		want          bool
	}{
		{
			name:          "mapped directly by field name",
			field:         "client_id",
			inputKeys:     map[string]any{},
			inputMappings: map[string]any{"client_id": "inputs.client_id"},
			want:          true,
		},
		{
			name:          "inputKeys var is mapped",
			field:         "client_id",
			inputKeys:     map[string]any{"client_id": "{{client_id}}"},
			inputMappings: map[string]any{"client_id": "inputs.client_id"},
			want:          true,
		},
		{
			name:          "ERP-MINSA shape: inputKeys present, inputMappings empty",
			field:         "client_id",
			inputKeys:     map[string]any{"client_id": "{{client_id}}"},
			inputMappings: map[string]any{},
			want:          false,
		},
		{
			name:          "field absent from both",
			field:         "client_id",
			inputKeys:     map[string]any{},
			inputMappings: map[string]any{},
			want:          false,
		},
		{
			name:          "literal constant in inputKeys is supplied",
			field:         "country",
			inputKeys:     map[string]any{"country": "MX"},
			inputMappings: map[string]any{},
			want:          true,
		},
		{
			name:          "builtin placeholder needs no mapping",
			field:         "sid",
			inputKeys:     map[string]any{"sid": "{{source_id}}"},
			inputMappings: map[string]any{},
			want:          true,
		},
		{
			name:          "non-identity remap, var mapped",
			field:         "personId",
			inputKeys:     map[string]any{"personId": "{{cedula}}"},
			inputMappings: map[string]any{"cedula": "inputs.cedula"},
			want:          true,
		},
		{
			name:          "non-identity remap, var unmapped",
			field:         "personId",
			inputKeys:     map[string]any{"personId": "{{cedula}}"},
			inputMappings: map[string]any{},
			want:          false,
		},
	}
	for _, c := range cases {
		if got := altdataRequiredFieldSatisfied(c.field, c.inputKeys, c.inputMappings); got != c.want {
			t.Errorf("%s: altdataRequiredFieldSatisfied = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestLookupAltdataSourceRequirements_FromCache(t *testing.T) {
	key := "TEST-SRC|v1"
	altdataSourceStatusCache[key] = map[string]any{
		"sourceId":      "TEST-SRC",
		"sourceVersion": "v1",
		"inputFields": []any{
			map[string]any{"field": "client_id", "required": "REQUIRED"},
			map[string]any{"field": "note", "required": "OPTIONAL"},
			map[string]any{"field": "tax_id", "required": "AT_LEAST_ONE"},
			map[string]any{"field": "flag", "required": true},
		},
	}
	defer delete(altdataSourceStatusCache, key)

	got, err := lookupAltdataSourceRequirements(nil, "TEST-SRC", "v1", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []altdataRequirement{{"client_id", "REQUIRED"}, {"tax_id", "AT_LEAST_ONE"}, {"flag", "REQUIRED"}}
	if len(got) != len(want) {
		t.Fatalf("requirements = %v, want %v (OPTIONAL left out)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("requirement %d = %v, want %v", i, got[i], want[i])
		}
	}
}

// AT_LEAST_ONE inputs used to be reported one by one as if each were required, and
// REQUIRED_IF_PARENT ones always: both read as "blocks publish" on a correctly wired task.
func TestUnmetAltdataRequirements(t *testing.T) {
	reqs := []altdataRequirement{
		{"client_id", "REQUIRED"},
		{"tax_id", "AT_LEAST_ONE"}, {"person_id", "AT_LEAST_ONE"},
		{"passport", "EXCLUSIVE_ONE"}, {"national_id", "EXCLUSIVE_ONE"},
		{"spouse_id", "REQUIRED_IF_PARENT"},
	}
	fedAll := map[string]any{"client_id": "inputs.c", "person_id": "inputs.p", "national_id": "inputs.n"}
	if got := unmetAltdataRequirements(reqs, nil, fedAll); len(got) != 0 {
		t.Errorf("one member per group fed and the required input fed must report nothing, got %v", got)
	}
	got := unmetAltdataRequirements(reqs, nil, map[string]any{})
	want := []string{"client_id", "one-of:tax_id,person_id", "one-of:passport,national_id"}
	if len(got) != len(want) {
		t.Fatalf("nothing fed: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %q, want %q", i, got[i], want[i])
		}
	}
	for _, g := range got {
		if g == "spouse_id" {
			t.Error("a REQUIRED_IF_PARENT input depends on another value and must never be reported")
		}
	}
}
