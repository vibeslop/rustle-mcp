package youtube

import (
	"encoding/binary"
	"errors"
	"math"
)

// Byte-range indexes of YouTube's DASH files. Every adaptive format is one file whose
// header lists where each few-second segment starts: a sidx box in MP4, a Cues element in
// WebM. With it, any timestamp maps to one small range request, so a long video is never
// downloaded whole. Each segment starts on a keyframe and keeps its absolute time.

type Segment struct {
	// Start and End are seconds on the video's timeline.
	Start, End float64
	// From and To are the inclusive byte range in the format's file.
	From, To int64
}

type DashIndex struct {
	// Init is the bytes that make a playable file when followed by any run of whole segments.
	Init     []byte
	Segments []Segment
}

// errTruncated stands for a header that ends before its fields do.
var errTruncated = errors.New("truncated index")

// guarded turns a read past a header's end into an error.
func guarded(parse func() (*DashIndex, error)) (index *DashIndex, err error) {
	defer func() {
		if recover() != nil {
			index, err = nil, errTruncated
		}
	}()
	return parse()
}

// ParseMP4Index reads an MP4 head: ftyp+moov is the init segment; the sidx after it lists
// moof+mdat fragments.
func ParseMP4Index(head []byte, initEnd int) (*DashIndex, error) {
	return guarded(func() (*DashIndex, error) {
		for at := initEnd + 1; at+8 <= len(head); {
			size := int(binary.BigEndian.Uint32(head[at:]))
			if string(head[at+4:at+8]) == "sidx" {
				segments, err := ParseSidx(head, at)
				if err != nil {
					return nil, err
				}
				return &DashIndex{Init: head[:initEnd+1], Segments: segments}, nil
			}
			if size < 8 {
				break
			}
			at += size
		}
		return nil, errors.New("no sidx box")
	})
}

func ParseSidx(buf []byte, at int) ([]Segment, error) {
	u32 := func(p int) uint32 { return binary.BigEndian.Uint32(buf[p:]) }
	size := int64(u32(at))
	version := buf[at+8]
	timescale := float64(u32(at + 16))
	p := at + 20
	var t, offset float64
	if version == 0 {
		t, offset = float64(u32(p)), float64(u32(p+4))
		p += 8
	} else {
		t, offset = float64(binary.BigEndian.Uint64(buf[p:])), float64(binary.BigEndian.Uint64(buf[p+8:]))
		p += 16
	}
	count := int(binary.BigEndian.Uint16(buf[p+2:]))
	p += 4
	from := int64(at) + size + int64(offset)
	var segments []Segment
	for range count {
		bytes := int64(u32(p) & 0x7fffffff)
		duration := float64(u32(p + 4))
		segments = append(segments, Segment{Start: t / timescale, End: (t + duration) / timescale, From: from, To: from + bytes - 1})
		t += duration
		from += bytes
		p += 12
	}
	if len(segments) == 0 {
		return nil, errors.New("empty sidx")
	}
	return segments, nil
}

const (
	ebmlHeader    = 0x1a45dfa3
	ebmlSegment   = 0x18538067
	ebmlInfo      = 0x1549a966
	ebmlTracks    = 0x1654ae6b
	ebmlCues      = 0x1c53bb6b
	timecodeScale = 0x2ad7b1
	cuePoint      = 0xbb
	cueTime       = 0xb3
	cuePositions  = 0xb7
	cueCluster    = 0xf1
)

// vint is an EBML variable-length integer at `at`: its value and length; ids keep their
// marker bit, and an all-ones size is -1 (unknown).
func vint(buf []byte, at int, id bool) (float64, int) {
	first := buf[at]
	length := 1
	for length <= 8 && first&(0x80>>(length-1)) == 0 {
		length++
	}
	if length > 8 {
		panic("bad EBML vint")
	}
	value := float64(first)
	if !id {
		value = float64(first & (0xff >> length))
	}
	unknown := value == float64(int(0xff)>>length)
	for i := 1; i < length; i++ {
		value = value*256 + float64(buf[at+i])
		if buf[at+i] != 0xff {
			unknown = false
		}
	}
	if !id && unknown {
		return -1, length
	}
	return value, length
}

