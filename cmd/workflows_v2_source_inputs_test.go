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

func TestLookupAltdataSourceRequiredFields_FromCache(t *testing.T) {
	// Pre-seed the per-run source-status cache so no API client is needed.
	key := "TEST-SRC|v1"
	altdataSourceStatusCache[key] = map[string]any{
		"sourceId":      "TEST-SRC",
		"sourceVersion": "v1",
		"inputFields": []any{
			map[string]any{"field": "client_id", "required": "REQUIRED"},
			map[string]any{"field": "note", "required": "OPTIONAL"},
		},
	}
	defer delete(altdataSourceStatusCache, key)

	got, err := lookupAltdataSourceRequiredFields(nil, "TEST-SRC", "v1", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0] != "client_id" {
		t.Errorf("required fields = %v, want [client_id]", got)
	}
}
