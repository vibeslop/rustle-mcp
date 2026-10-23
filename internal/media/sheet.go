package media

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/vibeslop/rustle-mcp/internal/imaging"
	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

// Contact sheets: frames laid out left to right, then top to bottom, each labelled in a
// strip above it, so no label covers a frame. Claude bills one token per 28 px patch, so a
// sheet is sized in patches: cells fill a 1176 px (42 patch) row and the sheet is padded
// to whole patches.

// Frame is a decoded picture: packed RGB24, Width * Height * 3 bytes.
type Frame struct {
	// At is the actual presentation time of the decoded frame, seconds from the media origin.
	At     float64
	Width  int
	Height int
	RGB    []byte
	// memo is what the sheet measured of these pixels; copies of a frame share it.
	memo *measures
}

// NewFrame is a frame of rgb pixels.
func NewFrame(at float64, width, height int, rgb []byte) *Frame {
	return &Frame{At: at, Width: width, Height: height, RGB: rgb, memo: &measures{}}
}

// WithAt is a copy of the frame labelled with another time; it shares the pixels and
// what was measured of them.
func (f *Frame) WithAt(at float64) *Frame {
	c := *f
	c.At = at
	return &c
}

// SheetCells is the cells on one sheet.
const SheetCells = 12

// gutter is the dark line between cells, so neighbouring frames never read as one picture.
const gutter = 2

var (
	background = [3]byte{16, 16, 16}
	ink        = [3]byte{255, 255, 255}
)

// A label sits in the strip above its frame, 1 px above it and at least 3 px below the
// row before, so it reads as its own frame's. The strip replaces the gutter between rows.
const (
	labelTop    = 3
	labelBottom = 1
	labelGap    = 2
)

// labelScale is the label font's scale in cells `width` px wide: 14 px tall, or 7 px in
// cells under 180 px (half-size cells of slides, storyboard thumbnails), where a 14 px
// strip would add a fifth to the sheet.
func labelScale(width int) int {
	if width < 180 {
		return 1
	}
	return 2
}

// Band is the height of a strip holding one line of labels over cells `width` px wide.
func Band(width int) int { return labelTop + imaging.TextHeight(labelScale(width)) + labelBottom }

// sheetCost is the patches of a sheet of rows x across cells of w x h under one-line strips.
func sheetCost(across, rows, w, h int) int {
	return toPatches(across*w+(across-1)*gutter) * toPatches(rows*(Band(w)+h)) / (rustle.Patch * rustle.Patch)
}

// fitShrink is how far FitPatches may shrink a cell: twelve 16:9 cells need 3.1% to give
// back the row of patches their label strips add.
const fitShrink = 0.96

// FitPatches shrinks a cell, keeping its shape, when that ends a full sheet of `samples`
// cells on whole patches and so saves a row or column of them. Frames are decoded at the
// result, so the picture is scaled, never cut.
func FitPatches(cell [2]int, samples, columns int) [2]int {
	page := min(samples, SheetCells)
	across := min(columns, page)
	rows := ceilDiv(page, across)
	best, cost := cell, sheetCost(across, rows, cell[0], cell[1])
	for h := cell[1] - 1; h >= int(math.Ceil(float64(cell[1])*fitShrink)); h-- {
		w := max(1, int(rustle.Round(float64(cell[0]*h)/float64(cell[1]))))
		if c := sheetCost(across, rows, w, h); c < cost {
			best, cost = [2]int{w, h}, c
		}
	}
	return best
}

// ColumnsFor is one frame for a single sample, two across for up to four, three for six,
// four beyond.
func ColumnsFor(samples int) int {
	n := min(samples, SheetCells)
	switch {
	case n <= 1:
		return 1
	case n <= 4:
		return 2
	case n <= 6:
		return 3
	}
	return 4
}

// maxCell is the largest cell, 14 patches: a few frames get room for detail without
// becoming a full-width picture per frame, which is what inspect_frame is for.
const maxCell = 392

// CellSize is the cell size for `samples` frames of a width x height source: the long edge
// fills its column up to maxCell and the source is never enlarged. Merged cells don't grow
// the others, so merging only ever removes tokens.
func CellSize(samples, width, height int) [2]int {
	columns := ColumnsFor(samples)
	edge := min(maxCell, (rustle.SheetWidth-(columns-1)*gutter)/columns)
	return Fit(width, height, edge, edge)
}

