package media

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vibeslop/rustle-mcp/internal/proc"
	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

// Many frames of one input, each scaled to a contact-sheet cell. Seeking is what costs:
// an accurate seek decodes from the previous keyframe, which in web video can be 8 s and
// 250 frames back. So far-apart overview samples take a keyframe near their time, close
// samples share sequential decodes, and only the rest pay for an accurate seek each.

// Input is what frames are decoded from.
type Input struct {
	Input string
	// Start is the format start time of the input (Probe.Start).
	Start float64
	// Offset is the timeline time of the input's first timestamp.
	Offset float64
	// Native is the display size of the video.
	Native [2]int
	FPS    float64
	// Duration is the timeline duration, 0 when unknown; times within two frames of it
	// look up the last frame first.
	Duration float64
	// Full also returns each frame at display size (Sampled.Full), decoded in the same pass.
	Full bool
	// Crop is [x, y, width, height] as fractions of the frame: cells show only this region.
	Crop *[4]float64
}

// Sampled is a decoded cell, with the frame at display size when Input.Full asked for it.
type Sampled struct {
	Frame
	Full *Frame
}

// tolerance: listed times are rounded to 0.01 s; a frame this close before a time counts as at it.
const tolerance = 0.0051

// runBudget is the frames one sequential pass may decode before its samples are split
// across parallel passes. Tuned on 1080p H.264 with 0.75 s and 8 s keyframe intervals:
// within 10% of the best plan.
const runBudget = 240

// keyframeSpacing: overview samples this far apart may move back to a keyframe.
const keyframeSpacing = 2

func frameStep(input *Input) float64 {
	if input.FPS > 0 {
		return 1 / input.FPS
	}
	return 0
}

// ToCell is the filters from a decoded frame to its cell. With source.Full the frame at
// display size sits above the cell, padded to its width, in one picture that `split` takes
// apart: one decode serves both, in RGB so odd cell sizes pad cleanly.
func ToCell(source *Input, size [2]int, tag string) string {
	region := ""
	if source.Crop != nil {
		// Fractions of the decoded size: the same share of the picture before an anamorphic
		// source is stretched to its display size, and fewer pixels for the scaler.
		c := source.Crop
		region = fmt.Sprintf("crop=%s*iw:%s*ih:%s*iw:%s*ih,", rustle.Fixed(c[2], 6), rustle.Fixed(c[3], 6), rustle.Fixed(c[0], 6), rustle.Fixed(c[1], 6))
	}
	cell := fmt.Sprintf("%sscale=%d:%d:flags=lanczos,setsar=1", region, size[0], size[1])
	if !source.Full {
		return cell
	}
	w, h := source.Native[0], source.Native[1]
	return fmt.Sprintf("scale=%d:%d,format=rgb24,setsar=1,split[full%s][small%s];[small%s]%s,pad=%d:%d[cell%s];[full%s][cell%s]vstack",
		w, h, tag, tag, tag, cell, w, size[1], tag, tag, tag)
}

func frameBytes(source *Input, size [2]int) int {
	if source.Full {
		return source.Native[0] * (source.Native[1] + size[1]) * 3
	}
	return size[0] * size[1] * 3
}

// picture is a decoded cell and, with Input.Full, the frame at display size above it. Its
// measures travel with it, so what OnFrame measured the sheet finds.
type picture struct {
	rgb  []byte
	full []byte
	memo *measures
}

// SampleOptions are how frames of one call are decoded.
type SampleOptions struct {
	Settings *rustle.Settings
	// OnFrame sees each cell as soon as it is decoded, while later ones still decode.
	OnFrame func(*Frame)
	// Spacing is the time between samples.
	Spacing float64
	// Overview lets a sample move up to half the spacing, staying inside [Floor, Ceiling],
	// to land on a keyframe: the frame is real and labelled with its own time, just off the grid.
	Overview bool
	Floor    float64
	Ceiling  float64
}

