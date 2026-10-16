package youtube

import (
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vibeslop/rustle-mcp/internal/fetch"
	"github.com/vibeslop/rustle-mcp/internal/jsonv"
	"github.com/vibeslop/rustle-mcp/internal/media"
	"github.com/vibeslop/rustle-mcp/internal/proc"
	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

type Storyboard struct {
	// Size is one storyboard cell, e.g. [320, 180].
	Size [2]int
	// Interval is seconds between storyboard frames.
	Interval float64
}

type Level struct {
	// Index is the level's position among the spec's levels, the $L of sheet URLs.
	Index             int
	Width, Height     int
	Count, Cols, Rows int
	Interval          float64
	Name, Sigh        string
}

// ParseSpec reads a spec, "base URL|level|level...", each level
// "w#h#count#cols#rows#intervalMs#name#sigh". Level 0 is one evenly spaced sheet named
// "default" with interval 0; the others are time-based sheets named "M$M" (M0, M1, ...).
func ParseSpec(spec string) (string, []Level) {
	parts := strings.Split(spec, "|")
	var levels []Level
	for index, part := range parts[1:] {
		f := strings.Split(part, "#")
		field := func(i int) string {
			if i < len(f) {
				return f[i]
			}
			return ""
		}
		n := func(i int) float64 {
			if i >= len(f) {
				return math.NaN()
			}
			return rustle.Number(f[i])
		}
		levels = append(levels, Level{
			Index: index, Width: wholeInt(n(0)), Height: wholeInt(n(1)), Count: wholeInt(n(2)), Cols: wholeInt(n(3)), Rows: wholeInt(n(4)),
			Interval: n(5) / 1000, Name: field(6), Sigh: field(7),
		})
	}
	return parts[0], levels
}

// wholeInt is a spec number as a count; anything unreadable is 0, which no usable level has.
func wholeInt(v float64) int {
	if !rustle.Finite(v) {
		return 0
	}
	return int(v)
}

// Finest is the finest time-based level of a spec.
func Finest(spec string) (string, *Level) {
	if spec == "" {
		return "", nil
	}
	base, levels := ParseSpec(spec)
	var usable []Level
	for _, l := range levels {
		if l.Interval > 0 && l.Width > 0 && l.Height > 0 && l.Count > 0 && l.Cols > 0 && l.Rows > 0 && strings.Contains(l.Name, "$M") {
			usable = append(usable, l)
		}
	}
	sort.SliceStable(usable, func(i, j int) bool { return usable[i].Width*usable[i].Height > usable[j].Width*usable[j].Height })
	if base == "" || len(usable) == 0 {
		return "", nil
	}
	return base, &usable[0]
}

func SheetURL(base string, level *Level, sheet int) string {
	u := strings.Replace(base, "$L", strconv.Itoa(level.Index), 1)
	u = strings.Replace(u, "$N", strings.Replace(level.Name, "$M", strconv.Itoa(sheet), 1), 1)
	if level.Sigh != "" {
		return u + "&sigh=" + level.Sigh
	}
	return u
}

// FrameIndex is the storyboard frame nearest `time`. Frame k shows the picture at
// k × interval: on a video of a millisecond timer the cells read 00:05:000, 00:10:000,
// ... exactly. The count runs one past the end (ceil(duration / interval) + 1); that last
// frame shows the final picture instead of its own time, so it is never chosen.
func FrameIndex(level *Level, time, duration float64) int {
	last := level.Count - 1
	if duration > 0 {
		last = min(last, max(0, int(math.Ceil(duration/level.Interval))-1))
	}
	return int(math.Min(float64(last), math.Max(0, rustle.Round(time/level.Interval))))
}

var specs = struct {
	sync.Mutex
	// fetched are WEB specs learned during this process, by record key; the record copy
	// that holds them reaches later calls, and these reach the calls holding older copies.
	fetched map[string]string
	flight  rustle.Flight[string]
}{fetched: map[string]string{}}

// storyboardSpec is the finest spec known: the WEB player's (to 320x180), fetched once
// per video, else the resolving player's.
func storyboardSpec(c *rustle.Call, record *rustle.Record) (string, error) {
	yt := record.YouTube
	if yt == nil {
		return "", nil
	}
	if yt.WebFetched {
		return orSpec(yt.WebStoryboard, yt.Storyboard), nil
	}
	specs.Lock()
	known, ok := specs.fetched[record.Key]
	specs.Unlock()
	if ok {
		return orSpec(known, yt.Storyboard), nil
	}
	return specs.flight.Do(c.Ctx, record.Key, func() (string, error) {
		spec, err := webSpec(c, yt.ID)
		var chapters []rustle.Chapter
		if err != nil {
			if c.Ctx.Err() != nil {
				return "", rustle.Cancelled()
			}
			spec, chapters, err = watchSpec(c, yt.ID)
			if err != nil {
				if c.Ctx.Err() != nil {
					return "", rustle.Cancelled()
				}
				return yt.Storyboard, nil
			}
		}
		updated := record.Copy()
		updated.YouTube.WebStoryboard, updated.YouTube.WebFetched = spec, true
		if len(chapters) > 0 {
			updated.Chapters = chapters
		}
		specs.Lock()
		specs.fetched[record.Key] = spec
		specs.Unlock()
		rustle.SaveRecord(updated, c.Cache)
		return orSpec(spec, yt.Storyboard), nil
	})
}

func orSpec(spec, fallback string) string {
	if spec != "" {
		return spec
	}
	return fallback
}

// webSpec asks the WEB player, which answers with every storyboard level in ~100 ms; the
// field mask trims its 130-280 KB response to the spec. It refuses playback without a
// signature timestamp, but any value from roughly 200 days old onward is accepted, so a
// week-old day number stands in for the player script's own. "" when YouTube has none.
func webSpec(c *rustle.Call, id string) (string, error) {
	sts := time.Now().UnixMilli()/86_400_000 - 7
	visitor, err := VisitorData(c, false)
	if err != nil {
		return "", err
	}
	response, _, err := innertube(c.Ctx, "player", map[string]any{
		"videoId": id, "contentCheckOk": true, "racyCheckOk": true,
		"playbackContext": map[string]any{"contentPlaybackContext": map[string]any{"signatureTimestamp": sts}},
	}, innertubeOptions{client: "WEB", visitorData: visitor, fields: "playabilityStatus.status,storyboards"})
	if err != nil {
		return "", err
	}
	if jsonv.String(jsonv.Get(response, "playabilityStatus", "status")) != "OK" {
		return "", rustle.Fail("WEB player refused", "", "")
	}
	spec, _ := jsonv.Get(response, "storyboards", "playerStoryboardSpecRenderer", "spec").(string)
	return spec, nil
}

var specInPage = regexp.MustCompile(`"playerStoryboardSpecRenderer":\{"spec":"((?:[^"\\]|\\.)*)"`)

// watchSpec reads the watch page (1.5 MB, ~0.8 s), which has the same spec, and chapter
// markers worth keeping.
func watchSpec(c *rustle.Call, id string) (string, []rustle.Chapter, error) {
	response, err := fetch.Get(c.Ctx, Origin+"/watch?v="+id+"&hl=en", fetch.Options{
		Headers: map[string]string{"user-agent": fetch.BrowserUA, "accept-language": "en-US,en;q=0.9", "cookie": "SOCS=CAI"},
		Limit:   8 << 20, Timeout: 20 * time.Second, What: "YouTube",
	})
	if err != nil {
		return "", nil, err
	}
	html := string(response.Body)
	var chapters []rustle.Chapter
	const marker = "var ytInitialData = "
	if start := strings.Index(html, marker); start >= 0 {
		from := start + len(marker)
		if end := strings.Index(html[from:], ";</script>"); end >= 0 {
			if data, err := jsonv.Parse([]byte(html[from : from+end])); err == nil {
				chapters = MarkerChapters(data)
			}
		}
	}
	m := specInPage.FindStringSubmatch(html)
	if m == nil {
		if !strings.Contains(html, "ytInitialPlayerResponse") {
			return "", nil, rustle.Fail("no player response in the watch page", "", "")
		}
		return "", chapters, nil
	}
	var spec string
	if err := json.Unmarshal([]byte(`"`+m[1]+`"`), &spec); err != nil {
		return "", nil, err
	}
	return spec, chapters, nil
}

// StoryboardInfo is the finest storyboard level YouTube offers for this video, or nil
// when there is none.
func StoryboardInfo(c *rustle.Call, record *rustle.Record) (*Storyboard, error) {
	spec, err := storyboardSpec(c, record)
	if err != nil {
		return nil, err
	}
	if _, level := Finest(spec); level != nil {
		return &Storyboard{Size: [2]int{level.Width, level.Height}, Interval: level.Interval}, nil
	}
	return nil, nil
}

var ppmHeader = regexp.MustCompile(`^P6\s+(\d+)\s+(\d+)\s+255\s`)

// ReadPPM reads the packed RGB of a PPM (P6, 8-bit) image.
func ReadPPM(data []byte) (width, height int, rgb []byte, ok bool) {
	m := ppmHeader.FindSubmatch(data[:min(len(data), 64)])
	if m == nil {
		return 0, 0, nil, false
	}
	width, _ = strconv.Atoi(string(m[1]))
	height, _ = strconv.Atoi(string(m[2]))
	rgb = data[len(m[0]):]
	return width, height, rgb, len(rgb) == width*height*3
}

type sheetImage struct {
	width, height int
	rgb           []byte
}

// sheet decodes one storyboard sheet. Sheets are WebP behind a .jpg name, and the last
// one of a level is cropped to the frames it holds, so ffmpeg decodes whatever arrives
// to PPM, whose header carries the size.
func sheet(c *rustle.Call, record *rustle.Record, base string, level *Level, index int) (*sheetImage, error) {
	name := "storyboard-L" + strconv.Itoa(level.Index) + "-" + strconv.Itoa(index) + ".img"
	image := c.Cache.Read(record.Key, name)
	if len(image) == 0 {
		response, err := fetch.Get(c.Ctx, SheetURL(base, level, index), fetch.Options{Limit: 4 << 20, Timeout: 15 * time.Second, What: "YouTube"})
		if err != nil {
			return nil, err
		}
		image = response.Body
		_, _ = c.Cache.Write(record.Key, name, image)
	}
	tools, err := media.ToolsFor(c.Settings)
	if err != nil {
		return nil, err
	}
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-i", "pipe:0", "-frames:v", "1", "-c:v", "ppm", "-f", "image2pipe", "pipe:1"}
	result, err := proc.Use(media.Decoders(c.Settings), c.Ctx, func() (proc.Result, error) {
		return proc.Run(c.Ctx, tools.FFmpeg, args, proc.Options{Input: image, Timeout: 30 * time.Second, MaxBytes: 64 << 20})
	})
	if err != nil {
		return nil, err
	}
	if result.Code == 0 {
		if width, height, rgb, ok := ReadPPM(result.Stdout); ok {
			return &sheetImage{width, height, rgb}, nil
		}
	}
	return nil, rustle.Fail("A YouTube storyboard image could not be decoded.", "DECODE_FAILED", "Retry; frames decoded from the video itself still work.")
}

// StoryboardFrames is the storyboard frame nearest each requested time, as RGB. A frame's
// time is the storyboard frame's own, so consecutive requests that land on the same
// storyboard frame return it twice.
func StoryboardFrames(c *rustle.Call, record *rustle.Record, times []float64) ([]*media.Frame, error) {
	spec, err := storyboardSpec(c, record)
	if err != nil {
		return nil, err
	}
	base, level := Finest(spec)
	if level == nil {
		return nil, rustle.Fail("YouTube has no storyboard for this video.", "NO_STORYBOARD", "Decode frames from the video instead.")
	}
	perSheet := level.Cols * level.Rows
	indices := make([]int, len(times))
	var wanted []int
	seen := map[int]bool{}
	for i, t := range times {
		indices[i] = FrameIndex(level, t, record.Duration)
		if s := indices[i] / perSheet; !seen[s] {
			seen[s] = true
			wanted = append(wanted, s)
		}
	}
	sheets, err := rustle.MapLimit(wanted, 8, func(s int, _ int) (*sheetImage, error) { return sheet(c, record, base, level, s) })
	if err != nil {
		return nil, err
	}
	bySheet := map[int]*sheetImage{}
	for i, s := range wanted {
		bySheet[s] = sheets[i]
	}
	frames := make([]*media.Frame, len(indices))
	for i, k := range indices {
		img := bySheet[k/perSheet]
		cell := k % perSheet
		x := (cell % level.Cols) * level.Width
		y := (cell / level.Cols) * level.Height
		if x >= img.width || y >= img.height {
			return nil, rustle.Fail("A YouTube storyboard image is smaller than its spec says.", "DECODE_FAILED", "Retry; frames decoded from the video itself still work.")
		}
		width, height := min(level.Width, img.width-x), min(level.Height, img.height-y)
		out := make([]byte, width*height*3)
		for row := range height {
			from := ((y+row)*img.width + x) * 3
			copy(out[row*width*3:], img.rgb[from:from+width*3])
		}
		frames[i] = media.NewFrame(float64(k)*level.Interval, width, height, out)
	}
	return frames, nil
}
