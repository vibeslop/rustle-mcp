package tools

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vibeslop/rustle-mcp/internal/media"
	"github.com/vibeslop/rustle-mcp/internal/rustle"
	"github.com/vibeslop/rustle-mcp/internal/transcript"
)

// ytRecord is a resolved YouTube video; tests change its fields.
func ytRecord(captions ...rustle.Caption) *rustle.Record {
	return &rustle.Record{Key: strings.Repeat("k", 32), Kind: "youtube", Source: "https://www.youtube.com/watch?v=aaaaaaaaaaa",
		Title: "Video", ResolvedAt: time.Now().UnixMilli(), Duration: 768.35, Captions: captions, OriginalLanguage: "en",
		Chapters: []rustle.Chapter{}}
}

type fetcher = func(rustle.Caption) (string, error)

// served serves one caption file per language and records which tracks were fetched.
type served struct {
	files   map[string]string
	fetched []string
}

func captions(files map[string]string) *served { return &served{files: files} }

func (s *served) fetch(t rustle.Caption) (string, error) {
	s.fetched = append(s.fetched, t.Language)
	return s.files[t.Language], nil
}

func offline(rustle.Caption) (string, error) { return "", errors.New("no caption fetch expected") }

// reading is one reading of speech: its arguments and the settings it runs with.
type reading struct {
	args     TranscriptArgs
	settings *rustle.Settings
}

type option func(*reading)

func from(seconds float64) option            { return func(r *reading) { r.args.Start = seconds } }
func until(seconds float64) option           { return func(r *reading) { r.args.End = &seconds } }
func query(q string) option                  { return func(r *reading) { r.args.Query = q } }
func language(l string) option               { return func(r *reading) { r.args.Language = l } }
func detail(d string) option                 { return func(r *reading) { r.args.Detail = d } }
func transcribing() option                   { return func(r *reading) { r.args.Transcribe = true } }
func chars(n int) option                     { return func(r *reading) { r.settings.TranscriptChars = n } }
func roots(folders ...string) option         { return func(r *reading) { r.settings.MediaRoots = folders } }
func withSettings(s *rustle.Settings) option { return func(r *reading) { r.settings = s } }

type readResult struct {
	meta  *rustle.Obj
	text  string
	lines []string
	notes []string
}

// readWith reads record's transcript as the operation does once the source is resolved:
// every word (detail=full) of the whole video, unless options say otherwise.
func readWith(t *testing.T, record *rustle.Record, fetch fetcher, options ...option) (readResult, error) {
	t.Helper()
	r := reading{args: TranscriptArgs{Source: record.Source, Language: "auto", Detail: "full"}, settings: testSettings(t)}
	for _, o := range options {
		o(&r)
	}
	c := newCall(t, r.settings, "test")
	result, err := ReadTranscript(c, record, r.args, fetch)
	if err != nil {
		return readResult{notes: c.Notes.List()}, err
	}
	if len(result.Content) != 1 || result.Content[0].Type != "text" {
		t.Fatalf("content %+v", result.Content)
	}
	text := result.Content[0].Text
	return readResult{meta: result.Meta, text: text, lines: strings.Split(text, "\n"), notes: c.Notes.List()}, nil
}

