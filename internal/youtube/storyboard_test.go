package youtube

import (
	"bytes"
	"encoding/json"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vibeslop/rustle-mcp/internal/media"
	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

// The spec of Ab3dEf6hIj0 (12:48) from the WEB player.
const webStoryboardSpec = "https://i.ytimg.com/sb/Ab3dEf6hIj0/storyboard3_L$L/$N.jpg?sqp=-oaymwENSDfyq4qpAwVwAcABBqLzl_8DBgissMvVBg==" +
	"|48#27#100#10#10#0#default#rs$AOn4CLA73s|80#45#155#10#10#5000#M$M#rs$AOn4CLCfwH|160#90#155#5#5#5000#M$M#rs$AOn4CLDNPJ" +
	"|320#180#155#3#3#5000#M$M#rs$AOn4CLBcun"

// storyboardRecord is a resolved video whose WEB spec hasn't been asked for.
func storyboardRecord(key string) *rustle.Record {
	return &rustle.Record{
		Key: key, Kind: "youtube", Source: "s", Title: "t", ResolvedAt: time.Now().UnixMilli(), Duration: 768.35,
		YouTube: &rustle.YtState{ID: "Ab3dEf6hIj0", VisitorData: "V", Client: "VISIONOS", Storyboard: vrSpec},
	}
}

func TestStoryboards(t *testing.T) {
	t.Run("specs parse into levels and sheet URLs", func(t *testing.T) {
		if _, levels := ParseSpec(webStoryboardSpec); len(levels) != 4 {
			t.Errorf("%d levels, want 4", len(levels))
		}
		base, level := Finest(webStoryboardSpec)
		if level == nil {
			t.Fatal("no finest level")
		}
		equal(t, []any{level.Index, level.Width, level.Height, level.Count, level.Cols, level.Rows, level.Interval},
			[]any{3, 320, 180, 155, 3, 3, 5.0}, "finest level")
		if got := SheetURL(base, level, 12); got != "https://i.ytimg.com/sb/Ab3dEf6hIj0/storyboard3_L3/M12.jpg?sqp=-oaymwENSDfyq4qpAwVwAcABBqLzl_8DBgissMvVBg==&sigh=rs$AOn4CLBcun" {
			t.Errorf("sheet URL %s", got)
		}
		// Level 0 is one evenly spaced sheet, not time-based.
		if _, level := Finest("https://x/$L/$N.jpg|48#27#100#10#10#0#default#rs$A"); level != nil {
			t.Errorf("level 0 chosen: %+v", level)
		}
		if _, level := Finest(""); level != nil {
			t.Errorf("a level from no spec: %+v", level)
		}
	})

	t.Run("frame k shows k × interval, and the trailing frame is never chosen", func(t *testing.T) {
		// A 10:00.066 millisecond-timer video (t653W89DGwg): 122 frames every 5 s; frame 121 repeats 10:00.
		_, timer := Finest("https://x/$L/$N|80#45#122#10#10#5000#M$M#rs$A")
		var got []int
		for _, at := range []float64{0, 2.4, 2.5, 7.6, 600.066, 9999, -3} {
			got = append(got, FrameIndex(timer, at, 600.066))
		}
		equal(t, got, []int{0, 0, 1, 2, 120, 120, 0}, "frame indexes")
		_, level := Finest(webStoryboardSpec)
		if k := FrameIndex(level, 768.35, 768.35); k != 153 {
			t.Errorf("frame %d at the end, want 153", k)
		}
		if k := FrameIndex(level, 1e9, 0); k != 154 {
			t.Errorf("frame %d past the end of an unknown duration, want 154", k)
		}
	})

	t.Run("PPM output is read with its size", func(t *testing.T) {
		rgb := []byte{1, 2, 3, 4, 5, 6}
		width, height, pixels, ok := ReadPPM(append([]byte("P6\n2 1\n255\n"), rgb...))
		equal(t, []any{width, height, pixels, ok}, []any{2, 1, rgb, true}, "PPM")
		if _, _, _, ok := ReadPPM([]byte("P6\n2 2\n255\n\x00")); ok {
			t.Error("a short PPM was read")
		}
	})

	t.Run("the WEB spec is fetched once and kept in the record", func(t *testing.T) {
		c := newCall(t)
		record := storyboardRecord("sb1")
		s := serve(t, func(r request) any {
			if isSwData(r) {
				return swData()
			}
			return M{"playabilityStatus": M{"status": "OK"}, "storyboards": M{"playerStoryboardSpecRenderer": M{"spec": webStoryboardSpec}}}
		})
		info, err := StoryboardInfo(c, record)
		if err != nil {
			t.Fatal(err)
		}
		equal(t, info, &Storyboard{Size: [2]int{320, 180}, Interval: 5}, "storyboard")
		posts := s.Posts()
		if len(posts) == 0 {
			t.Fatal("no player request")
		}
		post := posts[0]
		if post.client() != "WEB" {
			t.Errorf("client %q", post.client())
		}
		if mask := post.Header.Get("x-goog-fieldmask"); mask != "playabilityStatus.status,storyboards" {
			t.Errorf("field mask %q", mask)
		}
		sts, _ := dig(post.Body, "playbackContext", "contentPlaybackContext", "signatureTimestamp").(float64)
		if math.Abs(sts-float64(time.Now().UnixMilli())/86_400_000) >= 10 {
			t.Errorf("signature timestamp %v", sts)
		}
		// Records are never changed once published: the saved copy holds the spec, written to
		// disk once the call has moved on.
		rustle.FlushRecords()
		var saved rustle.Record
		if !c.Cache.ReadJSON("sb1", rustle.RecordFile, &saved) || saved.YouTube == nil || saved.YouTube.WebStoryboard != webStoryboardSpec {
			t.Errorf("saved record %+v", saved.YouTube)
		}
		if known := rustle.KnownRecord("sb1", c.Cache); known == nil || known.YouTube.WebStoryboard != webStoryboardSpec || !known.YouTube.WebFetched {
			t.Errorf("known record %+v", known)
		}
		s.Forget()
		for _, r := range []*rustle.Record{record, &saved} {
			if _, err := StoryboardInfo(c, r); err != nil {
				t.Fatal(err)
			}
		}
		if n := len(s.Calls()); n != 0 {
			t.Errorf("%d requests after the spec was fetched, want 0", n)
		}
	})

	t.Run("a refused WEB player falls back to the watch page, whose markers become chapters", func(t *testing.T) {
		c := newCall(t)
		record := storyboardRecord("sb2")
		record.Duration = 0
		record.Chapters = []rustle.Chapter{}
		var chapters A
		for i, ms := range []float64{0, 60_000, 120_000} {
			chapters = append(chapters, M{"chapterRenderer": M{"title": M{"simpleText": "Part " + strconv.Itoa(i)}, "timeRangeStartMillis": ms}})
		}
		initial := M{"playerOverlays": M{"playerOverlayRenderer": M{"decoratedPlayerBarRenderer": M{"decoratedPlayerBarRenderer": M{"playerBar": M{"multiMarkersPlayerBarRenderer": M{
			"markersMap": A{M{"key": "AUTO_CHAPTERS", "value": M{"chapters": chapters}}},
		}}}}}}}
		page := `<script>var ytInitialPlayerResponse = {"storyboards":{"playerStoryboardSpecRenderer":{"spec":` +
			strings.ReplaceAll(jsonText(t, webStoryboardSpec), "/", `\/`) + `}}};</script>` +
			`<script>var ytInitialData = ` + jsonText(t, initial) + `;</script>`
		s := serve(t, func(r request) any {
			switch {
			case isSwData(r):
				return swData()
			case r.Method == "POST":
				return M{"playabilityStatus": M{"status": "UNPLAYABLE"}}
			}
			return page
		})
		info, err := StoryboardInfo(c, record)
		if err != nil {
			t.Fatal(err)
		}
		equal(t, info, &Storyboard{Size: [2]int{320, 180}, Interval: 5}, "storyboard")
		if !slices.ContainsFunc(s.Calls(), func(r request) bool { return r.URL == "https://www.youtube.com/watch?v=Ab3dEf6hIj0&hl=en" }) {
			t.Error("the watch page was not fetched")
		}
		rustle.FlushRecords()
		known := rustle.KnownRecord("sb2", c.Cache)
		if known == nil {
			t.Fatal("no saved record")
		}
		equal(t, known.Chapters, []rustle.Chapter{{At: 0, Title: "Part 0"}, {At: 60, Title: "Part 1"}, {At: 120, Title: "Part 2"}}, "chapters")

		failing := record.Copy()
		failing.Key = "sb3"
		s = serve(t, func(request) any { return status(503) })
		info, err = StoryboardInfo(c, failing)
		if err != nil {
			t.Fatal(err)
		}
		equal(t, info, &Storyboard{Size: [2]int{80, 45}, Interval: 5}, "the resolving player's spec")
		// Tried again next time: nothing was kept.
		rustle.FlushRecords()
		if rustle.KnownRecord("sb3", c.Cache) != nil {
			t.Error("a failed fetch was saved")
		}
		s.Forget()
		if _, err := StoryboardInfo(c, failing); err != nil {
			t.Fatal(err)
		}
		if len(s.Calls()) == 0 {
			t.Error("the WEB spec was not asked for again")
		}
	})

	t.Run("frames are cut from decoded sheets at their own times", func(t *testing.T) {
		c := newCall(t)
		if _, err := media.ToolsFor(c.Settings); err != nil {
			t.Skip("ffmpeg not installed")
		}
		// 11 frames of 30x20, 3x3 per sheet: M0 is full, M1 holds the last two and is cropped to them.
		color := func(k int) [3]int { return [3]int{k * 20, 250 - k*20, 40} }
		makeSheet := func(first, count int) []byte {
			cols, rows := min(3, count), (count+2)/3
			rgb := make([]byte, cols*30*rows*20*3)
			for y := range rows * 20 {
				for x := range cols * 30 {
					k := first + y/20*3 + x/30
					at := (y*cols*30 + x) * 3
					for i, v := range color(k) {
						rgb[at+i] = byte(v)
					}
				}
			}
			jpeg, err := media.EncodeJPEGWithFFmpeg(c.Ctx, rgb, cols*30, rows*20, c.Settings, 1)
			if err != nil {
				t.Fatal(err)
			}
			return jpeg
		}
		sheets := [][]byte{makeSheet(0, 9), makeSheet(9, 2)}
		spec := "https://i.ytimg.com/sb/x/storyboard3_L$L/$N.jpg?sqp=a|30#20#11#3#3#5000#M$M#rs$abc"
		record := &rustle.Record{
			Key: "sb4", Kind: "youtube", Source: "s", Title: "t", ResolvedAt: time.Now().UnixMilli(), Duration: 52,
			YouTube: &rustle.YtState{ID: "x", WebStoryboard: spec, WebFetched: true},
		}
		sheetNumber := regexp.MustCompile(`M(\d+)\.jpg`)
		s := serve(t, func(r request) any {
			n, _ := strconv.Atoi(sheetNumber.FindStringSubmatch(r.URL)[1])
			return sheets[n]
		})
		frames, err := StoryboardFrames(c, record, []float64{0, 7.4, 12.6, 51, 47})
		if err != nil {
			t.Fatal(err)
		}
		var urls []string
		for _, call := range s.Calls() {
			urls = append(urls, call.URL)
		}
		slices.Sort(urls)
		equal(t, urls, []string{
			"https://i.ytimg.com/sb/x/storyboard3_L0/M0.jpg?sqp=a&sigh=rs$abc", "https://i.ytimg.com/sb/x/storyboard3_L0/M1.jpg?sqp=a&sigh=rs$abc",
		}, "sheet requests")
		var times []float64
		for _, f := range frames {
			times = append(times, f.At)
		}
		equal(t, times, []float64{0, 5, 15, 50, 45}, "frame times")
		for i, k := range []int{0, 1, 3, 10, 9} {
			f := frames[i]
			equal(t, []int{f.Width, f.Height, len(f.RGB)}, []int{30, 20, 1800}, "frame size")
			center := (10*30 + 15) * 3
			for channel, want := range color(k) {
				if got := int(f.RGB[center+channel]); got < want-12 || got > want+12 {
					t.Errorf("frame %d channel %d: %d vs %d", k, channel, got, want)
				}
			}
		}
		s.Forget()
		if _, err := StoryboardFrames(c, record, []float64{20}); err != nil {
			t.Fatal(err)
		}
		if n := len(s.Calls()); n != 0 {
			t.Errorf("%d requests, want 0: sheets are cached on disk", n)
		}
	})
}

// jsonText is v as JSON.stringify writes it: no HTML escapes.
func jsonText(t *testing.T, v any) string {
	t.Helper()
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(b.String(), "\n")
}
