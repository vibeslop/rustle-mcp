// Package media decodes frames and sound with ffmpeg and lays frames out as contact sheets.
//
// ffmpeg does all decoding. Every Homebrew, apt and winget build includes the decoders,
// the scaler and the PNG, MJPEG and FLAC encoders used here, and a system binary costs
// this package nothing to install. Each operation runs in its own short-lived process
// (or one started ahead of it, see proc's warm.go), which the caller's deadline or
// cancellation kills outright.
package media

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vibeslop/rustle-mcp/internal/proc"
	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

type Tools struct {
	FFmpeg  string
	FFprobe string
}

var located struct {
	sync.Mutex
	done  bool
	tools *Tools
}

var ffmpegName = regexp.MustCompile(`ffmpeg(\.exe)?$`)

// ToolsFor finds ffmpeg and ffprobe once: RUSTLE_FFMPEG and RUSTLE_FFPROBE, else PATH.
func ToolsFor(s *rustle.Settings) (Tools, error) {
	located.Lock()
	defer located.Unlock()
	if !located.done {
		located.done = true
		ffmpeg := ""
		if s.FFmpeg != "" {
			if exists(s.FFmpeg) {
				ffmpeg = s.FFmpeg
			}
		} else {
			ffmpeg = proc.Which("ffmpeg")
		}
		ffprobe := s.FFprobe
		if ffprobe == "" {
			if sibling := ffmpegName.ReplaceAllString(ffmpeg, "ffprobe$1"); ffmpeg != "" && exists(sibling) {
				ffprobe = sibling
			} else {
				ffprobe = proc.Which("ffprobe")
			}
		}
		if ffmpeg != "" && ffprobe != "" {
			located.tools = &Tools{FFmpeg: ffmpeg, FFprobe: ffprobe}
		}
	}
	if located.tools == nil {
		return Tools{}, rustle.Fail("Frames and sound need ffmpeg, which is not installed.", "FFMPEG_MISSING",
			"Ask the user to install ffmpeg (macOS: brew install ffmpeg; Debian/Ubuntu: sudo apt install ffmpeg; Windows: winget install ffmpeg) and restart the MCP server, or set RUSTLE_FFMPEG to its path. read_transcript and find_videos work without it.")
	}
	return *located.tools, nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

var pool struct {
	sync.Once
	s *proc.Semaphore
}

// Decoders bounds concurrent decoders so a fan-out of parallel agents can't
// oversubscribe the CPU.
func Decoders(s *rustle.Settings) *proc.Semaphore {
	pool.Do(func() { pool.s = proc.NewSemaphore(s.Workers) })
	return pool.s
}

var remote = regexp.MustCompile(`(?i)^https?:`)

// InputArgs is how ffmpeg may open an input: files stay files, and remote inputs can't
// reach local files.
func InputArgs(input string) []string {
	if remote.MatchString(input) {
		return []string{"-protocol_whitelist", "https,http,tls,tcp,crypto,data", "-user_agent", "Mozilla/5.0", "-i", input}
	}
	return []string{"-protocol_whitelist", "file,crypto,data", "-i", input}
}

type Probe struct {
	Duration float64
	// Start is the format start time; every reported timestamp is relative to it.
	Start    float64
	HasVideo bool
	HasAudio bool
	// Width and Height are the display size, after rotation and sample aspect ratio.
	Width      int
	Height     int
	FPS        float64
	SampleRate int
	Channels   int
}

var (
	refused403   = regexp.MustCompile(`Server returned 403|HTTP error 403`)
	missing404   = regexp.MustCompile(`Server returned 404|HTTP error 404`)
	unreachable  = regexp.MustCompile(`(?i)Connection refused|Network is unreachable|Failed to resolve|timed out`)
	noStreams    = regexp.MustCompile(`matches no streams|does not contain any stream`)
	emptyOutput  = regexp.MustCompile(`Output file is empty|nothing was encoded`)
	durationLine = regexp.MustCompile(`Duration: [^,]*, start: (-?[\d.]+)`)
	showinfoLine = regexp.MustCompile(`\bn:\s*\d+\s+pts:`)
	showinfoPTS  = regexp.MustCompile(`\bn:\s*\d+\s+pts:\s*(-?\d+)\s+pts_time:\s*(\S+)`)
	timeBase     = regexp.MustCompile(`config in time_base:\s*(\d+)/(\d+)`)
)

func decodeFailure(stderr string) *rustle.Error {
	switch {
	case refused403.MatchString(stderr):
		return rustle.Fail("The media host refused access (HTTP 403).", "ACCESS_DENIED", "Use a public source, a direct media URL or a local file.")
	case missing404.MatchString(stderr):
		return rustle.Fail("The media host reports the file as missing (HTTP 404).", "SOURCE_NOT_FOUND", "Check the link or choose another source.")
	case unreachable.MatchString(stderr):
		return rustle.Fail("Could not reach the media host.", "NETWORK_UNAVAILABLE", "Check network access, wait briefly, then retry.").Retry(0)
	}
	return rustle.Fail("Media decoding failed. The file may be damaged, still being written, or in an unsupported format.",
		"DECODE_FAILED", "Check that the file plays in a media player, then retry.")
}

type probeStream struct {
	CodecType         string           `json:"codec_type"`
	Width             json.RawMessage  `json:"width"`
	Height            json.RawMessage  `json:"height"`
	SampleAspectRatio string           `json:"sample_aspect_ratio"`
	AvgFrameRate      string           `json:"avg_frame_rate"`
	RFrameRate        string           `json:"r_frame_rate"`
	Duration          json.RawMessage  `json:"duration"`
	SampleRate        json.RawMessage  `json:"sample_rate"`
	Channels          json.RawMessage  `json:"channels"`
	Disposition       map[string]any   `json:"disposition"`
	SideDataList      []map[string]any `json:"side_data_list"`
	Tags              map[string]any   `json:"tags"`
}

// rawNumber is Number() of a JSON value ffprobe writes as a string or a number; NaN when missing.
func rawNumber(raw json.RawMessage) float64 {
	if len(raw) == 0 {
		return math.NaN()
	}
	var v any
	if json.Unmarshal(raw, &v) != nil || v == nil {
		return math.NaN()
	}
	return rustle.Number(v)
}

// ProbeInput reads duration, tracks, display size and frame rate with ffprobe.
func ProbeInput(ctx context.Context, input string, s *rustle.Settings) (*Probe, error) {
	tools, err := ToolsFor(s)
	if err != nil {
		return nil, err
	}
	args := append([]string{"-v", "error", "-print_format", "json", "-show_format", "-show_streams"}, InputArgs(input)...)
	result, err := proc.Run(ctx, tools.FFprobe, args, proc.Options{Timeout: min(s.Timeout, 30*time.Second), MaxBytes: 4 << 20})
	if err != nil {
		return nil, err
	}
	if result.Code != 0 {
		return nil, decodeFailure(result.Stderr)
	}
	var info struct {
		Streams []probeStream `json:"streams"`
		Format  struct {
			Duration  json.RawMessage `json:"duration"`
			StartTime json.RawMessage `json:"start_time"`
		} `json:"format"`
	}
	if err := json.Unmarshal(result.Stdout, &info); err != nil {
		return nil, decodeFailure(result.Stderr)
	}
	var video, audio *probeStream
	for i := range info.Streams {
		st := &info.Streams[i]
		// Album art in audio files is a one-frame "video" stream; it isn't video.
		if video == nil && st.CodecType == "video" && !truthy(st.Disposition["attached_pic"]) {
			video = st
		}
		if audio == nil && st.CodecType == "audio" {
			audio = st
		}
	}
	durationRaw := info.Format.Duration
	if len(durationRaw) == 0 && video != nil {
		durationRaw = video.Duration
	}
	if len(durationRaw) == 0 && audio != nil {
		durationRaw = audio.Duration
	}
	duration := rawNumber(durationRaw)
	if video == nil && audio == nil {
		return nil, rustle.Fail("The source contains no video or audio track.", "UNSUPPORTED_MEDIA", "Check that the file plays in a media player, or pass another source.")
	}
	if !rustle.Finite(duration) || duration <= 0 || duration > 24*3600 {
		return nil, rustle.Fail("The media duration is missing, live, or longer than 24 hours.", "UNSUPPORTED_MEDIA", "Pass a finished recording under 24 hours.")
	}
	p := &Probe{Duration: duration, HasVideo: video != nil, HasAudio: audio != nil}
	if start := rawNumber(info.Format.StartTime); rustle.Finite(start) {
		p.Start = start
	}
	if video != nil {
		width, height := rawNumber(video.Width), rawNumber(video.Height)
		sar := video.SampleAspectRatio
		if sar == "" {
			sar = "1:1"
		}
		if parts := strings.Split(sar, ":"); len(parts) == 2 {
			w, h := rustle.Number(parts[0]), rustle.Number(parts[1])
			if w > 0 && h > 0 && w != h {
				width = rustle.Round(width * w / h)
			}
		}
		rotation := 0.0
		found := false
		for _, d := range video.SideDataList {
			if r, ok := d["rotation"]; ok {
				rotation, found = rustle.Number(r), true
				break
			}
		}
		if !found {
			if r, ok := video.Tags["rotate"]; ok {
				rotation = rustle.Number(r)
			}
		}
		if math.Mod(math.Abs(rotation), 180) == 90 {
			width, height = height, width
		}
		if !(width > 0 && height > 0) || width*height > 40_000_000 {
			return nil, rustle.Fail("The video dimensions are missing or exceed the decoding limit.", "UNSUPPORTED_MEDIA", "Pass a video under 40 megapixels.")
		}
		p.Width, p.Height = int(width), int(height)
		rate := video.AvgFrameRate
		if rate == "" {
			rate = video.RFrameRate
		}
		if rate == "" {
			rate = "0/1"
		}
		if parts := strings.Split(rate, "/"); len(parts) == 2 {
			num, den := rustle.Number(parts[0]), rustle.Number(parts[1])
			if den > 0 && num > 0 {
				p.FPS = num / den
			}
		}
	}
	if audio != nil {
		if r := rawNumber(audio.SampleRate); rustle.Finite(r) && r > 0 {
			p.SampleRate = int(r)
		}
		if c := rawNumber(audio.Channels); rustle.Finite(c) && c > 0 {
			p.Channels = int(c)
		}
	}
	return p, nil
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return x != ""
	}
	return true
}

