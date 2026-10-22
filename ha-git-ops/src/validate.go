package main

// Pre-apply checks. Every file an apply would change is checked before
// any of them is written: one that doesn't parse, or isn't the shape its
// writer needs, blocks the whole apply. HA's own core check still runs
// once the files are on disk — it is the only check that understands
// HA semantics, and it reads the config dir — but before anything goes
// live over the websocket.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// checkLocked checks every rel in want, reporting all failures at once.
func (r *Reconciler) checkLocked(rels []string, want map[string][]byte) error {
	var errs []string
	for _, rel := range rels {
		if err := r.check(rel, want[rel]); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w: %s", ErrInvalid, strings.Join(errs, "; "))
	}
	return nil
}

// check validates one file's desired live content; nil means deletion.
func (r *Reconciler) check(rel string, content []byte) error {
	if isDashboard(rel) {
		if _, err := parseDashboard(rel, content); err != nil {
			return err
		}
		_, err := r.dashboardFor(rel)
		return err
	}
	if def := registryFor(rel); def != nil {
		_, err := parseRegistry(def, content)
		return err
	}
	if content == nil {
		return nil
	}
	switch strings.ToLower(filepath.Ext(rel)) {
	case ".yaml", ".yml":
		if err := checkYAML(content); err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
	case ".json":
		if !json.Valid(content) {
			return fmt.Errorf("%s: invalid JSON", rel)
		}
	}
	return nil
}

// checkYAML parses every document into nodes, so HA's custom tags
// (!include, !secret, !env_var, …) pass while syntax errors don't.
func checkYAML(content []byte) error {
	dec := yaml.NewDecoder(bytes.NewReader(content))
	for {
		var n yaml.Node
		err := dec.Decode(&n)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