func split(data []byte, source *Input, size [2]int) picture {
	if !source.Full {
		return picture{rgb: data, memo: &measures{}}
	}
	w, h := source.Native[0], source.Native[1]
	width, height := size[0], size[1]
	rgb := make([]byte, width*height*3)
	for y := range height {
		copy(rgb[y*width*3:], data[(h+y)*w*3:((h+y)*w+width)*3])
	}
	return picture{rgb: rgb, full: data[:w*h*3], memo: &measures{}}
}

func sampled(p picture, at float64, source *Input, size [2]int) *Sampled {
	s := &Sampled{Frame: Frame{At: at, Width: size[0], Height: size[1], RGB: p.rgb, memo: p.memo}}
	if p.full != nil {
		s.Full = &Frame{At: at, Width: source.Native[0], Height: source.Native[1], RGB: p.full}
	}
	return s
}

// decodePictures runs a decoder whose stdout is up to count raw pictures, cutting each out
// as it arrives and showing its cell to OnFrame: a sheet's per-frame measures then run
// while later frames decode, not all after the last one. With early, the call returns at
// the count-th picture rather than when ffmpeg exits.
func decodePictures(ctx context.Context, args []string, source *Input, size [2]int, count int, o *SampleOptions, input []byte, early bool) ([]picture, string, int, error) {
	tools, err := ToolsFor(o.Settings)
	if err != nil {
		return nil, "", 0, err
	}
	bytes := frameBytes(source, size)
	var pictures []picture
	data := make([]byte, bytes)
	filled := 0
	onStdout := func(chunk []byte) {
		for at := 0; at < len(chunk); {
			n := min(bytes-filled, len(chunk)-at)
			copy(data[filled:], chunk[at:at+n])
			filled += n
			at += n
			if filled < bytes {
				continue
			}
			p := split(data, source, size)
			pictures = append(pictures, p)
			if o.OnFrame != nil {
				o.OnFrame(&Frame{Width: size[0], Height: size[1], RGB: p.rgb, memo: p.memo})
			}
			data = make([]byte, bytes)
			filled = 0
		}
	}
	options := proc.Options{Input: input, Timeout: o.Settings.Timeout, MaxBytes: bytes * count, OnStdout: onStdout, Warm: true}
	if early {
		options.Until = func(_ int, _ []byte, stderr string) bool {
			return len(pictures) == count && ShowinfoCount(stderr) >= count
		}
	}
	result, err := proc.Use(Decoders(o.Settings), ctx, func() (proc.Result, error) {
		return proc.Run(ctx, tools.FFmpeg, args, options)
	})
	if err != nil {
		return nil, "", 0, err
	}
	return pictures, result.Stderr, result.Code, nil
}

func grabArgs(source *Input, seek []string, size [2]int, count int, selectFilter string) []string {
	filters := []string{}
	if selectFilter != "" {
		filters = append(filters, selectFilter)
	}
	filters = append(filters, ToCell(source, size, ""), "showinfo=checksum=0")
	threads := "2"
	if selectFilter != "" {
		threads = "0"
	}
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "info", "-threads", threads}
	args = append(args, seek...)
	args = append(args, "-copyts")
	args = append(args, InputArgs(source.Input)...)
	// The rawvideo muxer would otherwise duplicate frames to fill a constant rate.
	return append(args, "-map", "0:V:0", "-frames:v", strconv.Itoa(count), "-vf", strings.Join(filters, ","),
		"-fps_mode", "passthrough", "-pix_fmt", "rgb24", "-f", "rawvideo", "pipe:1")
}

// PrewarmDecoders starts the decoders a session's first calls use, ahead of them (see
// proc's warm.go): sequential passes, keyframe batches cut from an MP4's index, and
// inspect_frame's lossless frame.
func PrewarmDecoders(s *rustle.Settings) {
	tools, err := ToolsFor(s)
	if err != nil {
		return
	}
	mp4 := &Input{Input: "warm.mp4", Native: [2]int{16, 16}}
	proc.Prewarm(tools.FFmpeg, grabArgs(mp4, []string{"-ss", "0", "-t", "1"}, [2]int{16, 16}, 1, "select"), 2)
	proc.Prewarm(tools.FFmpeg, IndexedArgs("h264", "0", "null"), 1)
	proc.Prewarm(tools.FFmpeg, IndexedArgs("h264", "2", "null"), 2)
	args, _, _, _ := FrameArgs("warm.mp4", FrameRequest{Size: [2]int{16, 16}, Native: [2]int{16, 16}, Lone: true})
	proc.Prewarm(tools.FFmpeg, args, 1)
}

