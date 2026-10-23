package tools

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/vibeslop/rustle-mcp/internal/media"
	"github.com/vibeslop/rustle-mcp/internal/proc"
	"github.com/vibeslop/rustle-mcp/internal/rustle"
	"github.com/vibeslop/rustle-mcp/internal/source"
	"github.com/vibeslop/rustle-mcp/internal/transcript"
)

// WhisperSeconds is the speech one call transcribes at most.
const WhisperSeconds = 600

const speechInstall = "The user can enable it by installing whisper.cpp (macOS: brew install whisper-cpp), downloading a ggml model " +
	"(e.g. ggml-base.bin from https://huggingface.co/ggerganov/whisper.cpp) and setting RUSTLE_WHISPER_MODEL to its path " +
	"in the MCP server config, then restarting the server."

// maxSentence: units for digests and searches are sentences, or stretches of this many
// seconds where nothing marks a sentence's end.
const maxSentence = 12

// ReadTranscript reads the transcript of a resolved source as text.
func ReadTranscript(c *rustle.Call, record *rustle.Record, args TranscriptArgs, fetchCaptions func(rustle.Caption) (string, error)) (*rustle.Result, error) {
	said, err := speak(c, record, args, fetchCaptions)
	if err != nil {
		return nil, err
	}
	return &rustle.Result{Meta: said.meta, Content: []rustle.Block{rustle.TextBlock(said.text())}}, nil
}

// spoken is what is said in a span: lines led by their clock, ready to lay out under the
// frames they go with.
type spoken struct {
	meta  *rustle.Obj
	lines []string
	// at is when each line starts; with a query, when its first match does.
	at []float64
	// empty stands in for lines when there are none.
	empty string
	// next is where the lines end when that is before the span asked for, because a page
	// filled or transcription stopped.
	next *float64
}

func (s *spoken) text() string {
	if len(s.lines) == 0 {
		return s.empty
	}
	return strings.Join(s.lines, "\n")
}

