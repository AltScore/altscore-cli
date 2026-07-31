package cmd

import (
	"fmt"
	"math"
	"testing"
)

// layoutNode builds a minimal graph node for the layout passes.
func layoutNode(id string) map[string]any {
	return map[string]any{"nodeId": id, "type": "http", "label": id}
}

func layoutEdge(from, to string) map[string]any {
	return map[string]any{"sourceNodeId": from, "targetNodeId": to}
}

// nodePos reads back a position written by autoLayoutNodes.
func nodePos(t *testing.T, nodes []map[string]any, id string) (float64, float64) {
	t.Helper()
	for _, n := range nodes {
		if nid, _ := n["nodeId"].(string); nid != id {
			continue
		}
		p, ok := n["position"].(map[string]float64)
		if !ok {
			t.Fatalf("node %q has no float position: %#v", id, n["position"])
		}
		return p["x"], p["y"]
	}
	t.Fatalf("node %q not found", id)
	return 0, 0
}

// assertNoOverlap fails when any two cards are close enough to visually
// collide, using the same box the layout reserves for each node.
func assertNoOverlap(t *testing.T, nodes []map[string]any) {
	t.Helper()
	type box struct {
		id   string
		x, y float64
	}
	boxes := []box{}
	for _, n := range nodes {
		id, _ := n["nodeId"].(string)
		p, ok := n["position"].(map[string]float64)
		if !ok {
			t.Fatalf("node %q has no float position: %#v", id, n["position"])
		}
		boxes = append(boxes, box{id: id, x: p["x"], y: p["y"]})
	}
	for i := 0; i < len(boxes); i++ {
		for j := i + 1; j < len(boxes); j++ {
			a, b := boxes[i], boxes[j]
			if math.Abs(a.x-b.x) < layoutNodeW && math.Abs(a.y-b.y) < layoutNodeH {
				t.Errorf("nodes %q(%v,%v) and %q(%v,%v) overlap (card is %vx%v)",
					a.id, a.x, a.y, b.id, b.x, b.y, layoutNodeW, layoutNodeH)
			}
		}
	}
}

// assertEdgesPointForward fails on any edge whose target is not strictly right
// of its source -- the backward-edge class Hub #2503 fixed in the builder.
func assertEdgesPointForward(t *testing.T, nodes []map[string]any, edges []map[string]any) {
	t.Helper()
	for _, e := range edges {
		src, tgt := edgeEndpoints(e)
		sx, _ := nodePos(t, nodes, src)
		tx, _ := nodePos(t, nodes, tgt)
		if tx <= sx {
			t.Errorf("edge %s->%s points backward or sideways: x %v -> %v", src, tgt, sx, tx)
		}
	}
}

// A diamond is the shape the old positioner mangled worst: both arms and the
// join all landed on one row, and the short arm drew backwards into the join.
func TestAutoLayoutNodes_DiamondColumnsAndForwardEdges(t *testing.T) {
	nodes := []map[string]any{
		layoutNode("start"), layoutNode("split"),
		layoutNode("armA"), layoutNode("armB"),
		layoutNode("join"), layoutNode("end"),
	}
	edges := []map[string]any{
		layoutEdge("start", "split"),
		layoutEdge("split", "armA"),
		layoutEdge("split", "armB"),
		layoutEdge("armA", "join"),
		layoutEdge("armB", "join"),
		layoutEdge("join", "end"),
	}

	autoLayoutNodes(nodes, edges)

	assertEdgesPointForward(t, nodes, edges)
	assertNoOverlap(t, nodes)

	// Both arms share a column; the join sits one column right of BOTH of them
	// (longest-path leveling), not right of the first arm to be visited.
	ax, ay := nodePos(t, nodes, "armA")
	bx, by := nodePos(t, nodes, "armB")
	if ax != bx {
		t.Errorf("parallel arms should share a column: armA.x=%v armB.x=%v", ax, bx)
	}
	if ay == by {
		t.Errorf("parallel arms should be on different rows: both y=%v", ay)
	}
	jx, _ := nodePos(t, nodes, "join")
	if jx != ax+layoutNodeW+layoutColGap {
		t.Errorf("join should be exactly one column right of the arms: arms.x=%v join.x=%v", ax, jx)
	}
}

