package main

// Tier-3 → Tier-2 upgrade for registries with a meaningful custom
// representation (GITOPS.md "Tier 3 custom representations"): floors and
// labels project to registry/<name>.yaml as a map keyed by the HA slug,
// carrying only human-meaningful fields — ids and timestamps excluded.
//
// Apply diffs desired vs live per item and issues create/update/delete
// through the registry websocket APIs. Deleting the whole file from git
// is refused (it would wipe the registry); delete items instead.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type registryDef struct {
	rel     string
	storage string // filename under .storage
	listKey string // array key under "data"
	idKey   string
	wsAPI   string   // websocket command prefix
	fields  []string // managed fields; id and timestamps excluded
	// zero values sent on update when a field is absent in git, so the
	// live value is cleared rather than left behind
	clearValue map[string]any
}

var registries = []registryDef{
	{
		rel: "registry/floors.yaml", storage: "core.floor_registry",
		listKey: "floors", idKey: "floor_id", wsAPI: "config/floor_registry",
		fields:     []string{"name", "level", "icon", "aliases"},
		clearValue: map[string]any{"aliases": []any{}},
	},
	{
		rel: "registry/labels.yaml", storage: "core.label_registry",
		listKey: "labels", idKey: "label_id", wsAPI: "config/label_registry",
		fields: []string{"name", "icon", "color", "description"},
	},
}

func registryFor(rel string) *registryDef {
	for i := range registries {
		if registries[i].rel == rel {
			return &registries[i]
		}
	}
	return nil
}

func isRegistry(rel string) bool { return registryFor(rel) != nil }

// projectRegistry filters raw .storage JSON down to id → managed
// fields, omitting nulls, empty strings and empty lists.
func projectRegistry(def *registryDef, raw []byte) (map[string]any, error) {
	var doc struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	var items []map[string]any
	if err := json.Unmarshal(doc.Data[def.listKey], &items); err != nil {
		return nil, err
	}
	out := map[string]any{}
	for _, it := range items {
		id, _ := it[def.idKey].(string)
		if id == "" {
			continue
		}
		entry := map[string]any{}
		for _, f := range def.fields {
			switch v := it[f].(type) {
			case nil:
			case string:
				if v != "" {
					entry[f] = v
				}
			case []any:
				if len(v) > 0 {
					entry[f] = v
				}
			default:
				entry[f] = v
			}
		}
		out[id] = entry
	}
	return out, nil
}

// registryLive returns the canonical projection of a live registry, or
// nil when it's empty/absent (so empty registries don't surface).
func (r *Reconciler) registryLive(rel string) []byte {
	def := registryFor(rel)
	raw, err := os.ReadFile(filepath.Join(r.configDir, ".storage", def.storage))
	if err != nil {
		return nil
	}
	m, err := projectRegistry(def, raw)
	if err != nil {
		log.Printf("project %s: %v", rel, err)
		return nil
	}
	if len(m) == 0 {
		return nil
	}
	b, err := renderYAML(m)
	if err != nil {
		log.Printf("render %s: %v", rel, err)
		return nil
	}
	return b
}

// registryWrite converges the live registry to the desired projection
// with per-item create/update/delete websocket calls.
func (r *Reconciler) registryWrite(rel string, content []byte) error {
	def := registryFor(rel)
	if content == nil {
		return fmt.Errorf("%s: deleting the whole registry file is not applied — remove individual items instead", rel)
	}
	var desired map[string]map[string]any
	if err := yaml.Unmarshal(content, &desired); err != nil {
		return fmt.Errorf("%s: parse desired: %w", rel, err)
	}

	live := map[string]any{}
	if raw, err := os.ReadFile(filepath.Join(r.configDir, ".storage", def.storage)); err == nil {
		if m, err := projectRegistry(def, raw); err == nil {
			live = m
		}
	}

	for id, want := range desired {
		if _, ok := live[id]; !ok {
			cmd := map[string]any{"type": def.wsAPI + "/create"}
			for f, v := range want {
				cmd[f] = v
			}
			if err := r.ha.wsCall(cmd); err != nil {
				return fmt.Errorf("create %s %q: %w", rel, id, err)
			}
			continue
		}
		wantC, _ := renderYAML(want)
		haveC, _ := renderYAML(live[id])
		if bytes.Equal(wantC, haveC) {
			continue
		}
		cmd := map[string]any{"type": def.wsAPI + "/update", def.idKey: id}
		for _, f := range def.fields {
			if f == def.idKey {
				continue
			}
			if v, ok := want[f]; ok {
				cmd[f] = v
			} else if cv, ok := def.clearValue[f]; ok {
				cmd[f] = cv
			} else {
				cmd[f] = nil
			}
		}
		if err := r.ha.wsCall(cmd); err != nil {
			return fmt.Errorf("update %s %q: %w", rel, id, err)
		}
	}
	for id := range live {
		if _, ok := desired[id]; !ok {
			cmd := map[string]any{
				"type": def.wsAPI + "/delete", def.idKey: id}
			if err := r.ha.wsCall(cmd); err != nil {
				return fmt.Errorf("delete %s %q: %w", rel, id, err)
			}
		}
	}
	return nil
}
