package tools

import (
	"encoding/json"
	"fmt"
	"math"
	"sync"

	"github.com/vibeslop/rustle-mcp/internal/media"
	"github.com/vibeslop/rustle-mcp/internal/rustle"
	"github.com/vibeslop/rustle-mcp/internal/source"
	"github.com/vibeslop/rustle-mcp/internal/youtube"
)

// Excerpts that come with a sheet: three short clips, or one clip in fps mode.
const (
	excerptSeconds  = 4
	fpsAudioSeconds = 20
	// windowSeconds: YouTube times this close share one downloaded media window.
	windowSeconds = 10
)

func hundredths(v float64) float64 { return rustle.Round(v*100) / 100 }

// frameStep is seconds per frame; 0.05 when the rate is unknown.
func frameStep(record *rustle.Record) float64 {
	if record.FPS > 0 {
		return 1 / record.FPS
	}
	return 0.05
}

// Span clamps [start, end] to the media. A start within `last` seconds of the end moves
// back to cover the last span of the requested length (end - start, else length, else
// all), so the call still shows something. end and length are 0 when not given.
func Span(duration, start float64, end *float64, notes *rustle.Notes, last, length float64) (float64, float64, error) {
	if end != nil && *end <= start {
		return 0, 0, rustle.Invalid(fmt.Sprintf("end (%s) must be greater than start (%s).", rustle.Short(*end), rustle.Short(start)))
	}
	if start < 0 {
		notes.Add("start " + rustle.Short(start) + " clamped to 0")
		start = 0
	}
	if start > math.Max(0, duration-last) {
		wanted := duration
		if end != nil {
			wanted = *end - start
		} else if length != 0 {
			wanted = length
		}
		moved := rustle.Round(math.Max(0, duration-wanted)*1000) / 1000
		notes.Add(fmt.Sprintf("start %s is past the end (%s s); showing %s-%s", rustle.Short(start), rustle.Short(duration), rustle.Short(moved), rustle.Short(duration)))
		return moved, duration, nil
	}
	if end != nil && *end > duration {
		notes.Add("end " + rustle.Short(*end) + " clamped to the duration " + rustle.Short(duration))
	}
	if end == nil || *end > duration {
		return start, duration, nil
	}
	return start, *end, nil
}

type Plan struct {
	Start, End float64
	Times      []float64
	// Covered is the end of the time the samples stand for; the next call can start here.
	Covered float64
	// FPS is the sample rate in fps mode, after capping at the video's own rate; 0 otherwise.
	FPS float64
}

// MakePlan is the sample times: evenly across the span including both ends, or
// consecutive at FPS.
func MakePlan(record *rustle.Record, args WatchArgs, notes *rustle.Notes) (*Plan, error) {
	duration := record.Duration
	step := frameStep(record)
	fps := args.FPS
	if fps > 0 && record.FPS > 0 && fps > record.FPS {
		// Sampling faster than the video changes frames only repeats them.
		notes.Add(fmt.Sprintf("fps %s is above the video's %s fps; used that", rustle.Short(fps), rustle.Short(record.FPS)))
		fps = record.FPS
	}
	length := 0.0
	if fps > 0 {
		length = float64(args.Frames) / fps
	}
	start, end, err := Span(duration, args.Start, args.End, notes, step, length)
	if err != nil {
		return nil, err
	}
	if fps > 0 {
		var times []float64
		for i := 0; i < args.Frames && start+float64(i)/fps < end-1e-9; i++ {
			times = append(times, start+float64(i)/fps)
		}
		if len(times) == 0 {
			times = []float64{start}
		}
		return &Plan{Start: start, End: end, Times: times, Covered: math.Min(end, start+float64(len(times))/fps), FPS: fps}, nil
	}
	count := args.Frames
	// More samples than the span has frames would show frames twice.
	if record.FPS > 0 && count > 1 {
		count = max(1, min(count, int(math.Floor((end-start)/step+1e-6))))
	}
	// The last sample is one frame before the end, so it is inside the last frame.
	return &Plan{Start: start, End: end, Times: rustle.Linspace(start, math.Max(start, end-step), count), Covered: end}, nil
}

// clusters are consecutive times within windowSeconds of a group's first time.
func clusters(times []float64) [][]float64 {
	var groups [][]float64
	for _, t := range times {
		if n := len(groups); n > 0 && t-groups[n-1][0] <= windowSeconds {
			groups[n-1] = append(groups[n-1], t)
		} else {
			groups = append(groups, []float64{t})
		}
	}
	return groups
}

func distinct(frames []*media.Frame) []*media.Frame {
	var out []*media.Frame
	for i, f := range frames {
		if i == 0 || math.Abs(f.At-frames[i-1].At) > 0.0005 {
			out = append(out, f)
		}
	}
	return out
}

