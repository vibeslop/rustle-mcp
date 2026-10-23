// Package transcript turns captions, sidecar files and local speech recognition into
// timestamped paragraphs, and searches, pages and digests them.
//
// Lengths that feed a budget, a cost or a time count UTF-16 code units (rustle.UTF16Len),
// the unit the budgets and the digest ratio were measured in. Several patterns need
// backreferences or lookahead, which Go's regexp lacks; they are small hand-written
// matchers whose comments give the pattern they match.
package transcript

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

// Cue is a caption's text and when it is shown, in seconds.
type Cue struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
}

// Paragraph is cues merged up to a sentence end.
type Paragraph = Cue

// CaptionBytes is the largest caption file read.
const CaptionBytes = 8 << 20

// MaxMatches is the most matches a search shows.
const MaxMatches = 30

// MinParagraph and MaxParagraph are paragraph lengths in seconds. Each paragraph costs a
// timestamp, so longer ones are cheaper: 20-40 s reads 0.9% fewer tokens than 15-30 s on
// 11 real tracks, and timestamps stay 23 s apart at the median and 48 s at worst. 30-60 s
// would save another 0.9% at twice the citation error. Search results use short
// paragraphs instead: a match plus one paragraph either side is then about 30 s of
// speech, and its timestamp lands near it.
const (
	MinParagraph = 20.0
	MaxParagraph = 40.0
)

const pause = 2.0

var (
	errNotTimedText = errors.New("not timed text")
	errBadTime      = errors.New("bad time")
)

var (
	timedText = regexp.MustCompile(`<(?:transcript|timedtext)\b`)
	timing    = regexp.MustCompile(`^[` + rustle.SpaceClass + `]*(\d[\d:.,]*)[` + rustle.SpaceClass + `]*-->[` + rustle.SpaceClass + `]*(\d[\d:.,]*)`)
)

// closers may follow a sentence's last mark: quotes and closing brackets.
const closers = `'"”’)]」』`

var named = map[string]string{
	"amp": "&", "lt": "<", "gt": ">", "quot": `"`, "apos": "'", "nbsp": "\u00a0", "lrm": "\u200e", "rlm": "\u200f", "shy": "\u00ad",
	"hellip": "…", "mdash": "—", "ndash": "–", "lsquo": "‘", "rsquo": "’", "ldquo": "“", "rdquo": "”", "laquo": "«", "raquo": "»",
	"copy": "©", "reg": "®", "trade": "™", "deg": "°", "times": "×", "middot": "·", "bull": "•", "iexcl": "¡", "iquest": "¿",
}

