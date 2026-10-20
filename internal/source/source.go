// Package source turns what an agent passes as a source into something to read: a
// YouTube id, a normalized URL or a local path inside the allowed folders. It also finds
// the videos a web page names and lists the media in a local folder.
package source

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

var YouTubeHosts = setOf("youtube.com", "www.youtube.com", "m.youtube.com", "music.youtube.com", "youtu.be", "www.youtube-nocookie.com")

var AudioSuffixes = setOf(".wav", ".mp3", ".m4a", ".aac", ".flac", ".ogg", ".oga", ".opus", ".aif", ".aiff")

var VideoSuffixes = setOf(".mp4", ".webm", ".mov", ".mkv", ".m4v", ".avi", ".ogv", ".mpeg", ".mpg", ".ts", ".m3u8")

func setOf(items ...string) map[string]bool {
	set := make(map[string]bool, len(items))
	for _, item := range items {
		set[item] = true
	}
	return set
}

// Identity is a source normalized without network access.
type Identity struct {
	// Kind is "youtube", "url" or "local".
	Kind string
	// ID is the YouTube video id, for Kind "youtube".
	ID string
	// Source is the watch URL, the normalized URL or the absolute local path.
	Source string
	Key    string
}

// Sniff is the container named by a file's first bytes, so no extension or header is
// trusted; "" when none is recognized.
func Sniff(data []byte) string {
	ascii := func(from, to int) string {
		if from >= len(data) {
			return ""
		}
		return string(data[from:min(to, len(data))])
	}
	// at is -1 past the end, so it equals no byte; bits is 0 there, as undefined & mask is.
	at := func(i int) int {
		if i < len(data) {
			return int(data[i])
		}
		return -1
	}
	bits := func(i int) int { return max(at(i), 0) }
	if len(data) >= 12 {
		switch ascii(4, 8) {
		case "ftyp", "moov", "mdat", "free", "wide", "skip":
			return "mov"
		}
	}
	if at(0) == 0x1a && at(1) == 0x45 && at(2) == 0xdf && at(3) == 0xa3 {
		return "matroska"
	}
	if ascii(0, 4) == "RIFF" {
		switch ascii(8, 12) {
		case "AVI ":
			return "avi"
		case "WAVE":
			return "wav"
		}
	}
	if ascii(0, 4) == "FORM" && (ascii(8, 12) == "AIFF" || ascii(8, 12) == "AIFC") {
		return "aiff"
	}
	if ascii(0, 4) == "OggS" {
		return "ogg"
	}
	if ascii(0, 4) == "fLaC" {
		return "flac"
	}
	// ADTS AAC shares the 0xFFF sync word with MP3 but has layer bits 00.
	if at(0) == 0xff && bits(1)&0xf6 == 0xf0 {
		return "aac"
	}
	if ascii(0, 3) == "ID3" || at(0) == 0xff && bits(1)&0xe0 == 0xe0 {
		return "mp3"
	}
	if at(0) == 0 && at(1) == 0 && at(2) == 1 && at(3) == 0xba {
		return "mpeg"
	}
	if at(0) == 0x47 && len(data) > 188 && data[188] == 0x47 {
		return "mpegts"
	}
	return ""
}

var (
	videoID     = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
	channelPath = regexp.MustCompile(`^/(@|channel/|c/|user/)`)
)

// YouTubeID is the video id a YouTube link names; "" without an error when source isn't
// a YouTube URL, and an error for a YouTube link to anything but one video.
func YouTubeID(source string) (string, error) {
	u, ok := ParseURL(source)
	if !ok {
		return "", nil
	}
	host := strings.ToLower(u.Hostname)
	if !YouTubeHosts[host] {
		return "", nil
	}
	pathname := u.Pathname()
	bits := strings.Split(strings.Trim(pathname, "/"), "/")
	candidate := ""
	switch {
	case host == "youtu.be" && len(bits) == 1:
		candidate = bits[0]
	case pathname == "/watch":
		candidate, _ = u.Param("v")
	case len(bits) == 2 && (bits[0] == "shorts" || bits[0] == "embed" || bits[0] == "live" || bits[0] == "v"):
		candidate = bits[1]
	}
	// /embed/videoseries?list=... is a playlist whose name happens to be eleven characters.
	if candidate != "videoseries" && videoID.MatchString(candidate) {
		return candidate, nil
	}
	if channelPath.MatchString(pathname) {
		channel := source
		if rustle.UTF16Len(source) > 300 {
			channel = "<this link>"
		}
		return "", rustle.Fail("This is a YouTube channel link, not a video.", "INVALID_SOURCE",
			fmt.Sprintf(`Call find_videos(channel="%s") to list its videos, then pass one video's link.`, channel))
	}
	return "", rustle.Fail("Use a YouTube video link, not a playlist, search or home page.", "INVALID_SOURCE",
		"Pass a watch, shorts, embed, live or youtu.be link; find_videos(query=...) finds one.")
}

