package youtube

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/vibeslop/rustle-mcp/internal/fetch"
	"github.com/vibeslop/rustle-mcp/internal/proc"
	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

// Window is a local file holding part of a track.
type Window struct {
	// Input is a file ffmpeg can open (an init segment and media segments), inside the cache.
	Input string
	// Offset is the timeline time of the file's first timestamp: timeline = (pts - file
	// start_time) + offset.
	Offset float64
}

// chunk: larger single requests are throttled to playback speed (16 MB took 39 s, 8 MB
// 0.66 s), the reason yt-dlp also fetches YouTube in 10 MiB chunks.
const chunk = 8 << 20

const streamsFile = "yt/streams.json"

// attempts: three identities; at 2-3 in 10 limited, all three are about 1 in 50.
const attempts = 3

// inUse: files this recent are never evicted, so a window another call is about to decode survives.
const inUse = 2 * time.Minute

// downloads: an overview of many spread-out frames fetches many segments; a few
// connections at a time is as fast and stays polite.
var downloads = proc.NewSemaphore(6)

var state = struct {
	sync.Mutex
	streams map[string]*Streams
	indexes map[string]*DashIndex
	// segments are the runs being fetched, by segment path.
	segments map[string]*segmentRun
}{streams: map[string]*Streams{}, indexes: map[string]*DashIndex{}, segments: map[string]*segmentRun{}}

var (
	streamsFlight rustle.Flight[*Streams]
	indexFlight   rustle.Flight[*DashIndex]
	joinFlight    rustle.Flight[struct{}]
)

type segmentRun struct {
	done chan struct{}
	err  error
}

// stale is a googlevideo URL that stopped working (expired, or refused); new URLs may fix it.
type stale struct{ status int }

func (s *stale) Error() string { return fmt.Sprintf("HTTP %d", s.status) }

// Media is a local file holding at least [start, end] of the video's chosen video or audio
// track, fetched by timestamp so a long video is never downloaded whole. Segments are
// cached per source, so nearby and repeated requests reuse them.
func Media(c *rustle.Call, record *rustle.Record, track string, start, end float64) (*Window, error) {
	dir, err := c.Cache.Dir(record.Key)
	if err != nil {
		return nil, err
	}
	dir = filepath.Join(dir, "yt")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return rustle.Detach(c.Ctx, func(ctx context.Context) (*Window, error) {
		shared := *c
		shared.Ctx = ctx
		return rerouted(func() (*Window, error) { return fetchWindow(&shared, record, track, start, end, dir) })
	})
}

// fetchWindow replaces URLs that stop working, each time under a fresh visitor identity.
func fetchWindow(c *rustle.Call, record *rustle.Record, track string, start, end float64, dir string) (*Window, error) {
	for attempt := 0; ; attempt++ {
		streams, err := currentStreams(c, record, attempt > 0)
		if err != nil {
			return nil, err
		}
		format := ChooseFormat(streams.Formats, track)
		if format == nil {
			return nil, unavailable("no " + track + " stream is offered for this video")
		}
		window, err := openWindow(c, record, format, track, start, end, dir)
		var refused *stale
		if !errors.As(err, &refused) {
			return window, err
		}
		if attempt+1 < attempts {
			continue
		}
		if refused.status == 403 {
			return nil, accessDenied("the media server answered HTTP 403 (" + streams.Client + ")")
		}
		return nil, rustle.Fail("YouTube ended the playback session.", "PLAYBACK_REFRESH",
			"Retry the call; Rustle starts a new playback session. "+recovery).Retry(0)
	}
}

func openWindow(c *rustle.Call, record *rustle.Record, format *Format, track string, start, end float64, dir string) (*Window, error) {
	tag := strconv.Itoa(format.Itag) + "-" + format.LMT
	index, err := indexOf(c, record, format, tag)
	if err != nil {
		return nil, err
	}
	duration := record.Duration
	if duration == 0 {
		duration = format.Duration
	}
	from := math.Min(math.Max(0, start), duration)
	to := math.Min(math.Max(from, end), duration)
	// Audio decoders need a few packets of pre-roll, so a window starting on a segment
	// boundary also takes the segment before it; video segments start on keyframes.
	lead := from
	if track == "audio" {
		lead = math.Max(0, from-0.1)
	}
	a, b := Covering(index.Segments, lead, to)
	size := index.Segments[b].To - index.Segments[a].From + 1
	budget := c.Settings.CacheBytes / 2
	if size > budget {
		return nil, rustle.Fail(fmt.Sprintf("That span needs %d MB of media, more than the cache holds.", int64(rustle.Round(float64(size)/1e6))), "SPAN_TOO_LONG",
			"Request a shorter span or fewer, more spread-out times.")
	}
	segment := func(i int) string { return filepath.Join(dir, fmt.Sprintf("%s-%d.%s", tag, i, format.Container)) }
	fetched, err := segments(format, index, a, b, segment)
	if err != nil {
		return nil, err
	}
	if fetched {
		prune(dir, budget)
	}
	// The timeline starts at the first frame, as it does for a whole file. YouTube's files
	// start at 0; files with B-frame delay and no edit list start a few frames later.
	offset := rustle.Round((index.Segments[a].Start-index.Segments[0].Start)*1e6) / 1e6
	if a == b {
		return &Window{Input: segment(a), Offset: offset}, nil
	}
	// A span of several segments is one more file: ffmpeg's concat protocol is outside
	// the protocol whitelist that keeps inputs from reaching other files.
	path := filepath.Join(dir, fmt.Sprintf("%s-%d-%d.%s", tag, a, b, format.Container))
	if !fresh(path) {
		_, err := joinFlight.Do(c.Ctx, path, func() (struct{}, error) {
			parts := [][]byte{index.Init}
			for i := a; i <= b; i++ {
				data, err := os.ReadFile(segment(i))
				if err != nil {
					return struct{}{}, &stale{0}
				}
				parts = append(parts, data[min(len(data), len(index.Init)):])
			}
			return struct{}{}, rustle.WriteAtomic(path, bytes.Join(parts, nil))
		})
		if err != nil {
			return nil, err
		}
		prune(dir, budget)
	}
	return &Window{Input: path, Offset: offset}, nil
}

