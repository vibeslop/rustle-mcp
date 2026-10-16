package transcript

import (
	"cmp"
	"math"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

// The digest of a transcript: its key sentences, verbatim, in order, about a third of the
// words. Speech is redundant (restarts, asides, sign-posting, repetition), so a third of
// the sentences, chosen for the words the speaker keeps returning to, carries most of what
// is said, and a digest of a whole hour fits one call. Every stretch of the timeline keeps
// at least one sentence, so nothing is invisible, and "…" marks where speech was skipped,
// so a model knows to read the span in full before quoting across it.
//
// Scoring is SumBasic (Nenkova and Vanderwende, 2005): a sentence is worth the average
// frequency of its content words, and once chosen its words count for less, so the next
// pick says something new.

// DigestRatio is the share of a transcript's characters a digest keeps.
const DigestRatio = 0.26

var stopWords = func() map[string]bool {
	words := map[string]bool{}
	for _, w := range strings.Fields(`a about above after again against all also am an and any are as at be because been before being below between both but by
can could did do does doing down during each few for from further had has have having he her here hers herself him himself his how i if in into is it
its itself just let me more most my myself no nor not now of off on once only or other our ours ourselves out over own same she should so some such
than that the their theirs them themselves then there these they this those through to too under until up very was we were what when where which while
who whom why will with would you your yours yourself yourselves going get got gonna like yeah okay ok oh well really right actually basically
kind sort thing things lot know mean think see look want say said way one also even still something really quite pretty maybe`) {
		words[w] = true
	}
	return words
}()

// TidySpeech drops fillers that never carry meaning ("um", "uh") and says a word
// stuttered twice once.
func TidySpeech(text string) string {
	text = collapseSpaceRuns(dropStutters(dropFillers(text)))
	text = strings.TrimLeftFunc(text, func(r rune) bool { return r == ',' || rustle.IsSpace(r) })
	return rustle.TrimSpace(text)
}

// isPause is a character that may stand around a filler: [\s,.;:!?].
func isPause(r rune) bool {
	return r == ',' || r == '.' || r == ';' || r == ':' || r == '!' || r == '?' || rustle.IsSpace(r)
}

// dropFillers is text.replace(/(^|[\s,.;:!?])(?:u+[hm]+|e+r+m+|h+m+|m+h+m+|a+h+)(?=[\s,.;:!?]|$)[,.]?/giu, '$1'),
// which needs lookahead. The character before a filler belongs to the match, so one that
// a previous match took, its trailing comma included, cannot lead the next: "um,uh"
// leaves "uh".
func dropFillers(text string) string {
	var b strings.Builder
	last := 0
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		lead, end := 0, -1
		if i == 0 {
			end = fillerEnd(text, 0)
		}
		if end < 0 && isPause(r) {
			lead, end = size, fillerEnd(text, i+size)
		}
		if end < 0 {
			i += size
			continue
		}
		b.WriteString(text[last : i+lead])
		last, i = end, end
	}
	if last == 0 {
		return text
	}
	b.WriteString(text[last:])
	return b.String()
}

// fillerEnd is where a filler word starting at i ends, with a comma or full stop after
// it, or -1 when the word there is not a filler. Fillers are made of letters and must be
// followed by a pause or the end, so the filler is the whole run of filler letters.
func fillerEnd(text string, i int) int {
	j := i
	for j < len(text) && strings.IndexByte("uhmeraUHMERA", text[j]) >= 0 {
		j++
	}
	if j == i || !isFiller(text[i:j]) {
		return -1
	}
	if j < len(text) {
		if r, _ := utf8.DecodeRuneInString(text[j:]); !isPause(r) {
			return -1
		}
		if text[j] == ',' || text[j] == '.' {
			j++
		}
	}
	return j
}

// isFiller reports whether an ASCII word is u+[hm]+, e+r+m+, h+m+, m+h+m+ or a+h+, in any case.
func isFiller(word string) bool {
	// The word's letters with repeats collapsed: "uuhmm" is "uhm".
	runs := make([]byte, 0, len(word))
	for i := 0; i < len(word); i++ {
		if c := word[i] | 0x20; len(runs) == 0 || runs[len(runs)-1] != c {
			runs = append(runs, c)
		}
	}
	switch s := string(runs); s {
	case "erm", "hm", "mhm", "ah":
		return true
	default:
		return len(s) > 1 && s[0] == 'u' && strings.Trim(s[1:], "hm") == ""
	}
}