// grab decodes up to count frames after the seek, scaled to size, with their actual times.
func grab(ctx context.Context, source *Input, seek []string, size [2]int, count int, o *SampleOptions, selectFilter string) ([]*Sampled, error) {
	args := grabArgs(source, seek, size, count, selectFilter)
	pictures, stderr, code, err := decodePictures(ctx, args, source, size, count, o, nil, true)
	if err != nil || code != 0 {
		return nil, err
	}
	stamps := ShowinfoTimes(stderr)
	start := LoggedStart(stderr, source.Start)
	var frames []*Sampled
	for i, p := range pictures {
		if i >= len(stamps) {
			break
		}
		frames = append(frames, sampled(p, math.Max(0, stamps[i]-start+source.Offset), source, size))
	}
	return frames, nil
}

// lastFrameTime is the time of the last video frame at or before `near`, from the packet
// index (no decoding). A video track often ends a frame or two before the container,
// whose audio runs longer.
func lastFrameTime(ctx context.Context, source *Input, near float64, s *rustle.Settings) (float64, bool) {
	tools, err := ToolsFor(s)
	if err != nil {
		return 0, false
	}
	// read_intervals takes the input's own timestamps, which start at its start time.
	from := rustle.Fixed(math.Max(0, near-source.Offset+source.Start-10), 3)
	args := append([]string{"-v", "error", "-select_streams", "v:0", "-read_intervals", from + "%", "-show_entries", "packet=pts_time", "-of", "csv=p=0"}, InputArgs(source.Input)...)
	result, err := proc.Run(ctx, tools.FFprobe, args, proc.Options{Timeout: 30 * time.Second, MaxBytes: 4 << 20})
	if err != nil || result.Code != 0 {
		return 0, false
	}
	best, found := math.Inf(-1), false
	for _, line := range strings.Split(string(result.Stdout), "\n") {
		if t := rustle.Number(line); rustle.Finite(t) && t > 0 {
			best, found = math.Max(best, t), true
		}
	}
	if !found {
		return 0, false
	}
	return best - source.Start + source.Offset, true
}

// FrameAt decodes the frame on screen at `at`; after the video track's last frame, that
// last frame. An MPEG-TS seek can land past the frame, or past the end, so a frame later
// than asked for is decoded again from further back until going back finds nothing earlier.
func FrameAt(ctx context.Context, source *Input, at float64, size [2]int, crop *[4]float64, lone bool, s *rustle.Settings) (*DecodedFrame, error) {
	step := frameStep(source)
	if step == 0 {
		step = 0.05
	}
	if source.Duration > 0 && at > source.Duration-2*step {
		if last, ok := lastFrameTime(ctx, source, at, s); ok {
			at = math.Min(at, last)
		}
	}
	var best *DecodedFrame
	for _, lookback := range []float64{0, 3, 15, 60} {
		request := FrameRequest{At: at, Native: source.Native, Size: size, Crop: crop, FPS: source.FPS, Start: source.Start, Offset: source.Offset, Lone: lone, Lookback: lookback}
		decoded, err := DecodeFrame(ctx, source.Input, request, s)
		if err != nil {
			if !rustle.HasCode(err, "NO_FRAME") {
				return nil, err
			}
			if lookback == 0 {
				if last, ok := lastFrameTime(ctx, source, at, s); ok && last < at {
					at = last
				}
			}
			continue
		}
		if decoded.At <= at+tolerance || (best != nil && decoded.At >= best.At) {
			return decoded, nil
		}
		best = decoded
	}
	if best != nil {
		return best, nil
	}
	return nil, rustle.Fail("No frame exists at that time.", "NO_FRAME", "Pass a time inside the video.")
}

