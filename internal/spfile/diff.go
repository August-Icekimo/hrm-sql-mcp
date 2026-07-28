package spfile

import (
	"fmt"
	"strings"
)

// Diff produces a unified diff between a file definition and a server
// definition, both normalised first.
//
// Returns an empty string when they match, so callers can use the result
// itself as the "are they different" answer without a second comparison that
// could disagree with the diff.
func Diff(fileLabel, fileDef, serverLabel, serverDef string, context int) string {
	a := strings.Split(Normalize(fileDef), "\n")
	b := strings.Split(Normalize(serverDef), "\n")
	if strings.Join(a, "\n") == strings.Join(b, "\n") {
		return ""
	}
	if context < 0 {
		context = 3
	}

	ops := lcsOps(a, b)
	hunks := groupHunks(ops, context)
	if len(hunks) == 0 {
		return ""
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s\n+++ %s\n", fileLabel, serverLabel)
	for _, h := range hunks {
		fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", h.aStart+1, h.aLen, h.bStart+1, h.bLen)
		for _, o := range h.ops {
			switch o.kind {
			case opEqual:
				sb.WriteString(" " + o.text + "\n")
			case opDelete:
				sb.WriteString("-" + o.text + "\n")
			case opInsert:
				sb.WriteString("+" + o.text + "\n")
			}
		}
	}
	return sb.String()
}

type opKind int

const (
	opEqual opKind = iota
	opDelete
	opInsert
)

type op struct {
	kind opKind
	text string
	ai   int // index in a, -1 for inserts
	bi   int // index in b, -1 for deletes
}

// lcsOps builds an edit script via a standard longest-common-subsequence
// table. The inputs are procedure bodies — hundreds of lines, not millions —
// so the O(n*m) table is comfortably affordable and far easier to verify than
// a Myers implementation.
func lcsOps(a, b []string) []op {
	n, m := len(a), len(b)
	tbl := make([][]int, n+1)
	for i := range tbl {
		tbl[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				tbl[i][j] = tbl[i+1][j+1] + 1
			} else {
				tbl[i][j] = max(tbl[i+1][j], tbl[i][j+1])
			}
		}
	}

	var ops []op
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, op{opEqual, a[i], i, j})
			i++
			j++
		case tbl[i+1][j] >= tbl[i][j+1]:
			ops = append(ops, op{opDelete, a[i], i, -1})
			i++
		default:
			ops = append(ops, op{opInsert, b[j], -1, j})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, op{opDelete, a[i], i, -1})
	}
	for ; j < m; j++ {
		ops = append(ops, op{opInsert, b[j], -1, j})
	}
	return ops
}

type hunk struct {
	aStart, aLen int
	bStart, bLen int
	ops          []op
}

// groupHunks keeps only changed regions plus surrounding context.
func groupHunks(ops []op, context int) []hunk {
	changed := make([]bool, len(ops))
	any := false
	for i, o := range ops {
		if o.kind != opEqual {
			changed[i] = true
			any = true
		}
	}
	if !any {
		return nil
	}

	keep := make([]bool, len(ops))
	for i, c := range changed {
		if !c {
			continue
		}
		for k := max(0, i-context); k <= min(len(ops)-1, i+context); k++ {
			keep[k] = true
		}
	}

	var out []hunk
	for i := 0; i < len(ops); {
		if !keep[i] {
			i++
			continue
		}
		j := i
		for j < len(ops) && keep[j] {
			j++
		}
		h := hunk{aStart: -1, bStart: -1}
		for _, o := range ops[i:j] {
			if o.ai >= 0 && h.aStart < 0 {
				h.aStart = o.ai
			}
			if o.bi >= 0 && h.bStart < 0 {
				h.bStart = o.bi
			}
			if o.kind != opInsert {
				h.aLen++
			}
			if o.kind != opDelete {
				h.bLen++
			}
			h.ops = append(h.ops, o)
		}
		if h.aStart < 0 {
			h.aStart = 0
		}
		if h.bStart < 0 {
			h.bStart = 0
		}
		out = append(out, h)
		i = j
	}
	return out
}
