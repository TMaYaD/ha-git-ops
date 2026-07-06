package main

// Canonicalization for Tier-2 content: parse YAML or JSON and re-render
// deterministically (sorted map keys, 2-space indent). Semantically equal
// configs become byte-equal regardless of source formatting or key
// order, so drift comparison is format-insensitive and promotes always
// write one canonical form.

import (
	"bytes"
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"
)

// CanonYAML canonicalizes a YAML (or JSON — YAML superset) document.
func CanonYAML(in []byte) ([]byte, error) {
	var v any
	if err := yaml.Unmarshal(in, &v); err != nil {
		return nil, err
	}
	return renderYAML(v)
}

func renderYAML(v any) ([]byte, error) {
	n, err := sortedNode(v)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func sortedNode(v any) (*yaml.Node, error) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		for _, k := range keys {
			kn := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}
			vn, err := sortedNode(t[k])
			if err != nil {
				return nil, err
			}
			n.Content = append(n.Content, kn, vn)
		}
		return n, nil
	case map[any]any:
		m := make(map[string]any, len(t))
		for k, val := range t {
			m[fmt.Sprint(k)] = val
		}
		return sortedNode(m)
	case []any:
		n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, item := range t {
			in, err := sortedNode(item)
			if err != nil {
				return nil, err
			}
			n.Content = append(n.Content, in)
		}
		return n, nil
	default:
		n := &yaml.Node{}
		if err := n.Encode(v); err != nil {
			return nil, err
		}
		return n, nil
	}
}
