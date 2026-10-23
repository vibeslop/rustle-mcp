package media

import (
	"reflect"
	"testing"

	"github.com/vibeslop/rustle-mcp/internal/imaging"
	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

func solid(width, height int, value byte, at float64) *Frame {
	rgb := make([]byte, width*height*3)
	for i := range rgb {
		rgb[i] = value
	}
	return NewFrame(at, width, height, rgb)
}

func fill(rgb []byte, value byte, from, to int) {
	for i := from; i < to; i++ {
		rgb[i] = value
	}
}

func sizes(sheets []*Sheet) [][2]int {
	out := make([][2]int, len(sheets))
	for i, s := range sheets {
		out[i] = [2]int{s.Width, s.Height}
	}
	return out
}

// Layout

func TestColumnsGrowWithTheNumberOfSamplesUpToFour(t *testing.T) {
	var got []int
	for _, n := range []int{1, 2, 4, 5, 6, 7, 12, 48} {
		got = append(got, ColumnsFor(n))
	}
	if want := []int{1, 2, 2, 3, 3, 4, 4, 4}; !reflect.DeepEqual(got, want) {
		t.Fatalf("columns %v, want %v", got, want)
	}
}

func TestCellsFillARowAtTheSourceAspectAndNeverEnlargeIt(t *testing.T) {
	for _, c := range []struct {
		samples, width, height int
		want                   [2]int
	}{
		{12, 1920, 1080, [2]int{292, 164}},
		{1, 1920, 1080, [2]int{392, 221}},
		{6, 1920, 1080, [2]int{390, 219}},
		{12, 1080, 1920, [2]int{164, 292}},
		{12, 320, 180, [2]int{292, 164}},
		{12, 160, 90, [2]int{160, 90}},
	} {
		if got := CellSize(c.samples, c.width, c.height); got != c.want {
			t.Errorf("CellSize(%d, %d, %d) = %v, want %v", c.samples, c.width, c.height, got, c.want)
		}
	}
}

func TestSheetsAreWholePatchesAndAtMost1176Wide(t *testing.T) {
	cell := [2]int{292, 164}
	cells := func(n int) []Cell {
		out := make([]Cell, n)
		for i := range out {
			out[i] = Cell{Frame: solid(292, 164, byte(i*10), 0), First: i + 1, Last: i + 1, From: float64(i), To: float64(i)}
		}
		return out
	}
	for _, n := range []int{1, 3, 5, 12, 13, 30} {
		for _, sheet := range Compose(cells(n), ColumnsFor(n), cell) {
			if sheet.Width%28 != 0 || sheet.Height%28 != 0 || sheet.Width > 1176 {
				t.Errorf("%d cells: sheet %dx%d", n, sheet.Width, sheet.Height)
			}
		}
	}
	// The label strips take twelve 16:9 cells to 20 rows of patches; cells 3% smaller fit 19.
	cell = FitPatches(cell, 12, 4)
	if cell != [2]int{283, 159} {
		t.Errorf("fitted cell %v, want [283 159]", cell)
	}
	twelve := Compose(cells(12), 4, cell)[0]
	if twelve.Width != 1148 || twelve.Height != 532 {
		t.Errorf("twelve cells: %dx%d, want 1148x532", twelve.Width, twelve.Height)
	}
	if tokens := rustle.ImageTokens(twelve.Width, twelve.Height); tokens > 779 {
		t.Errorf("twelve cells cost %d tokens", tokens)
	}
	// Thirteen cells: a full sheet, then one cell, never a sheet of twelve empty slots.
	if got, want := sizes(Compose(cells(13), 4, cell)), [][2]int{{1148, 532}, {308, 196}}; !reflect.DeepEqual(got, want) {
		t.Errorf("thirteen cells: %v, want %v", got, want)
	}
	// Where a few percent saves no patches, cells keep their size.
	if got := FitPatches([2]int{392, 221}, 1, 1); got != [2]int{392, 221} {
		t.Errorf("one cell fitted to %v", got)
	}
}

func TestLabelsSitAboveTheirFramesAndCoverNoneOfThem(t *testing.T) {
	cells := []Cell{
		{Frame: solid(292, 164, 200, 0), First: 1, Last: 1},
		{Frame: solid(292, 164, 200, 1), First: 2, Last: 3, From: 1, To: 2},
	}
	sheet := Compose(cells, 2, [2]int{292, 164})[0]
	band := Band(292)
	for y := band; y < band+164; y++ {
		for x := range 292 {
			if at := (y*sheet.Width + x) * 3; sheet.RGB[at] != 200 {
				t.Fatalf("pixel (%d, %d) of the first frame is %d", x, y, sheet.RGB[at])
			}
		}
	}
	// The strip holds white label pixels, ending 1 px above the frame.
	ink, last := 0, 0
	for y := range band {
		for x := range 292 {
			if sheet.RGB[(y*sheet.Width+x)*3] == 255 {
				ink++
				last = y
			}
		}
	}
	if ink == 0 || last != band-2 {
		t.Errorf("%d label pixels, the lowest in row %d; want some, the lowest in row %d", ink, last, band-2)
	}
}

func TestLabelsNameTheSampleNumbersAndTimes(t *testing.T) {
	one := Cell{Frame: solid(4, 4, 0, 0), First: 3, Last: 3, From: 83.45, To: 83.45}
	run := one
	run.Last, run.To = 5, 165.1
	hour := one
	hour.From = 3725
	for _, c := range []struct {
		cell Cell
		want string
	}{
		{one, "#3 1:23.45"},
		{run, "#3-5 1:23.45-2:45.10"},
		{hour, "#3 1:02:05.00"},
	} {
		if got := Label(c.cell); got != c.want {
			t.Errorf("Label = %q, want %q", got, c.want)
		}
	}
}

func TestALabelWiderThanItsCellWrapsBetweenItsTimes(t *testing.T) {
	for _, c := range []struct {
		text  string
		width int
		want  []string
	}{
		{"#3-5 18:38.08-18:49.33", 119, []string{"#3-5 18:38.08-", "18:49.33"}},
		{"#3-5 18:38.08-18:49.33", 60, []string{"#3-5", "18:38.08-", "18:49.33"}},
		{"#12 1:02:05.00", 119, []string{"#12 1:02:05.00"}},
		// Where even the lines don't fit, one line covers less.
		{"#3-5 18:38.08-18:49.33", 30, []string{"#3-5 18:38.08-18:49.33"}},
	} {
		if got := Wrap(c.text, c.width, 1); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Wrap(%q, %d) = %q, want %q", c.text, c.width, got, c.want)
		}
	}
}

