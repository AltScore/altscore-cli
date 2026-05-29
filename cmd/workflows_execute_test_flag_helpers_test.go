package cmd

import "encoding/json"

func jsonHasTrue(b []byte, key string) bool {
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return false
	}
	v, ok := m[key].(bool)
	return ok && v
}

func jsonHasKey(b []byte, key string) bool {
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return false
	}
	_, ok := m[key]
	return ok
}