// Shown keeps display-size frames watch_video decoded from small sources, by source and
// listed time. A small source fits inspect_frame's default edge whole, so inspecting a
// time watch_video listed, the usual next step, costs a PNG encode instead of a decoder
// process.
type Shown struct {
	mu     sync.Mutex
	frames map[string]*media.Frame
	order  []string
	bytes  int
	budget int
}

func NewShown(budget int) *Shown { return &Shown{frames: map[string]*media.Frame{}, budget: budget} }

func shownKey(source string, at float64) string {
	return fmt.Sprintf("%s@%d", source, int64(rustle.Round(at*100)))
}

func (s *Shown) Add(source string, frame *media.Frame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := shownKey(source, frame.At)
	if old := s.frames[key]; old != nil {
		s.bytes -= len(old.RGB)
		for i, k := range s.order {
			if k == key {
				s.order = append(s.order[:i], s.order[i+1:]...)
				break
			}
		}
	}
	s.frames[key] = frame
	s.order = append(s.order, key)
	s.bytes += len(frame.RGB)
	for s.bytes > s.budget && len(s.order) > 0 {
		oldest := s.order[0]
		s.order = s.order[1:]
		s.bytes -= len(s.frames[oldest].RGB)
		delete(s.frames, oldest)
	}
}

// Find is the kept frame listed as `at`; listed times are rounded to 0.01 s.
func (s *Shown) Find(source string, at float64) *media.Frame {
	s.mu.Lock()
	defer s.mu.Unlock()
	frame := s.frames[shownKey(source, at)]
	if frame != nil && math.Abs(frame.At-at) <= 0.0051 {
		return frame
	}
	return nil
}

var shown = NewShown(32 << 20)

// keepPixels is the pixels of display-size frames one watch_video call may keep for inspect_frame.
const keepPixels = 1_000_000

func realFrames(c *rustle.Call, record *rustle.Record, p *Plan, size [2]int, crop *[4]float64) ([]*media.Frame, error) {
	native := [2]int{record.Width, record.Height}
	spacing := p.Covered - p.Start
	if len(p.Times) > 1 {
		spacing = p.Times[1] - p.Times[0]
	}
	options := &media.SampleOptions{Settings: c.Settings, Spacing: spacing, Overview: p.FPS == 0, Floor: p.Start, Ceiling: p.End, OnFrame: media.Measurer()}
	frames := func(sampled []*media.Sampled) []*media.Frame {
		out := make([]*media.Frame, len(sampled))
		for i, s := range sampled {
			out[i] = &s.Frame
		}
		return out
	}
	if record.Kind != "youtube" {
		input, err := source.MediaInput(c, record, "video", p.Start, p.End)
		if err != nil {
			return nil, err
		}
		// Keeping adds about 3 ns a pixel to the decode: 1 ms for the fan-out clips' 8 frames
		// of 320x180, which saves a 40-80 ms decoder per inspected time, but 30 ms for 48
		// frames of 640x360. Above 100 fps two frames would share a listed time.
		full := crop == nil && len(p.Times)*native[0]*native[1] <= keepPixels && max(native[0], native[1]) <= rustle.FrameEdge && frameStep(record) > 0.0102
		src := &media.Input{Input: input.Input, Start: input.Start, Offset: input.Offset, Native: native, FPS: record.FPS, Duration: record.Duration, Full: full, Crop: crop}
		sampled, err := media.Sample(c.Ctx, src, p.Times, size, options)
		if err != nil {
			return nil, err
		}
		for _, f := range sampled {
			if f.Full != nil {
				shown.Add(record.Key, f.Full)
			}
		}
		return frames(sampled), nil
	}
	step := frameStep(record)
	parts, err := rustle.MapLimit(clusters(p.Times), 3, func(times []float64, _ int) ([]*media.Frame, error) {
		input, err := source.MediaInput(c, record, "video", times[0], math.Min(record.Duration, times[len(times)-1]+step))
		if err != nil {
			return nil, err
		}
		src := &media.Input{Input: input.Input, Start: input.Start, Offset: input.Offset, Native: native, FPS: record.FPS, Duration: record.Duration, Crop: crop}
		sampled, err := media.Sample(c.Ctx, src, times, size, options)
		return frames(sampled), err
	})
	if err != nil {
		return nil, err
	}
	var all []*media.Frame
	for _, part := range parts {
		all = append(all, part...)
	}
	return distinct(all), nil
}

// storyboard is storyboard thumbnails when YouTube has them at least as often as the samples.
func storyboard(c *rustle.Call, record *rustle.Record, p *Plan) []*media.Frame {
	if record.Kind != "youtube" || p.FPS > 0 {
		return nil
	}
	board, err := youtube.StoryboardInfo(c, record)
	spacing := p.Covered - p.Start
	if len(p.Times) > 1 {
		spacing = p.Times[1] - p.Times[0]
	}
	if err != nil || board == nil || spacing < board.Interval {
		return nil
	}
	frames, err := youtube.StoryboardFrames(c, record, p.Times)
	if err != nil || len(frames) == 0 {
		return nil
	}
	return distinct(frames)
}