// Fit fits (width, height) inside a box without enlarging; at least 1 px each way.
func Fit(width, height, maxWidth, maxHeight int) [2]int {
	scale := math.Min(1, math.Min(float64(maxWidth)/float64(width), float64(maxHeight)/float64(height)))
	return [2]int{max(1, int(rustle.Round(float64(width)*scale))), max(1, int(rustle.Round(float64(height)*scale)))}
}

// FrameRequest asks for the frame shown at a time.
type FrameRequest struct {
	// At is the requested time in seconds from the media origin.
	At float64
	// Native is the display size of the source, from probe.
	Native [2]int
	// Size is the output size the region is scaled to, larger or smaller.
	Size [2]int
	// Crop is [x, y, width, height] as fractions of the frame; nil for the whole frame.
	Crop *[4]float64
	// FPS lets a time between two frames return the one on screen at that time.
	FPS float64
	// Start is the input's format start time (Probe.Start).
	Start float64
	// Offset is the timeline time of the input's first timestamp; 0 except for windows of
	// a longer video.
	Offset float64
	// Lone is a single frame someone waits for, decoded on every core; sheet frames decode
	// side by side, where two threads each avoid oversubscribing the CPU.
	Lone bool
	// Lookback is seconds to start demuxing before the seek point, dropping earlier frames
	// in a filter. MPEG-TS has no index, so its seek can land past the target keyframe.
	Lookback float64
}

