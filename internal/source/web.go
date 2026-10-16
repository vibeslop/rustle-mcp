package source

import (
	"context"
	"fmt"
	"io"
	"iter"
	"math/big"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vibeslop/rustle-mcp/internal/fetch"
	"github.com/vibeslop/rustle-mcp/internal/jsonv"
	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

// Candidate is a video a web page names.
type Candidate struct {
	URL   string
	Title string
	// Kind is "direct", "youtube" or "hls".
	Kind string
}

const (
	htmlLimit     = 4 * 1024 * 1024
	maxCandidates = 100
	maxIframes    = 8
)

func suffix(rawURL string) string {
	u, ok := ParseURL(rawURL)
	if !ok {
		return ""
	}
	return strings.ToLower(extname(u.Pathname()))
}

// mediaKind is what a URL's extension says it is, without asking the server: "media",
// "hls", or "" when it doesn't say.
func mediaKind(rawURL string) string {
	ext := suffix(rawURL)
	if ext == ".m3u8" {
		return "hls"
	}
	if VideoSuffixes[ext] || AudioSuffixes[ext] {
		return "media"
	}
	return ""
}

func candidateKind(kind string) string {
	if kind == "hls" {
		return "hls"
	}
	return "direct"
}

// header is a response header as Headers.get reads it: repeated values joined.
func header(h http.Header, name string) string { return strings.Join(h.Values(name), ", ") }

// contentType is the content type from HEAD, or from a one-byte ranged GET for servers
// that refuse HEAD; "" when neither answers. Neither reads a body.
func contentType(ctx context.Context, rawURL string) (string, error) {
	for _, ranged := range []bool{false, true} {
		method := http.MethodHead
		if ranged {
			method = http.MethodGet
		}
		attempt, cancel := context.WithTimeout(ctx, 15*time.Second)
		req, err := http.NewRequestWithContext(attempt, method, rawURL, nil)
		if err != nil {
			cancel()
			continue
		}
		req.Header.Set("User-Agent", fetch.BrowserUA)
		if ranged {
			req.Header.Set("Range", "bytes=0-0")
		}
		resp, err := fetch.Client().Do(req)
		if err != nil {
			cancel()
			if ctx.Err() != nil {
				return "", rustle.Cancelled()
			}
			continue
		}
		resp.Body.Close()
		cancel()
		if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
			return strings.ToLower(header(resp.Header, "Content-Type")), nil
		}
	}
	return "", nil
}

// ClassifyURL says whether a URL is media ffmpeg can open directly ("media"), an HLS
// playlist ("hls"), or a page to scan ("page").
func ClassifyURL(ctx context.Context, rawURL string) (string, error) {
	if known := mediaKind(rawURL); known != "" {
		return known, nil
	}
	t, err := contentType(ctx, rawURL)
	if err != nil {
		return "", err
	}
	// audio/mpegurl is a playlist, so this test comes before audio/*.
	if strings.Contains(t, "mpegurl") {
		return "hls", nil
	}
	if strings.HasPrefix(t, "video/") || strings.HasPrefix(t, "audio/") || strings.HasPrefix(t, "application/octet-stream") {
		return "media", nil
	}
	return "page", nil
}

// --- HTML ---------------------------------------------------------------------------

var entities = map[string]string{
	"amp": "&", "lt": "<", "gt": ">", "quot": `"`, "apos": "'", "nbsp": "\u00a0", "ndash": "–", "mdash": "—", "hellip": "…",
	"lsquo": "‘", "rsquo": "’", "ldquo": "“", "rdquo": "”", "laquo": "«", "raquo": "»", "middot": "·", "bull": "•",
	"copy": "©", "reg": "®", "trade": "™",
}

// The letters are spelled out rather than matched with (?i), which also folds non-ASCII
// letters such as the Kelvin sign into names.
var entity = regexp.MustCompile(`&(#[xX][0-9a-fA-F]+|#[0-9]+|[a-zA-Z]+[0-9]*)(;?)`)

