// Dev-only preview server for page.html. Serves the panel against an
// in-memory fixture of three-way file states behind the same JSON API as
// the add-on, so the UI can be exercised — staging, apply, commit —
// without the reconciler or an HA supervisor. Apply and commit really
// update the fixture, and the target can be switched to a couple of other
// branches; GET /reset restores it. Re-reads page.html on every
// request, so editing it and refreshing the browser is enough.
//
// Usage: go run ./cmd/preview   (from src/), then open http://localhost:8098
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TMaYaD/ha-git-ops/diff3"
)

// Mirror of the API shapes in web.go.
type apiFile struct {
	Rel     string      `json:"rel"`
	Kind    string      `json:"kind"`
	Token   string      `json:"token"`
	BaseSHA string      `json:"baseSha"`
	Absent  [3]bool     `json:"absent"`
	Masked  bool        `json:"masked"`
	Rows    []diff3.Row `json:"rows"`
}

type apiState struct {
	Branch          string    `json:"branch"`
	TargetBranch    string    `json:"targetBranch"`
	CommitBranch    string    `json:"commitBranch"`
	AppliedSHA      string    `json:"appliedSha"`
	MovedBaselines  int       `json:"movedBaselines"`
	TargetSHA       string    `json:"targetSha"`
	Error           string    `json:"error"`
	RestartRequired bool      `json:"restartRequired"`
	LastSync        string    `json:"lastSync"`
	Files           []apiFile `json:"files"`
}

type apiWrite struct {
	Message string `json:"message"`
	Branch  string `json:"branch"`
	Files   []struct {
		Rel   string            `json:"rel"`
		Token string            `json:"token"`
		Rows  map[string]string `json:"rows"`
	} `json:"files"`
}

// version content; nil = file absent.
type file struct {
	base, live, target []byte
	baseSHA            string
	masked             bool
}

func text(lines ...string) []byte { return []byte(strings.Join(lines, "\n") + "\n") }

func manyLines(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s %d", prefix, i+1)
	}
	return out
}

func fixture() map[string]*file {
	gitopsBase := append([]string{
		"# GitOps", "",
		"This folder mirrors `/config` on the Home Assistant host.",
		"Click-ops changes are promoted here by ha-git-ops.", "",
		"## Layout",
		"- `automations.yaml`, `scripts.yaml`, `scenes.yaml`",
		"- `dashboards/` — storage-mode Lovelace dashboards", "",
		"## Secrets",
	}, manyLines("Notes line", 20)...)
	gitopsTarget := append([]string(nil), gitopsBase...)
	gitopsTarget[2] = "This directory mirrors `/config` on the Home Assistant host."
	gitopsTarget = append(gitopsTarget[:8], append([]string{"- `registries/` — areas, floors and labels"}, gitopsTarget[8:]...)...)

	autoBase := append(manyLines("# automation preamble", 11),
		"- id: '1718822400001'",
		"  alias: Porch light at sunset",
		"  trigger:",
		"  - platform: sun",
		"    event: sunset",
		"  action:",
		"  - service: light.turn_on")
	autoLive := append(manyLines("# automation preamble", 11),
		"- id: '1718822400001'",
		"  alias: Porch light at sunset",
		"  trigger:",
		"  - platform: sun",
		"    event: sunset",
		`    offset: "-00:15:00"`,
		"  action:",
		"  - service: light.turn_on",
		"    target:",
		"      entity_id: light.porch")

	scripts := []string{
		"goodnight:",
		"  alias: Goodnight",
		"  sequence:",
		"  - service: light.turn_off",
		"    entity_id: all",
		"  - service: lock.lock",
		"    target:",
		"      entity_id: lock.front_door",
	}
	scriptsLive := append(append(append([]string(nil), scripts[:4]...), "    target:", "      entity_id: all"), scripts[5:]...)
	scriptsTarget := append(append([]string(nil), scripts...),
		"  - service: alarm_control_panel.alarm_arm_night",
		"    target:",
		"      entity_id: alarm_control_panel.home")

	sensors := []string{
		"- platform: template",
		"  sensors:",
		"    heating_degree_days:",
		"      friendly_name: Heating degree days",
		"      unit_of_measurement: HDD",
	}
	sensorsTarget := append(append([]string(nil), sensors...),
		"    cooling_degree_days:",
		"      friendly_name: Cooling degree days",
		"      unit_of_measurement: CDD")

	autoFeature := append(append([]string(nil), autoLive...), "  mode: single")
	branchTargets = map[string]map[string][]byte{
		"feature/porch-offset": {"automations.yaml": text(autoFeature...)},
		"gitops/drift":         {"scripts.yaml": text(scriptsLive...)},
	}

	return map[string]*file{
		"GITOPS.md":        {base: text(gitopsBase...), live: nil, target: text(gitopsTarget...)},
		"scripts.yaml":     {base: text(scripts...), live: text(scriptsLive...), target: text(scriptsTarget...)},
		"automations.yaml": {base: text(autoBase...), live: text(autoLive...), target: text(autoBase...)},
		"sensors.yaml":     {base: text(sensors...), live: text(sensors...), target: text(sensorsTarget...)},
		"secrets.yaml": {
			base:   text("mqtt_password: •••• 3f9a1c", "api_key: •••• 77b20e"),
			live:   text("mqtt_password: •••• 3f9a1c", "api_key: •••• d41e09"),
			target: text("mqtt_password: •••• 3f9a1c", "api_key: •••• 77b20e"),
			masked: true,
		},
	}
}

