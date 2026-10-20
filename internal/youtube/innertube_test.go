package youtube

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vibeslop/rustle-mcp/internal/rustle"

	"github.com/vibeslop/rustle-mcp/internal/fetch"
)

func byteSpan(start, end string) M { return M{"start": start, "end": end} }

func videoFmt(itag int, codecs string, width, height int, bitrate float64, extra M) M {
	container := "webm"
	if strings.HasPrefix(codecs, "avc1") {
		container = "mp4"
	}
	return merge(M{
		"itag": itag, "mimeType": "video/" + container + `; codecs="` + codecs + `"`, "width": width, "height": height, "fps": 30, "bitrate": bitrate,
		"url": "https://rr.googlevideo.com/videoplayback?itag=" + strconv.Itoa(itag), "initRange": byteSpan("0", "740"), "indexRange": byteSpan("741", "1900"),
		"contentLength": strconv.FormatFloat(bitrate*3, 'f', -1, 64), "approxDurationMs": "19020", "lastModified": "1790977241727275", "quality": "hd", "projectionType": "RECTANGULAR",
	}, extra)
}

func audioFmt(itag int, codecs string, bitrate float64, track M) M {
	container := "mp4"
	if codecs == "opus" {
		container = "webm"
	}
	format := M{
		"itag": itag, "mimeType": "audio/" + container + `; codecs="` + codecs + `"`, "bitrate": bitrate,
		"url": "https://rr.googlevideo.com/videoplayback?itag=" + strconv.Itoa(itag), "initRange": byteSpan("0", "600"), "indexRange": byteSpan("601", "900"),
		"contentLength": "300000", "approxDurationMs": "19041", "lastModified": "1790110185814505",
	}
	if track != nil {
		format["audioTrack"] = track
	}
	return format
}

var (
	originalTrack = M{"displayName": "English (US) original", "id": "en-US.4", "audioIsDefault": true}
	playerFormats = A{
		videoFmt(313, "vp9", 3840, 2160, 17_000_000, nil),
		videoFmt(137, "avc1.640028", 1920, 1080, 1_977_904, nil),
		videoFmt(248, "vp9", 1920, 1080, 1_691_172, nil),
		videoFmt(160, "avc1.4D400C", 256, 144, 41_332, nil),
		audioFmt(140, "mp4a.40.2", 130_869, originalTrack),
		audioFmt(251, "opus", 112_412, originalTrack),
		audioFmt(251, "opus", 112_000, M{"displayName": "Arabic", "id": "ar.10", "audioIsDefault": false, "isAutoDubbed": true}),
	}
)

const vrSpec = "https://i.ytimg.com/sb/jNQXAC9IVRw/storyboard3_L$L/$N.jpg?sqp=a|48#27#100#10#10#0#default#rs$A|80#45#5#10#10#5000#M$M#rs$B"

type playerOptions struct {
	status, reason, description string
	details                     M
}

func playerResponse(o playerOptions) M {
	playability := M{"status": "OK"}
	if o.status != "" {
		playability["status"] = o.status
	}
	if o.reason != "" {
		playability["reason"] = o.reason
	}
	return M{
		"playabilityStatus": playability,
		"videoDetails": merge(M{
			"videoId": "jNQXAC9IVRw", "title": "  Me at the zoo ", "lengthSeconds": "19", "shortDescription": o.description, "isLiveContent": false,
		}, o.details),
		"streamingData": M{"expiresInSeconds": "21540", "adaptiveFormats": playerFormats, "serverAbrStreamingUrl": "https://rr.googlevideo.com/videoplayback?sabr=1"},
		"captions":      M{"playerCaptionsTracklistRenderer": single},
		"storyboards":   M{"playerStoryboardSpecRenderer": M{"spec": vrSpec}},
		"playerConfig":  M{"mediaCommonConfig": M{"mediaUstreamerRequestConfig": M{"videoPlaybackUstreamerConfig": "CpgJ"}}},
	}
}

const botCheck = "Sign in to confirm you’re not a bot"

