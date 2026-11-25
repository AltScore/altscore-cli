package cmd

import (
	"encoding/json"
	"regexp"
	"testing"

	"github.com/AltScore/altscore-cli/internal/skill"
)

var skillJSONBlock = regexp.MustCompile("(?s)```json\n(.*?)\n```")

// Every spreadsheet-builder node the skill shows, in the order it shows them: a node
// block, or the nodes of a whole spec. Blocks that are not plain JSON are skipped.
func skillSpreadsheetNodes(t *testing.T) []map[string]any {
	t.Helper()
	body, err := skill.ReadFile("references/workflows-v2.md")
	if err != nil {
		t.Fatal(err)
	}
	var nodes []map[string]any
	for _, m := range skillJSONBlock.FindAllStringSubmatch(string(body), -1) {
		var doc map[string]any
		if json.Unmarshal([]byte(m[1]), &doc) != nil {
			continue
		}
		candidates := []map[string]any{doc}
		for _, n := range asSlice(doc["nodes"]) {
			candidates = append(candidates, asMap(n))
		}
		for _, n := range candidates {
			if n["type"] == "spreadsheet-builder" {
				nodes = append(nodes, n)
			}
		}
	}
	return nodes
}

// The examples are what agents copy: each must pass the publish rules apply mirrors.
func TestSkillSpreadsheetExamplesArePublishable(t *testing.T) {
	nodes := skillSpreadsheetNodes(t)
	if len(nodes) < 3 {
		t.Fatalf("want the blocks, template and code examples, found %d spreadsheet-builder nodes", len(nodes))
	}
	for _, n := range nodes {
		if problems := spreadsheetBuilderProblems(n); len(problems) > 0 {
			t.Errorf("skill example %v cannot be published: %v", n["ref"], problems)
		}
	}
}

// HQ-2297: agents copy the first example they find, so the first ones are native (blocks,
// then template) and code shows up once, as the exception.
func TestSkillSpreadsheetExamplesLeadWithNativeBlocks(t *testing.T) {
	nodes := skillSpreadsheetNodes(t)
	codeDrawn := func(n map[string]any) bool {
		g := &structureGraph{Nodes: []structureNode{{Ref: "x", Type: "spreadsheet-builder", Body: n}}}
		return len(g.adviseSpreadsheetCode()) > 0
	}
	if len(nodes) < 2 || codeDrawn(nodes[0]) || codeDrawn(nodes[1]) {
		t.Fatalf("the first two spreadsheet-builder examples must be built with blocks or a template")
	}
	if cfg := asMap(nodes[1]["spreadsheetBuilderConfig"]); cfg["source"] != "template" {
		t.Errorf("the second example is the template one, got source %v", cfg["source"])
	}
	code := 0
	for _, n := range nodes {
		if codeDrawn(n) {
			code++
		}
	}
	if code != 1 {
		t.Errorf("want exactly one code example, the exception, got %d", code)
	}
}
