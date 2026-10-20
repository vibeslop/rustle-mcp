package source

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

const mp4Head = "\x00\x00\x00\x0cftypmp42"

func TestSniff(t *testing.T) {
	padded := func(head string, size int) []byte {
		b := make([]byte, size)
		copy(b, head)
		return b
	}
	ts := make([]byte, 189)
	ts[0], ts[188] = 0x47, 0x47
	for _, c := range []struct {
		name string
		data []byte
		want string
	}{
		{"mp4", []byte(mp4Head), "mov"},
		{"mov atom", padded("\x00\x00\x00\x08moov", 12), "mov"},
		{"ftyp in under 12 bytes", []byte("\x00\x00\x00\x08ftyp"), ""},
		{"matroska", []byte{0x1a, 0x45, 0xdf, 0xa3}, "matroska"},
		{"avi", []byte("RIFF\x00\x00\x00\x00AVI LIST"), "avi"},
		{"wav", []byte("RIFF\x00\x00\x00\x00WAVEfmt "), "wav"},
		{"other RIFF", []byte("RIFF\x00\x00\x00\x00WEBP"), ""},
		{"aiff", []byte("FORM\x00\x00\x00\x00AIFF"), "aiff"},
		{"aifc", []byte("FORM\x00\x00\x00\x00AIFC"), "aiff"},
		{"ogg", []byte("OggS\x00"), "ogg"},
		{"flac", []byte("fLaC"), "flac"},
		{"adts aac", []byte{0xff, 0xf1, 0x50}, "aac"},
		{"mp3 frame", []byte{0xff, 0xfb, 0x90}, "mp3"},
		{"id3", []byte("ID3\x04"), "mp3"},
		{"mpeg program stream", []byte{0, 0, 1, 0xba}, "mpeg"},
		{"mpeg-ts", ts, "mpegts"},
		{"one ts packet", ts[:188], ""},
		{"text", []byte("export {};\n"), ""},
		{"empty", nil, ""},
		{"lone sync byte", []byte{0xff}, ""},
	} {
		if got := Sniff(c.data); got != c.want {
			t.Errorf("%s: Sniff = %q, want %q", c.name, got, c.want)
		}
	}
}

func wantError(t *testing.T, err error, code, message, recovery string) {
	t.Helper()
	e, ok := rustle.AsError(err)
	if !ok {
		t.Fatalf("got %v, want %s error %q", err, code, message)
	}
	if e.Code != code || e.Message != message || recovery != "" && e.Recovery != recovery {
		t.Errorf("got %s %q / %q\nwant %s %q / %q", e.Code, e.Message, e.Recovery, code, message, recovery)
	}
}

const (
	notAVideo    = "Use a YouTube video link, not a playlist, search or home page."
	notAVideoFix = "Pass a watch, shorts, embed, live or youtu.be link; find_videos(query=...) finds one."
	channelLink  = "This is a YouTube channel link, not a video."
	defaultFix   = "Check the source and arguments, then retry."
)

