package cmd

import (
	"sort"
	"strings"
)

// Counted in runes: labels and aliases carry accents, and slicing mid-rune prints a
// replacement glyph.
func truncateForError(s string) string {
	const max = 160
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "..."
}

func humanizeKey(key string) string {
	if key == "" {
		return ""
	}
	parts := strings.FieldsFunc(key, func(r rune) bool { return r == '_' || r == '-' })
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

// The backend activity accepts document-extraction fields under either spelling, so
// preflight has to recognise both before it can claim a document source is missing.
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

// Refuses a schema the ocr-tools provider cannot satisfy: its extraction targets are
// scalars and list[string] only, so a nested shape yields nothing rather than failing.
func firstNestedSchemaProperty(schema map[string]any) string {
	props := asMap(schema["properties"])
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
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

// The trailing 6-hex-after-dash pattern minted by borrower-central's generate_task_alias.
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

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
