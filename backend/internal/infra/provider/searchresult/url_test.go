package searchresult

import (
	"net/url"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestNormalizeURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
		ok   bool
	}{
		{name: "https", raw: " HTTPS://Example.COM/path?q=1#fragment ", want: "https://example.com/path?q=1#fragment", ok: true},
		{name: "http", raw: "http://example.com", want: "http://example.com", ok: true},
		{name: "script", raw: "javascript:alert(1)"},
		{name: "file", raw: "file:///etc/passwd"},
		{name: "relative", raw: "/relative/path"},
		{name: "credentials", raw: "https://user:secret@example.com/private"},
		{name: "control", raw: "https://example.com/\nprivate"},
		{name: "bidi control", raw: "https://example.com/\u202Ehidden"},
		{name: "oversized", raw: "https://example.com/" + strings.Repeat("a", maxURLBytes)},
		{name: "encoding expansion", raw: "https://example.com/#" + strings.Repeat("´", maxURLBytes/2)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := NormalizeURL(test.raw)
			if ok != test.ok || got != test.want {
				t.Fatalf("NormalizeURL(%q) = %q, %v; want %q, %v", test.raw, got, ok, test.want, test.ok)
			}
		})
	}
}

func TestNormalizeTitle(t *testing.T) {
	value := NormalizeTitle(strings.Repeat("界", MaxTitleRunes+10), "fallback")
	if utf8.RuneCountInString(value) != MaxTitleRunes {
		t.Fatalf("title runes = %d", utf8.RuneCountInString(value))
	}
	if got := NormalizeTitle("  ", " fallback "); got != "fallback" {
		t.Fatalf("fallback title = %q", got)
	}
	if got := NormalizeTitle("Safe\x1b[31m\nTitle\u202E", "fallback"); got != "Safe[31m Title" {
		t.Fatalf("control-safe title = %q", got)
	}
	if got := NormalizeTitle("\x1b\u202E", "Clean fallback"); got != "Clean fallback" {
		t.Fatalf("sanitized-empty fallback title = %q", got)
	}
}

func FuzzNormalizeURL(f *testing.F) {
	for _, seed := range []string{
		"https://example.com/path?q=1",
		"http://localhost:8080/test",
		"javascript:alert(1)",
		"https://user:pass@example.com",
		"\x00https://example.com",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		normalized, ok := NormalizeURL(raw)
		if !ok {
			return
		}
		parsed, err := url.Parse(normalized)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil {
			t.Fatalf("unsafe normalized URL %q from %q", normalized, raw)
		}
		if len(normalized) > maxURLBytes || strings.IndexFunc(normalized, func(r rune) bool {
			return unicode.IsControl(r) || unicode.In(r, unicode.Cf)
		}) >= 0 {
			t.Fatalf("unbounded normalized URL %q", normalized)
		}
	})
}
