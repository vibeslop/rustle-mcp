package transcript

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

func clocks(paras []Paragraph) []string {
	out := make([]string, len(paras))
	for i, p := range paras {
		out[i] = rustle.Clock(p.Start)
	}
	return out
}

func texts(cues []Cue) []string {
	out := make([]string, len(cues))
	for i, c := range cues {
		out[i] = c.Text
	}
	return out
}

func cue(start float64, text string) Cue { return Cue{start, start + 1, text} }

func equal[T any](t *testing.T, got, want T) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %#v\nwant %#v", got, want)
	}
}

func TestParagraphsOf15To30Seconds(t *testing.T) {
	t.Run("youtube captions become timestamped paragraphs", func(t *testing.T) {
		paras := Paragraphs(cuesOf(talk), 15, 30)
		equal(t, clocks(paras), []string{"0:00", "0:18", "0:41", "0:58"})
		equal(t, paras[0].Text,
			"Would you believe that plain water, salt, and some flour could rise as tall as this? Sure, it's a simple recipe, but it's "+
				"good, right? We we eat lots of it. I love slow mornings. I bake these loaves each Saturday morning, but this is not the most "+
				"important trick in today's talk.")
		if !strings.HasSuffix(paras[2].Text, "See them all at example.com/bake.") {
			t.Errorf("paragraph 2 ends %q", paras[2].Text)
		}
		// "lessons. We go" splits one cue; the second sentence starts 41.21 s in.
		equal(t, paras[2].Start, 41.21)
	})

	t.Run("captions without punctuation break on time", func(t *testing.T) {
		paras := Paragraphs(cuesOf(unpunctuated), 15, 30)
		equal(t, clocks(paras), []string{"0:01", "0:23", "0:55"})
		if !strings.HasPrefix(paras[0].Text, "hello friends it's been a few weeks since we've had a quiet") {
			t.Errorf("paragraph 0 starts %q", paras[0].Text)
		}
	})

	t.Run("hour timestamps", func(t *testing.T) {
		equal(t, clocks(Paragraphs(cuesOf(hourMark), 15, 30)), []string{"59:46", "1:00:07"})
		equal(t, []string{rustle.Clock(59.9), rustle.Clock(3599.9), rustle.Clock(3600), rustle.Clock(36000)},
			[]string{"0:59", "59:59", "1:00:00", "10:00:00"})
	})

	t.Run("CJK joins without spaces, Hangul keeps them", func(t *testing.T) {
		equal(t, Paragraphs([]Cue{cue(0, "パン生地の"), cue(1, "こね方")}, MinParagraph, MaxParagraph)[0].Text, "パン生地のこね方")
		equal(t, Paragraphs([]Cue{cue(0, "오늘은 빵을"), cue(1, "굽습니다")}, MinParagraph, MaxParagraph)[0].Text, "오늘은 빵을 굽습니다")
	})
}