// speak reads the speech of a resolved source. Captions and sidecars never touch ffmpeg,
// so a caption read is not held up by media work on the same source; only transcribe
// decodes.
func speak(c *rustle.Call, record *rustle.Record, args TranscriptArgs, fetchCaptions func(rustle.Caption) (string, error)) (*spoken, error) {
	duration := record.Duration
	start := args.Start
	end := math.Inf(1)
	if args.End != nil {
		end = *args.End
	}
	if duration > 0 && args.End != nil && end > duration {
		c.Notes.Add("end_seconds lowered to the video's end (" + rustle.Clock(duration) + ")")
		end = math.Inf(1)
	}
	if duration > 0 && start > duration {
		c.Notes.Add("start_seconds lowered to the video's end (" + rustle.Clock(duration) + ")")
		start = duration
	}
	wanted := transcript.WantedLanguage(args.Language)
	var said *speech
	var language, captions string
	heardTo := math.Inf(1)
	switch {
	case args.Transcribe:
		heard, err := whisper(c, record, start, end, wanted)
		if err != nil {
			return nil, err
		}
		language, heardTo = heard.language, heard.stop
		said = &speech{cues: transcript.DropRepeats(heard.cues)}
		if heard.duration > 0 {
			duration = heard.duration
		}
		captions = "whisper"
	case record.Kind == "local":
		path, tag, found := transcript.FindSidecar(record.Source, wanted)
		if path == "" {
			return nil, noCaptions(record)
		}
		if info, err := os.Stat(path); err == nil && info.Size() > transcript.CaptionBytes {
			return nil, rustle.Fail("The sidecar caption file exceeds 8 MiB.", "CAPTIONS_UNAVAILABLE", "Pass transcribe=true to transcribe the speech locally instead.")
		}
		c.Notes.Add(found...)
		text, err := readText(path)
		if err != nil {
			return nil, noCaptions(record)
		}
		if said, err = speechOf(text, filepath.Base(path)); err != nil {
			return nil, err
		}
		language, captions = tag, "sidecar"
	default:
		if len(record.Captions) == 0 {
			return nil, noCaptions(record)
		}
		track, picked := transcript.SelectTrack(record.Captions, wanted, record.OriginalLanguage)
		c.Notes.Add(picked...)
		raw, err := fetchCaptions(track)
		if err != nil {
			return nil, err
		}
		if rustle.TrimSpace(raw) == "" {
			return nil, rustle.Fail("YouTube returned an empty caption file.", "CAPTIONS_UNAVAILABLE",
				"Retry in a minute, or pass transcribe=true to transcribe the speech locally.").Retry(0)
		}
		if said, err = speechOf(raw, "YouTube captions"); err != nil {
			return nil, err
		}
		language, captions = track.Language, "manual"
		if track.Kind == "asr" {
			captions = "auto"
		}
	}

	stop := math.Min(end, heardTo)
	within := func(paras []transcript.Cue) []transcript.Cue {
		var out []transcript.Cue
		for _, p := range paras {
			if p.Start < stop && (p.End > start || p.Start >= start) {
				out = append(out, p)
			}
		}
		return out
	}
	covered := stop
	if math.IsInf(stop, 1) {
		covered = start
		if duration > 0 {
			covered = duration
		} else if all := said.paragraphs(); len(all) > 0 {
			covered = all[len(all)-1].End
		}
	}
	budget := c.Settings.TranscriptChars
	searched := rustle.TrimSpace(args.Query) != ""
	// Chapters come once, with the first page; a search returns only its matches.
	var chapters []rustle.Chapter
	if start == 0 && !searched && len(record.Chapters) > 0 {
		chapters = record.Chapters
		budget -= transcript.Cost(rustle.Compact(chapters))
	}
	out := &spoken{}
	next, hasNext := 0.0, false
	matches, hasMatches := 0, false
	digested := !searched && args.Detail == "digest"
	if searched {
		found, count, notes := transcript.Matches(within(said.sentences()), args.Query, budget)
		for _, m := range found {
			out.lines = append(out.lines, m.Text)
			out.at = append(out.at, m.At)
		}
		if count == 0 {
			out.empty = transcript.NoMatch(args.Query)
		}
		matches, hasMatches = count, true
		c.Notes.Add(notes...)
	} else {
		var units []transcript.Cue
		switch {
		case digested:
			units = transcript.Digest(within(said.sentences()), record.Chapters, 0, args.Breaks...)
		case len(args.Breaks) > 0:
			// Shown with frames from start, speech begun before it would sit under the wrong one.
			for _, p := range within(transcript.Paragraphs(said.cues, transcript.MinParagraph, transcript.MaxParagraph, append([]float64{start}, args.Breaks...)...)) {
				if p.Start >= start {
					units = append(units, p)
				}
			}
		default:
			units = within(said.paragraphs())
		}
		cut := transcript.Page(units, budget)
		next, hasNext = cut.Next, cut.HasNext
		if hasNext {
			covered = next
		}
		if cut.HasCut {
			c.Notes.Add("the paragraph at " + rustle.Clock(cut.Cut) + " was cut at the page size limit")
		}
		// Only a cut the caller didn't ask for gets a continuation: a deliberate end_seconds
		// is the whole answer, and offering more would invite a call nobody needs.
		limit := end
		if duration > 0 {
			limit = math.Min(limit, duration)
		}
		if !hasNext && !math.IsInf(heardTo, 1) && heardTo < limit {
			next, hasNext = rustle.MS(heardTo), true
			c.Notes.Add("transcribed up to " + rustle.Clock(heardTo) + "; call again with start_seconds=" + rustle.Short(heardTo) + " and transcribe=true for more")
		}
		out.lines, out.at = cut.Lines, cut.Starts
		if len(cut.Lines) == 0 {
			out.empty = transcript.Silence(start, covered)
		}
	}
	meta := rustle.NewObj().Set("title", record.Title)
	if duration > 0 {
		meta.Set("duration", duration)
	}
	if language != "" {
		meta.Set("language", language)
	}
	meta.Set("captions", captions).Set("span", []float64{start, covered})
	if hasNext {
		meta.Set("next_start_seconds", next)
	}
	if hasMatches {
		meta.Set("matches", matches)
	}
	if digested {
		meta.Set("digest", "… = skipped speech; query or detail=full for exact words")
	}
	if chapters != nil {
		meta.Set("chapters", chapters)
	}
	if hasNext {
		out.next = &next
	}
	out.meta = meta
	return out, nil
}

// speech is a parsed caption track. Digests and searches use sentences, so the 20-40 s
// paragraphs are built only when read.
type speech struct {
	cues      []transcript.Cue
	readOnce  sync.Once
	read      []transcript.Cue
	splitOnce sync.Once
	split     []transcript.Cue
}

func (s *speech) paragraphs() []transcript.Cue {
	s.readOnce.Do(func() { s.read = transcript.Paragraphs(s.cues, transcript.MinParagraph, transcript.MaxParagraph) })
	return s.read
}

func (s *speech) sentences() []transcript.Cue {
	s.splitOnce.Do(func() { s.split = transcript.Paragraphs(s.cues, 0, maxSentence) })
	return s.split
}

// parsed keeps captions parsed once per text, so paging or searching a transcript again
// takes about a millisecond.
var parsed = struct {
	sync.Mutex
	byText map[string]*speech
	order  []string
}{byText: map[string]*speech{}}