// Near duplicates

func TestIdenticalAndNoisyStillsShareACellAnyVisibleChangeDoesNot(t *testing.T) {
	base := func() *Frame {
		f := solid(334, 188, 240, 0)
		imaging.DrawText(f.RGB, 334, 188, 20, 20, "Quarterly report", 2, [3]byte{20, 20, 20})
		return f
	}
	still := base()
	noisy := base()
	for i := 0; i < len(noisy.RGB); i += 97 {
		noisy.RGB[i] ^= 3
	}
	word := base()
	imaging.DrawText(word.RGB, 334, 188, 20, 60, "Q3", 1, [3]byte{20, 20, 20})
	cells := Group([]*Frame{still, noisy.WithAt(1), word.WithAt(2)})
	var runs [][2]int
	for _, c := range cells {
		runs = append(runs, [2]int{c.First, c.Last})
	}
	if want := [][2]int{{1, 2}, {3, 3}}; !reflect.DeepEqual(runs, want) {
		t.Fatalf("runs %v, want %v", runs, want)
	}
	if cells[0].To != 1 {
		t.Errorf("first run ends at %v, want 1", cells[0].To)
	}
}

func TestASlowFadeCannotDriftIntoOneCell(t *testing.T) {
	// Frames are compared with the first frame of a run, not their neighbour.
	fade := make([]*Frame, 12)
	for i := range fade {
		fade[i] = solid(64, 36, byte(100+i*3), float64(i))
	}
	if cells := Group(fade); len(cells) < 4 {
		t.Fatalf("fade merged into %d cells", len(cells))
	}
}

// Content-adaptive cells

func card(scale int, text string) *Frame {
	if text == "" {
		text = "Quarterly report 2026"
	}
	f := solid(292, 164, 30, 0)
	imaging.DrawText(f.RGB, 292, 164, 8, 20, text, scale, [3]byte{240, 240, 240})
	return f
}

