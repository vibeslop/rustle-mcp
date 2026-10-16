package media

import (
	"context"
	"slices"
	"sort"

	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

// perDecoder is the keyframes per H.264 decoder; several decoders share the work of a
// long overview.
const perDecoder = 24

// KeyframesFor is the keyframe each overview time may move to: the one at or before it if
// no earlier than half the spacing, else the next one if within half the spacing (inside
// [floor, ceiling]), as the seeking path chooses. -1 where no keyframe is that close.
func KeyframesFor(index *VideoIndex, times []float64, spacing, floor, ceiling, step float64) []int {
	keyTimes := make([]float64, len(index.Keys))
	for i, k := range index.Keys {
		keyTimes[i] = index.Times[k]
	}
	k := -1
	out := make([]int, len(times))
	for i, at := range times {
		earliest := max(floor, at-spacing/2)
		latest := min(ceiling-step, at+spacing/2)
		for k+1 < len(keyTimes) && keyTimes[k+1] <= at+tolerance {
			k++
		}
		switch {
		case k >= 0 && keyTimes[k] >= earliest-tolerance:
			out[i] = index.Keys[k]
		case k+1 < len(keyTimes) && keyTimes[k+1] <= latest+tolerance:
			out[i] = index.Keys[k+1]
		default:
			out[i] = -1
		}
	}
	return out
}

// indexable: only a whole local file has an index of its own to read.
func indexable(source *Input) bool {
	return !schemeLike.MatchString(source.Input) && source.Offset == 0
}

// KeyTimes is the presentation times of a local MP4/MOV's keyframes, ascending; nil
// without an index.
func KeyTimes(source *Input) []float64 {
	if !indexable(source) {
		return nil
	}
	index := IndexOf(source.Input)
	if index == nil {
		return nil
	}
	times := make([]float64, len(index.Keys))
	for i, k := range index.Keys {
		times[i] = index.Times[k]
	}
	sort.Float64s(times)
	return times
}

// IndexedArgs decodes a raw H.264/HEVC stream from stdin through filter; the filter counts
// frames with showinfo.
func IndexedArgs(codec, threads, filter string) []string {
	return []string{"-hide_banner", "-nostdin", "-loglevel", "info", "-threads", threads, "-f", codec, "-i", "pipe:0", "-vf", filter,
		"-fps_mode", "passthrough", "-pix_fmt", "rgb24", "-f", "rawvideo", "pipe:1"}
}

// indexedKeyframes decodes keyframes for overview times of a local MP4/MOV, cut out of the
// file by its own index and decoded from stdin, several to a decoder: no seek and no
// re-reading the file's index per sample, which for 48 samples of an hour of video takes
// a third of the time. It returns one entry per time (nil where no keyframe is close
// enough, left to the seeking path), and false when the file can't be served this way.
func indexedKeyframes(ctx context.Context, source *Input, times []float64, size [2]int, o *SampleOptions) ([]*Sampled, bool, error) {
	if !indexable(source) {
		return nil, false, nil
	}
	index := IndexOf(source.Input)
	if index == nil {
		return nil, false, nil
	}
	chosen := KeyframesFor(index, times, o.Spacing, o.Floor, o.Ceiling, frameStep(source))
	var keys []int
	for _, k := range chosen {
		if k >= 0 {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	keys = slices.Compact(keys)
	if len(keys) == 0 {
		return make([]*Sampled, len(times)), true, nil
	}
	// H.264 IDR keyframes decode back to back in one stream; HEVC decoders drop some CRA
	// keyframes that follow other pictures, so each of those gets its own decoder.
	per := 1
	if index.Codec == "h264" {
		per = max(perDecoder, (len(keys)+o.Settings.Workers-1)/o.Settings.Workers)
	}
	var batches [][]int
	for i := 0; i < len(keys); i += per {
		batches = append(batches, keys[i:min(len(keys), i+per)])
	}
	type decoded struct {
		ok     bool
		frames map[int]*Sampled
	}
	results, err := rustle.MapLimit(batches, o.Settings.Workers, func(batch []int, _ int) (decoded, error) {
		threads := "0"
		if len(batches) > 1 {
			threads = "2"
		}
		args := IndexedArgs(index.Codec, threads, "showinfo=checksum=0,"+ToCell(source, size, ""))
		stream, err := Elementary(source.Input, index, batch)
		if err != nil {
			return decoded{}, nil
		}
		// Waits for ffmpeg to exit, so a keyframe that decoded to more than one picture shows.
		pictures, stderr, code, err := decodePictures(ctx, args, source, size, len(batch), o, stream, false)
		if err != nil {
			return decoded{}, err
		}
		// Counting what the decoder put out guards the positions: each fed keyframe is one frame.
		if code != 0 || ShowinfoCount(stderr) != len(batch) || len(pictures) != len(batch) {
			return decoded{}, nil
		}
		frames := map[int]*Sampled{}
		for i, k := range batch {
			frames[k] = sampled(pictures[i], index.Times[k], source, size)
		}
		return decoded{ok: true, frames: frames}, nil
	})
	if err != nil {
		return nil, false, err
	}
	frames := map[int]*Sampled{}
	for _, r := range results {
		if !r.ok {
			return nil, false, nil
		}
		for k, f := range r.frames {
			frames[k] = f
		}
	}
	out := make([]*Sampled, len(times))
	for i, k := range chosen {
		if k >= 0 {
			out[i] = frames[k]
		}
	}
	return out, true, nil
}
