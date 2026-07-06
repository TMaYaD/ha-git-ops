// ha-git-ops: ArgoCD-style GitOps reconciler for Home Assistant OS.
// Git is desired state; live click-ops changes surface as drift for
// human promote/revert and are never auto-committed.
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
	AutoRestartCore bool   `json:"auto_restart_core"`
	CommitName      string `json:"commit_name"`
	CommitEmail     string `json:"commit_email"`
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

	for {
		if err := rec.Tick(); err != nil {
			log.Printf("reconcile tick failed: %v", err)
			rec.SetError(err.Error())
		}
		time.Sleep(time.Duration(opts.PollInterval) * time.Second)
	}
}
