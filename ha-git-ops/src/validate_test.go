package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TMaYaD/ha-git-ops/diff3"
)

func (g *rig) coreChecks() int {
	n := 0
	for _, c := range g.sup.bodies {
		if strings.HasPrefix(c, "POST /core/check") {
			n++
		}
	}
	return n
}

func (g *rig) liveFile(rel string) string {
	g.t.Helper()
	b, err := os.ReadFile(filepath.Join(g.live, rel))
	if err != nil {
		g.t.Fatal(err)
	}
	return string(b)
}

// One file that doesn't parse blocks the whole apply before anything is
// written — the valid file alongside it included.
func TestInvalidFileBlocksApply(t *testing.T) {
	g := newRig(t)
	g.tick()
	run(t, g.work, `printf 'a1\na2: [unclosed\n' > ha/a.yaml && printf 'b1\nB2\n' > ha/b.yaml &&
		git commit -qam bad && git push -q ../origin.git main`)
	g.tick()
	a, b := g.file("a.yaml"), g.file("b.yaml")
	err := g.r.ApplyLive([]Picks{picksWhere(a, diff3.Live, diff3.Target), picksWhere(b, diff3.Live, diff3.Target)})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "a.yaml") {
		t.Fatalf("want ErrInvalid naming a.yaml, got %v", err)
	}
	if got := g.liveFile("a.yaml"); got != "a1\na2\na3\n" {
		t.Fatalf("live a.yaml = %q", got)
	}
	if got := g.liveFile("b.yaml"); got != "b1\nb2\n" {
		t.Fatalf("live b.yaml = %q", got)
	}
	if n := g.coreChecks(); n != 0 {
		t.Fatalf("core check ran %d times for a blocked apply", n)
	}
	if _, err := os.Stat(g.r.backupsDir); !os.IsNotExist(err) {
		t.Fatalf("blocked apply left backups: %v", err)
	}
	if !g.notified("GitOps apply blocked") {
		t.Fatal("blocked apply not notified")
	}
	if len(g.r.status.Files) != 2 {
		t.Fatalf("both files should still await a decision, got %+v", g.r.status.Files)
	}
}

// A failed core check restores live and carries HA's reason.
func TestCoreCheckFailureRollsBack(t *testing.T) {
	g := newRig(t)
	g.tick()
	run(t, g.work, `printf 'b1\nB2\n' > ha/b.yaml && git commit -qam up && git push -q ../origin.git main`)
	g.tick()
	g.sup.checkFail = "Invalid config for 'automation' at b.yaml, line 2"
	err := g.r.ApplyLive([]Picks{picksWhere(g.file("b.yaml"), diff3.Live, diff3.Target)})
	if !errors.Is(err, ErrRolledBack) || !strings.Contains(err.Error(), "Invalid config for 'automation'") {
		t.Fatalf("want ErrRolledBack with HA's reason, got %v", err)
	}
	if got := g.liveFile("b.yaml"); got != "b1\nb2\n" {
		t.Fatalf("live b.yaml = %q, want restored", got)
	}

	// The same failure again isn't re-notified; a success resets that.
	before := len(g.sup.bodies)
	_ = g.r.ApplyLive([]Picks{picksWhere(g.file("b.yaml"), diff3.Live, diff3.Target)})
	for _, c := range g.sup.bodies[before:] {
		if strings.Contains(c, "rolled back") {
			t.Fatal("identical failure notified twice")
		}
	}
	g.sup.checkFail = ""
	if err := g.r.ApplyLive([]Picks{picksWhere(g.file("b.yaml"), diff3.Live, diff3.Target)}); err != nil {
		t.Fatal(err)
	}
	if g.r.alerted != "" {
		t.Fatalf("success should clear the alert, got %q", g.r.alerted)
	}
}

func TestCheck(t *testing.T) {
	g := newRig(t)
	for _, c := range []struct {
		rel, content, want string // want "" means valid
	}{
		{"automations.yaml", "- alias: x\n  action: !include a.yaml\n  pw: !secret pw\n", ""},
		{"automations.yaml", "a: b\n\tc: d\n", "automations.yaml"},
		{"automations.yaml", "", ""},
		{"notes.txt", "a: [", ""},
		{"x.json", "{", "invalid JSON"},
		{"registry/floors.yaml", "ground:\n  name: Ground\n  level: 0\n", ""},
		{"registry/floors.yaml", "ground:\n  name: Ground\n  colour: red\n", `unknown field "colour"`},
		{"registry/floors.yaml", "- ground\n", "parse desired"},
		{"dashboards/home.yaml", "- views\n", "parse desired config"},
		{"dashboards/home.yaml", "views: []\n", "no live storage dashboard"},
	} {
		err := g.r.check(c.rel, []byte(c.content))
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s %q: want valid, got %v", c.rel, c.content, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s %q: want error containing %q, got %v", c.rel, c.content, c.want, err)
		}
	}
	if err := g.r.check("registry/floors.yaml", nil); err == nil {
		t.Error("deleting a registry file should be refused")
	}
	if err := g.r.check("automations.yaml", nil); err != nil {
		t.Errorf("deleting a file is valid, got %v", err)
	}
}