// DisplayPath writes the home folder as ~, which is shorter and keeps the user name out.
func DisplayPath(path string) string {
	home := rustle.Home()
	if path == home || strings.HasPrefix(path, home+string(filepath.Separator)) {
		return "~" + path[len(home):]
	}
	return path
}

// InsideRoots reports whether path is one of roots or inside one.
func InsideRoots(path string, roots []string) bool {
	path = absolute(path)
	for _, root := range roots {
		rel, err := filepath.Rel(absolute(root), path)
		if err != nil {
			continue
		}
		// A child named "..x" counts as outside: refusing an odd name is safer than
		// guessing at it.
		if rel == "." || !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel) {
			return true
		}
	}
	return false
}

func absolute(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// RootsError says why path may not be read, and how to allow it.
func RootsError(path string, s *rustle.Settings) *rustle.Error {
	if len(s.MediaRoots) == 0 {
		return rustle.Fail("Local files are disabled on this server.", "LOCAL_FILES_DISABLED",
			"Pass a YouTube link or a web URL instead of a local path.")
	}
	var shown []string
	for _, root := range s.MediaRoots {
		if exists(root) {
			shown = append(shown, DisplayPath(root))
		}
	}
	folders := strings.Join(shown, ", ")
	if folders == "" {
		folders = "none exist"
	}
	return rustle.Fail(fmt.Sprintf("%s is outside the folders Rustle may read: %s.", DisplayPath(path), folders), "OUTSIDE_MEDIA_ROOTS",
		fmt.Sprintf("Copy the file into one of those folders, or add its folder to RUSTLE_MEDIA_ROOTS (separated by '%s') in the MCP server config and restart the server.", rustle.PathListSeparator))
}

// RootedPath resolves a local path and checks it against the allowed folders before
// revealing anything about it.
func RootedPath(input string, s *rustle.Settings) (string, error) {
	path := absolute(rustle.Expand(input))
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	if !InsideRoots(path, s.MediaRoots) {
		return "", RootsError(path, s)
	}
	return path, nil
}

// LocalPath is a file inside the allowed folders; a missing file or a folder is an error.
func LocalPath(input string, s *rustle.Settings) (string, error) {
	path, err := RootedPath(input, s)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", rustle.Fail(fmt.Sprintf("Local file not found: %s.", DisplayPath(path)), "SOURCE_NOT_FOUND",
			"Check the path; pass an absolute path or one starting with ~.")
	}
	if info.IsDir() {
		return "", rustle.Fail(fmt.Sprintf("%s is a folder, not a media file.", DisplayPath(path)), "INVALID_SOURCE",
			fmt.Sprintf(`Call find_videos(url="%s") to list the videos in it, then pass one of them.`, DisplayPath(path)))
	}
	return path, nil
}

// AssertMediaFile is the container of a local file, and refuses files that aren't media,
// so notes.txt never reads out its sibling notes.srt.
func AssertMediaFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", rustle.Fail(fmt.Sprintf("Could not read %s.", DisplayPath(path)), "ACCESS_DENIED", "Check that the file is readable.")
	}
	defer f.Close()
	// The whole zeroed buffer is sniffed, so a file shorter than a header still reads as
	// its first bytes say.
	head := make([]byte, 512)
	if _, err := io.ReadFull(f, head); err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return "", err
	}
	format := Sniff(head)
	if format == "" {
		return "", rustle.Fail(fmt.Sprintf("%s is not a video or audio file.", DisplayPath(path)), "UNSUPPORTED_MEDIA",
			"Pass a video or audio file (MP4, MOV, WebM, MKV, AVI, MPEG-TS, MP3, M4A, AAC, WAV, FLAC, Ogg). An .srt or .vtt file is read through the media file it sits next to.")
	}
	return format, nil
}

