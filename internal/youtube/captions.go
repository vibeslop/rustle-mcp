package youtube

import (
	"strings"

	"github.com/vibeslop/rustle-mcp/internal/jsonv"
	"github.com/vibeslop/rustle-mcp/internal/rustle"
	"github.com/vibeslop/rustle-mcp/internal/transcript"
)

// TextOf reads simpleText, runs or content, the three ways YouTube spells a string.
func TextOf(value any) string {
	o := jsonv.Obj(value)
	if o == nil {
		s, _ := value.(string)
		return s
	}
	if s, ok := o.Get("content").(string); ok {
		return s
	}
	if s, ok := o.Get("simpleText").(string); ok {
		return s
	}
	runs := jsonv.Arr(o.Get("runs"))
	if runs == nil {
		return ""
	}
	var b strings.Builder
	for _, r := range runs {
		if s, ok := jsonv.Get(r, "text").(string); ok {
			b.WriteString(s)
		}
	}
	return b.String()
}

// OriginalLanguage is the language spoken in the uploaded audio, or "" when YouTube does
// not say.
//
// Auto-dubbed videos list an auto-generated (ASR) track for every dubbed audio language,
// alphabetically, so the first ASR track is often Arabic or Bengali. The audio tracks name
// the original instead: its id ends in ".4" ("en-US.4") while auto dubs end in ".10", and
// the matching stream's display name says "original".
func OriginalLanguage(renderer any, formats any) string {
	tracks := jsonv.Objects(jsonv.Get(renderer, "captionTracks"))
	audio := jsonv.Objects(jsonv.Get(renderer, "audioTracks"))
	spoken, found := "", false
	for _, item := range audio {
		id := jsonv.String(item.Get("audioTrackId"))
		if strings.HasSuffix(id, ".4") {
			spoken, found = id[:strings.LastIndex(id, ".")], true
			break
		}
	}
	if !found {
		for _, format := range jsonv.Objects(formats) {
			track := jsonv.Obj(format.Get("audioTrack"))
			if track != nil && jsonv.Truthy(track.Get("id")) && strings.Contains(strings.ToLower(jsonv.String(track.Get("displayName"))), "original") {
				spoken = strings.Split(jsonv.String(track.Get("id")), ".")[0]
				break
			}
		}
	}
	var asr []*jsonv.Object
	for _, t := range tracks {
		if jsonv.String(t.Get("kind")) == "asr" && jsonv.Truthy(t.Get("languageCode")) {
			asr = append(asr, t)
		}
	}
	if spoken != "" {
		// Report the caption list's spelling ("en", not "en-US") so it matches a track.
		for _, group := range [][]*jsonv.Object{asr, tracks} {
			for _, track := range group {
				if transcript.Primary(jsonv.String(track.Get("languageCode"))) == transcript.Primary(spoken) {
					return jsonv.String(track.Get("languageCode"))
				}
			}
		}
		return spoken
	}
	// YouTube runs speech recognition only on the uploaded audio.
	languages := map[string]bool{}
	for _, t := range asr {
		languages[transcript.Primary(jsonv.String(t.Get("languageCode")))] = true
	}
	if len(languages) == 1 {
		return jsonv.String(asr[0].Get("languageCode"))
	}
	if len(asr) > 0 && len(audio) > 0 {
		// Without audio track ids, the default audio track's caption order still starts with
		// the original-language track on the dubbed videos observed.
		chosen := audio[0]
		if index, ok := wholeNumber(jsonv.Get(renderer, "defaultAudioTrackIndex")); ok && index < len(audio) {
			chosen = audio[index]
		}
		for _, position := range jsonv.Arr(chosen.Get("captionTrackIndices")) {
			if p, ok := wholeNumber(position); ok && p < len(tracks) && jsonv.String(tracks[p].Get("kind")) == "asr" {
				return jsonv.String(tracks[p].Get("languageCode"))
			}
		}
	}
	return ""
}

// wholeNumber is v as a non-negative integer, as Number.isInteger(v) && v >= 0 accepts it.
func wholeNumber(v any) (int, bool) {
	n, ok := v.(float64)
	if !ok || n < 0 || n != float64(int(n)) {
		return 0, false
	}
	return int(n), true
}

// CaptionTracks is the caption tracks of a player response, marking the original language's.
func CaptionTracks(raw any, original string) []rustle.Caption {
	tracks := []rustle.Caption{}
	for _, item := range jsonv.Objects(raw) {
		if !jsonv.Truthy(item.Get("baseUrl")) {
			continue
		}
		language := jsonv.String(item.Get("languageCode"))
		// vssId is ".en", "a.en" or ".en.<name id>" for named tracks.
		if language == "" {
			vss := strings.TrimPrefix(jsonv.String(item.Get("vssId")), "a.")
			language = strings.Split(strings.Trim(vss, "."), ".")[0]
		}
		kind := "manual"
		if jsonv.String(item.Get("kind")) == "asr" {
			kind = "asr"
		}
		tracks = append(tracks, rustle.Caption{
			Language: language,
			Name:     rustle.Cut(TextOf(item.Get("name")), 200),
			Kind:     kind,
			URL:      jsonv.String(item.Get("baseUrl")),
			Original: original != "" && transcript.Primary(language) == transcript.Primary(original),
		})
	}
	return tracks
}

