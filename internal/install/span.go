package install

import (
	"encoding/json"
	"errors"
)

// node is one JSON value of a hook file and its byte span [start, end). Install and uninstall
// splice bytes at these offsets, so every byte outside the Brain entry stays as it is.
type node struct {
	start, end int
	kind       byte // '{' for an object, '[' for an array, 0 for any other value
	// items are the members of an object or the elements of an array, in file order.
	items []item
}

// item is one member of an object (key set) or one element of an array (key empty).
type item struct {
	key      string
	keyStart int // start of the key string of a member, else the start of the value
	val      node
}

// errSyntax says that the file is not one JSON value. The span parser runs only after
// json.Valid, so it reports no detail.
var errSyntax = errors.New("not valid JSON")

// parse returns the span tree of b, which must hold one valid JSON value.
func parse(b []byte) (node, error) {
	if !json.Valid(b) {
		return node{}, errSyntax
	}
	p := parser{b: b}
	return p.value()
}

type parser struct {
	b   []byte
	pos int
}

func (p *parser) space() {
	for p.pos < len(p.b) {
		switch p.b[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *parser) value() (node, error) {
	p.space()
	switch p.b[p.pos] {
	case '{', '[':
		return p.container()
	case '"':
		start := p.pos
		p.skipString()
		return node{start: start, end: p.pos}, nil
	}
	start := p.pos
	for p.pos < len(p.b) && !isDelim(p.b[p.pos]) {
		p.pos++
	}
	return node{start: start, end: p.pos}, nil
}

// container reads an object or an array; json.Valid has checked its syntax.
func (p *parser) container() (node, error) {
	n := node{start: p.pos, kind: p.b[p.pos]}
	closer := byte(']')
	if n.kind == '{' {
		closer = '}'
	}
	p.pos++
	for {
		p.space()
		if p.b[p.pos] == closer {
			p.pos++
			n.end = p.pos
			return n, nil
		}
		if p.b[p.pos] == ',' {
			p.pos++
			p.space()
		}
		it := item{keyStart: p.pos}
		if n.kind == '{' {
			start := p.pos
			p.skipString()
			if err := json.Unmarshal(p.b[start:p.pos], &it.key); err != nil {
				return node{}, err
			}
			p.space()
			p.pos++ // the ':'
		}
		v, err := p.value()
		if err != nil {
			return node{}, err
		}
		it.val = v
		n.items = append(n.items, it)
	}
}

func (p *parser) skipString() {
	p.pos++ // the opening quote
	for p.b[p.pos] != '"' {
		if p.b[p.pos] == '\\' {
			p.pos++
		}
		p.pos++
	}
	p.pos++
}

func isDelim(c byte) bool {
	switch c {
	case ',', '}', ']', ' ', '\t', '\n', '\r':
		return true
	}
	return false
}

// member returns the index of the last member of object n with key, or -1. JSON decoders
// keep the last of two equal keys, so the CLIs read that one.
func (n node) member(key string) int {
	for i := len(n.items) - 1; i >= 0; i-- {
		if n.items[i].key == key {
			return i
		}
	}
	return -1
}

// cut returns the span that removes item i of container n with one separator: from the end of
// the item before it, or else up to the start of the item after it. So a removal undoes an
// append (see appendAt) byte for byte.
func (n node) cut(i int) (from, to int) {
	switch {
	case i > 0:
		return n.items[i-1].val.end, n.items[i].val.end
	case len(n.items) > 1:
		return n.items[0].keyStart, n.items[1].keyStart
	default:
		return n.items[0].keyStart, n.items[0].val.end
	}
}

// appendAt returns the offset and the text that append text as the last item of container n.
// After an item, the text gets a comma and the same white space that sits before the last
// item, so it follows the file's indent.
func (n node) appendAt(b []byte, text string) (at int, insert string) {
	if len(n.items) == 0 {
		return n.start + 1, text
	}
	last := n.items[len(n.items)-1]
	ws := last.keyStart
	for ws > n.start+1 && isSpace(b[ws-1]) {
		ws--
	}
	return last.val.end, "," + string(b[ws:last.keyStart]) + text
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