// isBoundaryWord is a word character where a stutter may start and end: ASCII letters,
// digits and _, plus ſ and the Kelvin sign, which case-fold into them. Other letters are
// not, so "café cafés" reads as a stutter of "café" followed by "s"; the digest was tuned
// with that rule, and it costs a letter in a rare repeat.
func isBoundaryWord(r rune) bool {
	return r < 0x80 && isWordByte(byte(r)) || r == 0x17f || r == 0x212a
}

// isStutterLetter is [\p{L}'] under /iu, which also takes U+0345, a combining mark that
// case-folds to a letter.
func isStutterLetter(r rune) bool { return r == '\'' || unicode.IsLetter(r) || r == 0x345 }

func isStutterGap(r rune) bool { return r == ',' || rustle.IsSpace(r) }

// dropStutters is text.replace(/\b([\p{L}']+)(?:[\s,]+\1\b)+/giu, '$1'), which needs a
// backreference. The word cannot end before its run of letters does, since only a gap may
// follow it, so each start tries the whole run from there.
func dropStutters(text string) string {
	var b strings.Builder
	last := 0
	runEnd := 0 // the end of the run of letters at i, while i is inside it
	before := false
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		word := isBoundaryWord(r)
		if word != before && isStutterLetter(r) {
			if i >= runEnd {
				runEnd = skipRunes(text, i, isStutterLetter)
			}
			if end := repeated(text, i, runEnd); end > 0 {
				b.WriteString(text[last:runEnd])
				last, i = end, end
				r, _ = utf8.DecodeLastRuneInString(text[:end])
				before = isBoundaryWord(r)
				continue
			}
		}
		before = word
		i += size
	}
	if last == 0 {
		return text
	}
	b.WriteString(text[last:])
	return b.String()
}

// repeated is the end of the copies of text[start:end] that follow it, each after commas or
// white space and ending at a word boundary, or -1 when none does.
func repeated(text string, start, end int) int {
	word := text[start:end]
	at := end
	for {
		gap := skipRunes(text, at, isStutterGap)
		if gap == at {
			break
		}
		copyEnd, ok := foldedPrefix(text, gap, word)
		if !ok {
			break
		}
		last, _ := utf8.DecodeLastRuneInString(text[:copyEnd])
		next, _ := utf8.DecodeRuneInString(text[copyEnd:])
		if isBoundaryWord(last) == (copyEnd < len(text) && isBoundaryWord(next)) {
			break
		}
		at = copyEnd
	}
	if at == end {
		return -1
	}
	return at
}

// foldedPrefix reports whether text at i starts with prefix under simple case folding, as
// a backreference compares under /iu, and where that start ends.
func foldedPrefix(text string, i int, prefix string) (int, bool) {
	for _, want := range prefix {
		if i >= len(text) {
			return 0, false
		}
		got, size := utf8.DecodeRuneInString(text[i:])
		if !sameFold(got, want) {
			return 0, false
		}
		i += size
	}
	return i, true
}

func sameFold(a, b rune) bool {
	if a == b {
		return true
	}
	if a < 0x80 && b < 0x80 {
		return a|0x20 == b|0x20 && 'a' <= a|0x20 && a|0x20 <= 'z'
	}
	for f := unicode.SimpleFold(a); f != a; f = unicode.SimpleFold(f) {
		if f == b {
			return true
		}
	}
	return false
}

// collapseSpaceRuns writes each run of two or more white space characters as one space,
// as /\s{2,}/g; a lone line break stays.
func collapseSpaceRuns(text string) string {
	var b strings.Builder
	last := 0
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if !rustle.IsSpace(r) {
			i += size
			continue
		}
		end := skipRunes(text, i, rustle.IsSpace)
		if end-i > size {
			b.WriteString(text[last:i])
			b.WriteByte(' ')
			last = end
		}
		i = end
	}
	if last == 0 {
		return text
	}
	b.WriteString(text[last:])
	return b.String()
}

// isCJK is a character of the scripts written without spaces, as /[⺀-鿿豈-﫿぀-ヿ]/u.
func isCJK(r rune) bool {
	return r >= 0x2e80 && r <= 0x9fff || r >= 0xf900 && r <= 0xfaff || r >= 0x3040 && r <= 0x30ff
}

func isLetterOrNumber(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) }

func isTermRune(r rune) bool { return isLetterOrNumber(r) || r == '\'' || r == '’' || r == '-' }