func exact(ctx context.Context, source *Input, at float64, size [2]int, s *rustle.Settings) (*Sampled, error) {
	decoded, err := FrameAt(ctx, source, at, size, source.Crop, false, s)
	if err != nil {
		if rustle.HasCode(err, "NO_FRAME") {
			return nil, nil
		}
		return nil, err
	}
	return &Sampled{Frame: Frame{At: decoded.At, Width: decoded.Width, Height: decoded.Height, RGB: decoded.RGB, memo: &measures{}}}, nil
}

// sequential is one decoder pass over evenly spaced times: select keeps the first frame at
// or after each time (less one frame, as DecodeFrame seeks), so each kept frame is the one
// on screen then.
func sequential(ctx context.Context, source *Input, times []float64, size [2]int, o *SampleOptions) ([]*Sampled, error) {
	spacing := 1.0
	if len(times) > 1 {
		spacing = times[1] - times[0]
	}
	first := times[0] - source.Offset - frameStep(source) + tolerance
	length := times[len(times)-1] - times[0] + spacing + frameStep(source)
	// select sees copied timestamps, which include the input's start time.
	origin := rustle.Fixed(first+source.Start, 6)
	gap := rustle.Fixed(spacing, 6)
	selectFilter := fmt.Sprintf(`select='if(gte(t\,%s+ld(0))\,st(0\,%s*(floor((t-%s)/%s)+1))*0+1\,0)'`, origin, gap, origin, gap)
	var frames []*Sampled
	// An MPEG-TS seek can land past the first sample; select drops what an earlier start adds.
	for _, lookback := range []float64{0, 3, 15} {
		seek := math.Max(0, first-lookback)
		var err error
		frames, err = grab(ctx, source, []string{"-ss", rustle.Fixed(seek, 4), "-t", rustle.Fixed(length+first-seek, 4)}, size, len(times), o, selectFilter)
		if err != nil {
			return nil, err
		}
		if len(frames) == 0 || frames[0].At <= times[0]+tolerance || seek == 0 {
			break
		}
	}
	return frames, nil
}

// keyframesBefore is the keyframe at or before each time, several times to a process:
// each time is its own input with its own fast seek, and the frames leave stacked as one
// picture. Starting ffmpeg costs more than decoding one keyframe, so 48 times over an hour
// of video take under half the wall time of one process each. A batch that fails (one
// input without a frame stops the stack) leaves its times nil, to be grabbed one by one.
func keyframesBefore(ctx context.Context, source *Input, times []float64, size [2]int, o *SampleOptions) ([]*Sampled, error) {
	workers := o.Settings.Workers
	// Two or more times to a process, and no more processes than decoders.
	per := max(2, (len(times)+workers-1)/workers)
	var batches [][]float64
	for i := 0; i < len(times); i += per {
		batches = append(batches, times[i:min(len(times), i+per)])
	}
	found, err := rustle.MapLimit(batches, workers, func(batch []float64, _ int) ([]*Sampled, error) {
		return stacked(ctx, source, batch, size, o)
	})
	if err != nil {
		return nil, err
	}
	var out []*Sampled
	for _, f := range found {
		out = append(out, f...)
	}
	return out, nil
}

