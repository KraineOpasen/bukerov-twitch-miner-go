package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// MatrixProvider delivers notifications to a Matrix room via the client-server
// send API. It is configured through the MATRIX_HOMESERVER, MATRIX_ROOM_ID and
// MATRIX_ACCESS_TOKEN environment variables (each with the optional
// "_<USERNAME>" per-account override).
type MatrixProvider struct {
	homeserver  string
	roomID      string
	accessToken string

	txnCounter uint64
}

// NewMatrixProviderFromEnv constructs a MatrixProvider from the environment.
func NewMatrixProviderFromEnv(username string) *MatrixProvider {
	return &MatrixProvider{
		homeserver:  strings.TrimRight(envForAccount("MATRIX_HOMESERVER", username), "/"),
		roomID:      envForAccount("MATRIX_ROOM_ID", username),
		accessToken: envForAccount("MATRIX_ACCESS_TOKEN", username),
	}
}

// Name returns the provider's identifier.
func (p *MatrixProvider) Name() string { return "matrix" }

// IsConfigured reports whether the homeserver, room and token are all set.
func (p *MatrixProvider) IsConfigured() bool {
	return p.homeserver != "" && p.roomID != "" && p.accessToken != ""
}

// Send delivers a message to the configured Matrix room.
func (p *MatrixProvider) Send(ctx context.Context, msg Message) error {
	if !p.IsConfigured() {
		return fmt.Errorf("matrix not configured")
	}

	body := msg.Body
	if msg.Title != "" {
		body = msg.Title + "\n" + msg.Body
	}

	payload, err := json.Marshal(map[string]string{
		"msgtype": "m.text",
		"body":    body,
	})
	if err != nil {
		return fmt.Errorf("failed to encode matrix payload: %w", err)
	}

	// Transaction IDs must be unique per access token; combine a startup
	// timestamp with a monotonic counter so retries never collide.
	txn := fmt.Sprintf("%d-%d", time.Now().UnixNano(), atomic.AddUint64(&p.txnCounter, 1))
	url := fmt.Sprintf("%s/_matrix/client/v3/rooms/%s/send/m.room.message/%s",
		p.homeserver, p.roomID, txn)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("failed to build matrix request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.accessToken)

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("matrix request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("matrix returned %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	return nil
}
