package rustle

import (
	"context"
	"encoding/json"
	"fmt"
)

// Block is one piece of a tool result: text, an image or audio.
type Block struct {
	Type     string // text, image or audio
	Text     string
	Data     string // base64
	MimeType string
	Meta     map[string]any
}

func TextBlock(text string) Block { return Block{Type: "text", Text: text} }

func (b Block) MarshalJSON() ([]byte, error) {
	out := []byte(`{"type":`)
	out = AppendString(out, b.Type)
	if b.Type == "text" {
		out = append(out, `,"text":`...)
		out = AppendString(out, b.Text)
	} else {
		out = append(out, `,"data":`...)
		out = AppendString(out, b.Data)
		out = append(out, `,"mimeType":`...)
		out = AppendString(out, b.MimeType)
		if b.Meta != nil {
			out = append(out, `,"_meta":`...)
			out = AppendCompact(out, b.Meta)
		}
	}
	return append(out, '}'), nil
}

// Size is the bytes a block takes in a result.
func (b Block) Size() int {
	if b.Type == "text" {
		return len(b.Text)
	}
	return len(b.Data)
}

// Result is what an operation hands the server: one metadata object, then content blocks.
type Result struct {
	Meta    *Obj
	Content []Block
}

// Clone copies the result's metadata and block list, so a cached result is never changed by a caller.
func (r *Result) Clone() *Result {
	return &Result{Meta: r.Meta.Clone(), Content: append([]Block(nil), r.Content...)}
}

// Call is what an operation runs with.
type Call struct {
	// Ctx ends when the client cancels the call.
	Ctx      context.Context
	Settings *Settings
	Cache    *Cache
	Client   Profile
	// Notes are short remarks about clamped or ignored arguments, shown in the metadata.
	Notes *Notes
}

// Shared is the call for work other callers may join: one caller's cancellation must not
// fail the others.
func (c *Call) Shared() *Call {
	shared := *c
	shared.Ctx = context.WithoutCancel(c.Ctx)
	return &shared
}

type Caption struct {
	// Language is a BCP-47-ish code without YouTube's "a." prefix, e.g. "en", "en-US".
	Language string `json:"language"`
	Name     string `json:"name"`
	Kind     string `json:"kind"` // asr or manual
	URL      string `json:"url"`
	Original bool   `json:"original"`
}

// Chapter is a chapter's start in seconds and its title; JSON [start, title].
type Chapter struct {
	At    float64
	Title string
}

func (c Chapter) MarshalJSON() ([]byte, error) { return json.Marshal([]any{c.At, c.Title}) }

func (c *Chapter) UnmarshalJSON(data []byte) error {
	var pair []json.RawMessage
	if err := json.Unmarshal(data, &pair); err != nil || len(pair) != 2 {
		return fmt.Errorf("chapter is not [start, title]")
	}
	if err := json.Unmarshal(pair[0], &c.At); err != nil {
		return err
	}
	return json.Unmarshal(pair[1], &c.Title)
}

// Record is everything known about a source without decoding it; cached per source key.
// A published record is never changed: code that learns more stores a changed copy.
type Record struct {
	Key  string `json:"key"`
	Kind string `json:"kind"` // youtube, url or local
	// Source is the canonical source: watch URL, normalized URL or absolute path.
	Source     string `json:"source"`
	Title      string `json:"title"`
	ResolvedAt int64  `json:"resolvedAt"` // ms since the epoch
	// Duration and the media facts below are filled by probing (or by YouTube's format
	// list); 0 and nil mean not known yet.
	Duration float64 `json:"duration,omitempty"`
	Width    int     `json:"width,omitempty"`
	Height   int     `json:"height,omitempty"`
	FPS      float64 `json:"fps,omitempty"`
	HasVideo *bool   `json:"hasVideo,omitempty"`
	HasAudio *bool   `json:"hasAudio,omitempty"`
	// Origin is the format start time of Input; reported times are relative to it.
	Origin float64 `json:"origin,omitempty"`
	// Input is what ffmpeg opens for url and local sources: a path or an http(s) URL.
	Input string `json:"input,omitempty"`
	HLS   bool   `json:"hls,omitempty"`
	// PageURL is the page a video was discovered on.
	PageURL          string    `json:"pageUrl,omitempty"`
	Captions         []Caption `json:"captions,omitempty"`
	CaptionsError    bool      `json:"captionsError,omitempty"`
	OriginalLanguage string    `json:"originalLanguage,omitempty"`
	Chapters         []Chapter `json:"chapters,omitempty"`
	// YouTube is resolution state owned by the youtube package.
	YouTube *YtState `json:"youtube,omitempty"`
}

