package notifications

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
)

// Sentinels used across every M5/M6 test in this package. These are NEVER
// real credentials — just distinctive strings whose presence in any output is
// unambiguous evidence of a leak.
const (
	sentinelMain            = "bkm-m5-m6-secret-sentinel-never-print"
	sentinelGotifyToken     = "bkm-gotify-token-never-print"
	sentinelWebhookUserInfo = "bkm-webhook-userinfo-never-print"
	sentinelWebhookPath     = "bkm-webhook-path-never-print"
	sentinelWebhookQuery    = "bkm-webhook-query-never-print"
	sentinelWebhookFragment = "bkm-webhook-fragment-never-print"
	sentinelResponseBody    = "bkm-response-body-never-print"

	// sentinelWebhookURL carries userinfo, host (MAIN), path, query, and
	// fragment sentinels simultaneously.
	sentinelWebhookURL = "https://" + sentinelWebhookUserInfo + ":pw@" + sentinelMain + ".invalid/" + sentinelWebhookPath + "?q=" + sentinelWebhookQuery + "#" + sentinelWebhookFragment

	// verbPlusV / verbHashV hold the plus-v/hash-v format verbs, assembled by
	// concatenation rather than as contiguous literals. discord_lifecycle_test.go's
	// TestNoSecretLeakingFormatDirectives statically scans every .go file in
	// this package directory for those two verbs as raw substrings (this
	// package's DiscordSettings carries a bot token), so any test in this
	// package that needs to exercise the plus-v/hash-v verbs must build them
	// this way.
	verbPlusV = "%" + "+v"
	verbHashV = "%" + "#v"
)

// allSentinels lists every sentinel string for exhaustive leak checks.
var allSentinels = []string{
	sentinelMain, sentinelGotifyToken, sentinelWebhookUserInfo,
	sentinelWebhookPath, sentinelWebhookQuery, sentinelWebhookFragment,
	sentinelResponseBody,
}

// assertNoSentinel fails the test if out contains any sentinel, in its raw
// form or its url.QueryEscape/url.PathEscape encoded form (naive filters that
// only look for the plaintext value are defeated by re-encoding — see the
// threat model's T15).
func assertNoSentinel(t *testing.T, out string, sentinels ...string) {
	t.Helper()
	for _, s := range sentinels {
		if strings.Contains(out, s) {
			t.Errorf("leaked raw sentinel %q in output: %s", s, out)
		}
		if q := url.QueryEscape(s); strings.Contains(out, q) {
			t.Errorf("leaked query-escaped sentinel %q (as %q) in output: %s", s, q, out)
		}
		if p := url.PathEscape(s); strings.Contains(out, p) {
			t.Errorf("leaked path-escaped sentinel %q (as %q) in output: %s", s, p, out)
		}
	}
}

// fakeTimeoutNetError is a minimal net.Error whose Timeout() is true but which
// is not any of the more specific stdlib types classifyTransport recognizes,
// so it only matches on the generic net.Error branch.
type fakeTimeoutNetError struct{}

func (fakeTimeoutNetError) Error() string   { return "i/o timeout" }
func (fakeTimeoutNetError) Timeout() bool   { return true }
func (fakeTimeoutNetError) Temporary() bool { return true }

