package notifications

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"testing"
)

// FuzzSendErrorNeverLeaksSecret is the property-based backstop for the whole
// SendError construction: for ANY (urlStr, secret) pair, wrapping urlStr/secret
// in a *url.Error (or a variant that nests, double-wraps, or reflects the
// secret back as if it were a response body) and classifying it through
// newTransportError must never let either value — raw, query-escaped, or
// path-escaped — reach any rendering of the resulting *SendError.
func FuzzSendErrorNeverLeaksSecret(f *testing.F) {
	seeds := []struct{ urlStr, secret string }{
		// token in query
		{"https://gotify.invalid/message?token=seed-secret-token", "seed-secret-token"},
		// percent-encoded token
		{"https://gotify.invalid/message?token=" + url.QueryEscape("seed/secret+token="), "seed/secret+token="},
		// userinfo
		{"https://seed-user:seed-pass@webhook.invalid/hook", "seed-pass"},
		// path
		{"https://webhook.invalid/seed-secret-path/hook", "seed-secret-path"},
		// fragment
		{"https://webhook.invalid/hook#seed-secret-fragment", "seed-secret-fragment"},
		// malformed URL (embedded space, rejected by url.Parse)
		{"http://exa mple.invalid/hook?token=seed-secret-token", "seed-secret-token"},
		// unicode
		{"https://webhook.invalid/hook?token=seed-sécret-töken", "seed-sécret-töken"},
		// repeated query keys
		{"https://webhook.invalid/hook?token=a&token=seed-secret-token", "seed-secret-token"},
		// mixed escaping
		{"https://webhook.invalid/hook?token=seed%2Bsecret%3Dtoken", "seed+secret=token"},
	}
	for _, s := range seeds {
		f.Add(s.urlStr, s.secret)
	}

	f.Fuzz(func(t *testing.T, urlStr, secret string) {
		// A degenerate short secret/URL (e.g. a single digit) is virtually
		// guaranteed to coincidentally appear in ordinary, safe diagnostic
		// text (a timestamp, a status digit, a letter of "webhook") with no
		// bearing on whether real secret material leaked; that would make the
		// property fail for reasons unrelated to this package's behavior. Real
		// tokens/URLs are always many characters, so restricting the property
		// to inputs at least this long keeps the check meaningful while
		// eliminating that false-failure class.
		const minLen = 8
		if len(secret) < minLen || len(urlStr) < minLen {
			t.Skip()
		}

		innerVariants := []error{
			// Plain string error embedding the URL, as some transports do.
			errors.New("dial tcp " + urlStr + ": connection refused"),
			// The exact shape net/http actually returns.
			&url.Error{Op: "Post", URL: urlStr, Err: errors.New("connection refused")},
			// Double-wrapped via %w, as a regressed caller might produce.
			fmt.Errorf("wrap: %w", &url.Error{Op: "Post", URL: urlStr, Err: errors.New(secret)}),
			// Response-body-reflection shape: the secret appears in a plain
			// error string with no URL structure at all.
			errors.New("remote said: " + secret),
		}

		for _, inner := range innerVariants {
			se := newTransportError("webhook", "send", inner)
			checkSendErrorNeverLeaks(t, se, urlStr, secret)
		}
	})
}

// checkSendErrorNeverLeaks asserts every rendering of se is free of secret,
// its query/path-escaped forms, and the original urlStr, while still being a
// non-empty, provider-naming diagnostic.
func checkSendErrorNeverLeaks(t *testing.T, se *SendError, urlStr, secret string) {
	t.Helper()

	forbidden := []string{secret, url.QueryEscape(secret), url.PathEscape(secret)}
	if urlStr != "" {
		forbidden = append(forbidden, urlStr)
	}

	outputs := []string{
		se.Error(),
		fmt.Sprintf("%v", se),
		fmt.Sprintf(verbPlusV, se),
		fmt.Sprintf(verbHashV, se),
		fmt.Sprintf("%q", se),
	}

	// Drop the timestamp attribute: it is real-clock noise (digits/letters
	// that can coincidentally collide with a fuzzed secret) with nothing to
	// do with whether SendError leaks anything.
	dropTime := &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return a
	}}

	var textBuf, jsonBuf bytes.Buffer
	slog.New(slog.NewTextHandler(&textBuf, dropTime)).Error("send failed", "error", se)
	outputs = append(outputs, textBuf.String())
	slog.New(slog.NewJSONHandler(&jsonBuf, dropTime)).Error("send failed", "error", se)
	outputs = append(outputs, jsonBuf.String())

	for _, out := range outputs {
		for _, forbid := range forbidden {
			if forbid != "" && strings.Contains(out, forbid) {
				t.Fatalf("output leaked secret/url material %q: %s", forbid, out)
			}
		}
	}

	if se.Error() == "" {
		t.Fatal("Error() must not be empty")
	}
	if !strings.Contains(se.Error(), "webhook") {
		t.Fatalf("Error() should name the provider: %s", se.Error())
	}
}
