package source

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vibeslop/rustle-mcp/internal/fetch"
	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

// reply is a canned response. Zero values mean 200, an HTML content type, an empty body,
// the requested URL and no Content-Length.
type reply struct {
	Body   string
	Type   string
	Status int
	URL    string
	Length int64
}

type call struct{ URL, Method, Range string }

// stub serves canned replies by URL ("*" for any other), recording each request. A page is
// a body string, a reply, or a func(method string) reply.
type stub struct {
	mu    sync.Mutex
	calls []call
	pages map[string]any
}

func (s *stub) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	address := req.URL.String()
	s.mu.Lock()
	s.calls = append(s.calls, call{address, req.Method, req.Header.Get("Range")})
	s.mu.Unlock()
	page, ok := s.pages[address]
	if !ok {
		page, ok = s.pages["*"]
	}
	r := reply{Status: 404}
	if ok {
		switch p := page.(type) {
		case string:
			r = reply{Body: p}
		case reply:
			r = p
		case func(string) reply:
			r = p(req.Method)
		}
	}
	if r.Status == 0 {
		r.Status = 200
	}
	if r.Type == "" {
		r.Type = "text/html; charset=utf-8"
	}
	if req.Method == http.MethodHead {
		r.Body = ""
	}
	resp := &http.Response{
		StatusCode: r.Status, Status: strconv.Itoa(r.Status) + " " + http.StatusText(r.Status),
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{"Content-Type": {r.Type}}, Body: io.NopCloser(strings.NewReader(r.Body)),
		ContentLength: -1, Request: req,
	}
	if r.Length > 0 {
		resp.ContentLength = r.Length
		resp.Header.Set("Content-Length", strconv.FormatInt(r.Length, 10))
	}
	if r.URL != "" {
		// What a client reports after following redirects to r.URL.
		final := req.Clone(req.Context())
		final.URL, _ = url.Parse(r.URL)
		resp.Request = final
	}
	return resp, nil
}

func (s *stub) Calls() []call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]call(nil), s.calls...)
}

func (s *stub) URLs() []string {
	var out []string
	for _, c := range s.Calls() {
		out = append(out, c.URL)
	}
	return out
}

func serve(t *testing.T, pages map[string]any) *stub {
	t.Helper()
	s := &stub{pages: pages}
	fetch.SetTransport(s)
	t.Cleanup(func() { fetch.SetTransport(nil) })
	return s
}

func pageVideos(t *testing.T, address string) []Candidate {
	t.Helper()
	found, err := PageVideos(context.Background(), address, true)
	if err != nil {
		t.Fatalf("PageVideos(%q): %v", address, err)
	}
	return found
}

func same[T any](t *testing.T, got, want T) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %#v\nwant %#v", got, want)
	}
}