// CropCellSize is the cell size for a crop of width x height source pixels: the pixels of
// the uncropped cell, so a crop's sheet costs what the whole frame's does, enlarged while
// its long edge is under the uncropped cell's (see rustle.CropSize), and no wider than a sheet.
func CropCellSize(samples int, native [2]int, width, height int) [2]int {
	cell := CellSize(samples, native[0], native[1])
	return rustle.CropSize(width, height, float64(cell[0]*cell[1]), max(cell[0], cell[1]), rustle.SheetWidth)
}

// CropColumns is the columns for crop cells, whose shape is anyone's: as many as fit a
// sheet's width, in rows of equal length. A strip across the frame stacks one cell per
// row; a sidebar lines up six.
func CropColumns(samples, cellWidth int) int {
	page := min(samples, SheetCells)
	fits := max(1, (rustle.SheetWidth+gutter)/(cellWidth+gutter))
	rows := ceilDiv(page, min(page, fits))
	return ceilDiv(page, rows)
}

func ceilDiv(a, b int) int { return (a + b - 1) / b }

type Cell struct {
	Frame *Frame
	// First and Last are the 1-based numbers of the first and last sample this cell stands for.
	First, Last int
	// From and To are the times of those samples.
	From, To float64
}

// Label is a cell's badge: its sample numbers and times.
func Label(c Cell) string {
	if c.First == c.Last {
		return "#" + strconv.Itoa(c.First) + " " + rustle.LabelTime(c.From, 2)
	}
	return "#" + strconv.Itoa(c.First) + "-" + strconv.Itoa(c.Last) + " " + rustle.LabelTime(c.From, 2) + "-" + rustle.LabelTime(c.To, 2)
}

// grid: luma block means on a 48-column grid, so a block covers the same share of any cell.
const grid = 48

type Signature struct {
	Columns, Rows int
	Blocks        []float32
}

// measures is what the sheet measures of each frame, by its pixels: watch_video measures
// each cell as it is decoded (see Measurer), and the sheet then finds the numbers waiting.
type measures struct {
	mu        sync.Mutex
	luma      []byte
	signature Signature
	flatShare *float64
	strokes   *[2]float64
	flatness  *float64
	// shrunk are copies by scale (see Adapt).
	shrunk map[float64]*Frame
}

func (f *Frame) measures() *measures {
	if f.memo == nil {
		f.memo = &measures{}
	}
	m := f.memo
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.luma != nil {
		return m
	}
	width, height, rgb := f.Width, f.Height, f.RGB
	columns := min(grid, width)
	rows := max(1, min(height, int(rustle.Round(float64(grid*height)/float64(width)))))
	column := make([]int32, width)
	across := make([]uint32, columns)
	for x := range width {
		column[x] = int32(x * columns / width)
		across[column[x]]++
	}
	sums := make([]uint32, columns*rows)
	down := make([]uint32, rows)
	luma := make([]byte, width*height)
	for y, i, at := 0, 0, 0; y < height; y++ {
		block := y * rows / height
		row := block * columns
		down[block]++
		for x := 0; x < width; x, i, at = x+1, i+1, at+3 {
			v := (77*uint32(rgb[at]) + 150*uint32(rgb[at+1]) + 29*uint32(rgb[at+2])) >> 8
			luma[i] = byte(v)
			sums[row+int(column[x])] += v
		}
	}
	blocks := make([]float32, columns*rows)
	for i := range blocks {
		blocks[i] = float32(float64(sums[i]) / float64(across[i%columns]*down[i/columns]))
	}
	m.luma = luma
	m.signature = Signature{Columns: columns, Rows: rows, Blocks: blocks}
	return m
}

// SignatureOf is the frame's block luma signature.
func SignatureOf(f *Frame) Signature { return f.measures().signature }

