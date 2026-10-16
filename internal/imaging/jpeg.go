package imaging

import "math/bits"

/*
Baseline JPEG encoder for RGB24 with 4:2:0 chroma. Encoding a contact sheet in-process
takes a fraction of the time starting ffmpeg would (ffmpeg's dozens of shared libraries
cost 35-40 ms to load), and leaves the decoder slots to decoding. Huffman and
quantization tables are the standard ones of ITU-T T.81 Annex K; the DCT is the AAN
float DCT.

Every product that feeds an addition is written float64(a*b): Go may fuse a multiply
and an add into one FMA instruction (arm64 does), which skips the product's rounding, so
a quantized coefficient could land on the other side of a half and the same picture
would encode differently on different machines.
*/

// Natural index -> zigzag position.
var zigzag = [64]uint8{
	0, 1, 5, 6, 14, 15, 27, 28, 2, 4, 7, 13, 16, 26, 29, 42, 3, 8, 12, 17, 25, 30, 41, 43, 9, 11, 18, 24, 31, 40, 44, 53,
	10, 19, 23, 32, 39, 45, 52, 54, 20, 22, 33, 38, 46, 51, 55, 60, 21, 34, 37, 47, 50, 56, 59, 61, 35, 36, 48, 49, 57, 58, 62, 63,
}

var yQuant = [64]int{
	16, 11, 10, 16, 24, 40, 51, 61, 12, 12, 14, 19, 26, 58, 60, 55, 14, 13, 16, 24, 40, 57, 69, 56, 14, 17, 22, 29, 51, 87, 80, 62,
	18, 22, 37, 56, 68, 109, 103, 77, 24, 35, 55, 64, 81, 104, 113, 92, 49, 64, 78, 87, 103, 121, 120, 101, 72, 92, 95, 98, 112, 100, 103, 99,
}

var cQuant = [64]int{
	17, 18, 24, 47, 99, 99, 99, 99, 18, 21, 26, 66, 99, 99, 99, 99, 24, 26, 56, 99, 99, 99, 99, 99, 47, 66, 99, 99, 99, 99, 99, 99,
	99, 99, 99, 99, 99, 99, 99, 99, 99, 99, 99, 99, 99, 99, 99, 99, 99, 99, 99, 99, 99, 99, 99, 99, 99, 99, 99, 99, 99, 99, 99, 99,
}