type DecodedFrame struct {
	Frame
	// Native is the size of the region before scaling: the whole frame or the crop, in source pixels.
	Native [2]int
}

// CropPixels is the crop in source pixels, at least one pixel each way.
func CropPixels(native [2]int, crop [4]float64) [4]int {
	W, H := float64(native[0]), float64(native[1])
	x := math.Min(W-1, rustle.Round(crop[0]*W))
	y := math.Min(H-1, rustle.Round(crop[1]*H))
	w := math.Max(1, math.Min(W-x, rustle.Round((crop[0]+crop[2])*W)-x))
	h := math.Max(1, math.Min(H-y, rustle.Round((crop[1]+crop[3])*H)-y))
	return [4]int{int(x), int(y), int(w), int(h)}
}

// LoggedStart is the input's start time as ffmpeg itself reports it: the authority for
// converting pts.
func LoggedStart(stderr string, fallback float64) float64 {
	if m := durationLine.FindStringSubmatch(stderr); m != nil {
		return rustle.Number(m[1])
	}
	return fallback
}

// ShowinfoCount is the frames showinfo has logged so far.
func ShowinfoCount(stderr string) int { return len(showinfoLine.FindAllStringIndex(stderr, -1)) }

// ShowinfoTimes is frame times from showinfo, exactly: pts in the filter's time base.
// Its pts_time is printed to six significant digits, which past 1,000 s rounds to the hundredth.
func ShowinfoTimes(stderr string) []float64 {
	base := timeBase.FindStringSubmatch(stderr)
	var times []float64
	for _, m := range showinfoPTS.FindAllStringSubmatch(stderr, -1) {
		if base != nil && rustle.Number(base[2]) > 0 {
			times = append(times, rustle.Number(m[1])*rustle.Number(base[1])/rustle.Number(base[2]))
		} else {
			times = append(times, rustle.Number(m[2]))
		}
	}
	return times
}

