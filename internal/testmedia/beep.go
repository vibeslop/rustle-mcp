// Package testmedia generates the media tests listen to and look at, so the repository
// keeps no recordings.
package testmedia

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"runtime"

	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

// WriteBeep writes the beep the audio tests listen to, written when they run rather than
// kept as a 1.1 MB file: 6 s of 48 kHz 16-bit stereo, silent on the left, a 1 kHz tone at
// half scale on the right from 3 s to 3.25 s, faded in and out over 5 ms so its edges
// don't click.
func WriteBeep(path string) error {
	const rate, frames, start, length, fade = 48000, 6 * 48000, 3 * 48000, 48000 / 4, 48000 / 200
	wav := make([]byte, 44+frames*4)
	copy(wav[0:], "RIFF")
	binary.LittleEndian.PutUint32(wav[4:], 36+frames*4)
	copy(wav[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(wav[16:], 16)
	binary.LittleEndian.PutUint16(wav[20:], 1)
	binary.LittleEndian.PutUint16(wav[22:], 2)
	binary.LittleEndian.PutUint32(wav[24:], rate)
	binary.LittleEndian.PutUint32(wav[28:], rate*4)
	binary.LittleEndian.PutUint16(wav[32:], 4)
	binary.LittleEndian.PutUint16(wav[34:], 16)
	copy(wav[36:], "data")
	binary.LittleEndian.PutUint32(wav[40:], frames*4)
	// The phase is computed in steps, each rounded, rather than as one exact constant, so
	// the samples stay what the audio tests' expectations were measured on.
	twoPi := 2 * math.Pi
	for k := range length {
		envelope := min(1, float64(k)/fade, float64(length-k)/fade)
		value := 0.5 * envelope * math.Sin(twoPi*1000*float64(k)/rate) * 32767
		// Rounded away from zero, so troughs mirror peaks; rounding alone takes -16383.5 to -16383.
		sample := int16(math.Copysign(rustle.Round(math.Abs(value)), value))
		binary.LittleEndian.PutUint16(wav[44+(start+k)*4+2:], uint16(sample))
	}
	return os.WriteFile(path, wav, 0o644)
}

// Inspection is the path of the inspection video (1280 x 720, 12 s): a code label from
// 2 s, and a magenta box at [650, 250, 1050, 550] from 5.0 to 5.5 s.
func Inspection() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "testdata", "inspection.mp4")
}
