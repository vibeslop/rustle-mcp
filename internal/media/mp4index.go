package media

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"sync"
)

// VideoIndex is the sample table of an MP4/MOV video track: where every frame's bytes
// are, when it is shown, and which frames are keyframes. With it a frame is cut out of the
// file by byte range and handed to ffmpeg as a raw H.264/HEVC stream, so an hour-long file
// needs neither a seek nor ffmpeg re-reading its index (about 10 ms per input on an hour
// of video). Anything unusual yields no index, and the caller decodes the usual way.
type VideoIndex struct {
	Codec string // h264 or hevc
	// LengthSize is the bytes of each NAL unit's length prefix.
	LengthSize int
	// Params are the parameter sets (SPS/PPS, plus VPS for HEVC), without start codes.
	Params [][]byte
	// Offsets, Sizes and Times are per sample, in decode order; Times is the presentation
	// time in seconds from the media origin.
	Offsets []int64
	Sizes   []uint32
	Times   []float64
	// Keys are decode-order indices of keyframes, ascending.
	Keys []int
	// Ordered is true when presentation order equals decode order (no B-frames).
	Ordered bool
}

var indexes = struct {
	sync.Mutex
	known map[string]*VideoIndex
	order []string
}{known: map[string]*VideoIndex{}}

// IndexOf is the index of a local file's first video track, cached per path, size and
// mtime; nil without one.
func IndexOf(path string) *VideoIndex {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil
	}
	key := fmt.Sprintf("%s:%d:%d", path, info.Size(), info.ModTime().UnixNano())
	indexes.Lock()
	known, ok := indexes.known[key]
	indexes.Unlock()
	if ok {
		return known
	}
	index := parseIndex(f, info.Size())
	indexes.Lock()
	if _, ok := indexes.known[key]; !ok {
		indexes.known[key] = index
		indexes.order = append(indexes.order, key)
		if len(indexes.order) > 32 {
			delete(indexes.known, indexes.order[0])
			indexes.order = indexes.order[1:]
		}
	}
	indexes.Unlock()
	return index
}

func parseIndex(f *os.File, size int64) (index *VideoIndex) {
	// A truncated or corrupt table reads out of range: no index, and ffmpeg decides.
	defer func() {
		if recover() != nil {
			index = nil
		}
	}()
	moov := readMoov(f, 256<<20)
	if moov == nil {
		return nil
	}
	root := &box{kind: "moov", start: 8, end: len(moov)}
	var result *VideoIndex
	boxes(moov, root.start, root.end, func(trak box) bool {
		if trak.kind != "trak" {
			return true
		}
		mdia := child(moov, &trak, "mdia")
		hdlr := child(moov, mdia, "hdlr")
		if hdlr == nil || string(moov[hdlr.start+8:hdlr.start+12]) != "vide" {
			return true
		}
		// The first video track decides: it is the one ffmpeg decodes.
		result = track(moov, &trak, mdia)
		return false
	})
	return result
}

