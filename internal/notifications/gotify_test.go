package notifications

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// fakeErrTransport implements http.RoundTripper and always fails, returning
// exactly the *url.Error the real net/http stack would produce for a
// transport-level failure — including the full outgoing request URL — so
// tests can assert what a Send failure exposes without any real network I/O.
type fakeErrTransport struct {
	err error
	// recorded, if the test reads it after RoundTrip runs, is the request
	// gotify/webhook actually issued (used to assert the token/URL contract
	// is unchanged even though the resulting error must never carry it).
	recorded *http.Request
}

func (f *fakeErrTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.recorded = req
	return nil, &url.Error{Op: "Post", URL: req.URL.String(), Err: f.err}
}

// withFakeHTTPClient swaps the package-level httpClient for the duration of
// the test and restores it on cleanup — mirrors captureLogs'
// (discord_config_test.go) slog.SetDefault save/restore idiom, applied to the
// package's other swappable global.
func withFakeHTTPClient(t *testing.T, rt http.RoundTripper) {
	t.Helper()
	prev := httpClient
	httpClient = &http.Client{Transport: rt, Timeout: prev.Timeout}
	t.Cleanup(func() { httpClient = prev })
}

// TestGotifySendSuccessStillUsesRealTokenAndURL is the positive control: the
// fix must not change the wire contract, only how failures are reported. The
// token must still travel in the query string exactly as before.
func TestGotifySendSuccessStillUsesRealTokenAndURL(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := &GotifyProvider{serverURL: srv.URL, token: sentinelGotifyToken}
	if !p.IsConfigured() {
		t.Fatal("expected provider to report configured")
	}

	if err := p.Send(context.Background(), Message{Title: "t", Body: "b"}); err != nil {
		t.Fatalf("Send() error = %v, want nil", err)
	}

	if !strings.Contains(gotQuery, "token="+sentinelGotifyToken) {
		t.Errorf("outgoing request query = %q, want it to carry token=%s", gotQuery, sentinelGotifyToken)
	}
}

// TestGotifySendUsesDefaultTitleWhenEmpty covers the fallback title branch:
// an empty Message.Title still produces a real, non-empty title in the
// delivered payload.
func TestGotifySendUsesDefaultTitleWhenEmpty(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := &GotifyProvider{serverURL: srv.URL, token: sentinelGotifyToken}
	if err := p.Send(context.Background(), Message{Title: "", Body: "b"}); err != nil {
		t.Fatalf("Send() error = %v, want nil", err)
	}
	if !strings.Contains(gotBody, "Twitch Points Miner") {
		t.Errorf("expected the default title in the payload, got %q", gotBody)
	}
}

// TestGotifySendNotConfiguredReturnsStaticError covers the IsConfigured guard:
// Send on an unconfigured provider (unreachable through Manager, which
// filters on IsConfigured before wiring a provider in) returns a fixed,
// static error with no formatting to worry about.
func TestGotifySendNotConfiguredReturnsStaticError(t *testing.T) {
	p := &GotifyProvider{}
	err := p.Send(context.Background(), Message{Title: "t", Body: "b"})
	if err == nil {
		t.Fatal("expected an error for an unconfigured provider")
	}
	if err.Error() != "gotify not configured" {
		t.Errorf("Error() = %q, want %q", err.Error(), "gotify not configured")
	}
}

// TestGotifySendTransportFailureNeverContainsToken is M5's core scenario: a
// transport-level failure (here a generic connection error, wrapped by the
// real http.Client machinery in a *url.Error carrying the full request URL
// with token=...) must produce a SendError whose every rendering is clean.
func TestGotifySendTransportFailureNeverContainsToken(t *testing.T) {
	withFakeHTTPClient(t, &fakeErrTransport{err: errors.New("connection refused")})
	buf := captureLogs(t)

	p := &GotifyProvider{serverURL: "http://" + sentinelMain + ".invalid", token: sentinelGotifyToken}
	err := p.Send(context.Background(), Message{Title: "t", Body: "b"})
	if err == nil {
		t.Fatal("expected an error")
	}

	var se *SendError
	if !errors.As(err, &se) {
		t.Fatalf("expected a *SendError, got %T", err)
	}
	if se.Stage() != StageTransport {
		t.Errorf("Stage() = %v, want StageTransport", se.Stage())
	}

	var ue *url.Error
	if errors.As(err, &ue) {
		t.Fatal("errors.As(err, new(*url.Error)) unexpectedly succeeded")
	}

	assertNoSentinel(t, err.Error(), sentinelGotifyToken, sentinelMain)
	assertNoSentinel(t, fmt.Sprintf(verbPlusV, err), sentinelGotifyToken, sentinelMain)
	assertNoSentinel(t, fmt.Sprintf(verbHashV, err), sentinelGotifyToken, sentinelMain)

	slog.Error("simulated log site", "error", err)
	assertNoSentinel(t, buf.String(), sentinelGotifyToken, sentinelMain)
}

