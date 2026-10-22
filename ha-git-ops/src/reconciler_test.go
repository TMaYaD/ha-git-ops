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
	"time"

	"github.com/TMaYaD/ha-git-ops/diff3"
)

// fakeSupervisor answers every Supervisor call; the core check passes
// unless checkFail holds HA's reason.
type fakeSupervisor struct {
	bodies    []string
	checkFail string
}

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
		if f.checkFail != "" {
			b, _ := json.Marshal(map[string]string{"result": "error", "message": f.checkFail})
			body = string(b)
		}
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
		snapshotDir: filepath.Join(base, "baseline"),
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
	if g.r.state.Baseline["b.yaml"].Commit != remote || g.r.state.AppliedSHA == remote {
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
	if err := g.r.CommitTarget([]Picks{promote}, "promote A1", ""); err != nil {
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
	if len(g.r.status.Files) != 0 || g.r.state.AppliedSHA != g.r.status.TargetSHA {
		t.Fatalf("want everything in sync at head, got files=%v state=%+v", g.r.status.Files, g.r.state)
	}
	g.assertStorePruned()
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
		stateFile: old.stateFile, backupsDir: old.backupsDir, snapshotDir: old.snapshotDir}
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

func (g *rig) kind(rel string) string {
	g.t.Helper()
	for _, f := range g.r.status.Files {
		if f.Rel == rel {
			return f.Kind
		}
	}
	return ""
}

// assertStorePruned checks the baseline store holds exactly the content
// some file's last-applied refers to.
func (g *rig) assertStorePruned() {
	g.t.Helper()
	want := map[string]bool{}
	for _, e := range g.r.state.Baseline {
		want[e.Blob] = true
	}
	entries, err := os.ReadDir(g.r.snapshotDir)
	if err != nil {
		g.t.Fatal(err)
	}
	if len(entries) != len(want) {
		g.t.Fatalf("store has %d blobs, %d referenced", len(entries), len(want))
	}
	for _, d := range entries {
		if !want[d.Name()] {
			g.t.Fatalf("unreferenced blob %s", d.Name())
		}
	}
}

// The bug this guards: after moving the config to another repo, branch
// and folder, the old clone kept fetching from the old origin and the old
// baseline was diffed against the new paths. Last-applied is now stored
// when applied, so it survives the move even when the old repo is gone
// (a rename) along with the old clone.
func TestMoveKeepsLastApplied(t *testing.T) {
	g := newRig(t)
	g.tick()

	// The new home: a.yaml as HA has it, b.yaml moved ahead in git, and a
	// GITOPS.md that never existed before the move.
	base := filepath.Dir(g.work)
	run(t, base, `git init -q --bare -b main moved.git && git init -q -b ha moved &&
		cd moved && git config user.email t@t && git config user.name t && mkdir config &&
		printf 'a1\na2\na3\n' > config/a.yaml && printf 'b1\nB2\n' > config/b.yaml &&
		printf '# GitOps\n' > config/GITOPS.md &&
		git add -A && git commit -qm moved && git push -q ../moved.git ha &&
		rm -rf ../origin.git`)
	if err := os.RemoveAll(g.r.repoDir); err != nil {
		t.Fatal(err)
	}
	g.restart(func(o *Options) {
		o.RepoURL, o.Branch, o.Subfolder = filepath.Join(base, "moved.git"), "ha", "config"
	})
	g.tick()

	if got := g.gitOut("sparse-checkout", "list"); got != "config" {
		t.Fatalf("sparse = %s", got)
	}
	want := targetID{RepoURL: filepath.Join(base, "moved.git"), Branch: "ha", Subfolder: "config"}
	if g.r.state.Target == nil || *g.r.state.Target != want {
		t.Fatalf("target = %+v", g.r.state.Target)
	}
	b := g.file("b.yaml")
	if string(b.Base) != "b1\nb2\n" || !b.BaseMoved || b.Kind != kindTarget {
		t.Fatalf("b.yaml should keep last-applied from the old target and read as changed in git, got base=%q moved=%v kind=%q",
			b.Base, b.BaseMoved, b.Kind)
	}
	if k := g.kind("GITOPS.md"); k != kindTarget || g.file("GITOPS.md").Base != nil {
		t.Fatalf("GITOPS.md is new in the target: kind %q", k)
	}
	if k := g.kind("a.yaml"); k != "" || g.r.state.Baseline["a.yaml"].Target != want {
		t.Fatalf("a.yaml matches, so it converged on the new target: kind %q", k)
	}
	if !g.notified("GitOps: target changed") {
		t.Fatal("expected a target-changed notification")
	}

	// Applying the new target retires the old last-applied.
	for _, rel := range []string{"b.yaml", "GITOPS.md"} {
		if err := g.r.ApplyLive([]Picks{picksWhere(g.file(rel), diff3.Live, diff3.Target)}); err != nil {
			t.Fatal(err)
		}
	}
	if len(g.r.status.Files) != 0 {
		t.Fatalf("files = %v", g.r.status.Files)
	}
	for rel, e := range g.r.state.Baseline {
		if e.Target != want {
			t.Fatalf("%s still on the old target", rel)
		}
	}
	g.assertStorePruned()
}

func TestBranchChangeKeepsBaseline(t *testing.T) {
	g := newRig(t)
	g.tick()
	run(t, g.work, `git checkout -qb next && printf 'b1\nB2\n' > ha/b.yaml &&
		git commit -qam next && git push -q ../origin.git next`)
	g.restart(func(o *Options) { o.Branch = "next" })
	g.tick()
	if b := g.file("b.yaml"); string(b.Base) != "b1\nb2\n" || b.Kind != kindTarget || g.r.state.Target.Branch != "next" {
		t.Fatalf("want b.yaml changed in git against the kept baseline, got %+v", b)
	}
}

// 0.6.0 state recorded only commits: it is migrated from whichever clone
// is on disk, before a stale one is replaced.
func TestLegacyStateMigrates(t *testing.T) {
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
	g.sup.bodies = nil
	g.tick()
	if g.r.state.AppliedSHA != old || len(g.r.state.Baseline) != 2 || g.notified("GitOps: target changed") {
		t.Fatalf("matching clone: migrate in place, got %+v", g.r.state)
	}

	run(t, g.work, `mkdir -p ha2 && git mv ha/a.yaml ha2/a.yaml && printf 'b1\nB2\n' > ha2/b.yaml &&
		git add -A && git commit -qm move && git push -q ../origin.git main`)
	legacy()
	g.restart(func(o *Options) { o.Subfolder = "ha2" })
	g.tick()
	b := g.file("b.yaml")
	if g.gitOut("sparse-checkout", "list") != "ha2" || string(b.Base) != "b1\nb2\n" || !b.BaseMoved || b.Kind != kindTarget {
		t.Fatalf("stale clone: migrate from it before re-cloning, got %+v", b)
	}
	if !g.notified("GitOps: target changed") {
		t.Fatal("expected a target-changed notification")
	}
}

// 0.6.0 state whose commits are gone: start over like a first run.
func TestLegacyStateWithoutCommitsResets(t *testing.T) {
	g := newRig(t)
	g.tick()
	if err := os.WriteFile(g.r.stateFile, []byte(`{"applied_sha":"0123456789abcdef0123456789abcdef01234567"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	g.restart(func(*Options) {})
	g.tick()
	if g.r.state.AppliedSHA != g.r.status.TargetSHA || len(g.r.state.Baseline) != 2 || !g.notified("GitOps: baseline reset") {
		t.Fatalf("want a fresh baseline at head, got %+v", g.r.state)
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
	if g.r.state.AppliedSHA != old || g.kind("b.yaml") != kindTarget || g.file("b.yaml").BaseMoved {
		t.Fatalf("a re-clone of the same target must keep the baseline, got %+v", g.r.state)
	}
}

// A clone left on disk from the previous folder is replaced rather than
// fetched as-is.
func TestStaleCloneReplaced(t *testing.T) {
	g := newRig(t)
	g.tick()
	run(t, g.work, `mkdir -p ha2 && git mv ha/a.yaml ha2/a.yaml && printf 'b1\nB2\n' > ha2/b.yaml &&
		git add -A && git commit -qm move && git push -q ../origin.git main`)
	g.restart(func(o *Options) { o.Subfolder = "ha2" })
	g.tick()
	if g.gitOut("sparse-checkout", "list") != "ha2" || g.kind("b.yaml") != kindTarget || g.kind("a.yaml") != "" {
		t.Fatalf("want a fresh clone of ha2, got files=%v", g.r.status.Files)
	}
}

// A re-clone that fails (wrong repo_url, no access) leaves the working
// clone and state as they were, and picks up once the options are fixed.
func TestFailedRecloneKeepsOldClone(t *testing.T) {
	g := newRig(t)
	g.tick()
	before, _ := os.ReadFile(g.r.stateFile)
	origin := g.r.opts.RepoURL
	g.restart(func(o *Options) { o.RepoURL = filepath.Join(filepath.Dir(g.work), "missing.git") })
	if err := g.r.Tick(); err == nil {
		t.Fatal("want a clone error")
	}
	if got := g.gitOut("remote", "get-url", "origin"); got != origin {
		t.Fatalf("old clone replaced: origin %s", got)
	}
	if _, err := os.Stat(g.r.repoDir + ".new"); !os.IsNotExist(err) {
		t.Fatalf("leftover partial clone: %v", err)
	}
	if after, _ := os.ReadFile(g.r.stateFile); string(after) != string(before) {
		t.Fatalf("state changed:\n%s\n%s", before, after)
	}
	g.restart(func(o *Options) { o.RepoURL = origin })
	g.tick()
}

func TestAccessHint(t *testing.T) {
	r := &Reconciler{SSHPub: "ssh-ed25519 AAAA ha-git-ops"}
	github := "ERROR: Repository not found.\nfatal: Could not read from remote repository."
	if h := r.accessHint(github); !strings.Contains(h, "deploy key") || !strings.Contains(h, r.SSHPub) {
		t.Fatalf("hint = %q", h)
	}
	if h := r.accessHint("fatal: couldn't find remote ref main"); h != "" {
		t.Fatalf("unrelated errors get no hint, got %q", h)
	}
}

// originRev resolves rev in the origin repo, "" if absent.
func (g *rig) originRev(rev string) string {
	g.t.Helper()
	return run(g.t, g.work, "git --git-dir=../origin.git rev-parse --verify -q "+rev+" || true")
}

func TestCommitToOtherBranch(t *testing.T) {
	g := newRig(t)
	g.tick()
	main0 := g.originRev("main")
	if err := os.WriteFile(filepath.Join(g.live, "a.yaml"), []byte("A1\na2\nA3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g.tick()
	pick := func(line string) Picks {
		a := g.file("a.yaml")
		p := Picks{Rel: "a.yaml", Token: a.Token, Rows: map[int]diff3.Col{}}
		for i, row := range a.Rows {
			if row[diff3.Live] != nil && *row[diff3.Live] == line {
				p.Rows[i] = diff3.Live
			}
		}
		return p
	}
	show := func(rev string) string {
		return run(t, g.work, "git --git-dir=../origin.git show "+rev+":ha/a.yaml")
	}

	// A new branch starts from the target head; the target doesn't move.
	if err := g.r.CommitTarget([]Picks{pick("A1")}, "propose A1", "gitops/drift"); err != nil {
		t.Fatal(err)
	}
	if g.originRev("main") != main0 || g.r.status.TargetSHA != main0 {
		t.Fatal("target moved")
	}
	if got := show("gitops/drift"); got != "A1\na2\na3" {
		t.Fatalf("branch a.yaml = %q", got)
	}
	if g.originRev("gitops/drift^") != main0 {
		t.Fatal("branch should start at the target head")
	}
	if g.kind("a.yaml") != kindLive || g.gitOut("rev-parse", "HEAD") != main0 {
		t.Fatalf("want a.yaml still live changed and the clone at the target, got %q", g.kind("a.yaml"))
	}

	// An open branch grows: promoting more lines keeps what it proposes.
	if err := g.r.CommitTarget([]Picks{pick("A3")}, "propose A3", "gitops/drift"); err != nil {
		t.Fatal(err)
	}
	if got := show("gitops/drift"); got != "A1\na2\nA3" {
		t.Fatalf("branch a.yaml = %q", got)
	}
	tip := g.originRev("gitops/drift")
	if g.originRev("gitops/drift~2") != main0 {
		t.Fatal("want two commits on top of the target head")
	}
	// Proposing it again adds nothing.
	if err := g.r.CommitTarget([]Picks{pick("A3")}, "again", "gitops/drift"); err != nil {
		t.Fatal(err)
	}
	if g.originRev("gitops/drift") != tip {
		t.Fatal("a repeated proposal should not add a commit")
	}

	// A conflicting change is refused and leaves the branch alone.
	if err := os.WriteFile(filepath.Join(g.live, "a.yaml"), []byte("Z1\na2\nA3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g.tick()
	if err := g.r.CommitTarget([]Picks{pick("Z1")}, "conflict", "gitops/drift"); err == nil ||
		!strings.Contains(err.Error(), "already changes the same lines") {
		t.Fatalf("want a conflict, got %v", err)
	}
	if g.originRev("gitops/drift") != tip || g.gitOut("rev-parse", "HEAD") != main0 {
		t.Fatal("a refused proposal must leave the branch and the clone alone")
	}

	// Merging the PR converges the file.
	if err := os.WriteFile(filepath.Join(g.live, "a.yaml"), []byte("A1\na2\nA3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, g.work, "git fetch -q ../origin.git gitops/drift && git merge -q --ff-only FETCH_HEAD && git push -q ../origin.git main")
	g.tick()
	if len(g.r.status.Files) != 0 {
		t.Fatalf("want in sync after the merge, got %+v", g.r.status.Files)
	}

	// A merged branch starts over from the new target head, by default
	// through commit_branch.
	g.r.opts.CommitBranch = "gitops/drift"
	if err := os.WriteFile(filepath.Join(g.live, "b.yaml"), []byte("b1\nB2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g.tick()
	main1 := g.originRev("main")
	if err := g.r.CommitTarget([]Picks{picksWhere(g.file("b.yaml"), diff3.Target, diff3.Live)}, "", ""); err != nil {
		t.Fatal(err)
	}
	if g.originRev("main") != main1 || g.originRev("gitops/drift^") != main1 {
		t.Fatal("want the merged branch fast-forwarded past the new target head")
	}

	if err := g.r.CommitTarget(nil, "", "bad..name"); err == nil || !strings.Contains(err.Error(), "invalid branch") {
		t.Fatalf("want an invalid branch error, got %v", err)
	}
}

func TestPullFromOtherBranch(t *testing.T) {
	g := newRig(t)
	g.tick()
	main0 := g.originRev("main")
	base0 := g.r.state.Baseline["a.yaml"]
	run(t, g.work, `git checkout -qb feature && printf 'a1\na2\nA3\n' > ha/a.yaml &&
		git commit -qam feature && git push -q ../origin.git feature && git checkout -q main`)
	feature0 := g.originRev("feature")

	bs, err := g.r.Branches()
	if err != nil || len(bs) != 2 || bs[0] != (Branch{"feature", feature0}) || bs[1].Name != "main" {
		t.Fatalf("branches = %+v, %v", bs, err)
	}
	if err := g.r.View("nope"); err == nil || g.r.view != "" {
		t.Fatalf("an unknown branch is refused and keeps the target, got %v", err)
	}
	if err := g.r.View("feature"); err != nil {
		t.Fatal(err)
	}
	if g.r.status.Branch != "feature" || g.r.status.TargetSHA != feature0 || g.kind("a.yaml") != kindTarget {
		t.Fatalf("want feature as target, got %+v", g.r.status)
	}

	// Pulling feature's line into live records nothing as last-applied.
	if err := g.r.ApplyLive([]Picks{picksWhere(g.file("a.yaml"), diff3.Live, diff3.Target)}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(g.live, "a.yaml")); string(got) != "a1\na2\nA3\n" {
		t.Fatalf("live a.yaml = %q", got)
	}
	if len(g.r.status.Files) != 0 || g.r.state.Baseline["a.yaml"] != base0 || g.r.state.AppliedSHA != main0 {
		t.Fatalf("want in sync with feature and last-applied untouched, got %+v %+v", g.r.status.Files, g.r.state)
	}

	// Commits build on feature and go there by default; never to main.
	if err := os.WriteFile(filepath.Join(g.live, "a.yaml"), []byte("A1\na2\nA3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g.tick()
	promote := picksWhere(g.file("a.yaml"), diff3.Target, diff3.Live)
	if err := g.r.CommitTarget([]Picks{promote}, "", "main"); err == nil || !strings.Contains(err.Error(), "switch the target back") {
		t.Fatalf("want commits to main refused, got %v", err)
	}
	if err := g.r.CommitTarget([]Picks{promote}, "A1", ""); err != nil {
		t.Fatal(err)
	}
	if got := run(t, g.work, "git --git-dir=../origin.git show feature:ha/a.yaml"); got != "A1\na2\nA3" {
		t.Fatalf("feature a.yaml = %q", got)
	}
	if g.originRev("feature^") != feature0 || g.originRev("main") != main0 || g.gitOut("rev-parse", "HEAD") != main0 {
		t.Fatal("want one commit on feature, main and the clone untouched")
	}
	if g.r.status.TargetSHA != g.originRev("feature") || len(g.r.status.Files) != 0 {
		t.Fatalf("want in sync with the new feature head, got %+v", g.r.status)
	}

	// Back on main, what was pulled is a live change.
	if err := g.r.View(""); err != nil {
		t.Fatal(err)
	}
	if g.r.status.Branch != "main" || g.kind("a.yaml") != kindLive {
		t.Fatalf("want a.yaml live changed on main, got %+v", g.r.status)
	}

	// An idle panel, or a deleted branch, falls back to main.
	if err := g.r.View("feature"); err != nil {
		t.Fatal(err)
	}
	g.r.seen.Store(time.Now().Add(-viewIdle - time.Minute).UnixNano())
	g.tick()
	if g.r.view != "" || g.r.status.Branch != "main" {
		t.Fatalf("want main after idling, got %q", g.r.status.Branch)
	}
	if err := g.r.View("feature"); err != nil {
		t.Fatal(err)
	}
	run(t, g.work, "git push -q ../origin.git :feature")
	g.tick()
	if g.r.status.Branch != "main" || !strings.Contains(g.r.status.Error, "back on main") {
		t.Fatalf("want main with a note, got %q %q", g.r.status.Branch, g.r.status.Error)
	}
}
