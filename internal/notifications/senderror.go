// Wrapping discipline (SECURITY, M5/M6 corrective pass): no error derived from
// *http.Request, *http.Response, url.Parse, or httpClient.Do is ever wrapped
// with %w — or retained in any other form — anywhere in this package. Those
// are Go stdlib *url.Error values that embed the full request URL, including
// any query string, userinfo, path, and fragment, verbatim in their Error()
// text (net/url's own doing, not this codebase's). Once such an error is
// captured by %w it stays retrievable via errors.As by any caller, however
// deep the wrapping chain gets, and its Error() string leaks through every
// %v/plus-v/slog rendering. The only function permitted to look at a
// transport/request-construction error is classifyTransport, and it discards
// the error immediately after reading its TYPE — never its string form. See
// newTransportError and newRequestError below.
package notifications

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
)

// SendStage identifies where in a MessageProvider.Send pipeline a delivery
// failed. It is a closed, non-secret enumeration — never derived from
// request/response data.
type SendStage string

const (
	// StageRequest is building the *http.Request (URL parse, body encode).
	StageRequest SendStage = "request"
	// StageTransport is the httpClient.Do round trip (DNS, connect, TLS,
	// timeout, cancellation).
	StageTransport SendStage = "transport"
	// StageResponse is a non-2xx HTTP response from the remote endpoint.
	StageResponse SendStage = "response"
)

// NetClass is a coarse, URL-free classification of a transport failure. Every
// value is derived only from an error's TYPE or a well-known sentinel — never
// from a string/stringer field of the underlying error (see
// classifyTransport, which is the only place this type is ever assigned).
type NetClass string

const (
	// ClassNone applies outside StageTransport (there is nothing to classify).
	ClassNone NetClass = ""
	// ClassDNS is a *net.DNSError (name resolution failure).
	ClassDNS NetClass = "dns"
	// ClassConnect is a *net.OpError whose Op is "dial" (connection refused,
	// no route, etc).
	ClassConnect NetClass = "connect"
	// ClassTLS is a certificate, record, or alert-level TLS failure.
	ClassTLS NetClass = "tls"
	// ClassTimeout is a client timeout or a caller-supplied deadline.
	ClassTimeout NetClass = "timeout"
	// ClassCanceled is a caller-cancelled context.
	ClassCanceled NetClass = "canceled"
	// ClassOther is any transport failure that doesn't match a more specific
	// class above.
	ClassOther NetClass = "other"
)

// SendError is the only error type a MessageProvider's Send may return for a
// request-construction, transport, or non-2xx failure.
//
// SECURITY INVARIANT: no field of this struct ever holds a URL, host, token,
// query, path, address, or response body — not even in an unexported field,
// because fmt's hash-v verb prints unexported fields via reflection, bypassing
// any String/Error/GoString method the type defines (fmt.printValue falls
// back to raw reflection whenever the value is not otherwise interfaceable,
// e.g. when nested inside another unexported field). The originating error is
// CLASSIFIED AND DISCARDED at construction time by the constructors below; it
// is never retained in any form, so there is nothing for the hash-v verb, a
// struct dump, or an errors.As probe to find. Every field here is a plain
// value type by design — never add a field whose type could ever hold an
// error, a net.Addr, a url.URL, or a string copied from one of those.
type SendError struct {
	provider string
	op       string
	stage    SendStage
	class    NetClass
	status   int
	timeout  bool
	canceled bool
	deadline bool
}

// newRequestError reports a failure to build the outgoing *http.Request (e.g.
// url.Parse rejecting a malformed configured URL). The parse error is
// deliberately not even accepted as a parameter: it would only be discarded,
// so there is no way to forget to discard it.
func newRequestError(provider, op string) *SendError {
	return &SendError{provider: provider, op: op, stage: StageRequest}
}

// newTransportError reports an httpClient.Do failure. err is classified by
// classifyTransport and then dropped on the floor — it never reaches a field
// of the returned *SendError.
func newTransportError(provider, op string, err error) *SendError {
	class, timeout, canceled, deadline := classifyTransport(err)
	return &SendError{
		provider: provider,
		op:       op,
		stage:    StageTransport,
		class:    class,
		timeout:  timeout,
		canceled: canceled,
		deadline: deadline,
	}
}

// newResponseError reports a non-2xx HTTP response. status is the only datum
// retained; the response body — attacker/remote-controlled — must never
// reach this constructor (callers drain it to io.Discard instead).
func newResponseError(provider, op string, status int) *SendError {
	return &SendError{provider: provider, op: op, stage: StageResponse, status: status}
}

// classifyTransport derives a coarse failure class from err's TYPE and from
// sentinel identity only. It MUST NOT read err.Error(), *net.DNSError.Name, or
// *net.OpError.Addr — those echo the remote host or address, which for a
// webhook provider IS the capability (T16/T17 in the threat model). It must
// also never read any other string/stringer field of a wrapped network error.
// Adding such a read reintroduces the exact leak this type exists to close;
// this constraint is enforced by a dedicated test (a DNS error whose Name
// embeds a sentinel host).
func classifyTransport(err error) (class NetClass, timeout, canceled, deadline bool) {
	switch {
	case errors.Is(err, context.Canceled):
		return ClassCanceled, false, true, false
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return ClassTimeout, true, false, true
	}

	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return ClassTimeout, true, false, false
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return ClassDNS, false, false, false
	}

	var certErr *tls.CertificateVerificationError
	var recErr tls.RecordHeaderError
	var alertErr tls.AlertError
	if errors.As(err, &certErr) || errors.As(err, &recErr) || errors.As(err, &alertErr) {
		return ClassTLS, false, false, false
	}

	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" { // Op is one of Go's own fixed literals, never user data
		return ClassConnect, false, false, false
	}

	return ClassOther, false, false, false
}

