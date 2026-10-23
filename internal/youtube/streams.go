package youtube

import (
	"encoding/base64"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

// Which of YouTube's adaptive formats frames and sound are read from. The VISIONOS
// client's URLs serve any byte range without a PO token or the player script (measured
// October 2026; also yt-dlp's default without a JavaScript runtime). ANDROID_VR URLs, and
// VISIONOS URLs issued to some visitor ids, are refused past the first minute of media;
// a web PO token does not lift that, so the media code asks again under a fresh visitor id.

type Format struct {
	Itag int `json:"itag"`
	// Container is "mp4" or "webm".
	Container string  `json:"container"`
	Codecs    string  `json:"codecs"`
	Width     int     `json:"width,omitempty"`
	Height    int     `json:"height,omitempty"`
	FPS       float64 `json:"fps,omitempty"`
	Bitrate   float64 `json:"bitrate"`
	Length    float64 `json:"length"`
	InitEnd   float64 `json:"initEnd"`
	IndexEnd  float64 `json:"indexEnd"`
	// LMT is the encoding version; cached segments of another version are never mixed in.
	LMT      string       `json:"lmt"`
	URL      string       `json:"url"`
	Duration float64      `json:"duration"`
	Audio    *AudioTraits `json:"audio,omitempty"`
}

type AudioTraits struct {
	Original bool `json:"original"`
	DRC      bool `json:"drc"`
	Dubbed   bool `json:"dubbed"`
}

type Streams struct {
	Client string `json:"client"`
	// Expires is when the URLs stop working, ms since the epoch.
	Expires int64    `json:"expires"`
	Formats []Format `json:"formats"`
}

// recovery is the way on that is left when YouTube refuses media.
const recovery = "watch_video with frames=0 still reads what is said."

func accessDenied(detail string) *rustle.Error {
	return rustle.Fail("YouTube refused the video streams: "+detail, "YOUTUBE_ACCESS_DENIED",
		"Retry in a few minutes; YouTube sometimes refuses a network for a while. "+recovery+" A local copy or direct media URL avoids YouTube.").Retry(300)
}

func rateLimited() *rustle.Error {
	return rustle.Fail("YouTube is rate limiting this connection.", "RATE_LIMITED", "Wait a few minutes before retrying; "+recovery).Retry(120)
}

func unavailable(detail string) *rustle.Error {
	return rustle.Fail("YouTube's media servers failed: "+detail, "UPSTREAM_UNAVAILABLE", "Wait briefly, then retry; "+recovery).Retry(0)
}

var mimeParts = regexp.MustCompile(`^(audio|video)/(mp4|webm); codecs="([^"]+)"`)

// ParseFormats keeps the adaptive formats with a byte-range index right after the init
// segment, so one request reads both; live and post-live (OTF) formats have no index.
func ParseFormats(raw []rustle.YtFormat) []Format {
	var out []Format
	for _, f := range raw {
		mime := mimeParts.FindStringSubmatch(f.MimeType)
		if mime == nil || f.URL == "" || f.InitRange == nil || f.IndexRange == nil || f.Type == "FORMAT_STREAM_TYPE_OTF" {
			continue
		}
		if rustle.Number(f.IndexRange.Start) != rustle.Number(f.InitRange.End)+1 {
			continue
		}
		xtags := ""
		if f.Xtags != "" {
			// base64url, padded or not; Buffer.from skips what it can't read.
			if data, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(f.Xtags, "=")); err == nil {
				xtags = string(data)
			}
		}
		bitrate := f.AverageBitrate
		if bitrate == 0 {
			bitrate = f.Bitrate
		}
		lmt := f.LastModified
		if lmt == "" {
			if u, err := url.Parse(f.URL); err == nil {
				lmt = u.Query().Get("lmt")
			}
		}
		format := Format{
			Itag: f.Itag, Container: mime[2], Codecs: mime[3], Width: f.Width, Height: f.Height, FPS: f.FPS,
			Bitrate: bitrate, Length: number(f.ContentLength), InitEnd: number(f.InitRange.End), IndexEnd: number(f.IndexRange.End),
			LMT: lmt, URL: f.URL, Duration: number(f.ApproxDurationMs) / 1000,
		}
		if mime[1] == "audio" {
			traits := &AudioTraits{Original: true}
			if f.AudioTrack != nil {
				traits.Original = f.AudioTrack.AudioIsDefault || strings.Contains(xtags, "original")
			}
			traits.DRC = f.IsDrc || strings.Contains(xtags, "drc")
			traits.Dubbed = (f.AudioTrack != nil && f.AudioTrack.IsAutoDubbed) || strings.Contains(xtags, "dubbed")
			format.Audio = traits
		}
		if format.Length > 0 && format.InitEnd > 0 && format.IndexEnd > format.InitEnd {
			out = append(out, format)
		}
	}
	return out
}

