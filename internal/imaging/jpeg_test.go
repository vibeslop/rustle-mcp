package imaging

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"os/exec"
	"sync"
	"testing"
)

// decoded decodes with ffmpeg, which rejects a malformed stream (bad stuffing, short
// data) loudly.
func decoded(t *testing.T, jpeg []byte) (rgb []byte, errors string) {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not found")
	}
	cmd := exec.Command(ffmpeg, "-v", "error", "-f", "jpeg_pipe", "-i", "pipe:0", "-f", "rawvideo", "-pix_fmt", "rgb24", "pipe:1")
	var stdout, stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(jpeg), &stdout, &stderr
	cmd.Run()
	return stdout.Bytes(), stderr.String()
}

func psnr(a, b []byte) float64 {
	sum := 0.0
	for i := range a {
		d := float64(a[i]) - float64(b[i])
		sum += d * d
	}
	return 10 * math.Log10(255*255/(sum/float64(len(a))))
}

// picture is test/jpeg.test.ts's: smooth gradients, a hard-edged block and a noisy strip,
// every kind of 8 x 8 block. Its generator is JavaScript's double arithmetic, product
// rounded before the sum.
func picture(width, height int) []byte {
	rgb := make([]byte, width*height*3)
	seed := int64(7)
	i := 0
	for y := range height {
		for x := range width {
			noise := 0.0
			if float64(y) > float64(height)*0.8 {
				seed = int64(float64(float64(seed)*1103515245)+12345) & 0x7fffffff
				noise = float64(seed&63 - 32)
			}
			block := float64(x) > float64(width)/3 && float64(x) < float64(width)/2 && float64(y) < float64(height)/2
			clamp := func(v float64) byte { return byte(max(0, min(255, int(v)))) }
			rgb[i] = clamp(float64(x*255)/float64(width) + noise)
			rgb[i+1] = clamp(float64(y*255)/float64(height) + noise)
			rgb[i+2] = 128
			if block {
				rgb[i+1] = 255
				rgb[i+2] = 0
			}
			i += 3
		}
	}
	return rgb
}

func TestJPEGDecodesAtSizesThatAreNotWholeBlocks(t *testing.T) {
	for _, size := range [][2]int{{1176, 504}, {37, 23}, {1, 1}} {
		width, height := size[0], size[1]
		rgb := picture(width, height)
		back, errors := decoded(t, EncodeJPEG(rgb, width, height, 90))
		if errors != "" || len(back) != len(rgb) {
			t.Fatalf("%d x %d: %d bytes back, %q", width, height, len(back), errors)
		}
		// In a small picture the saturated block's edge, its colour shared by 2 x 2 pixels, weighs more.
		floor := 22.0
		if width > 100 {
			floor = 32
		}
		if p := psnr(rgb, back); width > 8 && p <= floor {
			t.Errorf("%d x %d: %.1f dB", width, height, p)
		}
	}
}

func TestJPEGQualityTradesBytesForFidelity(t *testing.T) {
	rgb := picture(320, 180)
	low := EncodeJPEG(rgb, 320, 180, 50)
	high := EncodeJPEG(rgb, 320, 180, 95)
	if len(low) >= len(high) {
		t.Fatalf("quality 50 is %d bytes, 95 is %d", len(low), len(high))
	}
	lowBack, _ := decoded(t, low)
	highBack, _ := decoded(t, high)
	if psnr(rgb, lowBack) >= psnr(rgb, highBack) {
		t.Fatalf("quality 50 is as faithful as 95")
	}
}

// The golden files and hashes are the TypeScript encoder's (src/media/jpeg.ts,
// encodeJpegRgb) output for test/jpeg.test.ts's picture, written once by a Node script
// that imported both; the small pictures are kept whole so a mismatch shows its offset.
func TestJPEGMatchesTheTypeScriptEncoder(t *testing.T) {
	sum := func(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
	for _, c := range []struct {
		width, height, quality int
		golden, input, jpeg    string
	}{
		{width: 37, height: 23, quality: 90, golden: "testdata/picture-37x23-q90.jpg"},
		{width: 64, height: 48, quality: 50, golden: "testdata/picture-64x48-q50.jpg"},
		{1176, 504, 90, "", "fc56aa38902425dd6f3f7581801eed0c9ead660c3096a6cc948882d760c70777", "1050e6946ac4d5306b92839678a4216c6de03269166906cbc5aacda00589e444"},
		{64, 48, 1, "", "08e67e42d7b7b7cae3514e5027e50327959938ee60c89d8a628f518658a1836b", "d9b3a1ed3742bb596cee1316574ca5263460da92136179a8de2953267cbeec5a"},
		{64, 48, 100, "", "08e67e42d7b7b7cae3514e5027e50327959938ee60c89d8a628f518658a1836b", "d2c0ab300674c5b11c33d1ee2dce7795b60f430c4a46d39f07507fb4d007eedb"},
		{1, 1, 90, "", "d3a064592dc6bda7ad009a53d77dc197fd15e0fd3b291f30eb8ab37862e2fc95", "854532a36acdb1e392c4e2f355600da43b4ec5a9815699374a072713a21b1cf6"},
	} {
		name := fmt.Sprintf("%dx%d q%d", c.width, c.height, c.quality)
		rgb := picture(c.width, c.height)
		got := EncodeJPEG(rgb, c.width, c.height, c.quality)
		if c.golden != "" {
			want, err := os.ReadFile(c.golden)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				at := 0
				for at < min(len(got), len(want)) && got[at] == want[at] {
					at++
				}
				t.Errorf("%s: %d bytes, want %d; first difference at %d", name, len(got), len(want), at)
			}
			continue
		}
		if sum(rgb) != c.input {
			t.Errorf("%s: the picture differs from the TypeScript test's", name)
		} else if sum(got) != c.jpeg {
			t.Errorf("%s: the JPEG differs from the TypeScript encoder's", name)
		}
	}
}

// Sheets are encoded concurrently in the server; each call must get its own work space.
func TestJPEGEncodesConcurrently(t *testing.T) {
	pictures := [][]byte{picture(320, 180), picture(37, 23)}
	sizes := [][2]int{{320, 180}, {37, 23}}
	want := make([][]byte, len(pictures))
	for i, rgb := range pictures {
		want[i] = EncodeJPEG(rgb, sizes[i][0], sizes[i][1], 90)
	}
	var wg sync.WaitGroup
	for range 4 {
		for i, rgb := range pictures {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if !bytes.Equal(EncodeJPEG(rgb, sizes[i][0], sizes[i][1], 90), want[i]) {
					t.Errorf("picture %d differs when encoded concurrently", i)
				}
			}()
		}
	}
	wg.Wait()
}

func BenchmarkEncodeJPEG(b *testing.B) {
	rgb := picture(1176, 504)
	b.SetBytes(int64(len(rgb)))
	for b.Loop() {
		EncodeJPEG(rgb, 1176, 504, 90)
	}
}
