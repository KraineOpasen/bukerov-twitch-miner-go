package supportbundle

import (
	"strings"
	"testing"
)

func TestRedactLeavesBenignStringsAlone(t *testing.T) {
	cases := []string{
		"",
		"Just Chatting",
		"streamername",
		"priority: configured channel",
		"channel stability: insufficient data (3/5 reports)",
		"per-drop rule: High priority",
		"drop progress stalled on this channel despite session recovery",
		"GAME_ORDER",
		"1.2.3",
		"go1.25.0",
		"linux/amd64",
	}
	for _, s := range cases {
		if got := Redact(s); got != s {
			t.Errorf("Redact(%q) = %q, want unchanged", s, got)
		}
	}
}

// TestRedactLeavesChannelPointsReasonUnchanged documents WHY the BKM-016 fix
// removed supportbundle.WatchSlot.Reason / StreamerEntry.Reason entirely
// instead of relying on Redact: a channel-points balance embedded in a
// free-form selection-reason string doesn't match any pattern Redact checks
// for (no bearer/basic phrase, no key=value assignment, no URL/email/IP/path,
// no high-entropy run - a bare decimal integer has digits but no letters, so
// looksHighEntropy never flags it either), so Redact passes it through
// unchanged.
func TestRedactLeavesChannelPointsReasonUnchanged(t *testing.T) {
	s := "watched: selected by POINTS_DESCENDING priority (918273645 channel points)"
	if got := Redact(s); got != s {
		t.Errorf("Redact(%q) = %q, want unchanged", s, got)
	}
}

func TestRedactCapsLongBenignStrings(t *testing.T) {
	long := strings.Repeat("word ", 200) // plain prose, no dangerous shape, > maxStringLen
	got := Redact(long)
	if got == long {
		t.Fatal("expected the long string to be truncated")
	}
	if n := len([]rune(got)); n != maxStringLen {
		t.Errorf("truncated length = %d, want %d", n, maxStringLen)
	}
	if !strings.HasPrefix(long, got) {
		t.Error("truncation must keep the original PREFIX, not rewrite content")
	}
}

func TestRedactMarksSensitivePatterns(t *testing.T) {
	cases := map[string]string{
		"bearer token":           "Authorization: Bearer sometokenvalue1234",
		"bearer phrase alone":    "Bearer abcdefgh12345678",
		"basic auth":             "Basic dXNlcjpwYXNz",
		"cookie header":          "Cookie: session=abc123def456",
		"set-cookie header":      "Set-Cookie: session=abc123def456; Path=/",
		"token assignment":       "token=abc123def456ghijkl",
		"password assignment":    "password: hunter2hunter2",
		"client secret assign":   "client_secret=abcdef0123456789",
		"https URL":              "https://example.com/callback?code=abc",
		"URL with userinfo":      "https://user:pass@example.com/",
		"webhook URL":            "https://discord.com/api/webhooks/123/abcDEF",
		"webhook word no scheme": "posts to a webhook endpoint internally",
		"spade-like signed URL":  "https://video-edge-abc.example.net/spade?sig=abc&token=def",
		"email address":          "person@example.com",
		"IPv4 literal":           "connection from 203.0.113.7 refused",
		"IPv6 literal":           "connection from 2001:db8::1 refused",
		"absolute unix path":     "read failed at /home/user/.config/secret.json",
		"windows drive path":     `failed reading C:\Users\alice\secrets.txt`,
		"multiline block":        "line one\nAuthorization: Bearer xyz",
		"high entropy run":       "Xk9Qm2Pv7Rt4Ws8Zc6Fh0Jd5Lg8Nn3Ss7Uu",
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			got := Redact(s)
			if got != redactedMarker {
				t.Errorf("Redact(%q) = %q, want the redaction marker", s, got)
			}
			if strings.Contains(got, "abc") || strings.Contains(got, "123") {
				t.Errorf("Redact(%q) = %q leaks a fragment of the input", s, got)
			}
		})
	}
}

func TestRedactMarkerNeverEncodesLength(t *testing.T) {
	short := "token=abc"
	long := "token=" + strings.Repeat("x", 5000)
	gotShort := Redact(short)
	gotLong := Redact(long)
	if gotShort != gotLong {
		t.Errorf("redaction marker differs by input length: %q vs %q", gotShort, gotLong)
	}
	if gotShort != redactedMarker {
		t.Errorf("Redact(%q) = %q, want the fixed marker", short, gotShort)
	}
}

func TestLooksLikeURLRequiresScheme(t *testing.T) {
	cases := map[string]bool{
		"https://example.com": true,
		"http://x":            true,
		"ftp://host/path":     true,
		"not a url":           false,
		"a://":                false, // nothing after the scheme
		"a/b://c":             false,
		"just/a/path":         false,
	}
	for tok, want := range cases {
		if got := looksLikeURL(tok); got != want {
			t.Errorf("looksLikeURL(%q) = %v, want %v", tok, got, want)
		}
	}
}

func TestLooksLikeEmail(t *testing.T) {
	cases := map[string]bool{
		"user@example.com":     true,
		"a.b+c@sub.example.io": true,
		"not-an-email":         false,
		"@example.com":         false,
		"user@":                false,
		"user@nodot":           false,
	}
	for tok, want := range cases {
		if got := looksLikeEmail(tok); got != want {
			t.Errorf("looksLikeEmail(%q) = %v, want %v", tok, got, want)
		}
	}
}

func TestLooksLikeUnixPath(t *testing.T) {
	cases := map[string]bool{
		"/home/user/file.json": true,
		"/etc/passwd":          true, // two slashes total: before "etc" and before "passwd"
		"/single":              false,
		"relative/path":        false,
		"":                     false,
	}
	for tok, want := range cases {
		if got := looksLikeUnixPath(tok); got != want {
			t.Errorf("looksLikeUnixPath(%q) = %v, want %v", tok, got, want)
		}
	}
}

func TestLooksLikeWindowsPath(t *testing.T) {
	cases := map[string]bool{
		`C:\Users\alice`: true,
		`d:/data/file`:   true,
		"not a path":     false,
		"C:":             false,
	}
	for tok, want := range cases {
		if got := looksLikeWindowsPath(tok); got != want {
			t.Errorf("looksLikeWindowsPath(%q) = %v, want %v", tok, got, want)
		}
	}
}

func TestLooksHighEntropy(t *testing.T) {
	cases := map[string]bool{
		strings.Repeat("a", 40):                    false, // all-letter, no digit
		strings.Repeat("1", 40):                    false, // all-digit, no letter
		"abcdefghij0123456789ABCDEFGHIJ0123456789": true,  // mixed, long enough
		"short1":                    false, // too short
		strings.Repeat("word ", 20): false, // spaces break up the run
	}
	for tok, want := range cases {
		if got := looksHighEntropy(tok); got != want {
			t.Errorf("looksHighEntropy(%q) = %v, want %v", tok, got, want)
		}
	}
}
