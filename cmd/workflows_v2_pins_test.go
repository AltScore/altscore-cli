package cmd

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func pin(entityType, code string, version int, entityID string) map[string]any {
	return map[string]any{
		"entityType": entityType, "code": code, "refCode": code, "refId": nil,
		"scopeAlias": "my-wf", "workflowAlias": "my-wf", "entityId": entityID, "version": version,
		"taskAliases": []any{"node-" + code},
	}
}

func workflowWithPins(id, status string, version int, pins ...map[string]any) map[string]any {
	wf := map[string]any{"id": id, "alias": "my-wf", "status": status, "version": version, "config": map[string]any{}}
	if len(pins) > 0 {
		raw := make([]any, len(pins))
		for i, p := range pins {
			raw[i] = p
		}
		wf["config"] = map[string]any{"entityPins": raw, "taskSnapshots": map[string]any{}}
	}
	return wf
}

func TestEntityPinsOfParsesWhatBCWritesAndDropsMalformedEntries(t *testing.T) {
	wf := workflowWithPins("w", "ACTIVE", 3,
		pin("mapping_table", "band", 3, "mt-1"),
		map[string]any{"entityType": "scorecard"}, // no entityId: dropped
		pin("evaluation_rule", "r1", 2, "er-1"),
	)
	wf["config"].(map[string]any)["entityPins"] = append(wf["config"].(map[string]any)["entityPins"].([]any), "junk")
	got := entityPinsOf(wf)
	if len(got) != 2 {
		t.Fatalf("expected the 2 well-formed pins, got %+v", got)
	}
	if got[0].Resource != "mapping-tables" || got[0].Version != 3 || got[0].EntityID != "mt-1" || got[0].TaskAliases[0] != "node-band" {
		t.Errorf("mapping table pin parsed wrong: %+v", got[0])
	}
	if got[1].Resource != "evaluation-rules" {
		t.Errorf("each pin names the CLI group that reads its body, got %+v", got[1])
	}
	if len(entityPinsOf(map[string]any{"config": nil})) != 0 {
		t.Error("no config means no pins")
	}
}

func TestDiffEntityPins(t *testing.T) {
	parse := func(pins ...map[string]any) []entityPin {
		return entityPinsOf(workflowWithPins("w", "ACTIVE", 1, pins...))
	}
	a := parse(pin("mapping_table", "band", 3, "mt-1"), pin("evaluation_rule", "r1", 2, "er-1"),
		pin("scorecard", "s1", 2, "sc-1"), pin("rule_tree", "t1", 1, "rt-1"))
	b := parse(pin("mapping_table", "band", 4, "mt-1"), pin("evaluation_rule", "r1", 2, "er-1"),
		pin("evaluation_rule", "r5", 1, "er-5"), pin("rule_tree", "t1", 1, "rt-2"))

	onlyA, onlyB, changed, unchanged := diffEntityPins(a, b)
	if unchanged != 1 {
		t.Errorf("r1 is the same version on both sides, unchanged=%d", unchanged)
	}
	if len(onlyA) != 1 || onlyA[0].Code != "s1" {
		t.Errorf("s1 is only in A, got %+v", onlyA)
	}
	if len(onlyB) != 1 || onlyB[0].Code != "r5" {
		t.Errorf("r5 is only in B, got %+v", onlyB)
	}
	if len(changed) != 2 {
		t.Fatalf("band moved v3->v4 and t1 now resolves to another entity, got %+v", changed)
	}
	if changed[0].EntityType != "mapping_table" || changed[0].VersionA != 3 || changed[0].VersionB != 4 {
		t.Errorf("band: %+v", changed[0])
	}
	if changed[1].EntityType != "rule_tree" || changed[1].EntityIDA != "rt-1" || changed[1].EntityIDB != "rt-2" {
		t.Errorf("t1: %+v", changed[1])
	}

	// A side without pins reads the latest entities: nothing is "removed".
	for _, pair := range [][2][]entityPin{{a, nil}, {nil, b}} {
		onlyA, onlyB, changed, unchanged := diffEntityPins(pair[0], pair[1])
		if len(onlyA)+len(onlyB)+len(changed)+unchanged != 0 {
			t.Errorf("one side without pins must not produce deltas, got %v %v %v %d", onlyA, onlyB, changed, unchanged)
		}
	}
}

