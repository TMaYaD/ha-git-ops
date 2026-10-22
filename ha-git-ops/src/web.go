package main

// Ingress UI: a single page that loads the three-way state as JSON and
// posts staged picks back. Served under HA ingress — all URLs are
// relative so the ingress path prefix is transparent. The secrets file
// travels masked: keys plus a keyed hash per value, so equal values
// still compare equal without revealing them.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/TMaYaD/ha-git-ops/diff3"
)

//go:embed page.html
var page []byte

// API shapes; cmd/preview mirrors them.
type apiFile struct {
	Rel     string `json:"rel"`
	Kind    string `json:"kind"`
	Token   string `json:"token"`
	BaseSHA string `json:"baseSha"`
	// BaseMoved marks last-applied taken from a previous target (another
	// repo, branch or subfolder) that this file hasn't converged away from.
	BaseMoved bool        `json:"baseMoved"`
	Absent    [3]bool     `json:"absent"` // base, live, target
	Masked    bool        `json:"masked"`
	Rows      []diff3.Row `json:"rows"`
}

type apiState struct {
	Branch          string    `json:"branch"`       // tracked
	TargetBranch    string    `json:"targetBranch"` // shown as target: tracked, or one switched to
	CommitBranch    string    `json:"commitBranch"` // default for the panel's branch field
	AppliedSHA      string    `json:"appliedSha"`
	MovedBaselines  int       `json:"movedBaselines"`
	TargetSHA       string    `json:"targetSha"`
	Error           string    `json:"error"`
	RestartRequired bool      `json:"restartRequired"`
	LastSync        string    `json:"lastSync"`
	Files           []apiFile `json:"files"`
}

type apiPicks struct {
	Rel   string            `json:"rel"`
	Token string            `json:"token"`
	Rows  map[string]string `json:"rows"`
}

type apiWrite struct {
	Message string     `json:"message"`
	Branch  string     `json:"branch"` // commit only; "" for the default
	Files   []apiPicks `json:"files"`
}

func NewWebUI(rec *Reconciler, branch string) http.Handler {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		log.Fatalf("mask key: %v", err)
	}
	state := func() apiState {
		st, applied, moved := rec.Snapshot()
		out := apiState{
			Branch: branch, TargetBranch: st.Branch, CommitBranch: rec.CommitBranch(), AppliedSHA: applied, MovedBaselines: moved,
			TargetSHA: st.TargetSHA, Error: st.Error,
			RestartRequired: st.RestartRequired, LastSync: st.LastSync,
			Files: []apiFile{},
		}
		for _, f := range st.Files {
			af := apiFile{
				Rel: f.Rel, Kind: f.Kind, Token: f.Token, BaseSHA: f.BaseCommit, BaseMoved: f.BaseMoved,
				Absent: [3]bool{f.Base == nil, f.Live == nil, f.Target == nil},
				Rows:   f.Rows,
			}
			if f.Rel == secretsPlain {
				af.Masked = true
				af.Rows = maskRows(key, f.Rows)
			}
			out.Files = append(out.Files, af)
		}
		return out
	}
	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	fail := func(w http.ResponseWriter, err error) {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, ErrStale):
			status = http.StatusConflict
		case errors.Is(err, ErrRolledBack):
			status = http.StatusUnprocessableEntity
		}
		writeJSON(w, status, map[string]any{"error": err.Error(), "state": state()})
	}
	decode := func(req *http.Request) (apiWrite, []Picks, error) {
		var body apiWrite
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return body, nil, err
		}
		var picks []Picks
		for _, f := range body.Files {
			p := Picks{Rel: f.Rel, Token: f.Token, Rows: map[int]diff3.Col{}}
			for k, v := range f.Rows {
				i, err := strconv.Atoi(k)
				c, ok := diff3.ParseCol(v)
				if err != nil || !ok {
					return body, nil, errors.New("bad pick " + k + "=" + v)
				}
				p.Rows[i] = c
			}
			picks = append(picks, p)
		}
		return body, picks, nil
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(page)
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, req *http.Request) {
		writeJSON(w, http.StatusOK, state())
	})
	mux.HandleFunc("POST /api/refresh", func(w http.ResponseWriter, req *http.Request) {
		if err := rec.RefreshNow(); err != nil {
			rec.SetError(err.Error())
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, state())
	})
	mux.HandleFunc("GET /api/branches", func(w http.ResponseWriter, req *http.Request) {
		bs, err := rec.Branches()
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
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
		if err := rec.View(body.Branch); err != nil {
			log.Printf("switch target to %q failed: %v", body.Branch, err)
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, state())
	})
	mux.HandleFunc("POST /api/apply", func(w http.ResponseWriter, req *http.Request) {
		_, picks, err := decode(req)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err := rec.ApplyLive(picks); err != nil {
			log.Printf("apply failed: %v", err)
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, state())
	})
	mux.HandleFunc("POST /api/commit", func(w http.ResponseWriter, req *http.Request) {
		body, picks, err := decode(req)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err := rec.CommitTarget(picks, body.Message, body.Branch); err != nil {
			log.Printf("commit failed: %v", err)
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, state())
	})
	// Any panel request keeps a switched target alive.
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasPrefix(req.URL.Path, "/api/") {
			rec.Seen()
		}
		mux.ServeHTTP(w, req)
	})
}

// maskRows replaces each secret value with a short keyed hash, keeping
// keys, indentation and comments readable.
func maskRows(key []byte, rows []diff3.Row) []diff3.Row {
	out := make([]diff3.Row, len(rows))
	for i, r := range rows {
		for c, s := range r {
			if s == nil {
				continue
			}
			m := maskLine(key, *s)
			out[i][c] = &m
		}
	}
	return out
}

func maskLine(key []byte, line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return line
	}
	k, v, found := strings.Cut(line, ":")
	if !found {
		k, v = "", line
	}
	if strings.TrimSpace(v) == "" {
		return line
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(strings.TrimSpace(v)))
	tag := "•••• " + hex.EncodeToString(mac.Sum(nil))[:6]
	if !found {
		return tag
	}
	return k + ": " + tag
}