// Measurer takes every measure a sheet may need of each decoded cell of one call, ahead of
// the sheet. A sheet shrinks to the largest scale any of its frames needs (see Adapt), so
// each cell is shrunk to the largest seen so far; footage needs none, and once a frame
// looks like footage the stroke counts are skipped too. Cells arrive from several decoders
// at once.
func Measurer() func(*Frame) {
	var mu sync.Mutex
	needed := 0.5
	return func(frame *Frame) {
		share := FlatShare(frame)
		flatness(frame)
		mu.Lock()
		if share < synthetic {
			needed = 1
		}
		// A frame too dark to tell doesn't count towards the scale (see DetailScale).
		if needed == 1 || math.IsNaN(share) {
			mu.Unlock()
			return
		}
		mu.Unlock()
		scale := strokeScale(FineStrokes(frame))
		mu.Lock()
		needed = math.Max(needed, scale)
		scale = needed
		mu.Unlock()
		for scale < 1 && float64(max(frame.Width, frame.Height))*scale < minCell {
			scale += 0.25
		}
		if scale < 1 {
			flatness(Shrink(frame, scale))
		}
	}
}

type Difference struct {
	// Mean is the mean absolute difference of block luma, 0-255.
	Mean float64
	// Max is the largest single-block difference: a changed word or cursor moves one block a lot.
	Max float64
}

func Compare(a, b Signature) Difference {
	if a.Columns != b.Columns || a.Rows != b.Rows {
		return Difference{255, 255}
	}
	total, most := 0.0, 0.0
	for i := range a.Blocks {
		d := math.Abs(float64(a.Blocks[i]) - float64(b.Blocks[i]))
		total += d
		if d > most {
			most = d
		}
	}
	return Difference{Mean: total / float64(len(a.Blocks)), Max: most}
}

// Measured on cells of real footage: two decodes of a still scene differ by at most 5 in a
// block; twinkling backgrounds, a new line of 7 px terminal text and a moved cursor reach
// 12-35; a changed word 30; a head turning in a talking-head video 70+. So only stills
// merge. A one-digit change in small text moves a block by 2-3, but it is also invisible
// at cell size, where showing it twice would not help.
const (
	sameMean = 1
	sameMax  = 8
)

func NearDuplicate(d Difference) bool { return d.Mean < sameMean && d.Max < sameMax }

// Group merges runs of consecutive near-duplicate frames into one cell showing the run's
// first frame. Each frame is compared with that first frame, not its neighbour, so a slow
// pan can't drift through a chain of small steps into one cell.
func Group(frames []*Frame) []Cell {
	var cells []Cell
	var anchor Signature
	for i, frame := range frames {
		current := SignatureOf(frame)
		if n := len(cells); n > 0 && NearDuplicate(Compare(anchor, current)) {
			cells[n-1].Last = i + 1
			cells[n-1].To = frame.At
			continue
		}
		cells = append(cells, Cell{Frame: frame, First: i + 1, Last: i + 1, From: frame.At, To: frame.At})
		anchor = current
	}
	return cells
}

// contrast is the luma swing that counts as a stroke. Text and line art swing far more; at
// 16 or 12 the ringing H.264 leaves beside sharp edges starts to count, and the fan-out
// clips' 3 px periods rise from 0.4-2.1 to 18-25 per 10,000 pixels.
const contrast = 32

// FineStrokes is fine strokes per 10,000 pixels: along every row and column, alternating
// peaks and valleys that each differ from the last by contrast, and how often two peaks
// (or two valleys) lie at most 2 px and at most 3 px apart. That distance is one stroke
// plus one gap; scaled by s it must stay over 2 px (the sampling limit) for the strokes to
// stay apart, so 0.75 loses periods of 2 px and 0.5 loses periods of 3 px.
func FineStrokes(frame *Frame) [2]float64 {
	m := frame.measures()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.strokes != nil {
		return *m.strokes
	}
	width, height := frame.Width, frame.Height
	var counts [4]uint32
	for y := range height {
		swings(m.luma, y*width, width, 1, &counts)
	}
	for x := range width {
		swings(m.luma, x, height, width, &counts)
	}
	per := 10_000 / float64(width*height)
	strokes := [2]float64{float64(counts[1]+counts[2]) * per, float64(counts[1]+counts[2]+counts[3]) * per}
	m.strokes = &strokes
	return strokes
}

