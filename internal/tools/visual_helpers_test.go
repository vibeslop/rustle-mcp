package tools_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/draw"
	_ "image/jpeg"
	_ "image/png"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/vibeslop/rustle-mcp/internal/proc"
	"github.com/vibeslop/rustle-mcp/internal/rustle"
	"github.com/vibeslop/rustle-mcp/internal/testmedia"
)

// The inspection video (1280 x 720) shows a code label from 2 s and a magenta box at
// [650, 250, 1050, 550] from 5.0 to 5.5 s.
var (
	inspection = testmedia.Inspection()
	magenta    = &[4]float64{650.0 / 1280, 250.0 / 720, 400.0 / 1280, 300.0 / 720}
)

const codeAt = 2

type fixtures struct {
	dir  string
	beep string
	// long is 40 s of testsrc2 at 30 fps with a 3 s keyframe interval and a tone.
	long string
	// transport is long as MPEG-TS, which starts its clock at 1.4 s and has no index, so
	// its seeks can overshoot.
	transport string
}

func ffmpeg(t *testing.T, args ...string) {
	t.Helper()
	path := proc.Which("ffmpeg")
	if path == "" {
		t.Skip("needs ffmpeg")
	}
	if out, err := exec.Command(path, append([]string{"-v", "error"}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %v: %v %s", args, err, out)
	}
}

func newFixtures(t *testing.T) *fixtures {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Records reach the cache folder after their call returns; the folder goes once they have.
	t.Cleanup(rustle.FlushRecords)
	f := &fixtures{dir: dir, beep: filepath.Join(dir, "beep.wav"), long: filepath.Join(dir, "long.mp4"), transport: filepath.Join(dir, "long.ts")}
	if err := testmedia.WriteBeep(f.beep); err != nil {
		t.Fatal(err)
	}
	ffmpeg(t, "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=30:duration=40", "-f", "lavfi", "-i", "sine=frequency=440:duration=40",
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "90", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", f.long)
	ffmpeg(t, "-i", f.long, "-c", "copy", f.transport)
	return f
}

func (f *fixtures) call(t *testing.T, client string) *rustle.Call {
	t.Helper()
	s, err := rustle.LoadSettings(rustle.MapEnv(map[string]string{"RUSTLE_CACHE": filepath.Join(f.dir, "cache")}))
	if err != nil {
		t.Fatal(err)
	}
	return &rustle.Call{Ctx: context.Background(), Settings: s, Cache: rustle.NewCache(s.CacheDir, s.CacheBytes), Client: rustle.ProfileFor(client), Notes: &rustle.Notes{}}
}

func ptr(v float64) *float64 { return &v }

// shown is metadata as the model reads it, field by field: unset fields left out.
func shown(t *testing.T, meta *rustle.Obj) map[string]json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(rustle.Compact(meta)), &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}

// sameJSON compares metadata as the model reads it with want, in any key order.
func sameJSON(t *testing.T, meta *rustle.Obj, want string) {
	t.Helper()
	var got, expected any
	if err := json.Unmarshal([]byte(rustle.Compact(meta)), &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, expected) {
		t.Errorf("metadata %s, want %s", rustle.Compact(meta), want)
	}
}

// fields checks metadata fields against their JSON; "" means left out.
func fields(t *testing.T, meta *rustle.Obj, want map[string]string) {
	t.Helper()
	got := shown(t, meta)
	for key, value := range want {
		if string(got[key]) != value {
			t.Errorf("%s is %s, want %s", key, got[key], value)
		}
	}
}

func number(t *testing.T, meta *rustle.Obj, key string) float64 {
	t.Helper()
	v, ok := meta.Get(key)
	n, isNumber := v.(float64)
	if !ok || !isNumber {
		t.Fatalf("%s is %v", key, v)
	}
	return n
}

func times(t *testing.T, meta *rustle.Obj) []float64 {
	t.Helper()
	v, _ := meta.Get("times")
	list, ok := v.([]float64)
	if !ok {
		t.Fatalf("times is %v", v)
	}
	return list
}

type picture struct {
	rgb           []byte
	width, height int
}

// pixels decodes an image block to RGB.
func pixels(t *testing.T, block rustle.Block) picture {
	t.Helper()
	if block.Type != "image" {
		t.Fatalf("a %s block, not an image", block.Type)
	}
	data, err := base64.StdEncoding.DecodeString(block.Data)
	if err != nil {
		t.Fatal(err)
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	bounds := img.Bounds()
	rgba := image.NewRGBA(bounds)
	draw.Draw(rgba, bounds, img, bounds.Min, draw.Src)
	w, h := bounds.Dx(), bounds.Dy()
	rgb := make([]byte, 0, w*h*3)
	for i := 0; i < len(rgba.Pix); i += 4 {
		rgb = append(rgb, rgba.Pix[i], rgba.Pix[i+1], rgba.Pix[i+2])
	}
	return picture{rgb: rgb, width: w, height: h}
}

func isMagenta(rgb []byte, i int) bool { return rgb[i] > 200 && rgb[i+1] < 60 && rgb[i+2] > 200 }

// magentaByThird is which thirds of a three-cell sheet show the magenta rectangle (over 200 pixels of it).
func magentaByThird(p picture) [3]bool {
	var counts [3]int
	for y := range p.height {
		for x := range p.width {
			if isMagenta(p.rgb, (y*p.width+x)*3) {
				counts[min(2, 3*x/p.width)]++
			}
		}
	}
	return [3]bool{counts[0] > 200, counts[1] > 200, counts[2] > 200}
}
