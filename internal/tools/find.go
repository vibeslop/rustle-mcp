package tools

import (
	"math"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/vibeslop/rustle-mcp/internal/rustle"
	"github.com/vibeslop/rustle-mcp/internal/source"
	"github.com/vibeslop/rustle-mcp/internal/youtube"
)

// fields are the columns a listing may have. The last is what the other tools take, so it
// is named for the argument it fills.
var fields = []struct {
	name  string
	label string
	get   func(*rustle.VideoRow) *string
}{
	{"title", "title", func(r *rustle.VideoRow) *string { return &r.Title }},
	{"channel", "channel", func(r *rustle.VideoRow) *string { return &r.Channel }},
	{"duration", "duration", func(r *rustle.VideoRow) *string { return &r.Duration }},
	{"views", "views", func(r *rustle.VideoRow) *string { return &r.Views }},
	{"published", "published", func(r *rustle.VideoRow) *string { return &r.Published }},
	{"size", "size", func(r *rustle.VideoRow) *string { return &r.Size }},
	{"modified", "modified", func(r *rustle.VideoRow) *string { return &r.Modified }},
	{"url", "source", func(r *rustle.VideoRow) *string { return &r.URL }},
}

// hoisted are fields shared by every row (a channel's uploads, one page's title); they
// move to the metadata.
var hoisted = []string{"channel", "title"}

// youtubePage reads a YouTube channel or search page passed as url as what channel= or
// query= would list.
func youtubePage(raw string) (channel, query string, ok bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", false
	}
	switch strings.ToLower(u.Hostname()) {
	case "youtube.com", "www.youtube.com", "m.youtube.com":
	default:
		return "", "", false
	}
	path := u.EscapedPath()
	if channelPrefix.MatchString(path) {
		return raw, "", true
	}
	if path == "/results" {
		if terms := rustle.TrimSpace(u.Query().Get("search_query")); terms != "" {
			return "", terms, true
		}
	}
	return "", "", false
}

var channelPrefix = regexp.MustCompile(`^/(@|channel/|c/|user/)`)

// shortSource is the bare video id: a fifth of a watch URL, and every tool accepts it as source.
func shortSource(u string) string {
	if id, err := source.YouTubeID(u); err == nil && id != "" {
		return id
	}
	return u
}

var (
	age   = regexp.MustCompile(`(?i)\b(\d+) (second|minute|hour|day|week|month|year)s? ago\b`)
	units = map[string]string{"second": "s", "minute": "min", "hour": "h", "day": "d", "week": "w", "month": "mo", "year": "y"}
)

// ShortAge writes "5 years ago" as "5y ago".
func ShortAge(text string) string {
	m := age.FindStringSubmatchIndex(text)
	if m == nil {
		return text
	}
	return text[:m[0]] + text[m[2]:m[3]] + units[strings.ToLower(text[m[4]:m[5]])] + " ago" + text[m[1]:]
}

func abbreviate(n float64) string {
	for _, step := range []struct {
		size float64
		unit string
	}{{1e9, "B"}, {1e6, "M"}, {1e3, "K"}} {
		if n < step.size {
			continue
		}
		value := n / step.size
		if value < 10 {
			return rustle.NumString(math.Floor(value*10)/10) + step.unit
		}
		return rustle.NumString(math.Floor(value)) + step.unit
	}
	return rustle.NumString(n)
}

var (
	noViews   = regexp.MustCompile(`(?i)^no views$`)
	viewCount = regexp.MustCompile(`(?i)^([\d.,]+) ?([KMB]?)(?: views?)?$`)
)

// CompactViews writes "155K views" and "155,577 views" as "155K" under a views column;
// "1.2K watching" stays as is.
func CompactViews(text string) string {
	value := rustle.CollapseSpace(text)
	if noViews.MatchString(value) {
		return "0"
	}
	m := viewCount.FindStringSubmatch(value)
	if m == nil {
		return value
	}
	if m[2] != "" {
		return m[1] + strings.ToUpper(m[2])
	}
	n := rustle.Number(strings.ReplaceAll(m[1], ",", ""))
	if !rustle.Finite(n) {
		return value
	}
	return abbreviate(n)
}

func cell(field, value string) string {
	if value == "" {
		return "-"
	}
	if field == "url" {
		return shortSource(value)
	}
	text := rustle.CollapseSpace(value)
	switch field {
	case "views":
		text = CompactViews(text)
	case "published":
		text = ShortAge(text)
	}
	// "What's new | Early look" would otherwise read as two columns.
	return strings.ReplaceAll(text, "|", "/")
}