// DecodeEntities decodes character references and the common named entities.
func DecodeEntities(text string) string {
	if !strings.Contains(text, "&") {
		return text
	}
	matches := entity.FindAllStringSubmatchIndex(text, -1)
	if matches == nil {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	last := 0
	for _, m := range matches {
		b.WriteString(text[last:m[0]])
		b.WriteString(decodeEntity(text, m))
		last = m[1]
	}
	b.WriteString(text[last:])
	return b.String()
}

func decodeEntity(text string, m []int) string {
	whole, name, semi := text[m[0]:m[1]], text[m[2]:m[3]], m[5] > m[4]
	if name[0] == '#' {
		var code int64
		var err error
		if name[1] == 'x' || name[1] == 'X' {
			code, err = strconv.ParseInt(name[2:], 16, 64)
		} else {
			code, err = strconv.ParseInt(name[1:], 10, 64)
		}
		if err != nil || code <= 0 || code > 0x10ffff {
			return whole
		}
		// A surrogate, which a Go string can't hold alone, becomes U+FFFD.
		return string(rune(code))
	}
	// Browsers decode only a few names without ';', and never before '=': "?a=1&copy=2" is a query string.
	if !semi {
		switch name {
		case "amp", "lt", "gt", "quot":
			if m[1] < len(text) && text[m[1]] == '=' {
				return whole
			}
		default:
			return whole
		}
	}
	if value, ok := entities[strings.ToLower(name)]; ok {
		return value
	}
	return whole
}

func clean(text string) string { return rustle.Cut(rustle.CollapseSpace(DecodeEntities(text)), 300) }

const space = "[" + rustle.SpaceClass + "]"

var attrPattern = regexp.MustCompile(`([^` + rustle.SpaceClass + `"'>/=]+)(?:` + space + `*=` + space + `*(?:"([^"]*)"|'([^']*)'|([^` + rustle.SpaceClass + `>]+)))?`)

// Their content is text, not markup: tags inside a script string are not elements.
var rawText = map[string]bool{"script": true, "style": true, "title": true, "textarea": true}

var metaVideo = setOf("og:video", "og:video:url", "og:video:secure_url", "twitter:player:stream")

type tag struct {
	close bool
	name  string
	attrs string
	// text is the content of a raw-text element (script, style, title, textarea).
	text string
}

// tags yields the tags of html in document order, comments skipped. Every character is
// visited once: a regex over the whole page backtracks quadratically on a stray '<' or an
// unclosed quote. As in a browser, a tag still open at the end of the input is dropped.
func tags(html string) iter.Seq[tag] {
	return func(yield func(tag) bool) {
		i := 0
		for {
			lt := strings.IndexByte(html[i:], '<')
			if lt < 0 {
				return
			}
			lt += i
			if strings.HasPrefix(html[lt:], "<!--") {
				end := strings.Index(html[lt+4:], "-->")
				if end < 0 {
					return
				}
				i = lt + 4 + end + 3
				continue
			}
			closing, name, nameEnd, ok := tagName(html, lt+1)
			if !ok {
				i = lt + 1
				continue
			}
			// The tag ends at the first '>' outside a quoted value; a quote opens a value only after '='.
			j := nameEnd
			var quote, previous byte
			for ; j < len(html); j++ {
				c := html[j]
				if quote != 0 {
					if c == quote {
						quote, previous = 0, 0
					}
				} else if c == '>' {
					break
				} else if (c == '"' || c == '\'') && previous == '=' {
					quote = c
				} else if c > ' ' {
					previous = c
				}
			}
			if j >= len(html) {
				return
			}
			t := tag{close: closing, name: strings.ToLower(name), attrs: html[nameEnd:j]}
			i = j + 1
			if !closing && rawText[t.name] {
				start, end := closingTag(html, i, t.name)
				t.text = html[i:start]
				i = end
			}
			if !yield(t) {
				return
			}
		}
	}
}

// tagName reads an optional '/' and a name at html[at:], as /(\/?)([a-zA-Z][^\s/>]*)/y does.
func tagName(html string, at int) (closing bool, name string, end int, ok bool) {
	if at < len(html) && html[at] == '/' {
		closing = true
		at++
	}
	if at >= len(html) || !isAlpha(html[at]) {
		return false, "", 0, false
	}
	start := at
	for at++; at < len(html); {
		r, size := utf8.DecodeRuneInString(html[at:])
		if r == '/' || r == '>' || rustle.IsSpace(r) {
			break
		}
		at += size
	}
	return closing, html[start:at], at, true
}

// closingTag finds </name\s*>, ASCII case-insensitively, from html[from:]; without one the
// element runs to the end.
func closingTag(html string, from int, name string) (start, end int) {
	for i := from; ; {
		k := strings.Index(html[i:], "</")
		if k < 0 {
			return len(html), len(html)
		}
		k += i
		j := k + 2
		if j+len(name) <= len(html) && asciiEqualFold(html[j:j+len(name)], name) {
			j += len(name)
			for j < len(html) {
				r, size := utf8.DecodeRuneInString(html[j:])
				if !rustle.IsSpace(r) {
					break
				}
				j += size
			}
			if j < len(html) && html[j] == '>' {
				return k, j + 1
			}
		}
		i = k + 1
	}
}

func asciiEqualFold(s, lower string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != lower[i] {
			return false
		}
	}
	return true
}

