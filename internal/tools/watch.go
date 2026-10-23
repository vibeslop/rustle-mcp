package tools

import (
	"strings"

	"encoding/json"
	"fmt"
	"math"
	"sync"

	"github.com/vibeslop/rustle-mcp/internal/mcp"
	"github.com/vibeslop/rustle-mcp/internal/media"
	"github.com/vibeslop/rustle-mcp/internal/rustle"
	"github.com/vibeslop/rustle-mcp/internal/source"
	"github.com/vibeslop/rustle-mcp/internal/youtube"
)

// windowSeconds: YouTube times this close share one downloaded media window.
const windowSeconds = 10

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
	// Exact plans put frames at moments, where a query is said, and keep them there.
	Exact bool
}

// spacing is the time between samples; the shortest gap when they are uneven.
func (p *Plan) spacing() float64 {
	if len(p.Times) < 2 {
		return p.Covered - p.Start
	}
	gap := p.Times[1] - p.Times[0]
	if p.Exact {
		for i := 2; i < len(p.Times); i++ {
			gap = math.Min(gap, p.Times[i]-p.Times[i-1])
		}
	}
	return gap
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
	options := &media.SampleOptions{Settings: c.Settings, Spacing: p.spacing(), Overview: p.FPS == 0 && !p.Exact, Floor: p.Start, Ceiling: p.End, OnFrame: media.Measurer()}
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
	// Cells come from YouTube's smaller format; a crop asks for detail, so it reads the sharp one.
	track := "sheet"
	if crop != nil {
		track = "video"
	}
	parts, err := rustle.MapLimit(clusters(p.Times), 3, func(times []float64, _ int) ([]*media.Frame, error) {
		input, err := source.MediaInput(c, record, track, times[0], math.Min(record.Duration, times[len(times)-1]+step))
		if err != nil {
			return nil, err
		}
		picture := native
		if input.Size[0] > 0 && input.Size[1] > 0 {
			picture = input.Size
		}
		src := &media.Input{Input: input.Input, Start: input.Start, Offset: input.Offset, Native: picture, FPS: record.FPS, Duration: record.Duration, Crop: crop}
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
	if err != nil || board == nil || p.spacing() < board.Interval {
		return nil
	}
	frames, err := youtube.StoryboardFrames(c, record, p.Times)
	if err != nil || len(frames) == 0 {
		return nil
	}
	return distinct(frames)
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

// outcome is what one part of a watch delivered, and the notes it made.
type outcome[T any] struct {
	value T
	err   error
	notes []string
}

// beside runs one part of a watch while the others run. Each part keeps its own notes,
// added in a fixed order once all are done, so a result reads the same however they raced.
func beside[T any](c *rustle.Call, work func(*rustle.Call) (T, error)) <-chan outcome[T] {
	done := make(chan outcome[T], 1)
	go func() {
		own := *c
		own.Notes = &rustle.Notes{}
		value, err := work(&own)
		done <- outcome[T]{value, err, own.Notes.List()}
	}()
	return done
}

// reach is where a span ends, +Inf for the end of the media.
func reach(end *float64, duration float64) float64 {
	if end == nil || (duration > 0 && *end >= duration) {
		return math.Inf(1)
	}
	return *end
}

// watch is what is shown, said and heard in one span: contact sheets, each followed by
// what is said in its frames, then the sound. A part that fails while another delivers
// leaves a note; the call fails only when nothing can be shown.
func watch(c *rustle.Call, args WatchArgs) (*rustle.Result, error) {
	id, err := source.Identify(args.Source, c.Settings)
	if err != nil {
		return nil, err
	}
	resolved, err := source.Resolve(c, id)
	if err != nil {
		return nil, err
	}
	reading := TranscriptArgs{Source: args.Source, Start: args.Start, End: args.End, Query: args.Query,
		Language: args.Language, Transcribe: args.Transcribe, Detail: args.Detail}
	read := func(c *rustle.Call, record *rustle.Record, reading TranscriptArgs) (*spoken, error) {
		return speak(c, record, reading, func(track rustle.Caption) (string, error) { return youtube.CaptionText(c, record, track) })
	}
	// A reading alone needs no media, so it never starts ffprobe or ffmpeg.
	if args.Frames == 0 && args.Listen == "" {
		said, err := read(c, resolved, reading)
		if err != nil {
			return nil, err
		}
		return &rustle.Result{Meta: said.meta, Content: []rustle.Block{rustle.TextBlock(said.text())}}, nil
	}
	wantSpeech := args.Detail != "none" || args.Query != ""
	// Captions load while the media is probed. Reading them again for the final span takes
	// a millisecond; transcribing again would not, so transcription waits for the span.
	var early <-chan outcome[*spoken]
	if wantSpeech && !args.Transcribe {
		early = beside(c, func(c *rustle.Call) (*spoken, error) { return read(c, resolved, reading) })
	}
	record, err := source.Probed(c, resolved)
	if err != nil {
		return nil, err
	}
	if args.Listen != "" && reach(args.End, record.Duration)-args.Start > ListenSeconds && args.Start+ListenSeconds < record.Duration {
		end := args.Start + ListenSeconds
		args.End = &end
		c.Notes.Add("the span is at most 60 s with listen; continue from next_start_seconds")
	}
	if args.Frames > 0 && !record.Video() {
		c.Notes.Add("audio only: no frames")
	}
	if args.Listen != "" && !record.Audio() {
		if args.Frames == 0 && !wantSpeech {
			return nil, rustle.NoAudioTrack(record.Title)
		}
		c.Notes.Add("listen ignored: no audio track")
		args.Listen = ""
	}
	// speech is the reading of [from, to) with no line spanning a break: the early one when
	// it read that same span without breaks.
	speech := func(from, to float64, breaks []float64) outcome[*spoken] {
		final := reading
		final.Start, final.End, final.Breaks = from, &to, breaks
		if early != nil {
			got := <-early
			early = nil
			// A failed read fails for any span; its notes were about the span it read.
			if got.err != nil {
				got.notes = nil
				return got
			}
			if reading.Start == from && reach(reading.End, record.Duration) == reach(&to, record.Duration) && len(breaks) == 0 {
				return got
			}
		}
		return <-beside(c, func(c *rustle.Call) (*spoken, error) { return read(c, record, final) })
	}

	showing := args.Frames > 0 && record.Video()
	var said outcome[*spoken]
	var plan *Plan
	if showing && args.Query == "" {
		if plan, err = MakePlan(record, args, c.Notes); err != nil {
			return nil, err
		}
	} else {
		start, end, err := Span(record.Duration, args.Start, args.End, c.Notes, frameStep(record), 0)
		if err != nil {
			return nil, err
		}
		plan = &Plan{Start: start, End: end, Covered: end}
		// The frames go where the query is said, so the speech comes first.
		if showing {
			said = speech(start, end, nil)
			plan = atMatches(record, plan, said.value, args.Frames, c.Notes)
		}
	}
	// The parts get copies: the plan may change once the speech is read.
	crop := args.Crop
	show := func(plan Plan) <-chan outcome[*rustle.Result] {
		if !showing || len(plan.Times) == 0 {
			return nil
		}
		return beside(c, func(c *rustle.Call) (*rustle.Result, error) { return look(c, record, &plan, crop) })
	}
	pictures := show(*plan)
	var sound <-chan outcome[*rustle.Result]
	if args.Listen != "" {
		hear := ListenArgs{Source: args.Source, Start: plan.Start, Seconds: plan.Covered - plan.Start, Output: args.Listen}
		sound = beside(c, func(c *rustle.Call) (*rustle.Result, error) { return listen(c, hear) })
	}
	hint := ""
	switch {
	case showing && plan.FPS > 0 && args.End == nil:
		hint = splitting(plan, record.Duration)
	case showing && plan.FPS == 0 && !plan.Exact:
		hint = overlooking(plan)
	}
	var shown, heard outcome[*rustle.Result]
	if pictures != nil {
		shown = <-pictures
	}
	// Speech is laid out once the frames are known, so that a line never spans two of them:
	// overview frames land on keyframes, off the planned times.
	if wantSpeech && !(showing && args.Query != "") {
		said = speech(plan.Start, plan.Covered, breaks(shown.value))
		// A page of speech that filled before the span ended ends the span there, frames and
		// all. The new frames break the speech elsewhere, which can move the page's end again.
		for tries := 0; tries < 3 && pictures != nil && said.value != nil && said.value.next != nil && *said.value.next < plan.Covered; tries++ {
			cut := *said.value.next
			args.End = &cut
			if plan, err = MakePlan(record, args, &rustle.Notes{}); err != nil {
				return nil, err
			}
			shown = <-show(*plan)
			said = speech(plan.Start, plan.Covered, breaks(shown.value))
		}
	}
	if sound != nil {
		heard = <-sound
	}
	c.Notes.Add(shown.notes...)
	c.Notes.Add(said.notes...)
	c.Notes.Add(heard.notes...)
	result, err := present(c, record, plan, args, shown, said, heard)
	if err == nil && hint != "" {
		result.Meta.Set("hint", hint)
	}
	return result, err
}

// partSeconds is how much of a long video one subagent watches: five calls and fifty
// sheets, half the images one conversation may hold.
const partSeconds = 600

// splitting tells the agent to watch a long video in parts, by subagents in parallel:
// watched a second at a time in one conversation, nine minutes of animation already filled
// 56,000 tokens of context and fifty images. Shorter rests are watched here.
func splitting(plan *Plan, duration float64) string {
	rest := duration - plan.Start
	if rest <= partSeconds {
		return ""
	}
	return fmt.Sprintf("%d minutes remain: watching them here takes %d calls like this one and would fill your context. %s "+
		"Only without subagents, watch here, or pass frames for an overview.",
		int(math.Ceil(rest/60)), int(math.Ceil(rest/float64(MaxFrames))), inParts(plan.Start, rest))
}

// overlooking tells the agent what an overview of a long span leaves out, and to watch it
// all before describing what it shows. Asked what an hour-long episode shows, agents went
// straight to an overview of 40 frames, one every 104 s; told how to watch it all, as an
// option, they answered from the overview in 2 runs of 2.
func overlooking(plan *Plan) string {
	span := plan.End - plan.Start
	if span <= partSeconds || len(plan.Times) == 0 {
		return ""
	}
	return fmt.Sprintf("An overview: a frame every %d s, so most of what is shown is missing. Before describing what the video "+
		"shows, watch all %d minutes: %s Only without subagents, answer from this overview and say what it may miss.",
		int(math.Round(span/float64(len(plan.Times)))), int(math.Ceil(span/60)), inParts(plan.Start, span))
}

// inParts is how to watch `length` seconds from start: ten-minute parts, each by its own
// subagent on a small model, all at once.
func inParts(start, length float64) string {
	return fmt.Sprintf("split them into %d parts of 10 minutes (start_seconds %s, %s and so on, each with end_seconds 600 later) "+
		"and give each part to a subagent on your smallest model (Haiku, Flash, Luna or similar), in parallel. Each watches its "+
		"part and reports what is shown and said, with times.",
		int(math.Ceil(length/partSeconds)), rustle.Short(start), rustle.Short(start+partSeconds))
}

// present lays out what the parts of a watch delivered, or fails with the first part's
// error when none did.
func present(c *rustle.Call, record *rustle.Record, plan *Plan, args WatchArgs, shown outcome[*rustle.Result], said outcome[*spoken], heard outcome[*rustle.Result]) (*rustle.Result, error) {
	errs := []error{shown.err, said.err, heard.err}
	if args.Frames > 0 && !record.Video() {
		errs[0] = rustle.NoVideoTrack(record.Title)
	}
	for _, err := range errs {
		if err == nil {
			continue
		}
		// YouTube refusing any part sends the whole call to the backup, which it may let through.
		if youtube.Refused(err) && c.Settings.Backup != "" && !mcp.Forwarded(c.Ctx) {
			return nil, err
		}
		// Only Rustle's own errors describe a part that is missing; anything else is a bug.
		if _, ok := rustle.AsError(err); !ok {
			return nil, err
		}
	}
	if shown.value == nil && said.value == nil && heard.value == nil {
		for _, err := range errs {
			if err != nil {
				return nil, err
			}
		}
		return nil, rustle.Fail("Nothing to show in this span.", "DECODE_FAILED", "Try another span.")
	}
	if failure, ok := rustle.AsError(shown.err); ok {
		c.Notes.Add("no frames: " + failure.Message)
	}
	if failure, ok := rustle.AsError(said.err); ok {
		note := "no speech: " + failure.Message
		if failure.Code == "NO_CAPTIONS" && SpeechAvailable() {
			note += " transcribe=true transcribes it."
		}
		c.Notes.Add(note)
	}
	if failure, ok := rustle.AsError(heard.err); ok {
		c.Notes.Add("no sound: " + failure.Message)
	}

	meta := rustle.NewObj()
	// A local path already names the file.
	if record.Kind != "local" {
		meta.Set("title", record.Title)
	}
	meta.Set("duration", record.Duration)
	if !(plan.Start == 0 && plan.Covered >= record.Duration) {
		meta.Set("span", []float64{plan.Start, plan.Covered})
	}
	if plan.Covered < record.Duration-frameStep(record) {
		meta.Set("next_start_seconds", plan.Covered)
	}
	if shown.value != nil {
		for _, key := range shown.value.Meta.Keys() {
			value, _ := shown.value.Meta.Get(key)
			meta.Set(key, value)
		}
	}
	if said.value != nil {
		for _, key := range []string{"language", "captions", "matches", "digest", "chapters"} {
			if value, ok := said.value.meta.Get(key); ok {
				meta.Set(key, value)
			}
		}
	}
	var content []rustle.Block
	switch {
	case shown.value != nil && said.value != nil && len(said.value.lines) > 0:
		content = interleave(shown.value, said.value)
	case shown.value != nil:
		content = append(content, shown.value.Content...)
		if said.value != nil {
			content = append(content, rustle.TextBlock(said.value.text()))
		}
	case said.value != nil:
		content = append(content, rustle.TextBlock(said.value.text()))
	}
	if heard.value != nil {
		sound := heard.value.Meta.Clone()
		sound.Delete("next_start_seconds")
		meta.Set("sound", sound)
		content = append(content, heard.value.Content...)
	}
	return &rustle.Result{Meta: meta, Content: content}, nil
}

// breaks are the times of a sheet's frames after the first, less the rounding of their
// listed times, so that speech starting at a frame goes with it.
func breaks(sheets *rustle.Result) []float64 {
	if sheets == nil {
		return nil
	}
	cells := cellsOf(sheets.Meta)
	var at []float64
	for i := 1; i < len(cells); i++ {
		at = append(at, cells[i].From-0.005)
	}
	return at
}

// atMatches puts a plan's frames where each match is said, at most `frames` of them.
func atMatches(record *rustle.Record, p *Plan, said *spoken, frames int, notes *rustle.Notes) *Plan {
	exact := &Plan{Start: p.Start, End: p.End, Covered: p.Covered, Exact: true}
	if said == nil {
		return exact
	}
	last := math.Max(p.Start, p.End-frameStep(record))
	for _, at := range said.at {
		t := math.Min(math.Max(at, p.Start), last)
		if n := len(exact.Times); n == 0 || t > exact.Times[n-1] {
			exact.Times = append(exact.Times, t)
		}
	}
	if len(exact.Times) > frames {
		notes.Add(fmt.Sprintf("frames at the first %d of %d matches; raise frames for more", frames, len(exact.Times)))
		exact.Times = exact.Times[:frames]
	}
	return exact
}

// interleave follows each sheet with what is said in its frames: each frame's label, then
// the lines that start between it and the next frame. A frame nobody speaks over is left
// out: the sheet labels it, and a second apart most frames are. With a query the frames
// are at the matches, so each line comes under the frame of its match.
func interleave(sheets *rustle.Result, said *spoken) []rustle.Block {
	cells := cellsOf(sheets.Meta)
	lines := make([][]string, len(cells))
	for i, line := range said.lines {
		owner := 0
		for k, cell := range cells {
			// Listed times are rounded to hundredths (see breaks).
			if cell.From-0.005 <= said.at[i] {
				owner = k
			}
		}
		lines[owner] = append(lines[owner], line)
	}
	var out []rustle.Block
	for s, sheet := range sheets.Content {
		out = append(out, sheet)
		first := s * media.SheetCells
		var text []string
		for k := first; k < min(len(cells), first+media.SheetCells); k++ {
			if len(lines[k]) > 0 {
				text = append(text, media.Label(cells[k]))
				text = append(text, lines[k]...)
			}
		}
		if len(text) > 0 {
			out = append(out, rustle.TextBlock(strings.Join(text, "\n")))
		}
	}
	return out
}

// cellsOf is the cells of sheets, from the times and merged runs they list.
func cellsOf(meta *rustle.Obj) []media.Cell {
	value, _ := meta.Get("times")
	times, _ := value.([]float64)
	runs := map[int]int{}
	if value, ok := meta.Get("merged"); ok {
		list, _ := value.([]any)
		for _, run := range list {
			if pair, ok := run.([]int); ok {
				runs[pair[0]] = pair[1]
			}
		}
	}
	var cells []media.Cell
	for i := 0; i < len(times); {
		first := i + 1
		last := max(first, runs[first])
		cells = append(cells, media.Cell{First: first, Last: last, From: times[first-1], To: times[last-1]})
		i = last
	}
	return cells
}

// look is the contact sheets of a plan; a repeated call is answered from memory.
func look(c *rustle.Call, record *rustle.Record, p *Plan, crop *[4]float64) (*rustle.Result, error) {
	key := RecentKey(record, []any{p, crop}, c)
	if cached := recentSheets.Get(key, c); cached != nil {
		return cached, nil
	}
	noted := c.Notes.Len()
	result, err := sheets(c, record, p, crop)
	if err != nil {
		return nil, err
	}
	recentSheets.Set(key, result, c.Notes.Since(noted))
	return result, nil
}

func sheets(c *rustle.Call, record *rustle.Record, p *Plan, crop *[4]float64) (*rustle.Result, error) {
	native := [2]int{record.Width, record.Height}
	var region *[2]int
	if crop != nil {
		px := media.CropPixels(native, *crop)
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
	columns := media.ColumnsFor(len(p.Times))
	// A crop asks for detail, so its cells aren't shrunk to save patches.
	if region != nil {
		columns = media.CropColumns(len(p.Times), size[0])
	} else {
		size = media.FitPatches(size, len(p.Times), columns)
	}
	var frames []*media.Frame
	var err error
	if preview != nil {
		for _, f := range preview {
			fit := media.Fit(f.Width, f.Height, size[0], size[1])
			frames = append(frames, media.Resize(f, fit[0], fit[1]))
		}
	} else if frames, err = realFrames(c, record, p, size, crop); err != nil {
		return nil, err
	}
	if len(frames) == 0 {
		return nil, rustle.Fail("No frames could be decoded in this span.", "DECODE_FAILED", "Try another span, or check that the file plays.")
	}
	if len(frames) < len(p.Times) && p.FPS == 0 && !p.Exact {
		c.Notes.Add(fmt.Sprintf("the span holds %d distinct frames, not %d", len(frames), len(p.Times)))
	}

	cells, cell := media.Group(frames), size
	// A crop asks for detail, so its cells aren't shrunk for showing little of it.
	if region == nil {
		cells, cell = media.Adapt(cells, columns, size)
	}
	var content []rustle.Block
	for _, sheet := range media.Compose(cells, columns, cell) {
		data, mimeType := media.EncodeSheet(sheet)
		content = append(content, image(data, mimeType, c))
	}
	var merged [][2]int
	for _, cell := range cells {
		if cell.Last > cell.First {
			merged = append(merged, [2]int{cell.First, cell.Last})
		}
	}
	meta := rustle.NewObj().Set("size", []int{record.Width, record.Height})
	if region != nil && size[0] > region[0] {
		meta.Set("zoom", rustle.Round(float64(size[0])/float64(region[0])*10)/10)
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
	if preview != nil {
		meta.Set("preview", "YouTube storyboard thumbnails; inspect_frame shows full resolution")
	}
	return &rustle.Result{Meta: meta, Content: content}, nil
}