func TestPlayerResponses(t *testing.T) {
	t.Run("a playable response becomes a record that needs no probing", func(t *testing.T) {
		record, err := PlayerRecord("jNQXAC9IVRw", "k", parse(t, playerResponse(playerOptions{description: "0:00 A\n0:05 B\n0:10 C\n0:30 Past the end"})), "VISIONOS", "V0")
		if err != nil {
			t.Fatal(err)
		}
		if record.Kind != "youtube" || record.Source != "https://www.youtube.com/watch?v=jNQXAC9IVRw" || record.Title != "Me at the zoo" {
			t.Errorf("kind %q, source %q, title %q", record.Kind, record.Source, record.Title)
		}
		if record.Duration != 19.02 {
			t.Errorf("duration %v, want 19.02", record.Duration)
		}
		// Frames come from at most 1080p, so the record describes that track, not the 4K one.
		equal(t, []any{record.Width, record.Height, record.FPS, record.Video(), record.Audio()}, []any{1920, 1080, 30.0, true, true}, "size, fps and tracks")
		if record.OriginalLanguage != "en" || !record.Captions[0].Original {
			t.Errorf("original language %q, first caption %+v", record.OriginalLanguage, record.Captions[0])
		}
		equal(t, record.Chapters, []rustle.Chapter{{At: 0, Title: "A"}, {At: 5, Title: "B"}, {At: 10, Title: "C"}}, "chapters")
		yt := record.YouTube
		if yt.ID != "jNQXAC9IVRw" || yt.Client != "VISIONOS" || yt.VisitorData != "V0" {
			t.Errorf("id %q, client %q, visitor %q", yt.ID, yt.Client, yt.VisitorData)
		}
		if yt.Storyboard != vrSpec || yt.VideoPlaybackUstreamerConfig != "CpgJ" {
			t.Errorf("storyboard %q, ustreamer config %q", yt.Storyboard, yt.VideoPlaybackUstreamerConfig)
		}
		if yt.ServerAbrStreamingURL == "" || yt.ExpiresAt <= time.Now().UnixMilli() {
			t.Errorf("SABR URL %q, expires at %d", yt.ServerAbrStreamingURL, yt.ExpiresAt)
		}
		if yt.Video != 137 && yt.Video != 248 {
			t.Errorf("video itag %d, want 137 or 248", yt.Video)
		}
		i := slices.IndexFunc(yt.Formats, func(f rustle.YtFormat) bool { return f.Itag == yt.Audio })
		if i < 0 || yt.Formats[i].AudioTrack == nil || yt.Formats[i].AudioTrack.ID != "en-US.4" {
			t.Errorf("audio itag %d is not the original track", yt.Audio)
		}
	})

	t.Run("formats keep what media code reads and drop auto-dubs", func(t *testing.T) {
		formats := TrimFormats(parse(t, playerFormats))
		if len(formats) != 6 {
			t.Fatalf("%d formats, want 6", len(formats))
		}
		for _, f := range formats {
			if f.AudioTrack != nil && f.AudioTrack.ID == "ar.10" {
				t.Error("the Arabic dub was kept")
			}
		}
		data, _ := json.Marshal(formats[0])
		if strings.Contains(string(data), `"quality"`) || strings.Contains(string(data), `"projectionType"`) {
			t.Errorf("unread fields kept: %s", data)
		}
		equal(t, formats[1].InitRange, &rustle.YtRange{Start: "0", End: "740"}, "init range")
		// When every audio track looks dubbed, keep them rather than lose the sound.
		if n := len(TrimFormats(parse(t, A{audioFmt(251, "opus", 1, M{"id": "ar.10", "isAutoDubbed": true})}))); n != 1 {
			t.Errorf("%d formats from one dubbed track, want 1", n)
		}
	})

	t.Run("refusals map to codes an agent can act on", func(t *testing.T) {
		region := M{"status": "UNPLAYABLE", "reason": "Video unavailable", "errorScreen": M{"playerErrorMessageRenderer": M{"subreason": M{"simpleText": "The uploader has not made this video available in your country"}}}}
		cases := []struct {
			status  M
			code    string
			details M
		}{
			{M{"status": "LOGIN_REQUIRED", "reason": botCheck}, "YOUTUBE_ACCESS_DENIED", nil},
			{M{"status": "LOGIN_REQUIRED", "reason": "Sign in to confirm your age"}, "SOURCE_RESTRICTED", nil},
			{M{"status": "LOGIN_REQUIRED", "reason": "This video is private"}, "SOURCE_RESTRICTED", nil},
			{M{"status": "UNPLAYABLE", "reason": "Join this channel to get access to members-only content like this video."}, "SOURCE_RESTRICTED", nil},
			{M{"status": "AGE_VERIFICATION_REQUIRED"}, "SOURCE_RESTRICTED", nil},
			{region, "SOURCE_RESTRICTED", nil},
			{M{"status": "ERROR", "reason": "This video is unavailable"}, "SOURCE_NOT_FOUND", nil},
			{M{"status": "UNPLAYABLE", "reason": "The page needs to be reloaded."}, "SOURCE_NOT_FOUND", nil},
			{M{"status": "UNPLAYABLE", "reason": "This live stream recording is not available."}, "LIVE_UNSUPPORTED", M{"isLiveContent": true}},
			{M{"status": "LIVE_STREAM_OFFLINE", "reason": "Premieres in 3 hours"}, "LIVE_UNSUPPORTED", nil},
			{M{"status": "SOMETHING_NEW"}, "YOUTUBE_CHANGED", nil},
		}
		for _, c := range cases {
			var details any
			if c.details != nil {
				details = parse(t, c.details)
			}
			e := PlayabilityError(parse(t, c.status), details)
			if e == nil || e.Code != c.code || e.Recovery == "" {
				t.Errorf("%v: got %+v, want %s with a recovery", c.status, e, c.code)
			}
		}
		if e := PlayabilityError(parse(t, M{"status": "OK"}), nil); e != nil {
			t.Errorf("OK refused: %+v", e)
		}
		if e := PlayabilityError(parse(t, M{"status": "LOGIN_REQUIRED", "reason": "This video is private"}), nil); !strings.Contains(e.Message, "private") {
			t.Errorf("message %q does not say private", e.Message)
		}
		if e := PlayabilityError(parse(t, region), nil); !strings.Contains(e.Recovery, "region") {
			t.Errorf("recovery %q does not mention the region", e.Recovery)
		}
	})

	t.Run("HTTP failures map to error codes, retryable where waiting helps", func(t *testing.T) {
		limited := HTTPError(429)
		equal(t, []any{limited.Code, limited.Retryable, limited.RetryAfter}, []any{"RATE_LIMITED", true, 300}, "429")
		if code := HTTPError(403).Code; code != "YOUTUBE_ACCESS_DENIED" {
			t.Errorf("403 is %s", code)
		}
		if e := HTTPError(503); e.Code != "UPSTREAM_UNAVAILABLE" || !e.Retryable {
			t.Errorf("503 is %s, retryable %v", e.Code, e.Retryable)
		}
		if HTTPError(400).Retryable {
			t.Error("400 is retryable")
		}
	})

	t.Run("malformed and live responses are refused", func(t *testing.T) {
		_, err := PlayerRecord("x", "k", parse(t, M{}), "VISIONOS", "V")
		expectCode(t, err, "YOUTUBE_CHANGED")
		_, err = PlayerRecord("x", "k", "<html>", "VISIONOS", "V")
		expectCode(t, err, "YOUTUBE_CHANGED")
		live := playerResponse(playerOptions{details: M{"isLive": true, "isLiveContent": true, "lengthSeconds": "0"}})
		live["streamingData"] = M{"hlsManifestUrl": "x"}
		_, err = PlayerRecord("x", "k", parse(t, live), "VISIONOS", "V")
		expectCode(t, err, "LIVE_UNSUPPORTED")
	})

	// The TypeScript test ("..., marked for a retry") threw from a getter inside the caption
	// list to reach the catch that sets captionsError. Parsed JSON has no getters, and the Go
	// caption readers take any shape without panicking, so the recover in PlayerRecord can't
	// be reached from a response. This checks that garbled shapes resolve without captions
	// and, as in TypeScript, without the retry mark.
	t.Run("a broken caption list still resolves", func(t *testing.T) {
		response := playerResponse(playerOptions{})
		response["captions"] = M{"playerCaptionsTracklistRenderer": M{
			"captionTracks": "odd", "audioTracks": M{"0": 1}, "defaultAudioTrackIndex": -1,
		}}
		record, err := PlayerRecord("x", "k", parse(t, response), "VISIONOS", "V")
		if err != nil {
			t.Fatal(err)
		}
		if len(record.Captions) != 0 || record.CaptionsError {
			t.Errorf("captions %v, retry mark %v from a garbled list", record.Captions, record.CaptionsError)
		}
	})
}