func stacked(ctx context.Context, source *Input, times []float64, size [2]int, o *SampleOptions) ([]*Sampled, error) {
	tools, err := ToolsFor(o.Settings)
	if err != nil {
		return nil, err
	}
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "info", "-copyts"}
	label := func(k int) string {
		if len(times) > 1 {
			return fmt.Sprintf("[v%d]", k)
		}
		return "[out]"
	}
	var graph []string
	for k, at := range times {
		args = append(args, "-threads", "1", "-noaccurate_seek", "-ss", rustle.Fixed(math.Max(0, at-source.Offset), 4))
		args = append(args, InputArgs(source.Input)...)
		graph = append(graph, fmt.Sprintf("[%d:V:0]trim=end_frame=1,%s,showinfo@f%d=checksum=0%s", k, ToCell(source, size, strconv.Itoa(k)), k, label(k)))
	}
	if len(times) > 1 {
		labels := ""
		for k := range times {
			labels += label(k)
		}
		graph = append(graph, fmt.Sprintf("%svstack=inputs=%d[out]", labels, len(times)))
	}
	args = append(args, "-filter_complex", strings.Join(graph, ";"), "-map", "[out]", "-frames:v", "1", "-pix_fmt", "rgb24", "-f", "rawvideo", "pipe:1")
	bytes := frameBytes(source, size)
	result, err := proc.Use(Decoders(o.Settings), ctx, func() (proc.Result, error) {
		return proc.Run(ctx, tools.FFmpeg, args, proc.Options{Timeout: o.Settings.Timeout, MaxBytes: bytes * len(times)})
	})
	if err != nil {
		return nil, err
	}
	out := make([]*Sampled, len(times))
	if result.Code != 0 || len(result.Stdout) != bytes*len(times) {
		return out, nil
	}
	start := LoggedStart(result.Stderr, source.Start)
	for k := range times {
		pts, ok := branchTime(result.Stderr, "f"+strconv.Itoa(k))
		if !ok {
			continue
		}
		p := split(slices.Clone(result.Stdout[k*bytes:(k+1)*bytes]), source, size)
		out[k] = sampled(p, math.Max(0, pts-start+source.Offset), source, size)
	}
	return out, nil
}

var branchPatterns sync.Map

// branchTime is the first frame time a named showinfo logged, in its own time base.
func branchTime(stderr, name string) (float64, bool) {
	cached, ok := branchPatterns.Load(name)
	if !ok {
		prefix := `\[showinfo@` + regexp.QuoteMeta(name) + ` @ [^\]]*\] `
		cached, _ = branchPatterns.LoadOrStore(name, [2]*regexp.Regexp{
			regexp.MustCompile(prefix + `config in time_base:\s*(\d+)/(\d+)`),
			regexp.MustCompile(prefix + `n:\s*\d+\s+pts:\s*(-?\d+)`),
		})
	}
	patterns := cached.([2]*regexp.Regexp)
	base := patterns[0].FindStringSubmatch(stderr)
	frame := patterns[1].FindStringSubmatch(stderr)
	if base == nil || frame == nil || !(rustle.Number(base[2]) > 0) {
		return 0, false
	}
	return rustle.Number(frame[1]) * rustle.Number(base[1]) / rustle.Number(base[2]), true
}

// keyframe is a keyframe within [earliest, latest]: the one at or before `at` (unless
// `before` already holds it), else the next one. One or two decoded frames instead of a
// whole GOP.
func keyframe(ctx context.Context, source *Input, at, earliest, latest float64, size [2]int, o *SampleOptions, before *Sampled) (*Sampled, error) {
	seek := rustle.Fixed(math.Max(0, at-source.Offset), 4)
	if before == nil {
		frames, err := grab(ctx, source, []string{"-noaccurate_seek", "-ss", seek}, size, 1, o, "")
		if err != nil {
			return nil, err
		}
		if len(frames) > 0 {
			before = frames[0]
		}
	}
	if before != nil && before.At >= earliest-tolerance && before.At <= at+tolerance {
		return before, nil
	}
	if latest <= at {
		return nil, nil
	}
	frames, err := grab(ctx, source, []string{"-skip_frame", "nokey", "-ss", seek, "-t", rustle.Fixed(latest-at+tolerance, 4)}, size, 1, o, "")
	if err != nil || len(frames) == 0 {
		return nil, err
	}
	if after := frames[0]; after.At >= at-tolerance && after.At <= latest+tolerance {
		return after, nil
	}
	return nil, nil
}