func TestYouTubeID(t *testing.T) {
	for _, c := range [][2]string{
		{"https://www.youtube.com/watch?v=jNQXAC9IVRw", "jNQXAC9IVRw"},
		{"https://WWW.YOUTUBE.COM/watch?feature=share&v=jNQXAC9IVRw", "jNQXAC9IVRw"},
		{"https://m.youtube.com/watch?v=jNQXAC9IVRw&t=42", "jNQXAC9IVRw"},
		{"https://music.youtube.com/watch?v=jNQXAC9IVRw", "jNQXAC9IVRw"},
		{"https://youtu.be/jNQXAC9IVRw?t=3", "jNQXAC9IVRw"},
		{"https://youtu.be/jNQXAC9IVRw/", "jNQXAC9IVRw"},
		{"https://www.youtube.com/shorts/jNQXAC9IVRw", "jNQXAC9IVRw"},
		{"https://www.youtube.com/embed/jNQXAC9IVRw", "jNQXAC9IVRw"},
		{"https://www.youtube.com/live/jNQXAC9IVRw", "jNQXAC9IVRw"},
		{"https://www.youtube.com/v/jNQXAC9IVRw", "jNQXAC9IVRw"},
		{"https://www.youtube-nocookie.com/embed/jNQXAC9IVRw", "jNQXAC9IVRw"},
		{"https://example.com/watch?v=jNQXAC9IVRw", ""},
		{"/Users/me/clip.mp4", ""},
		{"not a url", ""},
	} {
		id, err := YouTubeID(c[0])
		if err != nil || id != c[1] {
			t.Errorf("YouTubeID(%q) = %q, %v; want %q", c[0], id, err, c[1])
		}
	}
	for _, link := range []string{
		"https://www.youtube.com/embed/videoseries?list=PL1",
		"https://www.youtube.com/playlist?list=PL1",
		"https://www.youtube.com/watch?v=short",
		"https://www.youtube.com/",
		"https://youtu.be/",
	} {
		_, err := YouTubeID(link)
		wantError(t, err, "INVALID_SOURCE", notAVideo, notAVideoFix)
	}
	for _, link := range []string{
		"https://www.youtube.com/@mkbhd", "https://www.youtube.com/channel/UC1", "https://youtube.com/c/name", "https://youtube.com/user/name/videos",
	} {
		_, err := YouTubeID(link)
		wantError(t, err, "INVALID_SOURCE", channelLink, `Call find_videos(channel="`+link+`") to list its videos, then pass one video's link.`)
	}
	long := "https://www.youtube.com/@mkbhd?" + strings.Repeat("x", 300)
	_, err := YouTubeID(long)
	wantError(t, err, "INVALID_SOURCE", channelLink, `Call find_videos(channel="<this link>") to list its videos, then pass one video's link.`)
}

func TestDisplayPath(t *testing.T) {
	home := rustle.Home()
	for _, c := range [][2]string{
		{home, "~"},
		{filepath.Join(home, "Movies", "a.mp4"), "~" + string(filepath.Separator) + filepath.Join("Movies", "a.mp4")},
		{home + "x/a.mp4", home + "x/a.mp4"},
		{"/elsewhere/a.mp4", "/elsewhere/a.mp4"},
	} {
		if got := DisplayPath(c[0]); got != c[1] {
			t.Errorf("DisplayPath(%q) = %q, want %q", c[0], got, c[1])
		}
	}
}

func TestInsideRoots(t *testing.T) {
	roots := []string{"/media/videos", "/tmp"}
	for path, want := range map[string]bool{
		"/media/videos":         true,
		"/media/videos/":        true,
		"/media/videos/a.mp4":   true,
		"/media/videos/x/a.mp4": true,
		"/media/videos/.a.mp4":  true,
		"/tmp/a.mp4":            true,
		"/media/videosx/a.mp4":  false,
		"/media":                false,
		"/media/videos/../a":    false,
		// As in the TypeScript version, a child whose name starts with ".." counts as outside.
		"/media/videos/..a.mp4": false,
	} {
		if got := InsideRoots(path, roots); got != want {
			t.Errorf("InsideRoots(%q) = %v, want %v", path, got, want)
		}
	}
	if InsideRoots("/tmp/a.mp4", nil) {
		t.Error("no roots allow nothing")
	}
}

func TestRootsError(t *testing.T) {
	dir := tempDir(t)
	missing := filepath.Join(dir, "missing")
	path := "/elsewhere/a.mp4"
	fix := "Copy the file into one of those folders, or add its folder to RUSTLE_MEDIA_ROOTS (separated by '" + rustle.PathListSeparator + "') in the MCP server config and restart the server."
	wantError(t, RootsError(path, settings(t, dir, missing)), "OUTSIDE_MEDIA_ROOTS",
		"/elsewhere/a.mp4 is outside the folders Rustle may read: "+DisplayPath(dir)+".", fix)
	wantError(t, RootsError(path, settings(t, missing)), "OUTSIDE_MEDIA_ROOTS",
		"/elsewhere/a.mp4 is outside the folders Rustle may read: none exist.", fix)
	wantError(t, RootsError(path, settings(t)), "LOCAL_FILES_DISABLED",
		"Local files are disabled on this server.", "Pass a YouTube link or a web URL instead of a local path.")
}