func excerptSpans(p *Plan) [][2]float64 {
	if p.FPS > 0 {
		return [][2]float64{{p.Start, math.Min(p.Covered, p.Start+fpsAudioSeconds)}}
	}
	clips := 1
	if p.End-p.Start >= 3*excerptSeconds/2.0 {
		clips = 3
	}
	length := math.Min(excerptSeconds, (p.End-p.Start)/float64(clips))
	var spans [][2]float64
	for _, t := range rustle.Linspace(p.Start, p.End-length, clips) {
		spans = append(spans, [2]float64{t, math.Min(t+length, p.End)})
	}
	return spans
}

type sound struct {
	spans  [][2]float64
	blocks []rustle.Block
}

func excerpts(c *rustle.Call, record *rustle.Record, p *Plan) (*sound, error) {
	if !record.Audio() {
		c.Notes.Add("include_audio ignored: no audio track")
		return &sound{}, nil
	}
	type clip struct {
		span  [2]float64
		block rustle.Block
		ok    bool
	}
	clips, err := rustle.MapLimit(excerptSpans(p), 3, func(span [2]float64, _ int) (clip, error) {
		a, b := span[0], span[1]
		var flac []byte
		input, err := source.MediaInput(c, record, "audio", a, b)
		if err == nil {
			flac, err = media.EncodeFLAC(c.Ctx, input.Input, a, b, c.Settings, input.Offset)
		}
		if err != nil {
			failure, ok := rustle.AsError(err)
			if !ok {
				return clip{}, err
			}
			c.Notes.Add(fmt.Sprintf("audio %s-%s unavailable: %s", rustle.Short(a), rustle.Short(b), failure.Message))
			return clip{}, nil
		}
		return clip{span: [2]float64{hundredths(a), hundredths(b)}, block: rustle.Block{Type: "audio", Data: rustle.B64(flac), MimeType: "audio/flac"}, ok: true}, nil
	})
	if err != nil {
		return nil, err
	}
	out := &sound{}
	for _, c := range clips {
		if c.ok {
			out.spans = append(out.spans, c.span)
			out.blocks = append(out.blocks, c.block)
		}
	}
	return out, nil
}

// Recent keeps recent results by source and arguments. Agents repeat calls, and parallel
// agents ask for the same frames; a repeat costs nothing instead of a decode. Local
// sources are keyed by size and modification time, so a re-rendered file is decoded again.
type Recent struct {
	mu      sync.Mutex
	entries map[string]*recentEntry
	order   []string
	bytes   int
	budget  int
}

type recentEntry struct {
	result *rustle.Result
	notes  []string
	bytes  int
}

func NewRecent(budget int) *Recent {
	return &Recent{entries: map[string]*recentEntry{}, budget: budget}
}

// RecentKey keys a result by the source's record, the other arguments and the client's image needs.
func RecentKey(record *rustle.Record, args any, c *rustle.Call) string {
	data, _ := json.Marshal([]any{record.Key, args, c.Client.ImageMeta != nil})
	return string(data)
}

func (r *Recent) Get(key string, c *rustle.Call) *rustle.Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	hit := r.entries[key]
	if hit == nil {
		return nil
	}
	r.touch(key)
	c.Notes.Add(hit.notes...)
	return hit.result.Clone()
}

