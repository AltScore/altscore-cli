package cmd

import (
	"encoding/json"
	"testing"
)

// sampleExportBundle mirrors the /v2/workflows/{id}/export shape: a workflow
// object whose nodes reference tasks by taskAlias, plus a flat tasks[] array
// carrying the full backing task bodies.
const sampleExportBundle = `{
  "exportVersion": "1.0",
  "sourceAlias": "scoring-pipeline",
  "sourceVersion": 4,
  "workflow": {
    "label": "Scoring pipeline",
    "description": "fetch then score",
    "category": "EVALUATION",
    "inputVariables": {"borrower_id": {"type": "string", "required": true}},
    "customVariables": {},
    "nodes": [
      {"nodeId": "start", "type": "start", "label": "Start", "taskAlias": "start-aaaaaa"},
      {"nodeId": "fetch-ecu-111111", "type": "altdata-enrichment", "label": "Fetch ECU",
       "taskAlias": "fetch-ecu-111111", "taskVersion": 1,
       "data": {"inputMappings": {"borrower_id": "inputs.borrower_id"}}},
      {"nodeId": "score-222222", "type": "scorecard", "label": "Score",
       "taskAlias": "score-222222", "taskVersion": 2, "data": {}},
      {"nodeId": "end-333333", "type": "end", "label": "End", "taskAlias": "end-333333"}
    ],
    "edges": [
      {"id": "e1", "sourceNodeId": "start", "targetNodeId": "fetch-ecu-111111"},
      {"id": "e2", "sourceNodeId": "fetch-ecu-111111", "targetNodeId": "score-222222"},
      {"id": "e3", "sourceNodeId": "score-222222", "targetNodeId": "end-333333", "sourceHandle": "out_success", "label": "ok"}
    ]
  },
  "tasks": [
    {"alias": "start-aaaaaa", "type": "start", "label": "Start"},
    {"alias": "fetch-ecu-111111", "type": "altdata-enrichment", "label": "Fetch ECU",
     "sourcesConfig": [{"sourceId": "ECU-PUB-0002", "version": "v1"}],
     "borrowerIdField": "borrower_id",
     "inputMappings": {"borrower_id": "inputs.borrower_id"}},
    {"alias": "score-222222", "type": "scorecard", "label": "Score",
     "scorecardConfig": {"scorecardCode": "sc-001", "inputMappings": {"x": "task_outputs.fetch-ecu-111111.score"}}},
    {"alias": "end-333333", "type": "end", "label": "End",
     "endConfig": {"pdfConfig": {"enabled": true, "sourcesConfig": []}}}
  ],
  "scorecards": [{"code": "sc-001"}]
}`

