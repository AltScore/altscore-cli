package output

import (
	"encoding/json"
	"fmt"
	"os"
)

// JSON writes a value as indented JSON to stdout.
func JSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("cannot encode output: %w", err)
	}
	return nil
}

// RawJSON writes raw JSON bytes to stdout, re-indented for readability.
func RawJSON(data json.RawMessage) error {
	if data == nil {
		return nil
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		// Not valid JSON, write as-is
		os.Stdout.Write(data)
		fmt.Fprintln(os.Stdout)
		return nil
	}
	return JSON(v)
}
