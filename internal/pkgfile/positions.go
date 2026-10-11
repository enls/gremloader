package pkgfile

import (
	"strings"

	"github.com/pelletier/go-toml/v2/unstable"
)

// positions maps a key path to the line it is written on. go-toml reports positions only for
// syntax errors and unknown keys; this second, read-only pass over the same bytes gives every key
// a line, so semantic errors ("unknown preset") and `explain` can say where a value came from.
//
// It is best effort: the document has already been decoded successfully by the time it is
// built, and a key it cannot place simply has no line.
type positions map[string]int

func keyPath(keys ...string) string { return strings.Join(keys, "\x00") }

func indexPositions(data []byte) positions {
	pos := positions{}
	var p unstable.Parser
	p.Reset(data)
	var table []string
	for p.NextExpression() {
		e := p.Expression()
		switch e.Kind {
		case unstable.Table, unstable.ArrayTable:
			table = keys(e.Key())
			pos.add(&p, e, table)
		case unstable.KeyValue:
			k := append(append([]string{}, table...), keys(e.Key())...)
			pos.add(&p, e, k)
			pos.inline(&p, k, e.Value())
		}
	}
	return pos
}

func (pos positions) inline(p *unstable.Parser, prefix []string, v *unstable.Node) {
	if v == nil || v.Kind != unstable.InlineTable {
		return
	}
	it := v.Children()
	for it.Next() {
		kv := it.Node()
		k := append(append([]string{}, prefix...), keys(kv.Key())...)
		pos.add(p, kv, k)
		pos.inline(p, k, kv.Value())
	}
}

func (pos positions) add(p *unstable.Parser, n *unstable.Node, k []string) {
	it := n.Key()
	if !it.Next() {
		return
	}
	line := p.Shape(it.Node().Raw).Start.Line
	// A dotted key (dir.windows = ...) also places its parents, if nothing placed them earlier.
	for i := len(k); i > 0; i-- {
		path := keyPath(k[:i]...)
		if _, ok := pos[path]; ok && i < len(k) {
			break
		}
		pos[path] = line
	}
}

func keys(it unstable.Iterator) []string {
	var out []string
	for it.Next() {
		out = append(out, string(it.Node().Data))
	}
	return out
}

// line returns the line of the key path, or of its nearest placed parent.
func (pos positions) line(k ...string) int {
	for i := len(k); i > 0; i-- {
		if l, ok := pos[keyPath(k[:i]...)]; ok {
			return l
		}
	}
	return 0
}
