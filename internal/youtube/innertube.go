// Package youtube reads YouTube: player requests, captions, DASH byte-range windows,
// storyboards and search, without an API key, a signed-in account or the player script.
package youtube

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/vibeslop/rustle-mcp/internal/fetch"
	"github.com/vibeslop/rustle-mcp/internal/jsonv"
	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

const Origin = "https://www.youtube.com"

const (
	safariUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 15_7_3) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.0 Safari/605.1.15"
	vrUA     = "com.google.android.apps.youtube.vr.oculus/1.65.10 (Linux; U; Android 12L; eureka-user Build/SQ3A.220605.009.A1) gzip"
)

type client struct {
	id      string
	ua      string
	context map[string]any
}

var clients = map[string]client{
	"VISIONOS": {id: "101", ua: safariUA, context: map[string]any{
		"clientName": "VISIONOS", "clientVersion": "1.02", "deviceMake": "Apple", "deviceModel": "RealityDevice17,1",
		"osName": "visionOS", "osVersion": "26.5.23O471", "hl": "en", "gl": "US", "userAgent": safariUA,
	}},
	"ANDROID_VR": {id: "28", ua: vrUA, context: map[string]any{
		"clientName": "ANDROID_VR", "clientVersion": "1.65.10", "deviceMake": "Oculus", "deviceModel": "Quest 3",
		"osName": "Android", "osVersion": "12L", "androidSdkVersion": "32", "hl": "en", "gl": "US",
	}},
	"WEB": {id: "1", ua: fetch.BrowserUA, context: map[string]any{"clientName": "WEB", "clientVersion": "2.20261013.00.00", "hl": "en", "gl": "US"}},
}

// resolvers: VISIONOS and ANDROID_VR need neither YouTube's player script nor a BotGuard
// token, and their caption URLs work without a proof-of-origin token. VISIONOS answers as
// fast (median 72 ms against 98 ms) and its stream URLs serve any byte range, so the media
// code can use the record's formats as they are; ANDROID_VR stands in when it refuses.
var resolvers = []string{"VISIONOS", "ANDROID_VR"}

var errorStatuses = []int{400, 401, 403, 404, 405, 409, 410, 429, 500, 502, 503, 504}

func changed(what string) *rustle.Error {
	return rustle.Fail(fmt.Sprintf("YouTube changed its %s, which this version of Rustle cannot read.", what), "YOUTUBE_CHANGED",
		"Update rustle-mcp; until then use a direct media URL or a local file.")
}

// blocked is the recovery when YouTube refuses every route the server has. A hosted
// server's WARP tunnel can be refused too, and a proxy is the route left to add.
const blocked = "Retry in a few minutes. On a hosted server, a residential proxy set as HTTPS_PROXY gives Rustle another route to YouTube. A direct media URL or local file avoids YouTube."

// HTTPError maps an HTTP status from YouTube to an error an agent can act on.
func HTTPError(status int) *rustle.Error {
	switch status {
	case 429:
		return rustle.Fail("YouTube is rate limiting this connection (HTTP 429).", "RATE_LIMITED",
			"Wait several minutes before retrying; fewer YouTube requests per minute avoid this.").Retry(300)
	case 401, 403:
		return rustle.Fail(fmt.Sprintf("YouTube rejected the request (HTTP %d).", status), "YOUTUBE_ACCESS_DENIED",
			blocked)
	}
	e := rustle.Fail(fmt.Sprintf("YouTube returned HTTP %d.", status), "UPSTREAM_UNAVAILABLE", "Wait briefly, then retry; verify the video link if it recurs.")
	e.Retryable = status >= 500
	return e
}

type innertubeOptions struct {
	client      string
	visitorData string
	// fields is Google's response field mask, e.g. "storyboards"; the response then carries
	// only those fields.
	fields string
	// allow are statuses answered with (nil, status) instead of an error.
	allow []int
}