var (
	dcYCounts = []byte{0, 1, 5, 1, 1, 1, 1, 1, 1, 0, 0, 0, 0, 0, 0, 0}
	dcCCounts = []byte{0, 3, 1, 1, 1, 1, 1, 1, 1, 1, 1, 0, 0, 0, 0, 0}
	dcValues  = []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}
	acYCounts = []byte{0, 2, 1, 3, 3, 2, 4, 3, 5, 5, 4, 4, 0, 0, 1, 0x7d}
	acYValues = []byte{
		0x01, 0x02, 0x03, 0x00, 0x04, 0x11, 0x05, 0x12, 0x21, 0x31, 0x41, 0x06, 0x13, 0x51, 0x61, 0x07, 0x22, 0x71, 0x14, 0x32, 0x81, 0x91, 0xa1, 0x08,
		0x23, 0x42, 0xb1, 0xc1, 0x15, 0x52, 0xd1, 0xf0, 0x24, 0x33, 0x62, 0x72, 0x82, 0x09, 0x0a, 0x16, 0x17, 0x18, 0x19, 0x1a, 0x25, 0x26, 0x27, 0x28,
		0x29, 0x2a, 0x34, 0x35, 0x36, 0x37, 0x38, 0x39, 0x3a, 0x43, 0x44, 0x45, 0x46, 0x47, 0x48, 0x49, 0x4a, 0x53, 0x54, 0x55, 0x56, 0x57, 0x58, 0x59,
		0x5a, 0x63, 0x64, 0x65, 0x66, 0x67, 0x68, 0x69, 0x6a, 0x73, 0x74, 0x75, 0x76, 0x77, 0x78, 0x79, 0x7a, 0x83, 0x84, 0x85, 0x86, 0x87, 0x88, 0x89,
		0x8a, 0x92, 0x93, 0x94, 0x95, 0x96, 0x97, 0x98, 0x99, 0x9a, 0xa2, 0xa3, 0xa4, 0xa5, 0xa6, 0xa7, 0xa8, 0xa9, 0xaa, 0xb2, 0xb3, 0xb4, 0xb5, 0xb6,
		0xb7, 0xb8, 0xb9, 0xba, 0xc2, 0xc3, 0xc4, 0xc5, 0xc6, 0xc7, 0xc8, 0xc9, 0xca, 0xd2, 0xd3, 0xd4, 0xd5, 0xd6, 0xd7, 0xd8, 0xd9, 0xda, 0xe1, 0xe2,
		0xe3, 0xe4, 0xe5, 0xe6, 0xe7, 0xe8, 0xe9, 0xea, 0xf1, 0xf2, 0xf3, 0xf4, 0xf5, 0xf6, 0xf7, 0xf8, 0xf9, 0xfa,
	}
	acCCounts = []byte{0, 2, 1, 2, 4, 4, 3, 4, 7, 5, 4, 4, 0, 1, 2, 0x77}
	acCValues = []byte{
		0x00, 0x01, 0x02, 0x03, 0x11, 0x04, 0x05, 0x21, 0x31, 0x06, 0x12, 0x41, 0x51, 0x07, 0x61, 0x71, 0x13, 0x22, 0x32, 0x81, 0x08, 0x14, 0x42, 0x91,
		0xa1, 0xb1, 0xc1, 0x09, 0x23, 0x33, 0x52, 0xf0, 0x15, 0x62, 0x72, 0xd1, 0x0a, 0x16, 0x24, 0x34, 0xe1, 0x25, 0xf1, 0x17, 0x18, 0x19, 0x1a, 0x26,
		0x27, 0x28, 0x29, 0x2a, 0x35, 0x36, 0x37, 0x38, 0x39, 0x3a, 0x43, 0x44, 0x45, 0x46, 0x47, 0x48, 0x49, 0x4a, 0x53, 0x54, 0x55, 0x56, 0x57, 0x58,
		0x59, 0x5a, 0x63, 0x64, 0x65, 0x66, 0x67, 0x68, 0x69, 0x6a, 0x73, 0x74, 0x75, 0x76, 0x77, 0x78, 0x79, 0x7a, 0x82, 0x83, 0x84, 0x85, 0x86, 0x87,
		0x88, 0x89, 0x8a, 0x92, 0x93, 0x94, 0x95, 0x96, 0x97, 0x98, 0x99, 0x9a, 0xa2, 0xa3, 0xa4, 0xa5, 0xa6, 0xa7, 0xa8, 0xa9, 0xaa, 0xb2, 0xb3, 0xb4,
		0xb5, 0xb6, 0xb7, 0xb8, 0xb9, 0xba, 0xc2, 0xc3, 0xc4, 0xc5, 0xc6, 0xc7, 0xc8, 0xc9, 0xca, 0xd2, 0xd3, 0xd4, 0xd5, 0xd6, 0xd7, 0xd8, 0xd9, 0xda,
		0xe2, 0xe3, 0xe4, 0xe5, 0xe6, 0xe7, 0xe8, 0xe9, 0xea, 0xf2, 0xf3, 0xf4, 0xf5, 0xf6, 0xf7, 0xf8, 0xf9, 0xfa,
	}
)

var aan = [8]float64{1, 1.387039845, 1.306562965, 1.175875602, 1, 0.785694958, 0.5411961, 0.275899379}

// huffman holds a code and its length for every symbol; symbols the table lacks have
// length 0 and write nothing.
type huffman struct {
	codes [256]uint16
	sizes [256]uint8
}

func newHuffman(counts, values []byte) *huffman {
	h := &huffman{}
	code, k := uint16(0), 0
	for length := 1; length <= 16; length++ {
		for range counts[length-1] {
			h.codes[values[k]] = code
			h.sizes[values[k]] = uint8(length)
			code++
			k++
		}
		code <<= 1
	}
	return h
}