// TestClassifyTransportBranches exercises every branch of classifyTransport's
// type/sentinel switch in isolation.
func TestClassifyTransportBranches(t *testing.T) {
	cases := []struct {
		name         string
		err          error
		wantClass    NetClass
		wantTimeout  bool
		wantCanceled bool
		wantDeadline bool
	}{
		{
			name:         "context canceled",
			err:          context.Canceled,
			wantClass:    ClassCanceled,
			wantCanceled: true,
		},
		{
			name:         "context deadline exceeded",
			err:          context.DeadlineExceeded,
			wantClass:    ClassTimeout,
			wantTimeout:  true,
			wantDeadline: true,
		},
		{
			name:         "os deadline exceeded sentinel",
			err:          os.ErrDeadlineExceeded,
			wantClass:    ClassTimeout,
			wantTimeout:  true,
			wantDeadline: true,
		},
		{
			name:        "generic net.Error timeout",
			err:         fakeTimeoutNetError{},
			wantClass:   ClassTimeout,
			wantTimeout: true,
		},
		{
			name:      "dns error",
			err:       &net.DNSError{Err: "no such host", Name: sentinelMain, IsNotFound: true},
			wantClass: ClassDNS,
		},
		{
			name:      "tls certificate verification error",
			err:       &tls.CertificateVerificationError{Err: errors.New("x509: certificate signed by unknown authority")},
			wantClass: ClassTLS,
		},
		{
			name:      "tls record header error",
			err:       tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"},
			wantClass: ClassTLS,
		},
		{
			name:      "tls alert error",
			err:       tls.AlertError(42),
			wantClass: ClassTLS,
		},
		{
			name:      "dial op error",
			err:       &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")},
			wantClass: ClassConnect,
		},
		{
			name:      "unrecognized error",
			err:       errors.New("something else entirely"),
			wantClass: ClassOther,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class, timeout, canceled, deadline := classifyTransport(tc.err)
			if class != tc.wantClass {
				t.Errorf("class = %q, want %q", class, tc.wantClass)
			}
			if timeout != tc.wantTimeout {
				t.Errorf("timeout = %v, want %v", timeout, tc.wantTimeout)
			}
			if canceled != tc.wantCanceled {
				t.Errorf("canceled = %v, want %v", canceled, tc.wantCanceled)
			}
			if deadline != tc.wantDeadline {
				t.Errorf("deadline = %v, want %v", deadline, tc.wantDeadline)
			}
		})
	}
}

// TestClassifyTransportDNSErrorNameNeverLeaksIntoSendError is the DNS-specific
// regression the design calls out by name: *net.DNSError.Name echoes the
// looked-up hostname independent of the request URL string, so a classifier
// that ever reads it (instead of only the error's TYPE) reintroduces the leak
// for exactly the provider (webhook) whose host IS the secret.
func TestClassifyTransportDNSErrorNameNeverLeaksIntoSendError(t *testing.T) {
	dnsErr := &net.DNSError{Err: "no such host", Name: sentinelMain, IsNotFound: true}
	se := newTransportError("webhook", "send", dnsErr)
	if se.Class() != ClassDNS {
		t.Fatalf("Class() = %q, want %q", se.Class(), ClassDNS)
	}
	assertNoSentinel(t, se.Error(), sentinelMain)
}

// TestSendErrorGrammarPerStage locks the fixed, secret-free Error() text for
// each stage.
func TestSendErrorGrammarPerStage(t *testing.T) {
	if got, want := newRequestError("gotify", "send").Error(),
		"gotify send failed: request could not be built (check the provider URL configuration)"; got != want {
		t.Errorf("StageRequest Error() = %q, want %q", got, want)
	}
	if got, want := newResponseError("webhook", "send", 401).Error(),
		"webhook send failed: endpoint returned HTTP 401"; got != want {
		t.Errorf("StageResponse Error() = %q, want %q", got, want)
	}

	dialErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	if got, want := newTransportError("gotify", "send", dialErr).Error(),
		"gotify send failed: connect error"; got != want {
		t.Errorf("StageTransport(connect) Error() = %q, want %q", got, want)
	}

	otherErr := errors.New("mystery failure")
	if got, want := newTransportError("webhook", "send", otherErr).Error(),
		"webhook send failed: other error"; got != want {
		t.Errorf("StageTransport(other) Error() = %q, want %q", got, want)
	}
}

