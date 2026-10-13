package license

import (
	"errors"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

// Protocol v1 JSON limits, identical to the vendor issuer.
const (
	maxJSONDepth       = 8
	maxJSONKeyUnits    = 128
	maxJSONStringUnits = 4096
	maxJSONNumberChars = 20
)

var errJSON = errors.New("invalid protocol v1 JSON")

// parseStrictJSON parses a protocol v1 JSON object. Objects become
// map[string]interface{}, arrays []interface{}, numbers int64. It rejects
// what the vendor issuer rejects: invalid UTF-8, duplicate keys, fractions,
// exponents, non-finite numbers, overlong numbers, keys or strings, lone
// surrogates and nesting deeper than 8 (scalars count as a level).
// encoding/json is not used because it silently replaces invalid UTF-8 and
// lone surrogates and accepts duplicate keys.
func parseStrictJSON(data []byte) (map[string]interface{}, error) {
	if !utf8.Valid(data) {
		return nil, errJSON
	}
	p := &jsonParser{data: data}
	p.skipSpace()
	if p.peek() != '{' {
		return nil, errJSON
	}
	value, err := p.value(1)
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	if p.pos != len(p.data) {
		return nil, errJSON
	}
	return value.(map[string]interface{}), nil
}

type jsonParser struct {
	data []byte
	pos  int
}

func (p *jsonParser) peek() byte {
	if p.pos < len(p.data) {
		return p.data[p.pos]
	}
	return 0
}

func (p *jsonParser) skipSpace() {
	for p.pos < len(p.data) {
		switch p.data[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *jsonParser) value(depth int) (interface{}, error) {
	if depth > maxJSONDepth {
		return nil, errJSON
	}
	p.skipSpace()
	switch c := p.peek(); {
	case c == '{':
		return p.object(depth)
	case c == '[':
		return p.array(depth)
	case c == '"':
		s, err := p.str()
		if err != nil {
			return nil, err
		}
		if utf16Len(s) > maxJSONStringUnits {
			return nil, errJSON
		}
		return s, nil
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	case c == 't':
		return true, p.literal("true")
	case c == 'f':
		return false, p.literal("false")
	case c == 'n':
		return nil, p.literal("null")
	default:
		return nil, errJSON
	}
}

func (p *jsonParser) literal(word string) error {
	if len(p.data)-p.pos < len(word) || string(p.data[p.pos:p.pos+len(word)]) != word {
		return errJSON
	}
	p.pos += len(word)
	return nil
}

func (p *jsonParser) object(depth int) (interface{}, error) {
	p.pos++ // '{'
	obj := make(map[string]interface{})
	p.skipSpace()
	if p.peek() == '}' {
		p.pos++
		return obj, nil
	}
	for {
		p.skipSpace()
		if p.peek() != '"' {
			return nil, errJSON
		}
		key, err := p.str()
		if err != nil {
			return nil, err
		}
		if utf16Len(key) > maxJSONKeyUnits {
			return nil, errJSON
		}
		if _, dup := obj[key]; dup {
			return nil, errJSON
		}
		p.skipSpace()
		if p.peek() != ':' {
			return nil, errJSON
		}
		p.pos++
		v, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		obj[key] = v
		p.skipSpace()
		switch p.peek() {
		case ',':
			p.pos++
		case '}':
			p.pos++
			return obj, nil
		default:
			return nil, errJSON
		}
	}
}

func (p *jsonParser) array(depth int) (interface{}, error) {
	p.pos++ // '['
	arr := []interface{}{}
	p.skipSpace()
	if p.peek() == ']' {
		p.pos++
		return arr, nil
	}
	for {
		v, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		arr = append(arr, v)
		p.skipSpace()
		switch p.peek() {
		case ',':
			p.pos++
		case ']':
			p.pos++
			return arr, nil
		default:
			return nil, errJSON
		}
	}
}

// number accepts only integers: -?(0|[1-9][0-9]*), at most 20 characters
// including the sign, within int64.
func (p *jsonParser) number() (interface{}, error) {
	start := p.pos
	if p.peek() == '-' {
		p.pos++
	}
	switch c := p.peek(); {
	case c == '0':
		p.pos++
	case c >= '1' && c <= '9':
		for c := p.peek(); c >= '0' && c <= '9'; c = p.peek() {
			p.pos++
		}
	default:
		return nil, errJSON
	}
	if c := p.peek(); c == '.' || c == 'e' || c == 'E' || (c >= '0' && c <= '9') {
		return nil, errJSON
	}
	text := string(p.data[start:p.pos])
	if len(text) > maxJSONNumberChars {
		return nil, errJSON
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return nil, errJSON
	}
	return n, nil
}

// str parses a string token. Valid surrogate pairs combine; a lone
// surrogate, raw control character or bad escape is rejected.
func (p *jsonParser) str() (string, error) {
	p.pos++ // opening quote
	var out []rune
	for {
		if p.pos >= len(p.data) {
			return "", errJSON
		}
		c := p.data[p.pos]
		switch {
		case c == '"':
			p.pos++
			return string(out), nil
		case c < 0x20:
			return "", errJSON
		case c == '\\':
			r, err := p.escape()
			if err != nil {
				return "", err
			}
			out = append(out, r)
		default:
			r, size := utf8.DecodeRune(p.data[p.pos:])
			out = append(out, r)
			p.pos += size
		}
	}
}

func (p *jsonParser) escape() (rune, error) {
	p.pos++ // backslash
	if p.pos >= len(p.data) {
		return 0, errJSON
	}
	c := p.data[p.pos]
	p.pos++
	switch c {
	case '"', '\\', '/':
		return rune(c), nil
	case 'b':
		return '\b', nil
	case 'f':
		return '\f', nil
	case 'n':
		return '\n', nil
	case 'r':
		return '\r', nil
	case 't':
		return '\t', nil
	case 'u':
		r, err := p.hex4()
		if err != nil {
			return 0, err
		}
		if !utf16.IsSurrogate(r) {
			return r, nil
		}
		if r >= 0xDC00 || len(p.data)-p.pos < 6 || p.data[p.pos] != '\\' || p.data[p.pos+1] != 'u' {
			return 0, errJSON
		}
		p.pos += 2
		low, err := p.hex4()
		if err != nil {
			return 0, err
		}
		combined := utf16.DecodeRune(r, low)
		if combined == utf8.RuneError {
			return 0, errJSON
		}
		return combined, nil
	default:
		return 0, errJSON
	}
}

func (p *jsonParser) hex4() (rune, error) {
	if len(p.data)-p.pos < 4 {
		return 0, errJSON
	}
	var r rune
	for _, c := range p.data[p.pos : p.pos+4] {
		r <<= 4
		switch {
		case c >= '0' && c <= '9':
			r |= rune(c - '0')
		case c >= 'a' && c <= 'f':
			r |= rune(c-'a') + 10
		case c >= 'A' && c <= 'F':
			r |= rune(c-'A') + 10
		default:
			return 0, errJSON
		}
	}
	p.pos += 4
	return r, nil
}

// utf16Len counts UTF-16 code units, the unit of every protocol v1 length.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}
