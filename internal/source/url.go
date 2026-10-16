package source

import (
	"net/url"
	"strings"
	"unicode/utf8"
)

// URL is a URL read leniently, the way a browser reads a link or an address bar: what
// net/url leaves as written, it normalizes, so one video has one cache key and one
// canonical source however it was typed.
type URL struct {
	// Scheme and Hostname are lowercase; Port is "" when absent or the scheme's default.
	Scheme   string
	Hostname string
	Port     string
	Username string
	Password string

	u *url.URL
}

var defaultPorts = map[string]string{"http": "80", "https": "443", "ws": "80", "wss": "443", "ftp": "21"}

// special schemes have hosts and slash-separated paths, and read backslashes as slashes.
func special(scheme string) bool {
	_, ok := defaultPorts[scheme]
	return ok || scheme == "file"
}

// ParseURL parses an absolute URL.
func ParseURL(raw string) (*URL, bool) { return parse(raw, nil) }

// ResolveURL parses ref against base, as a link in a page at base.
func ResolveURL(ref, base string) (*URL, bool) {
	b, ok := ParseURL(base)
	if !ok {
		return nil, false
	}
	return parse(ref, b)
}

func parse(raw string, base *URL) (*URL, bool) {
	// Surrounding spaces and control characters go, and tabs and line breaks anywhere: a
	// link copied across lines still works.
	s := strings.TrimFunc(raw, func(r rune) bool { return r <= ' ' })
	s = strings.NewReplacer("\t", "", "\n", "", "\r", "").Replace(s)
	scheme, rest, hasScheme := splitScheme(s)
	if !hasScheme && base == nil {
		return nil, false
	}
	if hasScheme && special(scheme) || !hasScheme && special(base.Scheme) {
		head, tail := s, ""
		if i := strings.IndexAny(s, "?#"); i >= 0 {
			head, tail = s[:i], s[i:]
		}
		s = strings.ReplaceAll(head, `\`, "/") + tail
		if hasScheme {
			_, rest, _ = splitScheme(s)
			// "https:example.com" names a host, unless it continues a page of that scheme.
			if !strings.HasPrefix(rest, "/") && scheme != "file" {
				if base != nil && base.Scheme == scheme {
					s = rest
					hasScheme = false
				} else {
					s = scheme + "://" + rest
				}
			}
		}
	}
	u, err := url.Parse(escapeStrays(s))
	if err != nil {
		return nil, false
	}
	if base != nil {
		u = base.u.ResolveReference(u)
	} else {
		// Resolving an absolute URL against nothing removes its dot segments.
		u = new(url.URL).ResolveReference(u)
	}
	out := &URL{Scheme: strings.ToLower(u.Scheme), Hostname: strings.ToLower(u.Hostname()), Port: u.Port()}
	if out.Port == defaultPorts[out.Scheme] {
		out.Port = ""
	}
	if _, ok := defaultPorts[out.Scheme]; ok && out.Hostname == "" {
		return nil, false
	}
	if u.User != nil {
		out.Username = u.User.Username()
		out.Password, _ = u.User.Password()
	}
	u.Scheme = out.Scheme
	u.Host = out.Hostname
	if strings.Contains(out.Hostname, ":") {
		u.Host = "[" + out.Hostname + "]"
	}
	if out.Port != "" {
		u.Host += ":" + out.Port
	}
	if special(out.Scheme) && u.Opaque == "" && u.Path == "" {
		u.Path = "/"
	}
	u.RawQuery = escapeQuery(u.RawQuery)
	out.u = u
	return out, true
}

// splitScheme splits "scheme:rest"; a scheme starts with a letter.
func splitScheme(s string) (scheme, rest string, ok bool) {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case isAlpha(c):
		case i > 0 && (c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.'):
		case i > 0 && c == ':':
			return strings.ToLower(s[:i]), s[i+1:], true
		default:
			return "", s, false
		}
	}
	return "", s, false
}

func isAlpha(c byte) bool { return c|0x20 >= 'a' && c|0x20 <= 'z' }

func isHex(c byte) bool { return c >= '0' && c <= '9' || c|0x20 >= 'a' && c|0x20 <= 'f' }

// escapeStrays writes a '%' that starts no escape as %25, which net/url would refuse.
func escapeStrays(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && !(i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2])) {
			b.WriteString("%25")
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// escapeQuery escapes what can't stand in a request line: spaces, quotes, angle brackets,
// control characters and non-ASCII bytes. net/url keeps a query as written.
func escapeQuery(q string) string {
	var b strings.Builder
	for i := 0; i < len(q); i++ {
		c := q[i]
		if c <= ' ' || c >= 0x7f || c == '"' || c == '<' || c == '>' || c == '`' {
			b.WriteString("%" + string("0123456789ABCDEF"[c>>4]) + string("0123456789ABCDEF"[c&15]))
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// Pathname is the percent-encoded path.
func (u *URL) Pathname() string {
	if u.u.Opaque != "" {
		return u.u.Opaque
	}
	return u.u.EscapedPath()
}

// Param is the first value named name in the query.
func (u *URL) Param(name string) (string, bool) {
	values, ok := u.u.Query()[name]
	if !ok || len(values) == 0 {
		return "", false
	}
	return values[0], true
}

// DropFragment removes the #fragment.
func (u *URL) DropFragment() { u.u.Fragment, u.u.RawFragment = "", "" }

// Href is the normalized URL.
func (u *URL) Href() string { return u.u.String() }

func (u *URL) String() string { return u.Href() }

// DecodeURIComponent decodes %XX escapes; false on a malformed escape or bytes that aren't UTF-8.
func DecodeURIComponent(s string) (string, bool) {
	if !strings.Contains(s, "%") {
		return s, true
	}
	decoded, err := url.PathUnescape(s)
	if err != nil || !utf8.ValidString(decoded) {
		return "", false
	}
	return decoded, true
}