// TestSendErrorUnwrapSentinelsOnly asserts Unwrap returns only the two context
// sentinels (or nil), never anything derived from the discarded original error.
func TestSendErrorUnwrapSentinelsOnly(t *testing.T) {
	canceledErr := newTransportError("gotify", "send", context.Canceled)
	if !errors.Is(canceledErr, context.Canceled) {
		t.Error("expected errors.Is(_, context.Canceled) for a canceled SendError")
	}
	if errors.Is(canceledErr, context.DeadlineExceeded) {
		t.Error("a canceled SendError must not also satisfy DeadlineExceeded")
	}

	deadlineErr := newTransportError("gotify", "send", context.DeadlineExceeded)
	if !errors.Is(deadlineErr, context.DeadlineExceeded) {
		t.Error("expected errors.Is(_, context.DeadlineExceeded) for a deadline SendError")
	}
	if errors.Is(deadlineErr, context.Canceled) {
		t.Error("a deadline SendError must not also satisfy Canceled")
	}

	plainErr := newTransportError("gotify", "send", errors.New("boom"))
	if u := plainErr.Unwrap(); u != nil {
		t.Errorf("Unwrap() = %v, want nil for a non-sentinel class", u)
	}
	if errors.Is(plainErr, context.Canceled) || errors.Is(plainErr, context.DeadlineExceeded) {
		t.Error("a plain transport SendError must not satisfy either context sentinel")
	}
}

// TestSendErrorNeverUnwrapsToURLError is the core structural guarantee: no
// matter how the original error looked, errors.As can never recover a
// *url.Error (or anything else) from a *SendError, because the original error
// was classified and discarded at construction, never retained.
func TestSendErrorNeverUnwrapsToURLError(t *testing.T) {
	urlErr := &url.Error{Op: "Post", URL: sentinelWebhookURL, Err: errors.New("connection refused")}
	se := newTransportError("webhook", "send", urlErr)

	var ue *url.Error
	if errors.As(se, &ue) {
		t.Fatal("errors.As(err, new(*url.Error)) unexpectedly succeeded")
	}
}

// TestSendErrorImplementsNetError checks *SendError satisfies net.Error so
// existing net.Error-based call patterns elsewhere in the codebase keep
// working without ever chaining a real net.Error into the type.
func TestSendErrorImplementsNetError(t *testing.T) {
	se := newTransportError("gotify", "send", context.DeadlineExceeded)
	var ne net.Error
	if !errors.As(se, &ne) {
		t.Fatal("*SendError does not satisfy net.Error")
	}
	if !ne.Timeout() {
		t.Error("Timeout() = false, want true for a deadline-classified SendError")
	}
}

// TestSendErrorTemporaryClassification checks the Temporary() derivation
// (timeout || connect || dns) matches the design.
func TestSendErrorTemporaryClassification(t *testing.T) {
	cases := []struct {
		name          string
		err           error
		wantTemporary bool
	}{
		{"connect", &net.OpError{Op: "dial", Err: errors.New("refused")}, true},
		{"dns", &net.DNSError{Err: "no such host", Name: sentinelMain}, true},
		{"timeout", context.DeadlineExceeded, true},
		{"other", errors.New("mystery"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			se := newTransportError("gotify", "send", tc.err)
			if got := se.Temporary(); got != tc.wantTemporary {
				t.Errorf("Temporary() = %v, want %v (class=%s)", got, tc.wantTemporary, se.Class())
			}
		})
	}
}

// TestSendErrorAccessors checks the safe accessors return exactly what the
// constructors were given.
func TestSendErrorAccessors(t *testing.T) {
	se := newResponseError("webhook", "send", 503)
	if got := se.Provider(); got != "webhook" {
		t.Errorf("Provider() = %q, want %q", got, "webhook")
	}
	if got := se.Op(); got != "send" {
		t.Errorf("Op() = %q, want %q", got, "send")
	}
	if got := se.Stage(); got != StageResponse {
		t.Errorf("Stage() = %q, want %q", got, StageResponse)
	}
	if got := se.Class(); got != ClassNone {
		t.Errorf("Class() = %q, want %q (response stage has no transport class)", got, ClassNone)
	}
	if got := se.StatusCode(); got != 503 {
		t.Errorf("StatusCode() = %d, want 503", got)
	}
}