// RenderRows is a header naming the columns, then one line per video; constant columns
// move to meta.
func RenderRows(rows []rustle.VideoRow, meta *rustle.Obj) string {
	if len(rows) == 0 {
		return "No videos found."
	}
	var shown []int
	for i, f := range fields {
		for k := range rows {
			if *f.get(&rows[k]) != "" {
				shown = append(shown, i)
				break
			}
		}
	}
	for _, name := range hoisted {
		var f int
		for i := range fields {
			if fields[i].name == name {
				f = i
			}
		}
		first := *fields[f].get(&rows[0])
		same := len(rows) > 1 && first != ""
		for k := range rows {
			same = same && *fields[f].get(&rows[k]) == first
		}
		if same {
			meta.Set(name, rustle.CollapseSpace(first))
			kept := shown[:0]
			for _, i := range shown {
				if i != f {
					kept = append(kept, i)
				}
			}
			shown = kept
		}
	}
	lines := make([]string, 0, len(rows)+1)
	header := make([]string, len(shown))
	for k, i := range shown {
		header[k] = fields[i].label
	}
	lines = append(lines, strings.Join(header, " | "))
	for r := range rows {
		cells := make([]string, len(shown))
		for k, i := range shown {
			cells[k] = cell(fields[i].name, *fields[i].get(&rows[r]))
		}
		lines = append(lines, strings.Join(cells, " | "))
	}
	return strings.Join(lines, "\n")
}

var (
	hasScheme  = regexp.MustCompile(`(?i)^[a-z][a-z0-9+.-]+:`)
	bareDomain = regexp.MustCompile(`^[\w-]+(\.[\w-]+)+/`)
	schemeOf   = regexp.MustCompile(`(?i)^([a-z][a-z0-9+.-]+):`)
)

// Route is what the arguments ask for. A bare domain gets https, as Identify does for
// sources, and a YouTube channel or search page passed as url becomes channel or query.
func Route(args FindArgs) FindArgs {
	u := rustle.TrimSpace(args.URL)
	if u != "" && !hasScheme.MatchString(u) && bareDomain.MatchString(u) {
		if _, err := os.Stat(u); err != nil {
			u = "https://" + u
		}
	}
	if u != "" && args.Channel == "" {
		if channel, query, ok := youtubePage(u); ok {
			args.Channel = channel
			if query != "" {
				args.Query = query
			}
			u = ""
		}
	}
	args.URL = u
	return args
}

// find searches YouTube, lists a channel, or lists the videos on a web page or in a local folder.
func find(c *rustle.Call, raw FindArgs) (*rustle.Result, error) {
	args := Route(raw)
	var meta *rustle.Obj
	var rows []rustle.VideoRow
	switch {
	case args.URL != "":
		scheme := ""
		if m := schemeOf.FindStringSubmatch(args.URL); m != nil {
			scheme = strings.ToLower(m[1])
		}
		switch scheme {
		case "", "file":
			root, listed, err := source.LocalVideos(args.URL, c.Settings, args.Limit)
			if err != nil {
				return nil, err
			}
			// The path ends in the file name, so a title column would repeat it.
			for i := range listed {
				listed[i].Title = ""
			}
			rows = listed
			meta = rustle.NewObj().Set("scope", "media files, newest first").Set("source", root)
		case "http", "https":
			found, err := source.PageVideos(c.Ctx, args.URL, true)
			if err != nil {
				return nil, err
			}
			if len(found) > args.Limit {
				c.Notes.Add(strconv.Itoa(args.Limit) + " of " + strconv.Itoa(len(found)) + " shown")
				found = found[:args.Limit]
			}
			for _, f := range found {
				rows = append(rows, rustle.VideoRow{Title: f.Title, URL: f.URL})
			}
			meta = rustle.NewObj().Set("scope", "video sources in the page HTML; scripts not run").Set("source", args.URL)
		default:
			return nil, rustle.Fail("Unsupported url scheme '"+scheme+"'.", "INVALID_SOURCE",
				"Pass an http(s) page, a YouTube channel link or a local folder.")
		}
	case args.Channel != "":
		listing, listed, err := youtube.ChannelVideos(c, args.Channel, args.Query, args.Limit)
		if err != nil {
			return nil, err
		}
		rows = listed
		scope := "channel uploads, newest first"
		if args.Query != "" {
			scope = "channel search"
		}
		meta = rustle.NewObj().Set("scope", scope).Set("source", listing)
	case args.Query != "":
		found, err := youtube.Search(c, args.Query, args.Limit)
		if err != nil {
			return nil, err
		}
		rows = found
		meta = rustle.NewObj().Set("scope", "YouTube search")
	default:
		return nil, rustle.Fail("Nothing to find.", "INVALID_ARGUMENTS",
			"Pass query to search YouTube, channel to list a channel, or url to list the videos on a page or folder.")
	}
	text := RenderRows(rows, meta)
	meta.Set("count", len(rows))
	return &rustle.Result{Meta: meta, Content: []rustle.Block{rustle.TextBlock(text)}}, nil
}
