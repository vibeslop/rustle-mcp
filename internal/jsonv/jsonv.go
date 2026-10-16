// Package jsonv reads JSON into plain values whose objects keep their key order, for
// responses read by path (YouTube's) and walked in document order (YouTube's results, a
// page's JSON-LD). encoding/json's maps lose that order.
package jsonv

import (
	"errors"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

// Object is a JSON object in document order. A repeated key keeps its first position and
// last value, as JSON.parse does.
type Object struct {
	Keys   []string
	Values []any
}

// Get is the value of key, or nil.
func (o *Object) Get(key string) any {
	if o == nil {
		return nil
	}
	for i, k := range o.Keys {
		if k == key {
			return o.Values[i]
		}
	}
	return nil
}

// Has reports whether the object has key, even with a null value.
func (o *Object) Has(key string) bool {
	if o == nil {
		return false
	}
	for _, k := range o.Keys {
		if k == key {
			return true
		}
	}
	return false
}

// Parse reads JSON: objects are *Object, arrays []any, numbers float64, strings string,
// true and false bool, null nil.
func Parse(data []byte) (any, error) {
	p := parser{data: data}
	p.space()
	v, err := p.value(0)
	if err != nil {
		return nil, err
	}
	p.space()
	if p.at < len(p.data) {
		return nil, p.fail("unexpected data after the value")
	}
	return v, nil
}

type parser struct {
	data []byte
	at   int
}

// maxDepth bounds nesting, so hostile input can't exhaust the stack.
const maxDepth = 512

var errSyntax = errors.New("invalid JSON")

func (p *parser) fail(string) error { return errSyntax }

func (p *parser) space() {
	for p.at < len(p.data) {
		switch p.data[p.at] {
		case ' ', '\t', '\n', '\r':
			p.at++
		default:
			return
		}
	}
}

func (p *parser) value(depth int) (any, error) {
	if p.at >= len(p.data) {
		return nil, p.fail("unexpected end")
	}
	if depth > maxDepth {
		return nil, p.fail("too deep")
	}
	switch c := p.data[p.at]; {
	case c == '{':
		p.at++
		o := &Object{}
		p.space()
		if p.at < len(p.data) && p.data[p.at] == '}' {
			p.at++
			return o, nil
		}
		for {
			p.space()
			if p.at >= len(p.data) || p.data[p.at] != '"' {
				return nil, p.fail("expected a key")
			}
			key, err := p.str()
			if err != nil {
				return nil, err
			}
			p.space()
			if p.at >= len(p.data) || p.data[p.at] != ':' {
				return nil, p.fail("expected ':'")
			}
			p.at++
			p.space()
			v, err := p.value(depth + 1)
			if err != nil {
				return nil, err
			}
			replaced := false
			for i, k := range o.Keys {
				if k == key {
					o.Values[i] = v
					replaced = true
					break
				}
			}
			if !replaced {
				o.Keys = append(o.Keys, key)
				o.Values = append(o.Values, v)
			}
			p.space()
			if p.at >= len(p.data) {
				return nil, p.fail("unexpected end")
			}
			if p.data[p.at] == ',' {
				p.at++
				continue
			}
			if p.data[p.at] == '}' {
				p.at++
				return o, nil
			}
			return nil, p.fail("expected ',' or '}'")
		}
	case c == '[':
		p.at++
		list := []any{}
		p.space()
		if p.at < len(p.data) && p.data[p.at] == ']' {
			p.at++
			return list, nil
		}
		for {
			p.space()
			v, err := p.value(depth + 1)
			if err != nil {
				return nil, err
			}
			list = append(list, v)
			p.space()
			if p.at >= len(p.data) {
				return nil, p.fail("unexpected end")
			}
			if p.data[p.at] == ',' {
				p.at++
				continue
			}
			if p.data[p.at] == ']' {
				p.at++
				return list, nil
			}
			return nil, p.fail("expected ',' or ']'")
		}
	case c == '"':
		return p.str()
	case c == 't':
		return p.word("true", true)
	case c == 'f':
		return p.word("false", false)
	case c == 'n':
		return p.word("null", nil)
	case c == '-' || c >= '0' && c <= '9':
		return p.number()
	}
	return nil, p.fail("unexpected character")
}

func (p *parser) word(w string, v any) (any, error) {
	if len(p.data)-p.at < len(w) || string(p.data[p.at:p.at+len(w)]) != w {
		return nil, p.fail("bad literal")
	}
	p.at += len(w)
	return v, nil
}

func (p *parser) number() (any, error) {
	start := p.at
	if p.data[p.at] == '-' {
		p.at++
	}
	digits := func() int {
		n := 0
		for p.at < len(p.data) && p.data[p.at] >= '0' && p.data[p.at] <= '9' {
			p.at++
			n++
		}
		return n
	}
	if p.at < len(p.data) && p.data[p.at] == '0' {
		p.at++
	} else if digits() == 0 {
		return nil, p.fail("bad number")
	}
	if p.at < len(p.data) && p.data[p.at] == '.' {
		p.at++
		if digits() == 0 {
			return nil, p.fail("bad number")
		}
	}
	if p.at < len(p.data) && (p.data[p.at] == 'e' || p.data[p.at] == 'E') {
		p.at++
		if p.at < len(p.data) && (p.data[p.at] == '+' || p.data[p.at] == '-') {
			p.at++
		}
		if digits() == 0 {
			return nil, p.fail("bad number")
		}
	}
	v, err := strconv.ParseFloat(string(p.data[start:p.at]), 64)
	if err != nil {
		var ne *strconv.NumError
		if !errors.As(err, &ne) || ne.Err != strconv.ErrRange {
			return nil, p.fail("bad number")
		}
	}
	return v, nil
}

func (p *parser) str() (string, error) {
	p.at++ // the opening quote
	start := p.at
	// Most strings hold no escapes and are sliced straight out of the input.
	for p.at < len(p.data) {
		c := p.data[p.at]
		if c == '"' {
			s := string(p.data[start:p.at])
			p.at++
			return s, nil
		}
		if c == '\\' {
			break
		}
		if c < 0x20 {
			return "", p.fail("control character in string")
		}
		p.at++
	}
	buf := append([]byte(nil), p.data[start:p.at]...)
	for p.at < len(p.data) {
		c := p.data[p.at]
		switch {
		case c == '"':
			p.at++
			return string(buf), nil
		case c < 0x20:
			return "", p.fail("control character in string")
		case c != '\\':
			buf = append(buf, c)
			p.at++
			continue
		}
		p.at++
		if p.at >= len(p.data) {
			break
		}
		e := p.data[p.at]
		p.at++
		switch e {
		case '"', '\\', '/':
			buf = append(buf, e)
		case 'b':
			buf = append(buf, '\b')
		case 'f':
			buf = append(buf, '\f')
		case 'n':
			buf = append(buf, '\n')
		case 'r':
			buf = append(buf, '\r')
		case 't':
			buf = append(buf, '\t')
		case 'u':
			r, ok := p.hex4()
			if !ok {
				return "", p.fail("bad \\u escape")
			}
			if utf16.IsSurrogate(r) {
				// A pair makes one character; a lone half becomes U+FFFD, as Go strings can't hold it.
				if p.at+6 <= len(p.data) && p.data[p.at] == '\\' && p.data[p.at+1] == 'u' {
					save := p.at
					p.at += 2
					if low, ok := p.hex4(); ok {
						if pair := utf16.DecodeRune(r, low); pair != utf8.RuneError {
							buf = utf8.AppendRune(buf, pair)
							continue
						}
					}
					p.at = save
				}
				r = utf8.RuneError
			}
			buf = utf8.AppendRune(buf, r)
		default:
			return "", p.fail("bad escape")
		}
	}
	return "", p.fail("unterminated string")
}

func (p *parser) hex4() (rune, bool) {
	if p.at+4 > len(p.data) {
		return 0, false
	}
	var r rune
	for _, c := range p.data[p.at : p.at+4] {
		r <<= 4
		switch {
		case c >= '0' && c <= '9':
			r |= rune(c - '0')
		case c >= 'a' && c <= 'f':
			r |= rune(c - 'a' + 10)
		case c >= 'A' && c <= 'F':
			r |= rune(c - 'A' + 10)
		default:
			return 0, false
		}
	}
	p.at += 4
	return r, true
}