// timestamp finds a description line's first time, as
// /(?<![\d:.])(?:(\d{1,2}):)?(\d{1,2}):([0-5]\d)(?![\d:])/ does: not inside a longer
// number or time, hours optional. It reports where the match starts and ends.
func timestamp(line string) (seconds, start, end int, ok bool) {
	digit := func(i int) bool { return i >= 0 && i < len(line) && line[i] >= '0' && line[i] <= '9' }
	value := func(from, to int) int {
		n := 0
		for _, c := range line[from:to] {
			n = n*10 + int(c-'0')
		}
		return n
	}
	// minutesAt matches (\d{1,2}):([0-5]\d)(?![\d:]) at i, longest first.
	minutesAt := func(i int) (int, int, bool) {
		for width := 2; width >= 1; width-- {
			colon := i + width
			ok := true
			for k := i; k < colon; k++ {
				ok = ok && digit(k)
			}
			if !ok || colon >= len(line) || line[colon] != ':' {
				continue
			}
			s := colon + 1
			if !(digit(s) && line[s] <= '5' && digit(s+1)) {
				continue
			}
			after := s + 2
			if after < len(line) && (digit(after) || line[after] == ':') {
				continue
			}
			return value(i, colon)*60 + value(s, s+2), after, true
		}
		return 0, 0, false
	}
	for i := 0; i < len(line); i++ {
		if i > 0 && (digit(i-1) || line[i-1] == ':' || line[i-1] == '.') {
			continue
		}
		// With hours first, longest first, then without.
		for width := 2; width >= 1; width-- {
			colon := i + width
			ok := true
			for k := i; k < colon; k++ {
				ok = ok && digit(k)
			}
			if !ok || colon >= len(line) || line[colon] != ':' {
				continue
			}
			if m, after, found := minutesAt(colon + 1); found {
				return value(i, colon)*3600 + m, i, after, true
			}
		}
		if m, after, found := minutesAt(i); found {
			return m, i, after, true
		}
	}
	return 0, 0, 0, false
}

// chapterEdge is the characters trimmed from a chapter title's ends.
func chapterEdge(r rune) bool {
	return rustle.IsSpace(r) || strings.ContainsRune("-–—:|•·()[]", r)
}

// DescriptionChapters reads chapters from a description's timestamp list, under YouTube's
// own rules: YouTube shows chapters only when the list starts at 0:00 with at least three
// ascending timestamps, and the same rule skips times mentioned in prose. duration 0 is unknown.
func DescriptionChapters(description string, duration float64) []rustle.Chapter {
	var found []rustle.Chapter
	for _, line := range strings.Split(description, "\n") {
		line = strings.TrimSuffix(line, "\r")
		at, start, end, ok := timestamp(line)
		if !ok {
			continue
		}
		title := strings.TrimFunc(rustle.CollapseSpace(line[:start]+" "+line[end:]), chapterEdge)
		found = append(found, rustle.Chapter{At: float64(at), Title: rustle.Cut(title, 200)})
	}
	for begin := range found {
		if found[begin].At != 0 {
			continue
		}
		run := []rustle.Chapter{found[begin]}
		for _, item := range found[begin+1:] {
			if item.At <= run[len(run)-1].At || (duration > 0 && item.At >= duration) {
				break
			}
			run = append(run, item)
		}
		if len(run) >= 3 {
			return run
		}
	}
	return []rustle.Chapter{}
}

// MarkerChapters reads the chapter markers in a watch page's initial data, YouTube's
// automatic chapters included.
func MarkerChapters(data any) []rustle.Chapter {
	bar := jsonv.Get(data, "playerOverlays", "playerOverlayRenderer", "decoratedPlayerBarRenderer", "decoratedPlayerBarRenderer",
		"playerBar", "multiMarkersPlayerBarRenderer")
	for _, entry := range jsonv.Objects(jsonv.Get(bar, "markersMap")) {
		var chapters []rustle.Chapter
		for _, item := range jsonv.Objects(jsonv.Get(entry.Get("value"), "chapters")) {
			chapter := item.Get("chapterRenderer")
			ms := rustle.Number(jsonv.Get(chapter, "timeRangeStartMillis"))
			if rustle.Finite(ms) {
				chapters = append(chapters, rustle.Chapter{At: ms / 1000, Title: rustle.Cut(rustle.TrimSpace(TextOf(jsonv.Get(chapter, "title"))), 200)})
			}
		}
		if len(chapters) > 0 {
			return chapters
		}
	}
	return []rustle.Chapter{}
}