func attributes(text string) map[string]string {
	out := map[string]string{}
	for _, m := range attrPattern.FindAllStringSubmatchIndex(text, -1) {
		name := strings.ToLower(text[m[2]:m[3]])
		if _, seen := out[name]; seen {
			continue
		}
		value := ""
		for g := 4; g <= 8; g += 2 {
			if m[g] >= 0 {
				value = text[m[g]:m[g+1]]
				break
			}
		}
		out[name] = DecodeEntities(value)
	}
	return out
}

// PageVideo is a video source named in a page's HTML.
type PageVideo struct {
	URL   string
	Title string
	// Kind is "direct" for a media URL, "embed" for a player page.
	Kind string
}

// ParsedPage is what ParsePage finds in a page.
type ParsedPage struct {
	Title   string
	Videos  []PageVideo
	Iframes []string
}

// absoluteURL resolves raw against base and accepts only http(s) URLs with a host and no
// credentials; anything else in a page is ignored.
func absoluteURL(raw string, base *URL) (string, bool) {
	value := rustle.TrimSpace(raw)
	if value == "" || base == nil || rustle.UTF16Len(value) > 16384 || strings.ContainsFunc(value, func(r rune) bool { return r < 0x20 }) {
		return "", false
	}
	u, ok := parse(value, base)
	if !ok || u.Scheme != "http" && u.Scheme != "https" || u.Hostname == "" || u.Username != "" || u.Password != "" {
		return "", false
	}
	return u.Href(), true
}