func track(m []byte, trak, mdia *box) *VideoIndex {
	u32 := func(b *box, offset int) uint32 { return binary.BigEndian.Uint32(m[b.start+offset:]) }
	i32 := func(at int) int32 { return int32(binary.BigEndian.Uint32(m[at:])) }
	tkhd := child(m, trak, "tkhd")
	if tkhd == nil {
		return nil
	}
	// A rotated or anamorphic track needs ffmpeg's container-level display handling.
	matrix := tkhd.start + 40
	if m[tkhd.start] == 1 {
		matrix = tkhd.start + 52
	}
	if i32(matrix) != 0x10000 || i32(matrix+4) != 0 || i32(matrix+16) != 0x10000 {
		return nil
	}
	mdhd := child(m, mdia, "mdhd")
	if mdhd == nil {
		return nil
	}
	scaleAt := 12
	if m[mdhd.start] == 1 {
		scaleAt = 20
	}
	scale := u32(mdhd, scaleAt)
	stbl := child(m, child(m, mdia, "minf"), "stbl")
	stsd := child(m, stbl, "stsd")
	if stbl == nil || stsd == nil || scale == 0 || u32(stsd, 4) != 1 {
		return nil
	}
	entry := stsd.start + 8
	format := string(m[entry+4 : entry+8])
	sampleEntry := &box{kind: format, start: entry + 86, end: entry + int(binary.BigEndian.Uint32(m[entry:]))}
	if pasp := child(m, sampleEntry, "pasp"); pasp != nil && u32(pasp, 0) != u32(pasp, 4) {
		return nil
	}
	index := &VideoIndex{}
	switch format {
	case "avc1", "avc3":
		avcC := child(m, sampleEntry, "avcC")
		if avcC == nil {
			return nil
		}
		index.Codec = "h264"
		index.LengthSize = int(m[avcC.start+4]&3) + 1
		p := avcC.start + 5
		for _, mask := range []byte{31, 255} {
			count := int(m[p] & mask)
			p++
			for range count {
				length := int(binary.BigEndian.Uint16(m[p:]))
				index.Params = append(index.Params, m[p+2:p+2+length])
				p += 2 + length
			}
		}
	case "hvc1", "hev1":
		hvcC := child(m, sampleEntry, "hvcC")
		if hvcC == nil {
			return nil
		}
		index.Codec = "hevc"
		index.LengthSize = int(m[hvcC.start+21]&3) + 1
		p := hvcC.start + 23
		for range int(m[hvcC.start+22]) {
			count := int(binary.BigEndian.Uint16(m[p+1:]))
			p += 3
			for range count {
				length := int(binary.BigEndian.Uint16(m[p:]))
				index.Params = append(index.Params, m[p+2:p+2+length])
				p += 2 + length
			}
		}
	default:
		return nil
	}
	if len(index.Params) == 0 {
		return nil
	}

	stsz := child(m, stbl, "stsz")
	stts := child(m, stbl, "stts")
	stsc := child(m, stbl, "stsc")
	stco := child(m, stbl, "stco")
	co64 := child(m, stbl, "co64")
	chunkTable := stco
	if chunkTable == nil {
		chunkTable = co64
	}
	if stsz == nil || stts == nil || stsc == nil || chunkTable == nil {
		return nil
	}
	count := int(u32(stsz, 8))
	if count == 0 {
		return nil
	}
	fixed := u32(stsz, 4)
	sizes := make([]uint32, count)
	for i := range sizes {
		if fixed != 0 {
			sizes[i] = fixed
		} else {
			sizes[i] = u32(stsz, 12+4*i)
		}
	}

	dts := make([]float64, count)
	for e, i, t, n := 0, 0, 0.0, int(u32(stts, 4)); e < n; e++ {
		samples := int(u32(stts, 8+8*e))
		delta := float64(u32(stts, 12+8*e))
		for k := 0; k < samples && i < count; k, t = k+1, t+delta {
			dts[i] = t
			i++
		}
	}
	cts := append([]float64(nil), dts...)
	index.Ordered = true
	if ctts := child(m, stbl, "ctts"); ctts != nil {
		signed := m[ctts.start] == 1
		for e, i, n := 0, 0, int(u32(ctts, 4)); e < n; e++ {
			samples := int(u32(ctts, 8+8*e))
			var offset float64
			if signed {
				offset = float64(i32(ctts.start + 12 + 8*e))
			} else {
				offset = float64(u32(ctts, 12+8*e))
			}
			for k := 0; k < samples && i < count; k++ {
				cts[i] += offset
				i++
			}
		}
		for i := 1; i < count && index.Ordered; i++ {
			if cts[i] < cts[i-1] {
				index.Ordered = false
			}
		}
	}
	// One edit that starts the track at a media time is how encoders hide B-frame delay;
	// anything more (empty edits, several segments) is left to ffmpeg.
	shift := 0.0
	if elst := child(m, child(m, trak, "edts"), "elst"); elst != nil {
		if u32(elst, 4) != 1 {
			return nil
		}
		var mediaTime float64
		if m[elst.start] == 1 {
			mediaTime = float64(int64(binary.BigEndian.Uint64(m[elst.start+16:])))
		} else {
			mediaTime = float64(i32(elst.start + 12))
		}
		if mediaTime < 0 {
			return nil
		}
		shift = mediaTime
	} else {
		shift = math.Inf(1)
		for _, t := range cts {
			shift = math.Min(shift, t)
		}
	}
	index.Times = make([]float64, count)
	for i, t := range cts {
		index.Times[i] = (t - shift) / float64(scale)
	}

	index.Offsets = make([]int64, count)
	chunks := int(u32(chunkTable, 4))
	chunkOffset := func(c int) int64 {
		if co64 != nil {
			return int64(binary.BigEndian.Uint64(m[co64.start+8+8*c:]))
		}
		return int64(u32(stco, 8+4*c))
	}
	for e, s, n := 0, 0, int(u32(stsc, 4)); e < n; e++ {
		first := int(u32(stsc, 8+12*e)) - 1
		per := int(u32(stsc, 12+12*e))
		last := chunks
		if e+1 < n {
			last = int(u32(stsc, 8+12*(e+1))) - 1
		}
		for c := first; c < last && s < count; c++ {
			offset := chunkOffset(c)
			for k := 0; k < per && s < count; k++ {
				index.Offsets[s] = offset
				offset += int64(sizes[s])
				s++
			}
		}
	}
	if stss := child(m, stbl, "stss"); stss != nil {
		for e, n := 0, int(u32(stss, 4)); e < n; e++ {
			index.Keys = append(index.Keys, int(u32(stss, 8+4*e))-1)
		}
	} else {
		for i := range count {
			index.Keys = append(index.Keys, i)
		}
	}
	if len(index.Keys) == 0 || index.Keys[0] != 0 {
		return nil
	}
	index.Sizes = sizes
	return index
}