// branchTargets overrides the main target per file on other branches;
// set by fixture.
var branchTargets map[string]map[string][]byte

// head is a branch's target content; a rel missing from targets is absent.
type head struct {
	sha     string
	targets map[string][]byte
}

func same(a, b []byte) bool { return (a == nil) == (b == nil) && string(a) == string(b) }

func token(vs ...[]byte) string {
	h := sha256.New()
	for _, v := range vs {
		if v == nil {
			h.Write([]byte{0})
			continue
		}
		fmt.Fprintf(h, "\x01%d:", len(v))
		h.Write(v)
	}
	return hex.EncodeToString(h.Sum(nil))[:20]
}

type store struct {
	mu        sync.Mutex
	files     map[string]*file
	applied   string
	view      string // the target branch
	targetSHA string
	heads     map[string]*head // every branch but view
	commits   int
	lastSync  string
}

func newStore() *store {
	s := &store{files: fixture(), applied: "bbcdeb44f0c2", view: "main", targetSHA: "8770cc5e1a9d", heads: map[string]*head{}}
	for _, f := range s.files {
		f.baseSHA = s.applied
	}
	for name, over := range branchTargets {
		sum := sha256.Sum256([]byte(name))
		h := &head{sha: hex.EncodeToString(sum[:])[:12], targets: map[string][]byte{}}
		for rel, f := range s.files {
			h.targets[rel] = f.target
		}
		for rel, t := range over {
			h.targets[rel] = t
		}
		s.heads[name] = h
	}
	s.lastSync = time.Now().Format("2006-01-02 15:04:05")
	return s
}

func rows(f *file) []diff3.Row {
	return diff3.Align(diff3.Lines(f.base), diff3.Lines(f.live), diff3.Lines(f.target))
}

func (s *store) state() apiState {
	st := apiState{Branch: "main", TargetBranch: s.view, CommitBranch: "main", AppliedSHA: s.applied, TargetSHA: s.targetSHA, LastSync: s.lastSync, Files: []apiFile{}}
	rels := make([]string, 0, len(s.files))
	for rel := range s.files {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		f := s.files[rel]
		if same(f.live, f.target) {
			continue
		}
		lc, tc := !same(f.live, f.base), !same(f.target, f.base)
		kind := "target"
		switch {
		case lc && tc:
			kind = "both"
		case lc:
			kind = "live"
		}
		st.Files = append(st.Files, apiFile{
			Rel: rel, Kind: kind, Token: token(f.base, f.live, f.target), BaseSHA: f.baseSHA,
			Absent: [3]bool{f.base == nil, f.live == nil, f.target == nil},
			Masked: f.masked, Rows: rows(f),
		})
	}
	return st
}

// write composes each file's col from the request picks, or reports why
// the request is refused.
func (s *store) write(body apiWrite, col diff3.Col, allowed ...diff3.Col) (map[string][]byte, int, error) {
	out := map[string][]byte{}
	for _, p := range body.Files {
		f, ok := s.files[p.Rel]
		if !ok || token(f.base, f.live, f.target) != p.Token {
			return nil, http.StatusConflict, fmt.Errorf("%s: changed since the panel loaded it — refresh and review again", p.Rel)
		}
		rs := rows(f)
		picks := map[int]diff3.Col{}
		for k, v := range p.Rows {
			i, err := strconv.Atoi(k)
			c, ok := diff3.ParseCol(v)
			known := false
			for _, a := range allowed {
				known = known || c == a
			}
			if err != nil || !ok || !known || i < 0 || i >= len(rs) {
				return nil, http.StatusBadRequest, fmt.Errorf("%s: invalid pick %s=%s", p.Rel, k, v)
			}
			picks[i] = c
		}
		out[p.Rel] = diff3.Compose(rs, col, picks)
	}
	return out, 0, nil
}

