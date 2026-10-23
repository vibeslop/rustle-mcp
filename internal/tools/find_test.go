package tools

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vibeslop/rustle-mcp/internal/fetch"
	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

// pages answers every request with an HTML page by URL ("*" for any other) and records
// the URLs asked for.
type pages struct {
	mu     sync.Mutex
	html   map[string]string
	called []string
}

func (p *pages) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	address := req.URL.String()
	p.mu.Lock()
	p.called = append(p.called, address)
	p.mu.Unlock()
	body, ok := p.html[address]
	if !ok {
		body, ok = p.html["*"]
	}
	status := http.StatusOK
	if !ok {
		status = http.StatusNotFound
	}
	if req.Method == http.MethodHead {
		body = ""
	}
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{"Content-Type": {"text/html; charset=utf-8"}}, Body: io.NopCloser(strings.NewReader(body)),
		ContentLength: -1, Request: req}, nil
}

func (p *pages) last() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.called) == 0 {
		return ""
	}
	return p.called[len(p.called)-1]
}

func serve(t *testing.T, html map[string]string) *pages {
	p := &pages{html: html}
	fetch.SetTransport(p)
	t.Cleanup(func() { fetch.SetTransport(nil) })
	return p
}

func findOK(t *testing.T, c *rustle.Call, args FindArgs) (*rustle.Result, string) {
	t.Helper()
	result, err := find(c, args)
	if err != nil {
		t.Fatal(err)
	}
	return result, result.Content[0].Text
}

func TestFindRendersTheFolderAsPathsWithSizeAndTime(t *testing.T) {
	root := filepath.Join(tempDir(t), "media")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "music.mp4"), "\x00\x00\x00\x0cftypmp42")
	write(t, filepath.Join(root, "notes.txt"), "ignored")
	write(t, filepath.Join(root, "index.ts"), "export {};\n")
	ts := make([]byte, 376)
	ts[0], ts[188] = 0x47, 0x47
	write(t, filepath.Join(root, "broadcast.ts"), string(ts))
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.Local)
	if err := os.Chtimes(filepath.Join(root, "broadcast.ts"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "music.mp4"), filepath.Join(root, "link.mp4")); err != nil {
		t.Fatal(err)
	}
	s := testSettings(t)
	s.MediaRoots = []string{root}
	result, text := findOK(t, newCall(t, s, "test"), FindArgs{URL: root, Limit: 10})
	equal(t, rustle.Compact(result.Meta), rustle.Compact(rustle.NewObj().Set("scope", "media files, newest first").Set("source", root).Set("count", 2)))
	lines := strings.Split(text, "\n")
	equal(t, lines[0], "size | modified | source")
	if !regexp.MustCompile(`^12 B \| \d{4}-\d\d-\d\d \d\d:\d\d \| ` + regexp.QuoteMeta(filepath.Join(root, "music.mp4")) + `$`).MatchString(lines[1]) {
		t.Errorf("line %q", lines[1])
	}
}

