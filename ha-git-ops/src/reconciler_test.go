package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TMaYaD/ha-git-ops/diff3"
)

// fakeSupervisor answers every Supervisor call; the core check passes.
type fakeSupervisor struct{ bodies []string }

func (f *fakeSupervisor) RoundTrip(req *http.Request) (*http.Response, error) {
	call := req.Method + " " + req.URL.Path
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		call += " " + string(b)
	}
	f.bodies = append(f.bodies, call)
	body := "{}"
	if req.URL.Path == "/core/check" {
		body = `{"result":"ok"}`
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
}

func run(t *testing.T, dir, script string) string {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", script, err, out)
	}
	return strings.TrimSpace(string(out))
}

type rig struct {
	t                  *testing.T
	r                  *Reconciler
	sup                *fakeSupervisor
	work, live, origin string
}

func newRig(t *testing.T) *rig {
	base := t.TempDir()
	run(t, base, `git init -q --bare -b main origin.git && git init -q -b main work &&
		cd work && git config user.email t@t && git config user.name t && mkdir ha &&
		printf 'a1\na2\na3\n' > ha/a.yaml && printf 'b1\nb2\n' > ha/b.yaml &&
		git add -A && git commit -qm init && git push -q ../origin.git main`)
	live := filepath.Join(base, "live")
	if err := os.MkdirAll(live, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, base, "cp work/ha/a.yaml work/ha/b.yaml live/")
	sup := &fakeSupervisor{}
	ha := &HA{client: &http.Client{Transport: sup}}
	r := &Reconciler{
		opts: Options{RepoURL: filepath.Join(base, "origin.git"), Branch: "main", Subfolder: "ha",
			CommitName: "ha-git-ops", CommitEmail: "ha@test"},
		ha: ha, repoDir: filepath.Join(base, "repo"), configDir: live,
		stateFile: filepath.Join(base, "state.json"), backupsDir: filepath.Join(base, "backups"),
	}
	return &rig{t: t, r: r, sup: sup, work: filepath.Join(base, "work"), live: live, origin: filepath.Join(base, "origin.git")}
}

func (g *rig) tick() {
	g.t.Helper()
	if err := g.r.Tick(); err != nil {
		g.t.Fatal(err)
	}
}

func (g *rig) file(rel string) FileState {
	g.t.Helper()
	for _, f := range g.r.status.Files {
		if f.Rel == rel {
			return f
		}
	}
	g.t.Fatalf("%s not in status", rel)
	return FileState{}
}

// picksWhere stages every row whose column c differs from own.
func picksWhere(f FileState, own, c diff3.Col) Picks {
	p := Picks{Rel: f.Rel, Token: f.Token, Rows: map[int]diff3.Col{}}
	for i, row := range f.Rows {
		if (row[own] == nil) != (row[c] == nil) || (row[own] != nil && *row[own] != *row[c]) {
			p.Rows[i] = c
		}
	}
	return p
}

