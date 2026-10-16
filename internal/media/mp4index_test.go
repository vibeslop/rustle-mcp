package media

import (
	"math"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/vibeslop/rustle-mcp/internal/proc"
)

// indexClip writes 10 s at 30 fps with a keyframe every 5 s, B-frames on.
func indexClip(t *testing.T) string {
	t.Helper()
	ffmpeg := proc.Which("ffmpeg")
	if ffmpeg == "" {
		t.Skip("needs ffmpeg")
	}
	clip := filepath.Join(t.TempDir(), "clip.mp4")
	out, err := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=30:duration=10",
		"-c:v", "libx264", "-preset", "veryfast", "-g", "150", "-bf", "2", "-pix_fmt", "yuv420p", clip).CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
	return clip
}

func TestTheIndexFindsEveryFrameAndKeyframeFFprobeDoes(t *testing.T) {
	clip := indexClip(t)
	index := IndexOf(clip)
	if index == nil {
		t.Fatal("no index")
	}
	probe, err := exec.Command(proc.Which("ffprobe"), "-v", "error", "-select_streams", "v:0", "-show_entries", "packet=pts_time,flags", "-of", "csv=p=0", clip).Output()
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(probe)), "\n")
	if len(index.Sizes) != len(lines) {
		t.Errorf("%d samples, ffprobe finds %d packets", len(index.Sizes), len(lines))
	}
	ms := func(seconds float64) int { return int(math.Round(seconds * 1000)) }
	var want []int
	for _, line := range lines {
		pts, flags, _ := strings.Cut(line, ",")
		if strings.HasPrefix(flags, "K") {
			seconds, err := strconv.ParseFloat(pts, 64)
			if err != nil {
				t.Fatalf("pts %q: %v", pts, err)
			}
			want = append(want, ms(seconds))
		}
	}
	var keys []int
	for _, k := range index.Keys {
		keys = append(keys, ms(index.Times[k]))
	}
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("keyframes at %v ms, ffprobe %v", keys, want)
	}
	if index.Ordered {
		t.Error("B-frames, yet presentation order is decode order")
	}
}

func TestSequentialPassesSplitAtKeyframesNeverInsideAGOP(t *testing.T) {
	source := &Input{Input: indexClip(t), Native: [2]int{160, 90}, FPS: 30}
	if got := KeyTimes(source); !reflect.DeepEqual(got, []float64{0, 5}) {
		t.Fatalf("KeyTimes = %v, want [0 5]", got)
	}
	times := make([]float64, 12)
	for i := range times {
		times[i] = float64(i) * (10 - 1.0/30) / 11
	}
	chunks := Chunked(source, times, make([]*Sampled, 12), times[1])
	if want := [][]int{{0, 1, 2, 3, 4, 5}, {6, 7, 8, 9, 10, 11}}; !reflect.DeepEqual(chunks, want) {
		t.Errorf("chunks %v, want %v", chunks, want)
	}
	// Samples already found leave gaps that end a pass.
	found := make([]*Sampled, 12)
	found[2] = &Sampled{Frame: *NewFrame(times[2], 1, 1, make([]byte, 3))}
	if want := [][]int{{0, 1}, {3, 4, 5}, {6, 7, 8, 9, 10, 11}}; !reflect.DeepEqual(Chunked(source, times, found, times[1]), want) {
		t.Errorf("chunks with a gap %v, want %v", Chunked(source, times, found, times[1]), want)
	}
}