// terms are a sentence's content words, or character pairs for scripts written without spaces.
func terms(text string) []string {
	lower := strings.ToLower(text)
	if strings.IndexFunc(lower, isCJK) >= 0 {
		var chars []rune
		for _, r := range lower {
			if isLetterOrNumber(r) {
				chars = append(chars, r)
			}
		}
		var pairs []string
		for i := 1; i < len(chars); i++ {
			pairs = append(pairs, string(chars[i-1:i+1]))
		}
		return pairs
	}
	var words []string
	for i := 0; i < len(lower); {
		r, size := utf8.DecodeRuneInString(lower[i:])
		if !isLetterOrNumber(r) {
			i += size
			continue
		}
		end := skipRunes(lower, i+size, isTermRune)
		if w := lower[i:end]; rustle.UTF16Len(w) > 2 && !stopWords[w] {
			words = append(words, w)
		}
		i = end
	}
	return words
}

// vocabulary is each sentence's words as small integers, so counts live in slices, not maps.
type vocabulary struct {
	ids  [][]int32
	size int
}

func intern(words [][]string) vocabulary {
	index := map[string]int32{}
	ids := make([][]int32, len(words))
	for i, list := range words {
		row := make([]int32, len(list))
		for j, w := range list {
			id, ok := index[w]
			if !ok {
				id = int32(len(index))
				index[w] = id
			}
			row[j] = id
		}
		ids[i] = row
	}
	return vocabulary{ids, len(index)}
}

// Boundaries is where topics change, by lexical cohesion (TextTiling, Hearst 1997): the
// units where the words of the k sentences before and after share least, deepest dips
// first, at least k apart. Their first sentences mark when a new subject starts, which a
// digest chosen only for content tends to skip, since an opener such as "Now let me show
// you how" is short.
func Boundaries(words [][]string, count, k int) []int {
	if count <= 0 || len(words) < 2*k+2 {
		return nil
	}
	return boundaries(intern(words), count, k)
}

func boundaries(v vocabulary, count, k int) []int {
	n := len(v.ids)
	if count <= 0 || n < 2*k+2 {
		return nil
	}
	// The k sentences before i and the k from i, as word counts slid along the transcript,
	// with their dot product and squared norms updated word by word.
	before, after := make([]int32, v.size), make([]int32, v.size)
	dot, normBefore, normAfter := 0, 0, 0
	add := func(side, other []int32, norm *int, id int32, by int) {
		if by < 0 {
			side[id]--
		}
		*norm += by * (2*int(side[id]) + 1)
		dot += by * int(other[id])
		if by > 0 {
			side[id]++
		}
	}
	for j := 0; j < k; j++ {
		for _, id := range v.ids[j] {
			add(before, after, &normBefore, id, 1)
		}
	}
	for j := k; j < 2*k; j++ {
		for _, id := range v.ids[j] {
			add(after, before, &normAfter, id, 1)
		}
	}
	similarity := make([]float64, n+1)
	for i := range similarity {
		similarity[i] = 1
	}
	for i := k; i <= n-k; i++ {
		if i > k {
			for _, id := range v.ids[i-1-k] {
				add(before, after, &normBefore, id, -1)
			}
			for _, id := range v.ids[i-1] {
				add(after, before, &normAfter, id, -1)
				add(before, after, &normBefore, id, 1)
			}
			for _, id := range v.ids[i+k-1] {
				add(after, before, &normAfter, id, 1)
			}
		}
		similarity[i] = 0
		if normBefore != 0 && normAfter != 0 {
			similarity[i] = float64(dot) / math.Sqrt(float64(normBefore*normAfter))
		}
	}
	type dip struct {
		depth float64
		at    int
	}
	var depths []dip
	for i := k; i <= n-k; i++ {
		s := similarity[i]
		if s > similarity[i-1] || s > similarity[i+1] {
			continue
		}
		left, right := s, s
		for j := i - 1; j >= k && similarity[j] >= left; j-- {
			left = similarity[j]
		}
		for j := i + 1; j <= n-k && similarity[j] >= right; j++ {
			right = similarity[j]
		}
		depths = append(depths, dip{left + right - 2*s, i})
	}
	slices.SortStableFunc(depths, func(x, y dip) int { return cmp.Compare(y.depth, x.depth) })
	var chosen []int
	for _, d := range depths {
		if len(chosen) >= count {
			break
		}
		if !slices.ContainsFunc(chosen, func(j int) bool { return j-d.at < k && d.at-j < k }) {
			chosen = append(chosen, d.at)
		}
	}
	slices.Sort(chosen)
	return chosen
}