var (
	dcY = newHuffman(dcYCounts, dcValues)
	dcC = newHuffman(dcCCounts, dcValues)
	acY = newHuffman(acYCounts, acYValues)
	acC = newHuffman(acCCounts, acCValues)
)

// quantization holds the tables in zigzag order, and natural-order multipliers that fold
// in AAN scaling.
type quantization struct {
	y, c   [64]byte
	fy, fc [64]float64
}

func tables(quality int) *quantization {
	q := min(100, max(1, quality))
	scale := 200 - q*2
	if q < 50 {
		scale = 5000 / q
	}
	build := func(base *[64]int, table *[64]byte, factors *[64]float64) {
		for i := range 64 {
			table[zigzag[i]] = byte(min(255, max(1, (base[i]*scale+50)/100)))
		}
		for row := range 8 {
			for col := range 8 {
				factors[row*8+col] = 1 / (float64(table[zigzag[row*8+col]]) * aan[row] * aan[col] * 8)
			}
		}
	}
	t := &quantization{}
	build(&yQuant, &t.y, &t.fy)
	build(&cQuant, &t.c, &t.fc)
	return t
}

// quantize rounds a quantized coefficient, which stays within +-2048, halves up: shifted
// positive, truncation rounds, and costs less than math.Floor.
func quantize(x float64) int32 { return int32(x+16384.5) - 16384 }

// dctQuantize is the forward AAN DCT of one 8x8 block (rows in place, then columns),
// quantized straight into zigzag order.
func dctQuantize(d *[64]float64, factors *[64]float64, out *[64]int32) {
	for o := 0; o < 64; o += 8 {
		r := (*[8]float64)(d[o : o+8])
		d0, d1, d2, d3, d4, d5, d6, d7 := r[0], r[1], r[2], r[3], r[4], r[5], r[6], r[7]
		t0, t7, t1, t6, t2, t5, t3, t4 := d0+d7, d0-d7, d1+d6, d1-d6, d2+d5, d2-d5, d3+d4, d3-d4
		t10, t13, t11, t12 := t0+t3, t0-t3, t1+t2, t1-t2
		r[0] = t10 + t11
		r[4] = t10 - t11
		z1 := float64((t12 + t13) * 0.707106781)
		r[2] = t13 + z1
		r[6] = t13 - z1
		u10, u11, u12 := t4+t5, t5+t6, t6+t7
		z5 := float64((u10 - u12) * 0.382683433)
		z2 := float64(0.5411961*u10) + z5
		z4 := float64(1.306562965*u12) + z5
		z3 := float64(u11 * 0.707106781)
		z11, z13 := t7+z3, t7-z3
		r[5] = z13 + z2
		r[3] = z13 - z2
		r[1] = z11 + z4
		r[7] = z11 - z4
	}
	for o := range 8 {
		d0, d1, d2, d3, d4, d5, d6, d7 := d[o], d[o+8], d[o+16], d[o+24], d[o+32], d[o+40], d[o+48], d[o+56]
		t0, t7, t1, t6, t2, t5, t3, t4 := d0+d7, d0-d7, d1+d6, d1-d6, d2+d5, d2-d5, d3+d4, d3-d4
		t10, t13, t11, t12 := t0+t3, t0-t3, t1+t2, t1-t2
		z1 := float64((t12 + t13) * 0.707106781)
		u10, u11, u12 := t4+t5, t5+t6, t6+t7
		z5 := float64((u10 - u12) * 0.382683433)
		z2 := float64(0.5411961*u10) + z5
		z4 := float64(1.306562965*u12) + z5
		z3 := float64(u11 * 0.707106781)
		z11, z13 := t7+z3, t7-z3
		out[zigzag[o]&63] = quantize(float64((t10 + t11) * factors[o]))
		out[zigzag[o+8]&63] = quantize(float64((z11 + z4) * factors[o+8]))
		out[zigzag[o+16]&63] = quantize(float64((t13 + z1) * factors[o+16]))
		out[zigzag[o+24]&63] = quantize(float64((z13 - z2) * factors[o+24]))
		out[zigzag[o+32]&63] = quantize(float64((t10 - t11) * factors[o+32]))
		out[zigzag[o+40]&63] = quantize(float64((z13 + z2) * factors[o+40]))
		out[zigzag[o+48]&63] = quantize(float64((t13 - z1) * factors[o+48]))
		out[zigzag[o+56]&63] = quantize(float64((z11 - z4) * factors[o+56]))
	}
}