// FrameArgs is the command that decodes the frame shown at `at`, scaled to `size`: the
// last frame at or before it, allowing the 5 ms that reported times are rounded by, so a
// listed time returns that same frame. The seek lands on the keyframe at or before that
// time without trimming, and select keeps the first frame later than one frame before
// it. A target that is itself a keyframe, as overview samples often are, then decodes
// one frame instead of the whole interval before it.
func FrameArgs(input string, r FrameRequest) (args []string, width, height int, region [2]int) {
	step := 0.0
	if r.FPS > 0 {
		step = 1 / r.FPS
	}
	target := math.Max(0, r.At-r.Offset) + 0.0051
	region = r.Native
	var crop [4]int
	if r.Crop != nil {
		crop = CropPixels(r.Native, *r.Crop)
		region = [2]int{crop[2], crop[3]}
	}
	width, height = r.Size[0], r.Size[1]
	// With copied timestamps the filter sees the input's own clock, which begins at Start.
	// Rotation is applied by ffmpeg before these filters, so crop works in display pixels.
	filters := []string{
		fmt.Sprintf(`select='gt(t\,%s)'`, rustle.Fixed(target-step+r.Start, 6)),
		fmt.Sprintf("scale=%d:%d", r.Native[0], r.Native[1]),
	}
	if r.Crop != nil {
		filters = append(filters, fmt.Sprintf("crop=%d:%d:%d:%d", crop[2], crop[3], crop[0], crop[1]))
	}
	if width != region[0] || height != region[1] {
		filters = append(filters, fmt.Sprintf("scale=%d:%d:flags=lanczos", width, height))
	}
	filters = append(filters, "setsar=1", "showinfo=checksum=0")
	threads := "2"
	if r.Lone {
		threads = "0"
	}
	args = []string{"-hide_banner", "-nostdin", "-loglevel", "info", "-threads", threads,
		"-noaccurate_seek", "-ss", rustle.Fixed(math.Max(0, target-r.Lookback), 6), "-copyts"}
	args = append(args, InputArgs(input)...)
	args = append(args, "-map", "0:V:0", "-frames:v", "1", "-vf", strings.Join(filters, ","), "-pix_fmt", "rgb24", "-f", "rawvideo", "pipe:1")
	return args, width, height, region
}