func TestPageDiscovery(t *testing.T) {
	t.Run("video, source, OpenGraph, JSON-LD and YouTube embeds are found once each", func(t *testing.T) {
		s := serve(t, map[string]any{
			"*": `<title>Music &amp; motion</title><audio><source src="audio.mp3"></audio>
        <video><source src="/song.mp4"><source src="javascript:alert(1)"></video>
        <meta property="og:video" content="/song.mp4">
        <script type="application/ld+json">{"@graph":[{"@type":"VideoObject","contentUrl":"other.webm","name":"Live"}]}</script>
        <iframe src="https://www.youtube.com/embed/jNQXAC9IVRw"></iframe>`,
		})
		same(t, pageVideos(t, "https://example.com/music/page"), []Candidate{
			{"https://example.com/song.mp4", "Music & motion", "direct"},
			{"https://example.com/music/other.webm", "Live", "direct"},
			{"https://www.youtube.com/watch?v=jNQXAC9IVRw", "Music & motion", "youtube"},
		})
		same(t, len(s.Calls()), 1)
	})

	t.Run("iframes are followed one level deep, at most two of them", func(t *testing.T) {
		s := serve(t, map[string]any{
			"https://example.com/":    `<iframe src="/one"></iframe><iframe src="/two"></iframe><iframe src="/three"></iframe>`,
			"https://example.com/one": `<iframe src="/deep"></iframe>`,
			"https://example.com/two": "",
		})
		same(t, pageVideos(t, "https://example.com/"), []Candidate{})
		same(t, s.URLs(), []string{"https://example.com/", "https://example.com/one", "https://example.com/two"})
	})

	t.Run("an embed player page is followed when the page names nothing playable", func(t *testing.T) {
		serve(t, map[string]any{
			"https://news.example/story":  `<title>Story</title><meta property="og:video:url" content="https://player.example/e/42">`,
			"https://player.example/e/42": `<video src="https://cdn.example/42/master.m3u8" title="Clip 42"></video>`,
		})
		same(t, pageVideos(t, "https://news.example/story"), []Candidate{{"https://cdn.example/42/master.m3u8", "Clip 42", "hls"}})
	})

	t.Run("malformed JSON-LD is skipped and URLs resolve against the final URL after redirects", func(t *testing.T) {
		serve(t, map[string]any{"*": reply{
			URL:  "https://example.com/final/page",
			Body: `<script type="application/ld+json">{"@type":null}</script><video title src="clip.mp4"></video>`,
		}})
		found := pageVideos(t, "https://example.com/old")
		same(t, found[0].URL, "https://example.com/final/clip.mp4")
		same(t, found[0].Title, "Video")
	})

	t.Run("a media response is the video itself", func(t *testing.T) {
		serve(t, map[string]any{"*": reply{Type: "video/mp4", Body: "x"}})
		same(t, pageVideos(t, "https://example.com/play?id=1"), []Candidate{{"https://example.com/play?id=1", "play", "direct"}})
	})

	t.Run("a large extensionless media file is classified with HEAD instead of downloaded", func(t *testing.T) {
		s := serve(t, map[string]any{"*": func(method string) reply {
			if method == http.MethodHead {
				return reply{Type: "video/mp4", Length: 50e6}
			}
			return reply{Type: "application/octet-stream", Length: 50e6}
		}})
		found := pageVideos(t, "https://example.com/stream")
		same(t, len(found), 1)
		same(t, found[0].Kind, "direct")
		var methods []string
		for _, c := range s.Calls() {
			methods = append(methods, c.Method)
		}
		same(t, methods, []string{"GET", "HEAD"})
	})

	t.Run("parsing ignores markup inside scripts and comments, honours <base> and decodes entities", func(t *testing.T) {
		page := ParsePage(`<head><base href="https://cdn.example/media/"><title>A &mdash; B&#39;s</title></head>
       <script>document.write('<video src="fake.mp4"></video>')</script>
       <!-- <video src="commented.mp4"></video> -->
       <video src="clip.mp4?a=1&amp;b=2" title="It's > 1"></video>
       <iframe src="https://www.youtube.com/embed/videoseries?list=PL1"></iframe>
       <svg><title>icon</title></svg>`, "https://example.com/post")
		same(t, page.Title, "A — B's")
		same(t, page.Videos, []PageVideo{{"https://cdn.example/media/clip.mp4?a=1&b=2", "It's > 1", "direct"}})
		same(t, page.Iframes, []string{"https://www.youtube.com/embed/videoseries?list=PL1"})
	})

	t.Run("entities decode as a browser would in a URL, and the header charset is honoured", func(t *testing.T) {
		same(t, DecodeEntities("?a=1&copy=2&amp;b=3&lt;&ndash"), "?a=1&copy=2&b=3<&ndash")
		same(t, DecodeEntities("A&nbsp;B &#233; &#x2014;"), "A\u00a0B é —")
		serve(t, map[string]any{"*": reply{
			Type: "text/html; charset=iso-8859-1",
			Body: "<title>Caf\xe9</title><video src=\"/v.mp4\"></video>",
		}})
		same(t, pageVideos(t, "https://example.com/")[0].Title, "Café")
	})

	t.Run("a YouTube playlist embed is not mistaken for a video", func(t *testing.T) {
		s := serve(t, map[string]any{"*": `<iframe src="https://www.youtube.com/embed/videoseries?list=PL1"></iframe>`})
		same(t, pageVideos(t, "https://example.com/"), []Candidate{})
		same(t, len(s.Calls()), 1)
	})

	t.Run("JSON-LD embedUrl to YouTube becomes a watch link", func(t *testing.T) {
		serve(t, map[string]any{
			"*": `<script type="application/ld+json">[{"@type":["VideoObject"],"name":"Talk","embedUrl":"https://www.youtube-nocookie.com/embed/jNQXAC9IVRw"}]</script>`,
		})
		same(t, pageVideos(t, "https://example.com/"), []Candidate{{"https://www.youtube.com/watch?v=jNQXAC9IVRw", "Talk", "youtube"}})
	})

	t.Run("page errors keep their HTTP meaning", func(t *testing.T) {
		serve(t, map[string]any{"*": reply{Status: 404}})
		_, err := PageVideos(context.Background(), "https://example.com/gone", true)
		if !rustle.HasCode(err, "SOURCE_NOT_FOUND") {
			t.Fatalf("got %v, want SOURCE_NOT_FOUND", err)
		}
	})

	t.Run("a cancelled call stops instead of reading the page", func(t *testing.T) {
		serve(t, map[string]any{"*": ""})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := PageVideos(ctx, "https://example.com/", true); !rustle.HasCode(err, "CANCELLED") {
			t.Fatalf("got %v, want CANCELLED", err)
		}
	})
}