// innertube POSTs an InnerTube endpoint; HTTP failures become actionable errors.
func innertube(ctx context.Context, endpoint string, body map[string]any, o innertubeOptions) (any, int, error) {
	c := clients[o.client]
	headers := map[string]string{
		"content-type":             "application/json",
		"user-agent":               c.ua,
		"x-youtube-client-name":    c.id,
		"x-youtube-client-version": c.context["clientVersion"].(string),
		"origin":                   Origin,
	}
	if o.visitorData != "" {
		headers["x-goog-visitor-id"] = o.visitorData
	}
	if o.fields != "" {
		headers["x-goog-fieldmask"] = o.fields
	}
	context := c.context
	if o.visitorData != "" {
		context = map[string]any{"visitorData": o.visitorData}
		for k, v := range c.context {
			context[k] = v
		}
	}
	request := map[string]any{"context": map[string]any{"client": context}}
	for k, v := range body {
		request[k] = v
	}
	payload, _ := json.Marshal(request)
	// Player requests answer in about 80 ms; search and browse take 300-500 ms of server time.
	hedge := 1500 * time.Millisecond
	if endpoint == "player" {
		hedge = 300 * time.Millisecond
	}
	response, err := fetch.Get(ctx, Origin+"/youtubei/v1/"+endpoint+"?prettyPrint=false", fetch.Options{
		Method: "POST", Headers: headers, Body: payload, Limit: 16 << 20, Timeout: 15 * time.Second,
		What: "YouTube", Allow: errorStatuses, Hedge: hedge,
	})
	if err != nil {
		return nil, 0, err
	}
	for _, s := range o.allow {
		if s == response.Status {
			return nil, response.Status, nil
		}
	}
	// The requests are fixed, so a 400 means YouTube no longer accepts this client's.
	if response.Status == 400 {
		return nil, 0, changed(endpoint + " API")
	}
	if response.Status >= 400 {
		return nil, 0, HTTPError(response.Status)
	}
	data, err := jsonv.Parse(response.Body)
	if err != nil {
		return nil, 0, changed(endpoint + " response")
	}
	return data, response.Status, nil
}

// Without visitor data the player answers "Sign in to confirm you're not a bot". The
// service worker's data file carries a fresh visitor id in 2.5 KB, and one id serves many
// videos, so it is kept for half a day across processes.
const visitorTTL = 12 * time.Hour

type visitorID struct {
	Value string `json:"value"`
	At    int64  `json:"at"`
}

var visitor struct {
	sync.Mutex
	known  *visitorID
	flight rustle.Flight[string]
}

// VisitorData is a visitor id for player requests: from memory, the cache folder, or a
// fresh one from YouTube.
func VisitorData(c *rustle.Call, fresh bool) (string, error) {
	file := filepath.Join(c.Settings.CacheDir, "visitor.json")
	if !fresh {
		visitor.Lock()
		if visitor.known == nil {
			var stored visitorID
			if data, err := os.ReadFile(file); err == nil && json.Unmarshal(data, &stored) == nil && stored.Value != "" {
				visitor.known = &stored
			}
		}
		known := visitor.known
		visitor.Unlock()
		if known != nil && time.Since(time.UnixMilli(known.At)) < visitorTTL {
			return known.Value, nil
		}
	}
	// Shared work: one caller's cancellation must not fail the others.
	shared := context.WithoutCancel(c.Ctx)
	return visitor.flight.Do(c.Ctx, "visitor", func() (string, error) {
		response, err := fetch.Get(shared, Origin+"/sw.js_data", fetch.Options{Timeout: 15 * time.Second, What: "YouTube", Limit: 1 << 20, Hedge: 400 * time.Millisecond})
		if err != nil {
			return "", err
		}
		data, _ := jsonv.Parse(bytes.TrimPrefix(response.Body, []byte(")]}'")))
		value, _ := index(data, 0, 2, 0, 0, 13).(string)
		if value == "" {
			return "", changed("visitor data")
		}
		id := &visitorID{Value: value, At: time.Now().UnixMilli()}
		visitor.Lock()
		visitor.known = id
		visitor.Unlock()
		if os.MkdirAll(c.Settings.CacheDir, 0o700) == nil {
			if data, err := json.Marshal(id); err == nil {
				_ = rustle.WriteAtomic(file, data)
			}
		}
		return value, nil
	})
}