func TestCaptionFiles(t *testing.T) {
	t.Run("rolling WebVTT reads like plain captions", func(t *testing.T) {
		cues, err := ParseCaptions(talkVTT)
		if err != nil {
			t.Fatal(err)
		}
		rolling := strings.Join(texts(DropRepeats(cues)), " ")
		equal(t, rolling, strings.Join(texts(cuesOf(talk[:9])), " "))
		// The speaker's own "We we" is speech, not a rolling repeat.
		if !strings.Contains(rolling, "right? We we eat lots of it.") {
			t.Errorf("lost the speaker's repeat: %q", rolling)
		}
	})

	t.Run("a plain track keeps what a speaker repeats across cues", func(t *testing.T) {
		// Repeats shaped like real ones that deduplicating every track deleted.
		repeats := []row{
			{900, 2, "what ruins most finishes is the damp."}, {902, 2, "The damp gets under the wax and"},
			{904, 2, "turns it white, you see?"}, {906, 2, "You see?"},
		}
		text := strings.Join(texts(cuesOf(append(slices.Clone(lecture), repeats...))), " ")
		if !strings.HasSuffix(text, "what ruins most finishes is the damp. The damp gets under the wax and turns it white, you see? You see?") {
			t.Errorf("repeats lost: %q", text)
		}
	})

	t.Run("a track rolls when half its cues repeat, wherever they are", func(t *testing.T) {
		lines := []string{"one two three", "four five six", "seven eight nine", "ten eleven twelve", "alpha beta gamma",
			"delta epsilon zeta", "eta theta iota", "kappa lambda mu"}
		// The first `plain` cues show one line; the rest roll, showing the line before again.
		read := func(plain int) []string {
			var cues []Cue
			for i, text := range lines {
				if i >= plain {
					text = lines[i-1] + "\n" + text
				}
				cues = append(cues, Cue{float64(i * 2), float64(i*2 + 2), text})
			}
			return texts(DropRepeats(cues))
		}
		// Rolling starts late: four of eight cues repeat, so the repeats go.
		equal(t, read(4), lines)
		// Three of eight is speech that happens to repeat: kept, lines joined.
		equal(t, read(5)[5:], []string{"alpha beta gamma delta epsilon zeta", "delta epsilon zeta eta theta iota", "eta theta iota kappa lambda mu"})
	})

	t.Run("srv3 word runs, entities and line breaks", func(t *testing.T) {
		xml := `<?xml version="1.0" encoding="utf-8" ?><timedtext format="3"><head><pen id="1"/></head><body>` +
			`<p t="0" d="3280" w="1"><s ac="0">Would</s><s t="320" ac="0"> you</s><s t="440"> believe</s></p>` +
			"<p t=\"1750\" d=\"10\" w=\"1\" a=\"1\">\n</p>" +
			`<p t="3280" d="2000">Salt &amp;amp; pepper<br/>say &quot;hi&quot; &lt;3</p></body></timedtext>`
		cues, err := ParseCaptions(xml)
		if err != nil {
			t.Fatal(err)
		}
		equal(t, cues, []Cue{{0, 3.28, "Would you believe"}, {3.28, 5.28, "Salt & pepper\nsay \"hi\" <3"}})
	})

	t.Run("SRT with styling tags and an HTML page instead of captions", func(t *testing.T) {
		cues, err := ParseCaptions("1\r\n00:00:01,000 --> 00:00:03,500\r\n<i>Hallo</i> &amp; <font color=\"red\">willkommen</font>\r\n\r\n2\r\n00:01:02,250 --> 00:01:04,000\r\na < b\r\n")
		if err != nil {
			t.Fatal(err)
		}
		equal(t, cues, []Cue{{1, 3.5, "Hallo & willkommen"}, {62.25, 64, "a < b"}})
		_, err = Parse("<html><body>Sorry</body></html>", "YouTube captions")
		if !rustle.HasCode(err, "CAPTIONS_UNAVAILABLE") {
			t.Fatalf("got %v", err)
		}
		equal(t, err.Error(), "YouTube captions are not valid WebVTT, SRT or timed-text XML.")
	})

	t.Run("doubled lyric and sound markers collapse, sung parentheses stay", func(t *testing.T) {
		paras := Paragraphs([]Cue{cue(0, "♪ The kettle's singing at dawn ♪"), cue(1, "♪ Bread is in the oven ♪"), cue(2, "[Music]"), cue(3, "[Music]"),
			cue(4, "(Knead it now)"), cue(5, "(Knead it now)")}, MinParagraph, MaxParagraph)
		equal(t, paras[0].Text, "♪ The kettle's singing at dawn ♪ Bread is in the oven ♪ [Music] (Knead it now) (Knead it now)")
	})

	t.Run("a 63-minute srv1 file parses in a few milliseconds", func(t *testing.T) {
		read := func() []Paragraph {
			cues, err := ParseCaptions(workshopXML)
			if err != nil {
				t.Fatal(err)
			}
			return Paragraphs(DropRepeats(cues), MinParagraph, MaxParagraph)
		}
		paras := read()
		fastest := time.Duration(math.MaxInt64)
		for range 5 {
			began := time.Now()
			paras = read()
			fastest = min(fastest, time.Since(began))
		}
		// About 2 ms on a laptop. The bound leaves room for a slow CI machine and still
		// catches work that grows with the square of the track, which takes seconds.
		if fastest >= 50*time.Millisecond {
			t.Errorf("took %v", fastest)
		}
		if len(paras) <= 90 || paras[len(paras)-1].Start <= 3700 {
			t.Errorf("%d paragraphs, the last at %v", len(paras), paras[len(paras)-1].Start)
		}
		// A citation is never far from a timestamp while someone is speaking.
		for i := 1; i < len(paras); i++ {
			if gap := paras[i].Start - paras[i-1].Start; gap > 60 {
				t.Errorf("gap of %v s before %v", gap, paras[i].Start)
			}
		}
	})
}