func bundleWithPins(t *testing.T, version int, pins ...map[string]any) json.RawMessage {
	t.Helper()
	var b map[string]any
	if err := json.Unmarshal(diffBundle(version, diffBundleOpts{}), &b); err != nil {
		t.Fatal(err)
	}
	b["workflow"].(map[string]any)["config"] = workflowWithPins("w", "ACTIVE", version, pins...)["config"]
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func workflowJSON(t *testing.T, wf map[string]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(wf)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// What BC serves since #2407: the export bundle carries no pins, the workflow GET does.
func TestDiffComparesTheEntityPinsOfTwoPublishedVersions(t *testing.T) {
	f := fakeDoer{byPath: map[string]json.RawMessage{
		"/v2/workflows/v3/export": bundleWithPins(t, 3),
		"/v2/workflows/v4/export": bundleWithPins(t, 4),
		"/v2/workflows/v3": workflowJSON(t, workflowWithPins("v3", "ARCHIVED", 3,
			pin("mapping_table", "band", 3, "mt-1"), pin("scorecard", "s1", 2, "sc-1"))),
		"/v2/workflows/v4": workflowJSON(t, workflowWithPins("v4", "ACTIVE", 4,
			pin("mapping_table", "band", 4, "mt-1"), pin("scorecard", "s1", 2, "sc-1"))),
	}}
	specA, sideA, err := loadSideForDiff(f, "v3")
	if err != nil {
		t.Fatal(err)
	}
	specB, sideB, err := loadSideForDiff(f, "v4")
	if err != nil {
		t.Fatal(err)
	}
	if sideA.EntityPins != 2 || sideB.EntityPins != 2 {
		t.Fatalf("each side carries 2 pins, got %d and %d", sideA.EntityPins, sideB.EntityPins)
	}
	r := buildDiffReport(specA, sideA, specB, sideB)
	if len(r.EntityPinsChanged) != 1 || r.EntityPinsUnchanged != 1 {
		t.Fatalf("band v3 -> v4 changed, s1 unchanged; got %+v (unchanged %d)", r.EntityPinsChanged, r.EntityPinsUnchanged)
	}
	text := renderDiffReport(r)
	for _, want := range []string{"Entity pins", "1 unchanged", "~ mapping_table", "v3 -> v4", "get-version <entityId> <version>"} {
		if !strings.Contains(text, want) {
			t.Errorf("render is missing %q:\n%s", want, text)
		}
	}
	if !strings.Contains(text, "No differences in nodes, edges or variables.") {
		t.Errorf("an entity-only change leaves the graph identical, and the render should still say so:\n%s", text)
	}
	raw, _ := json.Marshal(r)
	if !strings.Contains(string(raw), `"entityPinsChanged":[{`) || !strings.Contains(string(raw), `"entityPins":2`) {
		t.Errorf("--json must carry the pin delta and counts: %s", raw)
	}
}

func TestDiffAgainstADraftSaysItHasNoPinsInsteadOfListingRemovals(t *testing.T) {
	f := fakeDoer{byPath: map[string]json.RawMessage{
		"/v2/workflows/v3/export":    bundleWithPins(t, 3),
		"/v2/workflows/draft/export": bundleWithPins(t, 4),
		"/v2/workflows/v3":           workflowJSON(t, workflowWithPins("v3", "ACTIVE", 3, pin("mapping_table", "band", 3, "mt-1"))),
		"/v2/workflows/draft":        workflowJSON(t, workflowWithPins("draft", "DRAFT", 4)),
	}}
	specA, sideA, _ := loadSideForDiff(f, "v3")
	specB, sideB, _ := loadSideForDiff(f, "draft")
	if sideA.EntityPins != 1 {
		t.Fatalf("A's pins come from its GET, got %d", sideA.EntityPins)
	}
	text := renderDiffReport(buildDiffReport(specA, sideA, specB, sideB))
	if !strings.Contains(text, "B carries none and reads the latest entities") {
		t.Errorf("expected the no-pins note for B:\n%s", text)
	}
	if strings.Contains(text, "- mapping_table") {
		t.Errorf("a draft's missing pins are not removals:\n%s", text)
	}
}

// v0.46.0 read pins from the export bundle and found none once BC stopped exporting them.
// The GET is the source whenever it answers, even over pins an older export still carries.
func TestDiffReadsPinsFromTheWorkflowGetNotTheExport(t *testing.T) {
	f := fakeDoer{byPath: map[string]json.RawMessage{
		"/v2/workflows/v3/export": bundleWithPins(t, 3, pin("mapping_table", "band", 1, "mt-old")),
		"/v2/workflows/v3":        workflowJSON(t, workflowWithPins("v3", "ACTIVE", 3, pin("mapping_table", "band", 3, "mt-1"))),
	}}
	_, side, err := loadSideForDiff(f, "v3")
	if err != nil {
		t.Fatal(err)
	}
	if side.Status != "ACTIVE" || len(side.pins) != 1 || side.pins[0].EntityID != "mt-1" || side.pins[0].Version != 3 {
		t.Fatalf("pins must come from the GET (mt-1 v3), never the export (mt-old v1): status=%q pins=%+v", side.Status, side.pins)
	}
}

// A backend whose workflow GET does not answer still diffs, on the pins its export carries.
func TestDiffFallsBackToExportPinsWhenTheGetFails(t *testing.T) {
	f := fakeDoer{byPath: map[string]json.RawMessage{
		"/v2/workflows/v3/export": bundleWithPins(t, 3, pin("mapping_table", "band", 3, "mt-1")),
	}}
	_, side, err := loadSideForDiff(f, "v3")
	if err != nil {
		t.Fatalf("a failed GET is a decoration loss, not a diff failure: %v", err)
	}
	if side.Status != "" || side.EntityPins != 1 {
		t.Fatalf("expected no status and the export's 1 pin, got status=%q pins=%d", side.Status, side.EntityPins)
	}
}

func TestPinsCommandReadsTheActiveVersionOfAnAlias(t *testing.T) {
	active := workflowWithPins(activeWorkflowID, "ACTIVE", 3, pin("mapping_table", "band", 3, "mt-1"), pin("rule_tree", "t1", 2, "rt-1"))
	draft := workflowWithPins(draftWorkflowID, "DRAFT", 4)
	versions := map[string]any{"versions": []any{
		map[string]any{"status": "DRAFT", "versionNumber": 4, "workflowId": draftWorkflowID},
		map[string]any{"status": "ACTIVE", "versionNumber": 3, "workflowId": activeWorkflowID},
	}}
	srv, seen := newRecordingBC(t, func(r *http.Request) (int, string) {
		var v any
		switch r.URL.Path {
		case "/v2/workflows/my-wf/versions":
			v = versions
		case "/v2/workflows/no-active/versions":
			v = map[string]any{"versions": []any{map[string]any{"status": "DRAFT", "workflowId": draftWorkflowID}}}
		case "/v2/workflows/" + activeWorkflowID:
			v = active
		case "/v2/workflows/my-wf/4", "/v2/workflows/" + draftWorkflowID:
			v = draft
		default:
			return 404, `{"code":"NotFoundError"}`
		}
		b, _ := json.Marshal(v)
		return 200, string(b)
	})
	routeLoadClientTo(t, srv.URL)

	cases := []struct {
		name       string
		args       []string
		wantRows   int
		wantStderr string
	}{
		{"alias means its ACTIVE version, not the open DRAFT", []string{"my-wf"}, 2, "my-wf v3 ACTIVE"},
		{"an exact id", []string{activeWorkflowID}, 2, "2 entity pin(s)"},
		{"a draft by number carries none", []string{"my-wf", "4"}, 0, "a DRAFT never carries them"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, err := runListCmd(t, makeWfv2PinsCmd(), tc.args...)
			if err != nil {
				t.Fatal(err)
			}
			rows := decodeRows(t, stdout)
			if len(rows) != tc.wantRows {
				t.Errorf("expected %d rows, got %d:\n%s", tc.wantRows, len(rows), stdout)
			}
			if !strings.Contains(stderr, tc.wantStderr) {
				t.Errorf("stderr missing %q: %s", tc.wantStderr, stderr)
			}
			if tc.wantRows > 0 && (rows[0]["resource"] != "mapping-tables" || rows[0]["entityId"] != "mt-1" || rows[0]["version"] != float64(3)) {
				t.Errorf("row must say which body to read: %v", rows[0])
			}
		})
	}
	for _, r := range seen() {
		if r.path == "/v2/workflows/my-wf/latest" || r.path == "/v2/workflows/my-wf" {
			t.Errorf("an alias must not be read through %s: that is the highest version, the DRAFT", r.path)
		}
	}

	if _, _, err := runListCmd(t, makeWfv2PinsCmd(), "no-active"); err == nil || !strings.Contains(err.Error(), "has no ACTIVE version") {
		t.Errorf("an alias with no ACTIVE version has nothing running on pins, got %v", err)
	}
}

func TestPinsIsRegisteredUnderWorkflowsV2(t *testing.T) {
	if findSubcommand(t, "workflows-v2", "pins") == nil {
		t.Fatal("workflows-v2 pins is not registered")
	}
}