// index follows array positions, as v[a][b]... does; nil when one is missing.
func index(v any, positions ...int) any {
	for _, p := range positions {
		list := jsonv.Arr(v)
		if p >= len(list) {
			return nil
		}
		v = list[p]
	}
	return v
}

var notABot = regexp.MustCompile(`(?i)not a bot`)

func isBotCheck(response any) bool {
	status := jsonv.Get(response, "playabilityStatus")
	return jsonv.String(jsonv.Get(status, "status")) == "LOGIN_REQUIRED" && notABot.MatchString(jsonv.String(jsonv.Get(status, "reason")))
}

func live() *rustle.Error {
	return rustle.Fail("Live streams are not supported.", "LIVE_UNSUPPORTED", "Wait for the finished recording to be published, then pass that link.")
}

var (
	regionText     = regexp.MustCompile(`(?i)country|region`)
	restrictedText = regexp.MustCompile(`(?i)private|members|sign in|\bage\b`)
	liveText       = regexp.MustCompile(`(?i)\blive (stream|event)|premiere`)
)

// PlayabilityError is the error for a player response that refuses playback, or nil when it plays.
func PlayabilityError(status, details any) *rustle.Error {
	state := jsonv.String(jsonv.Get(status, "status"))
	if state == "OK" {
		return nil
	}
	screen := jsonv.Get(status, "errorScreen", "playerErrorMessageRenderer")
	reasonText := jsonv.String(jsonv.Get(status, "reason"))
	if !jsonv.Truthy(jsonv.Get(status, "reason")) {
		reasonText = TextOf(jsonv.Get(screen, "reason"))
	}
	reason := rustle.Cut(rustle.CollapseSpace(reasonText), 200)
	text := reason + " " + TextOf(jsonv.Get(screen, "subreason"))
	switch {
	case notABot.MatchString(text):
		return rustle.Fail("YouTube treats this connection as automated and refused playback.", "YOUTUBE_ACCESS_DENIED",
			blocked)
	case state == "LIVE_STREAM_OFFLINE":
		return live()
	case regionText.MatchString(text):
		return rustle.Fail("YouTube blocks this video in the server's region.", "SOURCE_RESTRICTED",
			"Choose another video or run the server from a permitted region.")
	case state == "LOGIN_REQUIRED" || state == "AGE_CHECK_REQUIRED" || state == "AGE_VERIFICATION_REQUIRED" || state == "CONTENT_CHECK_REQUIRED" || restrictedText.MatchString(text):
		says := ""
		if reason != "" {
			says = " (YouTube says: " + reason + ")"
		}
		return rustle.Fail("This video needs a signed-in YouTube account, which this server does not use"+says+".",
			"SOURCE_RESTRICTED", "Choose a public video, a direct media URL, or a local file.")
	case jsonv.Truthy(jsonv.Get(details, "isLive")) || jsonv.Truthy(jsonv.Get(details, "isUpcoming")) || jsonv.Truthy(jsonv.Get(details, "isLiveContent")) || liveText.MatchString(text):
		return live()
	case state == "ERROR" || state == "UNPLAYABLE":
		says := ""
		if reason != "" {
			says = " (" + reason + ")"
		}
		return rustle.Fail("YouTube reports the video as unavailable"+says+".", "SOURCE_NOT_FOUND",
			"Verify the link in a browser or choose another video.")
	}
	return changed("player response")
}

// num is a JSON value as a number when it is one, or a numeric string; false otherwise.
func num(value any) (float64, bool) {
	if value == nil || value == "" {
		return 0, false
	}
	n := rustle.Number(value)
	return n, rustle.Finite(n)
}

