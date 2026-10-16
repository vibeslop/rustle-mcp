package source

import "testing"

func TestParseURLNormalizes(t *testing.T) {
	for _, c := range []struct{ in, href, hostname, pathname string }{
		{"https://example.com", "https://example.com/", "example.com", "/"},
		{"HTTPS://Example.COM:443/a#frag", "https://example.com/a#frag", "example.com", "/a"},
		{"http://example.com:80/", "http://example.com/", "example.com", "/"},
		{"http://example.com:8080/x", "http://example.com:8080/x", "example.com", "/x"},
		{"https://example.com/a/../b/./c", "https://example.com/b/c", "example.com", "/b/c"},
		{"https://example.com/a/b/..", "https://example.com/a/", "example.com", "/a/"},
		{"https:example.com/x", "https://example.com/x", "example.com", "/x"},
		{`https:\\example.com\a\b?c\d`, `https://example.com/a/b?c\d`, "example.com", "/a/b"},
		{"https://exa\tmple.com/a\nb", "https://example.com/ab", "example.com", "/ab"},
		{" \u0001https://example.com/ ", "https://example.com/", "example.com", "/"},
		{"https://example.com/a b", "https://example.com/a%20b", "example.com", "/a%20b"},
		{"https://example.com/?a b<", "https://example.com/?a%20b%3C", "example.com", "/"},
		{"https://example.com/é", "https://example.com/%C3%A9", "example.com", "/%C3%A9"},
		{"https://example.com/a%zz", "https://example.com/a%25zz", "example.com", "/a%25zz"},
		{"https://www.youtube.com/watch?v=jNQXAC9IVRw&t=1", "https://www.youtube.com/watch?v=jNQXAC9IVRw&t=1", "www.youtube.com", "/watch"},
		{"https://youtu.be/jNQXAC9IVRw?t=5", "https://youtu.be/jNQXAC9IVRw?t=5", "youtu.be", "/jNQXAC9IVRw"},
		{"file:///a/b", "file:///a/b", "", "/a/b"},
		{"https://", "", "", ""},
		{"not a url", "", "", ""},
		{"/local/path.mp4", "", "", ""},
		{"", "", "", ""},
	} {
		u, ok := ParseURL(c.in)
		if c.href == "" {
			if ok {
				t.Errorf("ParseURL(%q) = %q, want failure", c.in, u.Href())
			}
			continue
		}
		if !ok {
			t.Errorf("ParseURL(%q) failed, want %q", c.in, c.href)
			continue
		}
		if u.Href() != c.href || u.Hostname != c.hostname || u.Pathname() != c.pathname {
			t.Errorf("ParseURL(%q) = %q host %q path %q, want %q host %q path %q", c.in, u.Href(), u.Hostname, u.Pathname(), c.href, c.hostname, c.pathname)
		}
	}
	u, _ := ParseURL("https://user:secret@example.com/")
	if u.Username != "user" || u.Password != "secret" {
		t.Errorf("credentials read as %q, %q", u.Username, u.Password)
	}
}

func TestResolveURLAsALinkInAPage(t *testing.T) {
	const base = "https://example.com/music/page?x=1#h"
	for _, c := range []struct{ ref, href string }{
		{"audio.mp3", "https://example.com/music/audio.mp3"},
		{"/song.mp4", "https://example.com/song.mp4"},
		{"//cdn.example/x", "https://cdn.example/x"},
		{"?q", "https://example.com/music/page?q"},
		{"#f", "https://example.com/music/page?x=1#f"},
		{"../a", "https://example.com/a"},
		{"../../../../a", "https://example.com/a"},
		{"./", "https://example.com/music/"},
		{"..", "https://example.com/"},
		{"a/./b/../c", "https://example.com/music/a/c"},
		{"https:foo", "https://example.com/music/foo"},
		{"https://other/x", "https://other/x"},
		{`\\other\x`, "https://other/x"},
		{`\x`, "https://example.com/x"},
		{"clip.mp4?a=1&b=2", "https://example.com/music/clip.mp4?a=1&b=2"},
		{" x ", "https://example.com/music/x"},
		{"a b", "https://example.com/music/a%20b"},
		{"HTTPS://CDN.example/A", "https://cdn.example/A"},
		{"é", "https://example.com/music/%C3%A9"},
	} {
		u, ok := ResolveURL(c.ref, base)
		got := ""
		if ok {
			got = u.Href()
		}
		if got != c.href {
			t.Errorf("ResolveURL(%q) = %q, want %q", c.ref, got, c.href)
		}
	}
	if _, ok := ResolveURL("https://example.com/", "not a base"); ok {
		t.Error("an invalid base resolves nothing, even an absolute URL")
	}
}

func TestParam(t *testing.T) {
	for _, c := range []struct {
		query, v string
		ok       bool
	}{
		{"v=abc", "abc", true}, {"v=a+b", "a b", true}, {"v=%41&v=x", "A", true}, {"x=1&v", "", true},
		{"v=%zz", "%zz", true}, {"&&v=1", "1", true}, {"V=1&v=2", "2", true}, {"v=1=2", "1=2", true},
		{"v=%C3%A9", "é", true}, {"x=1", "", false}, {"", "", false},
	} {
		u, _ := ParseURL("https://e.com/?" + c.query)
		v, ok := u.Param("v")
		if v != c.v || ok != c.ok {
			t.Errorf("Param(v) of ?%s = %q, %v; want %q, %v", c.query, v, ok, c.v, c.ok)
		}
	}
}

func TestDecodeURIComponent(t *testing.T) {
	for _, c := range []struct {
		in, out string
		ok      bool
	}{
		{"a%20b", "a b", true}, {"%C3%A9", "é", true}, {"a+b", "a+b", true}, {"%2F", "/", true},
		{"100%", "", false}, {"%zz", "", false}, {"%E9", "", false},
	} {
		out, ok := DecodeURIComponent(c.in)
		if out != c.out || ok != c.ok {
			t.Errorf("DecodeURIComponent(%q) = %q, %v; want %q, %v", c.in, out, ok, c.out, c.ok)
		}
	}
}

func TestFileURLToPath(t *testing.T) {
	for _, c := range []struct{ in, path string }{
		{"file:///a/b", "/a/b"}, {"file:///a%20b", "/a b"}, {"file:///a b", "/a b"}, {"file:///a%2Fb", ""},
		{"file:///%E9", ""}, {"file:///%C3%A9", "/é"}, {"file://localhost/x", "/x"}, {"file://host/x", ""},
		{"file:///a?q#h", "/a"}, {"FILE:///x", "/x"}, {"https://example.com/x", ""},
	} {
		path, err := FileURLToPath(c.in)
		if c.path == "" && err == nil || c.path != "" && path != c.path {
			t.Errorf("FileURLToPath(%q) = %q, %v; want %q", c.in, path, err, c.path)
		}
	}
}
