package imaging

import (
	"strings"
	"testing"
)

func TestTextWidthCountsCharacters(t *testing.T) {
	for _, c := range []struct {
		text  string
		scale int
		want  int
	}{{"", 2, 0}, {"a", 1, 5}, {"abc", 2, 34}, {"1:02–1:03", 1, 53}} {
		if got := TextWidth(c.text, c.scale); got != c.want {
			t.Errorf("TextWidth(%q, %d) = %d, want %d", c.text, c.scale, got, c.want)
		}
	}
	if TextHeight(2) != 14 {
		t.Error("TextHeight(2) != 14")
	}
}

// drawn is the lit pixels of a w x h canvas as rows of '#' and '.'.
func drawn(rgb []byte, w, h int) string {
	var b strings.Builder
	for y := range h {
		if y > 0 {
			b.WriteByte('|')
		}
		for x := range w {
			if rgb[(y*w+x)*3] != 0 {
				b.WriteByte('#')
			} else {
				b.WriteByte('.')
			}
		}
	}
	return b.String()
}

func TestDrawText(t *testing.T) {
	canvas := func(text string, x, y int) string {
		rgb := make([]byte, 5*7*3)
		DrawText(rgb, 5, 7, x, y, text, 1, [3]byte{255, 255, 255})
		return drawn(rgb, 5, 7)
	}
	if got := canvas("1", 0, 0); got != glyphArt['1'] {
		t.Errorf("1 drew %s", got)
	}
	// Aliases draw their ASCII stand-in; a character outside the font draws as "?".
	if got := canvas("—", 0, 0); got != glyphArt['-'] {
		t.Errorf("em dash drew %s", got)
	}
	if got := canvas("é", 0, 0); got != glyphArt['?'] {
		t.Errorf("é drew %s", got)
	}
	// Glyphs past the edges are clipped.
	if got := canvas("1", -2, 1); got != ".....|#....|#....|#....|#....|#....|#...." {
		t.Errorf("shifted 1 drew %s", got)
	}

	// At scale 2 each pixel of the glyph is a 2 x 2 square, with its colour in all three bytes.
	rgb := make([]byte, 10*14*3)
	DrawText(rgb, 10, 14, 0, 0, "1", 2, [3]byte{1, 2, 3})
	if rgb[(0*10+4)*3] != 1 || rgb[(1*10+5)*3+2] != 3 || rgb[(0*10+3)*3] != 0 {
		t.Error("scale 2 did not double the glyph")
	}
}

func TestFillRectClips(t *testing.T) {
	rgb := make([]byte, 4*3*3)
	FillRect(rgb, 4, 3, -1, 1, 3, 5, [3]byte{9, 9, 9})
	if got := drawn(rgb, 4, 3); got != "....|##..|##.." {
		t.Errorf("drew %s", got)
	}
}
