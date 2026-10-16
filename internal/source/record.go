package source

import (
	"path/filepath"
	"time"

	"github.com/vibeslop/rustle-mcp/internal/media"
	"github.com/vibeslop/rustle-mcp/internal/rustle"
	"github.com/vibeslop/rustle-mcp/internal/youtube"
)

var resolving rustle.Flight[*rustle.Record]

// Resolve describes a source without decoding it: from memory, then disk, then the network.
func Resolve(c *rustle.Call, id Identity) (*rustle.Record, error) {
	if known := rustle.KnownRecord(id.Key, c.Cache); known != nil {
		c.Cache.Touch(id.Key)
		return known, nil
	}
	// Parallel agents resolve the same source together: one caller's cancellation must not
	// fail the others, so the shared work runs without it and each caller waits on its own.
	shared := c.Shared()
	return resolving.Do(c.Ctx, id.Key, func() (*rustle.Record, error) {
		record, err := resolveFresh(shared, id, 0)
		if err != nil {
			return nil, err
		}
		rustle.SaveRecord(record, shared.Cache)
		return record, nil
	})
}

func resolveFresh(c *rustle.Call, id Identity, depth int) (*rustle.Record, error) {
	now := time.Now().UnixMilli()
	switch id.Kind {
	case "youtube":
		return youtube.Resolve(c, id.ID, id.Key)
	case "local":
		if _, err := AssertMediaFile(id.Source); err != nil {
			return nil, err
		}
		return &rustle.Record{Key: id.Key, Kind: "local", Source: id.Source, Title: filepath.Base(id.Source), ResolvedAt: now,
			Input: id.Source, Captions: []rustle.Caption{}}, nil
	}
	kind, err := ClassifyURL(c.Ctx, id.Source)
	if err != nil {
		return nil, err
	}
	if kind != "page" {
		return &rustle.Record{Key: id.Key, Kind: "url", Source: id.Source, Title: FileTitle(id.Source), ResolvedAt: now,
			Input: id.Source, HLS: kind == "hls", Captions: []rustle.Caption{}}, nil
	}
	found, err := PageVideos(c.Ctx, id.Source, true)
	if err != nil {
		return nil, err
	}
	var candidates []Candidate
	for _, f := range found {
		if f.URL != id.Source {
			candidates = append(candidates, f)
		}
	}
	if len(candidates) == 0 || depth > 0 {
		return nil, rustle.Fail("No public video source found in this page. JavaScript-only, DRM and signed-in players are unsupported.",
			"NO_VIDEO_FOUND", "Pass a direct media URL or a local file, or use find_videos(url=...) to list what the page links to.")
	}
	var last error
	for _, candidate := range candidates[:min(3, len(candidates))] {
		record, err := func() (*rustle.Record, error) {
			inner, err := Identify(candidate.URL, c.Settings)
			if err != nil {
				return nil, err
			}
			return resolveFresh(c, inner, depth+1)
		}()
		if err != nil {
			last = err
			continue
		}
		// Cached under the page's key, so the page link keeps working as a source.
		record = record.Copy()
		record.Key, record.PageURL = id.Key, id.Source
		if candidate.Title != "" {
			record.Title = candidate.Title
		}
		return record, nil
	}
	if e, ok := rustle.AsError(last); ok {
		return nil, &rustle.Error{Message: "The video in this page could not be opened: " + e.Message, Code: e.Code, Recovery: e.Recovery,
			Retryable: e.Retryable, RetryAfter: e.RetryAfter}
	}
	return nil, last
}

// Probed fills duration, size, frame rate and tracks; YouTube records already carry them.
// It returns the record that holds them, a copy when they were missing.
func Probed(c *rustle.Call, record *rustle.Record) (*rustle.Record, error) {
	if record.Probed() {
		return record, nil
	}
	if record.Input == "" {
		return nil, rustle.Fail("This source has no media to decode.", "UNSUPPORTED_MEDIA", "Pass a video or audio source.")
	}
	var info *media.Probe
	if record.Kind == "local" {
		info = media.ProbeHeader(record.Input)
	}
	if info == nil {
		var err error
		if info, err = media.ProbeInput(c.Ctx, record.Input, c.Settings); err != nil {
			return nil, err
		}
	}
	probed := record.Copy()
	probed.Duration, probed.Origin = info.Duration, info.Start
	probed.HasVideo, probed.HasAudio = rustle.Bool(info.HasVideo), rustle.Bool(info.HasAudio)
	probed.Width, probed.Height, probed.FPS = info.Width, info.Height, info.FPS
	rustle.SaveRecord(probed, c.Cache)
	return probed, nil
}

// Input is what ffmpeg should open for a span of a track.
type Input struct {
	Input string
	// Start is the format start time of Input.
	Start float64
	// Offset is the timeline time of the input's first timestamp.
	Offset float64
}

// MediaInput is what ffmpeg should open for [start, end] of a track.
func MediaInput(c *rustle.Call, record *rustle.Record, track string, start, end float64) (Input, error) {
	if track == "video" && record.HasVideo != nil && !*record.HasVideo {
		return Input{}, rustle.NoVideoTrack(record.Title)
	}
	if track == "audio" && record.HasAudio != nil && !*record.HasAudio {
		return Input{}, rustle.NoAudioTrack(record.Title)
	}
	if record.Kind == "youtube" {
		window, err := youtube.Media(c, record, track, start, end)
		if err != nil {
			return Input{}, err
		}
		info, err := media.ProbeInput(c.Ctx, window.Input, c.Settings)
		if err != nil {
			return Input{}, err
		}
		return Input{Input: window.Input, Start: info.Start, Offset: window.Offset}, nil
	}
	return Input{Input: record.Input, Start: record.Origin}, nil
}