// switchTo makes branch the target, the way Reconciler.View does.
func (s *store) switchTo(branch string) error {
	if branch == "" {
		branch = "main"
	}
	if branch == s.view {
		return nil
	}
	next, ok := s.heads[branch]
	if !ok {
		return fmt.Errorf("git fetch: fatal: couldn't find remote ref refs/heads/%s", branch)
	}
	cur := &head{sha: s.targetSHA, targets: map[string][]byte{}}
	for rel, f := range s.files {
		cur.targets[rel] = f.target
		f.target = next.targets[rel]
	}
	s.heads[s.view] = cur
	delete(s.heads, branch)
	s.view, s.targetSHA = branch, next.sha
	s.settle()
	return nil
}

// settle advances baselines the way the reconciler does: only on main.
func (s *store) settle() {
	s.lastSync = time.Now().Format("2006-01-02 15:04:05")
	if s.view != "main" {
		return
	}
	behind := false
	for _, f := range s.files {
		if same(f.live, f.target) {
			f.base, f.baseSHA = f.target, s.targetSHA
		} else if !same(f.base, f.target) {
			behind = true
		}
	}
	if !behind {
		s.applied = s.targetSHA
	}
}

func main() {
	s := newStore()
	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, req *http.Request) {
		src, err := os.ReadFile("page.html")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(src)
	})
	mux.HandleFunc("GET /reset", func(w http.ResponseWriter, req *http.Request) {
		fresh := newStore()
		s.mu.Lock()
		s.files, s.applied, s.view, s.targetSHA, s.heads, s.commits, s.lastSync =
			fresh.files, fresh.applied, fresh.view, fresh.targetSHA, fresh.heads, fresh.commits, fresh.lastSync
		s.mu.Unlock()
		http.Redirect(w, req, "./", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, req *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		writeJSON(w, http.StatusOK, s.state())
	})
	mux.HandleFunc("GET /api/branches", func(w http.ResponseWriter, req *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		type branch struct {
			Name string `json:"name"`
			SHA  string `json:"sha"`
		}
		bs := []branch{{s.view, s.targetSHA}}
		for name, h := range s.heads {
			bs = append(bs, branch{name, h.sha})
		}
		sort.Slice(bs, func(i, j int) bool { return bs[i].Name < bs[j].Name })
		time.Sleep(300 * time.Millisecond) // ls-remote is a network round trip
		writeJSON(w, http.StatusOK, map[string]any{"branches": bs})
	})
	mux.HandleFunc("POST /api/view", func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Branch string `json:"branch"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.switchTo(body.Branch); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "state": s.state()})
			return
		}
		writeJSON(w, http.StatusOK, s.state())
	})
	mux.HandleFunc("POST /api/refresh", func(w http.ResponseWriter, req *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.lastSync = time.Now().Format("2006-01-02 15:04:05")
		writeJSON(w, http.StatusOK, s.state())
	})
	post := func(col diff3.Col, allowed []diff3.Col, commit bool) http.HandlerFunc {
		return func(w http.ResponseWriter, req *http.Request) {
			var body apiWrite
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			out, status, err := s.write(body, col, allowed...)
			if err != nil {
				writeJSON(w, status, map[string]any{"error": err.Error(), "state": s.state()})
				return
			}
			if commit && s.view != "main" && body.Branch == "main" {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the target is " + s.view +
					", so commits build on it — switch the target back to main to commit there", "state": s.state()})
				return
			}
			if commit && body.Branch != "" && body.Branch != s.view {
				// A proposal: the target doesn't move.
				log.Printf("commit to %s: %q %v", body.Branch, body.Message, len(out))
				writeJSON(w, http.StatusOK, s.state())
				return
			}
			for rel, content := range out {
				if commit {
					s.files[rel].target = content
				} else {
					s.files[rel].live = content
				}
			}
			if commit {
				s.commits++
				s.targetSHA = fmt.Sprintf("c0ffee%06d", s.commits)
				log.Printf("commit %s: %q %v", s.targetSHA, body.Message, len(out))
			}
			s.settle()
			writeJSON(w, http.StatusOK, s.state())
		}
	}
	mux.HandleFunc("POST /api/apply", post(diff3.Live, []diff3.Col{diff3.Base, diff3.Target}, false))
	mux.HandleFunc("POST /api/commit", post(diff3.Target, []diff3.Col{diff3.Live}, true))

	addr := "localhost:8098"
	log.Printf("preview at http://%s/ (GET /reset restores the fixture)", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
