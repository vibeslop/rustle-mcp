package source

import (
	"regexp"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

var charsetLabel = regexp.MustCompile(`charset=["']?([\w-]+)`)

// encodings are the WHATWG labels of the encodings decoded here. Others (Shift_JIS, GBK,
// windows-1251...) need tables the standard library doesn't have, so those pages are
// read as UTF-8, as an unknown label is; only a page's title can suffer.
var encodings = map[string]string{
	"utf-16be": "utf-16be", "unicodefffe": "utf-16be",
	"utf-16le": "utf-16le", "utf-16": "utf-16le", "unicode": "utf-16le", "unicodefeff": "utf-16le",
	"ucs-2": "utf-16le", "csunicode": "utf-16le", "iso-10646-ucs-2": "utf-16le",
	"windows-1252": "windows-1252", "iso-8859-1": "windows-1252", "iso8859-1": "windows-1252",
	"iso88591": "windows-1252", "iso_8859-1": "windows-1252", "latin1": "windows-1252", "l1": "windows-1252",
	"us-ascii": "windows-1252", "ascii": "windows-1252", "cp1252": "windows-1252", "x-cp1252": "windows-1252",
	"cp819": "windows-1252", "ibm819": "windows-1252", "csisolatin1": "windows-1252", "iso-ir-100": "windows-1252",
}

// decodeBody is the body in the charset its content type names, so a Latin-1 page keeps
// its accented titles. contentType is lowercase.
func decodeBody(body []byte, contentType string) string {
	label := "utf-8"
	if m := charsetLabel.FindStringSubmatch(contentType); m != nil {
		label = m[1]
	}
	switch encodings[label] {
	case "utf-16le":
		return decodeUTF16(body, false)
	case "utf-16be":
		return decodeUTF16(body, true)
	case "windows-1252":
		return decodeWindows1252(body)
	}
	return decodeUTF8(body)
}

// decodeUTF8 is TextDecoder's UTF-8: a leading BOM dropped, and each maximal invalid
// subsequence replaced by one U+FFFD. Go's own conversion replaces byte by byte.
func decodeUTF8(b []byte) string {
	if len(b) >= 3 && b[0] == 0xef && b[1] == 0xbb && b[2] == 0xbf {
		b = b[3:]
	}
	if utf8.Valid(b) {
		return string(b)
	}
	var out strings.Builder
	out.Grow(len(b))
	for i := 0; i < len(b); {
		c := b[i]
		if c < 0x80 {
			out.WriteByte(c)
			i++
			continue
		}
		need, lower, upper := 0, byte(0x80), byte(0xbf)
		var cp rune
		switch {
		case c >= 0xc2 && c <= 0xdf:
			need, cp = 1, rune(c&0x1f)
		case c >= 0xe0 && c <= 0xef:
			if c == 0xe0 {
				lower = 0xa0
			} else if c == 0xed {
				upper = 0x9f
			}
			need, cp = 2, rune(c&0x0f)
		case c >= 0xf0 && c <= 0xf4:
			if c == 0xf0 {
				lower = 0x90
			} else if c == 0xf4 {
				upper = 0x8f
			}
			need, cp = 3, rune(c&0x07)
		default:
			out.WriteRune(utf8.RuneError)
			i++
			continue
		}
		j := i + 1
		for ; need > 0; need-- {
			if j >= len(b) || b[j] < lower || b[j] > upper {
				break
			}
			cp = cp<<6 | rune(b[j]&0x3f)
			lower, upper = 0x80, 0xbf
			j++
		}
		if need == 0 {
			out.WriteRune(cp)
		} else {
			// The byte that broke the sequence starts the next one.
			out.WriteRune(utf8.RuneError)
		}
		i = j
	}
	return out.String()
}

func decodeUTF16(b []byte, bigEndian bool) string {
	if len(b) >= 2 && (bigEndian && b[0] == 0xfe && b[1] == 0xff || !bigEndian && b[0] == 0xff && b[1] == 0xfe) {
		b = b[2:]
	}
	units := make([]uint16, len(b)/2)
	for i := range units {
		if bigEndian {
			units[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
		} else {
			units[i] = uint16(b[2*i+1])<<8 | uint16(b[2*i])
		}
	}
	text := string(utf16.Decode(units))
	if len(b)%2 == 1 {
		text += "�"
	}
	return text
}

// windows1252 is the standard's mapping of bytes 0x80-0x9F; it also serves the labels
// iso-8859-1, latin1 and ascii, as browsers have it.
var windows1252 = [32]rune{
	0x20ac, 0x81, 0x201a, 0x192, 0x201e, 0x2026, 0x2020, 0x2021, 0x2c6, 0x2030, 0x160, 0x2039, 0x152, 0x8d, 0x17d, 0x8f,
	0x90, 0x2018, 0x2019, 0x201c, 0x201d, 0x2022, 0x2013, 0x2014, 0x2dc, 0x2122, 0x161, 0x203a, 0x153, 0x9d, 0x17e, 0x178,
}

func decodeWindows1252(b []byte) string {
	var out strings.Builder
	out.Grow(len(b))
	for _, c := range b {
		switch {
		case c < 0x80:
			out.WriteByte(c)
		case c < 0xa0:
			out.WriteRune(windows1252[c-0x80])
		default:
			out.WriteRune(rune(c))
		}
	}
	return out.String()
}
