package cmd

import (
	"sort"
	"strings"
)

// Small helpers shared by the apply files.

// truncateForError shortens a value for an error message. Residual refs turn up
// in SQL bodies and JSON templates that are far too long to quote whole, and an
// error nobody reads to the end names the path for nothing.
// Counted in runes, not bytes: task labels and aliases carry accented
// characters, and slicing mid-rune would print a replacement glyph.
func truncateForError(s string) string {
	const max = 160
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "..."
}

// humanizeKey turns "borrower_id" into "Borrower Id", "minScore" into "Min Score",
// etc. Used to auto-generate display titles for input variables when the spec
// doesn't supply one.
func humanizeKey(key string) string {
	if key == "" {
		return ""
	}
	// Split on _ and -
	parts := strings.FieldsFunc(key, func(r rune) bool { return r == '_' || r == '-' })
	// Also split camelCase within each part.
	var words []string
	for _, p := range parts {
		buf := []rune{}
		for i, r := range p {
			if i > 0 && r >= 'A' && r <= 'Z' {
				words = append(words, string(buf))
				buf = []rune{r}
			} else {
				buf = append(buf, r)
			}
		}
		if len(buf) > 0 {
			words = append(words, string(buf))
		}
	}
	for i, w := range words {
		if w == "" {
			continue
		}
		runes := []rune(w)
		if runes[0] >= 'a' && runes[0] <= 'z' {
			runes[0] = runes[0] - 32
		}
		words[i] = string(runes)
	}
	return strings.Join(words, " ")
}

// camelToSnake converts a camelCase wire field to its snake_case spelling.
// document-extraction's runtime-resolvable fields are accepted by the backend
// activity under either spelling, so preflight has to recognise both before it
// can claim a document source is missing.
func camelToSnake(field string) string {
	var out strings.Builder
	for i, r := range field {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				out.WriteByte('_')
			}
			out.WriteRune(r - 'A' + 'a')
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}

// firstNestedSchemaProperty returns the name of the first top-level property of
// a JSON Schema that is an object, or an array of objects, else "". Used to
// refuse a schema the ocr-tools provider cannot satisfy: its extraction targets
// are scalars and list[string] only, so a nested shape there yields nothing
// rather than failing loudly.
func firstNestedSchemaProperty(schema map[string]any) string {
	props := asMap(schema["properties"])
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	// Deterministic report: map iteration order would otherwise name a
	// different field on each run for a schema with several nested entries.
	sort.Strings(names)
	for _, name := range names {
		prop := asMap(props[name])
		switch t, _ := prop["type"].(string); t {
		case "object":
			return name
		case "array":
			if it, _ := asMap(prop["items"])["type"].(string); it == "object" {
				return name
			}
		}
	}
	return ""
}

// isServerAlias reports whether s looks like a server-assigned task alias --
// the trailing 6-hex-after-dash pattern produced by
// borrower-central/app/utils/alias_generator.py::generate_task_alias.
func isServerAlias(s string) bool {
	if len(s) < 8 {
		return false
	}
	dash := strings.LastIndex(s, "-")
	if dash < 1 || len(s)-dash-1 != 6 {
		return false
	}
	for i := dash + 1; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// sortedKeys returns the keys of m in deterministic order.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