func (r *Recent) touch(key string) {
	for i, k := range r.order {
		if k == key {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
	r.order = append(r.order, key)
}

func (r *Recent) Set(key string, result *rustle.Result, notes []string) {
	bytes := 0
	for _, b := range result.Content {
		bytes += b.Size()
	}
	if bytes > r.budget/4 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if old := r.entries[key]; old != nil {
		r.bytes -= old.bytes
	}
	r.entries[key] = &recentEntry{result: result.Clone(), notes: notes, bytes: bytes}
	r.touch(key)
	r.bytes += bytes
	for r.bytes > r.budget && len(r.order) > 0 {
		oldest := r.order[0]
		r.order = r.order[1:]
		r.bytes -= r.entries[oldest].bytes
		delete(r.entries, oldest)
	}
}

var recentSheets = NewRecent(48 << 20)

// sourceless is the arguments with the source left out: the record's key stands for it.
func sourceless[T any](args T, clear func(*T)) T {
	clear(&args)
	return args
}

func watch(c *rustle.Call, args WatchArgs) (*rustle.Result, error) {
	id, err := source.Identify(args.Source, c.Settings)
	if err != nil {
		return nil, err
	}
	record, err := source.Resolve(c, id)
	if err != nil {
		return nil, err
	}
	if record, err = source.Probed(c, record); err != nil {
		return nil, err
	}
	if !record.Video() {
		return nil, rustle.NoVideoTrack(record.Title)
	}
	key := RecentKey(record, sourceless(args, func(a *WatchArgs) { a.Source = "" }), c)
	if cached := recentSheets.Get(key, c); cached != nil {
		return cached, nil
	}
	noted := c.Notes.Len()
	result, err := sheets(c, record, args)
	if err != nil {
		return nil, err
	}
	recentSheets.Set(key, result, c.Notes.Since(noted))
	return result, nil
}

func sheets(c *rustle.Call, record *rustle.Record, args WatchArgs) (*rustle.Result, error) {
	p, err := MakePlan(record, args, c.Notes)
	if err != nil {
		return nil, err
	}
	type audioResult struct {
		sound *sound
		err   error
	}
	var audio chan audioResult
	if args.IncludeAudio {
		audio = make(chan audioResult, 1)
		go func() {
			s, err := excerpts(c, record, p)
			audio <- audioResult{s, err}
		}()
	}

	native := [2]int{record.Width, record.Height}
	var region *[2]int
	if args.Crop != nil {
		px := media.CropPixels(native, *args.Crop)
		region = &[2]int{px[2], px[3]}
	}
	// Storyboard thumbnails are 160 px or so: a crop of one would show nothing more.
	var preview []*media.Frame
	if region == nil {
		preview = storyboard(c, record, p)
	}
	var size [2]int
	switch {
	case region != nil:
		size = media.CropCellSize(len(p.Times), native, region[0], region[1])
	case preview != nil:
		size = media.CellSize(len(p.Times), preview[0].Width, preview[0].Height)
	default:
		size = media.CellSize(len(p.Times), native[0], native[1])
	}
	var frames []*media.Frame
	if preview != nil {
		for _, f := range preview {
			fit := media.Fit(f.Width, f.Height, size[0], size[1])
			frames = append(frames, media.Resize(f, fit[0], fit[1]))
		}
	} else if frames, err = realFrames(c, record, p, size, args.Crop); err != nil {
		return nil, err
	}
	if len(frames) == 0 {
		return nil, rustle.Fail("No frames could be decoded in this span.", "DECODE_FAILED", "Try another span, or check that the file plays.")
	}
	if len(frames) < len(p.Times) && p.FPS == 0 {
		c.Notes.Add(fmt.Sprintf("the span holds %d distinct frames, not %d", len(frames), len(p.Times)))
	}

	columns := media.ColumnsFor(len(p.Times))
	if region != nil {
		columns = media.CropColumns(len(p.Times), size[0])
	}
	cells, cell := media.Group(frames), size
	// A crop asks for detail, so its cells aren't shrunk for showing little of it.
	if region == nil {
		cells, cell = media.Adapt(cells, columns, size)
	}
	var content []rustle.Block
	for _, sheet := range media.Compose(cells, columns, cell) {
		data, mimeType := media.EncodeSheet(sheet)
		content = append(content, rustle.Block{Type: "image", Data: rustle.B64(data), MimeType: mimeType, Meta: c.Client.ImageMeta})
	}
	var sounded *sound
	if audio != nil {
		result := <-audio
		if result.err != nil {
			return nil, result.err
		}
		sounded = result.sound
		content = append(content, sounded.blocks...)
	}

	var merged [][2]int
	for _, cell := range cells {
		if cell.Last > cell.First {
			merged = append(merged, [2]int{cell.First, cell.Last})
		}
	}
	meta := rustle.NewObj()
	// A local path already names the file; gaps between samples are in times.
	if record.Kind != "local" {
		meta.Set("title", record.Title)
	}
	meta.Set("duration", record.Duration).Set("size", []int{record.Width, record.Height})
	if region != nil && size[0] > region[0] {
		meta.Set("zoom", rustle.Round(float64(size[0])/float64(region[0])*10)/10)
	}
	if !(p.Start == 0 && p.Covered == record.Duration) {
		meta.Set("span", []float64{p.Start, p.Covered})
	}
	times := make([]float64, len(frames))
	for i, f := range frames {
		times[i] = hundredths(f.At)
	}
	meta.Set("times", times)
	if len(merged) > 0 {
		pairs := make([]any, len(merged))
		for i, m := range merged {
			pairs[i] = []int{m[0], m[1]}
		}
		meta.Set("merged", pairs)
	}
	if p.Covered < record.Duration-frameStep(record) {
		meta.Set("next_start_seconds", p.Covered)
	}
	if preview != nil {
		meta.Set("preview", "YouTube storyboard thumbnails; inspect_frame shows full resolution")
	}
	if sounded != nil && len(sounded.spans) > 0 {
		meta.Set("audio", sounded.spans)
	}
	return &rustle.Result{Meta: meta, Content: content}, nil
}
