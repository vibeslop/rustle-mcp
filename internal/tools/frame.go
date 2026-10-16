package tools

import (
	"math"

	"github.com/vibeslop/rustle-mcp/internal/imaging"
	"github.com/vibeslop/rustle-mcp/internal/media"
	"github.com/vibeslop/rustle-mcp/internal/rustle"
	"github.com/vibeslop/rustle-mcp/internal/source"
)

// pngLimit: Claude Code re-encodes images over 3.75 MB as JPEG at a quality it picks; our
// own JPEG keeps more.
const pngLimit = 3.5 * 1024 * 1024

var recentFrames = NewRecent(48 << 20)

func frame(c *rustle.Call, args FrameArgs) (*rustle.Result, error) {
	id, err := source.Identify(args.Source, c.Settings)
	if err != nil {
		return nil, err
	}
	record, err := source.Resolve(c, id)
	if err != nil {
		return nil, err
	}
	if record, err = source.Probed(c, record); err != nil {
		return nil, err
	}
	if !record.Video() {
		return nil, rustle.NoVideoTrack(record.Title)
	}
	key := RecentKey(record, sourceless(args, func(a *FrameArgs) { a.Source = "" }), c)
	if cached := recentFrames.Get(key, c); cached != nil {
		return cached, nil
	}
	noted := c.Notes.Len()
	result, err := inspect(c, record, args)
	if err != nil {
		return nil, err
	}
	recentFrames.Set(key, result, c.Notes.Since(noted))
	return result, nil
}

func inspect(c *rustle.Call, record *rustle.Record, args FrameArgs) (*rustle.Result, error) {
	duration := record.Duration
	step := frameStep(record)
	at := math.Max(0, args.At)
	if at > duration {
		at = math.Max(0, duration-step)
		c.Notes.Add("at " + rustle.Short(args.At) + " is past the end (" + rustle.Short(duration) + " s); showing the last frame")
	}
	native := [2]int{record.Width, record.Height}
	region := [4]int{0, 0, native[0], native[1]}
	if args.Crop != nil {
		region = media.CropPixels(native, *args.Crop)
	}
	x, y, w, h := region[0], region[1], region[2], region[3]
	size := sizeFor(native, w, h, args)
	// A frame watch_video listed for a small source is already decoded, at display size.
	if kept := shown.Find(record.Key, at); kept != nil && kept.Width == native[0] && kept.Height == native[1] && size[0] == w && size[1] == h {
		meta := rustle.NewObj().Set("at", kept.At).Set("size", []int{size[0], size[1]})
		return &rustle.Result{Meta: meta, Content: []rustle.Block{image(cropped(kept, x, y, w, h), "image/png", c)}}, nil
	}
	input, err := source.MediaInput(c, record, "video", math.Max(0, at-step), math.Min(duration, at+step))
	if err != nil {
		return nil, err
	}
	src := &media.Input{Input: input.Input, Start: input.Start, Offset: input.Offset, Native: native, FPS: record.FPS, Duration: duration}
	// ffmpeg decodes and Rustle encodes: its own PNG encoder takes three times as long on
	// a 1080p frame and writes a larger file, and the pixels serve the JPEG too, should
	// the PNG be too large.
	decoded, err := media.FrameAt(c.Ctx, src, at, size, args.Crop, true, c.Settings)
	if err != nil {
		return nil, err
	}
	data, mimeType := imaging.EncodePNG(decoded.RGB, decoded.Width, decoded.Height, 3, 6), "image/png"
	if len(data) > pngLimit {
		if data, err = media.EncodeJPEGWithFFmpeg(c.Ctx, decoded.RGB, decoded.Width, decoded.Height, c.Settings, 2); err != nil {
			return nil, err
		}
		mimeType = "image/jpeg"
		c.Notes.Add("PNG over 3.5 MB, which Claude Code would re-encode; sent as high-quality JPEG. Pass a crop for lossless detail")
	}
	scale := float64(size[0]) / float64(w)
	smaller := "crop"
	if args.Crop != nil {
		smaller = "a smaller crop"
	}
	meta := rustle.NewObj().Set("at", decoded.At).Set("size", []int{size[0], size[1]})
	if scale != 1 {
		meta.Set("native", []int{w, h})
	}
	if scale > 1 {
		meta.Set("zoom", rustle.Round(scale*10)/10)
	}
	if scale < 1 {
		if args.Detail == "full" {
			meta.Set("hint", smaller+" for full detail")
		} else {
			meta.Set("hint", smaller+", or detail=full, for full detail")
		}
	}
	return &rustle.Result{Meta: meta, Content: []rustle.Block{image(data, mimeType, c)}}, nil
}

// sizeFor is the whole frame at FrameEdge; a crop at what that costs, enlarged when small
// (see rustle.CropSize); with detail=full, the source pixels, as many as a harness passes unscaled.
func sizeFor(native [2]int, width, height int, args FrameArgs) [2]int {
	if args.Detail == "full" {
		return media.Fit(width, height, rustle.MaxEdge, rustle.MaxEdge)
	}
	whole := media.Fit(native[0], native[1], rustle.FrameEdge, rustle.FrameEdge)
	if args.Crop != nil {
		return rustle.CropSize(width, height, float64(whole[0]*whole[1]), rustle.FrameEdge, rustle.MaxEdge)
	}
	return whole
}

func image(data []byte, mimeType string, c *rustle.Call) rustle.Block {
	return rustle.Block{Type: "image", Data: rustle.B64(data), MimeType: mimeType, Meta: c.Client.ImageMeta}
}

// cropped is a lossless PNG of a rectangle of a frame.
func cropped(frame *media.Frame, x, y, width, height int) []byte {
	rgb := make([]byte, width*height*3)
	for row := range height {
		from := ((y+row)*frame.Width + x) * 3
		copy(rgb[row*width*3:], frame.RGB[from:from+width*3])
	}
	return imaging.EncodePNG(rgb, width, height, 3, 6)
}
