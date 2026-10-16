package jsonv

import (
	"math"
	"strconv"
	"strings"
)

// Get follows keys from v, as v?.a?.b does; nil when any step is missing or not an object.
func Get(v any, keys ...string) any {
	for _, key := range keys {
		o, ok := v.(*Object)
		if !ok {
			return nil
		}
		v = o.Get(key)
	}
	return v
}

// Obj is v as an object, or nil.
func Obj(v any) *Object {
	o, _ := v.(*Object)
	return o
}

// Arr is v as an array, or nil.
func Arr(v any) []any {
	a, _ := v.([]any)
	return a
}

// Objects is the objects in an array, skipping anything else.
func Objects(v any) []*Object {
	var out []*Object
	for _, item := range Arr(v) {
		if o, ok := item.(*Object); ok {
			out = append(out, o)
		}
	}
	return out
}

// Str is v as a string, and whether it was one.
func Str(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

// String is v as JavaScript's String() writes JSON values: "" for missing.
func String(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return numString(x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case nil:
		return ""
	case []any:
		return "" // arrays and objects never name anything Rustle reads
	}
	return "[object Object]"
}

// Num is v as a number, and whether it was one.
func Num(v any) (float64, bool) {
	n, ok := v.(float64)
	return n, ok
}

// Truthy is JavaScript's truthiness of a JSON value.
func Truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0 && !math.IsNaN(x)
	case string:
		return x != ""
	}
	return true
}

// Walk visits every object in a JSON tree, parents before children, in document order;
// iterative, so deep nesting can't overflow the stack. fn returns false to stop.
func Walk(v any, fn func(*Object) bool) {
	stack := []any{v}
	for len(stack) > 0 {
		node := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		switch x := node.(type) {
		case []any:
			for i := len(x) - 1; i >= 0; i-- {
				stack = append(stack, x[i])
			}
		case *Object:
			if !fn(x) {
				return
			}
			for i := len(x.Values) - 1; i >= 0; i-- {
				stack = append(stack, x.Values[i])
			}
		}
	}
}

// numString is a number as JavaScript's String() prints it.
func numString(v float64) string {
	abs := math.Abs(v)
	switch {
	case v == 0:
		return "0"
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 0):
		if v > 0 {
			return "Infinity"
		}
		return "-Infinity"
	case abs >= 1e-6 && abs < 1e21:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	s := strconv.FormatFloat(v, 'e', -1, 64)
	if i := strings.Index(s, "e"); i >= 0 && i+3 < len(s) && s[i+2] == '0' {
		s = s[:i+2] + s[i+3:]
	}
	return s
}