// A short-circuit edge (start straight to end) must not drag `end` left into an
// early column: end belongs one column right of its DEEPEST parent.
func TestAutoLayoutNodes_ShortCircuitEdgeStaysForward(t *testing.T) {
	nodes := []map[string]any{
		layoutNode("start"), layoutNode("a"), layoutNode("b"), layoutNode("end"),
	}
	edges := []map[string]any{
		layoutEdge("start", "a"),
		layoutEdge("a", "b"),
		layoutEdge("b", "end"),
		layoutEdge("start", "end"),
	}

	autoLayoutNodes(nodes, edges)

	assertEdgesPointForward(t, nodes, edges)
	assertNoOverlap(t, nodes)

	ex, _ := nodePos(t, nodes, "end")
	bx, _ := nodePos(t, nodes, "b")
	if ex != bx+layoutNodeW+layoutColGap {
		t.Errorf("end should sit one column right of b: b.x=%v end.x=%v", bx, ex)
	}
}

// colTopY is the y of the top-left corner of a column holding n cards. Cards
// are centered on the y=0 axis, matching the Hub, so a lone card in a column
// sits at -height/2 rather than 0.
func colTopY(n int) float64 {
	return -((layoutNodeH+layoutRowGap)*float64(n) - layoutRowGap) / 2
}

// A long linear chain is the common case and must come out as one row of evenly
// spaced, non-overlapping columns -- the old 200px pitch overlapped every pair.
func TestAutoLayoutNodes_LinearChainIsOneRow(t *testing.T) {
	nodes := []map[string]any{}
	edges := []map[string]any{}
	for i := 0; i < 8; i++ {
		nodes = append(nodes, layoutNode(fmt.Sprintf("n%d", i)))
		if i > 0 {
			edges = append(edges, layoutEdge(fmt.Sprintf("n%d", i-1), fmt.Sprintf("n%d", i)))
		}
	}

	autoLayoutNodes(nodes, edges)

	assertEdgesPointForward(t, nodes, edges)
	assertNoOverlap(t, nodes)
	for i := 0; i < 8; i++ {
		x, y := nodePos(t, nodes, fmt.Sprintf("n%d", i))
		if want := float64(i) * (layoutNodeW + layoutColGap); x != want {
			t.Errorf("n%d.x = %v, want %v", i, x, want)
		}
		if y != colTopY(1) {
			t.Errorf("n%d.y = %v, want %v (lone card centers on the y=0 axis)", i, y, colTopY(1))
		}
	}
}

// Duplicate and self edges are spec redundancies, not topology: they must not
// inflate in-degree and exile a node to the unconnected column.
func TestAutoLayoutNodes_DuplicateAndSelfEdgesIgnored(t *testing.T) {
	nodes := []map[string]any{layoutNode("start"), layoutNode("mid"), layoutNode("end")}
	edges := []map[string]any{
		layoutEdge("start", "mid"),
		layoutEdge("start", "mid"), // duplicate
		layoutEdge("mid", "mid"),   // self
		layoutEdge("mid", "end"),
		layoutEdge("start", "ghost"), // unknown target
	}

	autoLayoutNodes(nodes, edges)

	pitch := layoutNodeW + layoutColGap
	for i, id := range []string{"start", "mid", "end"} {
		x, y := nodePos(t, nodes, id)
		if want := float64(i) * pitch; x != want {
			t.Errorf("%s.x = %v, want %v (duplicate/self/ghost edges must not shift columns)", id, x, want)
		}
		if y != colTopY(1) {
			t.Errorf("%s.y = %v, want %v", id, y, colTopY(1))
		}
	}
}