// TrimFormats keeps the adaptive formats with the fields media code reads. A dubbed video
// lists ~80 dubbed audio formats; only the original's are kept.
func TrimFormats(raw any) []rustle.YtFormat {
	var formats []*jsonv.Object
	for _, f := range jsonv.Objects(raw) {
		if _, ok := f.Get("mimeType").(string); ok {
			formats = append(formats, f)
		}
	}
	dubbed := func(f *jsonv.Object) bool {
		track := jsonv.Obj(f.Get("audioTrack"))
		return track != nil && (jsonv.Truthy(track.Get("isAutoDubbed")) ||
			!(jsonv.Truthy(track.Get("audioIsDefault")) || strings.HasSuffix(jsonv.String(track.Get("id")), ".4")))
	}
	original := false
	for _, f := range formats {
		if strings.HasPrefix(jsonv.String(f.Get("mimeType")), "audio/") && !dubbed(f) {
			original = true
		}
	}
	out := []rustle.YtFormat{}
	for _, f := range formats {
		if original && dubbed(f) {
			continue
		}
		out = append(out, trimFormat(f))
	}
	return out
}

func trimFormat(f *jsonv.Object) rustle.YtFormat {
	n := func(key string) float64 {
		v, _ := jsonv.Num(f.Get(key))
		return v
	}
	s := func(key string) string {
		if v := f.Get(key); v != nil {
			return jsonv.String(v)
		}
		return ""
	}
	byteRange := func(key string) *rustle.YtRange {
		r := jsonv.Obj(f.Get(key))
		if r == nil {
			return nil
		}
		return &rustle.YtRange{Start: jsonv.String(r.Get("start")), End: jsonv.String(r.Get("end"))}
	}
	out := rustle.YtFormat{
		Itag: int(n("itag")), MimeType: s("mimeType"), URL: s("url"), Bitrate: n("bitrate"), AverageBitrate: n("averageBitrate"),
		Width: int(n("width")), Height: int(n("height")), FPS: n("fps"), ContentLength: s("contentLength"),
		ApproxDurationMs: s("approxDurationMs"), LastModified: s("lastModified"), Xtags: s("xtags"), Type: s("type"),
		IsDrc: jsonv.Truthy(f.Get("isDrc")), InitRange: byteRange("initRange"), IndexRange: byteRange("indexRange"),
	}
	if track := jsonv.Obj(f.Get("audioTrack")); track != nil {
		out.AudioTrack = &rustle.YtAudioTrack{
			ID: jsonv.String(track.Get("id")), DisplayName: jsonv.String(track.Get("displayName")),
			AudioIsDefault: jsonv.Truthy(track.Get("audioIsDefault")), IsAutoDubbed: jsonv.Truthy(track.Get("isAutoDubbed")),
		}
	}
	return out
}

// chosen is the tracks the media code will fetch (its own rule, so the record's size and
// frame rate describe the frames that are decoded), else the first video format of at most 1080p.
func chosen(formats []rustle.YtFormat) (video, audio *rustle.YtFormat) {
	parsed := ParseFormats(formats)
	byItag := func(f *Format) *rustle.YtFormat {
		if f == nil {
			return nil
		}
		for i := range formats {
			if formats[i].Itag == f.Itag {
				return &formats[i]
			}
		}
		return nil
	}
	video = byItag(ChooseFormat(parsed, "video"))
	if video == nil {
		for i := range formats {
			f := &formats[i]
			if strings.HasPrefix(f.MimeType, "video/") && f.Width > 0 && f.Height > 0 {
				if video == nil {
					video = f
				}
				if min(f.Width, f.Height) <= 1080 {
					video = f
					break
				}
			}
		}
	}
	return video, byItag(ChooseFormat(parsed, "audio"))
}

