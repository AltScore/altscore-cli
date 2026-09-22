package cmd

import "sort"

// Mirrors the Hub's Align button (COL_GAP 120 / ROW_GAP 60 around each measured box).
// The CLI has no DOM, so the height is a generous stand-in and rows never collide.
const (
	layoutNodeW  = 250.0
	layoutNodeH  = 140.0
	layoutColGap = 120.0
	layoutRowGap = 60.0
)

func autoLayoutNodes(nodes []map[string]any, edges []map[string]any) {
	if len(nodes) == 0 {
		return
	}

	// Node order drives every tie-break below, so a given spec always lays out the same.
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

	// Self-edges, edges to unknown nodes and duplicates are dropped: each inflates in-degree
	// past the number of real parents, and Kahn would then never reach zero.
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

	// Presence in `level` means resolved; a node the queue never reaches (a cycle member)
	// stays absent and lands in the trailing unconnected column.
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

// Auto-layout stays out of a hand-placed canvas: pinning one node and moving the rest
// would produce a graph that matches neither intent.
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
