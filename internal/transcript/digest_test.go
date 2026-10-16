package transcript

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

func TestDigest(t *testing.T) {
	t.Run("an hour is a third of the words, verbatim, covering the whole hour", func(t *testing.T) {
		cues, err := ParseCaptions(workshopXML)
		if err != nil {
			t.Fatal(err)
		}
		cues = DropRepeats(cues)
		full := Page(Paragraphs(cues, MinParagraph, MaxParagraph), 1e9)
		brief := Page(Digest(Paragraphs(cues, 0, 12), nil, 0), 45000)
		fullText, briefText := strings.Join(full.Lines, "\n"), strings.Join(brief.Lines, "\n")
		if ratio := float64(rustle.UTF16Len(fullText)) / float64(rustle.UTF16Len(briefText)); ratio <= 3 || ratio >= 3.9 {
			t.Errorf("ratio %v", ratio)
		}
		if brief.HasNext {
			t.Error("an hour should fit one page")
		}
		// Every kept sentence is the speaker's own words, less fillers, in order.
		var said []string
		for _, l := range full.Lines {
			_, text, _ := strings.Cut(l, " ")
			said = append(said, text)
		}
		speech := TidySpeech(strings.Join(said, " "))
		stamp := regexp.MustCompile(`^\d+:\d{2}(?::\d{2})? `)
		from := 0
		for _, l := range brief.Lines {
			_, text, _ := strings.Cut(l, " ")
			for _, piece := range strings.Split(text, " … ") {
				piece = strings.TrimSuffix(strings.TrimPrefix(piece, "… "), "…")
				if piece = rustle.TrimSpace(stamp.ReplaceAllString(piece, "")); piece == "" {
					continue
				}
				at := strings.Index(speech[from:], rustle.Cut(piece, 60))
				if at < 0 {
					t.Fatalf("not verbatim or out of order: %s", rustle.Cut(piece, 60))
				}
				from += at
			}
		}
		// No stretch of the hour goes unseen: lines start at most a few minutes apart.
		var starts []int
		for _, s := range stamps(brief.Lines) {
			n := 0
			for _, part := range strings.Split(s, ":") {
				var v int
				fmt.Sscan(part, &v)
				n = n*60 + v
			}
			starts = append(starts, n)
		}
		for i := 1; i < len(starts); i++ {
			if starts[i]-starts[i-1] > 300 {
				t.Errorf("gap before %d s", starts[i])
			}
		}
		if !strings.Contains(briefText, " … ") {
			t.Error("skipped speech is not marked")
		}
	})

	t.Run("the opening and each chapter start are kept", func(t *testing.T) {
		var units []Cue
		for i := range 60 {
			units = append(units, Cue{Start: float64(i * 10), Text: fmt.Sprintf("Sentence %d talks about topic %d at length with several words.", i, i%7)})
		}
		var said []string
		for _, l := range Digest(units, []rustle.Chapter{{At: 0, Title: "Intro"}, {At: 300, Title: "Middle"}}, 0) {
			said = append(said, l.Text)
		}
		text := strings.Join(said, " ")
		if !strings.HasPrefix(text, "Sentence 0 talks") || !strings.Contains(text, "Sentence 1 talks") {
			t.Errorf("the opening is missing: %q", text)
		}
		if !strings.Contains(text, "Sentence 30 talks") {
			t.Errorf("the first sentence of the chapter at 5:00 is missing: %q", text)
		}
	})

	t.Run("fillers and stutters go, words stay", func(t *testing.T) {
		equal(t, TidySpeech("So, um, I I think uh the the answer is yes."), "So, I think the answer is yes.")
		equal(t, TidySpeech("Uhm, ahh, right."), "right.")
		equal(t, TidySpeech("The human mind"), "The human mind")
	})

	t.Run("nothing to say", func(t *testing.T) {
		equal(t, len(Digest(nil, nil, 0)), 0)
		equal(t, len(Digest([]Cue{{Start: 1, Text: "um, uh"}}, nil, 0)), 0)
		equal(t, Digest([]Cue{{Start: 1, Text: "One thought."}, {Start: 4, Text: "uh"}}, nil, 0), []Cue{{1, 4, "One thought."}})
	})
}

// The tables below hold what the JavaScript patterns these matchers replace return for
// the same input.

func TestFillers(t *testing.T) {
	for _, c := range []pair[string]{
		{"So, um, I think", "So,  I think"},
		// A comma taken by one filler cannot lead the next.
		{"um,uh", "uh"},
		{"Uhm, ahh, right.", "  right."},
		{"uh", ""},
		{"the uh. end", "the  end"},
		{"humm um", "humm "},
		{"uhu uh", "uhu "},
		{"ERM, well", " well"},
		{"mhm.", ""},
		{"a uh b", "a  b"},
		{"x,um.,y", "x,,y"},
		{"hmmm?", "?"},
		{"umbrella um", "umbrella "},
	} {
		equal(t, dropFillers(c.in), c.want)
	}
}

func TestStutters(t *testing.T) {
	for _, c := range []pair[string]{
		{"I I think", "I think"},
		{"the the the end", "the end"},
		{"The the", "The"},
		// \b knows only ASCII letters: "é" ends no word, so "café" repeats inside "cafés".
		{"café cafés", "cafés"},
		{"éte te", "éte"},
		{"don't don't", "don't"},
		{"go, go,go", "go"},
		// Under /iu, the Kelvin sign is a k and ſ an s.
		{"K k", "K"},
		{"ſ s", "ſ"},
		{"is island", "is island"},
		{"ab ab_", "ab ab_"},
		{"ͅ ι", "ͅ ι"},
		{"ΟΔΟΣ οδος", "ΟΔΟΣ οδος"},
		{"x é é", "x é é"},
	} {
		equal(t, dropStutters(c.in), c.want)
	}
}

func TestOpener(t *testing.T) {
	for _, c := range []pair[bool]{
		{"Now, let me", true}, {"NOW then", true}, {"and now", true}, {"So now", true}, {"Okay, next", true},
		{"alright, finally", true}, {"The Final Step!", true}, {"nowhere", false}, {"ſo now", false},
		{"the next oneself", false}, {"Let's go", true},
	} {
		equal(t, opener.MatchString(asciiLower(c.in)), c.want)
	}
}

func TestTerms(t *testing.T) {
	for _, c := range []pair[[]string]{
		{"The quick brown fox, and the fox again!", []string{"quick", "brown", "fox", "fox"}},
		{"It's the B-screw’s job", []string{"it's", "b-screw’s", "job"}},
		{"パン生地", []string{"パン", "ン生", "生地"}},
		{"go 1 22 333", []string{"333"}},
		{"Right.", nil},
	} {
		equal(t, terms(c.in), c.want)
	}
}

func BenchmarkDigestWorkshop(b *testing.B) {
	cues, _ := ParseCaptions(workshopXML)
	sentences := Paragraphs(DropRepeats(cues), 0, 12)
	for b.Loop() {
		Digest(sentences, nil, 0)
	}
}