// Unescape decodes HTML character references; YouTube's srv1 escapes its already escaped
// text a second time. Like /&(?:#(\d{1,7})|#[xX]([\da-fA-F]{1,6})|([a-zA-Z]+));/g, a
// reference must end in ";", and code points that are not characters become U+FFFD.
func Unescape(text string) string {
	i := strings.IndexByte(text, '&')
	if i < 0 {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	last := 0
	for i >= 0 {
		if value, end, ok := reference(text, i); ok {
			b.WriteString(text[last:i])
			b.WriteString(value)
			last = end
			i = end
		} else {
			i++
		}
		next := strings.IndexByte(text[i:], '&')
		if next < 0 {
			break
		}
		i += next
	}
	b.WriteString(text[last:])
	return b.String()
}

// reference decodes the character reference at text[at], an "&", and returns where it ends.
func reference(text string, at int) (string, int, bool) {
	i := at + 1
	if i < len(text) && text[i] == '#' {
		i++
		hex := i < len(text) && (text[i] == 'x' || text[i] == 'X')
		limit, base := 7, 10
		if hex {
			i++
			limit, base = 6, 16
		}
		from := i
		code := 0
		for i < len(text) && digit(text[i], base) >= 0 {
			if i-from < limit {
				code = code*base + digit(text[i], base)
			}
			i++
		}
		if n := i - from; n == 0 || n > limit || i >= len(text) || text[i] != ';' {
			return "", 0, false
		}
		if code > 0 && code <= 0x10ffff && !(code >= 0xd800 && code <= 0xdfff) {
			return string(rune(code)), i + 1, true
		}
		return "\ufffd", i + 1, true
	}
	for i < len(text) && ('a' <= text[i]|0x20 && text[i]|0x20 <= 'z') {
		i++
	}
	if i == at+1 || i >= len(text) || text[i] != ';' {
		return "", 0, false
	}
	// An unknown name is kept as written.
	if value, ok := named[text[at+1:i]]; ok {
		return value, i + 1, true
	}
	return text[at : i+1], i + 1, true
}

func digit(c byte, base int) int {
	switch {
	case '0' <= c && c <= '9':
		return int(c - '0')
	case base == 16 && 'a' <= c|0x20 && c|0x20 <= 'f':
		return int(c|0x20-'a') + 10
	}
	return -1
}

// Clean decodes references, drops styling tags and collapses white space.
func Clean(text string) string {
	if strings.IndexByte(text, '&') >= 0 {
		text = Unescape(text)
	}
	if strings.IndexByte(text, '<') >= 0 {
		text = removeTags(text)
	}
	return rustle.CollapseSpace(text)
}

// removeTags drops the styling tags of WebVTT/SRT and old YouTube tracks, plus WebVTT
// per-word timestamps. Other angle brackets are kept because speech about code says "a < b".
func removeTags(text string) string {
	var b strings.Builder
	last := 0
	for i := strings.IndexByte(text, '<'); i >= 0; {
		next := i + 1
		if end := tagEnd(text, i); end > 0 {
			b.WriteString(text[last:i])
			last, next = end, end
		}
		k := strings.IndexByte(text[next:], '<')
		if k < 0 {
			break
		}
		i = next + k
	}
	if last == 0 {
		return text
	}
	b.WriteString(text[last:])
	return b.String()
}

// tagEnd is the end of the tag at text[i], a "<", or -1, as
// /<\/?(?:[biuc]|font|v|ruby|rt|span|lang)(?:[\s.][^<>]*)?>|<\d{1,2}:\d{2}(?::\d{2})?[.,]\d{3}>/i
// matches. Only white space, "." or ">" may follow a name, so the name is a whole run of
// ASCII letters; without /u, /i folds no other letters.
func tagEnd(text string, i int) int {
	j := i + 1
	if j < len(text) && isDigit(text[j]) {
		return timeTagEnd(text, j)
	}
	if j < len(text) && text[j] == '/' {
		j++
	}
	k := j
	for k < len(text) && 'a' <= text[k]|0x20 && text[k]|0x20 <= 'z' {
		k++
	}
	if !isTagName(text[j:k]) || k >= len(text) {
		return -1
	}
	if text[k] == '>' {
		return k + 1
	}
	if r, size := utf8.DecodeRuneInString(text[k:]); r == '.' || rustle.IsSpace(r) {
		if m := strings.IndexAny(text[k+size:], "<>"); m >= 0 && text[k+size+m] == '>' {
			return k + size + m + 1
		}
	}
	return -1
}

func isTagName(name string) bool {
	if len(name) == 0 || len(name) > 4 {
		return false
	}
	switch asciiLower(name) {
	case "b", "i", "u", "c", "font", "v", "ruby", "rt", "span", "lang":
		return true
	}
	return false
}

// timeTagEnd is the end of a WebVTT word timestamp such as <00:01:02.500> whose first
// digit is at text[j], or -1.
func timeTagEnd(text string, j int) int {
	digits := func(at, n int) bool {
		for ; n > 0; n-- {
			if at >= len(text) || !isDigit(text[at]) {
				return false
			}
			at++
		}
		return true
	}
	k := j + 1
	if k < len(text) && isDigit(text[k]) {
		k++
	}
	if k >= len(text) || text[k] != ':' || !digits(k+1, 2) {
		return -1
	}
	k += 3
	if k < len(text) && text[k] == ':' && digits(k+1, 2) {
		k += 3
	}
	if k >= len(text) || text[k] != '.' && text[k] != ',' || !digits(k+1, 3) || k+4 >= len(text) || text[k+4] != '>' {
		return -1
	}
	return k + 5
}

func isDigit(c byte) bool { return '0' <= c && c <= '9' }

// asciiLower lowers ASCII letters only, keeping every byte offset.
func asciiLower(s string) string {
	for i := 0; i < len(s); i++ {
		if 'A' <= s[i] && s[i] <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if 'A' <= b[j] && b[j] <= 'Z' {
					b[j] += 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}

// splitLines splits at each line break: \r\n, \n, \r, \v, \f, U+001C-U+001E, NEL, and the
// Unicode line and paragraph separators.
func splitLines(text string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(text); {
		c, size, brk := text[i], 1, false
		switch {
		case c == '\r':
			brk = true
			if i+1 < len(text) && text[i+1] == '\n' {
				size = 2
			}
		case c == '\n' || c == '\v' || c == '\f' || 0x1c <= c && c <= 0x1e:
			brk = true
		case c >= 0x80:
			var r rune
			r, size = utf8.DecodeRuneInString(text[i:])
			brk = r == 0x85 || r == 0x2028 || r == 0x2029
		}
		if brk {
			lines = append(lines, text[start:i])
			start = i + size
		}
		i += size
	}
	return append(lines, text[start:])
}

// cleanLines cleans each displayed line; rolling captions are recognised line by line.
func cleanLines(lines []string) string {
	var b strings.Builder
	for _, line := range lines {
		if line = Clean(line); line != "" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(line)
		}
	}
	return b.String()
}

// seconds is JavaScript's Number(value) / scale, with "" as 0.
func seconds(value string, scale float64) (float64, error) {
	if value == "" {
		return 0, nil
	}
	n := rustle.Number(value)
	if !rustle.Finite(n) {
		return 0, errBadTime
	}
	return n / scale, nil
}

func timestamp(value string) (float64, error) {
	total := 0.0
	for _, part := range strings.Split(strings.ReplaceAll(value, ",", "."), ":") {
		if part == "" {
			return 0, errBadTime
		}
		s, err := seconds(part, 1)
		if err != nil {
			return 0, err
		}
		// The conversion keeps Go from fusing the multiply and add, which rounds once
		// where JavaScript rounds twice.
		total = float64(total*60) + s
	}
	return total, nil
}

// ParseCaptions reads timed-text XML (YouTube srv1 or srv3), WebVTT or SRT into cues.
func ParseCaptions(raw string) ([]Cue, error) {
	// trim also removes byte-order marks.
	text := rustle.TrimSpace(raw)
	if strings.HasPrefix(text, "<") {
		return parseTimedText(text)
	}
	lines := splitLines(text)
	type cueTiming struct {
		line       int
		begin, end string
	}
	var timings []cueTiming
	for i, line := range lines {
		if m := timing.FindStringSubmatch(line); m != nil {
			timings = append(timings, cueTiming{i, m[1], m[2]})
		}
	}
	var cues []Cue
	for n, at := range timings {
		stop := len(lines)
		if n+1 < len(timings) {
			stop = timings[n+1].line
		}
		body := lines[at.line+1 : stop]
		// The next cue's identifier: an SRT counter or a WebVTT cue id.
		if len(body) >= 2 && rustle.TrimSpace(body[len(body)-1]) != "" && rustle.TrimSpace(body[len(body)-2]) == "" {
			body = body[:len(body)-1]
		}
		// Only an empty line ends a cue: YouTube's WebVTT uses " " for a blank row.
		if empty := slices.Index(body, ""); empty >= 0 {
			body = body[:empty]
		}
		cleaned := cleanLines(body)
		if cleaned == "" {
			continue
		}
		start, err := timestamp(at.begin)
		if err != nil {
			return nil, err
		}
		end, err := timestamp(at.end)
		if err != nil {
			return nil, err
		}
		cues = append(cues, Cue{rustle.MS(start), rustle.MS(end), cleaned})
	}
	return cues, nil
}

// parseTimedText is a small reader for the two timed-text shapes YouTube serves:
// <text start dur> (srv1) and <p t d> with <s> word runs (srv3). Anything else is not a
// caption file.
func parseTimedText(text string) ([]Cue, error) {
	if !timedText.MatchString(text) {
		return nil, errNotTimedText
	}
	var cues []Cue
	for from := 0; ; {
		e, ok := nextElement(text, from)
		if !ok {
			return cues, nil
		}
		from = e.end
		var startAttr, durAttr string
		var hasStart bool
		scanAttributes(e.attrs, func(name, value string) {
			switch {
			case e.name == "text" && name == "start", e.name == "p" && name == "t":
				startAttr, hasStart = value, true
			case e.name == "text" && name == "dur", e.name == "p" && name == "d":
				durAttr = value
			}
		})
		if !hasStart {
			continue
		}
		scale := 1.0
		if e.name == "p" {
			scale = 1000
		}
		start, err := seconds(startAttr, scale)
		if err != nil {
			return nil, err
		}
		duration, err := seconds(durAttr, scale)
		if err != nil {
			return nil, err
		}
		body := e.body
		if strings.IndexByte(body, '<') >= 0 {
			body = stripTags(breakLines(body))
		}
		if lines := cleanLines(splitLines(Unescape(body))); lines != "" {
			cues = append(cues, Cue{rustle.MS(start), rustle.MS(start + duration), lines})
		}
	}
}

// element is a <text> or <p> element: its name, attributes and body (empty when self-closed).
type element struct {
	name, attrs, body string
	end               int
}

// nextElement finds the next element from `from` as
// /<(text|p)\b([^>]*?)(?:\/>|>([\s\S]*?)<\/\1\s*>)/g does, which needs a backreference.
func nextElement(text string, from int) (element, bool) {
	for {
		i := strings.IndexByte(text[from:], '<')
		if i < 0 {
			return element{}, false
		}
		i += from
		if e, ok := elementAt(text, i); ok {
			return e, true
		}
		from = i + 1
	}
}

func elementAt(text string, i int) (element, bool) {
	var name string
	switch rest := text[i+1:]; {
	case strings.HasPrefix(rest, "text"):
		name = "text"
	case strings.HasPrefix(rest, "p"):
		name = "p"
	default:
		return element{}, false
	}
	a := i + 1 + len(name)
	if a < len(text) && isWordByte(text[a]) {
		return element{}, false
	}
	// The lazy attributes stop at the first ">", or at "/>" just before it.
	g := strings.IndexByte(text[a:], '>')
	if g < 0 {
		return element{}, false
	}
	g += a
	if g > a && text[g-1] == '/' {
		return element{name: name, attrs: text[a : g-1], end: g + 1}, true
	}
	closing := "</" + name
	for k := g + 1; ; {
		c := strings.Index(text[k:], closing)
		if c < 0 {
			return element{}, false
		}
		c += k
		if j := skipRunes(text, c+len(closing), rustle.IsSpace); j < len(text) && text[j] == '>' {
			return element{name: name, attrs: text[a:g], body: text[g+1 : c], end: j + 1}, true
		}
		k = c + 1
	}
}

func isWordByte(c byte) bool {
	return 'a' <= c|0x20 && c|0x20 <= 'z' || '0' <= c && c <= '9' || c == '_'
}

func isNameByte(c byte) bool { return isWordByte(c) || c == ':' || c == '.' || c == '-' }

// scanAttributes calls fn with each name="value" or name='value', as
// /([\w:.-]+)\s*=\s*(?:"([^"]*)"|'([^']*)')/g finds them.
func scanAttributes(s string, fn func(name, value string)) {
	for i := 0; i < len(s); {
		if !isNameByte(s[i]) {
			i++
			continue
		}
		j := i + 1
		for j < len(s) && isNameByte(s[j]) {
			j++
		}
		if k := skipRunes(s, j, rustle.IsSpace); k < len(s) && s[k] == '=' {
			if k = skipRunes(s, k+1, rustle.IsSpace); k < len(s) && (s[k] == '"' || s[k] == '\'') {
				if end := strings.IndexByte(s[k+1:], s[k]); end >= 0 {
					fn(s[i:j], s[k+1:k+1+end])
					i = k + 1 + end + 1
					continue
				}
			}
		}
		// A match starting later in the same name would fail the same way.
		i = j
	}
}

// breakLines turns each <br>, <br/> or <BR />, as /<br\s*\/?>/gi, into a line break.
func breakLines(s string) string {
	var b strings.Builder
	last := 0
	for i := strings.IndexByte(s, '<'); i >= 0; {
		end := -1
		if i+2 < len(s) && s[i+1]|0x20 == 'b' && s[i+2]|0x20 == 'r' {
			j := skipRunes(s, i+3, rustle.IsSpace)
			if j < len(s) && s[j] == '/' {
				j++
			}
			if j < len(s) && s[j] == '>' {
				end = j + 1
			}
		}
		next := i + 1
		if end > 0 {
			b.WriteString(s[last:i])
			b.WriteByte('\n')
			last, next = end, end
		}
		k := strings.IndexByte(s[next:], '<')
		if k < 0 {
			break
		}
		i = next + k
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// stripTags removes everything from each "<" to the next ">", as /<[^>]*>/g.
func stripTags(s string) string {
	var b strings.Builder
	last := 0
	for {
		i := strings.IndexByte(s[last:], '<')
		if i < 0 {
			break
		}
		i += last
		j := strings.IndexByte(s[i:], '>')
		if j < 0 {
			break
		}
		b.WriteString(s[last:i])
		last = i + j + 1
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// skipRunes is the index of the first rune at or after i that is not in.
func skipRunes(s string, i int, in func(rune) bool) int {
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		if !in(r) {
			break
		}
		i += size
	}
	return i
}

// DropRepeats removes the text that rolling auto-captions repeat from the previous cue.
//
// Rolling captions show the previous cue's last line again above each new line, so those
// lines are dropped; other overlaps with the text so far count only from two words on, so
// a speaker repeating one word across cues is kept. Only a track that rolls loses
// anything: in plain tracks a repeat is speech ("garbage collection. Garbage collection
// is...", "do you? Do you?"), which deduplicating every track deleted from 17 cues of 11
// real YouTube tracks (at most 0.5% of a track's cues), while rolling tracks repeat in
// nearly every cue.
func DropRepeats(cues []Cue) []Cue {
	sorted := slices.Clone(cues)
	slices.SortStableFunc(sorted, func(a, b Cue) int { return cmp.Compare(a.Start, b.Start) })
	var result []Cue
	var tail, shown, words []string
	var ends []int
	changed := 0
	for index, cue := range sorted {
		// Even if every remaining cue repeated, fewer than half would: the track does not roll,
		// so a plain track stops matching words halfway through.
		if (changed+len(sorted)-index)*2 < len(sorted) {
			break
		}
		lines := strings.Split(cue.Text, "\n")
		first := 0
		for first < len(lines) && slices.Contains(shown, lines[first]) {
			first++
		}
		shown = lines
		text := strings.Join(lines[first:], " ")
		words, ends = words[:0], ends[:0]
		for at := 0; ; {
			start, end := nextWord(text, at)
			if start < 0 {
				break
			}
			words = append(words, strings.ToLower(text[start:end]))
			ends = append(ends, end)
			at = end
		}
		overlap := 0
		for n := min(len(words), len(tail), 60); n > 1 && overlap == 0; n-- {
			k := 0
			for k < n && tail[len(tail)-n+k] == words[k] {
				k++
			}
			if k == n {
				overlap = n
			}
		}
		if overlap > 0 {
			// The repeated words' punctuation was already emitted with them.
			text = strings.TrimLeft(text[ends[overlap-1]:], " ,.;:!?…")
		}
		if first > 0 || overlap > 0 {
			changed++
		}
		if text != "" {
			result = append(result, Cue{cue.Start, cue.End, text})
			tail = append(tail, words[overlap:]...)
			if len(tail) > 60 {
				tail = tail[len(tail)-60:]
			}
		}
	}
	if changed >= 2 && changed*2 >= len(sorted) {
		return result
	}
	for i := range sorted {
		sorted[i].Text = strings.ReplaceAll(sorted[i].Text, "\n", " ")
	}
	return sorted
}

func isWordRune(r rune) bool {
	if r < 0x80 {
		return isWordByte(byte(r))
	}
	return unicode.IsLetter(r) || unicode.IsMark(r) || unicode.IsNumber(r)
}

// nextWord finds the next word from `from`, as /[\p{L}\p{M}\p{N}_]+(?:'[\p{L}\p{M}\p{N}_]+)*/gu,
// or -1.
func nextWord(text string, from int) (int, int) {
	for i := from; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if !isWordRune(r) {
			i += size
			continue
		}
		end := skipRunes(text, i, isWordRune)
		for end < len(text) && text[end] == '\'' {
			next := skipRunes(text, end+1, isWordRune)
			if next == end+1 {
				break
			}
			end = next
		}
		return i, end
	}
	return -1, -1
}

// cjk reports whether a character is Han, kana or CJK punctuation, written without
// spaces; Hangul is not. Every character beyond the Basic Multilingual Plane passes too,
// emoji included, as the test was first made on UTF-16 code units.
func cjk(r rune) bool {
	return r >= 0x2e80 && !(r >= 0x3130 && r <= 0x318f) && !(r >= 0xac00 && r <= 0xd7af)
}

// joinParts joins cues, with the markers that cue boundaries double collapsed: lyric lines
// "♪ a ♪" "♪ b ♪" read "♪ a ♪ b ♪", and "[Music] [Music]" reads once. Parentheses are left
// alone, since lyrics put sung words in them.
func joinParts(parts []string) string {
	var b strings.Builder
	for _, part := range parts {
		if b.Len() > 0 {
			last, _ := utf8.DecodeLastRuneInString(b.String())
			first, _ := utf8.DecodeRuneInString(part)
			if !(cjk(last) && part != "" && cjk(first)) {
				b.WriteByte(' ')
			}
		}
		b.WriteString(part)
	}
	return collapseBrackets(collapseNotes(b.String()))
}

const note = "♪"

// collapseNotes replaces each run of notes separated by white space with one, as /♪(?:\s+♪)+/g.
func collapseNotes(s string) string {
	var b strings.Builder
	last := 0
	for i := 0; ; {
		j := strings.Index(s[i:], note)
		if j < 0 {
			break
		}
		j += i
		end := j + len(note)
		for {
			k := skipRunes(s, end, rustle.IsSpace)
			if k == end || !strings.HasPrefix(s[k:], note) {
				break
			}
			end = k + len(note)
		}
		if end > j+len(note) {
			b.WriteString(s[last:j])
			b.WriteString(note)
			last = end
		}
		i = end
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// collapseBrackets writes a bracketed marker repeated after white space once, as
// /(\[[^\]\n]{1,40}\])(?:\s+\1)+/g, which needs a backreference. The 40 counts UTF-16 units.
func collapseBrackets(s string) string {
	var b strings.Builder
	last := 0
	for i := 0; ; {
		j := strings.IndexByte(s[i:], '[')
		if j < 0 {
			break
		}
		j += i
		i = j + 1
		k, units := j+1, 0
		for k < len(s) && s[k] != ']' && s[k] != '\n' && units <= 40 {
			r, size := utf8.DecodeRuneInString(s[k:])
			units += utf16Len(r)
			k += size
		}
		if k >= len(s) || s[k] != ']' || units < 1 || units > 40 {
			continue
		}
		marker := s[j : k+1]
		end := k + 1
		for {
			m := skipRunes(s, end, rustle.IsSpace)
			if m == end || !strings.HasPrefix(s[m:], marker) {
				break
			}
			end = m + len(marker)
		}
		if end > k+1 {
			b.WriteString(s[last:j])
			b.WriteString(marker)
			last, i = end, end
		}
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

func utf16Len(r rune) int {
	if r >= 0x10000 {
		return 2
	}
	return 1
}

func isStop(r rune) bool     { return r == '.' || r == '!' || r == '?' || r == '…' }
func isFullStop(r rune) bool { return r == '。' || r == '！' || r == '？' }
func isCloser(r rune) bool   { return strings.ContainsRune(closers, r) }

// sentenceBreaks calls fn with the end of each sentence break, as
// /[.!?…]+[closers]*\s+|[。！？]+[closers]*/g finds them: marks, closing quotes and
// brackets, and the white space after them, which full-width marks do without.
func sentenceBreaks(text string, fn func(end int)) {
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		switch {
		case isStop(r):
			j := skipRunes(text, i, isStop)
			k := skipRunes(text, j, isCloser)
			if l := skipRunes(text, k, rustle.IsSpace); l > k {
				fn(l)
				i = l
			} else {
				// Starting later in the same run of marks fails the same way.
				i = j
			}
			continue
		case isFullStop(r):
			k := skipRunes(text, skipRunes(text, i, isFullStop), isCloser)
			fn(k)
			i = k
			continue
		}
		i += size
	}
}

// endsSentence reports whether text ends in a sentence's mark and any closers.
func endsSentence(text string) bool {
	r, _ := utf8.DecodeLastRuneInString(strings.TrimRight(text, closers))
	return isStop(r) || isFullStop(r)
}

// Paragraphs merges cues into paragraphs of about shortest to longest seconds that end at
// sentence ends, and at every break: a paragraph never spans one.
//
// Sentences often end inside a cue, so cues are split there and the split's time is
// interpolated by character position.
func Paragraphs(cues []Cue, shortest, longest float64, breaks ...float64) []Paragraph {
	var result []Paragraph
	var parts []string
	first, last := 0.0, 0.0
	add := func(at, until float64, piece string) {
		long := at-first >= longest
		paused := at-last >= pause && at-first >= shortest
		if len(parts) > 0 && (long || paused || crosses(breaks, first, at)) {
			result = append(result, Paragraph{first, last, joinParts(parts)})
			parts = parts[:0]
		}
		if len(parts) == 0 {
			first, last = at, at
		}
		parts = append(parts, piece)
		last = math.Max(last, until)
		if endsSentence(piece) && until-first >= shortest {
			result = append(result, Paragraph{first, last, joinParts(parts)})
			parts = parts[:0]
		}
	}
	for index, cue := range cues {
		following := cue.End
		if index+1 < len(cues) {
			following = cues[index+1].Start
		}
		stop := cue.End
		if following > cue.Start {
			stop = math.Min(cue.End, following)
		}
		span := math.Max(0, stop-cue.Start)
		size := float64(rustle.UTF16Len(cue.Text))
		// Cuts are byte offsets, and their UTF-16 positions place them in time.
		cut, cutUnits := 0, 0
		piece := func(end int) {
			units := cutUnits + rustle.UTF16Len(cue.Text[cut:end])
			if text := rustle.TrimSpace(cue.Text[cut:end]); text != "" {
				add(rustle.MS(cue.Start+span*float64(cutUnits)/size), rustle.MS(cue.Start+span*float64(units)/size), text)
			}
			cut, cutUnits = end, units
		}
		sentenceBreaks(cue.Text, piece)
		piece(len(cue.Text))
	}
	if len(parts) > 0 {
		result = append(result, Paragraph{first, last, joinParts(parts)})
	}
	// A paragraph lasts until the next begins, so a page that starts where the previous
	// one stopped never repeats a paragraph.
	for i := 0; i+1 < len(result); i++ {
		result[i].End = result[i+1].Start
	}
	return result
}

// crosses reports whether a break falls after from, up to and including to.
func crosses(breaks []float64, from, to float64) bool {
	for _, b := range breaks {
		if from < b && b <= to {
			return true
		}
	}
	return false
}

// Primary is the primary language subtag: "en-US" and "en" name the same spoken language.
func Primary(code string) string {
	code, _, _ = strings.Cut(code, "-")
	code, _, _ = strings.Cut(code, "_")
	return strings.ToLower(code)
}

// WantedLanguage is the caller's language, or "" for the spoken one; YouTube's "a.en" means "en".
func WantedLanguage(language string) string {
	value := rustle.TrimSpace(language)
	switch strings.ToLower(value) {
	case "", "auto", "original":
		return ""
	}
	if len(value) >= 2 && (value[0] == 'a' || value[0] == 'A') && value[1] == '.' {
		value = value[2:]
	}
	return strings.Trim(value, ".")
}

func isOriginal(track rustle.Caption, original string) bool {
	return track.Original || original != "" && Primary(track.Language) == Primary(original)
}

func available(tracks []rustle.Caption) string {
	var languages []string
	for _, t := range tracks {
		if !slices.Contains(languages, t.Language) {
			languages = append(languages, t.Language)
		}
	}
	return strings.Join(languages, ", ")
}

// SelectTrack picks a caption track: the requested language, else the original spoken
// language. language and original are "" when not known.
//
// Auto order: human captions in the original language, auto captions in the original
// language, human captions (English first), English auto captions, the first track.
// Auto-dubbed languages' auto captions therefore never beat the original.
//
// Tracks without a URL are skipped; with none left, the track is the zero Caption.
func SelectTrack(all []rustle.Caption, language, original string) (rustle.Caption, []string) {
	var tracks []rustle.Caption
	for _, t := range all {
		if t.URL != "" {
			tracks = append(tracks, t)
		}
	}
	if len(tracks) == 0 {
		return rustle.Caption{}, nil
	}
	if language != "" {
		wanted := strings.ToLower(language)
		tests := []func(rustle.Caption) bool{
			func(t rustle.Caption) bool { return strings.ToLower(t.Language) == wanted },
			func(t rustle.Caption) bool { return Primary(t.Language) == Primary(wanted) },
		}
		if rustle.UTF16Len(wanted) > 3 {
			tests = append(tests, func(t rustle.Caption) bool { return strings.HasPrefix(strings.ToLower(t.Name), wanted) })
		}
		// Human before ASR, then original before translation, then list order.
		order := func(t rustle.Caption) int {
			n := 0
			if t.Kind == "asr" {
				n += 2
			}
			if !isOriginal(t, original) {
				n++
			}
			return n
		}
		for _, test := range tests {
			best := -1
			for i, t := range tracks {
				if test(t) && (best < 0 || order(t) < order(tracks[best])) {
					best = i
				}
			}
			if best >= 0 {
				return tracks[best], nil
			}
		}
	}
	rank := func(t rustle.Caption) int {
		manual := t.Kind != "asr"
		english := Primary(t.Language) == "en"
		switch {
		case isOriginal(t, original) && manual:
			return 0
		case isOriginal(t, original):
			return 1
		case manual && english:
			return 2
		case manual:
			return 3
		case english:
			return 4
		}
		return 5
	}
	chosen := tracks[0]
	for _, t := range tracks {
		if rank(t) < rank(chosen) {
			chosen = t
		}
	}
	var notes []string
	if language != "" {
		notes = append(notes, fmt.Sprintf("No %s captions; returned %s. Available: %s.", language, chosen.Language, available(tracks)))
	} else if original != "" && !isOriginal(chosen, original) {
		notes = append(notes, fmt.Sprintf("No captions in the original language (%s); returned %s.", original, chosen.Language))
	}
	var human []rustle.Caption
	for _, t := range tracks {
		if t.Kind == "manual" {
			human = append(human, t)
		}
	}
	if chosen.Kind == "asr" && len(human) > 0 {
		notes = append(notes, "Human captions exist in: "+available(human)+". Pass language to read one.")
	}
	return chosen, notes
}

// extname is Node's path.extname of a file name: from its last dot, but "" for a name
// that starts with its only dot (".bashrc") and for "..".
func extname(name string) string {
	dot := strings.LastIndexByte(name, '.')
	if dot <= 0 || name == ".." {
		return ""
	}
	return name[dot:]
}

// FindSidecar finds a .srt or .vtt next to a local file: "talk.srt", or "talk.en.vtt" as
// yt-dlp names it. path is "" when there is none; tag is the language part of its name.
func FindSidecar(media, language string) (path, tag string, notes []string) {
	name := filepath.Base(media)
	stem := name[:len(name)-len(extname(name))]
	dir := filepath.Dir(media)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", "", nil
	}
	type sidecar struct{ tag, path, suffix string }
	var found []sidecar
	prefix := stem + "."
	for _, entry := range entries {
		n := entry.Name()
		suffix := strings.ToLower(extname(n))
		// Like Node's Dirent.isFile, a symlink is not a file.
		if (suffix == ".srt" || suffix == ".vtt") && strings.HasPrefix(n, prefix) && entry.Type().IsRegular() {
			tag := ""
			if from, to := len(stem)+1, len(n)-len(suffix); from < to {
				tag = n[from:to]
			}
			found = append(found, sidecar{tag, filepath.Join(dir, n), suffix})
		}
	}
	if len(found) == 0 {
		return "", "", nil
	}
	// Untagged first, then English, then by tag; SRT before rolling WebVTT.
	flag := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	slices.SortStableFunc(found, func(a, b sidecar) int {
		return cmp.Or(
			cmp.Compare(flag(a.tag != ""), flag(b.tag != "")),
			cmp.Compare(flag(Primary(a.tag) != "en"), flag(Primary(b.tag) != "en")),
			strings.Compare(a.tag, b.tag),
			cmp.Compare(flag(a.suffix != ".srt"), flag(b.suffix != ".srt")),
		)
	})
	if language != "" {
		for _, same := range []func(string) bool{
			func(tag string) bool { return strings.ToLower(tag) == strings.ToLower(language) },
			func(tag string) bool { return Primary(tag) == Primary(language) },
		} {
			for _, f := range found {
				if f.tag != "" && same(f.tag) {
					return f.path, f.tag, nil
				}
			}
		}
	}
	f := found[0]
	if language != "" {
		var tagged []string
		for _, s := range found {
			if s.tag != "" && !slices.Contains(tagged, s.tag) {
				tagged = append(tagged, s.tag)
			}
		}
		list := strings.Join(tagged, ", ")
		if list == "" {
			list = "none"
		}
		notes = append(notes, fmt.Sprintf("No %s sidecar; returned %s. Tagged sidecars: %s.", language, filepath.Base(f.path), list))
	}
	return f.path, f.tag, notes
}

// Parse reads a caption file; label names it in the error, e.g. "YouTube captions".
func Parse(text, label string) ([]Cue, error) {
	cues, err := ParseCaptions(text)
	if err != nil {
		return nil, rustle.Fail(label+" are not valid WebVTT, SRT or timed-text XML.", "CAPTIONS_UNAVAILABLE",
			"Pass transcribe=true to transcribe the speech locally instead.")
	}
	return cues, nil
}

// weight is what a UTF-16 code unit costs against the page budget, which is set in English
// characters (about four to a token). Japanese runs near one character a token and
// Cyrillic near two, so counting each as one character would let a page reach three
// times the harness's limit.
func weight(c rune) int {
	switch {
	case c < 0x370 || c >= 0x2000 && c < 0x2e80:
		return 1
	case c < 0x900 || c >= 0x1e00 && c < 0x1f00:
		return 2
	}
	return 4
}

// runeCost is weight per character: beyond the Basic Multilingual Plane, both surrogate halves.
func runeCost(r rune) int {
	if r >= 0x10000 {
		return 8
	}
	return weight(r)
}

// Cost is what text costs against the page budget.
func Cost(text string) int {
	total := 0
	for i := 0; i < len(text); {
		if text[i] < 0x80 {
			total++
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(text[i:])
		total += runeCost(r)
		i += size
	}
	return total
}

// fit is the longest start of text that costs at most budget, ending between characters.
func fit(text string, budget int) string {
	used := 0
	for i, r := range text {
		if used += runeCost(r); used > budget {
			return text[:i]
		}
	}
	return text
}

func line(p Paragraph) string { return rustle.Clock(p.Start) + " " + p.Text }

// Match is one line of a search: the matching units and their neighbours, led by the
// clock of Start. At is when the first match in it starts.
type Match struct {
	Start, At float64
	Text      string
}

// NoMatch says that nothing said matches query, naming it as JSON.
func NoMatch(query string) string {
	return "Nothing said mentions " + string(rustle.AppendString(nil, rustle.TrimSpace(query))) + "."
}

// Matches finds the units that mention any `a|b` alternative (case-insensitive), each with
// the unit before and after it; neighbouring hits share one line, led by its first unit's
// time. count is the number of units that matched.
func Matches(units []Paragraph, query string, budget int) (found []Match, count int, notes []string) {
	var terms []string
	for _, t := range strings.Split(query, "|") {
		if t = strings.ToLower(rustle.CollapseSpace(t)); t != "" {
			terms = append(terms, t)
		}
	}
	var hits []int
	for i, u := range units {
		text := strings.ToLower(u.Text)
		for _, t := range terms {
			if strings.Contains(text, t) {
				hits = append(hits, i)
				break
			}
		}
	}
	if len(hits) == 0 {
		return nil, 0, nil
	}
	if len(hits) > MaxMatches {
		notes = append(notes, fmt.Sprintf("showing the first %d of %d matches; narrow the query or pass start_seconds", MaxMatches, len(hits)))
	}
	// Each group is the units [from, to) and its first hit.
	var groups [][3]int
	for _, i := range hits[:min(len(hits), MaxMatches)] {
		from, to := max(0, i-1), min(len(units), i+2)
		if n := len(groups); n > 0 && from <= groups[n-1][1] {
			groups[n-1][1] = max(groups[n-1][1], to)
		} else {
			groups = append(groups, [3]int{from, to, i})
		}
	}
	used := 0
	for _, g := range groups {
		parts := make([]string, 0, g[1]-g[0])
		for _, u := range units[g[0]:g[1]] {
			parts = append(parts, u.Text)
		}
		text := rustle.Clock(units[g[0]].Start) + " " + joinParts(parts)
		size := Cost(text) + 1
		if len(found) > 0 && used+size > budget {
			notes = append(notes, "stopped at the size limit; narrow the query")
			break
		}
		found = append(found, Match{Start: units[g[0]].Start, At: units[g[2]].Start, Text: text})
		used += size
	}
	return found, len(hits), notes
}

// Paged is one page of paragraphs.
type Paged struct {
	Lines []string
	// Starts is when each line's paragraph starts.
	Starts []float64
	// Next is where the following page starts, when HasNext.
	Next    float64
	HasNext bool
	// Cut is the start of a paragraph too long for a page on its own, which was cut to
	// fit, when HasCut.
	Cut    float64
	HasCut bool
}

// Page is as many whole paragraphs as fit the budget. A paragraph too long for a page on
// its own is cut, since a page can only continue at a paragraph's start.
func Page(paras []Paragraph, budget int) Paged {
	var page Paged
	used := 0
	for index := 0; index < len(paras); index++ {
		text := line(paras[index])
		size := Cost(text) + 1
		if used+size <= budget {
			page.Lines = append(page.Lines, text)
			page.Starts = append(page.Starts, paras[index].Start)
			used += size
			continue
		}
		if len(page.Lines) == 0 {
			page.Lines = append(page.Lines, fit(text, budget))
			page.Starts = append(page.Starts, paras[index].Start)
			page.Cut, page.HasCut = paras[index].Start, true
			index++
		}
		if index < len(paras) {
			page.Next, page.HasNext = paras[index].Start, true
		}
		return page
	}
	return page
}

// Silence is the text of a read that found no speech between start and stop.
func Silence(start, stop float64) string {
	if stop > start {
		return "No speech in the transcript between " + rustle.Clock(start) + " and " + rustle.Clock(stop) + "."
	}
	return "No speech in the transcript after " + rustle.Clock(start) + "."
}
