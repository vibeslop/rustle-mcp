package tools_test

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vibeslop/rustle-mcp/internal/imaging"
	"github.com/vibeslop/rustle-mcp/internal/media"
	"github.com/vibeslop/rustle-mcp/internal/proc"
	"github.com/vibeslop/rustle-mcp/internal/rustle"
	"github.com/vibeslop/rustle-mcp/internal/tools"
)

func TestInspectFrame(t *testing.T) {
	fx := newFixtures(t)
	frame := func(t *testing.T, args tools.FrameArgs) (*rustle.Result, *rustle.Call) {
		t.Helper()
		c := fx.call(t, "claude-code")
		result, err := tools.Default.Frame(c, args)
		if err != nil {
			t.Fatalf("inspect_frame %+v: %v", args, err)
		}
		return result, c
	}

	t.Run("a crop returns the code region at native scale, its size given once", func(t *testing.T) {
		result, _ := frame(t, tools.FrameArgs{Source: inspection, At: 1, Crop: &[4]float64{0.03, 0.07, 0.36, 0.19}})
		sameJSON(t, result.Meta, `{"at":1,"size":[461,137]}`)
		if result.Content[0].MimeType != "image/png" {
			t.Errorf("sent as %s", result.Content[0].MimeType)
		}
		p := pixels(t, result.Content[0])
		// The white label box with dark text inside it.
		at := func(x, y int) int { return (y*p.width + x) * 3 }
		if p.rgb[at(30, 20)] <= 240 {
			t.Errorf("label box at (30, 20) is %d", p.rgb[at(30, 20)])
		}
		dark := 0
		for x := 40; x < 300; x++ {
			if p.rgb[at(x, 68)] < 80 {
				dark++
			}
		}
		if dark <= 10 {
			t.Errorf("%d text pixels on the code line", dark)
		}
	})

	t.Run("a frame fits 672 px; a crop costs no more than it, so a tall one arrives native and a small one enlarged", func(t *testing.T) {
		hd := filepath.Join(fx.dir, "hd-frame.mp4")
		ffmpeg(t, "-f", "lavfi", "-i", "testsrc2=size=1920x1080:rate=30:duration=1", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", hd)
		at := func(t *testing.T, crop *[4]float64, detail string) *rustle.Obj {
			result, _ := frame(t, tools.FrameArgs{Source: hd, At: 0.5, Crop: crop, Detail: detail})
			return result.Meta
		}
		budget := rustle.ImageTokens(672, 378)
		for _, c := range []struct {
			crop   *[4]float64
			detail string
			want   string
		}{
			{nil, "", `{"at":0.5,"size":[672,378],"native":[1920,1080],"hint":"crop, or detail=full, for full detail"}`},
			{&[4]float64{0.5, 0.5, 0.5, 0.5}, "", `{"at":0.5,"size":[672,378],"native":[960,540],"hint":"a smaller crop, or detail=full, for full detail"}`},
			{&[4]float64{0.5, 0.5, 0.25, 0.25}, "", `{"at":0.5,"size":[480,270]}`},
			// A strip across the frame is as many pixels as the frame, so more of its detail shows.
			{&[4]float64{0, 0, 1, 0.5}, "", `{"at":0.5,"size":[950,267],"native":[1920,540],"hint":"a smaller crop, or detail=full, for full detail"}`},
			// A sidebar, 259 x 821: a long-edge cap of 672 px would have shown it at 82%.
			{&[4]float64{0.515, 0.2, 0.135, 0.76}, "", `{"at":0.5,"size":[259,821]}`},
			{&[4]float64{0.5, 0.9, 0.135, 0.06}, "", `{"at":0.5,"size":[672,169],"native":[259,65],"zoom":2.6}`},
			{&[4]float64{0.5, 0.5, 0.02, 0.02}, "", `{"at":0.5,"size":[152,88],"native":[38,22],"zoom":4}`},
			{nil, "full", `{"at":0.5,"size":[1920,1080]}`},
			{&[4]float64{0.5, 0.9, 0.135, 0.06}, "full", `{"at":0.5,"size":[259,65]}`},
		} {
			name := "whole frame"
			if c.crop != nil {
				name = fmt.Sprintf("crop %v", *c.crop)
			}
			if c.detail != "" {
				name += " detail=" + c.detail
			}
			t.Run(name, func(t *testing.T) {
				meta := at(t, c.crop, c.detail)
				sameJSON(t, meta, c.want)
				if c.crop != nil && c.detail == "" {
					size, _ := meta.Get("size")
					wh := size.([]int)
					if tokens := rustle.ImageTokens(wh[0], wh[1]); float64(tokens) > float64(budget)*1.02 {
						t.Errorf("%d tokens, the whole frame %d", tokens, budget)
					}
				}
			})
		}
	})

	t.Run("an enlarged crop is the same region, smoothly enlarged", func(t *testing.T) {
		crop := &[4]float64{0.03, 0.07, 0.15, 0.1}
		zoomed, _ := frame(t, tools.FrameArgs{Source: inspection, At: codeAt, Crop: crop})
		native, _ := frame(t, tools.FrameArgs{Source: inspection, At: codeAt, Crop: crop, Detail: "full"})
		fields(t, zoomed.Meta, map[string]string{"zoom": "3.5"})
		small := pixels(t, native.Content[0])
		big := pixels(t, zoomed.Content[0])
		// Back to source size, the enlarged crop matches the source pixels.
		scale := exec.Command(proc.Which("ffmpeg"), "-v", "error", "-f", "rawvideo", "-pix_fmt", "rgb24", "-s", fmt.Sprintf("%dx%d", big.width, big.height), "-i", "pipe:0",
			"-vf", fmt.Sprintf("scale=%d:%d:flags=area", small.width, small.height), "-f", "rawvideo", "-pix_fmt", "rgb24", "pipe:1")
		scale.Stdin = bytes.NewReader(big.rgb)
		back, err := scale.Output()
		if err != nil || len(back) != len(small.rgb) {
			t.Fatalf("scaling back: %v, %d bytes for %d", err, len(back), len(small.rgb))
		}
		total := 0
		for i := range back {
			total += max(int(back[i])-int(small.rgb[i]), int(small.rgb[i])-int(back[i]))
		}
		if mean := float64(total) / float64(len(back)); mean >= 8 {
			t.Errorf("mean error %.1f", mean)
		}
	})

	t.Run("a time watch_video listed for a small source comes from the frames it decoded, pixel for pixel", func(t *testing.T) {
		sheet, err := tools.Default.Watch(fx.call(t, "claude-code"), tools.WatchArgs{Source: fx.long, Frames: 12, Start: 20, End: ptr(30)})
		if err != nil {
			t.Fatal(err)
		}
		at := times(t, sheet.Meta)[3]
		half := &[4]float64{0.25, 0, 0.5, 1}
		kept, _ := frame(t, tools.FrameArgs{Source: fx.long, At: at, Crop: half})
		// Another path is another source, decoded afresh.
		copied := filepath.Join(fx.dir, "long-copy.mp4")
		data, err := os.ReadFile(fx.long)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(copied, data, 0o644); err != nil {
			t.Fatal(err)
		}
		fresh, _ := frame(t, tools.FrameArgs{Source: copied, At: at, Crop: half})
		if rustle.Compact(kept.Meta) != rustle.Compact(fresh.Meta) {
			t.Errorf("kept %s, fresh %s", rustle.Compact(kept.Meta), rustle.Compact(fresh.Meta))
		}
		decoded := pixels(t, fresh.Content[0])
		if !bytes.Equal(pixels(t, kept.Content[0]).rgb, decoded.rgb) {
			t.Error("the kept frame's pixels differ from a fresh decode's")
		}
		// Rustle's own PNG of those pixels, byte for byte: ffmpeg's encoder, had a decoder run, writes other chunks.
		png, _ := base64.StdEncoding.DecodeString(kept.Content[0].Data)
		if !bytes.Equal(png, imaging.EncodePNG(decoded.rgb, decoded.width, decoded.height, 3, 6)) {
			t.Error("a decoder process made the kept frame's PNG")
		}
	})

	t.Run("a time past the end shows the last frame and says so", func(t *testing.T) {
		last, c := frame(t, tools.FrameArgs{Source: inspection, At: 100})
		if got := number(t, last.Meta, "at"); got != 11.75 {
			t.Errorf("at %v, want 11.75", got)
		}
		if list := c.Notes.List(); len(list) == 0 || !strings.Contains(list[0], "past the end") {
			t.Errorf("notes %q", list)
		}
		// The video track of an AAC-muxed file ends before the container; the last frame still comes back.
		end, _ := frame(t, tools.FrameArgs{Source: fx.long, At: 39.999})
		if got := number(t, end.Meta, "at"); got <= 39.9 {
			t.Errorf("mp4 at 39.999 shows %v", got)
		}
		ts, _ := frame(t, tools.FrameArgs{Source: fx.transport, At: 1000})
		if got := number(t, ts.Meta, "at"); got <= 39.9 {
			t.Errorf("MPEG-TS at 1000 shows %v", got)
		}
	})
}

func TestKeptFramesAreFoundByListedTimeAndTheOldestLeavePastTheBudget(t *testing.T) {
	frameAt := func(at float64) *media.Frame { return media.NewFrame(at, 10, 10, make([]byte, 300)) }
	kept := tools.NewShown(700)
	kept.Add("a", frameAt(1.004))
	kept.Add("a", frameAt(2))
	if f := kept.Find("a", 1); f == nil || f.At != 1.004 {
		t.Errorf("Find(a, 1) = %v, want the frame at 1.004", f)
	}
	if f := kept.Find("a", 1.01); f != nil {
		t.Errorf("Find(a, 1.01) = %v", f.At)
	}
	if f := kept.Find("b", 2); f != nil {
		t.Errorf("Find(b, 2) = %v", f.At)
	}
	kept.Add("a", frameAt(3))
	if f := kept.Find("a", 1); f != nil {
		t.Errorf("the oldest frame stayed past the budget")
	}
	if f := kept.Find("a", 3); f == nil || f.At != 3 {
		t.Errorf("Find(a, 3) = %v", f)
	}
}
