package supportbundle

import (
	"net"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// redactedMarker replaces an entire string the moment it looks like it might
// carry something sensitive. It is a fixed constant - never a function of the
// input's length or content - so the marker itself never leaks how long or
// what shape the original value was.
const redactedMarker = "[REDACTED]"

// entropyRunLen is the minimum length of an unbroken run of token-ish
// characters (letters, digits, and a small set of encoding punctuation) that
// this package treats as "looks like a token/hash/key" even without any
// surrounding "token=" or "Bearer " context. Real prose (game names,
// campaign titles, reason codes) is made of words separated by spaces; a
// single 32+ character unbroken run practically never occurs in it.
const entropyRunLen = 32

// Precompiled patterns for the multi-token or keyword-anchored checks. Single
// -token checks (URL, email, IP, paths, high-entropy runs) are done without
// regexp in looksSensitiveToken, both for clarity and because net.ParseIP is a
// far more precise IP test than any hand-rolled regex.
var (
	// reBearerPhrase matches an HTTP Authorization-style bearer/basic token
	// presented as two space-separated tokens ("Bearer abc123...").
	reBearerPhrase = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9\-_.=]{8,}`)

	// reAssignment matches key=value / key: value assignments for the
	// classic secret-ish key names, wherever they appear in the string.
	reAssignment = regexp.MustCompile(`(?i)\b(access[_-]?token|refresh[_-]?token|token|password|passwd|pwd|secret|client[_-]?secret|api[_-]?key|apikey)\s*[:=]\s*\S+`)
)

// Redact is the defense-in-depth sanitizer applied to every string this
// package writes into the bundle. It is NOT the primary security boundary -
// that is the typed allowlist in Input, which never gives a raw/free-form
// field (tokens, cookies, raw errors, request payloads, ...) anywhere to
// copy from in the first place. Redact exists in case a field this package
// considers "safe" (a game name, a channel login, a reason code) ever ends
// up carrying something it shouldn't.
//
// Behavior: if s contains anything that looks like a bearer/basic token, a
// cookie or Authorization header, a key=value secret assignment, a URL
// (with or without userinfo/query/fragment - ANY scheme://... is treated as
// sensitive, since a signed playback/spade URL is indistinguishable from a
// benign one by shape alone), an email address, an IPv4/IPv6 literal, an
// absolute Unix or Windows path, embedded newlines (a raw header block or
// stack trace), or a long unbroken high-entropy run, the ENTIRE string is
// replaced with the fixed marker "[REDACTED]" - never a partial redaction,
// so no fragment of the sensitive value and no hint of its original length
// survives. Otherwise the string is capped at maxStringLen runes (a pure
// bound, not a security event) and returned unchanged.
func Redact(s string) string {
	if s == "" {
		return s
	}
	if looksSensitive(s) {
		return redactedMarker
	}
	return truncateRunes(s, maxStringLen)
}

// looksSensitive runs every structural check against s.
func looksSensitive(s string) bool {
	if strings.ContainsAny(s, "\n\r") {
		return true
	}
	lower := strings.ToLower(s)
	if strings.Contains(lower, "authorization:") {
		return true
	}
	if strings.Contains(lower, "cookie:") || strings.Contains(lower, "set-cookie:") {
		return true
	}
	if strings.Contains(lower, "webhook") {
		return true
	}
	if reBearerPhrase.MatchString(s) {
		return true
	}
	if reAssignment.MatchString(s) {
		return true
	}

	for _, raw := range strings.Fields(s) {
		tok := strings.Trim(raw, ",;:()[]{}<>\"'")
		if tok == "" {
			continue
		}
		if looksLikeURL(tok) {
			return true
		}
		if looksLikeEmail(tok) {
			return true
		}
		if looksLikeUnixPath(tok) {
			return true
		}
		if looksLikeWindowsPath(tok) {
			return true
		}
		if looksHighEntropy(tok) {
			return true
		}
		if ip := net.ParseIP(tok); ip != nil {
			return true
		}
	}
	return false
}

// looksLikeURL reports whether tok has a "scheme://" prefix followed by
// something. Deliberately broad (no requirement of userinfo/query/fragment):
// per the BKM-016 design, ANY absolute URL is treated as potentially
// sensitive (a signed Twitch spade/playback URL is just a URL with a query
// string, indistinguishable from a benign one without parsing it).
func looksLikeURL(tok string) bool {
	idx := strings.Index(tok, "://")
	if idx < 1 || idx+3 >= len(tok) {
		return false
	}
	scheme := tok[:idx]
	for _, c := range scheme {
		if !unicode.IsLetter(c) && !unicode.IsDigit(c) && c != '+' && c != '-' && c != '.' {
			return false
		}
	}
	return true
}

// looksLikeEmail is a light heuristic (local@domain.tld) - it doesn't need to
// be RFC-5322-perfect, only good enough to catch a real address.
func looksLikeEmail(tok string) bool {
	at := strings.IndexByte(tok, '@')
	if at <= 0 || at >= len(tok)-1 {
		return false
	}
	domain := tok[at+1:]
	dot := strings.LastIndexByte(domain, '.')
	if dot <= 0 || dot >= len(domain)-1 {
		return false
	}
	tld := domain[dot+1:]
	for _, c := range tld {
		if !unicode.IsLetter(c) {
			return false
		}
	}
	return len(tld) >= 2
}

// looksLikeUnixPath requires a token that starts with "/" and has at least
// one more "/" further in, so a single leading slash in an unrelated token
// doesn't false-positive.
func looksLikeUnixPath(tok string) bool {
	if len(tok) < 2 || tok[0] != '/' {
		return false
	}
	return strings.Count(tok, "/") >= 2
}

// looksLikeWindowsPath matches "C:\" / "C:/" style drive-letter paths.
func looksLikeWindowsPath(tok string) bool {
	if len(tok) < 3 {
		return false
	}
	c := tok[0]
	isLetter := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
	return isLetter && tok[1] == ':' && (tok[2] == '\\' || tok[2] == '/')
}

// looksHighEntropy flags a token that is a long, unbroken run of
// letters/digits/base64-ish punctuation containing BOTH a letter and a
// digit - the shape of a real access token, hash, or API key. Ordinary prose
// tokens (words) are excluded because they contain neither: a token that's
// pure letters (no digit) or pure digits (no letter) is left alone, which
// keeps ordinary long words/numbers from being flagged.
func looksHighEntropy(tok string) bool {
	if utf8.RuneCountInString(tok) < entropyRunLen {
		return false
	}
	hasDigit, hasLetter := false, false
	for _, c := range tok {
		switch {
		case c >= '0' && c <= '9':
			hasDigit = true
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
			hasLetter = true
		case c == '+' || c == '/' || c == '=' || c == '_' || c == '-' || c == '.':
			// allowed base64url/hex separator punctuation; doesn't affect the
			// letter/digit determination.
		default:
			return false
		}
	}
	return hasDigit && hasLetter
}

// truncateRunes hard-caps s to max runes without splitting a multi-byte UTF-8
// sequence. This is a pure size bound, not a redaction event - no marker is
// added, so an operator reading the bundle sees a plain, if shortened,
// string.
func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max])
}
