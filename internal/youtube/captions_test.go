package youtube

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/vibeslop/rustle-mcp/internal/jsonv"
	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

// Caption fixtures: trimmed copies of real player responses.

// captionRenderer is a playerCaptionsTracklistRenderer; tracks are [vssId, language, name].
func captionRenderer(tracks [][3]string, audio []M, defaultIndex int) M {
	captionTracks := A{}
	for _, track := range tracks {
		item := M{
			"baseUrl": "https://www.youtube.com/api/timedtext?v=x&lang=" + track[1], "name": M{"simpleText": track[2]},
			"vssId": track[0], "languageCode": track[1], "isTranslatable": true, "trackName": "",
		}
		if strings.HasPrefix(track[0], "a.") {
			item["kind"] = "asr"
		}
		captionTracks = append(captionTracks, item)
	}
	audioTracks := A{}
	for _, a := range audio {
		audioTracks = append(audioTracks, a)
	}
	return M{"captionTracks": captionTracks, "audioTracks": audioTracks, "defaultAudioTrackIndex": defaultIndex, "translationLanguages": A{}}
}

func asrTracks(codes []string) [][3]string {
	var out [][3]string
	for _, c := range codes {
		out = append(out, [3]string{"a." + c, c, c + " (auto-generated)"})
	}
	return out
}

func dubbedRenderer(codes, audioIDs []string, defaultIndex int) M {
	first := []int{slices.Index(codes, "en")}
	for i, c := range codes {
		if c != "en" {
			first = append(first, i)
		}
	}
	var audio []M
	for _, id := range audioIDs {
		audio = append(audio, M{"captionTrackIndices": first, "audioTrackId": id})
	}
	return captionRenderer(asrTracks(codes), audio, defaultIndex)
}

// Auto-dubbed English videos (kREHuzsZt70, hojwx9Hxz9w) list an ASR track per dub language, alphabetically.
var (
	dubbed21      = []string{"ar", "bn", "nl-NL", "en", "fr-FR", "de-DE", "iw", "hi", "id", "it", "ja", "ko", "ml", "pl", "pt-BR", "pa", "ru", "es-US", "ta", "te", "uk"}
	dubbed21Audio = []string{"pt-BR.10", "fr-FR.10", "ko.10", "ml.10", "nl-NL.10", "pl.10", "ta.10", "it.10", "te.10", "uk.10", "es-US.10", "bn.10", "ar.10", "de-DE.10", "hi.10", "ru.10", "id.10", "iw.10", "en-US.4", "pa.10", "ja.10"}

	kreh = dubbedRenderer(dubbed21, dubbed21Audio, 18)
	aiot = dubbedRenderer(dubbed21[1:], slices.DeleteFunc(slices.Clone(dubbed21Audio), func(a string) bool { return a == "ar.10" }), 18)
	ghfj = dubbedRenderer([]string{"nl-NL", "en", "fr-FR", "hi", "id", "es-US", "uk"}, []string{"id.10", "hi.10", "es-US.10", "fr-FR.10", "nl-NL.10", "en-US.4"}, 5)

	paella = captionRenderer([][3]string{{".es", "es", "Spanish"}, {"a.es", "es", "Spanish (auto-generated)"}},
		[]M{{"captionTrackIndices": []int{0, 1}, "audioTrackId": "en-US.10"}, {"captionTrackIndices": []int{0, 1}, "audioTrackId": "es-US.4"}}, 1)
	subtitled = captionRenderer([][3]string{{".en", "en", "English"}, {"a.es", "es", "Spanish (auto-generated)"}},
		[]M{{"captionTrackIndices": []int{0, 1}, "defaultCaptionTrackIndex": 0, "hasDefaultTrack": true}}, 0)
	rick = captionRenderer([][3]string{
		{".en", "en", "English"}, {"a.en", "en", "English (auto-generated)"}, {".de-DE", "de-DE", "German (Germany)"},
		{".ja", "ja", "Japanese"}, {".pt-BR", "pt-BR", "Portuguese (Brazil)"}, {".es-419", "es-419", "Spanish (Latin America)"},
	}, []M{{"captionTrackIndices": []int{0, 2, 3, 4, 5, 1}, "defaultCaptionTrackIndex": 0}}, 0)
	single = captionRenderer(asrTracks([]string{"en"}), []M{{"captionTrackIndices": []int{0}}}, 0)
)

