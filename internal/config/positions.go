package config

import (
	"fmt"
	"strings"

	"github.com/pelletier/go-toml/v2/unstable"
)

// positions maps a dotted key path in config.toml — `loops[1].steps[2]`,
// `loops[0].source.kind` — to where it is written, so a validation problem can
// name its file:line:column the way a TOML syntax error does (#53). The decoder
// does not keep positions, so the file is walked once more with go-toml's own
// parser.
type positions struct {
	file string
	at   map[string]unstable.Position
}

// indexPositions walks raw, which has already decoded cleanly. Every key, table
// header and array element is recorded at its first byte: a key at its name, an
// inline table at its `{`.
func indexPositions(file string, raw []byte) positions {
	ix := indexer{pos: positions{file: file, at: map[string]unstable.Position{}}, arrays: map[string]int{}}
	ix.p.Reset(raw)
	table := ""
	for ix.p.NextExpression() {
		e := ix.p.Expression()
		switch e.Kind {
		case unstable.Table, unstable.ArrayTable:
			key, first := keyParts(e.Key())
			table = ix.resolve(key, e.Kind == unstable.ArrayTable)
			ix.record(table, first)
		case unstable.KeyValue:
			key, first := keyParts(e.Key())
			path := join(table, strings.Join(key, "."))
			ix.record(path, first)
			ix.value(path, e.Value())
		}
	}
	return ix.pos
}

// prefix is "file:line:col: " for the first of keys that was recorded, or just
// "file: " when none was.
func (p positions) prefix(keys ...string) string {
	for _, k := range keys {
		if pos, ok := p.at[k]; ok {
			return fmt.Sprintf("%s:%d:%d: ", p.file, pos.Line, pos.Column)
		}
	}
	return p.file + ": "
}

type indexer struct {
	p   unstable.Parser
	pos positions
	// arrays counts the [[array table]] entries seen at each path.
	arrays map[string]int
}

func (ix *indexer) record(path string, n *unstable.Node) {
	if n == nil || n.Raw.Length == 0 {
		return
	}
	if _, ok := ix.pos.at[path]; !ok {
		ix.pos.at[path] = ix.p.Shape(n.Raw).Start
	}
}

// resolve turns a header's dotted key into an indexed path: a part that names an
// array of tables refers to its latest entry, and a new [[array table]] header
// opens the next one.
func (ix *indexer) resolve(parts []string, arrayTable bool) string {
	path := ""
	for i, part := range parts {
		path = join(path, part)
		n, isArray := ix.arrays[path]
		switch {
		case arrayTable && i == len(parts)-1:
			ix.arrays[path] = n + 1
			path = fmt.Sprintf("%s[%d]", path, n)
		case isArray:
			path = fmt.Sprintf("%s[%d]", path, n-1)
		}
	}
	return path
}

// value records an inline table's keys and an array's elements under path.
func (ix *indexer) value(path string, v *unstable.Node) {
	switch v.Kind {
	case unstable.InlineTable:
		it := v.Children()
		for it.Next() {
			kv := it.Node()
			if kv.Kind != unstable.KeyValue {
				continue
			}
			key, first := keyParts(kv.Key())
			sub := join(path, strings.Join(key, "."))
			ix.record(sub, first)
			ix.value(sub, kv.Value())
		}
	case unstable.Array:
		it := v.Children()
		i := 0
		for it.Next() {
			el := it.Node()
			if el.Kind == unstable.Comment {
				continue
			}
			sub := fmt.Sprintf("%s[%d]", path, i)
			ix.record(sub, el)
			ix.value(sub, el)
			i++
		}
	}
}

// keyParts reads a dotted key and its first part's node.
func keyParts(it unstable.Iterator) ([]string, *unstable.Node) {
	var parts []string
	var first *unstable.Node
	for it.Next() {
		if first == nil {
			first = it.Node()
		}
		parts = append(parts, string(it.Node().Data))
	}
	return parts, first
}

func join(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}
