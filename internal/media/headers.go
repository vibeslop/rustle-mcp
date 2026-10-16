package media

import (
	"encoding/binary"
	"math"
	"os"

	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

// ProbeHeader probes MP4/MOV and WAV files from their headers, without starting ffprobe:
// loading ffmpeg's libraries costs 35-40 ms, as much as decoding a frame. Anything
// unusual (fragmented MP4, anamorphic pixels, compressed WAV) returns nil and goes to
// ffprobe. The start time is ffmpeg's to decide, so decoders read it from their own log.
func ProbeHeader(path string) *Probe {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	head := readAt(f, 0, 12)
	if len(head) < 12 {
		return nil
	}
	if string(head[:4]) == "RIFF" && string(head[8:12]) == "WAVE" {
		wav := WAVLayoutOf(f)
		if wav == nil {
			return nil
		}
		return &Probe{Duration: float64(wav.Frames) / float64(wav.Rate), HasAudio: true, SampleRate: wav.Rate, Channels: wav.Channels}
	}
	switch string(head[4:8]) {
	case "ftyp", "moov", "mdat", "free", "wide", "skip":
		return mp4Probe(f)
	}
	return nil
}

func readAt(f *os.File, position int64, length int) []byte {
	if length <= 0 || position < 0 {
		return nil
	}
	buf := make([]byte, length)
	n, _ := f.ReadAt(buf, position)
	return buf[:n]
}

func fileSize(f *os.File) int64 {
	info, err := f.Stat()
	if err != nil {
		return 0
	}
	return info.Size()
}

type WAVLayout struct {
	// Format is 1 for integer PCM, 3 for IEEE float.
	Format   int
	Bits     int
	Channels int
	Rate     int
	// Data is the byte offset of the first sample; Frames the number of sample frames.
	Data   int64
	Frames int64
}

// WAVLayoutOf reads an uncompressed WAV's layout; nil for anything else.
func WAVLayoutOf(f *os.File) *WAVLayout {
	size := fileSize(f)
	var fmtChunk []byte
	for at := int64(12); at+8 <= size; {
		header := readAt(f, at, 8)
		if len(header) < 8 {
			return nil
		}
		id := string(header[:4])
		length := int64(binary.LittleEndian.Uint32(header[4:]))
		if id == "fmt " {
			fmtChunk = readAt(f, at+8, int(min(length, 64)))
		} else if id == "data" {
			if len(fmtChunk) < 16 {
				return nil
			}
			format := int(binary.LittleEndian.Uint16(fmtChunk))
			// WAVE_FORMAT_EXTENSIBLE names the real format in its sub-format GUID.
			if format == 0xfffe && len(fmtChunk) >= 26 {
				format = int(binary.LittleEndian.Uint16(fmtChunk[24:]))
			}
			channels := int(binary.LittleEndian.Uint16(fmtChunk[2:]))
			rate := int(binary.LittleEndian.Uint32(fmtChunk[4:]))
			align := int(binary.LittleEndian.Uint16(fmtChunk[12:]))
			bits := int(binary.LittleEndian.Uint16(fmtChunk[14:]))
			valid := (format == 1 && (bits == 8 || bits == 16 || bits == 24 || bits == 32)) || (format == 3 && (bits == 32 || bits == 64))
			if !valid || channels == 0 || rate == 0 || align != channels*(bits/8) {
				return nil
			}
			// Streaming writers leave the size at 0 or 0xFFFFFFFF; the samples run to the end.
			if length == 0 || length == 0xffffffff || at+8+length > size {
				length = size - at - 8
			}
			return &WAVLayout{Format: format, Bits: bits, Channels: channels, Rate: rate, Data: at + 8, Frames: length / int64(align)}
		}
		at += 8 + length + length&1
	}
	return nil
}

type box struct {
	kind       string
	start, end int
}

// boxes is the ISO-BMFF boxes in data[from:to]; it stops at the first malformed one.
func boxes(data []byte, from, to int, each func(box) bool) {
	for at := from; at+8 <= to; {
		size := int(binary.BigEndian.Uint32(data[at:]))
		header := 8
		if size == 1 {
			if at+16 > to {
				return
			}
			size = int(binary.BigEndian.Uint64(data[at+8:]))
			header = 16
		} else if size == 0 {
			size = to - at
		}
		if size < header || at+size > to || size < 0 {
			return
		}
		if !each(box{kind: string(data[at+4 : at+8]), start: at + header, end: at + size}) {
			return
		}
		at += size
	}
}

func child(data []byte, parent *box, kind string) *box {
	if parent == nil {
		return nil
	}
	var found *box
	boxes(data, parent.start, parent.end, func(b box) bool {
		if b.kind == kind {
			found = &b
			return false
		}
		return true
	})
	return found
}

// readMoov is the moov box of an MP4 file, at most limit bytes.
func readMoov(f *os.File, limit int64) []byte {
	size := fileSize(f)
	for at := int64(0); at+8 <= size; {
		head := readAt(f, at, 16)
		if len(head) < 8 {
			return nil
		}
		length := int64(binary.BigEndian.Uint32(head))
		if length == 1 {
			if len(head) < 16 {
				return nil
			}
			length = int64(binary.BigEndian.Uint64(head[8:]))
		} else if length == 0 {
			length = size - at
		}
		if length < 8 {
			return nil
		}
		if string(head[4:8]) == "moov" {
			if length > limit {
				return nil
			}
			return readAt(f, at, int(length))
		}
		at += length
	}
	return nil
}

func mp4Probe(f *os.File) (probe *Probe) {
	// A truncated or corrupt header reads out of range; ffprobe then decides.
	defer func() {
		if recover() != nil {
			probe = nil
		}
	}()
	moov := readMoov(f, 64<<20)
	if moov == nil {
		return nil
	}
	root := &box{kind: "moov", start: 8, end: len(moov)}
	u32 := func(at int) uint32 { return binary.BigEndian.Uint32(moov[at:]) }
	mvhd := child(moov, root, "mvhd")
	if mvhd == nil {
		return nil
	}
	var timescale uint32
	var length float64
	if moov[mvhd.start] == 1 {
		timescale = u32(mvhd.start + 20)
		length = float64(binary.BigEndian.Uint64(moov[mvhd.start+24:]))
	} else {
		timescale = u32(mvhd.start + 12)
		length = float64(u32(mvhd.start + 16))
	}
	p := &Probe{}
	if timescale > 0 {
		p.Duration = length / float64(timescale)
	}
	ok := true
	boxes(moov, root.start, root.end, func(trak box) bool {
		if trak.kind != "trak" {
			return true
		}
		mdia := child(moov, &trak, "mdia")
		hdlr := child(moov, mdia, "hdlr")
		mdhd := child(moov, mdia, "mdhd")
		minf := child(moov, mdia, "minf")
		if minf == nil {
			minf = mdia
		}
		stbl := child(moov, minf, "stbl")
		if hdlr == nil || mdhd == nil || stbl == nil {
			return true
		}
		handler := string(moov[hdlr.start+8 : hdlr.start+12])
		stsd := child(moov, stbl, "stsd")
		stts := child(moov, stbl, "stts")
		if stsd == nil || stts == nil {
			return true
		}
		samples := 0.0
		for i, n := 0, int(u32(stts.start+4)); i < n; i++ {
			samples += float64(u32(stts.start + 8 + i*8))
		}
		// A fragmented file keeps its samples in moof boxes; ffprobe reads those.
		if samples == 0 {
			ok = false
			return false
		}
		entry := stsd.start + 8
		if handler == "soun" && !p.HasAudio {
			p.HasAudio = true
			p.Channels = int(binary.BigEndian.Uint16(moov[entry+24:]))
			p.SampleRate = int(u32(entry+32) >> 16)
		} else if handler == "vide" && !p.HasVideo && samples > 1 {
			// A one-sample video track is cover art, as ffprobe's attached_pic says.
			sampleEntry := &box{start: entry + 86, end: entry + int(u32(entry))}
			if pasp := child(moov, sampleEntry, "pasp"); pasp != nil && u32(pasp.start) != u32(pasp.start+4) {
				ok = false
				return false
			}
			tkhd := child(moov, &trak, "tkhd")
			if tkhd == nil {
				ok = false
				return false
			}
			matrix := tkhd.start + 40
			if moov[tkhd.start] == 1 {
				matrix = tkhd.start + 52
			}
			a, b := int32(u32(matrix)), int32(u32(matrix+4))
			width := int(binary.BigEndian.Uint16(moov[entry+32:]))
			height := int(binary.BigEndian.Uint16(moov[entry+34:]))
			// A quarter-turn matrix means the frames are displayed rotated, with sides swapped.
			if a == 0 && b != 0 {
				width, height = height, width
			}
			if width == 0 || height == 0 {
				ok = false
				return false
			}
			var scale uint32
			var ticks float64
			if moov[mdhd.start] == 1 {
				scale = u32(mdhd.start + 20)
				ticks = float64(binary.BigEndian.Uint64(moov[mdhd.start+24:]))
			} else {
				scale = u32(mdhd.start + 12)
				ticks = float64(u32(mdhd.start + 16))
			}
			p.HasVideo, p.Width, p.Height = true, width, height
			if ticks > 0 && scale > 0 {
				p.FPS = samples / (ticks / float64(scale))
			}
		}
		return true
	})
	if !ok || (!p.HasVideo && !p.HasAudio) || !(p.Duration > 0 && p.Duration <= 24*3600) {
		return nil
	}
	return p
}

// ReadWAV reads samples of [start, end) from an uncompressed WAV as one slice per
// channel, -1..1; nil when it isn't one.
func ReadWAV(path string, start, end float64, mono bool) *PCM {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	wav := WAVLayoutOf(f)
	if wav == nil {
		return nil
	}
	bytes := wav.Bits / 8
	align := bytes * wav.Channels
	rate := float64(wav.Rate)
	first := min(wav.Frames, int64(math.Max(0, rustle.Round(start*rate))))
	count := max(0, min(wav.Frames, int64(rustle.Round(end*rate)))-first)
	raw := readAt(f, wav.Data+first*int64(align), int(count)*align)
	frames := len(raw) / align
	kept := min(8, wav.Channels)
	channels := make([][]float32, kept)
	for c := range channels {
		out := make([]float32, frames)
		for i, at := 0, c*bytes; i < frames; i, at = i+1, at+align {
			var v float64
			switch {
			case wav.Format == 3 && bytes == 4:
				v = float64(math.Float32frombits(binary.LittleEndian.Uint32(raw[at:])))
			case wav.Format == 3:
				v = math.Float64frombits(binary.LittleEndian.Uint64(raw[at:]))
			case bytes == 1:
				v = (float64(raw[at]) - 128) / 128
			case bytes == 2:
				v = float64(int16(binary.LittleEndian.Uint16(raw[at:]))) / 32768
			case bytes == 3:
				v = float64(int32(uint32(raw[at])|uint32(raw[at+1])<<8|uint32(raw[at+2])<<16)<<8>>8) / 8388608
			default:
				v = float64(int32(binary.LittleEndian.Uint32(raw[at:]))) / 2147483648
			}
			out[i] = float32(v)
		}
		channels[c] = out
	}
	if !mono || kept == 1 {
		return &PCM{Channels: channels, Rate: wav.Rate}
	}
	mix := make([]float32, frames)
	for _, channel := range channels {
		for i := range mix {
			mix[i] = float32(float64(mix[i]) + float64(channel[i])/float64(kept))
		}
	}
	return &PCM{Channels: [][]float32{mix}, Rate: wav.Rate}
}
