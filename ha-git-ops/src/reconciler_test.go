package main

import (
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
type fakeSupervisor struct{ calls []string }

func (f *fakeSupervisor) RoundTrip(req *http.Request) (*http.Response, error) {
	f.calls = append(f.calls, req.Method+" "+req.URL.Path)
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
	ha := &HA{client: &http.Client{Transport: &fakeSupervisor{}}}
	r := &Reconciler{
		opts: Options{RepoURL: filepath.Join(base, "origin.git"), Branch: "main", Subfolder: "ha",
			CommitName: "ha-git-ops", CommitEmail: "ha@test"},
		ha: ha, repoDir: filepath.Join(base, "repo"), configDir: live,
		stateFile: filepath.Join(base, "state.json"), backupsDir: filepath.Join(base, "backups"),
	}
	return &rig{t: t, r: r, work: filepath.Join(base, "work"), live: live, origin: filepath.Join(base, "origin.git")}
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
