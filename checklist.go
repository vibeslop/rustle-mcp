package main

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// checklist asks which of labels to use, none ticked to begin with, and returns the indexes
// chosen, none when the user cancels. It redraws in place and erases itself when done, so
// what remains on screen is what setup did.
func checklist(t io.ReadWriter, question string, labels []string) []int {
	on := make([]bool, len(labels))
	at, drawn := 0, 0
	style := func(code, s string) string {
		if os.Getenv("NO_COLOR") != "" {
			return s
		}
		return "\x1b[" + code + "m" + s + "\x1b[0m"
	}
	erase := func() string {
		if drawn == 0 {
			return ""
		}
		return fmt.Sprintf("\x1b[%dA\x1b[J", drawn)
	}
	draw := func() {
		var b strings.Builder
		b.WriteString(erase())
		b.WriteString(question + "\n")
		for i, label := range labels {
			pointer := "  "
			if i == at {
				pointer = style("36", "›") + " "
			}
			item := style("36", "●") + " " + label
			if !on[i] {
				item = style("2", "○ "+label)
			}
			b.WriteString(pointer + item + "\n")
		}
		b.WriteString("  " + style("2", "↑↓ move · space toggle · enter confirm") + "\n")
		drawn = len(labels) + 2
		io.WriteString(t, b.String())
	}
	// The cursor is hidden while it would blink over the list.
	io.WriteString(t, "\x1b[?25l")
	defer func() { io.WriteString(t, erase()+"\x1b[?25h") }()
	buf := make([]byte, 64)
	for {
		draw()
		n, err := t.Read(buf)
		if err != nil {
			return nil
		}
		for _, key := range keys(buf[:n]) {
			switch key {
			case "\x1b[A", "\x1bOA", "k":
				at = (at + len(labels) - 1) % len(labels)
			case "\x1b[B", "\x1bOB", "j":
				at = (at + 1) % len(labels)
			case " ":
				on[at] = !on[at]
			case "\r", "\n":
				var chosen []int
				for i := range on {
					if on[i] {
						chosen = append(chosen, i)
					}
				}
				return chosen
			case "\x1b", "\x03", "\x04", "q":
				return nil
			}
		}
	}
}

// keys splits what one read returned into keypresses: an escape sequence such as an arrow's
// is one key, and a lone escape is the escape key.
func keys(b []byte) []string {
	var out []string
	for len(b) > 0 {
		n := 1
		if b[0] == 0x1b && len(b) > 1 && (b[1] == '[' || b[1] == 'O') {
			// Parameters, then a final byte from @ to ~.
			n = 2
			for n < len(b) && (b[n] < 0x40 || b[n] > 0x7e) {
				n++
			}
			n = min(n+1, len(b))
		}
		out = append(out, string(b[:n]))
		b = b[n:]
	}
	return out
}