type element struct {
	id float64
	// at is the start of the element's header, data of its payload.
	at, data int
	// size is -1 when unknown (live-style).
	size int
}

func readElement(buf []byte, at int) element {
	id, idLength := vint(buf, at, true)
	size, sizeLength := vint(buf, at+idLength, false)
	return element{id: id, at: at, data: at + idLength + sizeLength, size: int(size)}
}

func children(buf []byte, from, to int) []element {
	var out []element
	for at := from; at < to; {
		e := readElement(buf, at)
		out = append(out, e)
		if e.size < 0 {
			break
		}
		at = e.data + e.size
	}
	return out
}

func uintOf(buf []byte, e element) float64 {
	value := 0.0
	for i := range e.size {
		value = value*256 + float64(buf[e.data+i])
	}
	return value
}

// ParseWebMIndex reads a WebM head: the init range holds EBML header, Segment header,
// SeekHead, Info and Tracks; the index range holds Cues, whose cluster positions are
// relative to the Segment payload. The rebuilt init drops SeekHead and gives the Segment
// an unknown size, so a file of init plus a few clusters reads like a live stream: no
// pointer leads past its end.
func ParseWebMIndex(head []byte, initEnd int, contentLength int64) (*DashIndex, error) {
	return guarded(func() (*DashIndex, error) {
		header := readElement(head, 0)
		if header.id != ebmlHeader {
			return nil, errors.New("not WebM")
		}
		segment := readElement(head, header.data+header.size)
		if segment.id != ebmlSegment {
			return nil, errors.New("no WebM Segment")
		}
		scale := 1e6
		var kept [][]byte
		var cues *element
		for _, e := range children(head, segment.data, len(head)) {
			if e.id == ebmlInfo || e.id == ebmlTracks {
				if e.data+e.size > initEnd+1 {
					return nil, errors.New("truncated WebM header")
				}
				kept = append(kept, head[e.at:e.data+e.size])
				if e.id == ebmlInfo {
					for _, c := range children(head, e.data, e.data+e.size) {
						if c.id == timecodeScale {
							scale = uintOf(head, c)
						}
					}
				}
			} else if e.id == ebmlCues {
				cues = &e
				break
			}
		}
		if cues == nil || cues.data+cues.size > len(head) {
			return nil, errors.New("no WebM Cues")
		}
		type point struct {
			start float64
			from  int64
		}
		var points []point
		for _, p := range children(head, cues.data, cues.data+cues.size) {
			if p.id != cuePoint {
				continue
			}
			t, position := -1.0, -1.0
			for _, c := range children(head, p.data, p.data+p.size) {
				if c.id == cueTime {
					t = uintOf(head, c)
				} else if c.id == cuePositions && position < 0 {
					for _, q := range children(head, c.data, c.data+c.size) {
						if q.id == cueCluster {
							position = uintOf(head, q)
						}
					}
				}
			}
			if t >= 0 && position >= 0 {
				points = append(points, point{t * scale / 1e9, int64(segment.data) + int64(position)})
			}
		}
		if len(points) == 0 {
			return nil, errors.New("empty WebM Cues")
		}
		init := append([]byte(nil), head[:segment.at]...)
		init = append(init, 0x18, 0x53, 0x80, 0x67, 0x01, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff)
		for _, k := range kept {
			init = append(init, k...)
		}
		segments := make([]Segment, len(points))
		for i, p := range points {
			// The last cluster's end is unknown until read; the caller clamps it to the duration.
			s := Segment{Start: p.start, End: math.Inf(1), From: p.from, To: contentLength - 1}
			if i+1 < len(points) {
				s.End, s.To = points[i+1].start, points[i+1].from-1
			}
			segments[i] = s
		}
		return &DashIndex{Init: init, Segments: segments}, nil
	})
}

// Covering is the indexes of the segments that cover [start, end]; at least one.
func Covering(segments []Segment, start, end float64) (int, int) {
	first := len(segments) - 1
	for i, s := range segments {
		if s.End > start {
			first = i
			break
		}
	}
	last := first
	for last+1 < len(segments) && segments[last+1].Start < end {
		last++
	}
	return first, last
}
