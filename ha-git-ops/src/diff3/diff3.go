// Package diff3 aligns three versions of a file line by line — the
// last-applied baseline, live, and target — into rows the panel renders
// as columns, and composes a version back from per-row picks.
package diff3

import (
	"encoding/json"
	"strings"
)

// Col names one of the three versions.
type Col int

const (
	Base Col = iota
	Live
	Target
)

// ParseCol maps the panel's version keys to columns.
func ParseCol(s string) (Col, bool) {
	switch s {
	case "base":
		return Base, true
	case "live":
		return Live, true
	case "target":
		return Target, true
	}
	return 0, false
}

// Row holds one aligned line per version; nil means the line does not
// exist in that version.
type Row [3]*string

// MarshalJSON encodes a row as [base, live, target] with nulls.
func (r Row) MarshalJSON() ([]byte, error) {
	return json.Marshal([3]*string(r))
}

// Lines splits content into lines without their terminators. A missing
// trailing newline is not distinguished.
func Lines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	s := strings.TrimSuffix(string(b), "\n")
	return strings.Split(s, "\n")
}

// hunk replaces a[s:e] with lines.
type hunk struct {
	s, e  int
	lines []string
}

// Align merges the base→live and base→target diffs into rows anchored
// on base lines. A side's replacement lines pair with the base lines
// they replace; any surplus becomes rows of its own after them, paired
// across sides at the same spot. So each side lines up with base by its
// own diff, however the other side changed.
func Align(base, live, target []string) []Row {
	lv, lx := project(base, diff(base, live))
	tv, tx := project(base, diff(base, target))
	var rows []Row
	for p := 0; p <= len(base); p++ {
		for k := 0; k < max(len(lx[p]), len(tx[p])); k++ {
			rows = append(rows, Row{nil, at(lx[p], k), at(tx[p], k)})
		}
		if p < len(base) {
			rows = append(rows, Row{&base[p], lv[p], tv[p]})
		}
	}
	return rows
}

// project lays one side's hunks over base: val[p] is the side's line at
// base line p (nil when deleted), extra[p] the lines it inserts before
// base line p (p == len(base) for the end).
func project(base []string, hs []hunk) ([]*string, [][]string) {
	val := make([]*string, len(base))
	for p := range base {
		val[p] = &base[p]
	}
	extra := make([][]string, len(base)+1)
	for _, h := range hs {
		n := min(len(h.lines), h.e-h.s)
		for k := 0; k < h.e-h.s; k++ {
			val[h.s+k] = at(h.lines, k)
			if k >= n {
				val[h.s+k] = nil
			}
		}
		extra[h.e] = append(extra[h.e], h.lines[n:]...)
	}
	return val, extra
}

func at(lines []string, k int) *string {
	if k < len(lines) {
		return &lines[k]
	}
	return nil
}

// Compose builds a version from rows: each row contributes the line of
// the column picks names for it, or own's line when unpicked. The result
// is nil when no line remains — the file does not exist.
func Compose(rows []Row, own Col, picks map[int]Col) []byte {
	var b strings.Builder
	n := 0
	for i, r := range rows {
		c := own
		if p, ok := picks[i]; ok {
			c = p
		}
		if r[c] == nil {
			continue
		}
		b.WriteString(*r[c])
		b.WriteByte('\n')
		n++
	}
	if n == 0 {
		return nil
	}
	return []byte(b.String())
}

// maxEdit bounds the Myers search; beyond it the whole range is treated
// as replaced rather than spending quadratic memory on a rewrite.
const maxEdit = 4000

// diff returns the hunks turning a into b.
func diff(a, b []string) []hunk {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	a2, b2 := a[pre:len(a)-suf], b[pre:len(b)-suf]
	if len(a2) == 0 && len(b2) == 0 {
		return nil
	}
	var hs []hunk
	if len(a2) == 0 || len(b2) == 0 || len(a2)+len(b2) > maxEdit {
		hs = []hunk{{0, len(a2), b2}}
	} else {
		hs = myers(a2, b2)
	}
	for k := range hs {
		hs[k].s += pre
		hs[k].e += pre
	}
	return hs
}

// myers computes a shortest edit script (Myers 1986) and groups it into
// hunks over a.
func myers(a, b []string) []hunk {
	n, m := len(a), len(b)
	off := n + m + 1
	v := make([]int, 2*off+1)
	var trace [][]int
	for d := 0; d <= n+m; d++ {
		trace = append(trace, append([]int(nil), v...))
		done := false
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = v[off+k+1]
			} else {
				x = v[off+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[off+k] = x
			if x >= n && y >= m {
				done = true
				break
			}
		}
		if done {
			break
		}
	}

	// Walk back from (n, m), recording edits in reverse.
	type edit struct {
		del  bool
		a, b int // index in a (delete) or b (insert)
	}
	var edits []edit
	x, y := n, m
	for d := len(trace) - 1; d > 0; d-- {
		v := trace[d]
		k := x - y
		var pk int
		if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
			pk = k + 1
		} else {
			pk = k - 1
		}
		px := v[off+pk]
		py := px - pk
		for x > px && y > py {
			x--
			y--
		}
		if x == px {
			edits = append(edits, edit{false, x, py})
		} else {
			edits = append(edits, edit{true, px, 0})
		}
		x, y = px, py
	}

	var hs []hunk
	for k := len(edits) - 1; k >= 0; k-- {
		e := edits[k]
		if len(hs) > 0 {
			last := &hs[len(hs)-1]
			if e.del && e.a == last.e {
				last.e++
				continue
			}
			if !e.del && e.a == last.e {
				last.lines = append(last.lines, b[e.b])
				continue
			}
		}
		if e.del {
			hs = append(hs, hunk{e.a, e.a + 1, nil})
		} else {
			hs = append(hs, hunk{e.a, e.a, []string{b[e.b]}})
		}
	}
	return hs
}
