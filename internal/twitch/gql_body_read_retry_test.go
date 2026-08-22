package twitch

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/auth"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
)

var errScriptedBodyRead = errors.New("scripted response body read failure")

type failingReadCloser struct {
	closed *int
}

func (*failingReadCloser) Read([]byte) (int, error) { return 0, errScriptedBodyRead }
func (r *failingReadCloser) Close() error {
	(*r.closed)++
	return nil
}

type gqlScriptedResponse struct {
	status   int
	body     string
	readFail bool
}

type gqlRetryRecorder struct {
	mu        sync.Mutex
	responses []gqlScriptedResponse
	requests  [][]byte
	headers   []http.Header
	closed    int
}

func (r *gqlRetryRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, append([]byte(nil), body...))
	r.headers = append(r.headers, req.Header.Clone())
	i := len(r.requests) - 1
	response := r.responses[i]

	var responseBody io.ReadCloser
	if response.readFail {
		responseBody = &failingReadCloser{closed: &r.closed}
	} else {
		responseBody = io.NopCloser(bytes.NewBufferString(response.body))
	}
	return &http.Response{StatusCode: response.status, Header: make(http.Header), Body: responseBody}, nil
}

func newGQLRetryTestClient(recorder *gqlRetryRecorder) *TwitchClient {
	c := NewTwitchClient(auth.NewTwitchAuth("tester", "device"), "device")
	c.client = &http.Client{Transport: recorder}
	c.gqlRetrySleep = func(time.Duration) {}
	return c
}

func TestGQLRetriesSuccessfulStatusBodyReadFailure(t *testing.T) {
	recorder := &gqlRetryRecorder{responses: []gqlScriptedResponse{
		{status: http.StatusOK, readFail: true},
		{status: http.StatusOK, body: `{"data":{"ok":true}}`},
	}}
	c := newGQLRetryTestClient(recorder)
	body := []byte(`{"operationName":"Identity"}`)

	got, status, err := c.doGQLRequestWithRetry(body, "Identity", "test-client", "test-token")
	if err != nil {
		t.Fatalf("retry after body read failure: %v", err)
	}
	if status != http.StatusOK || string(got) != `{"data":{"ok":true}}` {
		t.Fatalf("response = (%d, %s), want successful second response", status, got)
	}
	if len(recorder.requests) != 2 {
		t.Fatalf("attempts = %d, want 2", len(recorder.requests))
	}
	if recorder.closed != 1 {
		t.Fatalf("failed response bodies closed = %d, want 1", recorder.closed)
	}
	for i, requestBody := range recorder.requests {
		if !bytes.Equal(requestBody, body) {
			t.Errorf("attempt %d body changed: got %q, want %q", i+1, requestBody, body)
		}
		if got := recorder.headers[i].Get("Client-Id"); got != "test-client" {
			t.Errorf("attempt %d Client-Id = %q, want test-client", i+1, got)
		}
		if got := recorder.headers[i].Get("Authorization"); got != "OAuth test-token" {
			t.Errorf("attempt %d Authorization = %q, want fixture value", i+1, got)
		}
	}
}

func TestGQLExhaustsSuccessfulStatusBodyReadFailures(t *testing.T) {
	responses := make([]gqlScriptedResponse, gqlMaxRetries+1)
	for i := range responses {
		responses[i] = gqlScriptedResponse{status: http.StatusOK, readFail: true}
	}
	recorder := &gqlRetryRecorder{responses: responses}
	c := newGQLRetryTestClient(recorder)
	sentinel := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	c.lastSuccess = sentinel

	_, _, err := c.doGQLRequestWithRetry([]byte(`{}`), "Exhaust", "test-client", "test-token")
	if err == nil {
		t.Fatal("expected exhausted body read failures to return an error")
	}
	if len(recorder.requests) != gqlMaxRetries+1 {
		t.Fatalf("attempts = %d, want %d", len(recorder.requests), gqlMaxRetries+1)
	}
	if got := c.RecentGQLFailures(time.Hour); got != 1 {
		t.Fatalf("request-cycle failures = %d, want 1", got)
	}
	if got := c.LastSuccessAt(); !got.Equal(sentinel) {
		t.Fatalf("failed reads marked success: got %v, want %v", got, sentinel)
	}
}

func TestGQLBodyReadRetryBoundaries(t *testing.T) {
	tests := []struct {
		name      string
		responses []gqlScriptedResponse
		attempts  int
		wantErr   bool
	}{
		{name: "ordinary 200", responses: []gqlScriptedResponse{{status: http.StatusOK, body: `{"data":{}}`}, {status: http.StatusOK, body: `{"data":{}}`}}, attempts: 1},
		{name: "429 remains transient", responses: []gqlScriptedResponse{{status: http.StatusTooManyRequests}, {status: http.StatusOK, body: `{"data":{}}`}}, attempts: 2},
		{name: "503 remains transient", responses: []gqlScriptedResponse{{status: http.StatusServiceUnavailable}, {status: http.StatusOK, body: `{"data":{}}`}}, attempts: 2},
		{name: "302 read failure remains terminal", responses: []gqlScriptedResponse{{status: http.StatusFound, readFail: true}, {status: http.StatusOK, body: `{"data":{}}`}}, attempts: 1, wantErr: true},
		{name: "400 read failure remains terminal", responses: []gqlScriptedResponse{{status: http.StatusBadRequest, readFail: true}, {status: http.StatusOK, body: `{"data":{}}`}}, attempts: 1, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := &gqlRetryRecorder{responses: tt.responses}
			c := newGQLRetryTestClient(recorder)
			_, _, err := c.doGQLRequestWithRetry([]byte(`{}`), "Boundary", "test-client", "test-token")
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if len(recorder.requests) != tt.attempts {
				t.Fatalf("attempts = %d, want %d", len(recorder.requests), tt.attempts)
			}
		})
	}
}

func TestGQLBatchRetriesSuccessfulStatusBodyReadFailure(t *testing.T) {
	recorder := &gqlRetryRecorder{responses: []gqlScriptedResponse{
		{status: http.StatusOK, readFail: true},
		{status: http.StatusOK, body: `[{"data":{}}]`},
	}}
	c := newGQLRetryTestClient(recorder)

	result, err := c.postGQLBatchRequest([]constants.GQLOperation{constants.Inventory})
	if err != nil {
		t.Fatalf("batch retry: %v", err)
	}
	if len(result) != 1 || len(recorder.requests) != 2 {
		t.Fatalf("batch result entries = %d, attempts = %d; want 1, 2", len(result), len(recorder.requests))
	}
}
