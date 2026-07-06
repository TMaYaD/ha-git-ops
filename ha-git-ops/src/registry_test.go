package main

import (
	"bytes"
	"testing"
)

func TestProjectRegistryFloors(t *testing.T) {
	raw := []byte(`{"data": {"floors": [
		{"aliases": [], "floor_id": "ground", "icon": null, "level": 0,
		 "name": "Ground", "created_at": "x", "modified_at": "y"},
		{"aliases": ["top"], "floor_id": "second", "icon": "mdi:home-roof",
		 "level": 2, "name": "Second", "created_at": "x", "modified_at": "y"}
	]}}`)
	m, err := projectRegistry(&registries[0], raw)
	if err != nil {
		t.Fatal(err)
	}
	got, err := renderYAML(m)
	if err != nil {
		t.Fatal(err)
	}
	want := "ground:\n  level: 0\n  name: Ground\nsecond:\n  aliases:\n    - top\n  icon: mdi:home-roof\n  level: 2\n  name: Second\n"
	if string(got) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

// Desired parsed from YAML (ints) must compare equal to live projected
// from JSON (floats) after canonical rendering — no spurious updates.
func TestRegistryNumericEquivalence(t *testing.T) {
	fromYAML, err := CanonYAML([]byte("level: 2\nname: Second\n"))
	if err != nil {
		t.Fatal(err)
	}
	fromJSON, err := CanonYAML([]byte(`{"name": "Second", "level": 2.0}`))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fromYAML, fromJSON) {
		t.Fatalf("int/float mismatch:\n%s\nvs\n%s", fromYAML, fromJSON)
	}
}