// Error renders a fixed, secret-free grammar. The provider/op/class/status
// values are all closed enumerations or integers — none of them can carry a
// URL, host, or token.
func (e *SendError) Error() string {
	switch e.stage {
	case StageResponse:
		return fmt.Sprintf("%s %s failed: endpoint returned HTTP %d", e.provider, e.op, e.status)
	case StageRequest:
		return fmt.Sprintf("%s %s failed: request could not be built (check the provider URL configuration)", e.provider, e.op)
	default:
		return fmt.Sprintf("%s %s failed: %s error", e.provider, e.op, e.class)
	}
}

// Unwrap deliberately exposes ONLY the two context sentinels, both of which
// are stdlib errors.errorString values with no URL or host in them. This lets
// errors.Is(err, context.Canceled) / errors.Is(err, context.DeadlineExceeded)
// keep working across the boundary while guaranteeing
// errors.As(err, new(*url.Error)) can never succeed: the original error was
// never retained, so there is nothing further down the chain to find.
func (e *SendError) Unwrap() error {
	switch {
	case e.canceled:
		return context.Canceled
	case e.deadline:
		return context.DeadlineExceeded
	}
	return nil
}

// Timeout reports whether the failure was a timeout, satisfying net.Error.
func (e *SendError) Timeout() bool { return e.timeout }

// Temporary reports whether the failure is plausibly transient, satisfying
// net.Error. This preserves the same errors.As(err, &netErr) && netErr.Timeout()
// pattern already used elsewhere in the codebase (internal/twitch/client.go)
// without ever chaining a real net.Error into this type.
func (e *SendError) Temporary() bool {
	return e.timeout || e.class == ClassConnect || e.class == ClassDNS
}

// Provider returns the provider name ("gotify", "webhook", ...).
func (e *SendError) Provider() string { return e.provider }

// Op returns the operation that failed ("send").
func (e *SendError) Op() string { return e.op }

// Stage returns the pipeline stage the failure occurred at.
func (e *SendError) Stage() SendStage { return e.stage }

// Class returns the transport failure classification (ClassNone outside
// StageTransport).
func (e *SendError) Class() NetClass { return e.class }

// StatusCode returns the HTTP status code (0 unless Stage() == StageResponse).
func (e *SendError) StatusCode() int { return e.status }

// String satisfies fmt.Stringer with the same safe text as Error.
func (e *SendError) String() string { return e.Error() }

// GoString satisfies fmt.GoStringer so that even the hash-v verb renders the
// safe error text instead of falling through to a raw field dump.
func (e *SendError) GoString() string {
	return fmt.Sprintf("notifications.SendError(%q)", e.Error())
}

// Format is the highest-precedence fmt hook: it intercepts every verb before
// fmt's default struct-reflection path can run, so no future field addition
// can silently become printable under the hash-v verb. This is defence in
// depth — the fields are already safe today — for exactly the reason
// described in the SendError doc comment.
func (e *SendError) Format(f fmt.State, verb rune) {
	switch {
	case verb == 'v' && f.Flag('#'):
		_, _ = io.WriteString(f, e.GoString())
	case verb == 'q':
		_, _ = io.WriteString(f, strconv.Quote(e.Error()))
	default:
		_, _ = io.WriteString(f, e.Error())
	}
}

// LogValue renders the error as a structured, safe attribute group so slog
// (text or JSON handler) never falls back to a raw Error()/plain-v/hash-v
// rendering path for this type. Deliberately NOT a json.Marshaler: slog's
// JSON handler would prefer a hand-rolled MarshalJSON over Error(), which is
// one more independently-maintained rendering path to get wrong for no
// benefit — API serialization is the DTO's job (see ProviderTestResult /
// newProviderTestFailure in manager.go).
func (e *SendError) LogValue() slog.Value {
	attrs := []slog.Attr{
		slog.String("provider", e.provider),
		slog.String("op", e.op),
		slog.String("stage", string(e.stage)),
	}
	if e.class != ClassNone {
		attrs = append(attrs, slog.String("class", string(e.class)))
	}
	if e.status != 0 {
		attrs = append(attrs, slog.Int("status", e.status))
	}
	return slog.GroupValue(attrs...)
}

// safeSendErrorAttr renders a provider send error for logging WITHOUT
// trusting the provider to have returned a *SendError. This is a second,
// independent barrier at the logging boundary (batch.go Flush, manager.go
// dispatchPush): if a provider ever regresses and returns a raw *url.Error or
// any other error, nothing here prints it. A non-SendError is reduced to its
// Go type name via %T, which names only the error's TYPE — never its message,
// so it cannot be config- or attacker-derived. This is the ONLY way those two
// call sites may log a provider send error; do not reintroduce a plain
// "error", err attribute at either site.
func safeSendErrorAttr(err error) slog.Attr {
	var se *SendError
	if errors.As(err, &se) {
		return slog.Attr{Key: "error", Value: se.LogValue()}
	}
	return slog.Attr{Key: "error", Value: slog.GroupValue(
		slog.String("summary", "unclassified provider send failure"),
		slog.String("type", fmt.Sprintf("%T", err)),
	)}
}
