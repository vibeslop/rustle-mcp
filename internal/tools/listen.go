package tools

import (
	"math"

	"github.com/vibeslop/rustle-mcp/internal/audio"
	"github.com/vibeslop/rustle-mcp/internal/media"
	"github.com/vibeslop/rustle-mcp/internal/rustle"
	"github.com/vibeslop/rustle-mcp/internal/source"
)

// AudioBlockBytes is the largest FLAC in one result; 60 s of 24-bit stereo can exceed it.
const AudioBlockBytes = 8 * 1024 * 1024

// listen is sound in [start, start + seconds): lossless FLAC, or a spectrogram for clients
// that can't hear audio; loudness, balance, onsets and silences come with either. The
// span's samples are decoded once, and the numbers describe exactly the span delivered.
func listen(c *rustle.Call, args ListenArgs) (*rustle.Result, error) {
	id, err := source.Identify(args.Source, c.Settings)
	if err != nil {
		return nil, err
	}
	resolved, err := source.Resolve(c, id)
	if err != nil {
		return nil, err
	}
	// A file's first call probes it. Decoding the requested span meanwhile hides the probe;
	// the samples are dropped if the span moves, and any failure is met again below.
	var early chan *media.PCM
	if resolved.Duration == 0 && resolved.Kind != "youtube" && resolved.Input != "" {
		early = make(chan *media.PCM, 1)
		go func() {
			pcm, _ := media.DecodePCM(c.Ctx, resolved.Input, args.Start, args.Start+args.Seconds, c.Settings, media.PCMOptions{})
			early <- pcm
		}()
	}
	record, err := source.Probed(c, resolved)
	if err != nil {
		return nil, err
	}
	if !record.Audio() {
		return nil, rustle.NoAudioTrack(record.Title)
	}
	duration := record.Duration
	start := args.Start
	end := math.Min(duration, start+args.Seconds)
	if start >= duration {
		asked := start
		start = rustle.MS(math.Max(0, duration-args.Seconds))
		end = duration
		c.Notes.Add("start_seconds " + rustle.Short(asked) + " is past the end (" + rustle.Short(duration) + " s); used " + rustle.Short(start) + "-" + rustle.Short(end))
	}
	input, err := source.MediaInput(c, record, "audio", start, end)
	if err != nil {
		return nil, err
	}
	decode := func() (*media.PCM, error) {
		return media.DecodePCM(c.Ctx, input.Input, start, end, c.Settings, media.PCMOptions{Offset: input.Offset})
	}
	wantAudio := args.Output == "audio"
	type encoded struct {
		flac []byte
		err  error
	}
	var flacs chan encoded
	if wantAudio {
		flacs = make(chan encoded, 1)
		go func() {
			flac, err := media.EncodeFLAC(c.Ctx, input.Input, start, end, c.Settings, input.Offset)
			flacs <- encoded{flac, err}
		}()
	}
	var pcm *media.PCM
	if early != nil && start == args.Start {
		pcm = <-early
	}
	if pcm == nil {
		if pcm, err = decode(); err != nil {
			if flacs != nil {
				<-flacs
			}
			return nil, err
		}
	}
	var flac []byte
	if flacs != nil {
		got := <-flacs
		if got.err != nil {
			return nil, got.err
		}
		flac = got.flac
	}
	if flac != nil && len(flac) > AudioBlockBytes {
		// FLAC size grows about linearly with length, so one shorter encode nearly always fits.
		for tries := 0; tries < 3 && len(flac) > AudioBlockBytes; tries++ {
			end = rustle.MS(start + (end-start)*AudioBlockBytes*0.95/float64(len(flac)))
			if flac, err = media.EncodeFLAC(c.Ctx, input.Input, start, end, c.Settings, input.Offset); err != nil {
				return nil, err
			}
		}
		c.Notes.Add("shortened to fit one audio block; continue from next_start_seconds")
	}
	keep := min(len(pcm.Channels[0]), int(rustle.Round((end-start)*float64(pcm.Rate))))
	channels := make([][]float32, len(pcm.Channels))
	for i, channel := range pcm.Channels {
		channels[i] = channel[:keep]
	}
	// The spectrogram is drawn from the analysis's own spectra. The arrays have one entry
	// per channel and the picture names its panels, so rate and channel count are left out.
	var picture *audio.Spectrogram
	var sink audio.SpectrumSink
	if flac == nil {
		picture = audio.NewSpectrogram(len(channels), keep, pcm.Rate)
		sink = picture
	}
	metrics := audio.Analyze(channels, pcm.Rate, start, sink)
	meta := rustle.NewObj().Set("span", []float64{start, end}).
		Set("peak_dbfs", metrics.PeakDBFS).Set("rms_dbfs", metrics.RMSDBFS)
	if metrics.BalanceDB != nil {
		meta.Set("balance_db", *metrics.BalanceDB)
	}
	silence := make([]any, len(metrics.Silence))
	for i, s := range metrics.Silence {
		silence[i] = []float64{s[0], s[1]}
	}
	meta.Set("onsets", metrics.Onsets).Set("silence", silence)
	if end < duration {
		meta.Set("next_start_seconds", end)
	}
	var block rustle.Block
	if flac != nil {
		block = rustle.Block{Type: "audio", Data: rustle.B64(flac), MimeType: "audio/flac"}
	} else {
		block = image(picture.PNG(start), "image/png", c)
	}
	return &rustle.Result{Meta: meta, Content: []rustle.Block{block}}, nil
}
