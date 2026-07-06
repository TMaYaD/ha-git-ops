package main

// Tier-2 translation for storage-mode Lovelace dashboards:
// .storage/lovelace.<id> JSON ⇄ dashboards/<url_path>.yaml in git.
//
// Reads come straight from .storage (the add-on runs privileged inside
// its container and the config mount includes it); writes MUST go
// through the websocket API so a running core picks them up — HA holds
// dashboard state in memory and would overwrite direct file edits.

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const dashPrefix = "dashboards/"

type dashboard struct {
	URLPath     string // "" means the unregistered default dashboard
	StorageFile string
}

// liveDashboards enumerates live storage-mode dashboards keyed by their
// git rel path (dashboards/<url_path>.yaml).
func (r *Reconciler) liveDashboards() (map[string]dashboard, error) {
	out := map[string]dashboard{}

	raw, err := os.ReadFile(filepath.Join(r.configDir, ".storage", "lovelace_dashboards"))
	if err == nil {
		var doc struct {
			Data struct {
				Items []struct {
					ID      string `json:"id"`
					URLPath string `json:"url_path"`
					Mode    string `json:"mode"`
				} `json:"items"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("parse lovelace_dashboards: %w", err)
		}
		for _, it := range doc.Data.Items {
			if it.Mode != "storage" || it.URLPath == "" {
				continue
			}
			out[dashPrefix+it.URLPath+".yaml"] = dashboard{
				URLPath:     it.URLPath,
				StorageFile: filepath.Join(r.configDir, ".storage", "lovelace."+it.ID),
			}
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	// The default dashboard, when not in the registry, lives at
	// .storage/lovelace with no url_path.
	bare := filepath.Join(r.configDir, ".storage", "lovelace")
	if _, err := os.Stat(bare); err == nil {
		out[dashPrefix+"default.yaml"] = dashboard{URLPath: "", StorageFile: bare}
	}
	return out, nil
}

func isDashboard(rel string) bool { return strings.HasPrefix(rel, dashPrefix) }

// dashboardLive returns the canonical YAML of a live dashboard config,
// or nil when the dashboard (or its config) doesn't exist.
func (r *Reconciler) dashboardLive(rel string) []byte {
	dbs, err := r.liveDashboards()
	if err != nil {
		log.Printf("dashboard enumeration failed: %v", err)
		return nil
	}
	db, ok := dbs[rel]
	if !ok {
		return nil
	}
	raw, err := os.ReadFile(db.StorageFile)
	if err != nil {
		return nil // registered but never configured
	}
	var doc struct {
		Data struct {
			Config any `json:"config"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || doc.Data.Config == nil {
		return nil
	}
	b, err := renderYAML(doc.Data.Config)
	if err != nil {
		log.Printf("render %s failed: %v", rel, err)
		return nil
	}
	return b
}

// dashboardWrite pushes desired config to the live dashboard via the
// websocket API. content == nil (file deleted in git) is unsupported.
func (r *Reconciler) dashboardWrite(rel string, content []byte) error {
	if content == nil {
		return fmt.Errorf("%s: deleting dashboards from git is not applied; delete it in the UI and promote", rel)
	}
	dbs, err := r.liveDashboards()
	if err != nil {
		return err
	}
	db, ok := dbs[rel]
	if !ok {
		return fmt.Errorf("%s: no live storage dashboard with this url_path; create it in the UI first", rel)
	}
	var cfg any
	if err := yaml.Unmarshal(content, &cfg); err != nil {
		return fmt.Errorf("%s: parse desired config: %w", rel, err)
	}
	return r.ha.LovelaceSave(db.URLPath, cfg)
}