func tracksOf(t *testing.T, data M, formats A) (string, []rustle.Caption) {
	t.Helper()
	if formats == nil {
		formats = A{}
	}
	renderer := parse(t, data)
	original := OriginalLanguage(renderer, parse(t, formats))
	return original, CaptionTracks(jsonv.Get(renderer, "captionTracks"), original)
}

func TestCaptions(t *testing.T) {
	t.Run("auto-dubbed videos resolve to the original audio language", func(t *testing.T) {
		for _, c := range []struct {
			data  M
			first string
		}{{kreh, "ar"}, {aiot, "bn"}, {ghfj, "nl-NL"}} {
			original, tracks := tracksOf(t, c.data, nil)
			if tracks[0].Language != c.first {
				t.Errorf("first track %q, want %q", tracks[0].Language, c.first)
			}
			if original != "en" {
				t.Errorf("original %q, want en", original)
			}
			var marked []string
			for _, track := range tracks {
				if track.Original {
					marked = append(marked, track.Language)
				}
				if track.Kind != "asr" {
					t.Errorf("track %s is %s, want asr", track.Language, track.Kind)
				}
			}
			equal(t, marked, []string{"en"}, "tracks marked original")
		}
	})

	t.Run("the track list is never truncated", func(t *testing.T) {
		codes := slices.Clone(dubbed21)
		for i := range 30 {
			codes = append(codes, fmt.Sprintf("x%d", i))
		}
		if _, tracks := tracksOf(t, dubbedRenderer(codes, dubbed21Audio, 18), nil); len(tracks) != 51 {
			t.Errorf("%d tracks, want 51", len(tracks))
		}
	})

	t.Run("a dub into English keeps the Spanish original", func(t *testing.T) {
		original, tracks := tracksOf(t, paella, nil)
		if original != "es" {
			t.Errorf("original %q, want es", original)
		}
		var got [][3]any
		for _, track := range tracks {
			got = append(got, [3]any{track.Language, track.Kind, track.Original})
		}
		equal(t, got, [][3]any{{"es", "manual", true}, {"es", "asr", true}}, "tracks")
	})

	t.Run("a single ASR track names the spoken language", func(t *testing.T) {
		original, tracks := tracksOf(t, single, nil)
		if original != "en" {
			t.Errorf("original %q, want en", original)
		}
		equal(t, tracks, []rustle.Caption{{
			Language: "en", Name: "en (auto-generated)", Kind: "asr", URL: "https://www.youtube.com/api/timedtext?v=x&lang=en", Original: true,
		}}, "tracks")
	})

	t.Run("human captions and subtitles keep the speech language", func(t *testing.T) {
		if original, _ := tracksOf(t, rick, nil); original != "en" {
			t.Errorf("original %q, want en", original)
		}
		original, tracks := tracksOf(t, subtitled, nil)
		if original != "es" {
			t.Errorf("original %q, want es", original)
		}
		var got [][2]any
		for _, track := range tracks {
			got = append(got, [2]any{track.Language, track.Original})
		}
		equal(t, got, [][2]any{{"en", false}, {"es", true}}, "tracks")
	})

	t.Run("stream names give the original when tracks lack ids", func(t *testing.T) {
		data := captionRenderer(asrTracks([]string{"ar", "en", "fr-FR"}),
			[]M{{"captionTrackIndices": []int{0, 1, 2}}, {"captionTrackIndices": []int{0, 1, 2}}}, 0)
		formats := A{
			M{"audioTrack": M{"displayName": "Arabic", "id": "ar.10", "isAutoDubbed": true}},
			M{"audioTrack": M{"displayName": "English (US) original", "id": "en-US.4"}},
		}
		if original := OriginalLanguage(parse(t, data), parse(t, formats)); original != "en" {
			t.Errorf("original %q, want en", original)
		}
	})

	t.Run("an unknown original marks no track", func(t *testing.T) {
		original, tracks := tracksOf(t, captionRenderer(asrTracks([]string{"ar", "bn", "en"}), nil, 0), nil)
		if original != "" {
			t.Errorf("original %q, want none", original)
		}
		for _, track := range tracks {
			if track.Original {
				t.Errorf("track %s is marked original", track.Language)
			}
		}
	})

	t.Run("languages fall back to the vssId and names to runs", func(t *testing.T) {
		tracks := CaptionTracks(parse(t, A{
			M{"baseUrl": "u1", "vssId": ".en.nP7-2PuUl7o", "name": M{"runs": A{M{"text": "English"}, M{"text": " - CC"}}}},
			M{"baseUrl": "u2", "vssId": "a.de", "kind": "asr"},
			M{"vssId": ".fr"},
		}), "de")
		var got [][4]any
		for _, track := range tracks {
			got = append(got, [4]any{track.Language, track.Name, track.Kind, track.Original})
		}
		equal(t, got, [][4]any{{"en", "English - CC", "manual", false}, {"de", "", "asr", true}}, "tracks")
	})

	t.Run("description chapters follow YouTube rules", func(t *testing.T) {
		description := "Released at 12:30 today.\n\n🔖 Chapters:\n0:00 Intro\n1:00 1. Flour\n" +
			"2:34 2. Starter\n(4:15) 3. A Second Rise\nFinal Thoughts - 12:19\n1:00:05 After the end\n\n#baking"
		equal(t, DescriptionChapters(description, 768), []rustle.Chapter{
			{At: 0, Title: "Intro"}, {At: 60, Title: "1. Flour"}, {At: 154, Title: "2. Starter"}, {At: 255, Title: "3. A Second Rise"}, {At: 739, Title: "Final Thoughts"},
		}, "chapters")
		for _, prose := range []string{"0:00 Intro\n5:00 End", "See 3:10 and 0:00 then 1:00\n2:00 x"} {
			if found := DescriptionChapters(prose, 0); len(found) != 0 {
				t.Errorf("%q: chapters %v, want none", prose, found)
			}
		}
		equal(t, DescriptionChapters("00:00 A\r\n01:02:03 B\r\n1:02:04 C", 0), []rustle.Chapter{
			{At: 0, Title: "A"}, {At: 3723, Title: "B"}, {At: 3724, Title: "C"},
		}, "chapters with hours and CRLF")
	})

	t.Run("watch page markers become chapters", func(t *testing.T) {
		chapter := func(title string, ms float64) M {
			return M{"chapterRenderer": M{"title": M{"simpleText": title}, "timeRangeStartMillis": ms}}
		}
		data := M{"playerOverlays": M{"playerOverlayRenderer": M{"decoratedPlayerBarRenderer": M{"decoratedPlayerBarRenderer": M{"playerBar": M{
			"multiMarkersPlayerBarRenderer": M{"markersMap": A{M{"key": "AUTO_CHAPTERS", "value": M{"chapters": A{chapter("Start", 0), chapter("Auto chapter", 90500)}}}}},
		}}}}}}
		equal(t, MarkerChapters(parse(t, data)), []rustle.Chapter{{At: 0, Title: "Start"}, {At: 90.5, Title: "Auto chapter"}}, "chapters")
		if found := MarkerChapters(parse(t, M{})); len(found) != 0 {
			t.Errorf("chapters %v from an empty page, want none", found)
		}
	})
}