func classify(t *testing.T, address string) string {
	t.Helper()
	kind, err := ClassifyURL(context.Background(), address)
	if err != nil {
		t.Fatalf("ClassifyURL(%q): %v", address, err)
	}
	return kind
}

func TestClassifyURL(t *testing.T) {
	t.Run("known extensions need no request", func(t *testing.T) {
		s := serve(t, map[string]any{})
		same(t, classify(t, "https://example.com/a/video.mp4?sig=1"), "media")
		same(t, classify(t, "https://example.com/song.MP3"), "media")
		same(t, classify(t, "https://example.com/live/index.m3u8"), "hls")
		same(t, len(s.Calls()), 0)
	})

	t.Run("extensionless URLs are classified by HEAD only", func(t *testing.T) {
		s := serve(t, map[string]any{"*": reply{Type: "video/mp4"}})
		same(t, classify(t, "https://example.com/play?id=1"), "media")
		same(t, s.Calls(), []call{{"https://example.com/play?id=1", "HEAD", ""}})
		for address, ctype := range map[string]string{
			"https://example.com/live":  "application/vnd.apple.mpegurl",
			"https://example.com/radio": "audio/x-mpegurl",
		} {
			serve(t, map[string]any{"*": reply{Type: ctype}})
			same(t, classify(t, address), "hls")
		}
		serve(t, map[string]any{"*": reply{Type: "application/octet-stream"}})
		same(t, classify(t, "https://example.com/blob"), "media")
		serve(t, map[string]any{"*": reply{}})
		same(t, classify(t, "https://example.com/article"), "page")
	})

	t.Run("a refused HEAD falls back to a one-byte ranged GET", func(t *testing.T) {
		s := serve(t, map[string]any{"*": func(method string) reply {
			if method == http.MethodHead {
				return reply{Status: 405}
			}
			return reply{Status: 206, Type: "audio/mpeg"}
		}})
		same(t, classify(t, "https://example.com/episode"), "media")
		var got [][2]string
		for _, c := range s.Calls() {
			got = append(got, [2]string{c.Method, c.Range})
		}
		same(t, got, [][2]string{{"HEAD", ""}, {"GET", "bytes=0-0"}})
	})

	t.Run("no answer at all reads as a page", func(t *testing.T) {
		serve(t, map[string]any{"*": reply{Status: 500}})
		same(t, classify(t, "https://example.com/down"), "page")
	})
}

// decodeBody against Node's TextDecoder on the same bytes.
func TestDecodeBody(t *testing.T) {
	for _, c := range []struct {
		label string
		body  []byte
		want  string
	}{
		{"iso-8859-1", []byte{0x80, 0x81, 0xe9, 0x9f}, "€\u0081éŸ"},
		{"latin1", []byte{0x93}, "“"},
		{"us-ascii", []byte{0xe9}, "é"},
		{"utf-16", []byte{0xff, 0xfe, 0x41, 0, 0x42}, "A\ufffd"},
		{"utf-16be", []byte{0xfe, 0xff, 0, 0x41}, "A"},
		{"utf-16le", []byte{0x00, 0xd8, 0x41, 0}, "\ufffdA"},
		{"utf-8", []byte{0xef, 0xbb, 0xbf, 0x41, 0xe2, 0x82, 0x41, 0xc0, 0xed, 0xa0, 0x80, 0xf0, 0x9f}, "A\ufffdA\ufffd\ufffd\ufffd\ufffd\ufffd"},
		{"bogus", []byte("Café"), "Café"},
	} {
		if got := decodeBody(c.body, "text/html; charset="+c.label); got != c.want {
			t.Errorf("%s % x: got %q, want %q", c.label, c.body, got, c.want)
		}
	}
	if got := decodeBody([]byte("Café"), "text/html"); got != "Café" {
		t.Errorf("no charset: got %q", got)
	}
}

