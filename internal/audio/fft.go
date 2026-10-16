// Package audio measures sound: loudness, balance, onsets and silences, and draws a
// spectrogram from the same spectra.
//
// Every product that feeds an addition is written float64(a*b): Go may fuse a multiply
// and an add into one FMA instruction (arm64 does), which skips the product's rounding,
// and the reported numbers would then differ by machine. Samples are float32, as decoded;
// the arithmetic is float64, and the spectrogram stores float32 again.
package audio

import (
	"fmt"
	"math"
	"math/bits"
	"sync"
)

// sqrt1_2 is 1/√2 rounded to float64.
const sqrt1_2 = 0.7071067811865476

// fftTables are one size's bit reversal and twiddles, read-only once built.
type fftTables struct {
	rev []uint32
	// Twiddles stage by stage: entries [h, 2h) hold exp(-i*pi*k/h) for the stage of half-size h.
	cos, sin []float64
}

var tablesBySize sync.Map // int -> *fftTables

func tablesFor(size int) *fftTables {
	if t, ok := tablesBySize.Load(size); ok {
		return t.(*fftTables)
	}
	shift := bits.Len(uint(size)) - 2
	t := &fftTables{rev: make([]uint32, size), cos: make([]float64, size), sin: make([]float64, size)}
	for i := range size {
		t.rev[i] = t.rev[i>>1]>>1 | uint32(i&1)<<shift
	}
	for h := 1; h < size; h <<= 1 {
		for k := range h {
			t.cos[h+k] = math.Cos(math.Pi * float64(k) / float64(h))
			t.sin[h+k] = -math.Sin(math.Pi * float64(k) / float64(h))
		}
	}
	actual, _ := tablesBySize.LoadOrStore(size, t)
	return actual.(*fftTables)
}

// FFT is an iterative radix-2 FFT for power spectra of real frames. Each transform packs
// two real frames into one complex signal (a + ib) and separates them afterwards, so a
// real frame costs half a complex FFT, the same as a dedicated real FFT with simpler code.
//
// The stages run two or three to a pass (radix 2^2, 2^3) over values held in locals: a
// 2048-point transform makes 4 passes over memory instead of 11.
//
// Its scratch buffers make one FFT single-threaded; the tables behind it are shared.
type FFT struct {
	size int
	*fftTables
	re, im              []float64
	zeros, edgeA, edgeB []float32
}

// NewFFT is a transform of size points, a power of two from 4.
func NewFFT(size int) *FFT {
	if size < 4 || size&(size-1) != 0 {
		panic(fmt.Sprintf("FFT size must be a power of two, got %d", size))
	}
	return &FFT{
		size:      size,
		fftTables: tablesFor(size),
		re:        make([]float64, size),
		im:        make([]float64, size),
		zeros:     make([]float32, size),
		edgeA:     make([]float32, size),
		edgeB:     make([]float32, size),
	}
}

// Powers writes the power spectra |A[k]|^2 and |B[k]|^2, k = 0..size/2, of two windowed
// real frames: a[aStart..aStart+size) and b[bStart..). A frame reaching past the signal
// reads it mirrored at the edges; a nil b leaves powerB holding zeros.
func (f *FFT) Powers(a []float32, aStart int, b []float32, bStart int, window []float64, powerA, powerB []float64) {
	size, rev, re, im := f.size, f.rev, f.re, f.im
	sa, oa, sb, ob := a, aStart, f.zeros, 0
	if aStart < 0 || aStart+size > len(a) {
		sa, oa = mirror(f.edgeA, a, aStart), 0
	}
	if b != nil {
		if bStart < 0 || bStart+size > len(b) {
			sb = mirror(f.edgeB, b, bStart)
		} else {
			sb, ob = b, bStart
		}
	}
	sa, sb, window = sa[oa:oa+size], sb[ob:ob+size], window[:size]
	re, im = re[:size], im[:size]
	// The bit-reversed load is fused with the first two stages, which need no multiplications:
	// positions i..i+3 (i a multiple of 4) hold samples j, j + size/2, j + size/4, j + 3size/4.
	q1, q2 := size>>2, size>>1
	q3 := q1 * 3
	for i := 0; i+3 < size; i += 4 {
		j := int(rev[i])
		w0, w1, w2, w3 := window[j], window[j+q2], window[j+q1], window[j+q3]
		r0, r1 := float64(float64(sa[j])*w0), float64(float64(sa[j+q2])*w1)
		r2, r3 := float64(float64(sa[j+q1])*w2), float64(float64(sa[j+q3])*w3)
		i0, i1 := float64(float64(sb[j])*w0), float64(float64(sb[j+q2])*w1)
		i2, i3 := float64(float64(sb[j+q1])*w2), float64(float64(sb[j+q3])*w3)
		ar, ai, br, bi := r0+r1, i0+i1, r0-r1, i0-i1
		cr, ci, dr, di := r2+r3, i2+i3, r2-r3, i2-i3
		re[i], im[i] = ar+cr, ai+ci
		re[i+2], im[i+2] = ar-cr, ai-ci
		// (dr + i*di) * -i = di - i*dr
		re[i+1], im[i+1] = br+di, bi-dr
		re[i+3], im[i+3] = br-di, bi+dr
	}
	f.transform()
	half := size >> 1
	powerA, powerB = powerA[:half+1], powerB[:half+1]
	for k := 0; k <= half; k++ {
		j := (size - k) & (size - 1)
		sr, di := re[k]+re[j], im[k]-im[j]
		si, dr := im[k]+im[j], re[k]-re[j]
		powerA[k] = (float64(sr*sr) + float64(di*di)) * 0.25
		powerB[k] = (float64(si*si) + float64(dr*dr)) * 0.25
	}
}

