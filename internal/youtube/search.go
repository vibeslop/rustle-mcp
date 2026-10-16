package youtube

import (
	"net/url"
	"regexp"
	"strings"
	"sync"

	"github.com/vibeslop/rustle-mcp/internal/jsonv"
	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

var videoID = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

// Protobuf filters: search results of type video; a channel's Videos and Search tabs.
const (
	videosOnly = "EgIQAQ=="
	videosTab  = "EgZ2aWRlb3PyBgQKAjoA"
	searchTab  = "EgZzZWFyY2jyBgQKAloA"
)

func text(value any) string { return rustle.Cut(TextOf(value), 300) }

var bareCount = regexp.MustCompile(`^[\d.,]+\s?[KMB]?$`)

// Views says what a count counts: YouTube shows "155K" beside a play icon.
func Views(value string) string {
	clean := strings.Join(strings.FieldsFunc(value, rustle.IsSpace), " ")
	if bareCount.MatchString(clean) {
		return clean + " views"
	}
	return clean
}

var (
	durationBadge = regexp.MustCompile(`^[0-9:]+$`)
	viewWords     = regexp.MustCompile(`\bviews?\b|\bwatching\b`)
	ageWords      = regexp.MustCompile(`\bago\b|^(streamed|premiered|scheduled)\b`)
)

// lockup maps the newer card layout of channel pages onto videoRenderer's fields.
func lockup(card *jsonv.Object) *jsonv.Object {
	metadata := jsonv.Obj(jsonv.Get(card, "metadata", "lockupMetadataViewModel"))
	video := &jsonv.Object{}
	set := func(key string, value any) {
		for i, k := range video.Keys {
			if k == key {
				video.Values[i] = value
				return
			}
		}
		video.Keys = append(video.Keys, key)
		video.Values = append(video.Values, value)
	}
	simple := func(s string) *jsonv.Object {
		return &jsonv.Object{Keys: []string{"simpleText"}, Values: []any{s}}
	}
	title := metadata.Get("title")
	if title == nil {
		title = &jsonv.Object{}
	}
	set("videoId", card.Get("contentId"))
	set("title", title)
	image := card.Get("contentImage")
	if image == nil {
		image = &jsonv.Object{}
	}
	jsonv.Walk(image, func(child *jsonv.Object) bool {
		badge := jsonv.Obj(child.Get("thumbnailBadgeViewModel"))
		if badge != nil && durationBadge.MatchString(jsonv.String(badge.Get("text"))) {
			set("lengthText", simple(jsonv.String(badge.Get("text"))))
			return false
		}
		return true
	})
	for index, row := range jsonv.Arr(jsonv.Get(metadata, "metadata", "contentMetadataViewModel", "metadataRows")) {
		for _, part := range jsonv.Arr(jsonv.Get(row, "metadataParts")) {
			value := text(jsonv.Get(part, "text"))
			if value == "" {
				continue
			}
			label := strings.ToLower(jsonv.String(jsonv.Get(part, "accessibilityLabel")))
			icon := jsonv.String(jsonv.Get(part, "leadingIcon", "name"))
			switch {
			case strings.Contains(icon, "PLAY") || viewWords.MatchString(label+" "+strings.ToLower(value)):
				set("shortViewCountText", simple(value))
			case ageWords.MatchString(strings.ToLower(value)):
				set("publishedTimeText", simple(value))
			case index == 0 && video.Get("ownerText") == nil:
				set("ownerText", simple(value))
			}
		}
	}
	return video
}

// Results is the videos in a search or browse response, in YouTube's order, without thumbnails.
func Results(data any, limit int) ([]rustle.VideoRow, error) {
	if jsonv.Obj(data) == nil || !jsonv.Truthy(jsonv.Get(data, "contents")) {
		return nil, rustle.Fail("YouTube did not return public video results.", "UPSTREAM_UNAVAILABLE",
			"Wait a minute and retry; consent pages or rate limits can block free browsing from this connection.").Retry(0)
	}
	pageChannel := text(jsonv.Get(data, "metadata", "channelMetadataRenderer", "title"))
	rows := []rustle.VideoRow{}
	seen := map[string]bool{}
	jsonv.Walk(data, func(node *jsonv.Object) bool {
		video := jsonv.Obj(node.Get("videoRenderer"))
		if video == nil {
			video = jsonv.Obj(node.Get("gridVideoRenderer"))
		}
		if video == nil {
			card := jsonv.Obj(node.Get("lockupViewModel"))
			if card == nil || jsonv.String(card.Get("contentType")) != "LOCKUP_CONTENT_TYPE_VIDEO" {
				return true
			}
			video = lockup(card)
		}
		id, ok := video.Get("videoId").(string)
		if !ok || !videoID.MatchString(id) || seen[id] {
			return true
		}
		seen[id] = true
		row := rustle.VideoRow{Title: text(video.Get("title"))}
		owner := video.Get("ownerText")
		if owner == nil {
			owner = video.Get("shortBylineText")
		}
		row.Channel = text(owner)
		if row.Channel == "" {
			row.Channel = pageChannel
		}
		row.Duration = text(video.Get("lengthText"))
		count := video.Get("shortViewCountText")
		if count == nil {
			count = video.Get("viewCountText")
		}
		row.Views = Views(text(count))
		row.Published = text(video.Get("publishedTimeText"))
		row.URL = Origin + "/watch?v=" + id
		rows = append(rows, row)
		return len(rows) < limit
	})
	return rows, nil
}

// Search uses InnerTube search, which answers in ~0.37 s against ~0.48 s for the results
// page, in under a third of the bytes.
func Search(c *rustle.Call, query string, limit int) ([]rustle.VideoRow, error) {
	data, _, err := innertube(c.Ctx, "search", map[string]any{"query": query, "params": videosOnly}, innertubeOptions{client: "WEB"})
	if err != nil {
		return nil, err
	}
	return Results(data, limit)
}

func notChannel() *rustle.Error {
	return rustle.Fail("Use a YouTube channel URL, channel ID or @handle.", "INVALID_ARGUMENTS",
		"Pass channel as a link (https://www.youtube.com/@name), a UC... channel ID or an @handle.")
}

var (
	httpPrefix   = regexp.MustCompile(`(?i)^https?://`)
	channelTab   = regexp.MustCompile(`/(videos|shorts|streams|featured|search|about)$`)
	channelID    = regexp.MustCompile(`^UC[A-Za-z0-9_-]{22}$`)
	channelKind  = regexp.MustCompile(`^(@|channel/|c/|user/)`)
	channelShape = regexp.MustCompile(`^(?:@[^/?#\s]+|(?:channel|c|user)/[A-Za-z0-9_.-]+)$`)
)

// ChannelPath reads "@name", "channel/UC...", "c/name" or "user/name" from any of the
// forms people paste.
func ChannelPath(channel string) (string, error) {
	path := channel
	if httpPrefix.MatchString(channel) {
		u, err := url.Parse(channel)
		if err != nil {
			return "", notChannel()
		}
		host := strings.ToLower(u.Hostname())
		port := u.Port()
		if (host != "youtube.com" && host != "www.youtube.com" && host != "m.youtube.com") || u.User != nil ||
			(port != "" && port != "80" && port != "443") {
			return "", notChannel()
		}
		path = u.EscapedPath()
		if decoded, err := url.PathUnescape(path); err == nil {
			path = decoded
		}
	}
	path = channelTab.ReplaceAllString(strings.Trim(rustle.TrimSpace(path), "/"), "")
	if channelID.MatchString(path) {
		path = "channel/" + path
	} else if !channelKind.MatchString(path) {
		path = "@" + path
	}
	if !channelShape.MatchString(path) {
		return "", notChannel()
	}
	return path, nil
}

var browseIDs sync.Map

func browseID(c *rustle.Call, path string) (string, error) {
	if id, ok := strings.CutPrefix(path, "channel/"); ok {
		return id, nil
	}
	if known, ok := browseIDs.Load(path); ok {
		return known.(string), nil
	}
	response, _, err := innertube(c.Ctx, "navigation/resolve_url", map[string]any{"url": Origin + "/" + path},
		innertubeOptions{client: "WEB", allow: []int{400, 404}})
	if err != nil {
		return "", err
	}
	id, _ := jsonv.Get(response, "endpoint", "browseEndpoint", "browseId").(string)
	if !strings.HasPrefix(id, "UC") {
		return "", rustle.Fail("YouTube has no channel at "+path+".", "SOURCE_NOT_FOUND",
			"Check the handle, or find the channel with find_videos(query=...).")
	}
	browseIDs.Store(path, id)
	return id, nil
}

// encodeURIComponent escapes as JavaScript's encodeURIComponent does.
func encodeURIComponent(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.IndexByte("-_.!~*'()", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteString("%" + strings.ToUpper(string("0123456789abcdef"[c>>4])) + strings.ToUpper(string("0123456789abcdef"[c&15])))
	}
	return b.String()
}

// formEncode encodes a value as URLSearchParams does: spaces as +, and every byte escaped
// but letters, digits and *-._.
func formEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ':
			b.WriteByte('+')
		case c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.IndexByte("*-._", c) >= 0:
			b.WriteByte(c)
		default:
			b.WriteString("%" + strings.ToUpper(string("0123456789abcdef"[c>>4])) + strings.ToUpper(string("0123456789abcdef"[c&15])))
		}
	}
	return b.String()
}

// ChannelVideos lists a channel's latest videos, or its search results when query is set.
// source is the listing's URL.
func ChannelVideos(c *rustle.Call, channel, query string, limit int) (source string, rows []rustle.VideoRow, err error) {
	path, err := ChannelPath(channel)
	if err != nil {
		return "", nil, err
	}
	searching := rustle.TrimSpace(query) != ""
	escaped := strings.ReplaceAll(strings.ReplaceAll(encodeURIComponent(path), "%2F", "/"), "%40", "@")
	source = Origin + "/" + escaped + "/videos"
	if searching {
		source = Origin + "/" + escaped + "/search?query=" + formEncode(query)
	}
	id, err := browseID(c, path)
	if err != nil {
		return "", nil, err
	}
	body := map[string]any{"browseId": id, "params": videosTab}
	if searching {
		body = map[string]any{"browseId": id, "params": searchTab, "query": query}
	}
	data, _, err := innertube(c.Ctx, "browse", body, innertubeOptions{client: "WEB"})
	if err != nil {
		return "", nil, err
	}
	rows, err = Results(data, limit)
	return source, rows, err
}