func readOK(t *testing.T, record *rustle.Record, fetch fetcher, options ...option) readResult {
	t.Helper()
	r, err := readWith(t, record, fetch, options...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func readFails(t *testing.T, record *rustle.Record, fetch fetcher, options ...option) *rustle.Error {
	t.Helper()
	_, err := readWith(t, record, fetch, options...)
	return failure(t, err)
}

func metaField(m *rustle.Obj, key string) any {
	v, _ := m.Get(key)
	return v
}

func TestReadTranscript(t *testing.T) {
	t.Run("youtube captions become timestamped paragraphs", func(t *testing.T) {
		record := ytRecord(track("en", "asr", true, ""))
		record.Chapters, record.Title = talkChapters, "Baking"
		fetch := captions(map[string]string{"en": srv1(talk)})
		result := readOK(t, record, fetch.fetch)
		// 20 to 40 s paragraphs that end at sentence ends.
		equal(t, stamps(result.lines), []string{"0:00", "0:24", "0:55"})
		for _, l := range result.lines {
			if !lineStart.MatchString(l) {
				t.Errorf("line %q", l)
			}
		}
		if !strings.HasPrefix(result.lines[0], "0:00 Would you believe that plain water, salt, and some flour could") ||
			!strings.HasSuffix(result.lines[1], "change how fast a dough will rise.") ||
			!strings.HasPrefix(result.lines[2], "0:55 See them all at example.com/bake.") {
			t.Errorf("lines %q", result.lines)
		}
		// Header fields live in metadata only; the text block is nothing but paragraphs.
		equal(t, rustle.Compact(result.meta),
			`{"title":"Baking","duration":768.35,"language":"en","captions":"auto","span":[0,768.35],"chapters":[[0,"Intro"],[60,"1. Your Starter"],[154,"2. Cold Proof"]]}`)
		equal(t, fetch.fetched, []string{"en"})
		equal(t, result.notes, []string{})
	})

	t.Run("pages continue where the last stopped", func(t *testing.T) {
		record := ytRecord(track("en", "asr", true, ""))
		record.Chapters = talkChapters
		fetch := captions(map[string]string{"en": srv1(talk)}).fetch
		whole := readOK(t, record, fetch).lines
		first := readOK(t, record, fetch, chars(700))
		next := metaField(first.meta, "next_start_seconds")
		// "and more. Today" splits one cue; the second sentence starts 24.301 s in.
		equal(t, next, any(24.301))
		equal(t, metaField(first.meta, "span"), any([]float64{0, 24.301}))
		equal(t, metaField(first.meta, "chapters"), any(talkChapters))
		// The continuation lives in the metadata; the text holds only what was said.
		equal(t, len(first.lines), 1)
		second := readOK(t, record, fetch, from(24.301), chars(700))
		equal(t, metaField(second.meta, "next_start_seconds"), nil)
		equal(t, metaField(second.meta, "chapters"), nil)
		equal(t, append(first.lines, second.lines...), whole)
	})

	t.Run("end_seconds bounds the read without inviting another call", func(t *testing.T) {
		result := readOK(t, ytRecord(track("en", "asr", true, "")), captions(map[string]string{"en": srv1(talk)}).fetch, until(30))
		equal(t, stamps(result.lines), []string{"0:00", "0:24"})
		equal(t, metaField(result.meta, "span"), any([]float64{0, 30}))
		// The caller chose the end, so the span is the whole answer.
		equal(t, metaField(result.meta, "next_start_seconds"), nil)
	})

	t.Run("a cut page continues inside a deliberate span", func(t *testing.T) {
		record := ytRecord(track("en", "asr", true, ""))
		fetch := captions(map[string]string{"en": srv1(talk)}).fetch
		var pages [][]string
		for start, more := 0.0, true; more; {
			result := readOK(t, record, fetch, from(start), until(50), chars(600))
			pages = append(pages, result.lines)
			var next any
			next, more = result.meta.Get("next_start_seconds")
			if more {
				start = next.(float64)
			}
		}
		equal(t, len(pages), 2)
		equal(t, slices.Concat(pages...), readOK(t, record, fetch, until(50)).lines)
	})

	t.Run("a paragraph longer than a page is cut with a note", func(t *testing.T) {
		result := readOK(t, ytRecord(track("en", "asr", true, "")), captions(map[string]string{"en": srv1(talk)}).fetch, chars(100))
		equal(t, rustle.UTF16Len(result.text), 100)
		equal(t, metaField(result.meta, "next_start_seconds"), any(24.301))
		equal(t, result.notes, []string{"the paragraph at 0:00 was cut at the page size limit"})
	})

	t.Run("the window is clamped with notes", func(t *testing.T) {
		record := ytRecord(track("en", "asr", true, ""))
		fetch := captions(map[string]string{"en": srv1(talk)}).fetch
		result := readOK(t, record, fetch, until(9999))
		equal(t, result.notes, []string{"end_seconds lowered to the video's end (12:48)"})
		equal(t, metaField(result.meta, "span"), any([]float64{0, 768.35}))
		equal(t, readOK(t, record, fetch, from(900)).text, "No speech in the transcript after 12:48.")
		equal(t, readOK(t, record, fetch, from(100), until(200)).text, "No speech in the transcript between 1:40 and 3:20.")
	})

	t.Run("query returns matching sentences with one sentence of context", func(t *testing.T) {
		record := ytRecord(track("en", "asr", true, ""))
		record.Duration, record.Chapters = 2859.667, []rustle.Chapter{{At: 0, Title: "Intro"}}
		fetch := captions(map[string]string{"en": srv1(lecture)}).fetch
		result := readOK(t, record, fetch, query("introduce"))
		equal(t, rustle.Compact(result.meta), `{"title":"Video","duration":2859.667,"language":"en","captions":"auto","span":[0,2859.667],"matches":1}`)
		equal(t, result.lines, []string{"13:22 My own aunt waxed her desk in one day. So now I'll introduce a better option. Hardwax oil soaks deep into the wood like oil, then sets hard as wax."})
		// Case-insensitive alternatives; each match keeps its neighbouring sentences.
		either := readOK(t, record, fetch, query("  INTRODUCE | Drawers "))
		equal(t, metaField(either.meta, "matches"), any(2))
		equal(t, either.lines, append(slices.Clone(result.lines), "13:44 On that table, the hardwax oil really proved itself over a long winter. Let's open up the drawers, where the older repairs hide, and honestly, uh, I would call a few of them rather creative. The worst of all is this bent nail."))
		missing := readOK(t, record, fetch, query("kubernetes"))
		equal(t, metaField(missing.meta, "matches"), any(0))
		equal(t, missing.text, `Nothing said mentions "kubernetes".`)
	})

	t.Run("query shows at most 30 matches", func(t *testing.T) {
		// Forty back-to-back copies of the first minute, each naming example.com once.
		var rows []row
		for copy := range 40 {
			for _, r := range talk {
				rows = append(rows, row{r.start + float64(61*copy), r.dur, r.text})
			}
		}
		record := ytRecord(track("en", "asr", true, ""))
		record.Duration = 2500
		result := readOK(t, record, captions(map[string]string{"en": srv1(rows)}).fetch, query("example.com"))
		equal(t, metaField(result.meta, "matches"), any(40))
		equal(t, result.notes, []string{"showing the first 30 of 40 matches; narrow the query or pass start_seconds"})
		mentions := 0
		for _, l := range result.lines {
			if strings.Contains(l, "example.com") {
				mentions++
			}
		}
		equal(t, mentions, 30)
	})

	dubbed := func() *rustle.Record {
		return ytRecord(track("ar", "asr", false, ""), track("bn", "asr", false, ""), track("en", "asr", true, ""), track("fr-FR", "asr", false, ""))
	}
	dubbedFiles := func() *served {
		files := map[string]string{}
		for _, code := range []string{"ar", "bn", "en", "fr-FR"} {
			files[code] = srv1(talk[:4])
		}
		return captions(files)
	}

	for _, l := range []string{"auto", "", "en", "EN", "en-US", "a.en", "a.en-US"} {
		t.Run(fmt.Sprintf("language %q picks the original track", l), func(t *testing.T) {
			fetch := dubbedFiles()
			result := readOK(t, dubbed(), fetch.fetch, language(l))
			equal(t, metaField(result.meta, "language"), any("en"))
			equal(t, result.notes, []string{})
			equal(t, fetch.fetched, []string{"en"})
		})
	}

	t.Run("a missing language falls back to the original with a note", func(t *testing.T) {
		fetch := dubbedFiles().fetch
		result := readOK(t, dubbed(), fetch, language("de"))
		equal(t, metaField(result.meta, "language"), any("en"))
		equal(t, result.notes, []string{"No de captions; returned en. Available: ar, bn, en, fr-FR."})
		equal(t, metaField(readOK(t, dubbed(), fetch, language("fr")).meta, "language"), any("fr-FR"))
	})

	t.Run("human captions are reported as manual", func(t *testing.T) {
		result := readOK(t, ytRecord(track("en", "manual", true, ""), track("en", "asr", true, "")), captions(map[string]string{"en": srv1(talk[:4])}).fetch)
		equal(t, []any{metaField(result.meta, "language"), metaField(result.meta, "captions")}, []any{"en", "manual"})
	})

	t.Run("Japanese pages are budgeted by what they cost in tokens", func(t *testing.T) {
		var rows []row
		for i := range 200 {
			rows = append(rows, row{float64(i * 3), 3, fmt.Sprintf("第%d章、生地をゆっくり発酵させます。焼くのが待ち遠しい。", i)})
		}
		record := ytRecord(track("ja", "asr", true, ""))
		record.OriginalLanguage, record.Duration = "ja", 600
		result := readOK(t, record, captions(map[string]string{"ja": srv1(rows)}).fetch, chars(2000))
		if transcript.Cost(result.text) > 2000 || rustle.UTF16Len(result.text) >= 600 {
			t.Errorf("%d characters cost %d", rustle.UTF16Len(result.text), transcript.Cost(result.text))
		}
		if next, _ := metaField(result.meta, "next_start_seconds").(float64); next == 0 {
			t.Error("no next_start_seconds")
		}
	})
}

// fakeWhisper is a stand-in whisper-cli that logs its arguments and the size of the WAV it
// was given, and writes whisper.cpp's JSON output.
func fakeWhisper(t *testing.T) (bin, log, model string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in whisper-cli is a shell script")
	}
	dir := tempDir(t)
	bin, log, model = filepath.Join(dir, "bin"), filepath.Join(dir, "whisper.log"), filepath.Join(dir, "ggml-tiny.bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, model, "")
	output := `{"result":{"language":"en"},"transcription":[` +
		`{"timestamps":{"from":"00:00:01,000","to":"00:00:04,000"},"offsets":{"from":1000,"to":4000},"text":" Hello and welcome."},` +
		`{"timestamps":{"from":"00:00:20,000","to":"00:00:24,000"},"offsets":{"from":20000,"to":24000},"text":" Today we look at lazy imports."}]}`
	script := `#!/bin/sh
echo "$@" >> '` + log + `'
while [ $# -gt 0 ]; do
  case "$1" in -of) out="$2"; shift;; -f) wav="$2"; shift;; esac
  shift
done
wc -c < "$wav" >> '` + log + `'
printf '%s' '` + output + `' > "$out.json"
`
	if err := os.WriteFile(filepath.Join(bin, "whisper-cli"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log, model
}

// noWhisper is a machine without whisper.cpp, or at least without a model for it.
func noWhisper(t *testing.T) {
	t.Setenv("PATH", "/nonexistent")
	t.Setenv("RUSTLE_WHISPER_MODEL", "")
}

func logLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func TestLocalSpeechRecognition(t *testing.T) {
	bin, log, model := fakeWhisper(t)
	installed := func(t *testing.T) {
		t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		t.Setenv("RUSTLE_WHISPER_MODEL", model)
	}

	t.Run("no captions suggests transcribe only when it can work", func(t *testing.T) {
		noWhisper(t)
		e := readFails(t, ytRecord(), offline)
		equal(t, e.Code, "NO_CAPTIONS")
		// A retry would only fail with SPEECH_UNAVAILABLE, so the user is told how to install.
		if strings.Contains(e.Recovery, "transcribe=true") ||
			!strings.Contains(e.Recovery, "isn't available on this server") || !strings.Contains(e.Recovery, "brew install whisper-cpp") {
			t.Errorf("recovery %q", e.Recovery)
		}
		installed(t)
		e = readFails(t, ytRecord(), offline)
		if !strings.Contains(e.Recovery, "Retry with transcribe=true") || strings.Contains(e.Recovery, "brew") {
			t.Errorf("recovery %q", e.Recovery)
		}
		unlisted := ytRecord()
		unlisted.CaptionsError = true
		e = readFails(t, unlisted, offline)
		equal(t, []any{e.Code, e.Retryable}, []any{"NO_CAPTIONS", true})
	})

	t.Run("transcribe without whisper.cpp says how to install it", func(t *testing.T) {
		noWhisper(t)
		e := readFails(t, ytRecord(), offline, transcribing())
		equal(t, e.Code, "SPEECH_UNAVAILABLE")
		if !strings.Contains(e.Recovery, "RUSTLE_WHISPER_MODEL") {
			t.Errorf("recovery %q", e.Recovery)
		}
	})

	t.Run("whisper-cli arguments and the WAV it reads", func(t *testing.T) {
		equal(t, WhisperArgs("/m/ggml-base.bin", "/t/a.wav", "/t/out", "en-US")[:10],
			[]string{"-m", "/m/ggml-base.bin", "-f", "/t/a.wav", "-of", "/t/out", "-oj", "-np", "-l", "en"})
		equal(t, WhisperArgs("m", "a", "o", "")[9], "auto")
		data := WAV([]float32{0, 1, -1, 2}, 16000)
		equal(t, len(data), 44+8)
		equal(t, string(data[0:4])+string(data[8:16]), "RIFFWAVEfmt ")
		le16 := func(at int) int16 { return int16(uint16(data[at]) | uint16(data[at+1])<<8) }
		rate := uint32(data[24]) | uint32(data[25])<<8 | uint32(data[26])<<16 | uint32(data[27])<<24
		equal(t, []int{int(rate), int(le16(46)), int(le16(48)), int(le16(50))}, []int{16000, 32767, -32767, 32767})
	})

	t.Run("whisper transcribes at most 600 seconds per call", func(t *testing.T) {
		installed(t)
		s := testSettings(t)
		tools := needFFmpeg(t, s)
		audioFile := filepath.Join(tempDir(t), "talk.flac")
		if out, err := exec.Command(tools.FFmpeg, "-v", "error", "-f", "lavfi", "-i", "anullsrc=r=8000:cl=mono", "-t", "1500",
			"-c:a", "flac", "-y", audioFile).CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg: %v\n%s", err, out)
		}
		record := &rustle.Record{Key: strings.Repeat("w", 32), Kind: "url", Source: "https://example.com/talk.flac", Title: "Talk",
			ResolvedAt: time.Now().UnixMilli(), Input: audioFile, Captions: []rustle.Caption{}}
		result := readOK(t, record, offline, from(100), transcribing(), withSettings(s))
		logged := logLines(t, log)
		if !strings.Contains(logged[0], "-l auto") {
			t.Errorf("arguments %q", logged[0])
		}
		// 600 s of 16 kHz 16-bit mono audio, from 100 s to 700 s.
		equal(t, strings.TrimSpace(logged[1]), strconv.Itoa(44+600*16000*2))
		equal(t, rustle.Compact(result.meta), `{"title":"Talk","duration":1500,"language":"en","captions":"whisper","span":[100,700],"next_start_seconds":700}`)
		equal(t, result.lines, []string{"1:41 Hello and welcome. Today we look at lazy imports."})
		equal(t, result.notes, []string{"transcribed up to 11:40; call again with start_seconds=700 and transcribe=true for more"})
		// The transcription is cached with the source, so asking again decodes nothing.
		again := readOK(t, record, offline, from(100), transcribing(), withSettings(s))
		equal(t, again.lines, result.lines)
		equal(t, len(logLines(t, log)), 2)
		last := readOK(t, record, offline, from(1200), transcribing(), withSettings(s))
		equal(t, metaField(last.meta, "next_start_seconds"), nil)
		logged = logLines(t, log)
		equal(t, strings.TrimSpace(logged[len(logged)-1]), strconv.Itoa(44+300*16000*2))
	})
}

func TestSidecars(t *testing.T) {
	folder := filepath.Join(tempDir(t), "renders")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	video := filepath.Join(folder, "talk.mp4")
	// An MP4 header, so the file passes the media sniff; nothing could decode it.
	write(t, video, "\x00\x00\x00\x18ftypisom"+strings.Repeat("\x00", 500))
	record := &rustle.Record{Key: strings.Repeat("s", 32), Kind: "local", Source: video, Title: "talk.mp4",
		ResolvedAt: time.Now().UnixMilli(), Input: video, Captions: []rustle.Caption{}}
	in := roots(folder)

	t.Run("captions next to a local file", func(t *testing.T) {
		noWhisper(t)
		write(t, filepath.Join(folder, "talk.en.vtt"), talkVTT)
		write(t, filepath.Join(folder, "talk.de.srt"), "1\n00:00:01,000 --> 00:00:03,000\nHallo &amp; willkommen\n")
		write(t, filepath.Join(folder, "talk-notes.srt"), "1\n00:00:01,000 --> 00:00:02,000\nnot this one\n")
		english := readOK(t, record, offline, in)
		equal(t, rustle.Compact(english.meta), `{"title":"talk.mp4","language":"en","captions":"sidecar","span":[0,18.51]}`)
		if !strings.HasPrefix(english.lines[0], "0:00 Would you believe that plain water, salt,") {
			t.Errorf("line %q", english.lines[0])
		}
		equal(t, readOK(t, record, offline, in, language("de-DE")).lines, []string{"0:01 Hallo & willkommen"})
		french := readOK(t, record, offline, in, language("fr"))
		equal(t, metaField(french.meta, "language"), any("en"))
		equal(t, french.notes, []string{"No fr sidecar; returned talk.en.vtt. Tagged sidecars: en, de."})
		os.Remove(filepath.Join(folder, "talk.en.vtt"))
		os.Remove(filepath.Join(folder, "talk.de.srt"))
		e := readFails(t, record, offline, in)
		equal(t, e.Code, "NO_CAPTIONS")
		if !strings.Contains(e.Recovery, ".srt or .vtt") {
			t.Errorf("recovery %q", e.Recovery)
		}
	})

	t.Run("a UTF-16 sidecar from a Windows tool", func(t *testing.T) {
		sidecar := filepath.Join(folder, "talk.srt")
		text := []byte{0xff, 0xfe}
		for _, u := range "1\r\n00:00:01,000 --> 00:00:02,000\r\nCafé\r\n" {
			text = append(text, byte(u), byte(u>>8))
		}
		if err := os.WriteFile(sidecar, text, 0o644); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(sidecar)
		equal(t, readOK(t, record, offline, in).lines, []string{"0:01 Café"})
	})

	t.Run("a caption read resolves the source but never starts ffmpeg", func(t *testing.T) {
		// TypeScript counted spawn calls. Go can't watch process starts, and ffmpeg is located
		// once per process, so emptying PATH proves nothing once another test has decoded.
		// The read runs again in a fresh test process instead, where ffmpeg and ffprobe are
		// located for the first time from the call's settings, which name a stand-in that
		// logs every start.
		if log := os.Getenv("RUSTLE_TEST_FFMPEG_LOG"); log != "" {
			captionReadInFreshProcess(t, folder, video, log)
			return
		}
		if runtime.GOOS == "windows" {
			t.Skip("the stand-in ffmpeg is a shell script")
		}
		log := filepath.Join(tempDir(t), "ffmpeg.log")
		cmd := exec.Command(os.Args[0], "-test.run=^TestSidecars$/^a_caption_read_resolves_the_source_but_never_starts_ffmpeg$", "-test.v")
		cmd.Env = append(os.Environ(), "RUSTLE_TEST_FFMPEG_LOG="+log)
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "--- PASS: TestSidecars/a_caption_read") {
			t.Fatalf("the read in a fresh process: %v\n%s", err, out)
		}
		if started, err := os.ReadFile(log); err == nil {
			t.Errorf("ffmpeg or ffprobe started: %s", started)
		}
	})
}

// captionReadInFreshProcess reads a sidecar through the whole operation, with settings
// whose ffmpeg and ffprobe are a stand-in that logs each start to log.
func captionReadInFreshProcess(t *testing.T, folder, video, log string) {
	standIn := filepath.Join(tempDir(t), "ffmpeg")
	if err := os.WriteFile(standIn, []byte("#!/bin/sh\necho \"$0 $@\" >> '"+log+"'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sidecar := filepath.Join(folder, "talk.srt")
	write(t, sidecar, "1\n00:00:00,000 --> 00:00:02,000\nRed &amp; green\n\n2\n00:00:02,000 --> 00:00:04,000\nBlue then yellow\n")
	s := testSettings(t)
	s.MediaRoots, s.FFmpeg, s.FFprobe = []string{folder}, standIn, standIn
	result, err := watch(newCall(t, s, "test"), WatchArgs{Source: video, Language: "auto", Detail: "full"})
	if err != nil {
		t.Fatal(err)
	}
	equal(t, result.Content, []rustle.Block{rustle.TextBlock("0:00 Red & green Blue then yellow")})
	equal(t, metaField(result.Meta, "captions"), any("sidecar"))
	// Whether or not the read located them, any decode in this process starts the stand-in.
	if tools, err := media.ToolsFor(s); err != nil || tools.FFmpeg != standIn || tools.FFprobe != standIn {
		t.Errorf("located %+v, %v; want the stand-in", tools, err)
	}
}

func TestDigest(t *testing.T) {
	record := ytRecord(track("en", "asr", true, ""))
	record.Duration = 3772.9

	t.Run("the default read is a third of the words, verbatim, covering the whole hour", func(t *testing.T) {
		full := readOK(t, record, captions(map[string]string{"en": workshopXML}).fetch, chars(1e9))
		brief := readOK(t, record, captions(map[string]string{"en": workshopXML}).fetch, detail("digest"))
		ratio := float64(rustle.UTF16Len(full.text)) / float64(rustle.UTF16Len(brief.text))
		if ratio <= 3 || ratio >= 3.9 {
			t.Errorf("ratio %v", ratio)
		}
		if _, ok := brief.meta.Get("next_start_seconds"); ok {
			t.Error("an hour fits one call")
		}
		if d, _ := metaField(brief.meta, "digest").(string); !strings.Contains(d, "skipped speech") {
			t.Errorf("digest %q", d)
		}
		// Every kept sentence is the speaker's own words, less fillers, in order.
		var spoken []string
		for _, l := range full.lines {
			_, words, _ := strings.Cut(l, " ")
			spoken = append(spoken, words)
		}
		said := transcript.TidySpeech(strings.Join(spoken, " "))
		stamp := regexp.MustCompile(`^\d+:\d{2}(?::\d{2})? `)
		at := 0
		for _, l := range brief.lines {
			_, words, _ := strings.Cut(l, " ")
			for _, piece := range strings.Split(words, " … ") {
				piece = strings.TrimSuffix(strings.TrimPrefix(piece, "… "), "…")
				piece = strings.TrimSpace(stamp.ReplaceAllString(piece, ""))
				if piece == "" {
					continue
				}
				head := []rune(piece)[:min(60, len([]rune(piece)))]
				found := strings.Index(said[at:], string(head))
				if found < 0 {
					t.Errorf("not verbatim or out of order: %s", string(head))
					continue
				}
				at += found
			}
		}
		// No stretch of the hour goes unseen: lines start at most a few minutes apart.
		var starts []int
		for _, s := range stamps(brief.lines) {
			n := 0
			for _, v := range strings.Split(s, ":") {
				part, _ := strconv.Atoi(v)
				n = n*60 + part
			}
			starts = append(starts, n)
		}
		for i := 1; i < len(starts); i++ {
			if starts[i]-starts[i-1] > 300 {
				t.Errorf("gap before %d", starts[i])
			}
		}
		if !strings.Contains(brief.text, " … ") {
			t.Error("skipped speech is not marked")
		}
	})

	t.Run("a span or detail=full reads every word", func(t *testing.T) {
		// The server picks detail=full for a span (TestLenientArguments); here the read
		// itself must not digest it.
		span := readOK(t, record, captions(map[string]string{"en": workshopXML}).fetch, from(60), until(120))
		if _, ok := span.meta.Get("digest"); ok {
			t.Errorf("meta %s", rustle.Compact(span.meta))
		}
	})
}