// PlayerRecord is the record for a player response.
func PlayerRecord(id, key string, response any, client, visitor string) (*rustle.Record, error) {
	status := jsonv.Obj(jsonv.Get(response, "playabilityStatus"))
	if status == nil {
		return nil, changed("player response")
	}
	if _, ok := status.Get("status").(string); !ok {
		return nil, changed("player response")
	}
	details := jsonv.Obj(jsonv.Get(response, "videoDetails"))
	if details == nil {
		details = &jsonv.Object{}
	}
	if refused := PlayabilityError(status, details); refused != nil {
		return nil, refused
	}
	streaming := jsonv.Get(response, "streamingData")
	formats := TrimFormats(jsonv.Get(streaming, "adaptiveFormats"))
	video, audio := chosen(formats)
	// The video track's duration to the millisecond, as the media reports it.
	length, hasLength := num(details.Get("lengthSeconds"))
	ms, hasMS := 0.0, false
	if video != nil {
		ms, hasMS = num(video.ApproxDurationMs)
	}
	if !hasMS {
		for _, f := range formats {
			if strings.HasPrefix(f.MimeType, "video/") {
				if v, ok := num(f.ApproxDurationMs); ok && v != 0 {
					ms, hasMS = v, true
					break
				}
			}
		}
	}
	duration := length
	if hasMS && ms != 0 {
		duration = ms / 1000
	} else if !hasLength {
		duration = 0
	}
	if jsonv.Truthy(details.Get("isLive")) || !(duration > 0) {
		if jsonv.Truthy(details.Get("isLive")) || jsonv.Truthy(details.Get("isLiveContent")) || jsonv.Truthy(details.Get("isUpcoming")) {
			return nil, live()
		}
		return nil, changed("player response")
	}
	record := &rustle.Record{
		Key: key, Kind: "youtube", Source: Origin + "/watch?v=" + id, ResolvedAt: time.Now().UnixMilli(), Duration: duration,
		HasVideo: rustle.Bool(true), HasAudio: rustle.Bool(true),
	}
	record.Title = rustle.Cut(rustle.TrimSpace(jsonv.String(details.Get("title"))), 300)
	if record.Title == "" {
		record.Title = "YouTube video"
	}
	if video != nil {
		record.Width, record.Height, record.FPS = video.Width, video.Height, video.FPS
	}
	func() {
		// A caption list YouTube garbled marks the record for a retry instead of failing it.
		defer func() {
			if recover() != nil {
				record.Captions, record.OriginalLanguage, record.CaptionsError = []rustle.Caption{}, "", true
			}
		}()
		renderer := jsonv.Get(response, "captions", "playerCaptionsTracklistRenderer")
		record.OriginalLanguage = OriginalLanguage(renderer, jsonv.Get(streaming, "adaptiveFormats"))
		record.Captions = CaptionTracks(jsonv.Get(renderer, "captionTracks"), record.OriginalLanguage)
	}()
	description := ""
	if d := details.Get("shortDescription"); d != nil {
		description = jsonv.String(d)
	}
	record.Chapters = DescriptionChapters(description, length)
	state := &rustle.YtState{ID: id, VisitorData: visitor, Client: client, Formats: formats}
	if expires, ok := num(jsonv.Get(streaming, "expiresInSeconds")); ok && expires != 0 {
		state.ExpiresAt = time.Now().UnixMilli() + int64(expires*1000)
	}
	if video != nil {
		state.Video = video.Itag
	}
	if audio != nil {
		state.Audio = audio.Itag
	}
	state.ServerAbrStreamingURL, _ = jsonv.Get(streaming, "serverAbrStreamingUrl").(string)
	state.VideoPlaybackUstreamerConfig, _ = jsonv.Get(response, "playerConfig", "mediaCommonConfig", "mediaUstreamerRequestConfig", "videoPlaybackUstreamerConfig").(string)
	state.Storyboard, _ = jsonv.Get(response, "storyboards", "playerStoryboardSpecRenderer", "spec").(string)
	record.YouTube = state
	return record, nil
}

func player(ctx context.Context, id, client, visitor string) (any, error) {
	data, _, err := innertube(ctx, "player", map[string]any{"videoId": id, "contentCheckOk": true, "racyCheckOk": true},
		innertubeOptions{client: client, visitorData: visitor})
	return data, err
}