var startCode = []byte{0, 0, 0, 1}

// Elementary is an Annex B elementary stream of the given samples (decode-order indices,
// ascending): each sample's length-prefixed NAL units get start codes, and the parameter
// sets lead every keyframe so each run decodes on its own. Samples next to each other in
// the file are read in one go.
func Elementary(path string, index *VideoIndex, samples []int) ([]byte, error) {
	keys := make(map[int]bool, len(index.Keys))
	for _, k := range index.Keys {
		keys[k] = true
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []byte
	for i := 0; i < len(samples); {
		// A run of samples stored back to back, up to 64 MB.
		j := i
		for j+1 < len(samples) && index.Offsets[samples[j+1]] == index.Offsets[samples[j]]+int64(index.Sizes[samples[j]]) &&
			index.Offsets[samples[j+1]]+int64(index.Sizes[samples[j+1]])-index.Offsets[samples[i]] < 64<<20 {
			j++
		}
		base := index.Offsets[samples[i]]
		block := readAt(f, base, int(index.Offsets[samples[j]]+int64(index.Sizes[samples[j]])-base))
		for k := i; k <= j; k++ {
			n := samples[k]
			if keys[n] {
				for _, set := range index.Params {
					out = append(out, startCode...)
					out = append(out, set...)
				}
			}
			from := index.Offsets[n] - base
			to := min(int64(len(block)), from+int64(index.Sizes[n]))
			if from > to {
				continue
			}
			sample := block[from:to]
			for o := 0; o+index.LengthSize <= len(sample); {
				length := 0
				for _, b := range sample[o : o+index.LengthSize] {
					length = length<<8 | int(b)
				}
				o += index.LengthSize
				end := min(len(sample), o+length)
				out = append(out, startCode...)
				out = append(out, sample[o:end]...)
				o += length
			}
		}
		i = j + 1
	}
	return out, nil
}