// DecodeFrame decodes the frame shown at r.At (see FrameArgs).
func DecodeFrame(ctx context.Context, input string, r FrameRequest, s *rustle.Settings) (*DecodedFrame, error) {
	tools, err := ToolsFor(s)
	if err != nil {
		return nil, err
	}
	args, width, height, region := FrameArgs(input, r)
	expected := width * height * 3
	result, err := proc.Use(Decoders(s), ctx, func() (proc.Result, error) {
		return proc.Run(ctx, tools.FFmpeg, args, proc.Options{
			Timeout: s.Timeout, MaxBytes: expected, Warm: true,
			Until: func(n int, _ []byte, stderr string) bool { return n == expected && ShowinfoCount(stderr) >= 1 },
		})
	})
	if err != nil {
		return nil, err
	}
	if result.Code != 0 || len(result.Stdout) != expected {
		if result.Code == 0 || emptyOutput.MatchString(result.Stderr) {
			return nil, rustle.Fail("No frame exists at that time.", "NO_FRAME", "Pass a time inside the video.")
		}
		return nil, decodeFailure(result.Stderr)
	}
	at := r.At
	if times := ShowinfoTimes(result.Stderr); len(times) > 0 {
		at = times[0] - LoggedStart(result.Stderr, r.Start) + r.Offset
	}
	return &DecodedFrame{Frame: Frame{At: math.Max(0, at), Width: width, Height: height, RGB: result.Stdout}, Native: region}, nil
}

// PCM is decoded samples, one slice per channel, -1..1.
type PCM struct {
	Channels [][]float32
	Rate     int
}

type PCMOptions struct {
	// Rate resamples when above 0.
	Rate   int
	Mono   bool
	Offset float64
}

var (
	schemeLike = regexp.MustCompile(`(?i)^[a-z][a-z0-9+.-]+:`)
	wavName    = regexp.MustCompile(`(?i)\.wav$`)
)

// DecodePCM decodes samples of [start, end) from the first audio track.
func DecodePCM(ctx context.Context, input string, start, end float64, s *rustle.Settings, o PCMOptions) (*PCM, error) {
	// Uncompressed WAV, what agents render sound effects to, needs no decoder at all.
	if !schemeLike.MatchString(input) && o.Rate == 0 && wavName.MatchString(input) {
		if pcm := ReadWAV(input, math.Max(0, start-o.Offset), end-o.Offset, o.Mono); pcm != nil && len(pcm.Channels) > 0 && len(pcm.Channels[0]) > 0 {
			return pcm, nil
		}
	}
	tools, err := ToolsFor(s)
	if err != nil {
		return nil, err
	}
	// A WAV header ahead of the samples carries the rate and channel count, which saves
	// an ffprobe process (about a third of a short listen call).
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-ss", rustle.Fixed(math.Max(0, start-o.Offset), 4)}
	args = append(args, InputArgs(input)...)
	args = append(args, "-t", rustle.Fixed(math.Max(0.001, end-start), 4), "-map", "0:a:0", "-vn")
	if o.Mono {
		args = append(args, "-ac", "1")
	}
	if o.Rate > 0 {
		args = append(args, "-ar", strconv.Itoa(o.Rate))
	}
	args = append(args, "-c:a", "pcm_f32le", "-f", "wav", "pipe:1")
	rate, channels := 192000.0, 8.0
	if o.Rate > 0 {
		rate = float64(o.Rate)
	}
	if o.Mono {
		channels = 1
	}
	limit := 65536 + int(math.Ceil((end-start+1)*rate*channels*4))
	result, err := proc.Use(Decoders(s), ctx, func() (proc.Result, error) {
		return proc.Run(ctx, tools.FFmpeg, args, proc.Options{Timeout: s.Timeout, MaxBytes: limit})
	})
	if err != nil {
		return nil, err
	}
	if result.Code != 0 && noStreams.MatchString(result.Stderr) {
		return nil, rustle.NoAudioTrack("")
	}
	if result.Code != 0 {
		return nil, decodeFailure(result.Stderr)
	}
	sampleRate, total, data, ok := wavHeader(result.Stdout)
	if !ok {
		return nil, decodeFailure(result.Stderr)
	}
	count := min(8, total)
	frames := (len(result.Stdout) - data) / 4 / total
	if frames <= 0 {
		return nil, rustle.Fail("No decodable audio exists in this span.", "NO_AUDIO", "Pass a span inside the media.")
	}
	out := make([][]float32, count)
	for c := range out {
		out[c] = make([]float32, frames)
	}
	raw := result.Stdout[data:]
	for i, k := 0, 0; i < frames; i, k = i+1, k+total*4 {
		for c := range count {
			out[c][i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[k+c*4:]))
		}
	}
	return &PCM{Channels: out, Rate: sampleRate}, nil
}

