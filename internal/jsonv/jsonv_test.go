package jsonv

import (
	"math"
	"testing"
)

func TestObjectsKeepTheirOrder(t *testing.T) {
	v, err := Parse([]byte(`{"z":1,"a":{"y":[true,false,null],"b":"x"},"m":-2.5e3,"z":3}`))
	if err != nil {
		t.Fatal(err)
	}
	o := Obj(v)
	if got := o.Keys; len(got) != 3 || got[0] != "z" || got[1] != "a" || got[2] != "m" {
		t.Fatalf("keys %v", got)
	}
	// A repeated key keeps its first position and its last value.
	if o.Get("z") != 3.0 {
		t.Errorf("z = %v", o.Get("z"))
	}
	if Get(v, "a", "b") != "x" || Get(v, "m") != -2500.0 || Get(v, "a", "y", "nope") != nil {
		t.Errorf("paths read %v %v", Get(v, "a", "b"), Get(v, "m"))
	}
	if list := Arr(Get(v, "a", "y")); len(list) != 3 || list[0] != true || list[2] != nil {
		t.Errorf("array %v", list)
	}
}

func TestStrings(t *testing.T) {
	for in, want := range map[string]string{
		`"plain"`:                 "plain",
		`"a\"b\\c\/d\n\t"`:        "a\"b\\c/d\n\t",
		`"é—"`:                    "é—",
		`"😀 smile"`:               "\U0001F600 smile",
		`"lone \ud800 half"`:      "lone � half",
		"\"raw é stays\"":         "raw é stays",
		`"AB"`:                    "AB",
		`"ends in escape \\"`:     `ends in escape \`,
		"\"mixed \\u00e9 and é\"": "mixed é and é",
	} {
		v, err := Parse([]byte(in))
		if err != nil || v != want {
			t.Errorf("Parse(%s) = %q, %v; want %q", in, v, err, want)
		}
	}
}

func TestNumbers(t *testing.T) {
	for in, want := range map[string]float64{"0": 0, "-0.5": -0.5, "12e2": 1200, "1E-2": 0.01, "1e400": math.Inf(1)} {
		v, err := Parse([]byte(in))
		if err != nil || v != want {
			t.Errorf("Parse(%s) = %v, %v; want %v", in, v, err, want)
		}
	}
}

func TestMalformedInputFails(t *testing.T) {
	for _, in := range []string{``, `{`, `{"a"}`, `{"a":}`, `[1,]`, `01`, `1.`, `-`, `"open`, `"bad \x"`, "\"tab\tin\"", `nul`, `{} {}`, `)]}'[1]`} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("Parse(%q) succeeded", in)
		}
	}
}

func TestDeepNestingIsRefused(t *testing.T) {
	deep := make([]byte, 0, 2*(maxDepth+10))
	for range maxDepth + 10 {
		deep = append(deep, '[')
	}
	for range maxDepth + 10 {
		deep = append(deep, ']')
	}
	if _, err := Parse(deep); err == nil {
		t.Error("nesting past the limit parsed")
	}
}

func TestWalkVisitsParentsFirstInDocumentOrder(t *testing.T) {
	v, _ := Parse([]byte(`{"name":"root","kids":[{"name":"a","kids":[{"name":"a1"}]},{"name":"b"}],"last":{"name":"c"}}`))
	var names []string
	Walk(v, func(o *Object) bool {
		names = append(names, String(o.Get("name")))
		return true
	})
	if got := len(names); got != 5 || names[0] != "root" || names[1] != "a" || names[2] != "a1" || names[3] != "b" || names[4] != "c" {
		t.Errorf("visited %v", names)
	}
	count := 0
	Walk(v, func(*Object) bool {
		count++
		return count < 2
	})
	if count != 2 {
		t.Errorf("walk went on after stop: %d", count)
	}
}

func TestStringAndTruthy(t *testing.T) {
	for _, c := range []struct {
		v      any
		s      string
		truthy bool
	}{
		{nil, "", false}, {"", "", false}, {"x", "x", true}, {0.0, "0", false}, {12.5, "12.5", true},
		{1e21, "1e+21", true}, {1e-7, "1e-7", true}, {true, "true", true}, {false, "false", false}, {[]any{}, "", true},
	} {
		if got := String(c.v); got != c.s {
			t.Errorf("String(%v) = %q, want %q", c.v, got, c.s)
		}
		if got := Truthy(c.v); got != c.truthy {
			t.Errorf("Truthy(%v) = %v", c.v, got)
		}
	}
}