// Copy is a copy whose fields can be changed without changing r.
func (r *Record) Copy() *Record {
	c := *r
	if r.YouTube != nil {
		yt := *r.YouTube
		c.YouTube = &yt
	}
	return &c
}

func (r *Record) Video() bool { return r.HasVideo != nil && *r.HasVideo }
func (r *Record) Audio() bool { return r.HasAudio != nil && *r.HasAudio }

// Probed reports whether duration, tracks and size are known.
func (r *Record) Probed() bool { return r.Duration > 0 && r.HasVideo != nil && r.HasAudio != nil }

func Bool(v bool) *bool { return &v }

// YtState is what the media and storyboard code needs after resolution. Plain JSON: the
// record is cached as is.
type YtState struct {
	ID string `json:"id,omitempty"`
	// VisitorData is the visitor id the player request ran under; later requests for this
	// video should reuse it.
	VisitorData string `json:"visitorData,omitempty"`
	// Client is the InnerTube client whose response produced the formats, e.g. "VISIONOS".
	Client string `json:"client,omitempty"`
	// ExpiresAt is when the format URLs stop working, ms since the epoch.
	ExpiresAt int64 `json:"expiresAt,omitempty"`
	// Formats are the adaptive formats, auto-dubbed audio left out.
	Formats []YtFormat `json:"formats,omitempty"`
	// Video and Audio are the itags of the tracks the media code picks; the record's width,
	// height and fps describe Video.
	Video                        int    `json:"video,omitempty"`
	Audio                        int    `json:"audio,omitempty"`
	ServerAbrStreamingURL        string `json:"serverAbrStreamingUrl,omitempty"`
	VideoPlaybackUstreamerConfig string `json:"videoPlaybackUstreamerConfig,omitempty"`
	// Storyboard is the resolving player's storyboard spec, whose levels stop at 160x90.
	Storyboard string `json:"storyboard,omitempty"`
	// WebStoryboard is the WEB player's spec (levels to 320x180) once WebFetched; empty when
	// YouTube has none.
	WebStoryboard string `json:"webStoryboard,omitempty"`
	WebFetched    bool   `json:"webFetched,omitempty"`
}

// YtFormat is an adaptive format as InnerTube sends it, trimmed to the fields media code reads.
type YtFormat struct {
	Itag     int    `json:"itag"`
	MimeType string `json:"mimeType"`
	// URL is absent when a client offers only SABR or ciphered URLs.
	URL              string        `json:"url,omitempty"`
	Bitrate          float64       `json:"bitrate,omitempty"`
	AverageBitrate   float64       `json:"averageBitrate,omitempty"`
	Width            int           `json:"width,omitempty"`
	Height           int           `json:"height,omitempty"`
	FPS              float64       `json:"fps,omitempty"`
	ContentLength    string        `json:"contentLength,omitempty"`
	ApproxDurationMs string        `json:"approxDurationMs,omitempty"`
	LastModified     string        `json:"lastModified,omitempty"`
	Xtags            string        `json:"xtags,omitempty"`
	Type             string        `json:"type,omitempty"`
	AudioTrack       *YtAudioTrack `json:"audioTrack,omitempty"`
	IsDrc            bool          `json:"isDrc,omitempty"`
	InitRange        *YtRange      `json:"initRange,omitempty"`
	IndexRange       *YtRange      `json:"indexRange,omitempty"`
}

type YtAudioTrack struct {
	ID             string `json:"id"`
	DisplayName    string `json:"displayName,omitempty"`
	AudioIsDefault bool   `json:"audioIsDefault,omitempty"`
	IsAutoDubbed   bool   `json:"isAutoDubbed,omitempty"`
}

type YtRange struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// VideoRow is one listed video. Only fields that are known are set; URL always is.
type VideoRow struct {
	Title     string
	Channel   string
	Duration  string
	Views     string
	Published string
	Size      string
	Modified  string
	URL       string
}