func TestThreeWayFlow(t *testing.T) {
	g := newRig(t)
	g.tick()
	if len(g.r.status.Files) != 0 {
		t.Fatalf("fresh baseline should be in sync, got %+v", g.r.status.Files)
	}

	// HA edits a.yaml line 1; git edits a.yaml line 3 and b.yaml.
	if err := os.WriteFile(filepath.Join(g.live, "a.yaml"), []byte("A1\na2\na3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, g.work, `printf 'a1\na2\nA3\n' > ha/a.yaml && printf 'b1\nB2\n' > ha/b.yaml &&
		git commit -qam upstream && git push -q ../origin.git main`)
	g.tick()
	if k := g.file("a.yaml").Kind; k != kindBoth {
		t.Fatalf("a.yaml kind %q", k)
	}
	if k := g.file("b.yaml").Kind; k != kindTarget {
		t.Fatalf("b.yaml kind %q", k)
	}

	// A stale token is refused and nothing is written.
	b := g.file("b.yaml")
	stale := picksWhere(b, diff3.Live, diff3.Target)
	stale.Token = "stale"
	if err := g.r.ApplyLive([]Picks{stale}); !errors.Is(err, ErrStale) {
		t.Fatalf("want ErrStale, got %v", err)
	}

	// Apply b.yaml from target: in sync, its baseline moves ahead alone.
	if err := g.r.ApplyLive([]Picks{picksWhere(b, diff3.Live, diff3.Target)}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(g.live, "b.yaml")); string(got) != "b1\nB2\n" {
		t.Fatalf("live b.yaml = %q", got)
	}
	remote := g.r.status.TargetSHA
	if g.r.state.Baselines["b.yaml"] != remote || g.r.state.AppliedSHA == remote {
		t.Fatalf("want b.yaml baseline at remote only, got %+v", g.r.state)
	}

	// Merge a.yaml: commit HA's line 1, then apply git's line 3.
	a := g.file("a.yaml")
	promote := Picks{Rel: "a.yaml", Token: a.Token, Rows: map[int]diff3.Col{}}
	apply := Picks{Rel: "a.yaml", Rows: map[int]diff3.Col{}}
	for i, row := range a.Rows {
		if row[diff3.Live] != nil && *row[diff3.Live] == "A1" {
			promote.Rows[i] = diff3.Live
		}
	}
	if err := g.r.CommitTarget([]Picks{promote}, "promote A1"); err != nil {
		t.Fatal(err)
	}
	if got := run(t, g.work, "git --git-dir=../origin.git show main:ha/a.yaml"); got != "A1\na2\nA3" {
		t.Fatalf("pushed a.yaml = %q", got)
	}
	a = g.file("a.yaml")
	apply.Token = a.Token
	for i, row := range a.Rows {
		if row[diff3.Target] != nil && *row[diff3.Target] == "A3" {
			apply.Rows[i] = diff3.Target
		}
	}
	if err := g.r.ApplyLive([]Picks{apply}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(g.live, "a.yaml")); string(got) != "A1\na2\nA3\n" {
		t.Fatalf("live a.yaml = %q", got)
	}
	if len(g.r.status.Files) != 0 || g.r.state.AppliedSHA != g.r.status.TargetSHA || g.r.state.Baselines != nil {
		t.Fatalf("want everything in sync at head, got files=%v state=%+v", g.r.status.Files, g.r.state)
	}
}

func TestPicksAreValidatedPerBuffer(t *testing.T) {
	g := newRig(t)
	g.tick()
	run(t, g.work, `printf 'b1\nB2\n' > ha/b.yaml && git commit -qam up && git push -q ../origin.git main`)
	g.tick()
	b := g.file("b.yaml")
	bad := Picks{Rel: "b.yaml", Token: b.Token, Rows: map[int]diff3.Col{0: diff3.Live}}
	if err := g.r.ApplyLive([]Picks{bad}); err == nil || !strings.Contains(err.Error(), "invalid pick") {
		t.Fatalf("live may not pick from live, got %v", err)
	}
	unknown := Picks{Rel: "../../etc/passwd", Token: b.Token}
	if err := g.r.ApplyLive([]Picks{unknown}); !errors.Is(err, ErrStale) {
		t.Fatalf("unknown files are refused, got %v", err)
	}
}

func TestMaskLine(t *testing.T) {
	key := []byte("k")
	if got := maskLine(key, "api_key: hunter2"); !strings.HasPrefix(got, "api_key: •••• ") || strings.Contains(got, "hunter2") {
		t.Fatalf("got %q", got)
	}
	if maskLine(key, "a: x") == maskLine(key, "a: y") || maskLine(key, "a: x") != maskLine(key, "a:  x") {
		t.Fatal("tags must compare values")
	}
	for _, keep := range []string{"# comment", "nested:", ""} {
		if maskLine(key, keep) != keep {
			t.Fatalf("%q should pass through", keep)
		}
	}
}

// restart simulates an add-on restart with changed options: a fresh
// reconciler over the same /data, loading persisted state.
func (g *rig) restart(change func(*Options)) {
	g.t.Helper()
	old := g.r
	r := &Reconciler{opts: old.opts, ha: old.ha, repoDir: old.repoDir, configDir: old.configDir,
		stateFile: old.stateFile, backupsDir: old.backupsDir}
	change(&r.opts)
	if raw, err := os.ReadFile(r.stateFile); err == nil {
		if err := json.Unmarshal(raw, &r.state); err != nil {
			g.t.Fatal(err)
		}
	}
	g.r = r
}

func (g *rig) gitOut(args ...string) string {
	g.t.Helper()
	out, err := g.r.git(args...)
	if err != nil {
		g.t.Fatal(err)
	}
	return strings.TrimSpace(out)
}

func (g *rig) notified(title string) bool {
	for _, c := range g.sup.bodies {
		if strings.Contains(c, title) {
			return true
		}
	}
	return false
}

// The bug this guards: after moving the config to another repo, branch
// and folder, the old clone kept fetching from the old origin and the
// old baseline was diffed against the new paths.
func TestRetargetToMovedConfig(t *testing.T) {
	g := newRig(t)
	g.tick()
	old := g.r.state.AppliedSHA

	base := filepath.Dir(g.work)
	run(t, base, `git init -q --bare -b main moved.git && git init -q -b ha moved &&
		cd moved && git config user.email t@t && git config user.name t && mkdir config &&
		printf 'a1\na2\na3\n' > config/a.yaml && printf 'b1\nb2\nb3\n' > config/b.yaml &&
		git add -A && git commit -qm moved && git push -q ../moved.git ha`)
	g.restart(func(o *Options) {
		o.RepoURL, o.Branch, o.Subfolder = filepath.Join(base, "moved.git"), "ha", "config"
	})
	g.tick()

	if got := g.gitOut("remote", "get-url", "origin"); got != filepath.Join(base, "moved.git") {
		t.Fatalf("origin = %s", got)
	}
	if got := g.gitOut("sparse-checkout", "list"); got != "config" {
		t.Fatalf("sparse = %s", got)
	}
	head := g.r.status.TargetSHA
	want := targetID{RepoURL: filepath.Join(base, "moved.git"), Branch: "ha", Subfolder: "config"}
	if g.r.state.AppliedSHA != head || head == old || g.r.state.Target == nil || *g.r.state.Target != want {
		t.Fatalf("want baseline at new head with new target, got %+v", g.r.state)
	}
	// Only b.yaml differs (live lacks b3), and against the new baseline it
	// reads as a live change, not a full deletion plus a full addition.
	if len(g.r.status.Files) != 1 || g.file("b.yaml").Kind != kindLive {
		t.Fatalf("files = %+v", g.r.status.Files)
	}
	if !g.notified("GitOps: target changed") {
		t.Fatal("expected a target-changed notification")
	}
}

func TestRetargetBranchOnly(t *testing.T) {
	g := newRig(t)
	g.tick()
	old := g.r.state.AppliedSHA
	run(t, g.work, `git checkout -qb next && printf 'b1\nB2\n' > ha/b.yaml &&
		git commit -qam next && git push -q ../origin.git next`)
	g.restart(func(o *Options) { o.Branch = "next" })
	g.tick()
	if g.r.state.AppliedSHA == old || g.r.state.AppliedSHA != g.r.status.TargetSHA || g.r.state.Target.Branch != "next" {
		t.Fatalf("want baseline at next's head, got %+v", g.r.state)
	}
}

// State written before targets were recorded: a stale clone means the
// options moved, a matching one keeps its baseline.
func TestRetargetLegacyState(t *testing.T) {
	g := newRig(t)
	g.tick()
	old := g.r.state.AppliedSHA
	legacy := func() {
		if err := os.WriteFile(g.r.stateFile, []byte(`{"applied_sha":"`+old+`"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	legacy()
	g.restart(func(*Options) {})
	g.tick()
	if g.r.state.AppliedSHA != old || g.r.state.Target == nil || g.r.state.Target.Subfolder != "ha" {
		t.Fatalf("matching clone should keep its baseline, got %+v", g.r.state)
	}

	run(t, g.work, `mkdir -p ha2 && git mv ha/a.yaml ha2/a.yaml && printf 'b1\nb2\n' > ha2/b.yaml &&
		git add -A && git commit -qm move && git push -q ../origin.git main`)
	legacy()
	g.restart(func(o *Options) { o.Subfolder = "ha2" })
	g.tick()
	if g.r.state.AppliedSHA == old || g.gitOut("sparse-checkout", "list") != "ha2" {
		t.Fatalf("stale legacy clone should re-clone and re-baseline, got %+v", g.r.state)
	}
}

func TestLostCloneKeepsBaseline(t *testing.T) {
	g := newRig(t)
	g.tick()
	old := g.r.state.AppliedSHA
	run(t, g.work, `printf 'b1\nB2\n' > ha/b.yaml && git commit -qam up && git push -q ../origin.git main`)
	if err := os.RemoveAll(g.r.repoDir); err != nil {
		t.Fatal(err)
	}
	g.restart(func(*Options) {})
	g.tick()
	if g.r.state.AppliedSHA != old || g.file("b.yaml").Kind != kindTarget {
		t.Fatalf("a re-clone of the same target must keep the baseline, got %+v", g.r.state)
	}
}
