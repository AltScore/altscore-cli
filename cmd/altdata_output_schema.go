package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/AltScore/altscore-cli/internal/client"
)

// Many sources have no dictionary and the endpoint answers 404 "no results found"; their
// outputSchema still names every output path, which is what a workflow wires.
func dictionaryFromOutputSchema(c *client.Client, sourceID, version string, stderr io.Writer) (json.RawMessage, error) {
	entries, err := fetchSourceEntries(c, sourceID)
	if err != nil {
		return nil, err
	}
	var schema any
	for _, e := range entries {
		if v, _ := e["sourceVersion"].(string); v == version {
			if byID, ok := e["outputSchema"].(map[string]any); ok {
				schema = byID[sourceID]
			}
			break
		}
	}
	fields := outputSchemaFields(schema)
	if len(fields) == 0 {
		return nil, fmt.Errorf("%s %s has no data dictionary and no outputSchema; 'altscore altdata describe %s' shows what the catalog knows about it", sourceID, version, sourceID)
	}
	rows := make([]map[string]any, 0, len(fields))
	for _, f := range fields {
		rows = append(rows, map[string]any{"sourceId": sourceID, "version": version, "field": f.Path, "dataType": f.Type})
	}
	fmt.Fprintf(stderr, "# %s %s has no data dictionary; these are its %d output paths from the outputSchema (types only, no descriptions). Wire them as task_outputs.<ref>.%s.<field>\n", sourceID, version, len(rows), sourceID)
	return json.Marshal(rows)
}

type schemaField struct {
	Path string
	Type string
}

// A node is a JSON Schema ({type, properties, items}) or, at the top, a map of named schemas
// such as {data: {...}, sourceData: {...}}. The fields of an array's elements read as "list[].field".
func outputSchemaFields(node any) []schemaField {
	var out []schemaField
	join := func(path, key string) string {
		if path == "" {
			return key
		}
		return path + "." + key
	}
	var walk func(n any, path string, depth int)
	walk = func(n any, path string, depth int) {
		m, ok := n.(map[string]any)
		if !ok || depth > 12 {
			return
		}
		if len(m) == 0 {
			if path != "" {
				out = append(out, schemaField{Path: path, Type: "any"})
			}
			return
		}
		if props, ok := m["properties"].(map[string]any); ok {
			for _, k := range sortedMapKeys(props) {
				walk(props[k], join(path, k), depth+1)
			}
			return
		}
		if items, ok := m["items"].(map[string]any); ok {
			out = append(out, schemaField{Path: path, Type: "array"})
			if _, nested := items["properties"]; nested {
				walk(items, path+"[]", depth+1)
			}
			return
		}
		if t := schemaTypeName(m["type"]); t != "" {
			out = append(out, schemaField{Path: path, Type: t})
			return
		}
		for _, k := range sortedMapKeys(m) {
			walk(m[k], join(path, k), depth+1)
		}
	}
	walk(node, "", 0)
	return out
}

// "type" is a string or a list such as ["string", "null"].
func schemaTypeName(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		names := []string{}
		for _, x := range t {
			if s, ok := x.(string); ok {
				names = append(names, s)
			}
		}
		return strings.Join(names, "|")
	}
	return ""
}

func sortedMapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
