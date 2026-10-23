package tools_test

import (
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unsafe"

	"github.com/vibeslop/rustle-mcp/internal/media"
	"github.com/vibeslop/rustle-mcp/internal/rustle"
	"github.com/vibeslop/rustle-mcp/internal/tools"
)

func record(edit func(r *rustle.Record)) *rustle.Record {
	r := &rustle.Record{Key: "k", Kind: "local", Source: "s", Title: "t", Duration: 8, FPS: 12, Width: 320, Height: 180, HasVideo: rustle.Bool(true)}
	if edit != nil {
		edit(r)
	}
	return r
}

func TestSamplingPlan(t *testing.T) {
	t.Run("even samples include both ends, the last one frame before the end", func(t *testing.T) {
		p, err := tools.MakePlan(record(nil), tools.WatchArgs{Source: "s", Frames: 12}, &rustle.Notes{})
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Times) != 12 || p.Times[0] != 0 || math.Abs(p.Times[11]-(8-1.0/12)) >= 1e-9 {
			t.Errorf("times %v", p.Times)
		}
		if p.Covered != 8 {
			t.Errorf("covered %v, want 8", p.Covered)
		}
	})

	t.Run("more samples than frames are reduced to the frames there are", func(t *testing.T) {
		short := record(func(r *rustle.Record) { r.Duration, r.FPS = 1, 4 })
		p, err := tools.MakePlan(short, tools.WatchArgs{Source: "s", Frames: 48}, &rustle.Notes{})
		if err != nil {
			t.Fatal(err)
		}
		if want := []float64{0, 0.25, 0.5, 0.75}; !reflect.DeepEqual(p.Times, want) {
			t.Errorf("times %v, want %v", p.Times, want)
		}
	})

	t.Run("fps mode samples consecutively and is capped at the video rate", func(t *testing.T) {
		notes := &rustle.Notes{}
		slow := record(func(r *rustle.Record) { r.FPS = 4 })
		p, err := tools.MakePlan(slow, tools.WatchArgs{Source: "s", Frames: 48, FPS: 8}, notes)
		if err != nil {
			t.Fatal(err)
		}
		if p.FPS != 4 || len(p.Times) != 32 || p.Covered != 8 {
			t.Errorf("fps %v, %d times, covered %v; want 4, 32, 8", p.FPS, len(p.Times), p.Covered)
		}
		if list := notes.List(); len(list) == 0 || !strings.Contains(list[0], "above the video's 4 fps") {
			t.Errorf("notes %q", list)
		}
		burst, err := tools.MakePlan(record(nil), tools.WatchArgs{Source: "s", Frames: 8, FPS: 4}, &rustle.Notes{})
		if err != nil {
			t.Fatal(err)
		}
		if want := []float64{0, 0.25, 0.5, 0.75, 1, 1.25, 1.5, 1.75}; !reflect.DeepEqual(burst.Times, want) {
			t.Errorf("burst times %v, want %v", burst.Times, want)
		}
		if burst.Covered != 2 {
			t.Errorf("burst covered %v, want 2", burst.Covered)
		}
	})

	t.Run("a start past the end shows the last span of the requested length", func(t *testing.T) {
		for _, c := range []struct {
			start  float64
			end    *float64
			length float64
			want   [2]float64
			note   string
		}{
			{100, nil, 0, [2]float64{0, 8}, `past the end \(8 s\); showing 0-8`},
			{100, ptr(103), 0, [2]float64{5, 8}, ""},
			{100, nil, 2, [2]float64{6, 8}, ""},
			{2, ptr(20), 0, [2]float64{2, 8}, "end 20 clamped to the duration 8"},
		} {
			notes := &rustle.Notes{}
			start, end, err := tools.Span(8, c.start, c.end, notes, 0.05, c.length)
			if err != nil || [2]float64{start, end} != c.want {
				t.Errorf("Span(start %v, length %v) = %v, %v, %v; want %v", c.start, c.length, start, end, err, c.want)
			}
			if list := notes.List(); c.note != "" && (len(list) == 0 || !regexp.MustCompile(c.note).MatchString(list[0])) {
				t.Errorf("notes %q, want %s", list, c.note)
			}
		}
		_, _, err := tools.Span(8, 4, ptr(2), &rustle.Notes{}, 0.05, 0)
		if _, ok := rustle.AsError(err); !ok {
			t.Errorf("an end before the start: %v", err)
		}
	})
}

