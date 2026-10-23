package youtube

import (
	"strconv"
	"testing"

	"github.com/vibeslop/rustle-mcp/internal/jsonv"
	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

func adaptive(itag int, mimeType string, extra M) M {
	return merge(M{
		"itag": itag, "mimeType": mimeType, "url": "https://rr.googlevideo.com/videoplayback?itag=" + strconv.Itoa(itag), "contentLength": "1000000",
		"initRange": byteSpan("0", "700"), "indexRange": byteSpan("701", "4000"), "approxDurationMs": "60000",
		"lastModified": "1700000000000000", "bitrate": 1000,
	}, extra)
}

// formatsOf is ParseFormats of formats in InnerTube's shape, each read as a player
// response's are. Unlike TrimFormats it keeps auto-dubs, so the choice is what skips them.
func formatsOf(t *testing.T, raw ...M) []Format {
	t.Helper()
	var trimmed []rustle.YtFormat
	for _, f := range raw {
		trimmed = append(trimmed, trimFormat(jsonv.Obj(parse(t, f))))
	}
	return ParseFormats(trimmed)
}

func chosenItag(formats []Format, track string) int {
	if f := ChooseFormat(formats, track); f != nil {
		return f.Itag
	}
	return 0
}

func TestFormatChoice(t *testing.T) {
	t.Run("video choice: sharpest up to 1080p, fewest bytes among fast codecs, AV1 last", func(t *testing.T) {
		formats := formatsOf(t,
			adaptive(313, `video/webm; codecs="vp9"`, M{"width": 3840, "height": 2160, "bitrate": 17e6}),
			adaptive(137, `video/mp4; codecs="avc1.640028"`, M{"width": 1920, "height": 1080, "bitrate": 4e6}),
			adaptive(248, `video/webm; codecs="vp9"`, M{"width": 1920, "height": 1080, "bitrate": 2e6}),
			adaptive(399, `video/mp4; codecs="av01.0.08M.08"`, M{"width": 1920, "height": 1080, "bitrate": 1e6}),
			adaptive(136, `video/mp4; codecs="avc1.4d401f"`, M{"width": 1280, "height": 720, "bitrate": 1e6}),
		)
		if itag := chosenItag(formats, "video"); itag != 248 {
			t.Errorf("chose %d, want 248", itag)
		}
		var without248 []Format
		for _, f := range formats {
			if f.Itag != 248 {
				without248 = append(without248, f)
			}
		}
		if itag := chosenItag(without248, "video"); itag != 137 {
			t.Errorf("without 248 chose %d, want 137", itag)
		}
		// Portrait: the short side decides, so 1080x1920 is "1080p".
		portrait := formatsOf(t,
			adaptive(1, `video/mp4; codecs="avc1.640028"`, M{"width": 1080, "height": 1920}),
			adaptive(2, `video/mp4; codecs="avc1.640028"`, M{"width": 2160, "height": 3840}),
		)
		if itag := chosenItag(portrait, "video"); itag != 1 {
			t.Errorf("portrait chose %d, want 1", itag)
		}
		// AV1 only when nothing else exists; past 1080p the smallest wins.
		av1 := formatsOf(t,
			adaptive(401, `video/mp4; codecs="av01.0.12M.08"`, M{"width": 3840, "height": 2160}),
			adaptive(400, `video/mp4; codecs="av01.0.12M.08"`, M{"width": 2560, "height": 1440}),
		)
		if itag := chosenItag(av1, "video"); itag != 400 {
			t.Errorf("AV1 past 1080p chose %d, want 400", itag)
		}
	})

	t.Run("sheet choice: the smallest fast format of at least 480p, else the video choice", func(t *testing.T) {
		formats := formatsOf(t,
			adaptive(248, `video/webm; codecs="vp9"`, M{"width": 1920, "height": 1080, "bitrate": 2e6}),
			adaptive(244, `video/webm; codecs="vp9"`, M{"width": 854, "height": 480, "bitrate": 5e5}),
			adaptive(135, `video/mp4; codecs="avc1.4d401f"`, M{"width": 854, "height": 480, "bitrate": 9e5}),
			adaptive(134, `video/mp4; codecs="avc1.4d401e"`, M{"width": 640, "height": 360, "bitrate": 3e5}),
			adaptive(397, `video/mp4; codecs="av01.0.04M.08"`, M{"width": 854, "height": 480, "bitrate": 2e5}),
		)
		if itag := chosenItag(formats, "sheet"); itag != 244 {
			t.Errorf("chose %d, want 244", itag)
		}
		small := formatsOf(t, adaptive(134, `video/mp4; codecs="avc1.4d401e"`, M{"width": 640, "height": 360}))
		if itag := chosenItag(small, "sheet"); itag != 134 {
			t.Errorf("with only 360p chose %d, want 134", itag)
		}
	})

	t.Run("audio choice: the original track, Opus before AAC, never DRC or auto-dubs", func(t *testing.T) {
		original := M{"audioTrack": M{"id": "en-US.4", "audioIsDefault": true}}
		dub := M{"audioTrack": M{"id": "de-DE.10", "audioIsDefault": false, "isAutoDubbed": true}}
		formats := formatsOf(t,
			adaptive(140, `audio/mp4; codecs="mp4a.40.2"`, merge(original, M{"bitrate": 130_000})),
			adaptive(251, `audio/webm; codecs="opus"`, merge(dub, M{"bitrate": 160_000})),
			adaptive(251, `audio/webm; codecs="opus"`, merge(original, M{"bitrate": 112_000})),
			adaptive(251, `audio/webm; codecs="opus"`, merge(original, M{"bitrate": 115_000, "isDrc": true})),
			adaptive(249, `audio/webm; codecs="opus"`, merge(original, M{"bitrate": 50_000})),
		)
		chosen := ChooseFormat(formats, "audio")
		if chosen == nil {
			t.Fatal("no audio chosen")
		}
		if chosen.Itag != 251 || chosen.Bitrate != 112_000 {
			t.Errorf("chose %d at %v b/s, want 251 at 112000", chosen.Itag, chosen.Bitrate)
		}
		equal(t, chosen.Audio, &AudioTraits{Original: true, DRC: false, Dubbed: false}, "audio traits")
	})

	t.Run("formats without a byte index (live, OTF) or URL are skipped", func(t *testing.T) {
		formats := formatsOf(t,
			adaptive(1, `video/mp4; codecs="avc1"`, M{"width": 640, "height": 360, "type": "FORMAT_STREAM_TYPE_OTF"}),
			adaptive(2, `video/mp4; codecs="avc1"`, M{"width": 640, "height": 360, "indexRange": nil}),
			adaptive(3, `video/mp4; codecs="avc1"`, M{"width": 640, "height": 360, "url": nil, "signatureCipher": "s=..."}),
			adaptive(4, `video/mp4; codecs="avc1"`, M{"width": 640, "height": 360}),
		)
		var itags []int
		for _, f := range formats {
			itags = append(itags, f.Itag)
		}
		equal(t, itags, []int{4}, "itags")
		if len(formats) == 1 && (formats[0].Container != "mp4" || formats[0].InitEnd != 700 || formats[0].IndexEnd != 4000) {
			t.Errorf("container %q, init end %v, index end %v", formats[0].Container, formats[0].InitEnd, formats[0].IndexEnd)
		}
	})
}
