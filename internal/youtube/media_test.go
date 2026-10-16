package youtube

import (
	"encoding/binary"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vibeslop/rustle-mcp/internal/media"
	"github.com/vibeslop/rustle-mcp/internal/proc"
	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

// End to end against a stubbed YouTube: a synthetic DASH MP4 (H.264, 2 s fragments) and
// WebM (Opus, 5 s clusters) served by byte range, decoded with the system ffmpeg.

func dashFixtures(ffmpeg, dir string) (video, audio []byte, ok bool) {
	run := func(args ...string) error {
		return exec.Command(ffmpeg, append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)...).Run()
	}
	if run("-f", "lavfi", "-i", "testsrc2=size=320x180:rate=25", "-t", "30", "-c:v", "libx264", "-g", "50", "-keyint_min", "50",
		"-sc_threshold", "0", "-movflags", "+dash+global_sidx", "-f", "mp4", filepath.Join(dir, "v.mp4")) != nil {
		return nil, nil, false
	}
	if run("-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "30", "-c:a", "libopus", "-cluster_time_limit", "5000",
		"-reserve_index_space", "2048", "-f", "webm", filepath.Join(dir, "a.webm")) != nil {
		return nil, nil, false
	}
	video, err := os.ReadFile(filepath.Join(dir, "v.mp4"))
	if err != nil {
		return nil, nil, false
	}
	audio, err = os.ReadFile(filepath.Join(dir, "a.webm"))
	return video, audio, err == nil
}

// dashRanges is a file's [initEnd, indexEnd]: MP4 boxes up to sidx, or WebM elements up to Cues.
func dashRanges(t *testing.T, file []byte, mp4 bool) (int, int) {
	t.Helper()
	if mp4 {
		for at := 0; at+8 <= len(file); at += int(binary.BigEndian.Uint32(file[at:])) {
			if string(file[at+4:at+8]) == "sidx" {
				return at - 1, at + int(binary.BigEndian.Uint32(file[at:])) - 1
			}
		}
		t.Fatal("no sidx in the MP4 fixture")
	}
	readVint := func(at int, id bool) (value, length int) {
		length = 1
		for file[at]&(0x80>>(length-1)) == 0 {
			length++
		}
		value = int(file[at])
		if !id {
			value = int(file[at] & (0xff >> length))
		}
		for i := 1; i < length; i++ {
			value = value*256 + int(file[at+i])
		}
		return value, length
	}
	skip := func(at int) (data, size int) {
		_, il := readVint(at, true)
		size, sl := readVint(at+il, false)
		return at + il + sl, size
	}
	data, size := skip(0)
	segmentData, _ := skip(data + size)
	initEnd := 0
	for at := segmentData; at < len(file); {
		id, _ := readVint(at, true)
		data, size := skip(at)
		if id == 0x1654ae6b {
			initEnd = data + size - 1
		}
		if id == 0x1c53bb6b {
			return initEnd, data + size - 1
		}
		at = data + size
	}
	t.Fatal("no Cues in the WebM fixture")
	return 0, 0
}

// youtubeStub serves the player and the fixtures' byte ranges.
type youtubeStub struct {
	mu      sync.Mutex
	players int
	ranges  []string
	// fail are statuses for the next media range requests, consumed in order; -1 stands for
	// a connection that ends early with a 200.
	fail         []int
	playerStatus string
}

func (y *youtubeStub) handler(video, audio []byte, formats A) func(request) any {
	return func(r request) any {
		u, _ := url.Parse(r.URL)
		switch {
		case u.Path == "/sw.js_data":
			return `)]}'[[0,0,[[[0,0,0,0,0,0,0,0,0,0,0,0,0,"fresh"]]]]]`
		case u.Path == "/youtubei/v1/player":
			y.mu.Lock()
			y.players++
			status := y.playerStatus
			y.mu.Unlock()
			playability := M{"status": "OK"}
			if status != "" {
				playability = M{"status": status, "reason": botCheck}
			}
			return M{"playabilityStatus": playability, "streamingData": M{"expiresInSeconds": "21540", "adaptiveFormats": formats}}
		case strings.HasSuffix(u.Hostname(), "googlevideo.com"):
			bounds := strings.Split(u.Query().Get("range"), "-")
			from, _ := strconv.Atoi(bounds[0])
			to, _ := strconv.Atoi(bounds[1])
			itag := u.Query().Get("itag")
			y.mu.Lock()
			y.ranges = append(y.ranges, itag+":"+bounds[0]+"-"+bounds[1])
			status := 0
			if len(y.fail) > 0 {
				status, y.fail = y.fail[0], y.fail[1:]
			}
			y.mu.Unlock()
			file := audio
			if itag == "137" {
				file = video
			}
			switch status {
			case -1:
				return file[from:to]
			case 0:
				return file[from : to+1]
			}
			return reply{status: status}
		}
		return reply{status: 404}
	}
}

func (y *youtubeStub) counts() (players, ranges int) {
	y.mu.Lock()
	defer y.mu.Unlock()
	return y.players, len(y.ranges)
}

