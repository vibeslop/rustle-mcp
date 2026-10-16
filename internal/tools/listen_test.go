package tools

import (
	"encoding/base64"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/vibeslop/rustle-mcp/internal/rustle"
	"github.com/vibeslop/rustle-mcp/internal/testmedia"
)

// listenSetup writes the beep (6 s of stereo: silence on the left, a 1 kHz tone on the
// right from 3 s to 3.25 s) and returns its path with settings whose roots hold it and the
// silent inspection video.
func listenSetup(t *testing.T) (string, *rustle.Settings) {
	t.Helper()
	s := testSettings(t)
	needFFmpeg(t, s)
	dir := tempDir(t)
	beep := filepath.Join(dir, "beep.wav")
	if err := testmedia.WriteBeep(beep); err != nil {
		t.Fatal(err)
	}
	s.MediaRoots = []string{dir, rustle.Real(filepath.Dir(testmedia.Inspection()))}
	return beep, s
}

func listenOK(t *testing.T, c *rustle.Call, args ListenArgs) *rustle.Result {
	t.Helper()
	result, err := listen(c, args)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestListenAnalysesTheBeepFixture(t *testing.T) {
	// Right-heavy, one onset at 3 s, silence before it.
	beep, s := listenSetup(t)
	c := newCall(t, s, "claude-code")
	result := listenOK(t, c, ListenArgs{Source: beep, Start: 0, Seconds: 20, Output: "analysis"})
	equal(t, rustle.Compact(result.Meta),
		`{"span":[0,6],"peak_dbfs":[-120,-6],"rms_dbfs":[-120,-23],"balance_db":-97,"onsets":[3.01],"silence":[[0,3],[3.25,6]]}`)
	equal(t, c.Notes.List(), []string{})
	block := result.Content[0]
	equal(t, block.Type, "image")
	equal(t, block.Meta, map[string]any(nil))
	codex := listenOK(t, newCall(t, s, "codex-cli"), ListenArgs{Source: beep, Start: 2, Seconds: 1, Output: "analysis"})
	equal(t, codex.Content[0].Meta, map[string]any{"codex/imageDetail": "original"})
	equal(t, metaField(codex.Meta, "span"), any([]float64{2, 3}))
	equal(t, metaField(codex.Meta, "next_start_seconds"), any(3.0))
}

func TestListenReturnsLosslessFLACForClientsThatHearAudio(t *testing.T) {
	beep, s := listenSetup(t)
	result := listenOK(t, newCall(t, s, "codex"), ListenArgs{Source: beep, Start: 2.5, Seconds: 1, Output: "audio"})
	block := result.Content[0]
	equal(t, []string{block.Type, block.MimeType}, []string{"audio", "audio/flac"})
	flac, err := base64.StdEncoding.DecodeString(block.Data)
	if err != nil || len(flac) < 4 {
		t.Fatalf("%d bytes, %v", len(flac), err)
	}
	equal(t, string(flac[:4]), "fLaC")
	equal(t, metaField(result.Meta, "span"), any([]float64{2.5, 3.5}))
	equal(t, metaField(result.Meta, "onsets"), any([]float64{3.01}))
}

func TestListenPastTheEndNamesTheSpanItUsed(t *testing.T) {
	// A silent video has no audio track.
	beep, s := listenSetup(t)
	c := newCall(t, s, "claude-code")
	result := listenOK(t, c, ListenArgs{Source: beep, Start: 50, Seconds: 20, Output: "analysis"})
	equal(t, metaField(result.Meta, "span"), any([]float64{0, 6}))
	equal(t, c.Notes.List(), []string{"start_seconds 50 is past the end (6 s); used 0-6"})
	_, err := listen(newCall(t, s, "claude-code"), ListenArgs{Source: testmedia.Inspection(), Start: 0, Seconds: 5, Output: "analysis"})
	equal(t, failure(t, err).Code, "NO_AUDIO_TRACK")
}

func TestFLACOverOneAudioBlockIsShortenedFromTheEnd(t *testing.T) {
	// With a place to continue.
	_, s := listenSetup(t)
	tools := needFFmpeg(t, s)
	noise := filepath.Join(s.MediaRoots[0], "noise.wav")
	// Independent 24-bit white noise in each channel doesn't compress: 32 s is about 9.2 MB of FLAC.
	if out, err := exec.Command(tools.FFmpeg, "-hide_banner", "-loglevel", "error", "-filter_complex",
		"anoisesrc=d=32:a=1:r=48000:seed=1[a];anoisesrc=d=32:a=1:r=48000:seed=2[b];[a][b]amerge=inputs=2",
		"-c:a", "pcm_s24le", noise).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, out)
	}
	c := newCall(t, s, "codex")
	result := listenOK(t, c, ListenArgs{Source: noise, Start: 0, Seconds: 32, Output: "audio"})
	flac, err := base64.StdEncoding.DecodeString(result.Content[0].Data)
	if err != nil {
		t.Fatal(err)
	}
	bytes := len(flac)
	if bytes > AudioBlockBytes || float64(bytes) <= AudioBlockBytes*0.8 {
		t.Errorf("%d bytes of FLAC", bytes)
	}
	span, _ := metaField(result.Meta, "span").([]float64)
	if len(span) != 2 || span[0] != 0 || span[1] <= 20 || span[1] >= 32 {
		t.Errorf("span %v", span)
	}
	equal(t, metaField(result.Meta, "next_start_seconds"), any(span[1]))
	equal(t, c.Notes.List(), []string{"shortened to fit one audio block; continue from next_start_seconds"})
}
