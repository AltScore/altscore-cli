package cmd

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// resolveTaskTypeName maps a near miss ("compute", "rule_tree", "computeVariables") onto the
// one task type it can only mean; anything ambiguous is left for the server to answer.
func resolveTaskTypeName(name string, known map[string]bool) (string, bool) {
	if known[name] {
		return name, false
	}
	norm := strings.TrimSpace(name)
	if !strings.ContainsAny(norm, "-_ ") {
		norm = camelToSnake(norm)
	}
	norm = strings.NewReplacer("_", "-", " ", "-").Replace(strings.ToLower(norm))
	if known[norm] {
		return norm, true
	}
	var hits []string
	for t := range known {
		if strings.HasPrefix(t, norm) {
			hits = append(hits, t)
		}
	}
	if len(hits) == 1 {
		return hits[0], true
	}
	return name, false
}

type taskTypeRow struct {
	Type    string `json:"type"`
	Notes   bool   `json:"notes"`
	Purpose string `json:"purpose,omitempty"`
}

type taskTypeIndex struct {
	Types      []taskTypeRow     `json:"types"`
	Deprecated map[string]string `json:"deprecated,omitempty"`
}

// The raw tasks section is about 100 KB: every type's notes and introspected fields at once.
func compactTaskTypeIndex(raw json.RawMessage) (taskTypeIndex, error) {
	var doc struct {
		Tasks struct {
			PerType         map[string]any             `json:"perType"`
			PerTypeAuto     map[string]json.RawMessage `json:"perTypeAuto"`
			DeprecatedTypes map[string]any             `json:"deprecatedTypes"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return taskTypeIndex{}, fmt.Errorf("parse schema-guide tasks: %w", err)
	}
	names := map[string]bool{}
	for t := range doc.Tasks.PerTypeAuto {
		if !strings.HasPrefix(t, "_") {
			names[t] = true
		}
	}
	for t := range doc.Tasks.PerType {
		names[t] = true
	}
	index := taskTypeIndex{Types: []taskTypeRow{}}
	for _, t := range sortedKeys(names) {
		row := taskTypeRow{Type: t}
		if notes, ok := doc.Tasks.PerType[t]; ok {
			row.Notes = true
			if m, ok := notes.(map[string]any); ok {
				if p, ok := m["purpose"].(string); ok {
					row.Purpose = firstSentence(p, 120)
				}
			}
		}
		index.Types = append(index.Types, row)
	}
	for name, v := range doc.Tasks.DeprecatedTypes {
		if s, ok := v.(string); ok && name != "description" {
			if index.Deprecated == nil {
				index.Deprecated = map[string]string{}
			}
			index.Deprecated[name] = firstSentence(s, 100)
		}
	}
	if len(index.Types) == 0 {
		return taskTypeIndex{}, fmt.Errorf("schema-guide tasks returned no task types")
	}
	return index, nil
}

func firstSentence(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, ". "); i >= 0 {
		s = s[:i+1]
	}
	if len(s) > max {
		s = strings.TrimSpace(s[:max]) + "..."
	}
	return s
}

type guideMatch struct {
	Path string `json:"path"`
	Open string `json:"open"`
	Text string `json:"text"`
}

// searchGuide finds term (case-insensitive) in the whole guide's keys and text. A key that
// matches is reported once for its whole subtree, so one task type is one hit, not fifty.
func searchGuide(raw json.RawMessage, term string, limit int) ([]guideMatch, int, error) {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, 0, fmt.Errorf("parse schema-guide: %w", err)
	}
	needle := strings.ToLower(strings.TrimSpace(term))
	if needle == "" {
		return nil, 0, fmt.Errorf("--search needs a word to look for")
	}
	var matches []guideMatch
	add := func(path []string, text string) {
		matches = append(matches, guideMatch{Path: strings.Join(path, "."), Open: guideOpenCommand(path), Text: text})
	}
	var walk func(node any, path []string)
	walk = func(node any, path []string) {
		switch v := node.(type) {
		case map[string]any:
			keys := make([]string, 0, len(v))
			for k := range v {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				child := append(append([]string{}, path...), k)
				if strings.Contains(strings.ToLower(k), needle) {
					add(child, guideSnippet(v[k], "", 160))
					continue
				}
				walk(v[k], child)
			}
		case []any:
			for i, item := range v {
				walk(item, append(append([]string{}, path...), strconv.Itoa(i)))
			}
		case string:
			if strings.Contains(strings.ToLower(v), needle) {
				add(path, guideSnippet(v, needle, 160))
			}
		}
	}
	walk(doc, nil)
	total := len(matches)
	if limit > 0 && len(matches) > limit {
		matches = matches[:limit]
	}
	return matches, total, nil
}

// The command that prints the section holding path.
func guideOpenCommand(path []string) string {
	if len(path) == 0 {
		return "altscore workflows-v2 schema-guide"
	}
	if path[0] == "tasks" && len(path) >= 3 && (path[1] == "perType" || path[1] == "perTypeAuto") && !strings.HasPrefix(path[2], "_") {
		return "altscore workflows-v2 schema-guide tasks " + path[2]
	}
	return "altscore workflows-v2 schema-guide " + path[0]
}

// A string is cut around the first hit; anything else is shown as compact JSON.
func guideSnippet(v any, needle string, max int) string {
	s, ok := v.(string)
	if !ok {
		b, _ := json.Marshal(v)
		s = string(b)
	}
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	start := 0
	if needle != "" {
		if i := strings.Index(strings.ToLower(s), needle); i > max/3 {
			start = i - max/3
		}
	}
	end := min(start+max, len(s))
	out := s[start:end]
	if start > 0 {
		out = "..." + out
	}
	if end < len(s) {
		out += "..."
	}
	return out
}
