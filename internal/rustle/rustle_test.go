package rustle

import (
	"math"
	"testing"
)

func TestRoundHalvesGoUp(t *testing.T) {
	for in, want := range map[float64]float64{2.5: 3, -2.5: -2, -0.5: -0, 0.49999999999999994: 0, 1.5: 2, -1.4: -1} {
		if got := Round(in); got != want {
			t.Errorf("Round(%v) = %v, want %v", in, got, want)
		}
	}
	// A product as the argument is rounded before Round compares it, never fused into one
	// multiply-add: 0.0055 * 1000 is 5.5 once rounded.
	x := 0.0055
	if got := MS(x); got != 0.006 {
		t.Errorf("MS(0.0055) = %v, want 0.006", got)
	}
}

func TestTimesPrintAsTranscriptsAndLabelsShowThem(t *testing.T) {
	for in, want := range map[float64]string{0: "0:00", 59.9: "0:59", 3599.9: "59:59", 3600: "1:00:00", 36000: "10:00:00", -4: "0:00"} {
		if got := Clock(in); got != want {
			t.Errorf("Clock(%v) = %q, want %q", in, got, want)
		}
	}
	for _, c := range []struct {
		at       float64
		decimals int
		want     string
	}{{83.45, 2, "1:23.45"}, {165.1, 2, "2:45.10"}, {3725, 2, "1:02:05.00"}, {59.996, 2, "1:00.00"}, {12.34, 0, "0:12"}, {0.05, 1, "0:00.1"}} {
		if got := LabelTime(c.at, c.decimals); got != c.want {
			t.Errorf("LabelTime(%v, %d) = %q, want %q", c.at, c.decimals, got, c.want)
		}
	}
}

func TestCompactIsTheMetadataLine(t *testing.T) {
	meta := NewObj().Set("title", "T \"q\" ♪").Set("duration", 12.34567).Set("gap", nil).Set("notes", []string{}).
		Set("span", []float64{0, 1e21}).Set("ok", true).Set("count", 3).Set("bad", math.NaN()).
		Set("nested", NewObj().Set("unset", nil).Set("x", -0.0))
	want := `{"title":"T \"q\" ♪","duration":12.346,"span":[0,1e+21],"ok":true,"count":3,"bad":null,"nested":{"x":0}}`
	if got := Compact(meta); got != want {
		t.Errorf("Compact = %s\nwant      %s", got, want)
	}
	if got := Compact(NewObj().Set("chapters", []Chapter{{0, "Intro"}, {60.5, "Part   two"}})); got != `{"chapters":[[0,"Intro"],[60.5,"Part `+" "+` two"]]}` {
		t.Errorf("chapters %s", got)
	}
	if got := string(AppendString(nil, "a\x01\b\u007f\xff")); got != `"a\u0001\b`+"\u007f�"+`"` {
		t.Errorf("escapes %s", got)
	}
}

func TestNumbersPrintAsJSON(t *testing.T) {
	tenth := 0.1
	for in, want := range map[float64]string{3: "3", 12.5: "12.5", 1e21: "1e+21", 1e-7: "1e-7", 123456789012: "123456789012", tenth + 0.2: "0.30000000000000004", -0.0: "0"} {
		if got := NumString(in); got != want {
			t.Errorf("NumString(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestObjKeepsOrderAcrossChanges(t *testing.T) {
	o := NewObj().Set("a", 1).Set("b", 2).Set("c", 3)
	o.Set("a", 9)
	o.Delete("b")
	c := o.Clone().Set("d", 4)
	if got := Compact(o); got != `{"a":9,"c":3}` {
		t.Errorf("o %s", got)
	}
	if got := Compact(c); got != `{"a":9,"c":3,"d":4}` {
		t.Errorf("clone %s", got)
	}
}

func TestArgumentsAreClampedWithANote(t *testing.T) {
	notes := &Notes{}
	if got := Clamp("limit", 100, 1, 25, notes); got != 25 {
		t.Errorf("Clamp = %v", got)
	}
	if got := Whole("frames", 12.5, 1, 48, notes); got != 13 {
		t.Errorf("Whole = %v", got)
	}
	want := []string{"limit 100 clamped to 25", "frames 12.5 rounded to 13"}
	if got := notes.List(); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("notes %q", got)
	}
}

func TestCropSize(t *testing.T) {
	whole := float64(672 * 378)
	for _, c := range []struct {
		w, h int
		want [2]int
	}{
		{960, 540, [2]int{672, 378}}, {480, 270, [2]int{480, 270}}, {1920, 540, [2]int{950, 267}},
		{259, 821, [2]int{259, 821}}, {259, 65, [2]int{672, 169}}, {38, 22, [2]int{152, 88}},
	} {
		if got := CropSize(c.w, c.h, whole, FrameEdge, MaxEdge); got != c.want {
			t.Errorf("CropSize(%d, %d) = %v, want %v", c.w, c.h, got, c.want)
		}
	}
	if got := ImageTokens(1176, 504); got != 42*18 {
		t.Errorf("ImageTokens = %d", got)
	}
}

func TestTextAndNumberHelpers(t *testing.T) {
	if UTF16Len("\u00e9\U0001F600a") != 4 || UTF16Len("plain") != 5 {
		t.Errorf("UTF16Len counts UTF-16 units")
	}
	if got := TrimSpace("\ufeff  x  "); got != "x" {
		t.Errorf("TrimSpace = %q", got)
	}
	if got := CollapseSpace("  a  \n b  "); got != "a b" {
		t.Errorf("CollapseSpace = %q", got)
	}
	if got := Cut("ab\U0001F600c", 3); got != "ab" {
		t.Errorf("Cut kept half a surrogate pair: %q", got)
	}
	for in, want := range map[any]float64{" 12 ": 12, "": 0, "0x10": 16, "1e3": 1000, "inf": math.NaN(), "1_0": math.NaN(), true: 1, nil: math.NaN()} {
		got := Number(in)
		if got != want && !(math.IsNaN(got) && math.IsNaN(want)) {
			t.Errorf("Number(%#v) = %v, want %v", in, got, want)
		}
	}
}