// opener matches sentences that open a new step or subject in English talks ("Now…",
// "Next…", "Let's…"). It is matched against asciiLower(text) rather than with (?i), which
// would also fold ſ to s and the Kelvin sign to k.
var opener = regexp.MustCompile(`^(?:(?:and |so |okay,? |alright,? )?(?:now|next|first|second|third|finally|another|let's|let me|today)\b|the (?:first|next|last|final) (?:thing|step|part|one)\b)`)

// Choose is the indices of the units to keep, in order. must are kept regardless (the
// opening, each chapter's first sentence); every window of `window` seconds keeps its best
// unit; the rest of the budget, in UTF-16 characters, goes to the best remaining units by
// SumBasic. words are each unit's terms; nil computes them.
func Choose(units []Cue, budget, window float64, must map[int]bool, words [][]string) []int {
	if words == nil {
		words = make([][]string, len(units))
		for i, u := range units {
			words[i] = terms(u.Text)
		}
	}
	return choose(units, budget, window, must, intern(words))
}

func choose(units []Cue, budget, window float64, must map[int]bool, v vocabulary) []int {
	// Words in more than a quarter of the sentences are the language's own glue, whatever
	// the language: they say nothing about this video.
	spread := make([]int32, v.size)
	seen := make([]int32, v.size)
	for id := range seen {
		seen[id] = -1
	}
	for i, list := range v.ids {
		for _, id := range list {
			if seen[id] != int32(i) {
				seen[id] = int32(i)
				spread[id]++
			}
		}
	}
	common := math.Inf(1)
	if len(units) >= 12 {
		common = float64(len(units)) / 4
	}
	ids := make([][]int32, len(v.ids))
	for i, list := range v.ids {
		for _, id := range list {
			if float64(spread[id]) <= common {
				ids[i] = append(ids[i], id)
			}
		}
	}
	p := make([]float64, v.size)
	total := 0
	for _, list := range ids {
		for _, id := range list {
			p[id]++
		}
		total += len(list)
	}
	for id := range p {
		p[id] /= float64(max(1, total))
	}
	length := make([]int, len(units))
	opens := make([]bool, len(units))
	for i, u := range units {
		length[i] = rustle.UTF16Len(u.Text)
		opens[i] = opener.MatchString(asciiLower(u.Text))
	}
	score := func(i int) float64 {
		list := ids[i]
		if len(list) == 0 {
			return 0
		}
		sum := 0.0
		for _, id := range list {
			sum += p[id]
		}
		n := float64(len(list))
		boost := 1.0
		if opens[i] {
			boost = 1.3
		}
		// Very short sentences ("Right.", "Let's go.") rarely carry content.
		return sum / n * math.Min(1, n/4) * boost
	}
	chosen := make([]bool, len(units))
	used := 0
	take := func(i int) {
		if chosen[i] {
			return
		}
		chosen[i] = true
		used += length[i] + 1
		for _, id := range ids[i] {
			p[id] *= p[id]
		}
	}
	var first []int
	for i, ok := range must {
		if ok {
			first = append(first, i)
		}
	}
	slices.Sort(first)
	for _, i := range first {
		take(i)
	}
	// Coverage: the best sentence of each window, so no stretch of the video goes unseen.
	var windows [][]int
	slot := map[float64]int{}
	for i, u := range units {
		key := math.Floor(u.Start / window)
		if w, ok := slot[key]; ok {
			windows[w] = append(windows[w], i)
		} else {
			slot[key] = len(windows)
			windows = append(windows, []int{i})
		}
	}
	for _, members := range windows {
		if slices.ContainsFunc(members, func(i int) bool { return chosen[i] }) {
			continue
		}
		best := members[0]
		for _, i := range members {
			if score(i) > score(best) {
				best = i
			}
		}
		take(best)
	}
	// Then the best of the rest until the budget is spent. A pick only ever lowers other
	// sentences' scores (its words' frequencies are squared), so a score computed earlier is
	// an upper bound: re-score the top of the heap, and it is the best once it still leads.
	var heap maxHeap
	for i := range units {
		if !chosen[i] {
			heap.push(score(i), i)
		}
	}
	for len(heap.items) > 0 && float64(used) < budget {
		i := heap.pop()
		fresh := score(i)
		if len(heap.items) > 0 && fresh < heap.scores[0] {
			heap.push(fresh, i)
			continue
		}
		if float64(used+length[i]+1) > budget*1.05 {
			continue
		}
		take(i)
	}
	var kept []int
	for i, ok := range chosen {
		if ok {
			kept = append(kept, i)
		}
	}
	return kept
}