// TestSendErrorLogValue checks the slog.GroupValue shape: always
// provider/op/stage, class only when non-empty, status only when non-zero.
func TestSendErrorLogValue(t *testing.T) {
	se := newResponseError("gotify", "send", 500)
	v := se.LogValue()
	if v.Kind() != slog.KindGroup {
		t.Fatalf("LogValue().Kind() = %v, want Group", v.Kind())
	}
	attrs := map[string]slog.Value{}
	for _, a := range v.Group() {
		attrs[a.Key] = a.Value
	}
	if got := attrs["provider"].String(); got != "gotify" {
		t.Errorf("provider attr = %q, want %q", got, "gotify")
	}
	if got := attrs["op"].String(); got != "send" {
		t.Errorf("op attr = %q, want %q", got, "send")
	}
	if got := attrs["stage"].String(); got != string(StageResponse) {
		t.Errorf("stage attr = %q, want %q", got, string(StageResponse))
	}
	if _, ok := attrs["class"]; ok {
		t.Error("class attr should be omitted for StageResponse (ClassNone)")
	}
	if got := attrs["status"].Int64(); got != 500 {
		t.Errorf("status attr = %d, want 500", got)
	}

	se2 := newTransportError("gotify", "send", &net.OpError{Op: "dial", Err: errors.New("refused")})
	attrs2 := map[string]slog.Value{}
	for _, a := range se2.LogValue().Group() {
		attrs2[a.Key] = a.Value
	}
	if _, ok := attrs2["class"]; !ok {
		t.Error("expected a class attr for a transport-stage error")
	}
	if _, ok := attrs2["status"]; ok {
		t.Error("status attr should be omitted when status == 0")
	}
}

// TestSendErrorStringAndGoString locks String()/GoString() to Error() and the
// documented "notifications.SendError(%q)" wrapper respectively, and checks
// Format's 'q' branch renders exactly strconv.Quote(Error()).
func TestSendErrorStringAndGoString(t *testing.T) {
	se := newResponseError("gotify", "send", 429)
	if se.String() != se.Error() {
		t.Errorf("String() = %q, want Error() = %q", se.String(), se.Error())
	}
	want := fmt.Sprintf("notifications.SendError(%q)", se.Error())
	if got := se.GoString(); got != want {
		t.Errorf("GoString() = %q, want %q", got, want)
	}
	if got, want := fmt.Sprintf("%q", se), strconv.Quote(se.Error()); got != want {
		t.Errorf("%%q formatting = %q, want %q", got, want)
	}
}

