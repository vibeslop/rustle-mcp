package audio

import (
	"math"
	"slices"

	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

const (
	// floorDB is where reported levels floor instead of -Inf for digital silence.
	floorDB        = -120
	silenceDB      = -50
	silenceSeconds = 0.3
	maxOnsets      = 20
	maxSilences    = 20
	// lag is the frames a rise is measured against: 30 ms at the 10 ms hop.
	lag = 3
)

// Metrics are the loudness, balance, onsets and silences of a span of sound.
type Metrics struct {
	PeakDBFS []float64 `json:"peak_dbfs"`
	RMSDBFS  []float64 `json:"rms_dbfs"`
	// BalanceDB is left minus right RMS, for two or more channels.
	BalanceDB *float64     `json:"balance_db,omitempty"`
	Onsets    []float64    `json:"onsets"`
	Silence   [][2]float64 `json:"silence"`
}

func db(power, floor float64) float64 {
	return max(10*math.Log10(max(power, 1e-30)), floor)
}

func round(value float64, digits int) float64 {
	factor := math.Pow10(digits)
	return rustle.Round(float64(value*factor)) / factor
}

// roundEven is banker's rounding, halves to the even neighbour: 22050 Hz gets a 220-sample hop.
func roundEven(value float64) float64 {
	rounded := rustle.Round(value)
	if rounded-value == 0.5 && math.Mod(rounded, 2) != 0 {
		return rounded - 1
	}
	return rounded
}

// SpectrumSink takes the power spectra the onset detector computes, so a spectrogram
// needs no second STFT. Frames of digital silence are left out: their spectrum is all
// zeros. power holds bins 0..size/2 and is reused once Add returns.
type SpectrumSink interface {
	Add(frame, channel int, power []float64)
}

// Framing is the onset detector's frames: about 21 ms long every 10 ms, frame i centred
// on sample i * hop.
func Framing(rate, n int) (size, hop, count int) {
	hop = max(1, int(roundEven(float64(rate)/100)))
	size = 1 << max(8, int(roundEven(math.Log2(float64(rate)*0.0213))))
	return size, hop, n/hop + 1
}

// Analyze measures channels holding -1..1 samples; times start at start. A sink, if not
// nil, receives the spectrum of each frame of each channel.
func Analyze(channels [][]float32, rate int, start float64, sink SpectrumSink) Metrics {
	n := 0
	if len(channels) > 0 {
		n = len(channels[0])
	}
	block := max(1, int(roundEven(float64(rate)/100)))
	// Each channel's peak and mean power, and the loudest channel's power in every 10 ms
	// block, which silence is judged on.
	loudest := make([]float64, n/block)
	m := Metrics{PeakDBFS: []float64{}, RMSDBFS: []float64{}}
	// Blocks of digital silence per channel, whose frames the onset detector need not transform.
	zero := make([][]bool, len(channels))
	for c, x := range channels {
		zero[c] = make([]bool, (n+block-1)/block)
		peak, total := scan(x[:n], block, loudest, zero[c])
		m.PeakDBFS = append(m.PeakDBFS, round(db(float64(peak*peak), floorDB), 1))
		mean := 0.0
		if n > 0 {
			mean = total / float64(n)
		}
		m.RMSDBFS = append(m.RMSDBFS, round(db(mean, floorDB), 1))
	}
	if len(channels) >= 2 {
		balance := round(m.RMSDBFS[0]-m.RMSDBFS[1], 1)
		m.BalanceDB = &balance
	}
	m.Onsets = onsets(channels, zero, n, rate, start, sink)
	m.Silence = silence(loudest, block, n, rate, start)
	return m
}

// scan returns one channel's peak and total power; it raises loudest[i] to block i's
// mean power and marks zero[i] when block i holds only zeros.
func scan(x []float32, block int, loudest []float64, zero []bool) (peak, total float64) {
	n := len(x)
	for i, j := 0, 0; j < n; i++ {
		sum := 0.0
		for _, s := range x[j:min(n, j+block)] {
			v := float64(s)
			sum += float64(v * v)
			if magnitude := math.Abs(v); magnitude > peak {
				peak = magnitude
			}
		}
		j += block
		total += sum
		zero[i] = sum == 0
		if i < len(loudest) {
			loudest[i] = max(loudest[i], sum/float64(block))
		}
	}
	return peak, total
}

// bandEdges are bin edges that sum FFT power into log-spaced bands of at least minimum
// bins. Single-bin bands flicker by several dB on steady noise, which reads as onsets.
func bandEdges(bins, bands, minimum int) []int {
	edges := []int{1}
	for i := 1; i <= bands; i++ {
		edge := int(roundEven(math.Pow(float64(bins), float64(i)/float64(bands))))
		if edge-edges[len(edges)-1] >= minimum {
			edges = append(edges, edge)
		}
	}
	edges[len(edges)-1] = bins
	return edges
}

// onsets are the times where energy rises sharply: log spectral flux against the last 30 ms.
func onsets(x [][]float32, zero [][]bool, n, rate int, start float64, sink SpectrumSink) []float64 {
	times := []float64{}
	if n == 0 {
		return times
	}
	size, hop, count := Framing(rate, n)
	edges := bandEdges(size/2+1, 48, 3)
	bands := len(edges) - 1
	power := bandPowers(x, zero, n, size, hop, count, edges, sink)
	levels := make([]float64, count*bands)
	loudness := make([]float64, count)
	toLevels(power, count, bands, levels, loudness)
	flux := make([]float64, count)
	before := make([]float64, count)
	for i := range before {
		before[i] = floorDB
	}
	strongest := rises(levels, loudness, count, bands, flux, before)
	if strongest == 0 {
		return times
	}
	floor := float64(0.1 * strongest)
	// Frames reaching past the clip edges see padding, not sound.
	first := int(math.Ceil(float64(size) / 2 / float64(hop)))
	last := int(math.Floor(float64(n-size/2) / float64(hop)))
	type candidate struct {
		flux  float64
		frame int
	}
	var candidates []candidate
	at := func(i int) float64 {
		if i < count {
			return loudness[i]
		}
		return math.Inf(-1)
	}
	for i := max(lag, first); i < min(count, last+1); i++ {
		after := max(loudness[i], at(i+1), at(i+2))
		// Real onsets get louder overall; the click of a sound cut short does not.
		if flux[i] < 3 || after < silenceDB || after < before[i]+1 {
			continue
		}
		local := 0.0
		for j := max(0, i-3); j < min(count, i+4); j++ {
			local = max(local, flux[j])
		}
		if flux[i] < local {
			continue
		}
		sum := 0.0
		lo, hi := max(0, i-10), min(count, i+11)
		for j := lo; j < hi; j++ {
			sum += flux[j]
		}
		if flux[i] < sum/float64(hi-lo)+floor {
			continue
		}
		candidates = append(candidates, candidate{flux[i], i})
	}
	// Strongest first, so a burst of weak rises cannot crowd out a clear onset.
	slices.SortStableFunc(candidates, func(p, q candidate) int {
		if p.flux != q.flux {
			if q.flux < p.flux {
				return -1
			}
			return 1
		}
		return q.frame - p.frame
	})
	var chosen []int
	for _, c := range candidates {
		if !slices.ContainsFunc(chosen, func(j int) bool { return abs(c.frame-j) < 10 }) {
			chosen = append(chosen, c.frame)
		}
		if len(chosen) >= maxOnsets {
			break
		}
	}
	slices.Sort(chosen)
	// Log flux peaks as the sound enters the leading half of the window, a little early.
	offset := (float64(float64(size)/2) - float64(float64(hop)/2)) / float64(rate)
	for _, i := range chosen {
		times = append(times, round(start+float64(i*hop)/float64(rate)+offset, 2))
	}
	return times
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// bandPowers is the power per (frame, band), channels summed, for frames centred on
// i * hop. Any two (frame, channel) spectra share a transform, and frames of digital
// silence, common in rendered sound and in one-sided stereo, are skipped: their
// spectrum is exactly zero.
func bandPowers(x [][]float32, zero [][]bool, n, size, hop, count int, edges []int, sink SpectrumSink) []float64 {
	bands := len(edges) - 1
	power := make([]float64, count*bands)
	// A full-scale sine has peak magnitude size / 4 under a Hann window.
	quarter := float64(size) / 4
	norm := 1 / (quarter * quarter)
	transform := NewFFT(size)
	window := Hann(size)
	a := make([]float64, size/2+1)
	b := make([]float64, size/2+1)
	c := len(x)
	half := size / 2
	// Spectrum t is frame t / c of channel t % c; one past the last flushes a spectrum
	// still waiting for a partner.
	waiting := -1
	for t, total := 0, count*c; t <= total; t++ {
		last := t == total
		if !last && silent(zero[t%c], t/c*hop-half, size, hop, n) {
			continue
		}
		if waiting < 0 {
			if !last {
				waiting = t
			}
			continue
		}
		fa, ca := waiting/c, waiting%c
		fb, cb := t/c, t%c
		var other []float32
		if !last {
			other = x[cb]
		}
		transform.Powers(x[ca], fa*hop-half, other, fb*hop-half, window, a, b)
		addBands(power[fa*bands:], a, edges, norm)
		if sink != nil {
			sink.Add(fa, ca, a)
		}
		if !last {
			addBands(power[fb*bands:], b, edges, norm)
			if sink != nil {
				sink.Add(fb, cb, b)
			}
		}
		waiting = -1
	}
	return power
}

// silent reports whether the frame of samples [from, from + size) reads only blocks of zeros.
func silent(zero []bool, from, size, block, n int) bool {
	if size >= n {
		return false
	}
	lo, hi := from, from+size-1
	// A frame past an edge reads the signal mirrored there, as far in as it reaches out.
	if lo < 0 {
		hi = max(hi, -lo)
		lo = 0
	}
	if hi > n-1 {
		lo = min(lo, 2*(n-1)-hi)
		hi = n - 1
	}
	for _, z := range zero[lo/block : hi/block+1] {
		if !z {
			return false
		}
	}
	return true
}

func addBands(row, spectrum []float64, edges []int, norm float64) {
	for band := range len(edges) - 1 {
		sum := 0.0
		for _, p := range spectrum[edges[band]:edges[band+1]] {
			sum += p
		}
		row[band] += float64(sum * norm)
	}
}

// toLevels writes each band's level floored at -90 dB, and each frame's loudness over all bands.
func toLevels(power []float64, count, bands int, levels, loudness []float64) {
	for i := range count {
		sum := 0.0
		for band := range bands {
			p := power[i*bands+band]
			sum += p
			// Skipped silent frames are all zeros, which floor without a logarithm.
			levels[i*bands+band] = -90
			if p > 0 {
				levels[i*bands+band] = db(p, -90)
			}
		}
		loudness[i] = floorDB
		if sum > 0 {
			loudness[i] = db(sum, floorDB)
		}
	}
}

// rises writes each frame's mean rise in band level over the loudest of the lag frames
// before (flux), and their loudest loudness (before); it returns the strongest flux.
func rises(levels, loudness []float64, count, bands int, flux, before []float64) float64 {
	strongest := 0.0
	for i := lag; i < count; i++ {
		// Comparing with the loudest of the previous frames ignores flicker in steady sound.
		rise := 0.0
		for band := range bands {
			at := i*bands + band
			reference := max(levels[at-bands], levels[at-2*bands], levels[at-3*bands])
			if levels[at] > reference {
				rise += levels[at] - reference
			}
		}
		flux[i] = rise / float64(bands)
		before[i] = max(loudness[i-1], loudness[i-2], loudness[i-3])
		if flux[i] > strongest {
			strongest = flux[i]
		}
	}
	return strongest
}

// silence is the spans where every channel stays under silenceDB for at least
// silenceSeconds, from 10 ms block powers.
func silence(loudest []float64, block, n, rate int, start float64) [][2]float64 {
	blocks := len(loudest)
	spans := [][2]float64{}
	begin := -1
	for i := 0; i <= blocks; i++ {
		quiet := i < blocks && db(loudest[i], floorDB) < silenceDB
		if quiet && begin < 0 {
			begin = i
		} else if !quiet && begin >= 0 {
			// A span running to the end ends at the last sample, not the last whole block.
			end := float64(i)
			if i == blocks {
				end = float64(n) / float64(block)
			}
			if (end-float64(begin))*float64(block)/float64(rate) >= silenceSeconds {
				spans = append(spans, [2]float64{
					round(start+float64(begin*block)/float64(rate), 2),
					round(start+float64(end*float64(block))/float64(rate), 2),
				})
			}
			begin = -1
		}
	}
	return spans[:min(len(spans), maxSilences)]
}