var watchID = regexp.MustCompile(`[?&]v=([\w-]{11})`)

// currentStreams is streams from memory, disk or the resolved record; after a refusal,
// from a new player request under a fresh visitor id, which also becomes the id later
// resolutions use.
func currentStreams(c *rustle.Call, record *rustle.Record, refresh bool) (*Streams, error) {
	now := time.Now().UnixMilli()
	if !refresh {
		state.Lock()
		known := state.streams[record.Key]
		state.Unlock()
		if known == nil || known.Expires <= now {
			var stored Streams
			if c.Cache.ReadJSON(record.Key, streamsFile, &stored) {
				known = &stored
			}
		}
		if known == nil || known.Expires <= now {
			known = streamsOf(record)
		}
		if known != nil && known.Expires > now {
			state.Lock()
			state.streams[record.Key] = known
			state.Unlock()
			return known, nil
		}
	}
	return streamsFlight.Do(c.Ctx, "streams:"+record.Key, func() (*Streams, error) {
		// Shared work: one caller's cancellation must not fail the others.
		shared := c.Shared()
		id := ""
		if record.YouTube != nil {
			id = record.YouTube.ID
		}
		if id == "" {
			if m := watchID.FindStringSubmatch(record.Source); m != nil {
				id = m[1]
			}
		}
		streams, err := func() (*Streams, error) {
			if refresh {
				if _, err := VisitorData(shared, true); err != nil {
					return nil, err
				}
			}
			resolved, err := Resolve(shared, id, record.Key)
			if err != nil {
				return nil, err
			}
			return streamsOf(resolved), nil
		}()
		if err != nil {
			if e, ok := rustle.AsError(err); ok && !bytes.Contains([]byte(e.Recovery), []byte("read_transcript")) {
				wrapped := *e
				wrapped.Recovery = e.Recovery + " " + recovery
				return nil, &wrapped
			}
			return nil, err
		}
		if streams == nil {
			return nil, unavailable("no format with a byte index is offered for this video")
		}
		state.Lock()
		state.streams[record.Key] = streams
		state.Unlock()
		_ = c.Cache.WriteJSON(record.Key, streamsFile, streams)
		return streams, nil
	})
}

// streamsOf is the record's chosen video and audio formats; a dubbed video lists 80 audio formats.
func streamsOf(record *rustle.Record) *Streams {
	yt := record.YouTube
	if yt == nil {
		return nil
	}
	all := ParseFormats(yt.Formats)
	var formats []Format
	for _, track := range []string{"video", "audio"} {
		if f := ChooseFormat(all, track); f != nil {
			formats = append(formats, *f)
		}
	}
	if len(formats) == 0 {
		return nil
	}
	expires := yt.ExpiresAt
	if expires == 0 {
		expires = record.ResolvedAt + int64(6*time.Hour/time.Millisecond)
	}
	// Ten minutes of margin, so a window is never fetched with a URL about to expire.
	return &Streams{Client: yt.Client, Expires: expires - int64(10*time.Minute/time.Millisecond), Formats: formats}
}

// indexOf is the init segment and segment table: bytes [0, end of index], one request,
// kept on disk.
func indexOf(c *rustle.Call, record *rustle.Record, format *Format, tag string) (*DashIndex, error) {
	key := record.Key + "/" + tag
	state.Lock()
	known := state.indexes[key]
	state.Unlock()
	if known != nil {
		return known, nil
	}
	return indexFlight.Do(c.Ctx, key, func() (*DashIndex, error) {
		name := "yt/" + tag + ".head"
		head := c.Cache.Read(record.Key, name)
		if float64(len(head)) != format.IndexEnd+1 {
			var err error
			if head, err = byteRange(format, 0, int64(format.IndexEnd)); err != nil {
				return nil, err
			}
			_, _ = c.Cache.Write(record.Key, name, head)
		}
		var index *DashIndex
		var err error
		if format.Container == "mp4" {
			index, err = ParseMP4Index(head, int(format.InitEnd))
		} else {
			index, err = ParseWebMIndex(head, int(format.InitEnd), int64(format.Length))
		}
		if err != nil {
			return nil, unavailable(fmt.Sprintf("the %s index of itag %d is unreadable", format.Container, format.Itag))
		}
		state.Lock()
		if len(state.indexes) > 64 {
			clear(state.indexes)
		}
		state.indexes[key] = index
		state.Unlock()
		return index, nil
	})
}