var (
	schemePrefix = regexp.MustCompile(`^([a-zA-Z][a-zA-Z0-9+.-]*):`)
	bareDomain   = regexp.MustCompile(`^[\w-]+(\.[\w-]+)+/`)
)

// Identify normalizes a source without network access; local files are checked against
// the allowed folders.
func Identify(raw string, s *rustle.Settings) (Identity, error) {
	source := rustle.TrimSpace(raw)
	if source == "" {
		return Identity{}, rustle.Fail("Pass a source: a YouTube link, a web page, a media URL or a local file path.", "INVALID_SOURCE", "")
	}
	if rustle.UTF16Len(source) > 16384 {
		return Identity{}, rustle.Fail("Source is too long.", "INVALID_SOURCE", "")
	}
	// A bare video id, as find_videos lists them, unless a file of that name is meant.
	if videoID.MatchString(source) && !exists(source) {
		source = "https://www.youtube.com/watch?v=" + source
	}
	// "youtu.be/ID" or "example.com/clip.mp4" pasted without a scheme.
	if !schemePrefix.MatchString(source) && bareDomain.MatchString(source) && !exists(source) {
		source = "https://" + source
	}
	id, err := YouTubeID(source)
	if err != nil {
		return Identity{}, err
	}
	if id != "" {
		return Identity{Kind: "youtube", ID: id, Source: "https://www.youtube.com/watch?v=" + id, Key: rustle.HashKey("youtube:" + id)}, nil
	}
	scheme := ""
	if m := schemePrefix.FindStringSubmatch(source); m != nil {
		scheme = strings.ToLower(m[1])
	}
	switch {
	case scheme == "file":
		path, err := FileURLToPath(source)
		if err != nil {
			return Identity{}, rustle.Fail("Use a local file path or a file URL without a remote host.", "INVALID_SOURCE", "")
		}
		source = path
	case scheme == "http" || scheme == "https":
		u, ok := ParseURL(source)
		if !ok {
			return Identity{}, rustle.Fail("That URL is malformed.", "INVALID_SOURCE", "")
		}
		if u.Username != "" || u.Password != "" {
			return Identity{}, rustle.Fail("Only HTTP(S) URLs without embedded credentials are accepted.", "INVALID_SOURCE", "")
		}
		u.DropFragment()
		normalized := u.Href()
		return Identity{Kind: "url", Source: normalized, Key: rustle.HashKey("url:" + normalized)}, nil
	case len(scheme) > 1:
		// One letter is a Windows drive, not a scheme.
		return Identity{}, rustle.Fail(fmt.Sprintf("Unsupported source scheme '%s'. Use a YouTube link, an HTTP(S) URL or a local path.", scheme), "INVALID_SOURCE", "")
	}
	path, err := LocalPath(source, s)
	if err != nil {
		return Identity{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return Identity{}, err
	}
	// Size and modification time are part of the key, so a re-rendered file is read again.
	mtime := rustle.NumString(mtimeMs(info.ModTime()))
	return Identity{Kind: "local", Source: path, Key: rustle.HashKey(fmt.Sprintf("local:%s:%d:%s", path, info.Size(), mtime))}, nil
}

// FileURLToPath is the local path a file URL names: the host must be empty or localhost,
// and an encoded separator is refused rather than decoded into one. On Windows,
// file:///C:/Users/a.mp4 names C:\Users\a.mp4.
func FileURLToPath(raw string) (string, error) {
	u, ok := ParseURL(raw)
	if !ok {
		return "", fmt.Errorf("invalid URL: %s", raw)
	}
	if u.Scheme != "file" {
		return "", fmt.Errorf("the URL must be of scheme file")
	}
	if u.Hostname != "" && u.Hostname != "localhost" {
		return "", fmt.Errorf("file URL host must be \"localhost\" or empty")
	}
	pathname := u.Pathname()
	if lower := strings.ToLower(pathname); strings.Contains(lower, "%2f") || runtime.GOOS == "windows" && strings.Contains(lower, "%5c") {
		return "", fmt.Errorf("file URL path must not include encoded separators")
	}
	path, ok := DecodeURIComponent(pathname)
	if !ok {
		return "", fmt.Errorf("URI malformed: %s", pathname)
	}
	if runtime.GOOS == "windows" {
		if len(path) >= 3 && path[0] == '/' && isAlpha(path[1]) && path[2] == ':' {
			path = path[1:]
		}
		path = filepath.FromSlash(path)
	}
	return path, nil
}