// TestGotifySendConnectFailureClassPreservedNoSentinel checks that a
// dial-level failure keeps its diagnostic value (ClassConnect) while still
// discarding the URL/token entirely.
func TestGotifySendConnectFailureClassPreservedNoSentinel(t *testing.T) {
	dialErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	withFakeHTTPClient(t, &fakeErrTransport{err: dialErr})

	p := &GotifyProvider{serverURL: "http://" + sentinelMain + ".invalid", token: sentinelGotifyToken}
	err := p.Send(context.Background(), Message{Title: "t", Body: "b"})

	var se *SendError
	if !errors.As(err, &se) {
		t.Fatalf("expected *SendError, got %T", err)
	}
	if se.Class() != ClassConnect {
		t.Errorf("Class() = %q, want %q", se.Class(), ClassConnect)
	}
	assertNoSentinel(t, err.Error(), sentinelGotifyToken, sentinelMain)
}

// TestGotifySendDNSFailureClassPreservedNoSentinel checks the DNS branch
// specifically: *net.DNSError.Name echoes the looked-up host independent of
// the URL string, so this is the regression this package must never
// reintroduce (see classifyTransport's doc comment).
func TestGotifySendDNSFailureClassPreservedNoSentinel(t *testing.T) {
	dnsErr := &net.DNSError{Err: "no such host", Name: sentinelMain + ".invalid", IsNotFound: true}
	withFakeHTTPClient(t, &fakeErrTransport{err: dnsErr})

	p := &GotifyProvider{serverURL: "http://" + sentinelMain + ".invalid", token: sentinelGotifyToken}
	err := p.Send(context.Background(), Message{Title: "t", Body: "b"})

	var se *SendError
	if !errors.As(err, &se) {
		t.Fatalf("expected *SendError, got %T", err)
	}
	if se.Class() != ClassDNS {
		t.Errorf("Class() = %q, want %q", se.Class(), ClassDNS)
	}
	assertNoSentinel(t, err.Error(), sentinelGotifyToken, sentinelMain)
}

// TestGotifySendContextCancellationNoSentinel checks a cancelled ctx still
// surfaces as errors.Is(err, context.Canceled) with no sentinel anywhere.
func TestGotifySendContextCancellationNoSentinel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	withFakeHTTPClient(t, &fakeErrTransport{err: context.Canceled})

	p := &GotifyProvider{serverURL: "http://" + sentinelMain + ".invalid", token: sentinelGotifyToken}
	err := p.Send(ctx, Message{Title: "t", Body: "b"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected errors.Is(err, context.Canceled), got %v", err)
	}
	assertNoSentinel(t, err.Error(), sentinelGotifyToken, sentinelMain)
}

// TestGotifySendDeadlineNoSentinel checks a deadline failure preserves both
// errors.Is(context.DeadlineExceeded) and net.Error.Timeout(), with no
// sentinel anywhere.
func TestGotifySendDeadlineNoSentinel(t *testing.T) {
	withFakeHTTPClient(t, &fakeErrTransport{err: context.DeadlineExceeded})

	p := &GotifyProvider{serverURL: "http://" + sentinelMain + ".invalid", token: sentinelGotifyToken}
	err := p.Send(context.Background(), Message{Title: "t", Body: "b"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected errors.Is(err, context.DeadlineExceeded), got %v", err)
	}
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Error("expected net.Error.Timeout() == true")
	}
	assertNoSentinel(t, err.Error(), sentinelGotifyToken, sentinelMain)
}

// TestGotifySendInvalidConfiguredURLNoSentinel checks a syntactically invalid
// configured GOTIFY_URL (embedded space, rejected by url.Parse before any
// network I/O) surfaces as StageRequest with no sentinel.
func TestGotifySendInvalidConfiguredURLNoSentinel(t *testing.T) {
	p := &GotifyProvider{serverURL: "http://abc " + sentinelMain, token: sentinelGotifyToken}
	err := p.Send(context.Background(), Message{Title: "t", Body: "b"})
	if err == nil {
		t.Fatal("expected an error for a malformed configured URL")
	}
	var se *SendError
	if !errors.As(err, &se) {
		t.Fatalf("expected *SendError, got %T", err)
	}
	if se.Stage() != StageRequest {
		t.Errorf("Stage() = %v, want StageRequest", se.Stage())
	}
	assertNoSentinel(t, err.Error(), sentinelGotifyToken, sentinelMain)
}

// TestGotifySendNonSuccessStatusBodyNeverAmplifiesLeak covers the
// response-body-reflection scenario: a remote (or hostile) Gotify server
// returns 401 with a body echoing the request's own URL (which carries the
// token) plus an unrelated response-body sentinel. Both must be absent from
// the resulting error; only the status code survives.
func TestGotifySendNonSuccessStatusBodyNeverAmplifiesLeak(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprintf(w, "unauthorized: %s (%s)", r.URL.String(), sentinelResponseBody)
	}))
	defer srv.Close()

	p := &GotifyProvider{serverURL: srv.URL, token: sentinelGotifyToken}
	err := p.Send(context.Background(), Message{Title: "t", Body: "b"})
	if err == nil {
		t.Fatal("expected a non-2xx error")
	}

	var se *SendError
	if !errors.As(err, &se) {
		t.Fatalf("expected *SendError, got %T", err)
	}
	if se.Stage() != StageResponse {
		t.Errorf("Stage() = %v, want StageResponse", se.Stage())
	}
	if se.StatusCode() != http.StatusUnauthorized {
		t.Errorf("StatusCode() = %d, want %d", se.StatusCode(), http.StatusUnauthorized)
	}
	assertNoSentinel(t, err.Error(), sentinelGotifyToken, sentinelResponseBody)
}