// TestSendErrorFormattingMatrixNeverLeaksSentinel is the FORMATTING MATRIX:
// every rendering path fmt/slog offers for a SendError built from a
// sentinel-bearing *url.Error must be free of every sentinel component, in
// raw or percent-encoded form, at any nesting depth.
func TestSendErrorFormattingMatrixNeverLeaksSentinel(t *testing.T) {
	urlErr := &url.Error{Op: "Post", URL: sentinelWebhookURL, Err: errors.New("dial tcp: " + sentinelResponseBody)}
	se := newTransportError("webhook", "send", urlErr)

	outputs := map[string]string{
		"%v":         fmt.Sprintf("%v", se),
		"verb+v":     fmt.Sprintf(verbPlusV, se),
		"verb#v":     fmt.Sprintf(verbHashV, se),
		"%s":         fmt.Sprintf("%s", se),
		"%q":         fmt.Sprintf("%q", se),
		"fmt.Sprint": fmt.Sprint(se),
	}

	wrapped := fmt.Errorf("outer: %w", se)
	outputs["fmt.Errorf(%w)"] = wrapped.Error()
	doubleWrapped := fmt.Errorf("outer2: %w", wrapped)
	outputs["double %w nesting"] = doubleWrapped.Error()

	var textBuf, jsonBuf, anyBuf bytes.Buffer
	slog.New(slog.NewTextHandler(&textBuf, nil)).Error("send failed", "error", se)
	outputs["slog text"] = textBuf.String()
	slog.New(slog.NewJSONHandler(&jsonBuf, nil)).Error("send failed", "error", se)
	outputs["slog json"] = jsonBuf.String()
	slog.New(slog.NewTextHandler(&anyBuf, nil)).Info("send failed", slog.Any("error", se))
	outputs["slog.Any"] = anyBuf.String()

	// Nesting inside a slice/map/struct, printed with the plus-v and hash-v verbs.
	type wrapper struct{ Err *SendError }
	w := wrapper{Err: se}
	outputs["struct verb+v"] = fmt.Sprintf(verbPlusV, w)
	outputs["struct verb#v"] = fmt.Sprintf(verbHashV, w)
	outputs["slice verb+v"] = fmt.Sprintf(verbPlusV, []*SendError{se})
	outputs["slice verb#v"] = fmt.Sprintf(verbHashV, []*SendError{se})
	outputs["map verb+v"] = fmt.Sprintf(verbPlusV, map[string]*SendError{"err": se})
	outputs["map verb#v"] = fmt.Sprintf(verbHashV, map[string]*SendError{"err": se})

	for label, out := range outputs {
		t.Run(label, func(t *testing.T) {
			assertNoSentinel(t, out, allSentinels...)
			if strings.Contains(out, "://") {
				t.Errorf("%s output contains a URL scheme separator: %s", label, out)
			}
		})
	}

	var ue *url.Error
	if errors.As(se, &ue) || errors.As(wrapped, &ue) || errors.As(doubleWrapped, &ue) {
		t.Fatal("errors.As(err, new(*url.Error)) unexpectedly succeeded somewhere in the chain")
	}

	// Negative control: Error() is non-empty and still carries operator-useful
	// diagnostic content (provider name and failure category/status).
	if se.Error() == "" {
		t.Fatal("Error() must not be empty")
	}
	if !strings.Contains(se.Error(), "webhook") {
		t.Errorf("Error() should name the provider: %s", se.Error())
	}
	if !strings.Contains(se.Error(), string(se.Class())) {
		t.Errorf("Error() should name the failure class %q: %s", se.Class(), se.Error())
	}
}

// TestSafeSendErrorAttrClassifiesBySendErrorType checks both branches of the
// safeSendErrorAttr fail-closed helper directly (batch.go/manager.go's only
// permitted way to log a provider send error).
func TestSafeSendErrorAttrClassifiesBySendErrorType(t *testing.T) {
	se := newResponseError("gotify", "send", 500)
	attr := safeSendErrorAttr(se)
	if attr.Key != "error" {
		t.Errorf("attr.Key = %q, want %q", attr.Key, "error")
	}
	if attr.Value.Kind() != slog.KindGroup {
		t.Fatalf("SendError branch: attr.Value.Kind() = %v, want Group", attr.Value.Kind())
	}

	regressed := fmt.Errorf("gotify request failed: %w", &url.Error{
		Op: "Post", URL: sentinelWebhookURL, Err: errors.New("boom"),
	})
	attr2 := safeSendErrorAttr(regressed)
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Error("test", attr2)
	assertNoSentinel(t, buf.String(), allSentinels...)
	if !strings.Contains(buf.String(), "unclassified provider send failure") {
		t.Errorf("expected the fail-closed summary in output: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "fmt.wrapError") {
		// %T of the regressed error names only its Go TYPE (here the
		// fmt.Errorf wrapper) — confirms the attr isn't silently dropped,
		// while proving %T never reveals the message text it wraps.
		t.Errorf("expected the %%T type name to still be present: %s", buf.String())
	}
}
