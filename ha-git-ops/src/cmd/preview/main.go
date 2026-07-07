// Dev-only preview server for page.gohtml. Renders the dashboard with
// canned fixture data so the template can be styled without running the
// reconciler / HA supervisor. Re-parses the template on every request,
// so editing page.gohtml and refreshing the browser is enough.
//
// Usage: go run ./cmd/preview   (from src/), then open http://localhost:8098
package main

import (
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"sort"
)

// Mirror of the view structs in web.go — the template only cares about
// field names, so these just need to stay in shape-sync.
type fileView struct {
	Rel, Why, Diff string
	CanRevert      bool
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

const diffAutomations = `--- a/automations.yaml
+++ b/automations.yaml
@@ -12,7 +12,9 @@
 - id: '1718822400001'
   alias: Porch light at sunset
   trigger:
   - platform: sun
     event: sunset
+    offset: "-00:15:00"
   action:
   - service: light.turn_on
+    target:
+      entity_id: light.porch
`

const diffScripts = `--- a/scripts.yaml
+++ b/scripts.yaml
@@ -1,6 +1,10 @@
 goodnight:
   alias: Goodnight
   sequence:
-  - service: light.turn_off
-    entity_id: all
+  - service: light.turn_off
+    target:
+      entity_id: all
+  - service: lock.lock
+    target:
+      entity_id: lock.front_door
`

const diffDashboard = `--- a/dashboards/home.yaml
+++ b/dashboards/home.yaml
@@ -4,6 +4,12 @@
 views:
 - title: Home
   cards:
   - type: weather-forecast
     entity: weather.home
+  - type: entities
+    title: Climate
+    entities:
+    - climate.living_room
+    - sensor.outdoor_temperature
`

func status(kind, sha, last string) template.HTML {
	var s string
	switch kind {
	case "error":
		s = `<span class="bad">✗ git fetch failed: could not resolve host github.com</span>`
	case "conflicts":
		s = `<span class="bad">⚠ conflicts</span>`
	case "incoming":
		s = `<span class="info">↓ updates in git</span>`
	case "drift":
		s = `<span class="warn">● drift</span>`
	default:
		s = `<span class="ok">✓ in sync</span>`
	}
	return template.HTML(fmt.Sprintf(
		`%s <span class="nw">applied <code>%s</code></span> <span class="nw">· last refresh %s</span>`,
		s, sha, last))
}

const diffIncoming = `--- live/sensors.yaml
+++ git/sensors.yaml
@@ -3,6 +3,11 @@
 - platform: template
   sensors:
     heating_degree_days:
       friendly_name: Heating degree days
       unit_of_measurement: HDD
+    cooling_degree_days:
+      friendly_name: Cooling degree days
+      unit_of_measurement: CDD
+      value_template: >-
+        {{ max(0, states('sensor.outdoor_temperature') | float - 18) }}
`

const diffIncomingNew = `--- live/blueprints/motion_light.yaml
+++ git/blueprints/motion_light.yaml
@@ -0,0 +1,6 @@
+blueprint:
+  name: Motion-activated light
+  domain: automation
+  input:
+    motion_sensor:
+      selector: { entity: { domain: binary_sensor } }
`

var states = map[string]pageView{
	"insync": {
		StatusHTML: status("ok", "bbcdeb44f", "2m ago"),
		InSync:     true,
	},
	"drift": {
		StatusHTML: status("drift", "bbcdeb44f", "5m ago"),
		Files: []fileView{
			{Rel: "automations.yaml", Why: "modified in HA", Diff: diffAutomations, CanRevert: true},
			{Rel: "scripts.yaml", Why: "modified in HA", Diff: diffScripts, CanRevert: true},
			{Rel: "dashboards/home.yaml", Why: "only in HA", Diff: diffDashboard, CanRevert: false},
		},
		PromoteAll: true,
	},
	"one": {
		StatusHTML: status("drift", "bbcdeb44f", "just now"),
		Files: []fileView{
			{Rel: "automations.yaml", Why: "modified in HA", Diff: diffAutomations, CanRevert: true},
		},
	},
	"incoming": {
		StatusHTML: status("incoming", "bbcdeb44f", "just now"),
		Incoming: []fileView{
			{Rel: "blueprints/motion_light.yaml", Why: "new in git", Diff: diffIncomingNew},
			{Rel: "sensors.yaml", Why: "updated in git", Diff: diffIncoming},
		},
	},
	"mixed": {
		StatusHTML: status("incoming", "bbcdeb44f", "just now"),
		Incoming: []fileView{
			{Rel: "sensors.yaml", Why: "updated in git", Diff: diffIncoming},
			{Rel: "automations.yaml", Why: "changed in both git and HA — apply will skip it", Diff: diffAutomations},
		},
		Files: []fileView{
			{Rel: "automations.yaml", Why: "modified in HA", Diff: diffAutomations, CanRevert: true},
			{Rel: "scripts.yaml", Why: "modified in HA", Diff: diffScripts, CanRevert: true},
		},
		PromoteAll: true,
	},
	"conflicts": {
		StatusHTML: status("conflicts", "bbcdeb44f", "12m ago"),
		Conflicts: []fileView{
			{Rel: "automations.yaml", Why: "changed in both git and HA"},
			{Rel: "configuration.yaml", Why: "changed in both git and HA"},
		},
		Files: []fileView{
			{Rel: "scripts.yaml", Why: "modified in HA", Diff: diffScripts, CanRevert: true},
		},
	},
	"error": {
		StatusHTML:      status("error", "bbcdeb44f", "1h ago"),
		RestartRequired: true,
		Files: []fileView{
			{Rel: "automations.yaml", Why: "modified in HA", Diff: diffAutomations, CanRevert: true},
		},
	},
	"restart": {
		StatusHTML:      status("ok", "bbcdeb44f", "just now"),
		RestartRequired: true,
		InSync:          true,
	},
}

func render(w http.ResponseWriter, v pageView) {
	src, err := os.ReadFile("page.gohtml")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tpl, err := template.New("page").Parse(string(src))
	if err != nil {
		http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tpl.Execute(w, v); err != nil {
		log.Printf("execute: %v", err)
	}
}

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, req *http.Request) {
		name := req.URL.Query().Get("state")
		if name == "" {
			name = "drift"
		}
		v, ok := states[name]
		if !ok {
			names := make([]string, 0, len(states))
			for k := range states {
				names = append(names, k)
			}
			sort.Strings(names)
			http.Error(w, fmt.Sprintf("unknown state %q, try: %v", name, names), http.StatusNotFound)
			return
		}
		render(w, v)
	})

	// Stub actions so the buttons don't 404 while clicking around.
	for _, p := range []string{"/refresh", "/apply", "/revert", "/promote"} {
		p := p
		mux.HandleFunc("POST "+p, func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = fmt.Fprintf(w, `<!doctype html><meta http-equiv="refresh" content="1;url=./">`+
				`<body style="font-family:system-ui;margin:1.5rem">✓ (preview stub) %s — refreshing…</body>`, p)
		})
	}

	addr := "localhost:8098"
	log.Printf("preview at http://%s/?state=drift (states: insync drift one incoming mixed conflicts error restart)", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
