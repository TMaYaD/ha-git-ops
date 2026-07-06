package main

import (
	"bytes"
	"testing"
)

// Differently formatted / ordered but semantically equal YAML and JSON
// must canonicalize to identical bytes — this is what makes Tier-2
// drift comparison format-insensitive.
func TestCanonYAMLEquivalence(t *testing.T) {
	yamlSrc := []byte("strategy:\n  options:\n    badges:\n      light_count: false\n  type: custom:mushroom-strategy\nviews: []\n")
	jsonSrc := []byte(`{"views": [], "strategy": {"type": "custom:mushroom-strategy", "options": {"badges": {"light_count": false}}}}`)

	a, err := CanonYAML(yamlSrc)
	if err != nil {
		t.Fatal(err)
	}
	b, err := CanonYAML(jsonSrc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("canonical forms differ:\n--- yaml\n%s\n--- json\n%s", a, b)
	}
}

func TestCanonYAMLDeterministic(t *testing.T) {
	src := []byte(`{"b": 2, "a": [3, {"z": 1, "y": null}], "c": "x"}`)
	want := "a:\n  - 3\n  - y: null\n    z: 1\nb: 2\nc: x\n"
	got, err := CanonYAML(src)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
}