func appendSegment(out []byte, marker byte, body ...[]byte) []byte {
	n := 2
	for _, b := range body {
		n += len(b)
	}
	out = append(out, 0xff, marker, byte(n>>8), byte(n))
	for _, b := range body {
		out = append(out, b...)
	}
	return out
}

func appendHeader(out []byte, width, height int, t *quantization) []byte {
	out = append(out, 0xff, 0xd8)
	out = appendSegment(out, 0xdb, []byte{0}, t.y[:], []byte{1}, t.c[:])
	out = appendSegment(out, 0xc0, []byte{8, byte(height >> 8), byte(height), byte(width >> 8), byte(width), 3, 1, 0x22, 0, 2, 0x11, 1, 3, 0x11, 1})
	out = appendSegment(out, 0xc4,
		[]byte{0x00}, dcYCounts, dcValues, []byte{0x10}, acYCounts, acYValues,
		[]byte{0x01}, dcCCounts, dcValues, []byte{0x11}, acCCounts, acCValues)
	return appendSegment(out, 0xda, []byte{3, 1, 0x00, 2, 0x11, 3, 0x11, 0, 63, 0})
}

// encoder is one call's work space: calls run concurrently, so nothing here is shared.
type encoder struct {
	rows, columns [16]int
	luma          [256]float64
	blue, red     [64]float64
	zz            [64]int32
	// DC predictions of Y, Cb and Cr.
	pred [3]int32
	// Bits not yet written, right-aligned in acc; fewer than 8 between symbols.
	acc  uint64
	bits uint
	out  []byte
}

// EncodeJPEG is a baseline JPEG of packed RGB pixels; quality is 1 to 100, sheets use 90.
func EncodeJPEG(rgb []byte, width, height, quality int) []byte {
	t := tables(quality)
	// Half a byte a pixel covers a typical sheet; append grows it for busier pictures.
	e := &encoder{out: make([]byte, 0, max(4096, width*height/2))}
	e.out = appendHeader(e.out, width, height, t)
	for my := 0; my < height; my += 16 {
		e.row(rgb, width, height, my, t)
	}
	// Pad the last byte with ones, then end the image.
	if e.bits > 0 {
		pad := 8 - e.bits
		e.byte(byte(e.acc<<pad | (1<<pad - 1)))
	}
	return append(e.out, 0xff, 0xd9)
}

// row encodes one row of 16 x 16 MCUs.
func (e *encoder) row(rgb []byte, width, height, my int, t *quantization) {
	for k := range 16 {
		e.rows[k] = min(my+k, height-1) * width * 3
	}
	for mx := 0; mx < width; mx += 16 {
		for k := range 16 {
			e.columns[k] = min(mx+k, width-1) * 3
		}
		e.loadMCU(rgb)
		for b := range 4 {
			e.block((*[64]float64)(e.luma[b*64:(b+1)*64]), &t.fy, 0, dcY, acY)
		}
		e.block(&e.blue, &t.fc, 1, dcC, acC)
		e.block(&e.red, &t.fc, 2, dcC, acC)
	}
}