// Chunked is the consecutive samples not yet found, in chunks of about runBudget frames.
// When the file's keyframes are known, a chunk holds whole GOPs: two passes over one GOP
// would both decode from its keyframe, and a pass that adds the next GOP lengthens the
// slowest pass, so it does only while the chunk stays within budget. 12 samples of a 10 s
// clip with a keyframe every 5 s take 60 ms in two passes split at the keyframe, 68 ms
// split by count.
func Chunked(source *Input, times []float64, found []*Sampled, spacing float64) [][]int {
	fps := source.FPS
	if fps == 0 {
		fps = 30
	}
	keys := KeyTimes(source)
	gop := func(at float64) float64 {
		lo := sort.Search(len(keys), func(i int) bool { return keys[i] > at+tolerance })
		return keys[max(0, lo-1)]
	}
	// Runs of consecutive samples, split at keyframes when they are known.
	var runs [][]int
	for i, at := range times {
		if found[i] != nil {
			continue
		}
		if n := len(runs); n > 0 {
			open := runs[n-1]
			if open[len(open)-1] == i-1 && (keys == nil || gop(at) == gop(times[i-1])) {
				runs[n-1] = append(open, i)
				continue
			}
		}
		runs = append(runs, []int{i})
	}
	if keys == nil {
		size := max(1, int(math.Floor(runBudget/(spacing*fps))))
		var chunks [][]int
		for _, run := range runs {
			for k := 0; k < len(run); k += size {
				chunks = append(chunks, run[k:min(len(run), k+size)])
			}
		}
		return chunks
	}
	var chunks [][]int
	for _, run := range runs {
		if n := len(chunks); n > 0 {
			open := chunks[n-1]
			if open[len(open)-1] == run[0]-1 && (times[run[len(run)-1]]-gop(times[open[0]]))*fps <= runBudget {
				chunks[n-1] = append(open, run...)
				continue
			}
		}
		chunks = append(chunks, append([]int(nil), run...))
	}
	return chunks
}

// Sample decodes frames for `times`, evenly o.Spacing apart, sorted, with repeats of one
// frame dropped.
func Sample(ctx context.Context, source *Input, times []float64, size [2]int, o *SampleOptions) ([]*Sampled, error) {
	limit := o.Settings.Workers * 2
	found := make([]*Sampled, len(times))
	overview := o.Overview && o.Spacing >= keyframeSpacing
	indexed := false
	if overview {
		// A local MP4/MOV hands over its keyframes by its own index, without seeking.
		frames, ok, err := indexedKeyframes(ctx, source, times, size, o)
		if err != nil {
			return nil, err
		}
		if ok {
			indexed = true
			copy(found, frames)
		}
	}
	if overview && !indexed {
		before, err := keyframesBefore(ctx, source, times, size, o)
		if err != nil {
			return nil, err
		}
		if _, err := rustle.MapLimit(times, limit, func(at float64, i int) (struct{}, error) {
			earliest := math.Max(o.Floor, at-o.Spacing/2)
			latest := math.Min(o.Ceiling-frameStep(source), at+o.Spacing/2)
			var prior *Sampled
			if i < len(before) {
				prior = before[i]
			}
			frame, err := keyframe(ctx, source, at, earliest, latest, size, o, prior)
			found[i] = frame
			return struct{}{}, err
		}); err != nil {
			return nil, err
		}
	}
	// The rest in chunks of consecutive samples, one sequential pass each; a chunk of one seeks.
	decoded, err := rustle.MapLimit(Chunked(source, times, found, o.Spacing), limit, func(indices []int, _ int) ([]*Sampled, error) {
		if len(indices) > 1 {
			chunk := make([]float64, len(indices))
			for k, i := range indices {
				chunk[k] = times[i]
			}
			frames, err := sequential(ctx, source, chunk, size, o)
			if err != nil || len(frames) > 0 {
				return frames, err
			}
		}
		return rustle.MapLimit(indices, limit, func(i int, _ int) (*Sampled, error) {
			return exact(ctx, source, times[i], size, o.Settings)
		})
	})
	if err != nil {
		return nil, err
	}
	var frames []*Sampled
	for _, f := range found {
		if f != nil {
			frames = append(frames, f)
		}
	}
	for _, chunk := range decoded {
		for _, f := range chunk {
			if f != nil {
				frames = append(frames, f)
			}
		}
	}
	sort.SliceStable(frames, func(i, j int) bool { return frames[i].At < frames[j].At })
	var out []*Sampled
	for i, f := range frames {
		if i == 0 || f.At-frames[i-1].At > 0.0005 {
			out = append(out, f)
		}
	}
	return out, nil
}