func TestTrackSelection(t *testing.T) {
	caption := func(language, kind, name string, original bool) rustle.Caption {
		c := track(language, kind, original, "")
		c.Name = name
		return c
	}

	t.Run("auto-dubbed videos resolve to the original audio language", func(t *testing.T) {
		var tracks []rustle.Caption
		for _, code := range []string{"ar", "bn", "nl-NL", "en", "fr-FR", "de-DE", "iw", "hi", "id", "it", "ja", "ko", "ml", "pl", "pt-BR", "pa", "ru", "es-US", "ta", "te", "uk"} {
			tracks = append(tracks, caption(code, "asr", code+" (auto-generated)", code == "en"))
		}
		chosen, _ := SelectTrack(tracks, "", "en")
		equal(t, chosen.Language, "en")
		// Asking for English must not pick the first track listed, an Arabic dub.
		chosen, _ = SelectTrack(tracks, "en", "en")
		equal(t, chosen.Language, "en")
	})

	t.Run("a dub into English keeps the Spanish original", func(t *testing.T) {
		chosen, notes := SelectTrack([]rustle.Caption{caption("es", "manual", "Spanish", true), caption("es", "asr", "Spanish (auto-generated)", true)}, "", "es")
		equal(t, []any{chosen.Language, chosen.Kind, len(notes)}, []any{"es", "manual", 0})
	})

	t.Run("human captions in the original language win", func(t *testing.T) {
		tracks := []rustle.Caption{caption("en", "manual", "English", true), caption("en", "asr", "English (auto-generated)", true), caption("de-DE", "manual", "German (Germany)", false),
			caption("ja", "manual", "Japanese", false), caption("pt-BR", "manual", "Portuguese (Brazil)", false), caption("es-419", "manual", "Spanish (Latin America)", false)}
		chosen, notes := SelectTrack(tracks, "", "en")
		equal(t, []any{chosen.Language, chosen.Kind, len(notes)}, []any{"en", "manual", 0})
		for language, want := range map[string]string{"ja": "ja", "es": "es-419", "japanese": "ja"} {
			chosen, _ := SelectTrack(tracks, language, "en")
			equal(t, chosen.Language, want)
		}
	})

	t.Run("subtitles in another language do not replace the speech", func(t *testing.T) {
		tracks := []rustle.Caption{caption("en", "manual", "English", false), caption("es", "asr", "Spanish (auto-generated)", true)}
		chosen, notes := SelectTrack(tracks, "", "es")
		equal(t, []string{chosen.Language, chosen.Kind}, []string{"es", "asr"})
		equal(t, notes, []string{"Human captions exist in: en. Pass language to read one."})
		chosen, _ = SelectTrack(tracks, "en", "es")
		equal(t, chosen.Kind, "manual")
	})

	t.Run("an unknown original prefers English over the first dub", func(t *testing.T) {
		tracks := []rustle.Caption{track("ar", "asr", false, ""), track("bn", "asr", false, ""), track("en", "asr", false, "")}
		chosen, _ := SelectTrack(tracks, "", "")
		equal(t, chosen.Language, "en")
	})

	dubbed := []rustle.Caption{track("ar", "asr", false, ""), track("bn", "asr", false, ""), track("en", "asr", true, ""), track("fr-FR", "asr", false, "")}
	for _, language := range []string{"auto", "", "en", "EN", "en-US", "a.en", "a.en-US"} {
		t.Run(fmt.Sprintf("language %q picks the original track", language), func(t *testing.T) {
			chosen, notes := SelectTrack(dubbed, WantedLanguage(language), "en")
			equal(t, []any{chosen.Language, len(notes)}, []any{"en", 0})
		})
	}

	t.Run("a missing language falls back to the original with a note", func(t *testing.T) {
		chosen, notes := SelectTrack(dubbed, "de", "en")
		equal(t, chosen.Language, "en")
		equal(t, notes, []string{"No de captions; returned en. Available: ar, bn, en, fr-FR."})
		chosen, _ = SelectTrack(dubbed, "fr", "en")
		equal(t, chosen.Language, "fr-FR")
	})

	t.Run("human captions are preferred over ASR", func(t *testing.T) {
		chosen, _ := SelectTrack([]rustle.Caption{track("en", "manual", true, ""), track("en", "asr", true, "")}, "", "en")
		equal(t, chosen.Kind, "manual")
	})

	t.Run("tracks without a URL are never chosen", func(t *testing.T) {
		chosen, notes := SelectTrack([]rustle.Caption{{Language: "en", Kind: "manual", Original: true}}, "", "en")
		equal(t, chosen, rustle.Caption{})
		equal(t, len(notes), 0)
	})
}