// transform runs the remaining decimation-in-time stages, from half-size 4 up: three per
// pass, then two or one.
func (f *FFT) transform() {
	size, re, im, cos, sin := f.size, f.re, f.im, f.cos, f.sin
	h := 4
	for ; h*8 <= size; h <<= 3 {
		step := h << 3
		for k := range h {
			w1r, w1i := cos[h+k], sin[h+k]
			w2r, w2i := cos[2*h+k], sin[2*h+k]
			w3r, w3i := cos[4*h+k], sin[4*h+k]
			for a := k; a < size; a += step {
				i1 := a + h
				i2, i3, i4, i5, i6, i7 := i1+h, i1+2*h, i1+3*h, i1+4*h, i1+5*h, i1+6*h
				// Stage h: the odd inputs take w1.
				xr, xi := re[i1], im[i1]
				tr, ti := float64(w1r*xr)-float64(w1i*xi), float64(w1r*xi)+float64(w1i*xr)
				y0r, y0i, y1r, y1i := re[a]+tr, im[a]+ti, re[a]-tr, im[a]-ti
				xr, xi = re[i3], im[i3]
				tr, ti = float64(w1r*xr)-float64(w1i*xi), float64(w1r*xi)+float64(w1i*xr)
				y2r, y2i, y3r, y3i := re[i2]+tr, im[i2]+ti, re[i2]-tr, im[i2]-ti
				xr, xi = re[i5], im[i5]
				tr, ti = float64(w1r*xr)-float64(w1i*xi), float64(w1r*xi)+float64(w1i*xr)
				y4r, y4i, y5r, y5i := re[i4]+tr, im[i4]+ti, re[i4]-tr, im[i4]-ti
				xr, xi = re[i7], im[i7]
				tr, ti = float64(w1r*xr)-float64(w1i*xi), float64(w1r*xi)+float64(w1i*xr)
				y6r, y6i, y7r, y7i := re[i6]+tr, im[i6]+ti, re[i6]-tr, im[i6]-ti
				// Stage 2h: w2, and w2 * -i for the second half of each pair.
				tr, ti = float64(w2r*y2r)-float64(w2i*y2i), float64(w2r*y2i)+float64(w2i*y2r)
				z0r, z0i, z2r, z2i := y0r+tr, y0i+ti, y0r-tr, y0i-ti
				tr, ti = float64(w2r*y3i)+float64(w2i*y3r), float64(w2i*y3i)-float64(w2r*y3r)
				z1r, z1i, z3r, z3i := y1r+tr, y1i+ti, y1r-tr, y1i-ti
				tr, ti = float64(w2r*y6r)-float64(w2i*y6i), float64(w2r*y6i)+float64(w2i*y6r)
				z4r, z4i, z6r, z6i := y4r+tr, y4i+ti, y4r-tr, y4i-ti
				tr, ti = float64(w2r*y7i)+float64(w2i*y7r), float64(w2i*y7i)-float64(w2r*y7r)
				z5r, z5i, z7r, z7i := y5r+tr, y5i+ti, y5r-tr, y5i-ti
				// Stage 4h: w3 times exp(-i*pi*m/4) for output pair m: 1, (1-i)/sqrt2, -i, (-1-i)/sqrt2.
				tr, ti = float64(w3r*z4r)-float64(w3i*z4i), float64(w3r*z4i)+float64(w3i*z4r)
				re[a], im[a], re[i4], im[i4] = z0r+tr, z0i+ti, z0r-tr, z0i-ti
				ur, ui := float64(w3r*z5r)-float64(w3i*z5i), float64(w3r*z5i)+float64(w3i*z5r)
				tr, ti = float64((ur+ui)*sqrt1_2), float64((ui-ur)*sqrt1_2)
				re[i1], im[i1], re[i5], im[i5] = z1r+tr, z1i+ti, z1r-tr, z1i-ti
				ur, ui = float64(w3r*z6r)-float64(w3i*z6i), float64(w3r*z6i)+float64(w3i*z6r)
				re[i2], im[i2], re[i6], im[i6] = z2r+ui, z2i-ur, z2r-ui, z2i+ur
				ur, ui = float64(w3r*z7r)-float64(w3i*z7i), float64(w3r*z7i)+float64(w3i*z7r)
				tr, ti = float64((ui-ur)*sqrt1_2), float64(-(ur+ui)*sqrt1_2)
				re[i3], im[i3], re[i7], im[i7] = z3r+tr, z3i+ti, z3r-tr, z3i-ti
			}
		}
	}
	for ; h*4 <= size; h <<= 2 {
		step := h << 2
		for k := range h {
			w1r, w1i := cos[h+k], sin[h+k]
			w2r, w2i := cos[2*h+k], sin[2*h+k]
			for a := k; a < size; a += step {
				b, c, d := a+h, a+2*h, a+3*h
				ar, ai, cr, ci := re[a], im[a], re[c], im[c]
				br, bi := float64(w1r*re[b])-float64(w1i*im[b]), float64(w1r*im[b])+float64(w1i*re[b])
				dr, di := float64(w1r*re[d])-float64(w1i*im[d]), float64(w1r*im[d])+float64(w1i*re[d])
				p0r, p0i, p1r, p1i := ar+br, ai+bi, ar-br, ai-bi
				q0r, q0i, q1r, q1i := cr+dr, ci+di, cr-dr, ci-di
				t0r, t0i := float64(w2r*q0r)-float64(w2i*q0i), float64(w2r*q0i)+float64(w2i*q0r)
				// The odd outputs take w2 * -i.
				t1r, t1i := float64(w2r*q1i)+float64(w2i*q1r), float64(w2i*q1i)-float64(w2r*q1r)
				re[a], im[a] = p0r+t0r, p0i+t0i
				re[c], im[c] = p0r-t0r, p0i-t0i
				re[b], im[b] = p1r+t1r, p1i+t1i
				re[d], im[d] = p1r-t1r, p1i-t1i
			}
		}
	}
	for ; h < size; h <<= 1 {
		step := h << 1
		for k := range h {
			wr, wi := cos[h+k], sin[h+k]
			for i := k; i < size; i += step {
				j := i + h
				tr := float64(wr*re[j]) - float64(wi*im[j])
				ti := float64(wr*im[j]) + float64(wi*re[j])
				re[j], im[j] = re[i]-tr, im[i]-ti
				re[i] += tr
				im[i] += ti
			}
		}
	}
}

func mirror(target, signal []float32, start int) []float32 {
	for j := range target {
		target[j] = signal[Reflect(start+j, len(signal))]
	}
	return target
}

// Reflect is numpy's "reflect" padding: mirror about the end samples without repeating
// them; "edge" for one sample.
func Reflect(index, n int) int {
	if n <= 1 {
		return 0
	}
	period := 2 * (n - 1)
	j := (index%period + period) % period
	if j < n {
		return j
	}
	return period - j
}

var windows sync.Map // int -> []float64

// Hann is the symmetric Hann window, as numpy.hanning; shared per size, so callers must
// not write to it.
func Hann(size int) []float64 {
	if w, ok := windows.Load(size); ok {
		return w.([]float64)
	}
	w := make([]float64, size)
	for i := range size {
		w[i] = 0.5 - float64(0.5*math.Cos(2*math.Pi*float64(i)/float64(size-1)))
	}
	actual, _ := windows.LoadOrStore(size, w)
	return actual.([]float64)
}