func (y *youtubeStub) failNext(statuses ...int) {
	y.mu.Lock()
	defer y.mu.Unlock()
	y.fail = append(y.fail, statuses...)
}

// calm drops failures a failed test left queued, so they don't spill into the next.
func (y *youtubeStub) calm() {
	y.mu.Lock()
	defer y.mu.Unlock()
	y.fail, y.playerStatus = nil, ""
}

func (y *youtubeStub) setPlayerStatus(status string) {
	y.mu.Lock()
	defer y.mu.Unlock()
	y.playerStatus = status
}

var windowRecords atomic.Int64

// windowRecord is a resolved video whose formats the media code has yet to ask for.
func windowRecord() *rustle.Record {
	n := strconv.FormatInt(windowRecords.Add(1), 10)
	return &rustle.Record{
		Key: strings.Repeat("a", 32-len(n)) + n, Kind: "youtube", Source: "https://www.youtube.com/watch?v=abcdefghijk",
		Title: "fixture", ResolvedAt: time.Now().UnixMilli(), Duration: 30, YouTube: &rustle.YtState{VisitorData: "visitor"},
	}
}

// windows opens the same track at each time at once.
func windows(t *testing.T, c *rustle.Call, record *rustle.Record, times ...float64) []*Window {
	t.Helper()
	out := make([]*Window, len(times))
	errs := make([]error, len(times))
	var wg sync.WaitGroup
	for i, at := range times {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i], errs[i] = Media(c, record, "video", at, at)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func TestYouTubeWindows(t *testing.T) {
	c := newCall(t)
	tools, err := media.ToolsFor(c.Settings)
	if err != nil {
		t.Skip("needs ffmpeg with libx264 and libopus")
	}
	video, audio, ok := dashFixtures(tools.FFmpeg, t.TempDir())
	if !ok {
		t.Skip("needs ffmpeg with libx264 and libopus")
	}
	// Frame decodes leave warm ffmpeg processes waiting on links in a temp folder.
	t.Cleanup(proc.Cleanup)
	format := func(itag int, file []byte, mimeType string, extra M) M {
		initEnd, indexEnd := dashRanges(t, file, strings.Contains(mimeType, "mp4"))
		return merge(M{
			"itag": itag, "mimeType": mimeType, "url": "https://rr1.googlevideo.com/videoplayback?itag=" + strconv.Itoa(itag), "contentLength": strconv.Itoa(len(file)),
			"initRange": byteSpan("0", strconv.Itoa(initEnd)), "indexRange": byteSpan(strconv.Itoa(initEnd+1), strconv.Itoa(indexEnd)),
			"lastModified": "1700000000000000", "approxDurationMs": "30000", "bitrate": 100_000,
		}, extra)
	}
	formats := A{
		format(137, video, `video/mp4; codecs="avc1.64000d"`, M{"width": 320, "height": 180, "fps": 25}),
		format(251, audio, `audio/webm; codecs="opus"`, M{"audioTrack": M{"id": "en.4", "audioIsDefault": true}}),
	}
	stub := &youtubeStub{}
	serve(t, stub.handler(video, audio, formats))

	decodeAt := func(t *testing.T, w *Window, at float64) float64 {
		t.Helper()
		info, err := media.ProbeInput(c.Ctx, w.Input, c.Settings)
		if err != nil {
			t.Fatal(err)
		}
		frame, err := media.DecodeFrame(c.Ctx, w.Input, media.FrameRequest{
			At: at, Native: [2]int{320, 180}, Size: [2]int{320, 180}, FPS: 25, Start: info.Start, Offset: w.Offset,
		}, c.Settings)
		if err != nil {
			t.Fatal(err)
		}
		return frame.At
	}

	t.Run("concurrent calls for one frame share one index and one segment request", func(t *testing.T) {
		r := windowRecord()
		opened := windows(t, c, r, 17, 17, 17)
		if opened[1].Input != opened[0].Input || opened[2].Input != opened[0].Input {
			t.Errorf("inputs %s, %s, %s", opened[0].Input, opened[1].Input, opened[2].Input)
		}
		if players, ranges := stub.counts(); players != 1 || ranges != 2 {
			t.Errorf("%d player requests and %d ranges, want 1 and 2", players, ranges)
		}
		if opened[0].Offset != 16 {
			t.Errorf("offset %v, want 16", opened[0].Offset)
		}
		if at := decodeAt(t, opened[0], 17.3); math.Abs(at-17.28) >= 0.001 {
			t.Errorf("frame at %v, want 17.28", at)
		}

		// Repeated and nearby calls are served from disk; a span reuses its cached segment.
		if _, err := Media(c, r, "video", 17.5, 17.9); err != nil {
			t.Fatal(err)
		}
		if _, ranges := stub.counts(); ranges != 2 {
			t.Errorf("%d ranges after a nearby call, want 2", ranges)
		}
		span, err := Media(c, r, "video", 15, 19)
		if err != nil {
			t.Fatal(err)
		}
		if _, ranges := stub.counts(); ranges != 4 {
			t.Errorf("%d ranges after a span, want 4", ranges)
		}
		if span.Offset != 14 {
			t.Errorf("span offset %v, want 14", span.Offset)
		}
		if at := decodeAt(t, span, 19.5); math.Abs(at-19.48) >= 0.001 {
			t.Errorf("frame at %v, want 19.48", at)
		}
	})

	t.Run("an audio window decodes at its timeline position", func(t *testing.T) {
		window, err := Media(c, windowRecord(), "audio", 12, 20)
		if err != nil {
			t.Fatal(err)
		}
		flac, err := media.EncodeFLAC(c.Ctx, window.Input, 12, 20, c.Settings, window.Offset)
		if err != nil {
			t.Fatal(err)
		}
		if window.Offset > 12 || window.Offset < 5 {
			t.Errorf("offset %v", window.Offset)
		}
		info, err := media.ProbeInput(c.Ctx, window.Input, c.Settings)
		if err != nil {
			t.Fatal(err)
		}
		if math.Abs(info.Start-window.Offset) >= 0.01 {
			t.Errorf("file starts at %v, offset %v", info.Start, window.Offset)
		}
		if len(flac) <= 1000 {
			t.Errorf("%d bytes of FLAC", len(flac))
		}
	})

	t.Run("a refused URL is replaced by a new player request", func(t *testing.T) {
		t.Cleanup(stub.calm)
		r := windowRecord()
		if _, err := Media(c, r, "video", 1, 1); err != nil {
			t.Fatal(err)
		}
		players, _ := stub.counts()
		stub.failNext(403)
		window, err := Media(c, r, "video", 25, 25)
		if err != nil {
			t.Fatal(err)
		}
		if now, _ := stub.counts(); now != players+1 {
			t.Errorf("%d player requests, want %d", now, players+1)
		}
		if window.Offset != 24 {
			t.Errorf("offset %v, want 24", window.Offset)
		}
	})

	t.Run("errors carry codes an agent can act on", func(t *testing.T) {
		t.Cleanup(stub.calm)
		stub.failNext(403, 403, 403)
		_, err := Media(c, windowRecord(), "video", 3, 3)
		if e := expectCode(t, err, "YOUTUBE_ACCESS_DENIED"); !strings.Contains(e.Recovery, "read_transcript") {
			t.Errorf("recovery %q", e.Recovery)
		}
		stub.failNext(429)
		_, err = Media(c, windowRecord(), "video", 3, 3)
		expectCode(t, err, "RATE_LIMITED")
		stub.failNext(503)
		_, err = Media(c, windowRecord(), "video", 3, 3)
		if e := expectCode(t, err, "UPSTREAM_UNAVAILABLE"); !e.Retryable {
			t.Error("503 is not retryable")
		}
		stub.failNext(-1, -1, -1)
		_, err = Media(c, windowRecord(), "video", 3, 3)
		if e := expectCode(t, err, "PLAYBACK_REFRESH"); !e.Retryable {
			t.Error("a cut connection is not retryable")
		}
		stub.setPlayerStatus("LOGIN_REQUIRED")
		_, err = Media(c, windowRecord(), "video", 3, 3)
		stub.setPlayerStatus("")
		if e := expectCode(t, err, "YOUTUBE_ACCESS_DENIED"); !strings.Contains(e.Recovery, "read_transcript") {
			t.Errorf("recovery %q", e.Recovery)
		}
	})

	t.Run("segments stay within half the cache budget, least recently used first", func(t *testing.T) {
		small := *c
		small.Settings = c.Settings.Copy()
		small.Settings.CacheBytes = 400_000
		r := windowRecord()
		_, err := Media(&small, r, "video", 0, 30)
		expectCode(t, err, "SPAN_TOO_LONG")
		old := windows(t, &small, r, 1, 5, 9, 13)
		hourAgo := time.Now().Add(-time.Hour)
		for i, w := range old {
			if err := os.Chtimes(w.Input, hourAgo, hourAgo.Add(time.Duration(i)*time.Millisecond)); err != nil {
				t.Fatal(err)
			}
		}
		latest, err := Media(&small, r, "video", 21, 21)
		if err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(filepath.Dir(latest.Input))
		if err != nil {
			t.Fatal(err)
		}
		var kept []string
		total := int64(0)
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".mp4") {
				info, err := e.Info()
				if err != nil {
					t.Fatal(err)
				}
				kept = append(kept, strconv.FormatInt(info.Size(), 10))
				total += info.Size()
			}
		}
		if total > 200_000 {
			t.Errorf("kept %s bytes", strings.Join(kept, "+"))
		}
		if _, err := os.Stat(latest.Input); err != nil {
			t.Errorf("the latest window is gone: %v", err)
		}
		if _, err := os.Stat(old[0].Input); err == nil {
			t.Error("the least recently used segment was kept")
		}
	})
}