// maxHeap is a binary max-heap of (score, item) pairs. How it breaks ties decides which
// of two equally good units is kept, so the order of its swaps is part of the digest.
type maxHeap struct {
	scores []float64
	items  []int
}

func (h *maxHeap) push(score float64, item int) {
	at := len(h.items)
	h.scores = append(h.scores, score)
	h.items = append(h.items, item)
	for at > 0 {
		parent := (at - 1) / 2
		if h.scores[parent] >= score {
			break
		}
		h.swap(at, parent)
		at = parent
	}
}

func (h *maxHeap) pop() int {
	item := h.items[0]
	last := len(h.items) - 1
	h.swap(0, last)
	h.scores, h.items = h.scores[:last], h.items[:last]
	for at := 0; ; {
		left := 2*at + 1
		right := left + 1
		largest := at
		if left < last && h.scores[left] > h.scores[largest] {
			largest = left
		}
		if right < last && h.scores[right] > h.scores[largest] {
			largest = right
		}
		if largest == at {
			break
		}
		h.swap(at, largest)
		at = largest
	}
	return item
}

func (h *maxHeap) swap(a, b int) {
	h.scores[a], h.scores[b] = h.scores[b], h.scores[a]
	h.items[a], h.items[b] = h.items[b], h.items[a]
}

// Digest is the digest's lines: kept sentences grouped into lines of about span seconds
// (0 picks a span from the duration), each starting at its first sentence. "…" stands
// where speech was skipped: between two sentences of a line (followed by the next
// sentence's time), at the start of a line after a gap, and at the end.
func Digest(units []Cue, chapters []rustle.Chapter, span float64) []Cue {
	var tidy []Cue
	for _, u := range units {
		if text := TidySpeech(u.Text); text != "" {
			tidy = append(tidy, Cue{Start: u.Start, Text: text})
		}
	}
	if len(tidy) == 0 {
		return nil
	}
	total := 0
	for _, u := range tidy {
		total += rustle.UTF16Len(u.Text) + 1
	}
	duration := tidy[len(tidy)-1].Start - tidy[0].Start
	if span <= 0 {
		span = math.Max(30, math.Min(120, duration/30))
	}
	// Coverage windows are wider than lines: a window whose only speech is a stray remark
	// shares its guaranteed sentence with a neighbour that has something to say.
	window := math.Max(45, duration/20)
	must := map[int]bool{0: true}
	if len(tidy) > 1 {
		must[1] = true
	}
	words := make([][]string, len(tidy))
	for i, u := range tidy {
		words[i] = terms(u.Text)
	}
	vocab := intern(words)
	// One topic opening per two and a half minutes, so "when does he start on X" has an anchor.
	for _, i := range boundaries(vocab, int(rustle.Round(duration/150)), 6) {
		must[i] = true
	}
	for _, c := range chapters {
		if first := slices.IndexFunc(tidy, func(u Cue) bool { return u.Start >= c.At-0.5 }); first >= 0 {
			must[first] = true
		}
	}
	var lines []Cue
	previous := -1
	for _, i := range choose(tidy, float64(total)*DigestRatio, window, must, vocab) {
		unit := tidy[i]
		gap := i > previous+1
		switch n := len(lines); {
		case n == 0 || unit.Start-lines[n-1].Start >= span:
			text := unit.Text
			if gap && i > 0 {
				text = "… " + text
			}
			lines = append(lines, Cue{Start: unit.Start, End: unit.Start, Text: text})
		case gap:
			// A run of sentences after a skip carries its own time, so a moment can be cited
			// from the digest to the second rather than to the line.
			lines[n-1].Text += " … " + rustle.Clock(unit.Start) + " " + unit.Text
		default:
			lines[n-1].Text += " " + unit.Text
		}
		previous = i
	}
	if previous < len(tidy)-1 {
		lines[len(lines)-1].Text += " …"
	}
	for i := 0; i+1 < len(lines); i++ {
		lines[i].End = lines[i+1].Start
	}
	lines[len(lines)-1].End = units[len(units)-1].Start
	return lines
}
