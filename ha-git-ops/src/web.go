package main

// Ingress UI: status, per-file diffs, promote / revert / sync actions.
// Served under HA ingress — all URLs are relative so the ingress path
// prefix is transparent. Secrets diffs are masked to key names only.

import (
	_ "embed"
	"html/template"
	"net/http"
	"strings"
)

//go:embed page.gohtml
var pageSrc string

var page = template.Must(template.New("page").Parse(pageSrc))

type fileView struct {
	Rel, Why, Diff string
}

type pageView struct {
	StatusHTML      template.HTML
	RestartRequired bool
	Conflicts       []fileView
	Files           []fileView
	PromoteAll      bool
}

func NewWebUI(rec *Reconciler) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, req *http.Request) {
		st, sha := rec.Snapshot()
		if len(sha) > 9 {
			sha = sha[:9]
		}
		v := pageView{RestartRequired: st.RestartRequired}

		var status string
		switch {
		case st.Error != "":
			status = `<span class="bad">✗ ` + template.HTMLEscapeString(st.Error) + `</span>`
		case len(st.Conflicts) > 0:
			status = `<span class="bad">⚠ conflicts</span>`
		case len(st.Drift) > 0:
			status = `<span class="warn">● drift</span>`
		default:
			status = `<span class="ok">✓ in sync</span>`
		}
		last := st.LastSync
		if last == "" {
			last = "never"
		}
		v.StatusHTML = template.HTML(status + " — applied <code>" +
			template.HTMLEscapeString(sha) + "</code>, last sync " +
			template.HTMLEscapeString(last))

		for _, rel := range sortedKeys(st.Conflicts) {
			v.Conflicts = append(v.Conflicts, fileView{Rel: rel, Why: st.Conflicts[rel]})
		}
		for _, rel := range sortedKeys(st.Drift) {
			v.Files = append(v.Files, fileView{
				Rel: rel, Why: st.Drift[rel], Diff: rec.DiffText(rel)})
		}
		v.PromoteAll = len(v.Files) > 1
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = page.Execute(w, v)
	})

	mux.HandleFunc("POST /sync", func(w http.ResponseWriter, req *http.Request) {
		if err := rec.SyncNow(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		http.Redirect(w, req, "./", http.StatusSeeOther)
	})

	mux.HandleFunc("POST /revert", func(w http.ResponseWriter, req *http.Request) {
		rel := req.FormValue("rel")
		if rel == "" || strings.Contains(rel, "..") {
			http.Error(w, "bad rel", http.StatusBadRequest)
			return
		}
		if err := rec.Revert(rel); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		http.Redirect(w, req, "./", http.StatusSeeOther)
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
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Redirect(w, req, "./", http.StatusSeeOther)
	})

	return mux
}
