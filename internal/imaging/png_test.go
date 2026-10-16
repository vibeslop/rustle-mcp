package imaging

import (
	"bytes"
	"image"
	"image/png"
	"testing"
)

func decodePNG(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func TestPNGRoundTrip(t *testing.T) {
	const width, height = 37, 23
	rgb := picture(width, height)
	img, ok := decodePNG(t, EncodePNG(rgb, width, height, 3, 6)).(*image.RGBA)
	if !ok {
		t.Fatal("RGB did not decode as RGBA")
	}
	for y := range height {
		for x := range width {
			at, px := (y*width+x)*3, img.RGBAAt(x, y)
			if [3]byte{px.R, px.G, px.B} != [3]byte(rgb[at:at+3]) {
				t.Fatalf("RGB pixel %d,%d is %v", x, y, px)
			}
		}
	}

	gray := make([]byte, width*height)
	for i := range gray {
		gray[i] = byte(i * 7)
	}
	g, ok := decodePNG(t, EncodePNG(gray, width, height, 1, 6)).(*image.Gray)
	if !ok || !bytes.Equal(g.Pix, gray) || g.Stride != width {
		t.Fatal("gray pixels changed")
	}

	palette := []byte{0, 0, 0, 255, 0, 0, 0, 255, 0}
	indices := make([]byte, width*height)
	for i := range indices {
		indices[i] = byte(i % 3)
	}
	p, ok := decodePNG(t, EncodeIndexedPNG(indices, width, height, palette, 6)).(*image.Paletted)
	if !ok || !bytes.Equal(p.Pix, indices) || len(p.Palette) != 3 {
		t.Fatal("indices changed")
	}
	if r, g, b, _ := p.Palette[1].RGBA(); r>>8 != 255 || g != 0 || b != 0 {
		t.Fatal("palette changed")
	}

	if w, h := PNGSize(EncodePNG(gray, width, height, 1, 6)); w != width || h != height {
		t.Fatalf("size %d x %d", w, h)
	}
}
