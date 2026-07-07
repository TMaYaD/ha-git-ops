// ha-git-ops: ArgoCD-style GitOps reconciler for Home Assistant OS.
// Git is desired state; incoming commits and live click-ops drift both
// surface as reviewable diffs, and a human applies or promotes each —
// nothing is written in either direction automatically.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"
)

type Options struct {
	RepoURL         string `json:"repo_url"`
	Branch          string `json:"branch"`
	Subfolder       string `json:"subfolder"`
	PollInterval    int    `json:"poll_interval"`
	AutoRefresh     bool   `json:"auto_refresh"`
	AutoApply       bool   `json:"auto_apply"`
	AutoRestartCore bool   `json:"auto_restart_core"`
	CommitName      string `json:"commit_name"`
	CommitEmail     string `json:"commit_email"`
	NotifyDrift     bool   `json:"notify_drift"`
	NotifyService   string `json:"notify_service"`
}

func main() {
	raw, err := os.ReadFile("/data/options.json")
	if err != nil {
		log.Fatalf("read options: %v", err)
	}
	var opts Options
	if err := json.Unmarshal(raw, &opts); err != nil {
		log.Fatalf("parse options: %v", err)
	}
	if opts.PollInterval < 30 {
		opts.PollInterval = 120
	}

	ha := NewHA()
	ha.NotifyService = opts.NotifyService
	rec := NewReconciler(opts, ha)
	if err := rec.EnsureKeys(); err != nil {
		log.Fatalf("key setup: %v", err)
	}

	go func() {
		log.Println("ingress UI listening on :8099")
		if err := http.ListenAndServe(":8099", NewWebUI(rec)); err != nil {
			log.Fatalf("web server: %v", err)
		}
	}()

	// ArgoCD semantics: refresh (fetch + diff) is automatic and read-only;
	// applying is opt-in via auto_apply, like syncPolicy.automated.
	refresh := func() {
		if err := rec.Tick(); err != nil {
			log.Printf("refresh failed: %v", err)
			rec.SetError(err.Error())
			return
		}
		if !opts.AutoApply {
			return
		}
		if st, _ := rec.Snapshot(); st.PendingSHA != "" {
			if err := rec.ApplyUpstream(); err != nil {
				log.Printf("auto-apply failed: %v", err)
				rec.SetError(err.Error())
			}
		}
	}

	refresh() // startup: baseline on first run, populate status
	for {
		time.Sleep(time.Duration(opts.PollInterval) * time.Second)
		if opts.AutoRefresh {
			refresh()
		}
	}
}