// swings counts peak-to-peak and valley-to-valley distances of 1-3 px along one line of luma.
func swings(luma []byte, start, length, stride int, counts *[4]uint32) {
	hi := int(luma[start])
	lo, hiAt, loAt, peak, valley := hi, 0, 0, -9, -9
	// 1 while climbing to a peak, -1 while falling to a valley, 0 until the first swing.
	heading := 0
	for i, at := 1, start+stride; i < length; i, at = i+1, at+stride {
		v := int(luma[at])
		if v > hi {
			hi, hiAt = v, i
		} else if v < lo {
			lo, loAt = v, i
		}
		if heading >= 0 && hi-v >= contrast {
			if hiAt-peak <= 3 {
				counts[hiAt-peak]++
			}
			peak, heading, lo, loAt = hiAt, -1, v, i
		} else if heading <= 0 && v-lo >= contrast {
			if loAt-valley <= 3 {
				counts[loAt-valley]++
			}
			valley, heading, hi, hiAt = loAt, 1, v, i
		}
	}
}

// dark is the luma at or below which a pair of neighbours is shadow or a letterbox bar, not content.
const dark = 24

// FlatShare is the share of horizontal neighbours with equal luma, among pairs that aren't
// both dark; NaN for a frame too dark to tell. Graphics are flat fills between sharp
// edges; footage has gradients and grain, except where it is crushed to black.
func FlatShare(frame *Frame) float64 {
	m := frame.measures()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.flatShare != nil {
		return *m.flatShare
	}
	width, height := frame.Width, frame.Height
	luma := m.luma
	same, lit := 0, 0
	for y := 0; y < height; y += 2 {
		for i, end := y*width+1, (y+1)*width; i < end; i++ {
			if luma[i] <= dark && luma[i-1] <= dark {
				continue
			}
			lit++
			if luma[i] == luma[i-1] {
				same++
			}
		}
	}
	share := math.NaN()
	if !(float64(lit) < float64(ceilDiv(height, 2)*(width-1))/20) {
		share = float64(same) / float64(lit)
	}
	m.flatShare = &share
	return share
}

// Most frames need their full cell: real footage, and any text under about 15 px tall.
// Flat synthetic frames with large text (slides, title cards, diagrams, the fan-out clips)
// read the same at half the size, for a quarter of the tokens. Measured on 292 px cells:
//
//   - synthetic (FlatShare): every frame of camera footage, 3D renders, pixel art and
//     talking heads, including versions darkened until faces are barely lit, 7-68%;
//     slides, title cards and test patterns 76-99%. Natural detail (fur, faces, texture)
//     is too low in contrast for FineStrokes to see, so a frame under 75% keeps its cell
//     whatever its strokes say. Screen recordings and motion graphics (29-93%) mostly
//     fail here too.
//   - fine, 3 px periods (lost at half size): the 36 fan-out clips 0.4-2.1 per 10,000
//     pixels, a code card 2.7; slides with 6-15 px text 33-113, screen recordings and
//     motion graphics with subtitles 145-290. 2 px periods (lost at three quarters): a
//     720p test pattern and 18-29 px text 0.2-1.0; 6-15 px text 5-42.
//   - dense: at three quarters a 3 px period keeps under half its contrast. A few read
//     from context (18-29 px text has 4-31 per 10,000 pixels); a frame full of them
//     doesn't, like pixel art's lit windows and rain at 98.
const (
	synthetic = 0.75
	fine      = 4
	dense     = 40
	// minCell is the long edge no cell shrinks below, at which a frame still shows its layout.
	minCell = 140
)

// strokeScale is the smallest of 1, 0.75 and 0.5 at which a frame with these stroke
// counts keeps them apart.
func strokeScale(s [2]float64) float64 {
	switch {
	case s[0] > fine || s[1] > dense:
		return 1
	case s[1] > fine:
		return 0.75
	}
	return 0.5
}

// DetailScale is the smallest of 1, 0.75 and 0.5 at which every frame keeps its strokes apart.
func DetailScale(frames []*Frame) float64 {
	var lit []*Frame
	for _, f := range frames {
		share := FlatShare(f)
		if share < synthetic {
			return 1
		}
		if !math.IsNaN(share) {
			lit = append(lit, f)
		}
	}
	if len(lit) == 0 {
		return 1
	}
	scale := 0.5
	for _, f := range lit {
		scale = math.Max(scale, strokeScale(FineStrokes(f)))
		if scale == 1 {
			break
		}
	}
	return scale
}

