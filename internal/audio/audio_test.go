package audio

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/vibeslop/rustle-mcp/internal/imaging"
	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

const testRate = 48000

// twoPi is 2 * Pi as JavaScript rounds it; signals multiply it at run time, as the
// TypeScript tests do, rather than folding 2 * Pi * f into one Go constant.
var twoPi = 2 * math.Pi

// beep is stereo: silence on the left, a 1 kHz tone at half scale on the right for [at, at + 0.5).
func beep(seconds, at float64, rate int) [][]float32 {
	right := make([]float32, int(seconds)*rate)
	for i := range right {
		t := float64(i) / float64(rate)
		if t >= at && t < at+0.5 {
			right[i] = float32(0.5 * math.Sin(twoPi*1000*t))
		}
	}
	return [][]float32{make([]float32, len(right)), right}
}

// spectrogram is the picture as listen draws it, from the analysis's own spectra.
func spectrogram(x [][]float32) []byte {
	picture := NewSpectrogram(len(x), len(x[0]), testRate)
	Analyze(x, testRate, 0, picture)
	return picture.PNG(0)
}

func TestFFTGivesPowerSpectraMirroredAtTheEdges(t *testing.T) {
	signal := make([]float32, 300)
	for i := range signal {
		signal[i] = float32(math.Sin(float64(i)*0.37) + 0.3*math.Cos(float64(i)*1.91))
	}
	// 64 and 128 points cover every pass: three stages, then one or two.
	for _, c := range [][3]int{{64, 10, 100}, {128, 10, 100}, {128, -40, 250}} {
		n, startA, startB := c[0], c[1], c[2]
		window := Hann(n)
		a := make([]float64, n/2+1)
		b := make([]float64, n/2+1)
		NewFFT(n).Powers(signal, startA, signal, startB, window, a, b)
		for _, frame := range []struct {
			start int
			power []float64
		}{{startA, a}, {startB, b}} {
			for k := 0; k <= n/2; k++ {
				re, im := 0.0, 0.0
				for t := range n {
					v := float64(signal[Reflect(frame.start+t, len(signal))]) * window[t]
					re += v * math.Cos(2*math.Pi*float64(k*t)/float64(n))
					im -= v * math.Sin(2*math.Pi*float64(k*t)/float64(n))
				}
				if math.Abs(re*re+im*im-frame.power[k]) >= 1e-9*max(1, frame.power[k]) {
					t.Errorf("%d points, bin %d from %d: %v, want %v", n, k, frame.start, frame.power[k], re*re+im*im)
				}
			}
		}
	}
	// numpy's "reflect": mirrored about the end samples, which are not repeated.
	var got []int
	for _, i := range []int{-3, -1, 0, 4, 5, 7} {
		got = append(got, Reflect(i, 5))
	}
	if !slices.Equal(got, []int{3, 1, 0, 4, 3, 1}) {
		t.Errorf("Reflect gave %v", got)
	}
}

func TestAnalysisReportsOnsetsBalanceAndSilence(t *testing.T) {
	m := Analyze(beep(6, 3, testRate), testRate, 10, nil)
	if len(m.Onsets) != 1 || math.Abs(m.Onsets[0]-13) > 0.03 {
		t.Errorf("onsets %v", m.Onsets)
	}
	if m.BalanceDB == nil || *m.BalanceDB >= -60 {
		t.Errorf("balance %v", m.BalanceDB)
	}
	if len(m.Silence) < 2 || m.Silence[0] != [2]float64{10, 13} || math.Abs(m.Silence[1][0]-13.5) > 0.02 {
		t.Errorf("silence %v", m.Silence)
	}
	if !slices.Equal(m.PeakDBFS, []float64{-120, -6}) {
		t.Errorf("peak %v", m.PeakDBFS)
	}

	// A tone already playing at the window start is not an onset.
	tone := make([]float32, 2*testRate)
	for i := range tone {
		tone[i] = float32(0.5 * math.Sin(twoPi*440*float64(i)/testRate))
	}
	steady := Analyze([][]float32{tone, tone}, testRate, 5, nil)
	if len(steady.Onsets) != 0 || len(steady.Silence) != 0 || steady.BalanceDB == nil || *steady.BalanceDB != 0 {
		t.Errorf("steady tone: %+v", steady)
	}

	// Digital silence floors at -120 dBFS; mono has no balance.
	mono := Analyze([][]float32{make([]float32, testRate)}, testRate, 0, nil)
	want := Metrics{PeakDBFS: []float64{-120}, RMSDBFS: []float64{-120}, Onsets: []float64{}, Silence: [][2]float64{{0, 1}}}
	if !reflect.DeepEqual(mono, want) {
		t.Errorf("mono silence: %+v", mono)
	}
}

func TestDigitalSilenceSkipsTheFFTWithoutChangingANumber(t *testing.T) {
	// A floor of 1e-30 makes every frame nonzero, so every one is transformed, yet it
	// stays far below anything reported.
	lifted := beep(6, 3, testRate)
	for _, x := range lifted {
		for i, v := range x {
			if v == 0 {
				x[i] = 1e-30
			}
		}
	}
	plain, transformed := Analyze(beep(6, 3, testRate), testRate, 10, nil), Analyze(lifted, testRate, 10, nil)
	if !reflect.DeepEqual(plain, transformed) {
		t.Errorf("%+v\n%+v", plain, transformed)
	}
}