// loadMCU reads one 16 x 16 MCU straight from the RGB rows: four luma blocks,
// level-shifted, and the two chroma blocks averaged over 2 x 2. rows and columns hold
// byte offsets with the last row and column repeated past the edge, so the padding
// needs no branch.
func (e *encoder) loadMCU(rgb []byte) {
	for cy := range 8 {
		top, bottom := e.rows[2*cy], e.rows[2*cy+1]
		// Luma index of this pair of rows in the left blocks; the right blocks are 64 further.
		base := (cy>>2)*128 + ((2*cy)&7)*8
		for cx := range 8 {
			left, right := e.columns[2*cx], e.columns[2*cx+1]
			p0, p1 := rgb[top+left:top+left+3], rgb[top+right:top+right+3]
			p2, p3 := rgb[bottom+left:bottom+left+3], rgb[bottom+right:bottom+right+3]
			r0, g0, b0 := float64(p0[0]), float64(p0[1]), float64(p0[2])
			r1, g1, b1 := float64(p1[0]), float64(p1[1]), float64(p1[2])
			r2, g2, b2 := float64(p2[0]), float64(p2[1]), float64(p2[2])
			r3, g3, b3 := float64(p3[0]), float64(p3[1]), float64(p3[2])
			at := (base + (cx>>2)*64 + ((2 * cx) & 7)) & 255
			l := (*[10]float64)(e.luma[at : at+10 : 256])
			l[0] = float64(0.299*r0) + float64(0.587*g0) + float64(0.114*b0) - 128
			l[1] = float64(0.299*r1) + float64(0.587*g1) + float64(0.114*b1) - 128
			l[8] = float64(0.299*r2) + float64(0.587*g2) + float64(0.114*b2) - 128
			l[9] = float64(0.299*r3) + float64(0.587*g3) + float64(0.114*b3) - 128
			r, g, bl := r0+r1+r2+r3, g0+g1+g2+g3, b0+b1+b2+b3
			e.blue[(cy*8+cx)&63] = float64(-0.042184*r) - float64(0.082816*g) + float64(0.125*bl)
			e.red[(cy*8+cx)&63] = float64(0.125*r) - float64(0.104672*g) - float64(0.020328*bl)
		}
	}
}

// magnitude is the number of bits in |value|.
func magnitude(value int32) uint {
	if value < 0 {
		value = -value
	}
	return uint(bits.Len32(uint32(value)))
}

// valueBits is the low `size` bits of value, one less when negative, as T.81 codes them.
func valueBits(value int32, size uint) uint64 {
	if value < 0 {
		value--
	}
	return uint64(uint32(value) & (1<<size - 1))
}

// block transforms one block and writes its symbols, each a Huffman code with its
// value bits appended.
func (e *encoder) block(d *[64]float64, factors *[64]float64, component int, dc, ac *huffman) {
	dctQuantize(d, factors, &e.zz)
	zz := &e.zz
	diff := zz[0] - e.pred[component]
	e.pred[component] = zz[0]
	size := magnitude(diff)
	e.put(uint64(dc.codes[size])<<size|valueBits(diff, size), uint(dc.sizes[size])+size)
	run := uint(0)
	for i := 1; i < 64; i++ {
		value := zz[i]
		if value == 0 {
			run++
			continue
		}
		for ; run > 15; run -= 16 {
			e.put(uint64(ac.codes[0xf0]), uint(ac.sizes[0xf0]))
		}
		size = magnitude(value)
		symbol := (run<<4 | size) & 255
		e.put(uint64(ac.codes[symbol])<<size|valueBits(value, size), uint(ac.sizes[symbol])+size)
		run = 0
	}
	if run > 0 {
		e.put(uint64(ac.codes[0]), uint(ac.sizes[0]))
	}
}

// put appends size bits of code: at most 16 of Huffman code and 13 of value, on top of
// fewer than 8 pending, well inside the 64-bit accumulator.
func (e *encoder) put(code uint64, size uint) {
	e.acc = e.acc<<size | code
	e.bits += size
	for e.bits >= 8 {
		e.bits -= 8
		e.byte(byte(e.acc >> e.bits))
	}
}

func (e *encoder) byte(b byte) {
	e.out = append(e.out, b)
	// A 0xFF in entropy-coded data is followed by a zero byte, or it reads as a marker.
	if b == 0xff {
		e.out = append(e.out, 0)
	}
}