func TestFlatFramesShrinkAsFarAsTheirFinestStrokesAllow(t *testing.T) {
	// A few stripes 3 px apart blur together at half size but not at three quarters; a
	// frame full of them would lose too much contrast even there.
	stripes := func(rows int) *Frame {
		f := solid(292, 164, 30, 0)
		for y := 60; y < 60+rows; y++ {
			for x := 20; x < 50; x += 3 {
				fill(f.RGB, 230, (y*292+x)*3, (y*292+x+2)*3)
			}
		}
		return f
	}
	// Camera grain keeps a full cell, whatever the strokes.
	grain := card(4, "")
	for i := range grain.RGB {
		noise := int(uint32(uint64(i)*2654435761)>>27) - 16
		grain.RGB[i] = byte(max(0, min(255, int(grain.RGB[i])+noise)))
	}
	for _, c := range []struct {
		name   string
		frames []*Frame
		want   float64
	}{
		// The 5x7 font at scale 2 has 2 px strokes and gaps: still apart at half size.
		{"text at scale 2 and 3", []*Frame{card(2, ""), card(3, "")}, 0.5},
		{"text in 1 px strokes", []*Frame{card(2, ""), card(1, "")}, 1},
		{"a few 3 px stripes", []*Frame{card(3, ""), stripes(8)}, 0.75},
		{"a frame full of 3 px stripes", []*Frame{card(3, ""), stripes(40)}, 1},
		{"camera grain", []*Frame{grain}, 1},
	} {
		if got := DetailScale(c.frames); got != c.want {
			t.Errorf("%s: DetailScale = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestShrinkingAveragesWholeBlocksAndDropsThePixelsPastTheLast(t *testing.T) {
	ramp := func(width, height int, value func(x int) int) *Frame {
		rgb := make([]byte, width*height*3)
		for i := range width * height {
			fill(rgb, byte(value(i%width)), i*3, i*3+3)
		}
		return NewFrame(3, width, height, rgb)
	}
	half := Shrink(ramp(5, 3, func(x int) int { return x * 10 }), 0.5)
	if half.Width != 2 || half.Height != 1 || half.At != 3 {
		t.Errorf("half: %dx%d at %v, want 2x1 at 3", half.Width, half.Height, half.At)
	}
	if want := []byte{5, 5, 5, 25, 25, 25}; !reflect.DeepEqual(half.RGB, want) {
		t.Errorf("half: %v, want %v", half.RGB, want)
	}
	three := Shrink(ramp(4, 4, func(x int) int { return x * 40 }), 0.75)
	if three.Width != 3 || three.Height != 3 {
		t.Errorf("three quarters: %dx%d, want 3x3", three.Width, three.Height)
	}
	if want := []byte{10, 10, 10, 60, 60, 60, 110, 110, 110}; !reflect.DeepEqual(three.RGB[:9], want) {
		t.Errorf("three quarters: %v, want %v", three.RGB[:9], want)
	}
}

func TestALowDetailSheetGetsHalfSizeCellsTrimmedToWholePatches(t *testing.T) {
	cells := make([]Cell, 12)
	for i := range cells {
		cells[i] = Cell{Frame: card(3, "").WithAt(float64(i)), First: i + 1, Last: i + 1, From: float64(i), To: float64(i)}
	}
	small, cell := Adapt(cells, 4, [2]int{292, 164})
	// 4 x 146 + gutters is 590 px, 22 patches; 145 px cells fit 21.
	if cell != [2]int{145, 82} {
		t.Errorf("cell %v, want [145 82]", cell)
	}
	if f := small[0].Frame; f.Width != 146 || f.Height != 82 {
		t.Errorf("shrunk frame %dx%d, want 146x82", f.Width, f.Height)
	}
	if got, want := sizes(Compose(small, 4, cell)), [][2]int{{588, 280}}; !reflect.DeepEqual(got, want) {
		t.Errorf("sheets %v, want %v", got, want)
	}
	detailed := make([]Cell, len(cells))
	for i, c := range cells {
		c.Frame = card(1, "")
		detailed[i] = c
	}
	if _, cell := Adapt(detailed, 4, [2]int{292, 164}); cell != [2]int{292, 164} {
		t.Errorf("detailed cell %v, want [292 164]", cell)
	}
}

func TestAFrameWiderThanItsCellLosesEqualStripsFromBothSides(t *testing.T) {
	rgb := make([]byte, 600)
	for i := range 200 {
		fill(rgb, byte((i%10)*20), i*3, i*3+3)
	}
	wide := NewFrame(0, 10, 20, rgb)
	sheet := Compose([]Cell{{Frame: wide, First: 1, Last: 1}}, 1, [2]int{6, 20})[0]
	// The cell shows source columns 2-7.
	row := (Band(6) + 19) * sheet.Width * 3
	if got := [2]byte{sheet.RGB[row], sheet.RGB[row+5*3]}; got != [2]byte{40, 140} {
		t.Errorf("bottom row starts %v, want [40 140]", got)
	}
}