// segments makes sure segments a..b exist as files, each a playable init + segment. Missing
// ones are fetched in contiguous runs; concurrent calls wait on the run that already covers
// a segment. It reports whether any were fetched.
func segments(format *Format, index *DashIndex, a, b int, path func(int) string) (bool, error) {
	var waits []*segmentRun
	var missing []int
	state.Lock()
	for i := a; i <= b; i++ {
		if running := state.segments[path(i)]; running != nil {
			waits = append(waits, running)
		} else if !fresh(path(i)) {
			missing = append(missing, i)
		}
	}
	for _, run := range Runs(missing) {
		work := &segmentRun{done: make(chan struct{})}
		for _, i := range run {
			state.segments[path(i)] = work
		}
		waits = append(waits, work)
		go func(run []int) {
			work.err = fetchRun(format, index, run, path)
			state.Lock()
			for _, i := range run {
				delete(state.segments, path(i))
			}
			state.Unlock()
			close(work.done)
		}(run)
	}
	state.Unlock()
	var first error
	for _, w := range waits {
		<-w.done
		if w.err != nil && first == nil {
			first = w.err
		}
	}
	return len(missing) > 0, first
}

func fetchRun(format *Format, index *DashIndex, run []int, path func(int) string) error {
	from := index.Segments[run[0]].From
	to := index.Segments[run[len(run)-1]].To
	var chunks [][2]int64
	for at := from; at <= to; at += chunk {
		chunks = append(chunks, [2]int64{at, min(to, at+chunk-1)})
	}
	parts, err := rustle.MapLimit(chunks, 3, func(r [2]int64, _ int) ([]byte, error) { return byteRange(format, r[0], r[1]) })
	if err != nil {
		return err
	}
	body := bytes.Join(parts, nil)
	for _, i := range run {
		s := index.Segments[i]
		data := append(append([]byte(nil), index.Init...), body[s.From-from:s.To-from+1]...)
		if err := rustle.WriteAtomic(path(i), data); err != nil {
			return err
		}
	}
	return nil
}

// byteRange is bytes [from, to] of a format. Callers' cancellation is not passed in: the
// work is shared.
func byteRange(format *Format, from, to int64) ([]byte, error) {
	size := to - from + 1
	response, err := proc.Use(downloads, context.Background(), func() (*fetch.Response, error) {
		return fetch.Get(context.Background(), fmt.Sprintf("%s&range=%d-%d", format.URL, from, to), fetch.Options{
			// Fails below 100 kB/s, far under what googlevideo serves (about 12 MB/s measured).
			What: "YouTube", Limit: size, Timeout: 15*time.Second + time.Duration(rustle.Round(float64(size)/100))*time.Millisecond,
			Allow: []int{401, 403, 404, 410, 416, 429, 500, 502, 503, 504},
		})
	})
	if err != nil {
		return nil, err
	}
	switch {
	case response.Status == 429:
		return nil, rateLimited()
	case response.Status >= 500:
		return nil, unavailable(fmt.Sprintf("HTTP %d", response.Status))
	case response.Status != 200 && response.Status != 206:
		return nil, &stale{response.Status}
	case int64(len(response.Body)) != size:
		return nil, &stale{0}
	}
	return response.Body, nil
}

// Runs is contiguous runs of sorted indexes: [3,4,5,9] -> [[3,4,5],[9]].
func Runs(items []int) [][]int {
	out := [][]int{}
	for _, i := range items {
		if n := len(out); n > 0 && out[n-1][len(out[n-1])-1] == i-1 {
			out[n-1] = append(out[n-1], i)
		} else {
			out = append(out, []int{i})
		}
	}
	return out
}

// fresh reports whether the file exists, and marks it used now for least-recently-used eviction.
func fresh(path string) bool {
	if _, err := os.Stat(path); err != nil {
		return false
	}
	now := time.Now()
	_ = os.Chtimes(path, now, now)
	return true
}

var mediaFile = regexp.MustCompile(`\.(mp4|webm)(\.\w+\.tmp)?$`)

// prune drops least recently used media files (and leftovers of interrupted writes) past
// the per-source budget.
func prune(dir string, budget int64) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type file struct {
		path  string
		bytes int64
		used  time.Time
	}
	var files []file
	var total int64
	for _, e := range entries {
		if !mediaFile.MatchString(e.Name()) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		files = append(files, file{path, info.Size(), info.ModTime()})
		total += info.Size()
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i].used.Before(files[j].used) })
	for _, f := range files {
		if total <= budget {
			break
		}
		if time.Since(f.used) < inUse {
			continue
		}
		os.Remove(f.path)
		total -= f.bytes
	}
}
