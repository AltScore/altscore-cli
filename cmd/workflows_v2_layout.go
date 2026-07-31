package cmd

import "sort"

// Canvas geometry for auto-layout. Mirrors the Hub's "Align" button
// (handleAutoLayout in
// altscore-ai-chat/components/workflow-builder-v2/canvas/WorkflowCanvas.tsx),
// which uses COL_GAP=120 / ROW_GAP=60 around each node's MEASURED box.
//
// The CLI has no DOM, so it assumes a uniform card. layoutNodeW is the Hub's
// fixed node width (w-[250px] in canvas/nodes/WorkflowNode.tsx); layoutNodeH is
// a deliberately generous stand-in for the content-dependent height so rows
// never collide before the Hub has measured anything. Pressing Align in the Hub
// afterwards tightens the spacing but reproduces the same columns and row
// order.
const (
	layoutNodeW  = 250.0
	layoutNodeH  = 140.0
	layoutColGap = 120.0
	layoutRowGap = 60.0
)

// autoLayoutNodes rewrites every node's `position` so the graph reads
// left-to-right with no overlapping cards and no edge pointing backwards.
// Nodes are matched to edges by `nodeId`; edges are read via edgeEndpoints, so
// this works on both spec-local refs and resolved server aliases.
//
// Port of the Hub's three passes:
//  1. longest-path leveling (Kahn order) -> column per node. A node sits one
//     column right of its DEEPEST parent, so a diamond's short arm can't land
//     left of the node feeding it (the pre-#2503 shortest-path BFS did).
//  2. barycenter crossing reduction -> row order within each column, 4
//     alternating down/up sweeps.
//  3. stack each column by row pitch, centered on y=0.
//
// Nodes stranded by a cycle, and nodes with no edges at all, land in a
// trailing column -- the Hub's "unconnected" bucket.
func autoLayoutNodes(nodes []map[string]any, edges []map[string]any) {
	if len(nodes) == 0 {
		return
	}

	// Node order drives every tie-break below, so the output is deterministic
	// for a given spec.
	ids := make([]string, 0, len(nodes))
	indexByID := make(map[string]int, len(nodes))
	for i, n := range nodes {
		id, _ := n["nodeId"].(string)
		if id == "" {
			continue
		}
		if _, dup := indexByID[id]; dup {
			continue
		}
		indexByID[id] = i
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return
	}

	// Adjacency. Self-edges, edges to unknown nodes, and duplicate edges are
	// dropped: each would inflate in-degree past the number of real parents, so
	// Kahn would never reach zero and the node would be exiled to the
	// unconnected column for what is really a harmless spec redundancy.
	children := make(map[string][]string, len(ids))
	parents := make(map[string][]string, len(ids))
	inDegree := make(map[string]int, len(ids))
	for _, id := range ids {
		inDegree[id] = 0
	}
	seenEdge := map[[2]string]bool{}
	for _, e := range edges {
		src, tgt := edgeEndpoints(e)
		if src == "" || tgt == "" || src == tgt {
			continue
		}
		if _, ok := indexByID[src]; !ok {
			continue
		}
		if _, ok := indexByID[tgt]; !ok {
			continue
		}
		key := [2]string{src, tgt}
		if seenEdge[key] {
			continue
		}
		seenEdge[key] = true
		children[src] = append(children[src], tgt)
		parents[tgt] = append(parents[tgt], src)
		inDegree[tgt]++
	}

	// Pass 1: longest-path leveling. Presence in `level` means "resolved";
	// a node the queue never reaches (cycle member) stays absent.
	level := make(map[string]int, len(ids))
	remaining := make(map[string]int, len(ids))
	for _, id := range ids {
		remaining[id] = inDegree[id]
	}
	queue := make([]string, 0, len(ids))
	for _, id := range ids {
		if inDegree[id] == 0 {
			level[id] = 0
			queue = append(queue, id)
		}
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		next := level[id] + 1
		for _, child := range children[id] {
			if cur, ok := level[child]; !ok || next > cur {
				level[child] = next
			}
			remaining[child]--
			if remaining[child] == 0 {
				queue = append(queue, child)
			}
		}
	}

	levelNodes := map[int][]string{}
	for _, id := range ids {
		l, ok := level[id]
		if !ok {
			continue
		}
		levelNodes[l] = append(levelNodes[l], id)
	}
	sortedLevels := make([]int, 0, len(levelNodes))
	for l := range levelNodes {
		sortedLevels = append(sortedLevels, l)
	}
	sort.Ints(sortedLevels)

	// Pass 2: barycenter crossing reduction. Order each column by the average
	// normalized position of its neighbours in the adjacent column; nodes with
	// no neighbour there keep their own relative slot.
	reorderByNeighbors := func(lvl, refLvl int, neighborsOf map[string][]string) {
		list := levelNodes[lvl]
		if len(list) < 2 {
			return
		}
		refIDs := levelNodes[refLvl]
		refPos := make(map[string]float64, len(refIDs))
		for i, id := range refIDs {
			if len(refIDs) > 1 {
				refPos[id] = float64(i) / float64(len(refIDs)-1)
			} else {
				refPos[id] = 0.5
			}
		}
		type slot struct {
			id  string
			key float64
			i   int
		}
		slots := make([]slot, len(list))
		for i, id := range list {
			sum, n := 0.0, 0
			for _, nb := range neighborsOf[id] {
				if p, ok := refPos[nb]; ok {
					sum += p
					n++
				}
			}
			key := 0.5
			if len(list) > 1 {
				key = float64(i) / float64(len(list)-1)
			}
			if n > 0 {
				key = sum / float64(n)
			}
			slots[i] = slot{id: id, key: key, i: i}
		}
		sort.SliceStable(slots, func(a, b int) bool {
			if slots[a].key != slots[b].key {
				return slots[a].key < slots[b].key
			}
			return slots[a].i < slots[b].i
		})
		out := make([]string, len(slots))
		for i, s := range slots {
			out[i] = s.id
		}
		levelNodes[lvl] = out
	}
	for sweep := 0; sweep < 4; sweep++ {
		if sweep%2 == 0 {
			for li := 1; li < len(sortedLevels); li++ {
				reorderByNeighbors(sortedLevels[li], sortedLevels[li-1], parents)
			}
			continue
		}
		for li := len(sortedLevels) - 2; li >= 0; li-- {
			reorderByNeighbors(sortedLevels[li], sortedLevels[li+1], children)
		}
	}

	// Pass 3: place. Uniform cards mean a fixed column/row pitch and no
	// per-column width math (the Hub centers narrower nodes inside a wider
	// column; every node here is the same width).
	colPitch := layoutNodeW + layoutColGap
	rowPitch := layoutNodeH + layoutRowGap
	place := func(column int, ids []string) {
		x := float64(column) * colPitch
		y := -(rowPitch*float64(len(ids)) - layoutRowGap) / 2
		for _, id := range ids {
			nodes[indexByID[id]]["position"] = map[string]float64{"x": x, "y": y}
			y += rowPitch
		}
	}
	for column, lvl := range sortedLevels {
		place(column, levelNodes[lvl])
	}

	unconnected := make([]string, 0)
	for _, id := range ids {
		if _, ok := level[id]; !ok {
			unconnected = append(unconnected, id)
		}
	}
	if len(unconnected) > 0 {
		place(len(sortedLevels), unconnected)
	}
}

// specHasPinnedPositions reports whether the author pinned a canvas position on
// any node in the spec. Auto-layout stays out of a hand-placed canvas: pinning
// one node and letting the layout move the rest would produce a graph that
// matches neither intent.
func specHasPinnedPositions(spec *composeSpec) bool {
	for _, buckets := range [][]map[string]any{spec.ExtraNodes, spec.Tasks} {
		for _, n := range buckets {
			if _, ok := n["position"]; ok {
				return true
			}
		}
	}
	return false
}