func TestResolveYouTube(t *testing.T) {
	t.Run("one player request with visitor data; a bot check gets one fresh visitor", func(t *testing.T) {
		c := newCall(t)
		refuseNext := false
		s := serve(t, func(r request) any {
			if isSwData(r) {
				return swData()
			}
			if refuseNext {
				refuseNext = false
				return playerResponse(playerOptions{status: "LOGIN_REQUIRED", reason: botCheck})
			}
			return playerResponse(playerOptions{})
		})
		record, err := Resolve(c, "jNQXAC9IVRw", "k1")
		if err != nil {
			t.Fatal(err)
		}
		posts := s.Posts()
		if len(posts) != 1 {
			t.Fatalf("%d POSTs, want 1", len(posts))
		}
		post := posts[0]
		if post.URL != "https://www.youtube.com/youtubei/v1/player?prettyPrint=false" {
			t.Errorf("posted to %s", post.URL)
		}
		if id := dig(post.Body, "videoId"); id != "jNQXAC9IVRw" {
			t.Errorf("videoId %v", id)
		}
		if post.client() != "VISIONOS" {
			t.Errorf("client %q", post.client())
		}
		if v := dig(post.Body, "context", "client", "visitorData"); v != record.YouTube.VisitorData {
			t.Errorf("body visitor %v, record %q", v, record.YouTube.VisitorData)
		}
		if v := post.Header.Get("x-goog-visitor-id"); v != record.YouTube.VisitorData {
			t.Errorf("header visitor %q, record %q", v, record.YouTube.VisitorData)
		}
		if _, err := os.Stat(filepath.Join(c.Settings.CacheDir, "visitor.json")); err != nil {
			t.Error(err)
		}

		refuseNext = true
		s.Forget()
		again, err := Resolve(c, "jNQXAC9IVRw", "k1")
		if err != nil {
			t.Fatal(err)
		}
		calls := s.Calls()
		var names []string
		for _, call := range calls {
			names = append(names, path.Base(call.URL))
		}
		equal(t, names, []string{"player?prettyPrint=false", "sw.js_data", "player?prettyPrint=false"}, "requests")
		if again.YouTube.VisitorData != lastVisitor() {
			t.Errorf("visitor %q, want %q", again.YouTube.VisitorData, lastVisitor())
		}
		if v := dig(calls[2].Body, "context", "client", "visitorData"); v != lastVisitor() {
			t.Errorf("retried with visitor %v, want %q", v, lastVisitor())
		}
	})

	t.Run("ANDROID_VR stands in when VISIONOS is refused; restrictions and rate limits are final", func(t *testing.T) {
		c := newCall(t)
		refuse := func(code int) func(request) any {
			return func(r request) any {
				if isSwData(r) {
					return swData()
				}
				if r.client() == "VISIONOS" {
					return status(code)
				}
				return playerResponse(playerOptions{})
			}
		}
		serve(t, refuse(403))
		record, err := Resolve(c, "jNQXAC9IVRw", "k2")
		if err != nil {
			t.Fatal(err)
		}
		if record.YouTube.Client != "ANDROID_VR" {
			t.Errorf("client %q after a 403", record.YouTube.Client)
		}
		serve(t, refuse(400))
		record, err = Resolve(c, "jNQXAC9IVRw", "k2")
		if err != nil {
			t.Fatal(err)
		}
		if record.YouTube.Client != "ANDROID_VR" {
			t.Errorf("client %q after a rejected request format", record.YouTube.Client)
		}

		s := serve(t, func(r request) any {
			if isSwData(r) {
				return swData()
			}
			return playerResponse(playerOptions{status: "LOGIN_REQUIRED", reason: "This video is private"})
		})
		_, err = Resolve(c, "jNQXAC9IVRw", "k3")
		expectCode(t, err, "SOURCE_RESTRICTED")
		if n := len(s.Posts()); n != 1 {
			t.Errorf("%d POSTs for a restricted video, want 1", n)
		}

		s = serve(t, func(r request) any {
			if isSwData(r) {
				return swData()
			}
			return status(429)
		})
		_, err = Resolve(c, "jNQXAC9IVRw", "k4")
		if limited := expectCode(t, err, "RATE_LIMITED"); limited.RetryAfter != 300 {
			t.Errorf("retry after %d, want 300", limited.RetryAfter)
		}
		if n := len(s.Posts()); n != 1 {
			t.Errorf("%d POSTs when rate limited, want 1", n)
		}

		serve(t, func(r request) any {
			if isSwData(r) {
				return swData()
			}
			return "not json"
		})
		_, err = Resolve(c, "jNQXAC9IVRw", "k5")
		expectCode(t, err, "YOUTUBE_CHANGED")
	})
}