// refused is true for an error that means YouTube distrusts the address the request came from.
func refused(err error) bool { return rustle.HasCode(err, "YOUTUBE_ACCESS_DENIED", "RATE_LIMITED") }

// rerouted runs work, and runs it again through the next route (RUSTLE_PROXIES) each time
// YouTube refuses the current one. Work starts over on the new route: media links are
// signed for the address that asked for them.
func rerouted[T any](work func() (T, error)) (T, error) {
	for {
		via := fetch.Route()
		v, err := work()
		if err == nil || !refused(err) || !fetch.Refuse(via) {
			return v, err
		}
	}
}

// Resolve reads a video's title, duration, captions, original language and chapters,
// without media: one visitor-data fetch (cached for hours) and one player request. The
// record's YouTube field holds whatever the media and storyboard code needs later.
func Resolve(c *rustle.Call, id, key string) (*rustle.Record, error) {
	return rerouted(func() (*rustle.Record, error) { return resolve(c, id, key) })
}

func resolve(c *rustle.Call, id, key string) (*rustle.Record, error) {
	visitor, err := VisitorData(c, false)
	if err != nil {
		return nil, err
	}
	refreshed := false
	var last error
	for _, client := range resolvers {
		record, err := func() (*rustle.Record, error) {
			response, err := player(c.Ctx, id, client, visitor)
			if err != nil {
				return nil, err
			}
			if isBotCheck(response) && !refreshed {
				// A visitor id YouTube has started to distrust is replaced once.
				refreshed = true
				if visitor, err = VisitorData(c, true); err != nil {
					return nil, err
				}
				if response, err = player(c.Ctx, id, client, visitor); err != nil {
					return nil, err
				}
			}
			return PlayerRecord(id, key, response, client, visitor)
		}()
		if err == nil {
			return record, nil
		}
		// Refusals another client may not share; rate limits and real restrictions are final.
		if !rustle.HasCode(err, "YOUTUBE_ACCESS_DENIED", "YOUTUBE_CHANGED") {
			return nil, err
		}
		last = err
	}
	return nil, last
}

var fmtParam = regexp.MustCompile(`([?&])fmt=[^&]*(&|$)`)

// TimedtextURL is the track URL without fmt: YouTube then serves srv1, one element per
// phrase, no rolling repeats or word timing.
func TimedtextURL(u string) string {
	m := fmtParam.FindStringSubmatchIndex(u)
	if m == nil {
		return u
	}
	keep := ""
	if m[5] > m[4] {
		keep = u[m[2]:m[3]]
	}
	return u[:m[0]] + keep + u[m[1]:]
}

// CaptionText is the caption track's text (timed-text XML), cached on disk per source.
func CaptionText(c *rustle.Call, record *rustle.Record, track rustle.Caption) (string, error) {
	keyText := rustle.Compact([]string{"srv1", track.Language, track.Kind, track.Name})
	name := "captions-" + rustle.HashKey(keyText)[:24] + ".xml"
	if cached := c.Cache.Read(record.Key, name); len(cached) > 0 {
		return string(cached), nil
	}
	response, err := rerouted(func() (*fetch.Response, error) {
		response, err := fetch.Get(c.Ctx, TimedtextURL(track.URL), fetch.Options{
			Limit: 8 << 20, Timeout: 20 * time.Second, What: "YouTube", Allow: errorStatuses, Hedge: 300 * time.Millisecond,
		})
		if err == nil && response.Status >= 400 {
			err = HTTPError(response.Status)
		}
		return response, err
	})
	if err != nil {
		return "", err
	}
	text := string(response.Body)
	if rustle.TrimSpace(text) == "" {
		return "", rustle.Fail("YouTube returned an empty caption file.", "CAPTIONS_UNAVAILABLE",
			"Retry in a minute, or pass transcribe=true to transcribe the speech locally.").Retry(0)
	}
	_, _ = c.Cache.Write(record.Key, name, response.Body)
	return text, nil
}