// Resize is a bilinear resize, for frames that arrive larger than their cell (YouTube storyboards).
func Resize(frame *Frame, width, height int) *Frame {
	w, h, rgb := frame.Width, frame.Height, frame.RGB
	if w == width && h == height {
		return frame
	}
	out := make([]byte, width*height*3)
	sx, sy := float64(w)/float64(width), float64(h)/float64(height)
	to := 0
	for y := range height {
		fy := math.Min(float64(h-1), math.Max(0, (float64(y)+0.5)*sy-0.5))
		y0 := int(fy)
		y1, wy := min(h-1, y0+1), fy-float64(y0)
		for x := range width {
			fx := math.Min(float64(w-1), math.Max(0, (float64(x)+0.5)*sx-0.5))
			x0 := int(fx)
			x1, wx := min(w-1, x0+1), fx-float64(x0)
			a, b, c, d := (y0*w+x0)*3, (y0*w+x1)*3, (y1*w+x0)*3, (y1*w+x1)*3
			for k := range 3 {
				// Each product is rounded before its sum, as JavaScript rounds it, not fused into one multiply-add.
				top := float64(rgb[a+k]) + float64(float64(int(rgb[b+k])-int(rgb[a+k]))*wx)
				bottom := float64(rgb[c+k]) + float64(float64(int(rgb[d+k])-int(rgb[c+k]))*wx)
				out[to] = byte(rustle.Round(top + float64((bottom-top)*wy)))
				to++
			}
		}
	}
	return NewFrame(frame.At, width, height, out)
}

// Shrink scales a frame by 0.5 or 0.75 with an area filter, each output pixel averaging
// the inputs it covers: 2 x 2 pixels to 1, or 4 x 4 to 3 x 3 with weights 3:1, 2:2 and
// 1:3 each way. Whole blocks keep the arithmetic in small integers; last pixels that
// don't fill one drop.
func Shrink(frame *Frame, scale float64) *Frame {
	m := frame.memo
	if m != nil {
		m.mu.Lock()
		done := m.shrunk[scale]
		m.mu.Unlock()
		if done != nil {
			return done.WithAt(frame.At)
		}
	}
	var shrunk *Frame
	if scale == 0.5 {
		shrunk = half(frame)
	} else {
		shrunk = threeQuarters(frame)
	}
	if m != nil {
		m.mu.Lock()
		if m.shrunk == nil {
			m.shrunk = map[float64]*Frame{}
		}
		m.shrunk[scale] = shrunk
		m.mu.Unlock()
	}
	return shrunk
}

func half(frame *Frame) *Frame {
	w, h, rgb := frame.Width, frame.Height, frame.RGB
	line := w * 3
	width, height := w>>1, h>>1
	out := make([]byte, width*height*3)
	to := 0
	for y := range height {
		for x, a, b := 0, 2*y*line, 2*y*line+line; x < width; x, a, b = x+1, a+6, b+6 {
			out[to] = byte((int(rgb[a]) + int(rgb[a+3]) + int(rgb[b]) + int(rgb[b+3]) + 2) >> 2)
			out[to+1] = byte((int(rgb[a+1]) + int(rgb[a+4]) + int(rgb[b+1]) + int(rgb[b+4]) + 2) >> 2)
			out[to+2] = byte((int(rgb[a+2]) + int(rgb[a+5]) + int(rgb[b+2]) + int(rgb[b+5]) + 2) >> 2)
			to += 3
		}
	}
	return NewFrame(frame.At, width, height, out)
}

func threeQuarters(frame *Frame) *Frame {
	w, h, rgb := frame.Width, frame.Height, frame.RGB
	line := w * 3
	width, height := (w>>2)*3, (h>>2)*3
	stride := width * 3
	rows := h &^ 3
	// Four times the horizontal average, unrounded until the vertical pass.
	across := make([]uint16, stride*rows)
	for y := range rows {
		for at, to, end := y*line, y*stride, y*stride+stride; to < end; at, to = at+12, to+9 {
			for c := range 3 {
				p0, p1, p2, p3 := uint16(rgb[at+c]), uint16(rgb[at+3+c]), uint16(rgb[at+6+c]), uint16(rgb[at+9+c])
				across[to+c] = 3*p0 + p1
				across[to+3+c] = 2 * (p1 + p2)
				across[to+6+c] = p2 + 3*p3
			}
		}
	}
	out := make([]byte, stride*height)
	for b := 0; b < height/3; b++ {
		q, to := 4*b*stride, 3*b*stride
		for x := range stride {
			p0, p1, p2, p3 := uint32(across[q+x]), uint32(across[q+stride+x]), uint32(across[q+2*stride+x]), uint32(across[q+3*stride+x])
			out[to+x] = byte((3*p0 + p1 + 8) >> 4)
			out[to+stride+x] = byte((2*(p1+p2) + 8) >> 4)
			out[to+2*stride+x] = byte((p2 + 3*p3 + 8) >> 4)
		}
	}
	return NewFrame(frame.At, width, height, out)
}

