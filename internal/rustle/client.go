package rustle

import (
	"math"
	"strings"
)

// SheetWidth is image geometry every harness passes through untouched.
//
// Claude Code re-encodes any image over 2000 px or 3.75 MB as JPEG (quality down to 20),
// and Claude bills one token per 28 px patch. OpenAI's "high" detail scales an image so
// its short side is at most 768 px. A 1176 px wide sheet (42 patches) of 16:9 cells stays
// under all of these, so what the server draws is exactly what the model sees.
const SheetWidth = 1176

// FrameEdge is the long edge of a whole frame from inspect_frame: 24 patches, so a 16:9
// frame costs 336 tokens, against 2,691 for a full 1080p frame. Detail comes from
// cropping (see CropSize).
const FrameEdge = 672

// MaxEdge is the largest long edge any supported harness passes without resampling.
const MaxEdge = 2000

const Patch = 28

// MaxZoom is the enlargement a crop may get; past it, interpolation adds size without
// anything to see.
const MaxZoom = 4

// minZoom: a smaller enlargement softens every edge for too little gain, so the crop stays native.
const minZoom = 1.5

// ImageTokens is Claude's image cost: one token per 28 px patch.
func ImageTokens(width, height int) int {
	return ((width + Patch - 1) / Patch) * ((height + Patch - 1) / Patch)
}

// CropSize is the size to show a crop of width x height source pixels at: no larger than
// `area` pixels, what the uncropped picture takes, so a crop never costs more than the
// whole frame. A tall sidebar then arrives native where a long-edge cap would shrink it. A
// region whose long edge is under `edge` is enlarged toward it, up to MaxZoom: details a
// few pixels wide in the source, like the dots of a pager in a downscaled screen share,
// otherwise share one 28 px patch with everything around them and can't be made out.
func CropSize(width, height int, area float64, edge, limit int) [2]int {
	long := float64(max(width, height))
	scale := math.Min(math.Min(math.Sqrt(area/float64(width*height)), MaxZoom), math.Max(1, float64(edge)/long))
	if scale > 1 && scale < minZoom {
		scale = 1
	}
	scale = math.Min(scale, float64(limit)/long)
	return [2]int{max(1, int(Round(float64(width)*scale))), max(1, int(Round(float64(height)*scale)))}
}

// Profile is what the connected harness does with images and audio.
type Profile struct {
	Name string
	// HearsAudio is false for clients that save audio blocks to a file the model never
	// hears (Claude).
	HearsAudio bool
	// ImageMeta is merged into every image block's _meta, e.g. Codex's request for original detail.
	ImageMeta map[string]any
}

func ProfileFor(name string) Profile {
	id := strings.ToLower(name)
	profile := Profile{Name: name, HearsAudio: !strings.Contains(id, "claude")}
	// Codex sends MCP images at "high" detail unless the block asks for the original.
	if strings.Contains(id, "codex") {
		profile.ImageMeta = map[string]any{"codex/imageDetail": "original"}
	}
	return profile
}