func TestClicksInAQuietRoomAreOnsetsAtAnyCommonRate(t *testing.T) {
	for _, rate := range []int{48000, 44100, 22050} {
		// The TypeScript test's generator in JavaScript's doubles: the product is rounded
		// before the sum.
		seed := 1.0
		noise := func() float64 {
			seed = math.Mod(float64(seed*1103515245)+12345, 2147483648)
			return seed/2147483648 - 0.5
		}
		r := float64(rate)
		x := make([]float32, 3*rate)
		for i := range x {
			x[i] = float32(0.001 * noise())
		}
		clicks := []float64{0.5, 1, 1.7, 2.4}
		for _, t := range clicks {
			for i := 0; float64(i) < 0.005*r; i++ {
				at := int(rustle.Round(float64(t*r))) + i
				x[at] = float32(float64(x[at]) + float64(0.8*noise()*math.Exp(-float64(i)/(0.001*r))))
			}
		}
		onsets := Analyze([][]float32{x}, rate, 0, nil).Onsets
		if len(onsets) != len(clicks) {
			t.Errorf("%d Hz: %v", rate, onsets)
			continue
		}
		for i, o := range onsets {
			if math.Abs(o-clicks[i]) > 0.02 {
				t.Errorf("%d Hz: %v", rate, onsets)
			}
		}
	}
}

func TestSpectrogramIsWholePatches(t *testing.T) {
	stereo := spectrogram(beep(6, 3, testRate))
	width, height := imaging.PNGSize(stereo)
	if width%28 != 0 || height%28 != 0 || rustle.ImageTokens(width, height) > 162 {
		t.Errorf("stereo %d x %d", width, height)
	}
	monoWidth, monoHeight := imaging.PNGSize(spectrogram(beep(6, 3, testRate)[1:]))
	if monoHeight >= height || monoHeight%28 != 0 || rustle.ImageTokens(monoWidth, monoHeight) > 90 {
		t.Errorf("mono %d x %d", monoWidth, monoHeight)
	}
	// The tone is bright in the right panel only, over about a twelfth of the width.
	pixels := indices(t, stereo)
	var bright [2][]int
	for y := range height - 50 {
		for x := range width {
			if p := pixels[y*width+x]; p >= 200 && p < 240 {
				half := 1
				if y < height/2 {
					half = 0
				}
				bright[half] = append(bright[half], x)
			}
		}
	}
	if len(bright[0]) != 0 || len(bright[1]) == 0 {
		t.Fatalf("bright pixels: %d left, %d right", len(bright[0]), len(bright[1]))
	}
	first, last := slices.Min(bright[1]), slices.Max(bright[1])
	if float64(last-first) >= float64(width)/10 || float64(first) <= float64(width)*0.5 || float64(last) >= float64(width)*0.65 {
		t.Errorf("tone spans %d-%d", first, last)
	}
}

// indices are the palette indices of an unfiltered 8-bit indexed PNG.
func indices(t *testing.T, png []byte) []byte {
	t.Helper()
	if png[25] != 3 {
		t.Fatal("not indexed colour")
	}
	width, height := imaging.PNGSize(png)
	var data []byte
	for at := 8; at < len(png); {
		n := int(binary.BigEndian.Uint32(png[at:]))
		if string(png[at+4:at+8]) == "IDAT" {
			data = append(data, png[at+8:at+8+n]...)
		}
		at += 12 + n
	}
	r, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]byte, 0, width*height)
	for y := range height {
		out = append(out, raw[y*(width+1)+1:(y+1)*(width+1)]...)
	}
	return out
}

// Analyses run concurrently in the server; each must get its own scratch buffers.
func TestAnalysesRunConcurrently(t *testing.T) {
	signals := [][][]float32{beep(6, 3, testRate), noise(2, testRate), beep(6, 3, testRate)[1:]}
	want := make([][]byte, len(signals))
	for i, x := range signals {
		want[i] = spectrogram(x)
	}
	var wg sync.WaitGroup
	for range 4 {
		for i, x := range signals {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if !bytes.Equal(spectrogram(x), want[i]) {
					t.Errorf("signal %d differs when analysed concurrently", i)
				}
			}()
		}
	}
	wg.Wait()
}

// noise is independent stereo white noise; 20 s of it is the load of a long listen call.
func noise(seconds, rate int) [][]float32 {
	seed := uint32(1)
	x := [][]float32{make([]float32, seconds*rate), make([]float32, seconds*rate)}
	for _, channel := range x {
		for i := range channel {
			seed = seed*1664525 + 1013904223
			channel[i] = float32(seed)/(1<<32)*2 - 1
		}
	}
	return x
}

func BenchmarkAnalyzeWithSpectrogram(b *testing.B) {
	x := noise(20, 48000)
	for b.Loop() {
		picture := NewSpectrogram(len(x), len(x[0]), 48000)
		Analyze(x, 48000, 0, picture)
		picture.PNG(0)
	}
}

func ExampleFraming() {
	size, hop, count := Framing(22050, 22050)
	fmt.Println(size, hop, count)
	// Output: 512 220 101
}