// Adapt shrinks the cells of a low-detail sheet (see DetailScale). A cell may then lose up
// to 3% of its width or height, a pixel or two from each edge, when that ends the first
// sheet on whole patches and so saves a column or row of them.
func Adapt(cells []Cell, columns int, cell [2]int) ([]Cell, [2]int) {
	cw, ch := cell[0], cell[1]
	frames := make([]*Frame, len(cells))
	for i, c := range cells {
		frames[i] = c.Frame
	}
	scale := DetailScale(frames)
	for scale < 1 && float64(max(cw, ch))*scale < minCell {
		scale += 0.25
	}
	if scale >= 1 {
		return cells, cell
	}
	for _, c := range cells {
		if min(c.Frame.Width, c.Frame.Height) < 4 {
			return cells, cell
		}
	}
	shrunk := make([]Cell, len(cells))
	for i, c := range cells {
		c.Frame = Shrink(c.Frame, scale)
		shrunk[i] = c
	}
	page := min(len(cells), SheetCells)
	across := min(columns, page)
	rows := ceilDiv(page, across)
	cost := func(w, h int) int { return sheetCost(across, rows, w, h) }
	full := [2]int{(cw >> 2) * 3, (ch >> 2) * 3}
	if scale == 0.5 {
		full = [2]int{cw >> 1, ch >> 1}
	}
	best := full
	for w := full[0]; w >= int(math.Ceil(float64(full[0])*0.97)); w-- {
		for h := full[1]; h >= int(math.Ceil(float64(full[1])*0.97)); h-- {
			if c, b := cost(w, h), cost(best[0], best[1]); c < b || (c == b && w*h > best[0]*best[1]) {
				best = [2]int{w, h}
			}
		}
	}
	return shrunk, best
}

type Sheet struct {
	RGB           []byte
	Width, Height int
	// Flat is the share of the frames' pixels equal to their left neighbour (see EncodeSheet).
	Flat float64
}

func toPatches(pixels int) int { return ceilDiv(pixels, rustle.Patch) * rustle.Patch }

// label is a cell's label laid out for a strip: its lines and their scale.
type label struct {
	lines []string
	scale int
}

// layoutLabel fits a label to `width` in a cell `cell` px wide: whole, else wrapped, else
// wrapped at scale 1, so it never runs into the next cell's.
func layoutLabel(text string, width, cell int) label {
	for _, scale := range []int{labelScale(cell), 1} {
		lines := Wrap(text, width, scale)
		fits := true
		for _, l := range lines {
			fits = fits && imaging.TextWidth(l, scale) <= width
		}
		if fits {
			return label{lines, scale}
		}
	}
	return label{Wrap(text, width, 1), 1}
}

func (l label) height() int {
	return len(l.lines)*imaging.TextHeight(l.scale) + (len(l.lines)-1)*labelGap
}

// draw puts the label's last line 1 px above a frame whose top-left corner is (x, top).
func (l label) draw(sheet *Sheet, x, top int) {
	y := top - labelBottom - l.height()
	for _, line := range l.lines {
		imaging.DrawText(sheet.RGB, sheet.Width, sheet.Height, x, y, line, l.scale, ink)
		y += imaging.TextHeight(l.scale) + labelGap
	}
}

// timeAhead is a time starting the rest of a label, after the dash of a range.
var timeAhead = regexp.MustCompile(`^\d+:\d`)

// Wrap breaks a label into lines no wider than width, after the space or between the two
// times of a range. A crop's cell can be narrower than its label, and the next cell would
// cut it off, "18:49.33" left reading "18:49.". Where even the lines don't fit, one line
// covers less.
func Wrap(text string, width, scale int) []string {
	var lines []string
	line := ""
	for _, piece := range wrapPieces(text) {
		if line != "" && imaging.TextWidth(strings.TrimRight(line+piece, " "), scale) > width {
			lines = append(lines, strings.TrimRight(line, " "))
			line = ""
		}
		line += piece
	}
	lines = append(lines, strings.TrimRight(line, " "))
	for _, l := range lines {
		if imaging.TextWidth(l, scale) > width {
			return []string{text}
		}
	}
	return lines
}