func TestCaptionText(t *testing.T) {
	record := &rustle.Record{Key: "captions", Kind: "youtube", Source: "s", Title: "t"}
	track := rustle.Caption{Language: "en", Name: "English", Kind: "manual", URL: "https://www.youtube.com/api/timedtext?v=x&fmt=srv3&lang=en", Original: true}
	c := newCall(t)

	t.Run("fetches srv1 once, then reads the cached file", func(t *testing.T) {
		s := serve(t, func(request) any { return `<transcript><text start="0" dur="1">hi</text></transcript>` })
		for range 2 {
			text, err := CaptionText(c, record, track)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(text, "hi") {
				t.Errorf("text %q", text)
			}
		}
		calls := s.Calls()
		if len(calls) != 1 {
			t.Fatalf("%d requests, want 1", len(calls))
		}
		if calls[0].URL != "https://www.youtube.com/api/timedtext?v=x&lang=en" {
			t.Errorf("fetched %s", calls[0].URL)
		}
	})

	t.Run("an empty file is retryable and a rate limit says so", func(t *testing.T) {
		serve(t, func(request) any { return "  " })
		german := track
		german.Language = "de"
		_, err := CaptionText(c, record, german)
		if empty := expectCode(t, err, "CAPTIONS_UNAVAILABLE"); !empty.Retryable {
			t.Error("an empty caption file is not retryable")
		}
		serve(t, func(request) any { return status(429) })
		french := track
		french.Language = "fr"
		_, err = CaptionText(c, record, french)
		expectCode(t, err, "RATE_LIMITED")
	})

	t.Run("fmt is removed wherever it sits", func(t *testing.T) {
		for in, want := range map[string]string{
			"https://y/t?fmt=srv3&v=x":         "https://y/t?v=x",
			"https://y/t?v=x&fmt=srv3":         "https://y/t?v=x",
			"https://y/t?v=x&fmt=srv3&lang=en": "https://y/t?v=x&lang=en",
			"https://y/t?v=x&xfmt=1":           "https://y/t?v=x&xfmt=1",
		} {
			if got := TimedtextURL(in); got != want {
				t.Errorf("TimedtextURL(%q) = %q, want %q", in, got, want)
			}
		}
	})
}

// When YouTube refuses the route in use, the request goes out again through the next
// one in RUSTLE_PROXIES, which later requests keep using.
func TestResolveMovesToTheNextRoute(t *testing.T) {
	c := newCall(t)
	if err := fetch.SetRoutes([]string{"http://127.0.0.1:9", "direct"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fetch.SetRoutes(nil) })
	serve(t, func(r request) any {
		if isSwData(r) {
			return swData()
		}
		if fetch.Route() == 0 {
			return playerResponse(playerOptions{status: "LOGIN_REQUIRED", reason: botCheck})
		}
		return playerResponse(playerOptions{})
	})
	record, err := Resolve(c, "jNQXAC9IVRw", "k9")
	if err != nil {
		t.Fatal(err)
	}
	if record.YouTube == nil || fetch.Route() != 1 {
		t.Errorf("route %d after the first was refused", fetch.Route())
	}
}
