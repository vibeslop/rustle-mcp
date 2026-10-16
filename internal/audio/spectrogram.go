package audio

import (
	"math"
	"strconv"

	"github.com/vibeslop/rustle-mcp/internal/imaging"
	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

// Geometry in whole 28 px patches, the unit Claude bills images in: 18 x 9 patches (162
// tokens) for stereo and 18 x 5 (90) for mono. Labels use the bitmap font at scale 2, so
// they survive a harness shrinking the image by half.
const (
	width = 504
	left  = 68
	right = 16
	plot  = width - left - right
	top   = 4
	gap   = 4
	// axis is the room below the panels: ticks, time labels, legend row.
	axis  = 44
	scale = 2
	line  = 7 * scale
	// labelGap is the least room between two time labels.
	labelGap = 12
	// low is the lowest frequency drawn, in Hz.
	low = 30
)

// panelRows is a panel's height by panel count.
var panelRows = [3]int{1: 92, 2: 100}

// palette is 240 magma-like colours from dark to bright, then background, text and dim
// text. The image is drawn as palette indices: imaging's font copies a colour's three
// bytes, so a colour of [i, i, i] writes index i into every byte of the pixel.
var palette = func() []byte {
	anchors := [][3]float64{{0, 0, 4}, {40, 11, 84}, {101, 21, 110}, {159, 42, 99}, {212, 72, 66}, {245, 125, 21}, {250, 193, 39}, {252, 255, 164}}
	p := make([]byte, 243*3)
	for i := range 240 {
		t := float64(float64(i) / 239 * float64(len(anchors)-1))
		whole := int(math.Floor(t))
		a, b := anchors[whole], anchors[min(whole+1, len(anchors)-1)]
		f := t - math.Floor(t)
		for k := range 3 {
			p[i*3+k] = byte(rustle.Round(a[k] + float64((b[k]-a[k])*f)))
		}
	}
	copy(p[240*3:], []byte{18, 18, 18, 235, 235, 235, 110, 110, 110})
	return p
}()

func colour(i byte) [3]byte { return [3]byte{i, i, i} }

var (
	backgroundColour = colour(240)
	textColour       = colour(241)
	dimColour        = colour(242)
)

func imageHeight(panels int) int {
	return top + panels*panelRows[panels] + (panels-1)*gap + axis
}

// tickStep is the finest step that keeps at most ten ticks and room between their labels.
func tickStep(start, span float64) (float64, int) {
	for _, step := range []float64{0.01, 0.02, 0.05, 0.1, 0.2, 0.5, 1, 2, 5, 10, 15, 30, 60, 120, 300, 600} {
		decimals := 2
		if step >= 1 {
			decimals = 0
		} else if step >= 0.1 {
			decimals = 1
		}
		label := imaging.TextWidth(rustle.LabelTime(start+span, decimals), scale)
		if span/step <= 10 && float64((plot-1)*step)/span >= float64(label+labelGap) {
			return step, decimals
		}
	}
	return 1200, 0
}

// Spectrogram is a log-frequency spectrogram, 30 Hz to Nyquist, of the first two
// channels ("L" and "R", or "mono") over a labelled time axis and a -100..0 dBFS legend.
// It takes the onset detector's spectra (Analyze's sink) instead of running a second
// STFT, and keeps for each column the loudest of the frames centred inside it, so no
// click goes missing. Rows wider than a bin keep their loudest bin, so a narrow tone
// stays bright; low rows, narrower than a bin, shade between the two bins around them in
// dB, rather than in blocks a bin tall.
type Spectrogram struct {
	panels, n, rate, rows int
	norm                  float64
	// Narrow row r shades between bins below[r] and below[r] + 1 with weight[r] on the upper one.
	below  []int
	weight []float32
	// The w-th wide row, above the narrow ones, keeps the loudest bin in [lows[w], highs[w]).
	lows, highs []int
	// Per column: the power of bins [0, bins), then of each wide row.
	bins, cells int
	columns     [][]float32
	// Frame i is pooled into columns [firstColumn[i], endColumn[i]).
	firstColumn, endColumn []int
	// The cells of the frame being added.
	scratch []float32
}

var _ SpectrumSink = (*Spectrogram)(nil)

// NewSpectrogram is an empty picture of n samples per channel at rate.
func NewSpectrogram(channels, n, rate int) *Spectrogram {
	size, hop, count := Framing(rate, n)
	half := size / 2
	s := &Spectrogram{panels: max(1, min(2, channels)), n: n, rate: rate}
	rows := panelRows[s.panels]
	s.rows = rows
	// A full-scale sine has peak magnitude size / 4 under a Hann window.
	quarter := float64(size) / 4
	s.norm = 1 / (quarter * quarter)
	edge := func(r float64) float64 {
		return low * math.Pow(float64(rate)/2/low, r/float64(rows)) * float64(size) / float64(rate)
	}
	narrow := 0
	for narrow < rows && edge(float64(narrow+1))-edge(float64(narrow)) < 1 {
		narrow++
	}
	s.below = make([]int, narrow)
	s.weight = make([]float32, narrow)
	bins := 0
	for r := range narrow {
		centre := min(edge(float64(r)+0.5), float64(half-1))
		s.below[r] = int(math.Floor(centre))
		s.weight[r] = float32(centre - float64(s.below[r]))
		bins = s.below[r] + 2
	}
	s.bins = bins
	s.lows = make([]int, rows-narrow)
	s.highs = make([]int, rows-narrow)
	for w := range rows - narrow {
		s.lows[w] = min(int(math.Floor(edge(float64(narrow+w)))), half)
		s.highs[w] = min(max(s.lows[w]+1, int(math.Ceil(edge(float64(narrow+w+1))))), half+1)
	}
	s.cells = bins + rows - narrow
	s.columns = make([][]float32, s.panels)
	for p := range s.columns {
		s.columns[p] = make([]float32, plot*s.cells)
	}
	s.scratch = make([]float32, s.cells)
	// Column col spans samples [col, col + 1) * n / plot and pools the frames centred in
	// it; one too narrow to hold a centre takes the nearest frame.
	s.firstColumn = make([]int, count)
	s.endColumn = make([]int, count)
	for i := range s.firstColumn {
		s.firstColumn[i] = plot
	}
	per := float64(hop * plot)
	for col := range plot {
		lo := int(math.Ceil(float64(col*n) / per))
		hi := count
		if col != plot-1 {
			hi = min(count, int(math.Ceil(float64((col+1)*n)/per)))
		}
		if hi <= lo {
			lo = min(count-1, int(rustle.Round(float64((float64(col)+0.5)*float64(n))/per)))
			hi = lo + 1
		}
		for i := lo; i < hi; i++ {
			s.firstColumn[i] = min(s.firstColumn[i], col)
			s.endColumn[i] = col + 1
		}
	}
	return s
}

// Add pools one frame's spectrum into the columns it is centred in.
func (s *Spectrogram) Add(frame, channel int, power []float64) {
	if channel >= s.panels {
		return
	}
	cells := s.scratch
	for k := range s.bins {
		cells[k] = float32(power[k])
	}
	for w := range s.lows {
		loudest := 0.0
		for _, p := range power[s.lows[w]:s.highs[w]] {
			loudest = max(loudest, p)
		}
		cells[s.bins+w] = float32(loudest)
	}
	pool(s.columns[channel], cells, s.firstColumn[frame], s.endColumn[frame])
}

// pool raises columns [first, end) to the frame's cells where the frame is louder.
func pool(columns, cells []float32, first, end int) {
	count := len(cells)
	for at := first * count; at < end*count; at += count {
		column := columns[at : at+count]
		for j, c := range cells {
			column[j] = max(column[j], c)
		}
	}
}

// PNG draws the picture, with times starting at start.
func (s *Spectrogram) PNG(start float64) []byte {
	panels, rows, n, rate := s.panels, s.rows, s.n, s.rate
	names := []string{"mono"}
	if panels == 2 {
		names = []string{"L", "R"}
	}
	W, H := width, imageHeight(panels)
	high := float64(rate) / 2
	canvas := make([]byte, W*H*3)
	for i := range canvas {
		canvas[i] = 240
	}
	for p := range panels {
		y0 := top + p*(rows+gap)
		paint(canvas, ((y0+rows-1)*W+left)*3, s.columns[p], s.cells, s.bins, s.norm, s.below, s.weight)
		for _, frequency := range []float64{100, 1000, 10000} {
			if frequency <= low || frequency >= high {
				continue
			}
			row := y0 + rows - int(rustle.Round(float64(math.Log(frequency/low)/math.Log(high/low)*float64(rows))))
			imaging.FillRect(canvas, W, H, left-5, row, 5, 1, textColour)
			label := strconv.Itoa(int(frequency)) + "Hz"
			if frequency >= 1000 {
				label = strconv.Itoa(int(frequency/1000)) + "kHz"
			}
			imaging.DrawText(canvas, W, H, left-8-imaging.TextWidth(label, scale), row-line/2, label, scale, textColour)
		}
		name := names[p]
		imaging.FillRect(canvas, W, H, left+2, y0+2, imaging.TextWidth(name, scale)+8, line+4, backgroundColour)
		imaging.DrawText(canvas, W, H, left+6, y0+4, name, scale, textColour)
	}

	span := float64(n) / float64(rate)
	base := top + panels*rows + (panels-1)*gap
	step, decimals := tickStep(start, span)
	type label struct {
		x, width int
		text     string
	}
	var labels []label
	for k := math.Ceil(start/step - 1e-9); k*step <= start+span+1e-9; k++ {
		column := left + int(rustle.Round(float64((float64(k*step)-start)/span*(plot-1))))
		imaging.FillRect(canvas, W, H, column, base, 1, 5, textColour)
		t := rustle.LabelTime(k*step, decimals)
		w := imaging.TextWidth(t, scale)
		// Edge labels shift inwards rather than lose a digit. Labels are even widths at
		// scale 2, so halving one is exact.
		labels = append(labels, label{min(max(column-w/2, 2), W-2-w), w, t})
	}
	// A label that would touch its left neighbour is left out, and its tick still marks
	// the time; the last one, usually the clip's end, ousts that neighbour instead.
	var shown []label
	for i, l := range labels {
		if len(shown) > 0 {
			prev := shown[len(shown)-1]
			if l.x < prev.x+prev.width+labelGap {
				if i != len(labels)-1 {
					continue
				}
				shown = shown[:len(shown)-1]
			}
		}
		shown = append(shown, l)
	}
	for _, l := range shown {
		imaging.DrawText(canvas, W, H, l.x, base+7, l.text, scale, textColour)
	}
	legend := base + 7 + line + 6
	for i := range 120 {
		imaging.FillRect(canvas, W, H, left+i, legend+2, 1, line-4, colour(byte(rustle.Round(float64(float64(i)/119*239)))))
	}
	imaging.DrawText(canvas, W, H, left+128, legend, "-100 to 0 dBFS", scale, textColour)
	unit := "time m:ss"
	if start+span >= 3600 {
		unit = "time h:mm:ss"
	}
	imaging.DrawText(canvas, W, H, W-right-imaging.TextWidth(unit, scale), legend, unit, scale, dimColour)
	return imaging.EncodeIndexedPNG(firstBytes(canvas), W, H, palette, 6)
}

// paint writes one panel's palette indices, column by column, from the bottom row at
// canvas offset at upwards.
func paint(canvas []byte, at int, columns []float32, cells, bins int, norm float64, below []int, weight []float32) {
	levels := make([]float32, cells)
	for col := range plot {
		paintColumn(canvas, at+col*3, columns[col*cells:(col+1)*cells], levels, bins, norm, below, weight)
	}
}

// level maps -100..0 dBFS onto palette indices 0..239, unclamped.
func level(power float64) float64 {
	return (float64(10*math.Log10(max(power, 1e-12))) + 100) * 2.39
}

// silentLevel is the level of digital silence, common in rendered sound, without a
// logarithm per pixel.
var silentLevel = float32(level(0))

func paintColumn(canvas []byte, at int, column, levels []float32, bins int, norm float64, below []int, weight []float32) {
	narrow := len(below)
	for j, power := range column {
		levels[j] = silentLevel
		if power > 0 {
			levels[j] = float32(level(float64(float64(power) * norm)))
		}
	}
	for r, rows := 0, narrow+len(column)-bins; r < rows; r, at = r+1, at-width*3 {
		var value float64
		if r < narrow {
			k := below[r]
			lo, hi := float64(levels[k]), float64(levels[k+1])
			value = lo + float64((hi-lo)*float64(weight[r]))
		} else {
			value = float64(levels[bins+r-narrow])
		}
		switch {
		case value <= 0:
			canvas[at] = 0
		case value >= 239:
			canvas[at] = 239
		default:
			canvas[at] = byte(value)
		}
	}
}

// firstBytes is the palette indices of a canvas whose pixels repeat their index in all
// three bytes.
func firstBytes(canvas []byte) []byte {
	pixels := make([]byte, len(canvas)/3)
	for i := range pixels {
		pixels[i] = canvas[i*3]
	}
	return pixels
}