// wrapPieces splits as text.split(/(?<= )|(?<=-)(?=\d+:\d)/) does: after every space, and
// after a dash that a time follows.
func wrapPieces(text string) []string {
	var pieces []string
	start := 0
	for i := 0; i < len(text); i++ {
		cut := text[i] == ' ' || (text[i] == '-' && timeAhead.MatchString(text[i+1:]))
		if cut && i+1 < len(text) {
			pieces = append(pieces, text[start:i+1])
			start = i + 1
		}
	}
	return append(pieces, text[start:])
}

// Compose packs cells into sheets of up to twelve, `columns` across (fewer when a sheet
// holds fewer cells), each row under a strip for its labels. Every cell is `cell` pixels;
// a smaller frame is centred in it, a larger one cropped around its centre.
func Compose(cells []Cell, columns int, cell [2]int) []*Sheet {
	cw, ch := cell[0], cell[1]
	var sheets []*Sheet
	for begin := 0; begin < len(cells); begin += SheetCells {
		page := cells[begin:min(len(cells), begin+SheetCells)]
		across := min(columns, len(page))
		rows := ceilDiv(len(page), across)
		labels := make([]label, len(page))
		band := 0
		for j, item := range page {
			// A label starts at its frame's left edge and may use the rest of the cell.
			labels[j] = layoutLabel(Label(item), cw-((cw-min(item.Frame.Width, cw))>>1), cw)
			band = max(band, labelTop+labels[j].height()+labelBottom)
		}
		width := toPatches(across*cw + (across-1)*gutter)
		height := toPatches(rows * (band + ch))
		sheet := &Sheet{RGB: make([]byte, width*height*3), Width: width, Height: height}
		imaging.FillRect(sheet.RGB, width, height, 0, 0, width, height, background)
		for j, item := range page {
			x := (j % across) * (cw + gutter)
			y := (j/across)*(band+ch) + band
			frame := item.Frame
			fw, fh := min(frame.Width, cw), min(frame.Height, ch)
			ox, oy := x+((cw-fw)>>1), y+((ch-fh)>>1)
			// A larger frame loses equal strips from opposite edges, never all from one.
			sx, sy := (frame.Width-fw)>>1, (frame.Height-fh)>>1
			for row := range fh {
				from := ((sy+row)*frame.Width + sx) * 3
				copy(sheet.RGB[((oy+row)*width+ox)*3:], frame.RGB[from:from+fw*3])
			}
			labels[j].draw(sheet, ox, oy)
			sheet.Flat += flatness(frame) / float64(len(page))
		}
		sheets = append(sheets, sheet)
	}
	return sheets
}

// flatness is the share of pixels equal to their left neighbour, on every fourth row.
func flatness(frame *Frame) float64 {
	m := frame.memo
	if m != nil {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.flatness != nil {
			return *m.flatness
		}
	}
	rgb, width, height := frame.RGB, frame.Width, frame.Height
	same, total := 0, 0
	for y := 0; y < height; y += 4 {
		for x, at := 1, (y*width+1)*3; x < width; x, at = x+1, at+3 {
			if rgb[at] == rgb[at-3] && rgb[at+1] == rgb[at-2] && rgb[at+2] == rgb[at-1] {
				same++
			}
			total++
		}
	}
	share := 1.0
	if total > 0 {
		share = float64(same) / float64(total)
	}
	if m != nil {
		m.flatness = &share
	}
	return share
}

// flat: flat synthetic frames (slides, renders, blank UI) compress far better as lossless
// PNG; anything decoded from camera or screen-recording video is 2-4x smaller as JPEG.
// Encoding only the likely winner saves an encoder pass.
const flat = 0.9

// EncodeSheet is the sheet as PNG or JPEG, whichever it likely compresses better as.
func EncodeSheet(sheet *Sheet) (data []byte, mimeType string) {
	if sheet.Flat >= flat {
		return imaging.EncodePNG(sheet.RGB, sheet.Width, sheet.Height, 3, 6), "image/png"
	}
	return imaging.EncodeJPEG(sheet.RGB, sheet.Width, sheet.Height, 90), "image/jpeg"
}