// ParsePage finds the video sources named in HTML, in document order, resolved against the
// page URL (or its <base>).
func ParsePage(html, pageURL string) ParsedPage {
	title := ""
	base := pageURL
	baseURL, _ := ParseURL(pageURL)
	videoDepth := 0
	var found []PageVideo
	var iframes []string
	for t := range tags(html) {
		if t.close {
			if t.name == "video" {
				videoDepth = max(0, videoDepth-1)
			}
			continue
		}
		a := attributes(t.attrs)
		switch t.name {
		case "title":
			if title == "" {
				title = clean(t.text)
			}
		case "base":
			if href := a["href"]; href != "" && base == pageURL {
				if resolved, ok := absoluteURL(href, baseURL); ok {
					base = resolved
				}
			}
		case "video":
			if !strings.HasSuffix(strings.TrimRightFunc(t.attrs, rustle.IsSpace), "/") {
				videoDepth++
			}
			if src := a["src"]; src != "" {
				found = append(found, PageVideo{src, a["title"], "direct"})
			}
		case "source":
			if src := a["src"]; videoDepth > 0 && src != "" {
				found = append(found, PageVideo{src, "", "direct"})
			}
		case "meta":
			key := a["property"]
			if key == "" {
				key = a["name"]
			}
			if content := a["content"]; content != "" && metaVideo[strings.ToLower(key)] {
				found = append(found, PageVideo{content, "", "embed"})
			}
		case "iframe":
			if src := a["src"]; src != "" && len(iframes) < maxIframes {
				iframes = append(iframes, src)
			}
		case "script":
			if strings.HasPrefix(strings.ToLower(a["type"]), "application/ld+json") {
				found = append(found, jsonLDVideos(t.text)...)
			}
		}
	}
	if base != pageURL {
		baseURL, _ = ParseURL(base)
	}
	page := ParsedPage{Title: title, Videos: []PageVideo{}, Iframes: []string{}}
	seen := map[string]bool{}
	for _, item := range found {
		u, ok := absoluteURL(item.URL, baseURL)
		if !ok || seen[u] || len(page.Videos) >= maxCandidates {
			continue
		}
		seen[u] = true
		kind := item.Kind
		// A media extension means the "player" is the file itself.
		if kind == "embed" && mediaKind(u) != "" {
			kind = "direct"
		}
		page.Videos = append(page.Videos, PageVideo{u, clean(item.Title), kind})
	}
	for _, raw := range iframes {
		if u, ok := absoluteURL(raw, baseURL); ok {
			page.Iframes = append(page.Iframes, u)
		}
	}
	return page
}

// jsonLDVideos are the VideoObjects in a JSON-LD script, skipped when it isn't JSON.
func jsonLDVideos(text string) []PageVideo {
	data, err := jsonv.Parse([]byte(text))
	if err != nil {
		return nil
	}
	var found []PageVideo
	jsonv.Walk(data, func(node *jsonv.Object) bool {
		t := node.Get("@type")
		if !isVideoObject(t) {
			return true
		}
		label, _ := jsonv.Str(node.Get("name"))
		if u, ok := jsonv.Str(node.Get("contentUrl")); ok {
			found = append(found, PageVideo{u, label, "direct"})
		}
		if u, ok := jsonv.Str(node.Get("embedUrl")); ok {
			found = append(found, PageVideo{u, label, "embed"})
		}
		return true
	})
	return found
}

func isVideoObject(t any) bool {
	if s, ok := t.(string); ok {
		return s == "VideoObject"
	}
	for _, item := range jsonv.Arr(t) {
		if s, ok := item.(string); ok && s == "VideoObject" {
			return true
		}
	}
	return false
}

func watchURL(u string) string {
	id, err := YouTubeID(u)
	if err != nil || id == "" {
		return ""
	}
	return "https://www.youtube.com/watch?v=" + id
}

func onYouTube(rawURL string) bool {
	u, ok := ParseURL(rawURL)
	return ok && YouTubeHosts[strings.ToLower(u.Hostname)]
}