// wavHeader is the rate, channel count and sample offset of a streamed WAV; a pipe
// leaves its chunk sizes unset.
func wavHeader(wav []byte) (rate, channels, data int, ok bool) {
	if len(wav) < 12 || string(wav[:4]) != "RIFF" {
		return 0, 0, 0, false
	}
	for at := 12; at+8 <= len(wav); {
		id := string(wav[at : at+4])
		size := int(binary.LittleEndian.Uint32(wav[at+4:]))
		if id == "data" {
			return rate, channels, at + 8, rate > 0 && channels > 0
		}
		if id == "fmt " && at+16 <= len(wav) {
			channels = int(binary.LittleEndian.Uint16(wav[at+10:]))
			rate = int(binary.LittleEndian.Uint32(wav[at+12:]))
		}
		at += 8 + size + size&1
	}
	return 0, 0, 0, false
}

// EncodeFLAC is lossless FLAC of [start, end), straight from the source's first audio
// track at its own rate.
func EncodeFLAC(ctx context.Context, input string, start, end float64, s *rustle.Settings, offset float64) ([]byte, error) {
	tools, err := ToolsFor(s)
	if err != nil {
		return nil, err
	}
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-ss", rustle.Fixed(math.Max(0, start-offset), 4)}
	args = append(args, InputArgs(input)...)
	args = append(args, "-t", rustle.Fixed(math.Max(0.001, end-start), 4),
		"-map", "0:a:0", "-vn", "-c:a", "flac", "-sample_fmt", "s32", "-bits_per_raw_sample", "24", "-f", "flac", "pipe:1")
	result, err := proc.Use(Decoders(s), ctx, func() (proc.Result, error) {
		return proc.Run(ctx, tools.FFmpeg, args, proc.Options{Timeout: s.Timeout, MaxBytes: 64 << 20})
	})
	if err != nil {
		return nil, err
	}
	if result.Code != 0 || len(result.Stdout) == 0 {
		return nil, decodeFailure(result.Stderr)
	}
	return result.Stdout, nil
}

// EncodeJPEGWithFFmpeg is a JPEG of an RGB buffer without chroma subsampling, so labels and
// small text stay sharp.
func EncodeJPEGWithFFmpeg(ctx context.Context, rgb []byte, width, height int, s *rustle.Settings, quality int) ([]byte, error) {
	tools, err := ToolsFor(s)
	if err != nil {
		return nil, err
	}
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error",
		"-f", "rawvideo", "-pix_fmt", "rgb24", "-s", fmt.Sprintf("%dx%d", width, height), "-i", "pipe:0",
		"-frames:v", "1", "-c:v", "mjpeg", "-pix_fmt", "yuvj444p", "-q:v", strconv.Itoa(quality), "-f", "image2pipe", "pipe:1"}
	result, err := proc.Run(ctx, tools.FFmpeg, args, proc.Options{Input: rgb, Timeout: s.Timeout, MaxBytes: 32 << 20})
	if err != nil {
		return nil, err
	}
	if result.Code != 0 || len(result.Stdout) == 0 {
		return nil, decodeFailure(result.Stderr)
	}
	return result.Stdout, nil
}