func TestWatchVideo(t *testing.T) {
	fx := newFixtures(t)
	watch := func(t *testing.T, client string, args tools.WatchArgs) (*rustle.Result, *rustle.Call) {
		t.Helper()
		c := fx.call(t, client)
		result, err := tools.Default.Watch(c, args)
		if err != nil {
			t.Fatalf("watch_video %+v: %v", args, err)
		}
		return result, c
	}

	t.Run("the inspection video: one cell per still run, the magenta frame kept apart, at half size", func(t *testing.T) {
		result, _ := watch(t, "claude-code", tools.WatchArgs{Source: inspection, Frames: 12})
		fields(t, result.Meta, map[string]string{
			"times":  "[0,1,2,3,4.25,5.25,6.25,7.25,8.5,9.5,10.5,11.75]",
			"merged": "[[1,5],[7,12]]",
			// A local file's path names it, and a whole-video span is the duration.
			"title": "", "span": "", "next_start_seconds": "",
		})
		if len(result.Content) != 1 {
			t.Fatalf("%d blocks, want 1", len(result.Content))
		}
		sheet := pixels(t, result.Content[0])
		// Three flat cells of 138 x 79, half of 283 x 159 trimmed to whole patches: the
		// fixture has no fine detail. Their labels, 7 px tall, sit in an 11 px strip.
		if sheet.width != 420 || sheet.height != 112 {
			t.Errorf("sheet %dx%d, want 420x112", sheet.width, sheet.height)
		}
		if got := magentaByThird(sheet); got != [3]bool{false, true, false} {
			t.Errorf("magenta by third %v", got)
		}
	})

	t.Run("fps mode shows the magenta frames at 5.0 and 5.25 s and where to continue", func(t *testing.T) {
		result, _ := watch(t, "claude-code", tools.WatchArgs{Source: inspection, Start: 4.5, FPS: 4, Frames: 6})
		fields(t, result.Meta, map[string]string{
			"times":              "[4.5,4.75,5,5.25,5.5,5.75]",
			"merged":             "[[1,2],[3,4],[5,6]]",
			"span":               "[4.5,6]",
			"next_start_seconds": "6",
		})
		if got := magentaByThird(pixels(t, result.Content[0])); got != [3]bool{false, true, false} {
			t.Errorf("magenta by third %v", got)
		}
	})

	t.Run("a crop follows one region: cells take its shape and what the whole frame's cells cost", func(t *testing.T) {
		result, _ := watch(t, "claude-code", tools.WatchArgs{Source: inspection, Start: 4.5, FPS: 4, Frames: 6, Crop: magenta})
		fields(t, result.Meta, map[string]string{
			"times":  "[4.5,4.75,5,5.25,5.5,5.75]",
			"merged": "[[1,2],[3,4],[5,6]]",
			"zoom":   "",
		})
		sheet := pixels(t, result.Content[0])
		// Three cells of 337 x 253: the 390 x 219 cell's pixels at the region's 4:3.
		if sheet.width != 1036 || sheet.height != 280 {
			t.Fatalf("sheet %dx%d, want 1036x280", sheet.width, sheet.height)
		}
		// Below the labels, the middle cell is all box (the crop holds nothing else) and the others none of it.
		share := func(cell int) float64 {
			count := 0
			for y := 30; y < 253; y++ {
				for x := cell * 339; x < cell*339+337; x++ {
					if isMagenta(sheet.rgb, (y*sheet.width+x)*3) {
						count++
					}
				}
			}
			return float64(count) / (223 * 337)
		}
		if shares := [3]float64{share(0), share(1), share(2)}; shares[1] <= 0.99 || shares[0]+shares[2] != 0 {
			t.Errorf("magenta shares by cell %v", shares)
		}
	})

	t.Run("a small crop is enlarged in its cells, and lines up across the sheet", func(t *testing.T) {
		// A corner of the label, unchanged all video: one cell for all twelve samples.
		result, _ := watch(t, "claude-code", tools.WatchArgs{Source: inspection, Frames: 12, Crop: &[4]float64{0.05, 0.1, 0.1, 0.05}})
		fields(t, result.Meta, map[string]string{"zoom": "2.3", "merged": "[[1,12]]"})
		if got := media.CropCellSize(12, [2]int{1280, 720}, 128, 36); got != [2]int{292, 82} {
			t.Errorf("crop cell %v, want [292 82]", got)
		}
		if sheet := pixels(t, result.Content[0]); sheet.width != 308 || sheet.height != 112 {
			t.Errorf("sheet %dx%d, want 308x112", sheet.width, sheet.height)
		}
		// A sidebar lines up six to a row, a strip across the frame one.
		hd := [2]int{1920, 1080}
		for _, c := range []struct {
			samples, width, height, want int
		}{
			{12, 259, 821, 6},
			{12, 1920, 162, 1},
			{3, 960, 540, media.ColumnsFor(3)},
		} {
			if got := media.CropColumns(c.samples, media.CropCellSize(c.samples, hd, c.width, c.height)[0]); got != c.want {
				t.Errorf("%d crops of %dx%d: %d columns, want %d", c.samples, c.width, c.height, got, c.want)
			}
		}
	})

	t.Run("a timestamp listed by watch_video is that same frame in inspect_frame", func(t *testing.T) {
		for _, args := range []tools.WatchArgs{
			{Source: inspection, FPS: 4, Frames: 12},
			{Source: fx.long, FPS: 6, Frames: 12, Start: 3},
			{Source: fx.long, Frames: 12, Start: 10, End: ptr(22)},
			{Source: fx.long, Frames: 12},
			{Source: fx.transport, Frames: 12},
			{Source: fx.transport, Frames: 12, Start: 10, End: ptr(22)},
			{Source: fx.transport, FPS: 6, Frames: 12, Start: 3},
		} {
			result, _ := watch(t, "claude-code", args)
			for _, at := range times(t, result.Meta) {
				frame, err := tools.Default.Frame(fx.call(t, "claude-code"), tools.FrameArgs{Source: args.Source, At: at})
				if err != nil {
					t.Fatalf("inspect_frame at %v: %v", at, err)
				}
				if got := rustle.Round(number(t, frame.Meta, "at")*100) / 100; got != at {
					t.Errorf("%s %+v: listed %v, inspect_frame shows %v", filepath.Base(args.Source), args, at, got)
				}
			}
		}
	})

	t.Run("overview samples stay inside their share of the span, in order", func(t *testing.T) {
		result, _ := watch(t, "claude-code", tools.WatchArgs{Source: fx.long, Frames: 12})
		listed := times(t, result.Meta)
		spacing := (40 - 1.0/30) / 11
		if len(listed) != 12 {
			t.Fatalf("%d times, want 12", len(listed))
		}
		widest := 0.0
		for i, at := range listed {
			if math.Abs(at-float64(i)*spacing) > spacing/2+0.01 {
				t.Errorf("sample %d at %v", i+1, at)
			}
			if i > 0 {
				widest = max(widest, at-listed[i-1])
			}
		}
		if widest >= spacing*2 {
			t.Errorf("samples %v leave a gap of %v", listed, widest)
		}
	})

	t.Run("a detailed 1080p overview of twelve frames keeps near-full cells for at most 779 tokens", func(t *testing.T) {
		// Grain makes the test pattern read as camera footage, whose cells never shrink.
		hd := filepath.Join(fx.dir, "hd.mp4")
		ffmpeg(t, "-f", "lavfi", "-i", "testsrc2=size=1920x1080:rate=30:duration=6", "-vf", "noise=alls=30:allf=t",
			"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", hd)
		result, _ := watch(t, "claude-code", tools.WatchArgs{Source: hd, Frames: 12})
		sheet := pixels(t, result.Content[0])
		if tokens := rustle.ImageTokens(sheet.width, sheet.height); tokens > 779 {
			t.Errorf("%d tokens", tokens)
		}
		// 283 px cells, 3% under a full row's 292, end the sheet on 41 patches instead of 42.
		if sheet.width != 1148 {
			t.Errorf("sheet %d px wide, want 1148", sheet.width)
		}
	})

	t.Run("listen adds the span's sound for clients that hear it; Codex images ask for original detail", func(t *testing.T) {
		result, _ := watch(t, "codex", tools.WatchArgs{Source: fx.long, Frames: 4, Detail: "none", Listen: "audio"})
		var kinds []string
		for _, b := range result.Content {
			kinds = append(kinds, b.Type)
		}
		if want := []string{"image", "audio"}; !reflect.DeepEqual(kinds, want) {
			t.Fatalf("blocks %v, want %v", kinds, want)
		}
		if result.Content[1].MimeType != "audio/flac" {
			t.Errorf("audio as %s", result.Content[1].MimeType)
		}
		sound, _ := result.Meta.Get("sound")
		if span, _ := sound.(*rustle.Obj).Get("span"); !reflect.DeepEqual(span, []float64{0, 40}) {
			t.Errorf("sound span %v", span)
		}
		if want := map[string]any{"codex/imageDetail": "original"}; !reflect.DeepEqual(result.Content[0].Meta, want) {
			t.Errorf("image _meta %v", result.Content[0].Meta)
		}
		_, silent := watch(t, "claude-code", tools.WatchArgs{Source: inspection, Frames: 2, Detail: "none", Listen: "analysis"})
		if !slices.Contains(silent.Notes.List(), "listen ignored: no audio track") {
			t.Errorf("notes %q", silent.Notes.List())
		}
	})

	t.Run("listening caps the span at a minute, and the frames and speech share it", func(t *testing.T) {
		long := filepath.Join(fx.dir, "minutes.mp4")
		ffmpeg(t, "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=10:duration=150", "-f", "lavfi", "-i", "sine=frequency=440:duration=150",
			"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", long)
		result, c := watch(t, "claude-code", tools.WatchArgs{Source: long, Start: 30, Frames: 4, Detail: "none", Listen: "analysis"})
		fields(t, result.Meta, map[string]string{"span": "[30,90]", "next_start_seconds": "90"})
		for _, at := range times(t, result.Meta) {
			if at < 30 || at >= 90 {
				t.Errorf("a frame at %v, outside the span", at)
			}
		}
		if kinds := []string{result.Content[0].Type, result.Content[len(result.Content)-1].MimeType}; !reflect.DeepEqual(kinds, []string{"image", "image/png"}) {
			t.Errorf("blocks %v: want a sheet, then the spectrogram", kinds)
		}
		if !slices.ContainsFunc(c.Notes.List(), func(n string) bool { return strings.Contains(n, "at most 60 s") }) {
			t.Errorf("notes %q", c.Notes.List())
		}
	})

	t.Run("each sheet is followed by what is said in its frames, under their labels", func(t *testing.T) {
		talk := filepath.Join(fx.dir, "talk.mp4")
		data, err := os.ReadFile(fx.long)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(talk, data, 0o644); err != nil {
			t.Fatal(err)
		}
		srt := "1\n00:00:01,000 --> 00:00:03,000\nRed comes first.\n\n" +
			"2\n00:00:16,000 --> 00:00:18,000\nThen green.\n\n" +
			"3\n00:00:31,000 --> 00:00:33,000\nBlue comes last.\n"
		if err := os.WriteFile(filepath.Join(fx.dir, "talk.srt"), []byte(srt), 0o644); err != nil {
			t.Fatal(err)
		}
		result, _ := watch(t, "claude-code", tools.WatchArgs{Source: talk, Frames: 4, Detail: "full", Language: "auto"})
		listed := times(t, result.Meta)
		if len(result.Content) != 2 || result.Content[0].Type != "image" || result.Content[1].Type != "text" {
			t.Fatalf("content %+v", result.Content)
		}
		label := func(n int) string {
			return media.Label(media.Cell{First: n, Last: n, From: listed[n-1], To: listed[n-1]})
		}
		// Frames at about 0, 13, 27 and 40 s: green is said after the second, blue after the
		// third, and nothing after the fourth, which the text leaves out.
		want := strings.Join([]string{label(1), "0:01 Red comes first.", label(2), "0:16 Then green.", label(3), "0:31 Blue comes last."}, "\n")
		if got := result.Content[1].Text; got != want {
			t.Errorf("text\n%s\nwant\n%s", got, want)
		}
		fields(t, result.Meta, map[string]string{"captions": `"sidecar"`})

		// With a query, the frames are where it is said.
		found, _ := watch(t, "claude-code", tools.WatchArgs{Source: talk, Frames: 12, Query: "blue", Language: "auto"})
		at := times(t, found.Meta)
		if len(at) != 1 || at[0] < 30.9 || at[0] > 31 {
			t.Errorf("frames at %v, want one at 31 s", at)
		}
		if text := found.Content[1].Text; !strings.HasPrefix(text, "#1 0:3") || !strings.Contains(text, "Blue comes last.") {
			t.Errorf("text %q", text)
		}
		fields(t, found.Meta, map[string]string{"matches": "1"})

		// When a page of speech fills before the span ends, the frames end there too.
		c := fx.call(t, "claude-code")
		c.Settings.TranscriptChars = 30
		paged, err := tools.Default.Watch(c, tools.WatchArgs{Source: talk, Frames: 4, Detail: "full", Language: "auto"})
		if err != nil {
			t.Fatal(err)
		}
		fields(t, paged.Meta, map[string]string{"span": "[0,16]", "next_start_seconds": "16"})
		for _, at := range times(t, paged.Meta) {
			if at >= 16 {
				t.Errorf("a frame at %v, past the page", at)
			}
		}
		if text := paged.Content[1].Text; !strings.Contains(text, "Red") || strings.Contains(text, "green") {
			t.Errorf("text %q", text)
		}
	})

	t.Run("watching a video longer than ten minutes tells the agent to watch it in parts", func(t *testing.T) {
		eleven := filepath.Join(fx.dir, "eleven.mp4")
		ffmpeg(t, "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=2:duration=660", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", eleven)
		hint := func(args tools.WatchArgs) string {
			result, _ := watch(t, "claude-code", args)
			text, _ := result.Meta.Get("hint")
			s, _ := text.(string)
			return s
		}
		watching := tools.WatchArgs{Source: eleven, FPS: 1, Frames: tools.MaxFrames, Detail: "none"}
		if got := hint(watching); !strings.Contains(got, "11 minutes remain") || !strings.Contains(got, "2 parts of 10 minutes") {
			t.Errorf("hint %q", got)
		}
		// An overview of it says how far apart its frames are, and how to watch it all.
		overview := tools.WatchArgs{Source: eleven, Frames: 12, Detail: "none"}
		if got := hint(overview); !strings.Contains(got, "a frame every 55 s") || !strings.Contains(got, "2 parts of 10 minutes") {
			t.Errorf("overview hint %q", got)
		}
		// A part with an end, or a rest under ten minutes, is watched here.
		part := watching
		part.End = ptr(600)
		late := watching
		late.Start = 120
		for _, args := range []tools.WatchArgs{part, late} {
			if got := hint(args); got != "" {
				t.Errorf("%+v: hint %q", args, got)
			}
		}
	})

	t.Run("a repeated call is answered from memory with its notes", func(t *testing.T) {
		args := tools.WatchArgs{Source: fx.long, Start: 100, Frames: 3}
		a, first := watch(t, "claude-code", args)
		b, again := watch(t, "claude-code", args)
		// The same image, not an equal one: nothing was decoded or encoded again.
		if unsafe.StringData(b.Content[0].Data) != unsafe.StringData(a.Content[0].Data) {
			t.Error("the image was made again")
		}
		if rustle.Compact(b.Meta) != rustle.Compact(a.Meta) {
			t.Errorf("metadata %s, then %s", rustle.Compact(a.Meta), rustle.Compact(b.Meta))
		}
		if !reflect.DeepEqual(again.Notes.List(), first.Notes.List()) {
			t.Errorf("notes %q, then %q", first.Notes.List(), again.Notes.List())
		}
		if list := again.Notes.List(); len(list) == 0 || !strings.Contains(list[0], "past the end") {
			t.Errorf("notes %q", list)
		}
	})

	t.Run("an audio-only source with nothing else to show is refused with a next step", func(t *testing.T) {
		_, watchErr := tools.Default.Watch(fx.call(t, "claude-code"), tools.WatchArgs{Source: fx.beep, Frames: 12})
		_, frameErr := tools.Default.Frame(fx.call(t, "claude-code"), tools.FrameArgs{Source: fx.beep, At: 1})
		for _, err := range []error{watchErr, frameErr} {
			failure, ok := rustle.AsError(err)
			if !ok || failure.Code != "NO_VIDEO_TRACK" || !strings.Contains(failure.Recovery, "listen") {
				t.Errorf("error %#v", err)
			}
		}
		// With its sound asked for, it is heard, and the missing frames are a note.
		result, c := watch(t, "claude-code", tools.WatchArgs{Source: fx.beep, Frames: 12, Listen: "analysis"})
		if len(result.Content) != 1 || result.Content[0].MimeType != "image/png" {
			t.Errorf("content %+v", result.Content)
		}
		if !slices.Contains(c.Notes.List(), "audio only: no frames") {
			t.Errorf("notes %q", c.Notes.List())
		}
	})
}