func speechOf(raw, label string) (*speech, error) {
	parsed.Lock()
	known := parsed.byText[raw]
	parsed.Unlock()
	if known != nil {
		return known, nil
	}
	cues, err := transcript.Parse(raw, label)
	if err != nil {
		return nil, err
	}
	s := &speech{cues: transcript.DropRepeats(cues)}
	parsed.Lock()
	defer parsed.Unlock()
	if _, ok := parsed.byText[raw]; !ok {
		parsed.order = append(parsed.order, raw)
	}
	parsed.byText[raw] = s
	if len(parsed.order) > 8 {
		delete(parsed.byText, parsed.order[0])
		parsed.order = parsed.order[1:]
	}
	return s, nil
}

// readText reads a sidecar: UTF-8, or UTF-16 when saved by Windows tools, which mark it
// with a byte-order mark.
func readText(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if len(data) >= 2 && data[0] == 0xff && data[1] == 0xfe {
		units := make([]uint16, len(data)/2)
		for i := range units {
			units[i] = binary.LittleEndian.Uint16(data[2*i:])
		}
		return string(utf16.Decode(units)), nil
	}
	return strings.ToValidUTF8(string(data), "�"), nil
}

// SpeechAvailable reports whether transcribe=true can work here, so the tool only offers it then.
func SpeechAvailable() bool {
	cli, model := whisperInstalled()
	return cli != "" && model != ""
}

// whisperInstalled is whisper.cpp's command line tool and a ggml model, "" for what is missing.
func whisperInstalled() (cli, model string) {
	model = os.Getenv("RUSTLE_WHISPER_MODEL")
	if model != "" {
		if _, err := os.Stat(model); err != nil {
			model = ""
		}
	}
	return proc.Which("whisper-cli"), model
}

func noCaptions(record *rustle.Record) *rustle.Error {
	// Advise transcribe=true only when it can work; otherwise the retry costs a call and
	// fails with SPEECH_UNAVAILABLE.
	recovery := "Transcription isn't available on this server. " + speechInstall
	if SpeechAvailable() {
		recovery = "Retry with transcribe=true to transcribe the speech locally (up to " + strconv.Itoa(WhisperSeconds) + " s per call)."
	}
	if record.Kind == "local" {
		recovery += " Or put an .srt or .vtt file with the same name next to the media file."
	}
	if record.CaptionsError {
		return rustle.Fail("YouTube's caption list could not be read for this video.", "NO_CAPTIONS", "Retry shortly. If it recurs: "+recovery).Retry(0)
	}
	where := "for this video"
	if record.Kind == "local" {
		where = "next to this file"
	}
	return rustle.Fail("No captions "+where+".", "NO_CAPTIONS", recovery)
}

// WhisperArgs is the whisper-cli command line.
func WhisperArgs(model, wav, out, language string) []string {
	threads := max(1, min(8, runtime.NumCPU()-1))
	lang := transcript.Primary(language)
	if lang == "" {
		lang = "auto"
	}
	return []string{"-m", model, "-f", wav, "-of", out, "-oj", "-np", "-l", lang, "-t", strconv.Itoa(threads)}
}

