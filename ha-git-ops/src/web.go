package main

// Ingress UI: status, per-file diffs, refresh / apply / promote actions.
// Served under HA ingress — all URLs are relative so the ingress path
// prefix is transparent. Secrets diffs are masked to key names only.

import (
	_ "embed"
	"html/template"
	"log"
	"net/http"
	"strings"
)

//go:embed page.gohtml
var pageSrc string

var page = template.Must(template.New("page").Parse(pageSrc))

type fileView struct {
	Rel, Why, Diff string
	CanRevert      bool
}

// done returns a tiny self-refreshing page instead of a 303 — HA's
// ingress iframe mishandles redirect responses to form POSTs and gets
// stuck on a loading screen.
func done(w http.ResponseWriter, what string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<!doctype html><meta http-equiv="refresh" content="1;url=./">` +
		`<body style="font-family:system-ui;margin:1.5rem">✓ ` +
		template.HTMLEscapeString(what) + ` — refreshing…</body>`))
}

type pageView struct {
	StatusHTML      template.HTML
	RestartRequired bool
	Conflicts       []fileView
	Incoming        []fileView
	Files           []fileView
	PromoteAll      bool
	InSync          bool
}

func NewWebUI(rec *Reconciler) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, req *http.Request) {
		st, sha := rec.Snapshot()
		if len(sha) > 9 {
			sha = sha[:9]
		}
		v := pageView{
			RestartRequired: st.RestartRequired,
			InSync: st.Error == "" && len(st.Conflicts) == 0 &&
				len(st.Drift) == 0 && len(st.Incoming) == 0,
		}

		var status string
		switch {
		case st.Error != "":
			status = `<span class="bad">✗ ` + template.HTMLEscapeString(st.Error) + `</span>`
		case len(st.Conflicts) > 0:
			status = `<span class="bad">⚠ conflicts</span>`
		case len(st.Incoming) > 0:
			status = `<span class="info">↓ updates in git</span>`
		case len(st.Drift) > 0:
			status = `<span class="warn">● drift</span>`
		default:
			status = `<span class="ok">✓ in sync</span>`
		}
		last := st.LastSync
		if last == "" {
			last = "never"
		}
		v.StatusHTML = template.HTML(status +
			` <span class="nw">applied <code>` + template.HTMLEscapeString(sha) + `</code></span>` +
			` <span class="nw">· last refresh ` + template.HTMLEscapeString(last) + `</span>`)

		for _, rel := range sortedKeys(st.Conflicts) {
			v.Conflicts = append(v.Conflicts, fileView{Rel: rel, Why: st.Conflicts[rel]})
		}
		for _, rel := range sortedKeys(st.Incoming) {
			v.Incoming = append(v.Incoming, fileView{
				Rel: rel, Why: st.Incoming[rel], Diff: rec.IncomingDiffText(rel)})
		}
		for _, rel := range sortedKeys(st.Drift) {
			v.Files = append(v.Files, fileView{
				Rel: rel, Why: st.Drift[rel], Diff: rec.DiffText(rel),
				CanRevert: rec.CanRevert(rel)})
		}
		v.PromoteAll = len(v.Files) > 1
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = page.Execute(w, v)
	})

	mux.HandleFunc("POST /refresh", func(w http.ResponseWriter, req *http.Request) {
		if err := rec.RefreshNow(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		done(w, "refreshed")
	})

	mux.HandleFunc("POST /apply", func(w http.ResponseWriter, req *http.Request) {
		if err := rec.ApplyUpstream(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		done(w, "applied updates from git")
	})

	mux.HandleFunc("POST /revert", func(w http.ResponseWriter, req *http.Request) {
		rel := req.FormValue("rel")
		if rel == "" || strings.Contains(rel, "..") {
			http.Error(w, "bad rel", http.StatusBadRequest)
			return
		}
		if err := rec.Revert(rel); err != nil {
			log.Printf("revert %s failed: %v", rel, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		done(w, "applied git version of "+rel)
	})

	mux.HandleFunc("POST /promote", func(w http.ResponseWriter, req *http.Request) {
		if err := req.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rels := req.PostForm["rel"]
		if len(rels) == 0 {
			http.Error(w, "no files", http.StatusBadRequest)
			return
		}
		for _, rel := range rels {
			if strings.Contains(rel, "..") {
				http.Error(w, "bad rel", http.StatusBadRequest)
				return
			}
		}
		if err := rec.Promote(rels, req.FormValue("message")); err != nil {
			log.Printf("promote %v failed: %v", rels, err)
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		done(w, "promoted")
	})

	return mux
}