// A node the leveling pass can't reach (cycle member) lands in the trailing
// column -- the Hub's "unconnected" bucket -- instead of piling up at the
// origin. An edge-less node is still a level-0 root, so it shares the first
// column with the other roots.
func TestAutoLayoutNodes_CycleFallsToTrailingColumn(t *testing.T) {
	nodes := []map[string]any{
		layoutNode("start"), layoutNode("a"), layoutNode("b"), layoutNode("loner"),
	}
	edges := []map[string]any{
		layoutEdge("start", "a"),
		layoutEdge("a", "b"),
		layoutEdge("b", "a"), // cycle: a keeps an unresolved parent forever
	}

	autoLayoutNodes(nodes, edges)
	assertNoOverlap(t, nodes)

	// Levels resolve to {0: start, loner}, {1: a}; b is stranded -> column 2.
	pitch := layoutNodeW + layoutColGap
	sx, _ := nodePos(t, nodes, "start")
	lx, _ := nodePos(t, nodes, "loner")
	if sx != 0 || lx != 0 {
		t.Errorf("edge-less and root nodes both belong in column 0: start.x=%v loner.x=%v", sx, lx)
	}
	if bx, _ := nodePos(t, nodes, "b"); bx != 2*pitch {
		t.Errorf("stranded cycle node should trail every leveled column: b.x=%v, want %v", bx, 2*pitch)
	}
}

// Determinism: the same input must lay out identically every run, so re-applying
// an unchanged spec produces no spurious position diff.
func TestAutoLayoutNodes_Deterministic(t *testing.T) {
	build := func() ([]map[string]any, []map[string]any) {
		nodes := []map[string]any{
			layoutNode("start"), layoutNode("f1"), layoutNode("f2"), layoutNode("f3"),
			layoutNode("merge"), layoutNode("end"),
		}
		edges := []map[string]any{
			layoutEdge("start", "f1"), layoutEdge("start", "f2"), layoutEdge("start", "f3"),
			layoutEdge("f1", "merge"), layoutEdge("f2", "merge"), layoutEdge("f3", "merge"),
			layoutEdge("merge", "end"),
		}
		return nodes, edges
	}

	nodesA, edgesA := build()
	autoLayoutNodes(nodesA, edgesA)
	for run := 0; run < 3; run++ {
		nodesB, edgesB := build()
		autoLayoutNodes(nodesB, edgesB)
		for i := range nodesA {
			id, _ := nodesA[i]["nodeId"].(string)
			ax, ay := nodePos(t, nodesA, id)
			bx, by := nodePos(t, nodesB, id)
			if ax != bx || ay != by {
				t.Fatalf("run %d: %s moved from (%v,%v) to (%v,%v)", run, id, ax, ay, bx, by)
			}
		}
	}
}

func TestAutoLayoutNodes_EmptyGraphIsSafe(t *testing.T) {
	autoLayoutNodes(nil, nil)
	nodes := []map[string]any{layoutNode("solo")}
	autoLayoutNodes(nodes, nil)
	x, y := nodePos(t, nodes, "solo")
	if x != 0 || y != colTopY(1) {
		t.Errorf("single node should sit at (0,%v), got (%v,%v)", colTopY(1), x, y)
	}
}

func TestSpecHasPinnedPositions(t *testing.T) {
	clean := &composeSpec{
		ExtraNodes: []map[string]any{{"ref": "start", "type": "start"}},
		Tasks:      []map[string]any{{"ref": "t", "type": "http"}},
	}
	if specHasPinnedPositions(clean) {
		t.Error("spec without positions reported as pinned")
	}

	pinnedExtra := &composeSpec{
		ExtraNodes: []map[string]any{{"ref": "start", "position": map[string]any{"x": 10.0, "y": 20.0}}},
		Tasks:      []map[string]any{{"ref": "t"}},
	}
	if !specHasPinnedPositions(pinnedExtra) {
		t.Error("position on an extra node not detected")
	}

	pinnedTask := &composeSpec{
		ExtraNodes: []map[string]any{{"ref": "start"}},
		Tasks:      []map[string]any{{"ref": "t", "position": map[string]any{"x": 1.0, "y": 2.0}}},
	}
	if !specHasPinnedPositions(pinnedTask) {
		t.Error("position on a task node not detected")
	}
}