func TestBundleToApplySpec_Shape(t *testing.T) {
	spec, err := bundleToApplySpec(json.RawMessage(sampleExportBundle))
	if err != nil {
		t.Fatalf("bundleToApplySpec: %v", err)
	}

	if spec["alias"] != "scoring-pipeline" {
		t.Errorf("alias = %v, want scoring-pipeline", spec["alias"])
	}
	if spec["label"] != "Scoring pipeline" {
		t.Errorf("label = %v", spec["label"])
	}
	if spec["category"] != "EVALUATION" {
		t.Errorf("category = %v", spec["category"])
	}
	if _, ok := spec["inputVariables"].(map[string]any)["borrower_id"]; !ok {
		t.Errorf("inputVariables.borrower_id missing")
	}
	// Entity arrays must NOT leak into the spec.
	if _, ok := spec["scorecards"]; ok {
		t.Errorf("scorecards array should not be present in apply-spec")
	}

	nodes, _ := spec["nodes"].([]map[string]any)
	if len(nodes) != 4 {
		t.Fatalf("nodes len = %d, want 4", len(nodes))
	}

	byRef := map[string]map[string]any{}
	for _, n := range nodes {
		ref, _ := n["ref"].(string)
		byRef[ref] = n
	}

	// start node: graph-only, ref = node alias, no task config inlined.
	start := byRef["start-aaaaaa"]
	if start == nil {
		t.Fatalf("start node missing")
	}
	if start["type"] != "start" {
		t.Errorf("start type = %v", start["type"])
	}

	// fetch node: task body inlined, sourcesConfig present, ref = server alias.
	fetch := byRef["fetch-ecu-111111"]
	if fetch == nil {
		t.Fatalf("fetch node missing")
	}
	if fetch["type"] != "altdata-enrichment" {
		t.Errorf("fetch type = %v", fetch["type"])
	}
	if _, ok := fetch["sourcesConfig"]; !ok {
		t.Errorf("fetch sourcesConfig not inlined: %v", fetch)
	}
	if im, ok := fetch["inputMappings"].(map[string]any); !ok || im["borrower_id"] != "inputs.borrower_id" {
		t.Errorf("fetch inputMappings missing/wrong: %v", fetch["inputMappings"])
	}
	// identity bookkeeping must be stripped.
	for _, k := range []string{"alias", "specRef", "workflowAlias"} {
		if _, present := fetch[k]; present {
			t.Errorf("fetch should not carry %q", k)
		}
	}

	// score node: nested scorecardConfig inlined.
	score := byRef["score-222222"]
	if _, ok := score["scorecardConfig"]; !ok {
		t.Errorf("score scorecardConfig not inlined")
	}

	// end node: endConfig inlined.
	end := byRef["end-333333"]
	if _, ok := end["endConfig"]; !ok {
		t.Errorf("end endConfig not inlined")
	}

	// edges: from/to + sourceHandle preserved.
	edges, _ := spec["edges"].([]map[string]any)
	if len(edges) != 3 {
		t.Fatalf("edges len = %d, want 3", len(edges))
	}
	var sawHandle bool
	for _, e := range edges {
		if e["sourceHandle"] == "out_success" {
			sawHandle = true
		}
		if e["from"] == "" || e["to"] == "" {
			t.Errorf("edge missing from/to: %v", e)
		}
	}
	if !sawHandle {
		t.Errorf("sourceHandle not preserved on edges")
	}
}

// TestBundleToApplySpec_RoundTripsToComposeSpec verifies the emitted spec
// parses into the exact composeSpec struct that `apply` unmarshals, and that
// the split-by-type pass apply runs at parse time classifies nodes correctly
// (start -> graph-only, everything else -> task-bearing).
func TestBundleToApplySpec_RoundTripsToComposeSpec(t *testing.T) {
	spec, err := bundleToApplySpec(json.RawMessage(sampleExportBundle))
	if err != nil {
		t.Fatalf("bundleToApplySpec: %v", err)
	}
	out, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// apply rejects the legacy two-bucket shape before unmarshal; the emitted
	// spec must pass that gate.
	if err := detectLegacySpecShape(out); err != nil {
		t.Fatalf("detectLegacySpecShape rejected the apply-spec: %v", err)
	}

	var parsed composeSpec
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("apply could not parse the emitted spec: %v", err)
	}
	if parsed.Label == "" {
		t.Errorf("parsed label empty")
	}
	if len(parsed.Nodes) != 4 {
		t.Fatalf("parsed nodes = %d, want 4", len(parsed.Nodes))
	}

	// Replicate apply's parse-time split.
	var taskNodes, graphNodes int
	for _, n := range parsed.Nodes {
		if t, _ := n["type"].(string); t == "start" {
			graphNodes++
		} else {
			taskNodes++
		}
	}
	if graphNodes != 1 {
		t.Errorf("graph-only nodes = %d, want 1 (start)", graphNodes)
	}
	if taskNodes != 3 {
		t.Errorf("task-bearing nodes = %d, want 3 (fetch/score/end)", taskNodes)
	}
	if len(parsed.Edges) != 3 {
		t.Errorf("parsed edges = %d, want 3", len(parsed.Edges))
	}
}
