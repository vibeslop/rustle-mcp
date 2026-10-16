package tools

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

// Arguments after the server has clamped, rounded and defaulted them.

type FindArgs struct {
	Query   string `json:"query"`
	Channel string `json:"channel"`
	URL     string `json:"url"`
	Limit   int    `json:"limit"`
}

type TranscriptArgs struct {
	Source string   `json:"source"`
	Start  float64  `json:"start"`
	End    *float64 `json:"end"`
	Query  string   `json:"query"`
	// Language "auto" picks the spoken language.
	Language   string `json:"language"`
	Transcribe bool   `json:"transcribe"`
	// Detail is "digest", the whole video's key sentences, or "full", every word.
	Detail string `json:"detail"`
}

type WatchArgs struct {
	Source string   `json:"source"`
	Start  float64  `json:"start"`
	End    *float64 `json:"end"`
	Frames int      `json:"frames"`
	// FPS samples consecutive frames at this rate from Start, for motion; 0 is off.
	FPS          float64 `json:"fps"`
	IncludeAudio bool    `json:"includeAudio"`
	// Crop is [x, y, width, height] as fractions of the frame, already clipped to it.
	Crop *[4]float64 `json:"crop"`
}

type FrameArgs struct {
	Source string  `json:"source"`
	At     float64 `json:"at"`
	// Crop is [x, y, width, height] as fractions of the frame, already clipped to it.
	Crop *[4]float64 `json:"crop"`
	// Detail is "fit" (the default), what the whole frame costs at most, or "full", the
	// source pixels.
	Detail string `json:"detail"`
}

type ListenArgs struct {
	Source  string  `json:"source"`
	Start   float64 `json:"start"`
	Seconds float64 `json:"seconds"`
	// Output is "audio" or "analysis".
	Output string `json:"output"`
}

// Args reads a call's raw arguments leniently. Agents echo values from earlier output,
// send null for optional arguments and overshoot ranges, and a rejected call costs a whole
// round trip. Null means the default; out-of-range numbers are clamped, fractional counts
// rounded and arguments that don't apply dropped, each with a short note; only malformed
// input fails.
type Args struct {
	Notes *rustle.Notes
	raw   map[string]any
	used  map[string]bool
}

func NewArgs(raw map[string]any) *Args {
	a := &Args{Notes: &rustle.Notes{}, raw: map[string]any{}, used: map[string]bool{}}
	// Models often send null for an optional argument they mean to leave out.
	for k, v := range raw {
		if v != nil {
			a.raw[k] = v
		}
	}
	return a
}

func (a *Args) take(name string) (any, bool) {
	a.used[name] = true
	v, ok := a.raw[name]
	return v, ok
}

func (a *Args) String(name, fallback string) (string, error) {
	value, ok := a.take(name)
	if !ok {
		return fallback, nil
	}
	switch v := value.(type) {
	case float64:
		return rustle.NumString(v), nil
	case string:
		return rustle.TrimSpace(v), nil
	}
	return "", rustle.Invalid(name + " must be a string.")
}

// Number is the named number; false when it is missing or empty.
func (a *Args) Number(name string) (float64, bool, error) {
	value, ok := a.take(name)
	if !ok || value == "" {
		return 0, false, nil
	}
	var parsed float64
	switch v := value.(type) {
	case float64:
		parsed = v
	case string:
		parsed = rustle.Number(rustle.TrimSpace(v))
	default:
		parsed = math.NaN()
	}
	if !rustle.Finite(parsed) {
		return 0, false, rustle.Invalid(name + " must be a finite number.")
	}
	return parsed, true, nil
}

func (a *Args) Boolean(name string) (bool, error) {
	value, ok := a.take(name)
	if !ok {
		return false, nil
	}
	switch v := value.(type) {
	case bool:
		return v, nil
	case string:
		if v == "true" || v == "false" {
			return v == "true", nil
		}
	}
	return false, rustle.Invalid(name + " must be true or false.")
}

func (a *Args) Source() (string, error) {
	value, err := a.String("source", "")
	if err != nil {
		return "", err
	}
	if value == "" {
		return "", rustle.Invalid("source is required: a YouTube link, a web page, a media URL or a local file path.")
	}
	return value, nil
}

// Span is start_seconds and end_seconds; end is nil when not given.
func (a *Args) Span() (float64, *float64, error) {
	start, _, err := a.Number("start_seconds")
	if err != nil {
		return 0, nil, err
	}
	start = rustle.Clamp("start_seconds", start, 0, math.Inf(1), a.Notes)
	end, ok, err := a.Number("end_seconds")
	if err != nil {
		return 0, nil, err
	}
	if !ok {
		return start, nil, nil
	}
	if end <= start {
		return 0, nil, rustle.Invalid("end_seconds must be greater than start_seconds.")
	}
	return start, &end, nil
}

func (a *Args) Crop() (*[4]float64, error) {
	value, ok := a.take("crop")
	if !ok {
		return nil, nil
	}
	fail := rustle.Invalid("crop must be [x, y, width, height] as fractions of the frame with a positive area inside it, e.g. [0.5, 0, 0.5, 0.5] for the top-right quarter.")
	list, isList := value.([]any)
	if !isList || len(list) != 4 {
		return nil, fail
	}
	var v [4]float64
	for i, item := range list {
		n, isNumber := item.(float64)
		if !isNumber || !rustle.Finite(n) {
			return nil, fail
		}
		v[i] = n
	}
	left, top := math.Max(v[0], 0), math.Max(v[1], 0)
	right, bottom := math.Min(v[0]+v[2], 1), math.Min(v[1]+v[3], 1)
	if right <= left || bottom <= top {
		return nil, fail
	}
	fitted := [4]float64{left, top, right - left, bottom - top}
	for i := range fitted {
		if math.Abs(fitted[i]-v[i]) > 1e-9 {
			shown := make([]string, 4)
			for k, f := range fitted {
				shown[k] = rustle.Short(f)
			}
			a.Notes.Add("crop clipped to the frame: [" + strings.Join(shown, ", ") + "]")
			break
		}
	}
	return &fitted, nil
}

// Finish notes arguments the tool doesn't define instead of failing on them.
func (a *Args) Finish() {
	var extra []string
	for k := range a.raw {
		if !a.used[k] {
			extra = append(extra, k)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		a.Notes.Add("ignored unknown arguments: " + strings.Join(extra, ", "))
	}
}

func (a *Args) whole(name string, fallback, low, high float64) (int, error) {
	value, ok, err := a.Number(name)
	if err != nil {
		return 0, err
	}
	if !ok {
		value = fallback
	}
	return rustle.Whole(name, value, low, high, a.Notes), nil
}

func oneOf(value string, allowed ...string) bool {
	for _, a := range allowed {
		if value == a {
			return true
		}
	}
	return false
}

func quoted(values ...string) string {
	q := make([]string, len(values))
	for i, v := range values {
		q[i] = fmt.Sprintf("%q", v)
	}
	if len(q) == 2 {
		return q[0] + " or " + q[1]
	}
	return strings.Join(q[:len(q)-1], ", ") + " or " + q[len(q)-1]
}