// WAV is 16-bit mono PCM WAV, the input whisper.cpp reads.
func WAV(samples []float32, rate int) []byte {
	out := make([]byte, 44+len(samples)*2)
	copy(out, "RIFF")
	binary.LittleEndian.PutUint32(out[4:], uint32(36+len(samples)*2))
	copy(out[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(out[16:], 16)
	binary.LittleEndian.PutUint16(out[20:], 1)
	binary.LittleEndian.PutUint16(out[22:], 1)
	binary.LittleEndian.PutUint32(out[24:], uint32(rate))
	binary.LittleEndian.PutUint32(out[28:], uint32(rate*2))
	binary.LittleEndian.PutUint16(out[32:], 2)
	binary.LittleEndian.PutUint16(out[34:], 16)
	copy(out[36:], "data")
	binary.LittleEndian.PutUint32(out[40:], uint32(len(samples)*2))
	for i, s := range samples {
		v := math.Max(-1, math.Min(1, float64(s)))
		binary.LittleEndian.PutUint16(out[44+i*2:], uint16(int16(rustle.Round(float64(v*32767)))))
	}
	return out
}

type heardCue struct {
	Start, End float64
	Text       string
}

func (h heardCue) MarshalJSON() ([]byte, error) { return json.Marshal([]any{h.Start, h.End, h.Text}) }

func (h *heardCue) UnmarshalJSON(data []byte) error {
	var parts []json.RawMessage
	if err := json.Unmarshal(data, &parts); err != nil || len(parts) != 3 {
		return err
	}
	if err := json.Unmarshal(parts[0], &h.Start); err != nil {
		return err
	}
	if err := json.Unmarshal(parts[1], &h.End); err != nil {
		return err
	}
	return json.Unmarshal(parts[2], &h.Text)
}

type heard struct {
	Language string     `json:"language,omitempty"`
	Cues     []heardCue `json:"cues"`
}

type transcribed struct {
	cues     []transcript.Cue
	language string
	stop     float64
	duration float64
}

// whisper transcribes up to WhisperSeconds from start with whisper.cpp; cached per span and model.
func whisper(c *rustle.Call, record *rustle.Record, start, end float64, language string) (*transcribed, error) {
	cli, model := whisperInstalled()
	if cli == "" || model == "" {
		message := "Local transcription is not installed."
		if cli != "" {
			message = "Local transcription needs a whisper.cpp model."
		}
		return nil, rustle.Fail(message, "SPEECH_UNAVAILABLE", speechInstall)
	}
	if record.Kind != "youtube" {
		var err error
		if record, err = source.Probed(c, record); err != nil {
			return nil, err
		}
	}
	limit := end
	if record.Duration > 0 {
		limit = math.Min(limit, record.Duration)
	}
	if start >= limit {
		return nil, rustle.Fail("start_seconds is at or past the end of the media ("+rustle.Clock(limit)+").", "BAD_INTERVAL", "Pass a start_seconds inside the media.")
	}
	stop := math.Min(limit, start+WhisperSeconds)
	spoken := language
	if spoken == "" {
		spoken = record.OriginalLanguage
	}
	var spokenKey any
	if spoken != "" {
		spokenKey = spoken
	}
	name := "whisper-" + rustle.HashKey(rustle.Compact([]any{"whisper.cpp-v1", filepath.Base(model), spokenKey, rustle.MS(start), rustle.MS(stop)})) + ".json"
	var known heard
	if !c.Cache.ReadJSON(record.Key, name, &known) {
		input, err := source.MediaInput(c, record, "audio", start, stop)
		if err != nil {
			return nil, err
		}
		pcm, err := media.DecodePCM(c.Ctx, input.Input, start, stop, c.Settings, media.PCMOptions{Rate: 16000, Mono: true, Offset: input.Offset})
		if err != nil {
			return nil, err
		}
		dir, err := os.MkdirTemp(rustle.TempDir(), "rustle-whisper-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(dir)
		audio := filepath.Join(dir, "audio.wav")
		if err := os.WriteFile(audio, WAV(pcm.Channels[0], pcm.Rate), 0o600); err != nil {
			return nil, err
		}
		out := filepath.Join(dir, "out")
		// Local transcription may take up to 600 s, as long as the audio itself.
		result, err := proc.Run(c.Ctx, cli, WhisperArgs(model, audio, out, spoken), proc.Options{
			Timeout: max(c.Settings.Timeout, 600*time.Second), MaxBytes: 64 << 20,
		})
		if err != nil {
			return nil, err
		}
		var data struct {
			Result struct {
				Language string `json:"language"`
			} `json:"result"`
			Transcription []struct {
				Offsets struct {
					From *float64 `json:"from"`
					To   *float64 `json:"to"`
				} `json:"offsets"`
				Text any `json:"text"`
			} `json:"transcription"`
		}
		raw, readErr := os.ReadFile(out + ".json")
		if result.Code != 0 || readErr != nil || json.Unmarshal(raw, &data) != nil || data.Transcription == nil {
			return nil, rustle.Fail("whisper.cpp failed to transcribe this audio.", "SPEECH_FAILED",
				"Check that RUSTLE_WHISPER_MODEL names a ggml model file whisper-cli can load, then retry.")
		}
		known = heard{Language: data.Result.Language, Cues: []heardCue{}}
		if known.Language == "" {
			known.Language = transcript.Primary(spoken)
		}
		for _, s := range data.Transcription {
			if s.Offsets.From == nil || s.Offsets.To == nil {
				continue
			}
			text := ""
			if s.Text != nil {
				text = jsonString(s.Text)
			}
			known.Cues = append(known.Cues, heardCue{start + *s.Offsets.From/1000, start + *s.Offsets.To/1000, text})
		}
		_ = c.Cache.WriteJSON(record.Key, name, known)
	}
	var cues []transcript.Cue
	for _, h := range known.Cues {
		if text := transcript.Clean(h.Text); text != "" {
			cues = append(cues, transcript.Cue{Start: rustle.MS(h.Start), End: rustle.MS(h.End), Text: text})
		}
	}
	return &transcribed{cues: cues, language: known.Language, stop: stop, duration: record.Duration}, nil
}

// jsonString is String(value) for a value whisper wrote.
func jsonString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return rustle.NumString(x)
	case bool:
		return strconv.FormatBool(x)
	}
	return ""
}