// Compose end-to-end: a branching spec must come out of composeWorkflowBody
// laid out, with the pinned-position escape hatch honored. This is the
// regression that made CLI-composed workflows open as an overlapping row.
func TestComposeWorkflowBody_LaysOutBranchingGraph(t *testing.T) {
	newSpec := func() *composeSpec {
		return &composeSpec{
			Label:    "Layout probe",
			Category: "EVALUATION",
			ExtraNodes: []map[string]any{
				{"ref": "start", "type": "start", "label": "Start"},
			},
			Tasks: []map[string]any{
				{"ref": "gate", "type": "http", "label": "Gate", "url": "https://example.com", "method": "GET"},
				{"ref": "left", "type": "http", "label": "Left", "url": "https://example.com", "method": "GET"},
				{"ref": "right", "type": "http", "label": "Right", "url": "https://example.com", "method": "GET"},
				{"ref": "fin", "type": "end", "label": "End"},
			},
			Edges: []map[string]any{
				{"from": "start", "to": "gate"},
				{"from": "gate", "to": "left"},
				{"from": "gate", "to": "right"},
				{"from": "left", "to": "fin"},
				{"from": "right", "to": "fin"},
			},
		}
	}

	wf, err := composeWorkflowBody(nil, newSpec(), true, false, true, false, false, true, newComposeCapture())
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	nodes, ok := wf["nodes"].([]map[string]any)
	if !ok {
		t.Fatalf("wf[nodes] is %T, want []map[string]any", wf["nodes"])
	}
	edges, ok := wf["edges"].([]map[string]any)
	if !ok {
		t.Fatalf("wf[edges] is %T, want []map[string]any", wf["edges"])
	}

	assertNoOverlap(t, nodes)
	assertEdgesPointForward(t, nodes, edges)

	lx, ly := nodePos(t, nodes, "left")
	rx, ry := nodePos(t, nodes, "right")
	if lx != rx {
		t.Errorf("branch arms should share a column: left.x=%v right.x=%v", lx, rx)
	}
	if ly == ry {
		t.Errorf("branch arms should occupy different rows: both y=%v", ly)
	}

	// --no-layout keeps the legacy single-row positions.
	legacy, err := composeWorkflowBody(nil, newSpec(), true, false, true, false, false, false, newComposeCapture())
	if err != nil {
		t.Fatalf("compose (no layout): %v", err)
	}
	legacyNodes := legacy["nodes"].([]map[string]any)
	for _, id := range []string{"left", "right"} {
		if _, y := nodePos(t, legacyNodes, id); y != 0 {
			t.Errorf("--no-layout should leave %s on y=0, got %v", id, y)
		}
	}

	// A pinned position anywhere in the spec disables layout entirely.
	pinned := newSpec()
	pinned.ExtraNodes[0]["position"] = map[string]float64{"x": 42, "y": 7}
	pinnedWf, err := composeWorkflowBody(nil, pinned, true, false, true, false, false, true, newComposeCapture())
	if err != nil {
		t.Fatalf("compose (pinned): %v", err)
	}
	pinnedNodes := pinnedWf["nodes"].([]map[string]any)
	if x, y := nodePos(t, pinnedNodes, "start"); x != 42 || y != 7 {
		t.Errorf("pinned start position was overwritten: got (%v,%v), want (42,7)", x, y)
	}
}

// A position pinned on a task entry must reach the graph node and NOT ride
// along in the body posted to /v2/tasks.
func TestComposeWorkflowBody_PinnedTaskPositionLiftedOffTaskBody(t *testing.T) {
	spec := &composeSpec{
		Label:    "Pinned task probe",
		Category: "EVALUATION",
		ExtraNodes: []map[string]any{
			{"ref": "start", "type": "start", "label": "Start"},
		},
		Tasks: []map[string]any{
			{
				"ref": "gate", "type": "http", "label": "Gate",
				"url": "https://example.com", "method": "GET",
				"position": map[string]float64{"x": 500, "y": 250},
			},
			{"ref": "fin", "type": "end", "label": "End"},
		},
		Edges: []map[string]any{
			{"from": "start", "to": "gate"},
			{"from": "gate", "to": "fin"},
		},
	}

	capture := newComposeCapture()
	wf, err := composeWorkflowBody(nil, spec, true, false, true, false, false, true, capture)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}

	nodes := wf["nodes"].([]map[string]any)
	if x, y := nodePos(t, nodes, "gate"); x != 500 || y != 250 {
		t.Errorf("pinned task position not applied to node: got (%v,%v), want (500,250)", x, y)
	}
	for _, entry := range capture.postPlan {
		if entry.placeholder != "gate" {
			continue
		}
		if _, leaked := entry.body["position"]; leaked {
			t.Error("position leaked into the /v2/tasks body")
		}
	}
}