func TestAssertMediaFile(t *testing.T) {
	dir := tempDir(t)
	for name, c := range map[string]struct{ data, want string }{
		"clip.mp4": {mp4Head, "mov"},
		// A file shorter than a header is sniffed with the zeros after it, as it was in TypeScript.
		"tiny.mov":  {"\x00\x00\x00\x08ftyp", "mov"},
		"song.mp3":  {"ID3\x04\x00", "mp3"},
		"sync.ts":   {"\x47", ""},
		"notes.txt": {"hello", ""},
	} {
		path := filepath.Join(dir, name)
		write(t, path, c.data)
		format, err := AssertMediaFile(path)
		if c.want != "" {
			if err != nil || format != c.want {
				t.Errorf("%s: got %q, %v; want %q", name, format, err, c.want)
			}
			continue
		}
		wantError(t, err, "UNSUPPORTED_MEDIA", DisplayPath(path)+" is not a video or audio file.",
			"Pass a video or audio file (MP4, MOV, WebM, MKV, AVI, MPEG-TS, MP3, M4A, AAC, WAV, FLAC, Ogg). An .srt or .vtt file is read through the media file it sits next to.")
	}
	missing := filepath.Join(dir, "gone.mp4")
	_, err := AssertMediaFile(missing)
	wantError(t, err, "ACCESS_DENIED", "Could not read "+DisplayPath(missing)+".", "Check that the file is readable.")
}