// PageVideos are the video sources named in a page's HTML: video and source tags,
// OpenGraph, JSON-LD, YouTube embeds and, with followEmbeds, one level of iframes.
func PageVideos(ctx context.Context, rawURL string, followEmbeds bool) ([]Candidate, error) {
	if known := mediaKind(rawURL); known != "" {
		return []Candidate{{rawURL, FileTitle(rawURL), candidateKind(known)}}, nil
	}
	page, err := fetch.Get(ctx, rawURL, fetch.Options{Limit: htmlLimit, Headers: map[string]string{"Accept": "text/html,*/*;q=0.8"}, What: "The page"})
	if err != nil {
		// A media file without an extension is larger than any page; HEAD tells which it is.
		if !rustle.HasCode(err, "DOWNLOAD_TOO_LARGE") {
			return nil, err
		}
		kind, classifyErr := ClassifyURL(ctx, rawURL)
		if classifyErr != nil {
			return nil, classifyErr
		}
		if kind == "page" {
			return nil, err
		}
		return []Candidate{{rawURL, FileTitle(rawURL), candidateKind(kind)}}, nil
	}
	t := strings.ToLower(header(page.Header, "Content-Type"))
	if strings.Contains(t, "mpegurl") {
		return []Candidate{{rawURL, FileTitle(rawURL), "hls"}}, nil
	}
	if strings.HasPrefix(t, "video/") || strings.HasPrefix(t, "audio/") {
		return []Candidate{{rawURL, FileTitle(rawURL), "direct"}}, nil
	}

	parsed := ParsePage(decodeBody(page.Body, t), page.URL)
	var results []Candidate
	embeds := slices.Clone(parsed.Iframes)
	for _, video := range parsed.Videos {
		title := video.Title
		if title == "" {
			title = parsed.Title
		}
		if title == "" {
			title = "Video"
		}
		if youtube := watchURL(video.URL); youtube != "" {
			results = append(results, Candidate{youtube, title, "youtube"})
		} else if video.Kind == "embed" {
			embeds = append(embeds, video.URL)
		} else {
			results = append(results, Candidate{video.URL, title, candidateKind(mediaKind(video.URL))})
		}
	}
	var unique []string
	seen := map[string]bool{}
	for _, embed := range embeds {
		if !seen[embed] {
			seen[embed] = true
			unique = append(unique, embed)
		}
	}
	for _, embed := range unique {
		if youtube := watchURL(embed); youtube != "" {
			title := parsed.Title
			if title == "" {
				title = "YouTube video"
			}
			results = append(results, Candidate{youtube, title, "youtube"})
		}
	}
	// Ad and tracking iframes are only worth a fetch when the page itself named nothing playable.
	if len(results) == 0 && followEmbeds {
		followed := 0
		for _, embed := range unique {
			if embed == rawURL || onYouTube(embed) {
				continue
			}
			if followed == 2 {
				break
			}
			followed++
			inner, err := PageVideos(ctx, embed, false)
			if err != nil {
				if ctx.Err() != nil {
					return nil, err
				}
				continue
			}
			results = append(results, inner...)
			if len(results) > 0 {
				break
			}
		}
	}
	out := []Candidate{}
	seen = map[string]bool{}
	for _, candidate := range results {
		if !seen[candidate.URL] && len(out) < maxCandidates {
			seen[candidate.URL] = true
			out = append(out, candidate)
		}
	}
	return out, nil
}

// FileTitle is the URL's file name; a literal '%' that isn't an escape leaves the name as written.
func FileTitle(rawURL string) string {
	name := ""
	if u, ok := ParseURL(rawURL); ok {
		pathname := u.Pathname()
		name = pathname[strings.LastIndexByte(pathname, '/')+1:]
		if decoded, ok := DecodeURIComponent(name); ok {
			name = decoded
		}
	}
	if name = rustle.Cut(name, 300); name == "" {
		return "Remote video"
	}
	return name
}

// extname is a name's extension: from its last dot, but "" for a hidden file's name
// (".mp4") and for "..". Trailing slashes are ignored.
func extname(p string) string {
	name := path.Base(p)
	dot := strings.LastIndexByte(name, '.')
	if dot <= 0 || name == ".." {
		return ""
	}
	return name[dot:]
}

// --- Local folders ------------------------------------------------------------------

const maxScanned = 1000

// FileSize is 12 B or 3.4 MB: decimal units, as file managers show them.
func FileSize(bytes int64) string {
	size := float64(bytes)
	for _, unit := range []string{"B", "KB", "MB", "GB"} {
		if size < 1000 || unit == "GB" {
			if unit == "B" {
				return rustle.NumString(size) + " B"
			}
			return toFixed1(size) + " " + unit
		}
		size /= 1000
	}
	return ""
}

// toFixed1 is JavaScript's toFixed(1) for sizes: the exact binary value rounded to one
// decimal, halves up. strconv rounds exact halves to even, so 1.25 would print 1.2.
func toFixed1(v float64) string {
	x := new(big.Float).SetPrec(128).SetFloat64(v)
	x.Mul(x, big.NewFloat(10))
	n, _ := x.Int(nil)
	if x.Sub(x, new(big.Float).SetInt(n)).Cmp(big.NewFloat(0.5)) >= 0 {
		n.Add(n, big.NewInt(1))
	}
	digits := n.String()
	if len(digits) < 2 {
		digits = "0" + digits
	}
	return digits[:len(digits)-1] + "." + digits[len(digits)-1:]
}

