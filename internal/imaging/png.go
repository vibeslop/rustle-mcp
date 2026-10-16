package imaging

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"hash/crc32"
)

var pngSignature = []byte{137, 80, 78, 71, 13, 10, 26, 10}

func appendChunk(out []byte, kind string, data []byte) []byte {
	out = binary.BigEndian.AppendUint32(out, uint32(len(data)))
	start := len(out)
	out = append(out, kind...)
	out = append(out, data...)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(out[start:]))
}

func deflate(raw []byte, level int) []byte {
	var buf bytes.Buffer
	buf.Grow(len(raw)/2 + 64)
	w, err := zlib.NewWriterLevel(&buf, level)
	if err != nil {
		w = zlib.NewWriter(&buf)
	}
	w.Write(raw)
	w.Close()
	return buf.Bytes()
}

func ihdr(width, height int, colorType byte) []byte {
	h := make([]byte, 13)
	binary.BigEndian.PutUint32(h[0:], uint32(width))
	binary.BigEndian.PutUint32(h[4:], uint32(height))
	h[8] = 8
	h[9] = colorType
	return h
}

// EncodePNG is a lossless PNG of packed 8-bit pixels (3 channels RGB, or 1 gray), with the
// Up filter on every row: photographic rows compress well against the row above, and
// choosing one filter avoids a per-row search that would cost more time than it saves
// bytes. Level is zlib's, 0 to 9; callers use 6.
func EncodePNG(pixels []byte, width, height, channels, level int) []byte {
	stride := width * channels
	raw := make([]byte, (stride+1)*height)
	for y := range height {
		row := raw[y*(stride+1)+1 : (y+1)*(stride+1)]
		at := y * stride
		if y == 0 {
			copy(row, pixels[:stride])
			continue
		}
		raw[y*(stride+1)] = 2
		cur, above := pixels[at:at+stride], pixels[at-stride:at]
		for x := range row {
			row[x] = cur[x] - above[x]
		}
	}
	colorType := byte(0)
	if channels == 3 {
		colorType = 2
	}
	out := append([]byte(nil), pngSignature...)
	out = appendChunk(out, "IHDR", ihdr(width, height, colorType))
	out = appendChunk(out, "IDAT", deflate(raw, level))
	return appendChunk(out, "IEND", nil)
}

// EncodeIndexedPNG is a lossless PNG of 8-bit indices into an RGB palette (at most 256
// entries, packed). Rows stay unfiltered, as filters predict intensities, not indices; a
// chart of a few hundred colours comes out at about 40% of the same image as RGB.
func EncodeIndexedPNG(indices []byte, width, height int, palette []byte, level int) []byte {
	raw := make([]byte, (width+1)*height)
	for y := range height {
		copy(raw[y*(width+1)+1:], indices[y*width:(y+1)*width])
	}
	out := append([]byte(nil), pngSignature...)
	out = appendChunk(out, "IHDR", ihdr(width, height, 3))
	out = appendChunk(out, "PLTE", palette)
	out = appendChunk(out, "IDAT", deflate(raw, level))
	return appendChunk(out, "IEND", nil)
}

// PNGSize is the width and height from a PNG header, or zeros when there is no header.
func PNGSize(png []byte) (int, int) {
	if len(png) < 24 {
		return 0, 0
	}
	return int(binary.BigEndian.Uint32(png[16:])), int(binary.BigEndian.Uint32(png[20:]))
}
