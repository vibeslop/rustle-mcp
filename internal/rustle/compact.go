package rustle

import (
	"math"
	"strconv"
	"unicode/utf8"
)

// Obj is a JSON object that keeps its keys in the order they were set, since the
// metadata line is read by a model and its order is part of how it reads.
type Obj struct {
	keys []string
	vals []any
}

func NewObj() *Obj { return &Obj{} }

// Set adds or replaces a key; a nil value is kept but never written.
func (o *Obj) Set(key string, value any) *Obj {
	for i, k := range o.keys {
		if k == key {
			o.vals[i] = value
			return o
		}
	}
	o.keys = append(o.keys, key)
	o.vals = append(o.vals, value)
	return o
}

func (o *Obj) Get(key string) (any, bool) {
	if o == nil {
		return nil, false
	}
	for i, k := range o.keys {
		if k == key {
			return o.vals[i], true
		}
	}
	return nil, false
}

func (o *Obj) Delete(key string) {
	for i, k := range o.keys {
		if k == key {
			o.keys = append(o.keys[:i:i], o.keys[i+1:]...)
			o.vals = append(o.vals[:i:i], o.vals[i+1:]...)
			return
		}
	}
}

func (o *Obj) Keys() []string {
	if o == nil {
		return nil
	}
	return append([]string(nil), o.keys...)
}

// Clone copies the object's own key list; values are shared.
func (o *Obj) Clone() *Obj {
	if o == nil {
		return NewObj()
	}
	return &Obj{keys: append([]string(nil), o.keys...), vals: append([]any(nil), o.vals...)}
}

func (o *Obj) MarshalJSON() ([]byte, error) { return AppendCompact(nil, o), nil }

// Compact is metadata as one line of JSON: numbers to milliseconds, unset fields and empty
// notes left out.
func Compact(value any) string { return string(AppendCompact(nil, value)) }

// AppendCompact writes value as Compact does.
func AppendCompact(out []byte, value any) []byte {
	switch v := value.(type) {
	case nil:
		return append(out, "null"...)
	case string:
		return AppendString(out, v)
	case bool:
		return strconv.AppendBool(out, v)
	case float64:
		return appendTidy(out, v)
	case float32:
		return appendTidy(out, float64(v))
	case int:
		return appendTidy(out, float64(v))
	case int64:
		return appendTidy(out, float64(v))
	case *Obj:
		if v == nil {
			return append(out, "null"...)
		}
		out = append(out, '{')
		first := true
		for i, key := range v.keys {
			item := v.vals[i]
			if isUnset(item) {
				continue
			}
			if notes, ok := item.([]string); ok && len(notes) == 0 && key == "notes" {
				continue
			}
			if !first {
				out = append(out, ',')
			}
			first = false
			out = AppendString(out, key)
			out = append(out, ':')
			out = AppendCompact(out, item)
		}
		return append(out, '}')
	case []any:
		out = append(out, '[')
		for i, item := range v {
			if i > 0 {
				out = append(out, ',')
			}
			out = AppendCompact(out, item)
		}
		return append(out, ']')
	case []string:
		out = append(out, '[')
		for i, item := range v {
			if i > 0 {
				out = append(out, ',')
			}
			out = AppendString(out, item)
		}
		return append(out, ']')
	case []float64:
		out = append(out, '[')
		for i, item := range v {
			if i > 0 {
				out = append(out, ',')
			}
			out = appendTidy(out, item)
		}
		return append(out, ']')
	case []int:
		out = append(out, '[')
		for i, item := range v {
			if i > 0 {
				out = append(out, ',')
			}
			out = strconv.AppendInt(out, int64(item), 10)
		}
		return append(out, ']')
	case [2]float64:
		return AppendCompact(out, v[:])
	case [][2]float64:
		out = append(out, '[')
		for i, item := range v {
			if i > 0 {
				out = append(out, ',')
			}
			out = AppendCompact(out, item[:])
		}
		return append(out, ']')
	case [2]int:
		return AppendCompact(out, v[:])
	case []Chapter:
		out = append(out, '[')
		for i, c := range v {
			if i > 0 {
				out = append(out, ',')
			}
			out = append(out, '[')
			out = appendTidy(out, c.At)
			out = append(out, ',')
			out = AppendString(out, c.Title)
			out = append(out, ']')
		}
		return append(out, ']')
	case map[string]any:
		// Only small fixed maps (an image block's _meta) reach here; one key, so no order to keep.
		out = append(out, '{')
		first := true
		for key, item := range v {
			if isUnset(item) {
				continue
			}
			if !first {
				out = append(out, ',')
			}
			first = false
			out = AppendString(out, key)
			out = append(out, ':')
			out = AppendCompact(out, item)
		}
		return append(out, '}')
	case interface{ MarshalJSON() ([]byte, error) }:
		data, err := v.MarshalJSON()
		if err != nil {
			return append(out, "null"...)
		}
		return append(out, data...)
	}
	return append(out, "null"...)
}

func isUnset(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case *Obj:
		return x == nil
	}
	return false
}

func appendTidy(out []byte, v float64) []byte {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return append(out, "null"...)
	}
	return AppendNumber(out, MS(v))
}

// AppendNumber writes a number as JSON usually shows it: 3, 12.5, 1e+21, 1e-7.
func AppendNumber(out []byte, v float64) []byte {
	if v == 0 {
		return append(out, '0')
	}
	if math.IsNaN(v) {
		return append(out, "NaN"...)
	}
	if math.IsInf(v, 1) {
		return append(out, "Infinity"...)
	}
	if math.IsInf(v, -1) {
		return append(out, "-Infinity"...)
	}
	abs := math.Abs(v)
	if abs >= 1e-6 && abs < 1e21 {
		return strconv.AppendFloat(out, v, 'f', -1, 64)
	}
	start := len(out)
	out = strconv.AppendFloat(out, v, 'e', -1, 64)
	// strconv writes e-07; JSON readers expect e-7.
	for i := start; i < len(out); i++ {
		if out[i] == 'e' && i+3 < len(out) && out[i+2] == '0' {
			out = append(out[:i+2], out[i+3:]...)
			break
		}
	}
	return out
}

// NumString is a number as AppendNumber writes it.
func NumString(v float64) string { return string(AppendNumber(nil, v)) }

const hexDigits = "0123456789abcdef"

// AppendString writes a JSON string as JSON.stringify does: only quotes, backslashes and
// control characters are escaped.
func AppendString(out []byte, s string) []byte {
	out = append(out, '"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c >= 0x80 {
			r, size := utf8.DecodeRuneInString(s[i:])
			if r == utf8.RuneError && size == 1 {
				out = append(out, s[start:i]...)
				out = append(out, `�`...)
				i++
				start = i
				continue
			}
			i += size
			continue
		}
		if c >= 0x20 && c != '"' && c != '\\' {
			i++
			continue
		}
		out = append(out, s[start:i]...)
		switch c {
		case '"':
			out = append(out, '\\', '"')
		case '\\':
			out = append(out, '\\', '\\')
		case '\n':
			out = append(out, '\\', 'n')
		case '\r':
			out = append(out, '\\', 'r')
		case '\t':
			out = append(out, '\\', 't')
		case '\b':
			out = append(out, '\\', 'b')
		case '\f':
			out = append(out, '\\', 'f')
		default:
			out = append(out, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&15])
		}
		i++
		start = i
	}
	out = append(out, s[start:]...)
	return append(out, '"')
}
