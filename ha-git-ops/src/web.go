package main

// Ingress UI: status, per-file diffs, promote / revert / sync actions.
// Served under HA ingress — all URLs are relative so the ingress path
// prefix is transparent. Secrets diffs are masked to key names only.

import (
	"html/template"
	"net/http"
	"strings"
)

var page = template.Must(template.New("page").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>HA GitOps</title><style>
 body { font-family: system-ui, sans-serif; margin: 1.5rem; max-width: 60rem; }
 .ok { color: #1a7f37; } .bad { color: #cf222e; } .warn { color: #9a6700; }
 pre { background: #f6f8fa; padding: .8rem; overflow-x: auto; font-size: .85rem; }
 .file { border: 1px solid #d0d7de; border-radius: 6px; margin: 1rem 0; }
 .file > header { padding: .5rem .8rem; background: #f6f8fa;
   display: flex; justify-content: space-between; align-items: center;
   flex-wrap: wrap; gap: .5rem; }
 button { cursor: pointer; } input[type=text] { width: 20rem; }
</style></head><body>
<h2>HA GitOps</h2>
<p>{{.StatusHTML}}</p>
<form method="post" action="./sync" style="display:inline"><button>Sync now</button></form>
{{if .RestartRequired}}<p class="warn">Core restart required to activate applied changes.</p>{{end}}
{{if .Conflicts}}<h3>Conflicts (resolve by revert or promote, then sync)</h3><ul>
{{range .Conflicts}}<li><code>{{.Rel}}</code> — {{.Why}}</li>{{end}}</ul>{{end}}
{{range .Files}}<div class="file"><header>
 <code>{{.Rel}}</code> <span>{{.Why}}</span>
 <span>
  <form method="post" action="./revert" style="display:inline">
   <input type="hidden" name="rel" value="{{.Rel}}"><button>Revert to git</button></form>
  <form method="post" action="./promote" style="display:inline">
   <input type="hidden" name="rel" value="{{.Rel}}">
   <input type="text" name="message" placeholder="commit message">
   <button>Promote</button></form>
 </span></header>
<pre>{{.Diff}}</pre></div>{{end}}
{{if .PromoteAll}}<form method="post" action="./promote">
{{range .Files}}<input type="hidden" name="rel" value="{{.Rel}}">{{end}}
<input type="text" name="message" placeholder="commit message">
<button>Promote all {{len .Files}} files</button></form>{{end}}
</body></html>`))

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
