package main

import (
	"encoding/json"
	"testing"
)

func TestResponsePreservesJSONRPCID(t *testing.T) {
	got := response(json.RawMessage(`17`), map[string]any{"ok": true})
	if got["id"] != float64(17) {
		t.Fatalf("id = %#v, want 17", got["id"])
	}
}