// number is Number(text), NaN for a missing value.
func number(text string) float64 {
	if text == "" {
		return math.NaN()
	}
	return rustle.Number(text)
}

var (
	fastCodec = regexp.MustCompile(`^(avc1|vp9|vp09\.00)`)
	vpCodec   = regexp.MustCompile(`^vp`)
)

// codecRank: H.264 and 8-bit VP9 decode fast; HDR VP9 needs tone mapping; AV1 decodes slowest.
func codecRank(codecs string) int {
	switch {
	case fastCodec.MatchString(codecs):
		return 0
	case vpCodec.MatchString(codecs):
		return 1
	}
	return 2
}

// sheetSide is the short side of the format contact sheets are decoded from. A cell is
// 159 px tall, so 480p still holds three times its detail; decoding two minutes for a
// frame a second took 0.5 s at 480p against 1.75 s at 1080p, from 18 MB of download
// against 75.
const sheetSide = 480

// ChooseFormat picks the format to read a track from. Video: the sharpest format whose
// short side is at most 1080 px (frames are fitted to 2000 px, so more is waste), in a
// codec ffmpeg decodes quickly, and of those the fewest bytes, since the download dominates
// a cold frame (VP9 is often half of H.264). AV1 only when nothing else exists. Sheet: of
// those codecs, the smallest format of at least sheetSide, else what video would pick.
// Audio: the original track at its best Opus or AAC bitrate, never an auto-dub or the
// loudness-normalized (DRC) copy.
func ChooseFormat(formats []Format, track string) *Format {
	if track == "video" || track == "sheet" {
		var video []Format
		fast := false
		for _, f := range formats {
			if f.Audio == nil && f.Width > 0 && f.Height > 0 {
				video = append(video, f)
				fast = fast || codecRank(f.Codecs) < 2
			}
		}
		if fast {
			var kept []Format
			for _, f := range video {
				if codecRank(f.Codecs) < 2 {
					kept = append(kept, f)
				}
			}
			video = kept
		}
		side := func(f Format) int { return min(f.Width, f.Height) }
		if track == "sheet" {
			var small []Format
			for _, f := range video {
				if side(f) >= sheetSide {
					small = append(small, f)
				}
			}
			if len(small) > 0 {
				sort.SliceStable(small, func(i, j int) bool {
					a, b := small[i], small[j]
					if side(a) != side(b) {
						return side(a) < side(b)
					}
					if codecRank(a.Codecs) != codecRank(b.Codecs) {
						return codecRank(a.Codecs) < codecRank(b.Codecs)
					}
					return a.Bitrate < b.Bitrate
				})
				return &small[0]
			}
		}
		var fitting []Format
		for _, f := range video {
			if side(f) <= 1080 {
				fitting = append(fitting, f)
			}
		}
		pool := video
		if len(fitting) > 0 {
			pool = fitting
		}
		if len(pool) == 0 {
			return nil
		}
		sort.SliceStable(pool, func(i, j int) bool {
			a, b := pool[i], pool[j]
			if side(a) != side(b) {
				if len(fitting) > 0 {
					return side(a) > side(b)
				}
				return side(a) < side(b)
			}
			if codecRank(a.Codecs) != codecRank(b.Codecs) {
				return codecRank(a.Codecs) < codecRank(b.Codecs)
			}
			return a.Bitrate < b.Bitrate
		})
		return &pool[0]
	}
	// Opus at 110-160 kb/s outranks AAC at 128 kb/s, so codec comes before bitrate.
	var audio []Format
	for _, f := range formats {
		if f.Audio != nil {
			audio = append(audio, f)
		}
	}
	if len(audio) == 0 {
		return nil
	}
	score := func(f Format) int {
		s := 0
		if !f.Audio.Original {
			s += 8
		}
		if f.Audio.Dubbed {
			s += 4
		}
		if f.Audio.DRC {
			s += 2
		}
		switch {
		case strings.HasPrefix(f.Codecs, "opus"):
		case strings.HasPrefix(f.Codecs, "mp4a"):
			s++
		default:
			s += 16
		}
		return s
	}
	sort.SliceStable(audio, func(i, j int) bool {
		if score(audio[i]) != score(audio[j]) {
			return score(audio[i]) < score(audio[j])
		}
		return audio[i].Bitrate > audio[j].Bitrate
	})
	return &audio[0]
}