func TestListingLines(t *testing.T) {
	// Real rows from a YouTube search, as the search parser reports them.
	search := []rustle.VideoRow{
		{Title: "Sourdough Is Easier Than I Expected", Channel: "BreadLab", Duration: "12:49", Views: "155K views", Published: "10d ago", URL: "https://www.youtube.com/watch?v=Ab3dEf6hIj0"},
		{Title: "What's new in home baking | Early look", Channel: "2MinuteBakes", Duration: "2:38", Views: "18,204 views", Published: "5mo ago", URL: "https://www.youtube.com/watch?v=Kl9mNo2pQr4"},
		{Title: "Stop Letting Video Game Addiction Hijack Your Life", Channel: "HealthyGamerGG", Views: "166K views", Published: "1y ago", URL: "https://www.youtube.com/watch?v=BT4gsZBpL0E"},
	}

	t.Run("search rows: one header, short links, compact views, a dash for a missing field", func(t *testing.T) {
		meta := rustle.NewObj()
		equal(t, RenderRows(search, meta),
			"title | channel | duration | views | published | source\n"+
				"Sourdough Is Easier Than I Expected | BreadLab | 12:49 | 155K | 10d ago | Ab3dEf6hIj0\n"+
				// A "|" inside a title would read as a column separator.
				"What's new in home baking / Early look | 2MinuteBakes | 2:38 | 18K | 5mo ago | Kl9mNo2pQr4\n"+
				"Stop Letting Video Game Addiction Hijack Your Life | HealthyGamerGG | - | 166K | 1y ago | BT4gsZBpL0E")
		equal(t, rustle.Compact(meta), "{}")
	})

	t.Run("a channel's own name moves to the metadata", func(t *testing.T) {
		meta := rustle.NewObj()
		rows := []rustle.VideoRow{
			{Title: "The Folding Method", Channel: "BreadLab", Duration: "15:48", Views: "42K", Published: "3d ago", URL: "https://www.youtube.com/watch?v=St5uVw8xYz1"},
			{Title: "Proofing", Channel: "BreadLab", Duration: "20:01", Views: "1.2M views", Published: "2y ago", URL: "https://www.youtube.com/watch?v=jNQXAC9IVRw"},
		}
		equal(t, RenderRows(rows, meta),
			"title | duration | views | published | source\n"+
				"The Folding Method | 15:48 | 42K | 3d ago | St5uVw8xYz1\n"+
				"Proofing | 20:01 | 1.2M | 2y ago | jNQXAC9IVRw")
		equal(t, rustle.Compact(meta), `{"channel":"BreadLab"}`)
	})

	t.Run("view counts", func(t *testing.T) {
		for text, want := range map[string]string{
			"155,577 views": "155K", "5,712 views": "5.7K", "1,250,000 views": "1.2M", "531 views": "531",
			"1 view": "1", "No views": "0", "2.1M views": "2.1M", "1.2K watching": "1.2K watching",
		} {
			if got := CompactViews(text); got != want {
				t.Errorf("CompactViews(%q) = %q, want %q", text, got, want)
			}
		}
	})

	t.Run("a page listing names its sources; a bare domain gets https", func(t *testing.T) {
		served := serve(t, map[string]string{"*": `<video src="/clip.mp4"></video>`})
		result, text := findOK(t, newCall(t, testSettings(t), "test"), FindArgs{URL: "https://example.com", Limit: 10})
		equal(t, rustle.Compact(result.Meta), `{"scope":"video sources in the page HTML; scripts not run","source":"https://example.com","count":1}`)
		equal(t, text, "title | source\nVideo | https://example.com/clip.mp4")
		_, bare := findOK(t, newCall(t, testSettings(t), "test"), FindArgs{URL: "example.com/none/", Limit: 10})
		equal(t, served.last(), "https://example.com/none/")
		equal(t, bare, "title | source\nVideo | https://example.com/clip.mp4")
	})

	t.Run("a truncated page listing says how many there were", func(t *testing.T) {
		var page strings.Builder
		for i := range 4 {
			fmt.Fprintf(&page, `<video src="/c%d.mp4" title="Clip %d"></video>`, i, i)
		}
		serve(t, map[string]string{"*": page.String()})
		c := newCall(t, testSettings(t), "test")
		result, _ := findOK(t, c, FindArgs{URL: "https://example.com/", Limit: 2})
		equal(t, metaField(result.Meta, "count"), any(2))
		equal(t, c.Notes.List(), []string{"2 of 4 shown"})
	})

	t.Run("a YouTube channel or search page given as url lists like channel or query", func(t *testing.T) {
		args := func(url string) FindArgs { return Route(FindArgs{URL: url, Limit: 10}) }
		equal(t, args("https://www.youtube.com/@mkbhd/videos"), FindArgs{Channel: "https://www.youtube.com/@mkbhd/videos", Limit: 10})
		equal(t, args("youtube.com/results?search_query=rust+explained"), FindArgs{Query: "rust explained", Limit: 10})
		equal(t, args(" https://example.com/talks "), FindArgs{URL: "https://example.com/talks", Limit: 10})
		equal(t, args("~/Movies").URL, "~/Movies")
	})

	t.Run("other url schemes are refused", func(t *testing.T) {
		_, err := find(newCall(t, testSettings(t), "test"), FindArgs{URL: "ftp://example.com/a", Limit: 5})
		if e := failure(t, err); !strings.Contains(e.Message, "Unsupported url scheme 'ftp'") {
			t.Errorf("message %q", e.Message)
		}
	})
}

func TestASearchOfLongVideosTellsTheAgentToDelegate(t *testing.T) {
	video := func(id, length string) string {
		return `{"videoRenderer":{"videoId":"` + id + `","title":{"runs":[{"text":"Talk"}]},"lengthText":{"simpleText":"` + length + `"}}}`
	}
	results := func(videos ...string) string {
		return `{"contents":{"twoColumnSearchResultsRenderer":{"primaryContents":{"sectionListRenderer":{"contents":[{"itemSectionRenderer":{"contents":[` +
			strings.Join(videos, ",") + `]}}]}}}}}`
	}
	for _, c := range []struct {
		videos []string
		hint   any
	}{
		{[]string{video("Ab3dEf6hIj0", "34:10"), video("Kl9mNo2pQr4", "1:02:03"), video("St5uVw8xYz1", "20:00")}, choosing},
		// Two long videos are few enough to watch; short ones cost a subagent more than they save.
		{[]string{video("Ab3dEf6hIj0", "34:10"), video("Kl9mNo2pQr4", "1:02:03"), video("St5uVw8xYz1", "19:59")}, nil},
		{[]string{video("Ab3dEf6hIj0", "3:40"), video("Kl9mNo2pQr4", "1:18"), video("St5uVw8xYz1", "LIVE")}, nil},
	} {
		serve(t, map[string]string{"*": results(c.videos...)})
		result, _ := findOK(t, newCall(t, testSettings(t), "test"), FindArgs{Query: "talks", Limit: 10})
		equal(t, metaField(result.Meta, "hint"), c.hint)
	}
}