// mtimeMs is a modification time as Node's stat reports mtimeMs.
func mtimeMs(t time.Time) float64 { return float64(t.Unix())*1000 + float64(t.Nanosecond())/1e6 }

// stamp is local time as YYYY-MM-DD HH:MM.
func stamp(t time.Time) string {
	t = t.Local()
	return fmt.Sprintf("%d-%02d-%02d %02d:%02d", t.Year(), int(t.Month()), t.Day(), t.Hour(), t.Minute())
}

// isMpegTS: TypeScript sources share the MPEG-TS extension, so .ts files are listed only
// when they sniff as video.
func isMpegTS(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 189)
	n, err := io.ReadFull(f, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return false
	}
	return Sniff(head[:n]) == "mpegts"
}

// LocalVideos lists the media files in one local folder, newest first, so a fresh render
// is listed first. source is the folder's resolved path.
func LocalVideos(folder string, s *rustle.Settings, limit int) (source string, rows []rustle.VideoRow, err error) {
	path := rustle.TrimSpace(folder)
	if len(path) >= 5 && strings.EqualFold(path[:5], "file:") {
		if path, err = FileURLToPath(path); err != nil {
			return "", nil, rustle.Fail("Use a local folder path or a file URL without a remote host.", "INVALID_SOURCE", "")
		}
	}
	root, err := RootedPath(path, s)
	if err != nil {
		return "", nil, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", nil, rustle.Fail(fmt.Sprintf("Folder not found: %s.", DisplayPath(root)), "SOURCE_NOT_FOUND",
			"Check the path; pass an absolute path or one starting with ~.")
	}
	if !info.IsDir() {
		return "", nil, rustle.Fail(fmt.Sprintf("%s is a file, not a folder.", DisplayPath(root)), "INVALID_SOURCE",
			"Pass it as source to read_transcript, watch_video, inspect_frame or listen_video.")
	}

	unreadable := rustle.Fail(fmt.Sprintf("Could not read the folder %s.", DisplayPath(root)), "ACCESS_DENIED",
		"Check that the folder is readable, or pass a file inside it.")
	dir, err := os.Open(root)
	if err != nil {
		return "", nil, unreadable
	}
	// Directory order, unsorted: a huge folder is cut off after maxScanned entries.
	entries, err := dir.ReadDir(maxScanned)
	dir.Close()
	if err != nil && err != io.EOF {
		return "", nil, unreadable
	}
	type file struct {
		name  string
		size  int64
		mtime time.Time
		ms    float64
	}
	var found []file
	for _, entry := range entries {
		ext := strings.ToLower(extname(entry.Name()))
		if !VideoSuffixes[ext] && !AudioSuffixes[ext] {
			continue
		}
		full := filepath.Join(root, entry.Name())
		// Lstat, so a symlink out of the allowed folders is never listed.
		info, err := os.Lstat(full)
		if err != nil || !info.Mode().IsRegular() || ext == ".ts" && !isMpegTS(full) {
			continue
		}
		found = append(found, file{entry.Name(), info.Size(), info.ModTime(), mtimeMs(info.ModTime())})
	}
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].ms != found[j].ms {
			return found[i].ms > found[j].ms
		}
		return found[i].name < found[j].name
	})
	end := limit
	if end < 0 {
		end = max(0, len(found)+end)
	}
	rows = []rustle.VideoRow{}
	for _, f := range found[:min(end, len(found))] {
		rows = append(rows, rustle.VideoRow{
			Title:    rustle.Cut(f.name, 300),
			Size:     FileSize(f.size),
			Modified: stamp(f.mtime),
			URL:      filepath.Join(root, f.name),
		})
	}
	return root, rows, nil
}