func TestSidecars(t *testing.T) {
	folder := t.TempDir()
	media := filepath.Join(folder, "talk.mp4")
	for name, text := range map[string]string{"talk.mp4": "", "talk.en.vtt": talkVTT, "talk.de.srt": "1\n00:00:01,000 --> 00:00:03,000\nHallo &amp; willkommen\n", "talk-notes.srt": ""} {
		if err := os.WriteFile(filepath.Join(folder, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Neither a link nor a folder is a sidecar, whatever its name.
	if err := os.Symlink(filepath.Join(folder, "talk.de.srt"), filepath.Join(folder, "talk.aa.srt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(folder, "talk.ab.srt"), 0o700); err != nil {
		t.Fatal(err)
	}
	find := func(language string) []any {
		path, tag, notes := FindSidecar(media, language)
		if path != "" {
			path = filepath.Base(path)
		}
		return []any{path, tag, notes}
	}
	equal(t, find(""), []any{"talk.en.vtt", "en", []string(nil)})
	equal(t, find("de-DE"), []any{"talk.de.srt", "de", []string(nil)})
	equal(t, find("fr"), []any{"talk.en.vtt", "en", []string{"No fr sidecar; returned talk.en.vtt. Tagged sidecars: en, de."}})
	// Untagged before English, SRT before WebVTT.
	for _, name := range []string{"talk.vtt", "talk.srt"} {
		if err := os.WriteFile(filepath.Join(folder, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	equal(t, find(""), []any{"talk.srt", "", []string(nil)})
	for _, name := range []string{"talk.en.vtt", "talk.de.srt", "talk.vtt", "talk.srt"} {
		os.Remove(filepath.Join(folder, name))
	}
	equal(t, find(""), []any{"", "", []string(nil)})
	equal(t, find("en"), []any{"", "", []string(nil)})
	path, _, _ := FindSidecar(filepath.Join(folder, "missing", "talk.mp4"), "")
	equal(t, path, "")
}

// search is Matches as the text a reading shows.
func search(units []Paragraph, query string, budget int) (string, int, []string) {
	found, count, notes := Matches(units, query, budget)
	if count == 0 {
		return NoMatch(query), 0, notes
	}
	lines := make([]string, len(found))
	for i, m := range found {
		lines[i] = m.Text
	}
	return strings.Join(lines, "\n"), count, notes
}

func TestSearch(t *testing.T) {
	sentences := Paragraphs(cuesOf(lecture), 0, 12)
	introduce := "13:22 My own aunt waxed her desk in one day. So now I'll introduce a better option. Hardwax oil soaks deep into the wood like oil, then sets hard as wax."

	t.Run("matching sentences come with one sentence of context", func(t *testing.T) {
		text, count, notes := search(sentences, "introduce", 45000)
		equal(t, []any{text, count, len(notes)}, []any{introduce, 1, 0})
		// Frames go to the match, a sentence after the context the line starts with.
		if found, _, _ := Matches(sentences, "introduce", 45000); found[0].At <= found[0].Start {
			t.Errorf("match at %v, its line at %v", found[0].At, found[0].Start)
		}
		// Case-insensitive alternatives; each match keeps its neighbouring sentences.
		text, count, _ = search(sentences, "  INTRODUCE | Drawers ", 45000)
		equal(t, count, 2)
		equal(t, strings.Split(text, "\n"), []string{introduce, "13:44 On that table, the hardwax oil really proved itself over a long winter. Let's open up the drawers, where the older repairs hide, and honestly, uh, I would call a few of them rather creative. The worst of all is this bent nail."})
	})

	t.Run("nothing found names the query as JSON", func(t *testing.T) {
		text, count, _ := search(sentences, " kubernetes ", 45000)
		equal(t, []any{text, count}, []any{`Nothing said mentions "kubernetes".`, 0})
		text, _, _ = search(sentences, `say "hi" \ <now>`, 45000)
		equal(t, text, `Nothing said mentions "say \"hi\" \\ <now>".`)
	})

	t.Run("at most 30 matches", func(t *testing.T) {
		// Forty back-to-back copies of the first minute, each naming example.com once.
		var rows []row
		for copy := range 40 {
			for _, r := range talk {
				rows = append(rows, row{r.start + float64(61*copy), r.dur, r.text})
			}
		}
		text, count, notes := search(Paragraphs(cuesOf(rows), 0, 12), "example.com", 45000)
		equal(t, count, 40)
		equal(t, notes, []string{"showing the first 30 of 40 matches; narrow the query or pass start_seconds"})
		equal(t, strings.Count(text, "example.com"), 30)
	})

	t.Run("a small budget stops after the first line", func(t *testing.T) {
		text, count, notes := search(sentences, "the", 300)
		if lines := strings.Split(text, "\n"); count < 2 || len(lines) != 1 || !lineStart.MatchString(lines[0]) {
			t.Errorf("%d matches, lines %q", count, lines)
		}
		equal(t, notes, []string{"stopped at the size limit; narrow the query"})
	})
}

func TestPage(t *testing.T) {
	paras := Paragraphs(cuesOf(talk), MinParagraph, MaxParagraph)
	whole := Page(paras, 45000)
	equal(t, stamps(whole.Lines), []string{"0:00", "0:24", "0:55"})
	equal(t, whole.HasNext || whole.HasCut, false)
	for _, l := range whole.Lines {
		if !lineStart.MatchString(l) {
			t.Errorf("line %q", l)
		}
	}

	t.Run("pages continue where the last stopped", func(t *testing.T) {
		first := Page(paras, 700)
		// "and more. Today" splits one cue; the second sentence starts 24.301 s in.
		equal(t, []any{len(first.Lines), first.Next, first.HasNext, first.HasCut}, []any{1, 24.301, true, false})
		rest := slices.IndexFunc(paras, func(p Paragraph) bool { return p.Start >= first.Next })
		second := Page(paras[rest:], 700)
		equal(t, second.HasNext, false)
		equal(t, append(first.Lines, second.Lines...), whole.Lines)
	})

	t.Run("a paragraph longer than a page is cut", func(t *testing.T) {
		page := Page(paras, 100)
		equal(t, len(page.Lines), 1)
		equal(t, rustle.UTF16Len(page.Lines[0]), 100)
		equal(t, []any{page.Cut, page.HasCut, page.Next, page.HasNext}, []any{0.0, true, 24.301, true})
		// A cut never ends inside a character. (The TypeScript kept the emoji's first UTF-16
		// half here, which a Go string cannot hold.)
		equal(t, Page([]Paragraph{{0, 1, "😀😀😀"}}, 10).Lines, []string{"0:00 "})
		equal(t, Page([]Paragraph{{0, 1, "😀😀😀"}}, 13).Lines, []string{"0:00 😀"})
	})

	t.Run("Japanese pages are budgeted by what they cost in tokens", func(t *testing.T) {
		var rows []row
		for i := range 200 {
			rows = append(rows, row{float64(i * 3), 3, fmt.Sprintf("第%d章、生地をゆっくり発酵させます。焼くのが待ち遠しい。", i)})
		}
		page := Page(Paragraphs(cuesOf(rows), MinParagraph, MaxParagraph), 2000)
		text := strings.Join(page.Lines, "\n")
		if Cost(text) > 2000 || rustle.UTF16Len(text) >= 600 || !page.HasNext {
			t.Errorf("%d characters cost %d, next %v", rustle.UTF16Len(text), Cost(text), page.HasNext)
		}
	})
}

func TestCost(t *testing.T) {
	for _, c := range []struct {
		text string
		want int
	}{
		{"", 0},
		{"plain English, 42!", 18},
		{"café … “quoted”", 15},
		{"Привет", 12},
		{"生地をこねる", 24},
		{"😀𠀀", 16},
		{"aͰb", 4},
	} {
		equal(t, Cost(c.text), c.want)
	}
}

func TestSilence(t *testing.T) {
	equal(t, Silence(900, 768.35), "No speech in the transcript after 15:00.")
	equal(t, Silence(100, 200), "No speech in the transcript between 1:40 and 3:20.")
}

// The tables below hold what the JavaScript patterns these matchers replace return for
// the same input.

type pair[T any] struct {
	in   string
	want T
}

func TestElements(t *testing.T) {
	for _, c := range []pair[[][3]string]{
		{`<text start="1">a</text >`, [][3]string{{"text", ` start="1"`, "a"}}},
		{`<texts start="1">a</texts>`, nil},
		{`<p t="1"/>`, [][3]string{{"p", ` t="1"`, ""}}},
		{`<p t="1">x`, nil},
		{`<p a="/">y</p>`, [][3]string{{"p", ` a="/"`, "y"}}},
		{`<p t="2">a</pen></p>`, [][3]string{{"p", ` t="2"`, "a</pen>"}}},
		{`<text start="1">a<text start="2">b</text>`, [][3]string{{"text", ` start="1"`, `a<text start="2">b`}}},
		{`<pen/><p>z</p>`, [][3]string{{"p", "", "z"}}},
	} {
		var got [][3]string
		for from := 0; ; {
			e, ok := nextElement(c.in, from)
			if !ok {
				break
			}
			got = append(got, [3]string{e.name, e.attrs, e.body})
			from = e.end
		}
		equal(t, got, c.want)
	}
}

func TestAttributes(t *testing.T) {
	for _, c := range []pair[[][2]string]{
		{` start="1" dur="2"`, [][2]string{{"start", "1"}, {"dur", "2"}}},
		{` t='5' d="6"`, [][2]string{{"t", "5"}, {"d", "6"}}},
		{` a = "1" a="2"`, [][2]string{{"a", "1"}, {"a", "2"}}},
		{` é="1" x:y.z-w="v"`, [][2]string{{"x:y.z-w", "v"}}},
		{` aa bb="2"`, [][2]string{{"bb", "2"}}},
		{` c="unclosed`, nil},
		{` ="1"`, nil},
	} {
		var got [][2]string
		scanAttributes(c.in, func(name, value string) { got = append(got, [2]string{name, value}) })
		equal(t, got, c.want)
	}
}

func TestUnescapeAndClean(t *testing.T) {
	for _, c := range []pair[string]{
		{"&amp;lt;3", "&lt;3"},
		{"&#x41;&#65;&#X42;&#0066;", "AABB"},
		{"&#0; &#55296; &#x110000;", "� � �"},
		{"&#12345678; &#x1234567;", "&#12345678; &#x1234567;"},
		{"&bogus; &;", "&bogus; &;"},
		{"&&amp;", "&&"},
		{"it&#39;s &hellip;&nbsp;", "it's …\u00a0"},
		{"&toString;", "&toString;"},
	} {
		equal(t, Unescape(c.in), c.want)
	}
	for _, c := range []pair[string]{
		{`<i>Hallo</i> &amp; <font color="red">willkommen</font>`, "Hallo & willkommen"},
		{"a < b", "a < b"},
		{`<SPAN class="x">y</SPAN>`, "y"},
		{"<ſpan>z</ſpan>", "<ſpan>z</ſpan>"},
		{"<00:00:00.280><c> you</c>", "you"},
		{"<123:00.000>x", "<123:00.000>x"},
		{"<b.x>  bold </b>", "bold"},
		{"<c2>k</c2>", "<c2>k</c2>"},
	} {
		equal(t, Clean(c.in), c.want)
	}
}

func TestSentenceBreaks(t *testing.T) {
	for _, c := range []pair[[]string]{
		{`Hello. World! Is it? Yes… "Quoted." (Paren.) end`, []string{"Hello. ", "Hello. World! ", "Hello. World! Is it? ", "Hello. World! Is it? Yes… ", `Hello. World! Is it? Yes… "Quoted." `, `Hello. World! Is it? Yes… "Quoted." (Paren.) `}},
		{"日本語。次！最後？」終", []string{"日本語。", "日本語。次！", "日本語。次！最後？」"}},
		{"a.. b", []string{"a.. "}},
		{"a.'x b", nil},
		{"No break.", nil},
	} {
		var got []string
		sentenceBreaks(c.in, func(end int) { got = append(got, c.in[:end]) })
		equal(t, got, c.want)
	}
	for _, c := range []pair[bool]{{"Done.", true}, {`Done."`, true}, {"Done.)x", false}, {"終わり。」", true}, {"why?!", true}, {"no", false}} {
		equal(t, endsSentence(c.in), c.want)
	}
}

func TestJoinParts(t *testing.T) {
	for _, c := range []pair[string]{
		{"[Music] [Music]", "[Music]"},
		{"[[Music] [Music]", "[[Music]"},
		{"[" + strings.Repeat("x", 41) + "] [" + strings.Repeat("x", 41) + "]", "[" + strings.Repeat("x", 41) + "] [" + strings.Repeat("x", 41) + "]"},
		// The 40 is UTF-16 units: twenty emoji fit, twenty-one do not.
		{"[" + strings.Repeat("😀", 20) + "] [" + strings.Repeat("😀", 20) + "]", "[" + strings.Repeat("😀", 20) + "]"},
		{"[" + strings.Repeat("😀", 21) + "] [" + strings.Repeat("😀", 21) + "]", "[" + strings.Repeat("😀", 21) + "] [" + strings.Repeat("😀", 21) + "]"},
		{"[a] [a] [b]  [b]", "[a] [b]"},
		{"[A] [a]", "[A] [a]"},
		{"[] []", "[] []"},
		{"[a\nb] [a\nb]", "[a\nb] [a\nb]"},
		{"♪ a ♪ ♪ b ♪", "♪ a ♪ b ♪"},
		{"♪♪", "♪♪"},
		{"♪ ♪ ♪", "♪"},
		{"♪\n♪ x", "♪ x"},
	} {
		equal(t, joinParts([]string{c.in}), c.want)
	}
	// Characters beyond the Basic Multilingual Plane join without a space, as in the TypeScript.
	equal(t, joinParts([]string{"hello 😀", "😀 world"}), "hello 😀😀 world")
}

func TestWantedLanguage(t *testing.T) {
	for _, c := range []pair[string]{{"", ""}, {" auto ", ""}, {"Original", ""}, {"en", "en"}, {"a.en-US", "en-US"}, {"A.de", "de"}, {"..fr..", "fr"}, {"...", ""}} {
		equal(t, WantedLanguage(c.in), c.want)
	}
	equal(t, []string{Primary("en-US"), Primary("pt_BR"), Primary("")}, []string{"en", "pt", ""})
}

func TestExtname(t *testing.T) {
	for _, c := range []pair[string]{{"a.b.c", ".c"}, {".bashrc", ""}, {"..", ""}, {"...", "."}, {"a.", "."}, {"..a", ".a"}, {"talk", ""}} {
		equal(t, extname(c.in), c.want)
	}
}

func BenchmarkParseWorkshop(b *testing.B) {
	for b.Loop() {
		cues, _ := ParseCaptions(workshopXML)
		Paragraphs(DropRepeats(cues), MinParagraph, MaxParagraph)
	}
}