func TestFileHelpers(t *testing.T) {
	for _, c := range []struct {
		bytes int64
		want  string
	}{
		{0, "0 B"}, {12, "12 B"}, {999, "999 B"}, {1000, "1.0 KB"}, {1150, "1.1 KB"}, {1250, "1.3 KB"}, {1350, "1.4 KB"},
		{999949, "999.9 KB"}, {999950, "1000.0 KB"}, {1e6, "1.0 MB"}, {3.4e6, "3.4 MB"}, {1e12, "1000.0 GB"}, {1234567890123, "1234.6 GB"},
	} {
		same(t, FileSize(c.bytes), c.want)
	}
	for _, c := range [][2]string{
		{"/a/b.mp4", ".mp4"}, {"/a/.mp4", ""}, {"/a/b.", "."}, {"/a/b.MP4/", ".MP4"}, {"..", ""}, {".", ""}, {"a.b.c", ".c"},
		{"/a.b/c", ""}, {"", ""}, {"/", ""}, {"a..", "."}, {".a.b", ".b"}, {"/x/.hidden.ts", ".ts"},
	} {
		same(t, extname(c[0]), c[1])
	}
	for _, c := range [][2]string{
		{"https://e.com/a/b%20c.mp4", "b c.mp4"}, {"https://e.com/", "Remote video"}, {"https://e.com/100%25%zz.mp4", "100%%zz.mp4"},
		{"https://e.com/%E9.mp4", "%E9.mp4"}, {"nope", "Remote video"}, {"https://e.com/play?id=1", "play"}, {"https://e.com/a%2Fb", "a/b"},
	} {
		same(t, FileTitle(c[0]), c[1])
	}
}

// tempDir is a fresh folder with symlinks resolved, as macOS puts /var under /private.
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func write(t *testing.T, path string, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func settings(t *testing.T, roots ...string) *rustle.Settings {
	t.Helper()
	s, err := rustle.LoadSettings(rustle.MapEnv(map[string]string{}))
	if err != nil {
		t.Fatal(err)
	}
	s.MediaRoots = roots
	return s
}

func fileURL(path string) string { return (&url.URL{Scheme: "file", Path: path}).String() }

func TestLocalFolders(t *testing.T) {
	folder := func(t *testing.T) (base, root string, s *rustle.Settings) {
		base = tempDir(t)
		root = filepath.Join(base, "media")
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
		return base, root, settings(t, root)
	}

	t.Run("lists media newest first, skipping symlinks, text and TypeScript files", func(t *testing.T) {
		_, root, s := folder(t)
		source, rows, err := LocalVideos(fileURL(root), s, 10)
		if err != nil {
			t.Fatal(err)
		}
		same(t, source, root)
		var got [][3]string
		for _, r := range rows {
			got = append(got, [3]string{r.Title, r.Size, r.URL})
		}
		same(t, got, [][3]string{
			{"music.mp4", "12 B", filepath.Join(root, "music.mp4")},
			{"broadcast.ts", "376 B", filepath.Join(root, "broadcast.ts")},
		})
		if !regexp.MustCompile(`^\d{4}-\d\d-\d\d \d\d:\d\d$`).MatchString(rows[0].Modified) {
			t.Errorf("modified %q", rows[0].Modified)
		}
		same(t, rows[1].Modified, "2020-01-01 00:00")
		_, rows, _ = LocalVideos(root, s, 1)
		same(t, len(rows), 1)
	})

	t.Run("files modified together are listed by name", func(t *testing.T) {
		root := tempDir(t)
		at := time.Date(2024, 5, 6, 7, 8, 0, 0, time.Local)
		for _, name := range []string{"b.mp3", "a.wav", "C.flac"} {
			write(t, filepath.Join(root, name), "ID3")
			os.Chtimes(filepath.Join(root, name), at, at)
		}
		_, rows, err := LocalVideos(root, settings(t, root), 10)
		if err != nil {
			t.Fatal(err)
		}
		same(t, []string{rows[0].Title, rows[1].Title, rows[2].Title}, []string{"C.flac", "a.wav", "b.mp3"})
	})

	t.Run("folders outside the media roots, missing folders and files are refused", func(t *testing.T) {
		base, root, s := folder(t)
		_, _, err := LocalVideos(base, s, 10)
		if err == nil || !strings.Contains(err.Error(), "outside the folders") {
			t.Errorf("outside: %v", err)
		}
		_, _, err = LocalVideos(filepath.Join(root, "nope"), s, 10)
		if !rustle.HasCode(err, "SOURCE_NOT_FOUND") {
			t.Errorf("missing: %v", err)
		}
		_, _, err = LocalVideos(filepath.Join(root, "music.mp4"), s, 10)
		if err == nil || !strings.Contains(err.Error(), "is a file, not a folder") {
			t.Errorf("file: %v", err)
		}
		_, _, err = LocalVideos(root, settings(t), 10)
		if !rustle.HasCode(err, "LOCAL_FILES_DISABLED") {
			t.Errorf("disabled: %v", err)
		}
		_, _, err = LocalVideos("file://host"+root, s, 10)
		if err == nil || err.Error() != "Use a local folder path or a file URL without a remote host." {
			t.Errorf("remote host: %v", err)
		}
	})
}
