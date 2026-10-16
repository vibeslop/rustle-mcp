package youtube

import (
	"strings"
	"testing"

	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

// Trimmed from real responses: a search result (videoRenderer) and a channel page card (lockupViewModel).
var searchResults = M{
	"contents": M{"twoColumnSearchResultsRenderer": M{"primaryContents": M{"sectionListRenderer": M{"contents": A{M{"itemSectionRenderer": M{"contents": A{
		M{"videoRenderer": M{
			"videoId": "Ab3dEf6hIj0", "title": M{"runs": A{M{"text": "Sourdough Is Easier Than I Expected"}}},
			"ownerText": M{"runs": A{M{"text": "BreadLab"}}}, "shortBylineText": M{"runs": A{M{"text": "BreadLab"}}},
			"lengthText":        M{"accessibility": M{"accessibilityData": M{"label": "12 minutes, 49 seconds"}}, "simpleText": "12:49"},
			"publishedTimeText": M{"simpleText": "10d ago"}, "viewCountText": M{"simpleText": "155,577 views"},
			"shortViewCountText": M{"accessibility": M{"accessibilityData": M{"label": "155 thousand views"}}, "simpleText": "155K"},
		}},
		M{"videoRenderer": M{"videoId": "Ab3dEf6hIj0", "title": M{"runs": A{M{"text": "duplicate"}}}}},
		M{"videoRenderer": M{
			"videoId": "Kl9mNo2pQr4", "title": M{"runs": A{M{"text": "What's new in home baking | Early look"}}}, "ownerText": M{"runs": A{M{"text": "2MinuteBakes"}}},
			"lengthText": M{"simpleText": "2:38"}, "publishedTimeText": M{"simpleText": "5mo ago"}, "viewCountText": M{"simpleText": "18,204 views"},
		}},
		M{"videoRenderer": M{"videoId": "not-an-id", "title": M{"runs": A{M{"text": "bad"}}}}},
	}}}}}}}},
}

var channelPage = M{
	"metadata": M{"channelMetadataRenderer": M{"title": "BreadLab"}},
	"contents": A{M{"lockupViewModel": M{
		"contentType": "LOCKUP_CONTENT_TYPE_VIDEO", "contentId": "St5uVw8xYz1",
		"contentImage": M{"thumbnailViewModel": M{"overlays": A{M{"thumbnailBottomOverlayViewModel": M{"badges": A{M{"thumbnailBadgeViewModel": M{"text": "15:48", "animatedText": "Now playing"}}}}}}}},
		"metadata": M{"lockupMetadataViewModel": M{
			"title": M{"content": "The Folding Method: Saving State Without Breaking Encapsulation"},
			"metadata": M{"contentMetadataViewModel": M{"metadataRows": A{M{"metadataParts": A{
				M{"text": M{"content": "42K"}, "accessibilityLabel": "42 thousand views", "leadingIcon": M{"name": "PLAY_ARROW_OUTLINED"}},
				M{"text": M{"content": "3d ago"}, "accessibilityLabel": "3 days ago"},
			}}}}},
		}},
	}}, M{"lockupViewModel": M{"contentType": "LOCKUP_CONTENT_TYPE_PLAYLIST", "contentId": "PL123"}}},
}

func TestSearch(t *testing.T) {
	t.Run("results keep YouTube order with views, age and channel", func(t *testing.T) {
		rows, err := Results(parse(t, searchResults), 10)
		if err != nil {
			t.Fatal(err)
		}
		equal(t, rows, []rustle.VideoRow{
			{Title: "Sourdough Is Easier Than I Expected", URL: "https://www.youtube.com/watch?v=Ab3dEf6hIj0", Channel: "BreadLab", Duration: "12:49", Views: "155K views", Published: "10d ago"},
			{Title: "What's new in home baking | Early look", URL: "https://www.youtube.com/watch?v=Kl9mNo2pQr4", Channel: "2MinuteBakes", Duration: "2:38", Views: "18,204 views", Published: "5mo ago"},
		}, "rows")
		if rows, _ := Results(parse(t, searchResults), 1); len(rows) != 1 {
			t.Errorf("%d rows with limit 1", len(rows))
		}
		_, err = Results(parse(t, M{"responseContext": M{}}), 3)
		expectCode(t, err, "UPSTREAM_UNAVAILABLE")
	})

	t.Run("channel cards carry views and the page channel", func(t *testing.T) {
		rows, err := Results(parse(t, channelPage), 10)
		if err != nil {
			t.Fatal(err)
		}
		equal(t, rows, []rustle.VideoRow{{
			Title: "The Folding Method: Saving State Without Breaking Encapsulation", URL: "https://www.youtube.com/watch?v=St5uVw8xYz1",
			Channel: "BreadLab", Duration: "15:48", Views: "42K views", Published: "3d ago",
		}}, "rows")
		if got := Views("1.2M"); got != "1.2M views" {
			t.Errorf("Views(1.2M) = %q", got)
		}
		if got := Views("1,204 watching"); got != "1,204 watching" {
			t.Errorf("Views(1,204 watching) = %q", got)
		}
	})

	t.Run("channel input forms", func(t *testing.T) {
		for _, c := range [][2]string{
			{"https://www.youtube.com/@mkbhd/videos", "@mkbhd"},
			{"mkbhd", "@mkbhd"},
			{"@BreadLab", "@BreadLab"},
			{"UCVhQ2NnY5Rskt6UjCUkJ_DA", "channel/UCVhQ2NnY5Rskt6UjCUkJ_DA"},
			{"https://m.youtube.com/channel/UCVhQ2NnY5Rskt6UjCUkJ_DA/featured", "channel/UCVhQ2NnY5Rskt6UjCUkJ_DA"},
			{"https://www.youtube.com/c/BlenderFoundation", "c/BlenderFoundation"},
			{"https://www.youtube.com/@%C3%84rjan", "@Ärjan"},
		} {
			if got, err := ChannelPath(c[0]); err != nil || got != c[1] {
				t.Errorf("ChannelPath(%q) = %q, %v; want %q", c[0], got, err, c[1])
			}
		}
		for _, bad := range []string{"https://evil.example/@mkbhd", "https://www.youtube.com:8080/@mkbhd", "user/bad name", "a/b/c"} {
			_, err := ChannelPath(bad)
			if !rustle.HasCode(err, "INVALID_ARGUMENTS") {
				t.Errorf("ChannelPath(%q): %v, want INVALID_ARGUMENTS", bad, err)
			}
		}
	})

	t.Run("search posts the videos-only filter", func(t *testing.T) {
		c := newCall(t)
		s := serve(t, func(request) any { return searchResults })
		rows, err := Search(c, "music & sound", 5)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 2 {
			t.Errorf("%d rows, want 2", len(rows))
		}
		call := s.Calls()[0]
		if call.URL != "https://www.youtube.com/youtubei/v1/search?prettyPrint=false" {
			t.Errorf("posted to %s", call.URL)
		}
		equal(t, []any{dig(call.Body, "query"), dig(call.Body, "params"), call.client()}, []any{"music & sound", "EgIQAQ==", "WEB"}, "query, params and client")
	})

	t.Run("channels resolve handles once, then browse their Videos or Search tab", func(t *testing.T) {
		c := newCall(t)
		s := serve(t, func(r request) any {
			if !strings.Contains(r.URL, "resolve_url") {
				return channelPage
			}
			if u, _ := dig(r.Body, "url").(string); strings.HasSuffix(u, "@nobody-here") {
				return status(404)
			}
			return M{"endpoint": M{"browseEndpoint": M{"browseId": "UCBJycsmduvYEL83R_U4JriQ"}}}
		})
		endpoints := func() []string {
			var out []string
			for _, call := range s.Calls() {
				_, after, _ := strings.Cut(call.URL, "/v1/")
				out = append(out, after)
			}
			return out
		}

		source, rows, err := ChannelVideos(c, "https://www.youtube.com/@mkbhd/videos", "", 5)
		if err != nil {
			t.Fatal(err)
		}
		if source != "https://www.youtube.com/@mkbhd/videos" {
			t.Errorf("source %s", source)
		}
		if rows[0].Channel != "BreadLab" {
			t.Errorf("channel %q", rows[0].Channel)
		}
		equal(t, endpoints(), []string{"navigation/resolve_url?prettyPrint=false", "browse?prettyPrint=false"}, "endpoints")
		calls := s.Calls()
		if u := dig(calls[0].Body, "url"); u != "https://www.youtube.com/@mkbhd" {
			t.Errorf("resolved %v", u)
		}
		equal(t, []any{dig(calls[1].Body, "browseId"), dig(calls[1].Body, "params")}, []any{"UCBJycsmduvYEL83R_U4JriQ", "EgZ2aWRlb3PyBgQKAjoA"}, "browse")

		s.Forget()
		source, _, err = ChannelVideos(c, "@mkbhd", "audio", 5)
		if err != nil {
			t.Fatal(err)
		}
		if source != "https://www.youtube.com/@mkbhd/search?query=audio" {
			t.Errorf("source %s", source)
		}
		calls = s.Calls()
		if len(calls) != 1 {
			t.Fatalf("%d requests, want 1: the handle is remembered", len(calls))
		}
		equal(t, []any{dig(calls[0].Body, "params"), dig(calls[0].Body, "query")}, []any{"EgZzZWFyY2jyBgQKAloA", "audio"}, "channel search")

		s.Forget()
		source, _, err = ChannelVideos(c, "UCVhQ2NnY5Rskt6UjCUkJ_DA", "", 5)
		if err != nil {
			t.Fatal(err)
		}
		if source != "https://www.youtube.com/channel/UCVhQ2NnY5Rskt6UjCUkJ_DA/videos" {
			t.Errorf("source %s", source)
		}
		if n := len(s.Calls()); n != 1 {
			t.Errorf("%d requests for a channel id, want 1", n)
		}
		_, _, err = ChannelVideos(c, "@nobody-here", "", 5)
		expectCode(t, err, "SOURCE_NOT_FOUND")
	})
}
