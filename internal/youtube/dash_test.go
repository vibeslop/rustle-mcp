package youtube

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
)

func u32(n uint32) []byte { return binary.BigEndian.AppendUint32(nil, n) }

func join(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

func mp4Box(kind string, payload []byte) []byte {
	return join(u32(uint32(8+len(payload))), []byte(kind), payload)
}

// sidxBox is a sidx of references [bytes, duration]; version 1 has 64-bit times.
func sidxBox(version byte, timescale, first, gap uint32, refs [][2]uint32) []byte {
	times := join(u32(first), u32(gap))
	if version == 1 {
		times = join(make([]byte, 4), u32(first), make([]byte, 4), u32(gap))
	}
	entries := []byte{}
	for _, r := range refs {
		entries = join(entries, u32(r[0]), u32(r[1]), u32(0x90000000))
	}
	count := []byte{0, 0, byte(len(refs) >> 8), byte(len(refs))}
	return mp4Box("sidx", join([]byte{version, 0, 0, 0}, u32(1), u32(timescale), times, count, entries))
}

// ebmlEl is an EBML element with a minimal size vint (ids are given with their marker bits).
func ebmlEl(id []byte, payload ...[]byte) []byte {
	data := join(payload...)
	n := len(data)
	var size []byte
	switch {
	case n < 127:
		size = []byte{0x80 | byte(n)}
	case n < 16383:
		size = []byte{0x40 | byte(n>>8), byte(n)}
	default:
		size = []byte{0x10, byte(n >> 16), byte(n >> 8), byte(n)}
	}
	return join(id, size, data)
}

func ebmlUint(n uint32) []byte {
	switch {
	case n < 256:
		return []byte{byte(n)}
	case n < 65536:
		return []byte{byte(n >> 8), byte(n)}
	}
	return u32(n)
}

func TestDashIndex(t *testing.T) {
	t.Run("MP4 sidx maps each fragment to its time span and bytes", func(t *testing.T) {
		init := join(mp4Box("ftyp", []byte("dash0000iso6")), mp4Box("moov", make([]byte, 40)))
		for _, version := range []byte{0, 1} {
			head := join(init, sidxBox(version, 12800, 0, 0, [][2]uint32{{1000, 76800}, {2000, 76800}, {500, 38400}}))
			index, err := ParseMP4Index(head, len(init)-1)
			if err != nil {
				t.Fatal(err)
			}
			if len(index.Init) != len(init) {
				t.Errorf("version %d: init of %d bytes, want %d", version, len(index.Init), len(init))
			}
			n := int64(len(head))
			equal(t, index.Segments, []Segment{
				{Start: 0, End: 6, From: n, To: n + 999},
				{Start: 6, End: 12, From: n + 1000, To: n + 2999},
				{Start: 12, End: 15, From: n + 3000, To: n + 3499},
			}, "segments")
		}
	})

	t.Run("MP4 sidx honours its first offset and earliest presentation time", func(t *testing.T) {
		init := mp4Box("moov", make([]byte, 8))
		head := join(init, sidxBox(0, 1000, 2500, 16, [][2]uint32{{100, 4000}}))
		index, err := ParseMP4Index(head, len(init)-1)
		if err != nil {
			t.Fatal(err)
		}
		n := int64(len(head))
		equal(t, index.Segments, []Segment{{Start: 2.5, End: 6.5, From: n + 16, To: n + 115}}, "segments")
	})

	t.Run("MP4 without sidx is rejected", func(t *testing.T) {
		init := mp4Box("moov", make([]byte, 8))
		if _, err := ParseMP4Index(join(init, mp4Box("free", make([]byte, 4))), len(init)-1); err == nil {
			t.Error("an MP4 without sidx was read")
		}
	})

	t.Run("WebM Cues map clusters to times; the rebuilt init has no SeekHead and an unknown-size Segment", func(t *testing.T) {
		ebml := ebmlEl([]byte{0x1a, 0x45, 0xdf, 0xa3}, ebmlEl([]byte{0x42, 0x82}, []byte("webm")))
		seekHead := ebmlEl([]byte{0x11, 0x4d, 0x9b, 0x74}, ebmlEl([]byte{0x4d, 0xbb}, make([]byte, 6)))
		info := ebmlEl([]byte{0x15, 0x49, 0xa9, 0x66}, ebmlEl([]byte{0x2a, 0xd7, 0xb1}, ebmlUint(1_000_000)))
		tracks := ebmlEl([]byte{0x16, 0x54, 0xae, 0x6b}, ebmlEl([]byte{0xae}, ebmlEl([]byte{0xd7}, ebmlUint(1))))
		point := func(ms, position uint32) []byte {
			return ebmlEl([]byte{0xbb}, ebmlEl([]byte{0xb3}, ebmlUint(ms)), ebmlEl([]byte{0xb7}, ebmlEl([]byte{0xf7}, ebmlUint(1)), ebmlEl([]byte{0xf1}, ebmlUint(position))))
		}
		cues := ebmlEl([]byte{0x1c, 0x53, 0xbb, 0x6b}, point(0, 400), point(6000, 1400), point(12000, 2600))
		segmentHeader := []byte{0x18, 0x53, 0x80, 0x67, 0x01, 0, 0, 0, 0, 0, 0x10, 0}
		head := join(ebml, segmentHeader, seekHead, info, tracks, cues)
		initEnd := len(ebml) + len(segmentHeader) + len(seekHead) + len(info) + len(tracks) - 1
		segmentData := int64(len(ebml) + len(segmentHeader))
		index, err := ParseWebMIndex(head, initEnd, 10_000)
		if err != nil {
			t.Fatal(err)
		}
		equal(t, index.Segments, []Segment{
			{Start: 0, End: 6, From: segmentData + 400, To: segmentData + 1399},
			{Start: 6, End: 12, From: segmentData + 1400, To: segmentData + 2599},
			{Start: 12, End: math.Inf(1), From: segmentData + 2600, To: 9999},
		}, "segments")
		equal(t, index.Init, join(ebml, []byte{0x18, 0x53, 0x80, 0x67, 0x01, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, info, tracks), "init")
	})

	t.Run("WebM cue times follow the TimecodeScale", func(t *testing.T) {
		ebml := ebmlEl([]byte{0x1a, 0x45, 0xdf, 0xa3})
		info := ebmlEl([]byte{0x15, 0x49, 0xa9, 0x66}, ebmlEl([]byte{0x2a, 0xd7, 0xb1}, ebmlUint(100_000)))
		cues := ebmlEl([]byte{0x1c, 0x53, 0xbb, 0x6b}, ebmlEl([]byte{0xbb}, ebmlEl([]byte{0xb3}, ebmlUint(25)), ebmlEl([]byte{0xb7}, ebmlEl([]byte{0xf1}, ebmlUint(9)))))
		segment := []byte{0x18, 0x53, 0x80, 0x67, 0x01, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
		head := join(ebml, segment, info, cues)
		index, err := ParseWebMIndex(head, len(ebml)+len(segment)+len(info)-1, 500)
		if err != nil {
			t.Fatal(err)
		}
		only := index.Segments[0]
		if only.Start != 0.0025 {
			t.Errorf("start %v, want 0.0025", only.Start)
		}
		if want := int64(len(ebml) + len(segment) + 9); only.From != want {
			t.Errorf("from %d, want %d", only.From, want)
		}
	})

	t.Run("covering picks the segments overlapping a span, at least one", func(t *testing.T) {
		var segments []Segment
		for i, start := range []float64{0, 6, 12, 18} {
			segments = append(segments, Segment{Start: start, End: start + 6, From: int64(i * 10), To: int64(i*10 + 9)})
		}
		for _, c := range []struct {
			start, end  float64
			first, last int
		}{
			{7, 7, 1, 1}, {6, 6, 1, 1}, {5.9, 12, 0, 1}, {5.9, 12.01, 0, 2}, {0, 1000, 0, 3}, {500, 501, 3, 3},
		} {
			if first, last := Covering(segments, c.start, c.end); first != c.first || last != c.last {
				t.Errorf("Covering(%v, %v) = [%d, %d], want [%d, %d]", c.start, c.end, first, last, c.first, c.last)
			}
		}
	})

	t.Run("runs groups consecutive segment numbers", func(t *testing.T) {
		equal(t, Runs([]int{3, 4, 5, 9, 11, 12}), [][]int{{3, 4, 5}, {9}, {11, 12}}, "runs")
		equal(t, Runs(nil), [][]int{}, "runs of nothing")
	})
}
