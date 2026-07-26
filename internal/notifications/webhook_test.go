package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestWebhookSendSuccessPreservesRequestSemantics is the positive control: the
// fix must not change the wire contract. The full sentinel URL (method, path,
// query, and JSON body) must still be delivered exactly as configured.
func TestWebhookSendSuccessPreservesRequestSemantics(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	var gotBody webhookPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := &WebhookProvider{url: srv.URL + "/" + sentinelWebhookPath + "?q=" + sentinelWebhookQuery}
	if !p.IsConfigured() {
		t.Fatal("expected provider to report configured")
	}

	msg := Message{Type: NotificationTypeOnline, Title: "t", Body: "b"}
	if err := p.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send() error = %v, want nil", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/"+sentinelWebhookPath {
		t.Errorf("path = %q, want %q", gotPath, "/"+sentinelWebhookPath)
	}
	if gotQuery != "q="+sentinelWebhookQuery {
		t.Errorf("query = %q, want %q", gotQuery, "q="+sentinelWebhookQuery)
	}
	if gotBody.Title != "t" || gotBody.Message != "b" || gotBody.Type != NotificationTypeOnline {
		t.Errorf("decoded body = %v, want title/message/type intact", gotBody)
	}
}

// TestWebhookSendNotConfiguredReturnsStaticError covers the IsConfigured
// guard (unreachable through Manager, which filters on IsConfigured before
// wiring a provider in): a fixed, static error with nothing to redact.
func TestWebhookSendNotConfiguredReturnsStaticError(t *testing.T) {
	p := &WebhookProvider{}
	err := p.Send(context.Background(), Message{Title: "t", Body: "b"})
	if err == nil {
		t.Fatal("expected an error for an unconfigured provider")
	}
	if err.Error() != "webhook not configured" {
		t.Errorf("Error() = %q, want %q", err.Error(), "webhook not configured")
	}
}

// TestWebhookSendTransportFailureNeverContainsURL is M6's core scenario: the
// configured URL IS the secret (here carrying userinfo, host, path, query,
// and fragment sentinels simultaneously), and a transport failure must not
// let any component escape via any rendering of the returned error.
func TestWebhookSendTransportFailureNeverContainsURL(t *testing.T) {
	withFakeHTTPClient(t, &fakeErrTransport{err: errors.New("connection refused")})
	buf := captureLogs(t)

	p := &WebhookProvider{url: sentinelWebhookURL}
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
	if se.Provider() != "webhook" {
		t.Errorf("Provider() = %q, want %q", se.Provider(), "webhook")
	}

	var ue *url.Error
	if errors.As(err, &ue) {
		t.Fatal("errors.As(err, new(*url.Error)) unexpectedly succeeded")
	}

	assertNoSentinel(t, err.Error(), allSentinels...)
	assertNoSentinel(t, fmt.Sprintf(verbPlusV, err), allSentinels...)
	assertNoSentinel(t, fmt.Sprintf(verbHashV, err), allSentinels...)

	slog.Error("simulated log site", "error", err)
	assertNoSentinel(t, buf.String(), allSentinels...)
}

// TestWebhookSendNestedWrappedTransportErrorNeverContainsURL checks a
// double-wrapped inner error (simulating a transport that itself wraps a
// *url.Error, e.g. a proxy dial failure) is still fully discarded.
func TestWebhookSendNestedWrappedTransportErrorNeverContainsURL(t *testing.T) {
	inner := &url.Error{Op: "dial", URL: sentinelWebhookURL, Err: errors.New("proxyconnect: " + sentinelMain)}
	wrapped := fmt.Errorf("dial tcp: %w", inner)
	withFakeHTTPClient(t, &fakeErrTransport{err: wrapped})

	p := &WebhookProvider{url: sentinelWebhookURL}
	err := p.Send(context.Background(), Message{Title: "t", Body: "b"})

	var se *SendError
	if !errors.As(err, &se) {
		t.Fatalf("expected a *SendError, got %T", err)
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		t.Fatal("errors.As(err, new(*url.Error)) unexpectedly succeeded")
	}
	assertNoSentinel(t, err.Error(), allSentinels...)
}

// TestWebhookSendContextCancellationNoSentinel mirrors the Gotify case for
// the webhook provider.
func TestWebhookSendContextCancellationNoSentinel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	withFakeHTTPClient(t, &fakeErrTransport{err: context.Canceled})

	p := &WebhookProvider{url: sentinelWebhookURL}
	err := p.Send(ctx, Message{Title: "t", Body: "b"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected errors.Is(err, context.Canceled), got %v", err)
	}
	assertNoSentinel(t, err.Error(), allSentinels...)
}

// TestWebhookSendTimeoutNoSentinel mirrors the Gotify deadline case.
func TestWebhookSendTimeoutNoSentinel(t *testing.T) {
	withFakeHTTPClient(t, &fakeErrTransport{err: context.DeadlineExceeded})

	p := &WebhookProvider{url: sentinelWebhookURL}
	err := p.Send(context.Background(), Message{Title: "t", Body: "b"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected errors.Is(err, context.DeadlineExceeded), got %v", err)
	}
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Error("expected net.Error.Timeout() == true")
	}
	assertNoSentinel(t, err.Error(), allSentinels...)
}

// TestWebhookSendRequestConstructionFailureNoSentinel covers a
// syntactically-invalid configured WEBHOOK_URL: http.NewRequestWithContext
// fails before any network I/O, and StageRequest's fixed message still must
// not echo the raw configured value.
func TestWebhookSendRequestConstructionFailureNoSentinel(t *testing.T) {
	p := &WebhookProvider{url: "http://abc " + sentinelMain}
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
	assertNoSentinel(t, err.Error(), sentinelMain)
}

// TestWebhookSendNonSuccessStatusBodyReflectsFullURLNoSentinel is the
// response-body-reflection scenario: a hostile/misconfigured webhook target
// answers 500 with a body echoing the request's own full URL (userinfo,
// path, query — everything) plus an unrelated response-body sentinel. None
// of that may survive into the returned error; only the status code does.
func TestWebhookSendNonSuccessStatusBodyReflectsFullURLNoSentinel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprintf(w, "error handling %s (%s)", r.URL.String(), sentinelResponseBody)
	}))
	defer srv.Close()

	// The httptest server URL doesn't carry userinfo/fragment (Go's HTTP
	// client/server can't round-trip those over the wire the same way), but
	// the path/query sentinels still exercise the reflection path end to end.
	p := &WebhookProvider{url: srv.URL + "/" + sentinelWebhookPath + "?q=" + sentinelWebhookQuery}
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
	if se.StatusCode() != http.StatusInternalServerError {
		t.Errorf("StatusCode() = %d, want %d", se.StatusCode(), http.StatusInternalServerError)
	}
	assertNoSentinel(t, err.Error(), sentinelWebhookPath, sentinelWebhookQuery, sentinelResponseBody)
}
