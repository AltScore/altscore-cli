package cmd

import (
	"encoding/json"
	"strings"
	"testing"
)

// sampleExportBundle mirrors the /v2/workflows/{id}/export shape: a workflow
// object whose nodes reference tasks by taskAlias, plus a flat tasks[] array
// carrying the full backing task bodies. The start node's id ("start") differs
// from its task alias, the way the Hub assigns ids, and its task has no specRef
// (Hub-created); the other three tasks carry the specRef the CLI stamped.
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
       "taskAlias": "fetch-ecu-111111", "taskVersion": 1, "position": {"x": 100, "y": 200},
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
    {"alias": "fetch-ecu-111111", "specRef": "fetch-ecu", "workflowAlias": "scoring-pipeline",
     "type": "altdata-enrichment", "label": "Fetch ECU",
     "sourcesConfig": [{"sourceId": "ECU-PUB-0002", "version": "v1"}],
     "borrowerIdField": "borrower_id",
     "inputMappings": {"borrower_id": "inputs.borrower_id"}},
    {"alias": "score-222222", "specRef": "score", "workflowAlias": "scoring-pipeline",
     "type": "scorecard", "label": "Score",
     "scorecardConfig": {"scorecardCode": "sc-001", "inputMappings": {"x": "task_outputs.fetch-ecu-111111.score"}}},
    {"alias": "end-333333", "specRef": "end", "workflowAlias": "scoring-pipeline",
     "type": "end", "label": "End",
     "endConfig": {"pdfConfig": {"enabled": true, "sourcesConfig": []}}}
  ],
  "scorecards": [{"code": "sc-001"}]
}`

func specNodesByRef(t *testing.T, spec map[string]any) map[string]map[string]any {
	t.Helper()
	nodes, _ := spec["nodes"].([]map[string]any)
	byRef := map[string]map[string]any{}
	for _, n := range nodes {
		ref, _ := n["ref"].(string)
		if ref == "" {
			t.Errorf("node without ref: %v", n)
		}
		byRef[ref] = n
	}
	return byRef
}

func specEdgeEndpoints(t *testing.T, spec map[string]any) []string {
	t.Helper()
	edges, _ := spec["edges"].([]map[string]any)
	out := make([]string, 0, len(edges))
	for _, e := range edges {
		from, _ := e["from"].(string)
		to, _ := e["to"].(string)
		out = append(out, from+"->"+to)
	}
	return out
}

func TestBundleToApplySpec_Shape(t *testing.T) {
	spec, err := bundleToApplySpec(json.RawMessage(sampleExportBundle), refFromSpecRef)
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

	byRef := specNodesByRef(t, spec)
	if len(byRef) != 4 {
		t.Fatalf("nodes = %d, want 4 (%v)", len(byRef), byRef)
	}

	// A CLI-applied task is keyed by the specRef it was applied with, so the
	// server matches it by (workflowAlias, specRef) on re-apply.
	fetch := byRef["fetch-ecu"]
	if fetch == nil {
		t.Fatalf("fetch node should be keyed by its specRef; got refs %v", keysOfNodes(byRef))
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
	// Identity bookkeeping must be stripped: the server stamps its own.
	for _, k := range []string{"alias", "specRef", "workflowAlias", "nodeId", "taskAlias", "taskVersion"} {
		if _, present := fetch[k]; present {
			t.Errorf("fetch should not carry %q", k)
		}
	}
	// The canvas position travels, so apply's auto-layout stays off.
	if pos, _ := fetch["position"].(map[string]any); pos == nil || pos["x"] != float64(100) || pos["y"] != float64(200) {
		t.Errorf("fetch position not carried: %v", fetch["position"])
	}

	// A Hub-created task has no specRef: it falls back to its alias, never to
	// the bare node id (which the server does not know).
	start := byRef["start-aaaaaa"]
	if start == nil {
		t.Fatalf("start node should fall back to its alias; got refs %v", keysOfNodes(byRef))
	}
	if start["type"] != "start" {
		t.Errorf("start type = %v", start["type"])
	}
	if _, ok := byRef["score"]["scorecardConfig"]; !ok {
		t.Errorf("score scorecardConfig not inlined")
	}
	if _, ok := byRef["end"]["endConfig"]; !ok {
		t.Errorf("end endConfig not inlined")
	}

	// Edges name refs, mapped through the node each endpoint points at. The
	// start node's id is "start", not its alias: a raw copy would dangle.
	got := specEdgeEndpoints(t, spec)
	want := []string{"start-aaaaaa->fetch-ecu", "fetch-ecu->score", "score->end"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("edges = %v, want %v", got, want)
	}
	edges, _ := spec["edges"].([]map[string]any)
	last := edges[2]
	if last["sourceHandle"] != "out_success" || last["label"] != "ok" || last["id"] != "e3" {
		t.Errorf("edge handle/label/id not preserved: %v", last)
	}
}

// `diff <a> <b>` keys nodes by alias on both sides: a specRef only appears when
// the CLI first applies over a Hub-authored task, and alias identity keeps the
// node matched across that version boundary.
func TestBundleToApplySpec_AliasModeKeepsAliases(t *testing.T) {
	spec, err := bundleToApplySpec(json.RawMessage(sampleExportBundle), refFromAlias)
	if err != nil {
		t.Fatalf("bundleToApplySpec: %v", err)
	}
	byRef := specNodesByRef(t, spec)
	for _, alias := range []string{"start-aaaaaa", "fetch-ecu-111111", "score-222222", "end-333333"} {
		if byRef[alias] == nil {
			t.Errorf("alias mode should key node by %q; got %v", alias, keysOfNodes(byRef))
		}
	}
	if byRef["fetch-ecu"] != nil {
		t.Errorf("alias mode must not use the specRef as ref")
	}
	got := specEdgeEndpoints(t, spec)
	want := []string{"start-aaaaaa->fetch-ecu-111111", "fetch-ecu-111111->score-222222", "score-222222->end-333333"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("edges = %v, want %v", got, want)
	}
}

// An edge pointing at a node the bundle does not have is refused, not copied
// through as a dangling endpoint apply would then reject with a worse message.
func TestBundleToApplySpec_UnknownEdgeEndpointIsAnError(t *testing.T) {
	bundle := strings.Replace(sampleExportBundle, `"targetNodeId": "end-333333"`, `"targetNodeId": "ghost-999999"`, 1)
	spec, err := bundleToApplySpec(json.RawMessage(bundle), refFromSpecRef)
	if err == nil {
		t.Fatalf("expected a dangling edge to be refused, got spec with edges %v", specEdgeEndpoints(t, spec))
	}
	for _, want := range []string{"ghost-999999", "e3", "targetNodeId"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q; got: %v", want, err)
		}
	}
}

// TestBundleToApplySpec_RoundTripsToComposeSpec verifies the emitted spec
// parses into the exact composeSpec struct that `apply` unmarshals, that the
// split-by-type pass apply runs at parse time classifies nodes correctly
// (start -> graph-only, everything else -> task-bearing), that every edge
// endpoint names a node, and that nothing server-owned travels on a node.
func TestBundleToApplySpec_RoundTripsToComposeSpec(t *testing.T) {
	spec, err := bundleToApplySpec(json.RawMessage(sampleExportBundle), refFromSpecRef)
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

	// Replicate apply's parse-time split and check what must NOT be on a node.
	refs := map[string]bool{}
	for _, n := range parsed.Nodes {
		ref, _ := n["ref"].(string)
		refs[ref] = true
		for _, k := range []string{"alias", "nodeId", "taskId", "taskVersion", "specRef", "workflowAlias"} {
			if _, present := n[k]; present {
				t.Errorf("node %q carries server-owned key %q", ref, k)
			}
		}
		if typ, _ := n["type"].(string); typ == "start" {
			parsed.ExtraNodes = append(parsed.ExtraNodes, n)
		} else {
			parsed.Tasks = append(parsed.Tasks, n)
		}
	}
	if len(parsed.ExtraNodes) != 1 {
		t.Errorf("graph-only nodes = %d, want 1 (start)", len(parsed.ExtraNodes))
	}
	if len(parsed.Tasks) != 3 {
		t.Errorf("task-bearing nodes = %d, want 3 (fetch/score/end)", len(parsed.Tasks))
	}
	for _, e := range parsed.Edges {
		for _, side := range []string{"from", "to"} {
			if v, _ := e[side].(string); !refs[v] {
				t.Errorf("edge %v: %s %q names no node", e["id"], side, v)
			}
		}
	}
	// The carried position pins the canvas, which is what keeps auto-layout off.
	if !specHasPinnedPositions(&parsed) {
		t.Errorf("exported positions should pin the canvas for apply")
	}
}

func keysOfNodes(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// A live workflow can still be running a retired node type. The exported spec
// carries it verbatim -- dropping the node would silently change the graph --
// so the warning is what tells the author the spec will not re-apply as-is.
func TestBundleToApplySpec_WarnsOnDeprecatedNodeType(t *testing.T) {
	bundle := strings.Replace(sampleExportBundle,
		`"type": "scorecard", "label": "Score",
       "taskAlias": "score-222222"`,
		`"type": "webhook", "label": "Score",
       "taskAlias": "score-222222"`, 1)
	if bundle == sampleExportBundle {
		t.Fatal("fixture edit did not apply -- the sample bundle's node shape changed")
	}

	var spec map[string]any
	var err error
	stderr := captureStderr(t, func() { spec, err = bundleToApplySpec(json.RawMessage(bundle), refFromSpecRef) })
	if err != nil {
		t.Fatalf("bundleToApplySpec: %v", err)
	}
	if !strings.Contains(stderr, "DEPRECATED") || !strings.Contains(stderr, "webhook") {
		t.Errorf("apply-spec export must warn about the deprecated node type, got: %q", stderr)
	}
	// The node still travels: the warning is advice, not a filter.
	if got := specNodesByRef(t, spec)["score"]["type"]; got != "webhook" {
		t.Errorf("node type = %v, want the original webhook (the export must not rewrite the graph)", got)
	}

	// diff flattens through the same function and must stay quiet: nobody
	// re-applies a comparison.
	quiet := captureStderr(t, func() { _, err = bundleToApplySpec(json.RawMessage(bundle), refFromAlias) })
	if err != nil {
		t.Fatalf("bundleToApplySpec(refFromAlias): %v", err)
	}
	if strings.Contains(quiet, "DEPRECATED") {
		t.Errorf("diff's flatten must not warn, got: %q", quiet)
	}
}