func TestIdentify(t *testing.T) {
	dir := tempDir(t)
	t.Chdir(dir)
	s := settings(t, dir)
	identify := func(t *testing.T, raw string) Identity {
		t.Helper()
		id, err := Identify(raw, s)
		if err != nil {
			t.Fatalf("Identify(%q): %v", raw, err)
		}
		return id
	}

	t.Run("YouTube links, bare ids and links without a scheme", func(t *testing.T) {
		// The key is the TypeScript version's, so both share a cache.
		want := Identity{Kind: "youtube", ID: "jNQXAC9IVRw", Source: "https://www.youtube.com/watch?v=jNQXAC9IVRw", Key: "4a83d0e7ecc312a7d02d0405eefc004d"}
		for _, raw := range []string{"jNQXAC9IVRw", " youtu.be/jNQXAC9IVRw ", "https://www.youtube.com/shorts/jNQXAC9IVRw#t=1"} {
			same(t, identify(t, raw), want)
		}
	})

	t.Run("a bare id or domain path is a file when one of that name exists", func(t *testing.T) {
		write(t, filepath.Join(dir, "abcdefghijk"), mp4Head)
		id := identify(t, "abcdefghijk")
		same(t, [2]string{id.Kind, id.Source}, [2]string{"local", filepath.Join(dir, "abcdefghijk")})
		if err := os.MkdirAll(filepath.Join(dir, "example.com"), 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(dir, "example.com", "clip.mp4"), mp4Head)
		id = identify(t, "example.com/clip.mp4")
		same(t, [2]string{id.Kind, id.Source}, [2]string{"local", filepath.Join(dir, "example.com", "clip.mp4")})
		same(t, identify(t, "example.org/clip.mp4"), Identity{Kind: "url", Source: "https://example.org/clip.mp4", Key: rustle.HashKey("url:https://example.org/clip.mp4")})
	})

	t.Run("URLs are normalized and lose their fragment", func(t *testing.T) {
		same(t, identify(t, "https://example.com/clip.mp4"), Identity{Kind: "url", Source: "https://example.com/clip.mp4", Key: "ea1786399be9aeb31bbed0598e63ad4d"})
		same(t, identify(t, "HTTPS://Example.COM:443/a/../clip.mp4?x=1#t=5").Source, "https://example.com/clip.mp4?x=1")
		same(t, identify(t, "http://example.com:8080").Source, "http://example.com:8080/")
	})

	t.Run("credentials, malformed URLs and other schemes are refused", func(t *testing.T) {
		for _, raw := range []string{"https://user:pw@example.com/v.mp4", "https://user@example.com/v.mp4"} {
			_, err := Identify(raw, s)
			wantError(t, err, "INVALID_SOURCE", "Only HTTP(S) URLs without embedded credentials are accepted.", defaultFix)
		}
		_, err := Identify("https://exa mple.com/v.mp4", s)
		wantError(t, err, "INVALID_SOURCE", "That URL is malformed.", defaultFix)
		_, err = Identify("FTP://example.com/a.mp4", s)
		wantError(t, err, "INVALID_SOURCE", "Unsupported source scheme 'ftp'. Use a YouTube link, an HTTP(S) URL or a local path.", defaultFix)
	})

	t.Run("channel links and playlists are refused", func(t *testing.T) {
		_, err := Identify("https://www.youtube.com/@mkbhd", s)
		wantError(t, err, "INVALID_SOURCE", channelLink, `Call find_videos(channel="https://www.youtube.com/@mkbhd") to list its videos, then pass one video's link.`)
		_, err = Identify("youtube.com/playlist?list=PL1", s)
		wantError(t, err, "INVALID_SOURCE", notAVideo, notAVideoFix)
	})

	t.Run("local files and file URLs", func(t *testing.T) {
		path := filepath.Join(dir, "my clip.mp4")
		write(t, path, mp4Head)
		id := identify(t, path)
		same(t, [2]string{id.Kind, id.Source}, [2]string{"local", path})
		same(t, identify(t, fileURL(path)), id)
		same(t, identify(t, strings.Replace(fileURL(path), "file://", "file://localhost", 1)), id)
		same(t, identify(t, "./my clip.mp4"), id)
		// The key changes with the file, so a re-rendered file is read again.
		later := time.Now().Add(time.Hour)
		os.Chtimes(path, later, later)
		if identify(t, path).Key == id.Key {
			t.Error("the key ignores the modification time")
		}
		for _, raw := range []string{strings.Replace(fileURL(path), "file://", "file://host", 1), "file:///a%2Fb.mp4", "file:///%E9.mp4"} {
			_, err := Identify(raw, s)
			wantError(t, err, "INVALID_SOURCE", "Use a local file path or a file URL without a remote host.", defaultFix)
		}
	})

	t.Run("a one-letter scheme is a Windows drive, read as a path", func(t *testing.T) {
		_, err := Identify("C:/clip.mp4", s)
		if runtime.GOOS == "windows" {
			// There it is an absolute path, outside the roots.
			wantError(t, err, "OUTSIDE_MEDIA_ROOTS", `C:\clip.mp4 is outside the folders Rustle may read: `+DisplayPath(dir)+".", "")
			return
		}
		wantError(t, err, "SOURCE_NOT_FOUND", "Local file not found: "+DisplayPath(filepath.Join(dir, "C:/clip.mp4"))+".",
			"Check the path; pass an absolute path or one starting with ~.")
	})

	t.Run("paths outside the roots are refused before anything about them is revealed", func(t *testing.T) {
		other := tempDir(t)
		for _, path := range []string{filepath.Join(other, "clip.mp4"), filepath.Join(other, "missing.mp4")} {
			_, err := Identify(path, s)
			wantError(t, err, "OUTSIDE_MEDIA_ROOTS", DisplayPath(path)+" is outside the folders Rustle may read: "+DisplayPath(dir)+".", "")
		}
		// A symlink inside the roots is followed to where it points.
		write(t, filepath.Join(other, "clip.mp4"), mp4Head)
		if err := os.Symlink(filepath.Join(other, "clip.mp4"), filepath.Join(dir, "link.mp4")); err != nil {
			t.Fatal(err)
		}
		_, err := Identify(filepath.Join(dir, "link.mp4"), s)
		wantError(t, err, "OUTSIDE_MEDIA_ROOTS", DisplayPath(filepath.Join(other, "clip.mp4"))+" is outside the folders Rustle may read: "+DisplayPath(dir)+".", "")
		_, err = Identify(filepath.Join(dir, "clip.mp4"), settings(t))
		wantError(t, err, "LOCAL_FILES_DISABLED", "Local files are disabled on this server.", "")
	})

	t.Run("folders, missing files and empty sources are refused", func(t *testing.T) {
		_, err := Identify(dir, s)
		wantError(t, err, "INVALID_SOURCE", DisplayPath(dir)+" is a folder, not a media file.",
			`Call find_videos(url="`+DisplayPath(dir)+`") to list the videos in it, then pass one of them.`)
		_, err = Identify(filepath.Join(dir, "missing.mp4"), s)
		wantError(t, err, "SOURCE_NOT_FOUND", "Local file not found: "+DisplayPath(filepath.Join(dir, "missing.mp4"))+".", "")
		for _, raw := range []string{"", " \u00a0\n"} {
			_, err = Identify(raw, s)
			wantError(t, err, "INVALID_SOURCE", "Pass a source: a YouTube link, a web page, a media URL or a local file path.", defaultFix)
		}
		_, err = Identify("https://example.com/"+strings.Repeat("a", 16384), s)
		wantError(t, err, "INVALID_SOURCE", "Source is too long.", defaultFix)
	})
}
