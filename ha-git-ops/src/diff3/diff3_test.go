package diff3

import (
	"math/rand"
	"strings"
	"testing"
)

func col(rows []Row, c Col) []string {
	var out []string
	for _, r := range rows {
		if r[c] != nil {
			out = append(out, *r[c])
		}
	}
	return out
}

func show(rows []Row) string {
	var b strings.Builder
	for _, r := range rows {
		for c := range r {
			if c > 0 {
				b.WriteString(" | ")
			}
			if r[c] == nil {
				b.WriteString("·")
			} else {
				b.WriteString(*r[c])
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func TestAlignPreservesEveryVersion(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	words := []string{"a", "b", "c", "d", "e"}
	mutate := func(in []string) []string {
		var out []string
		for _, s := range in {
			switch rng.Intn(6) {
			case 0: // drop
			case 1:
				out = append(out, words[rng.Intn(len(words))])
			case 2:
				out = append(out, s, words[rng.Intn(len(words))])
			default:
				out = append(out, s)
			}
		}
		return out
	}
	for n := 0; n < 500; n++ {
		base := make([]string, rng.Intn(12))
		for i := range base {
			base[i] = words[rng.Intn(len(words))]
		}
		live, target := mutate(base), mutate(base)
		rows := Align(base, live, target)
		for c, want := range [][]string{base, live, target} {
			if got := col(rows, Col(c)); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("col %d: got %v want %v\n%s", c, got, want, show(rows))
			}
		}
	}
}

func TestAlignSeparatesIndependentChanges(t *testing.T) {
	base := []string{"goodnight:", "  - light.turn_off", "    entity_id: all", "  - lock.lock", "    entity_id: front"}
	live := []string{"goodnight:", "  - light.turn_off", "    target:", "      entity_id: all", "  - lock.lock", "    entity_id: front"}
	target := append(append([]string(nil), base...), "  - alarm.arm")
	got := show(Align(base, live, target))
	want := "goodnight: | goodnight: | goodnight:\n" +
		"  - light.turn_off |   - light.turn_off |   - light.turn_off\n" +
		"    entity_id: all |     target: |     entity_id: all\n" +
		"· |       entity_id: all | ·\n" +
		"  - lock.lock |   - lock.lock |   - lock.lock\n" +
		"    entity_id: front |     entity_id: front |     entity_id: front\n" +
		"· | · |   - alarm.arm\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestAlignAbsentLive(t *testing.T) {
	base := []string{"# GitOps", "old"}
	target := []string{"# GitOps", "new", "more"}
	rows := Align(base, nil, target)
	if len(rows) != 3 || col(rows, Live) != nil {
		t.Fatalf("got:\n%s", show(rows))
	}
}

func TestCompose(t *testing.T) {
	base := []string{"a", "b"}
	live := []string{"a", "x"}
	target := []string{"a", "b", "c"}
	rows := Align(base, live, target)
	// Revert everything in live, except take the added target line.
	picks := map[int]Col{}
	for i := range rows {
		picks[i] = Base
	}
	picks[len(rows)-1] = Target
	if got := string(Compose(rows, Live, picks)); got != "a\nb\nc\n" {
		t.Fatalf("got %q\n%s", got, show(rows))
	}
	if Compose(Align(nil, nil, []string{"a"}), Live, nil) != nil {
		t.Fatal("empty compose should be nil")
	}
}

func TestLines(t *testing.T) {
	if Lines(nil) != nil || len(Lines([]byte("a\nb\n"))) != 2 || len(Lines([]byte("a\nb"))) != 2 {
		t.Fatal("Lines")
	}
}

// An absent live must not drag base and target out of line: target's
// insertion stays one row, the lines after it pair up again.
func TestAlignAbsentLiveKeepsTargetAligned(t *testing.T) {
	base := []string{"a", "b", "c", "d"}
	target := []string{"a", "B", "c", "new", "d"}
	rows := Align(base, nil, target)
	differ := 0
	for _, r := range rows {
		if r[Base] == nil || r[Target] == nil || *r[Base] != *r[Target] {
			differ++
		}
	}
	if differ != 2 {
		t.Fatalf("want 2 rows where base and target differ, got %d:\n%s", differ, show(rows))
	}
}
