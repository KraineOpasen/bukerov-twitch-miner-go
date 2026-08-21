package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
)

// TestPolicyPersistenceFailureMapsToInternalError pins the HTTP half of the
// policy persistence commit point, in the package that owns the status
// mapping (mirroring stale_generation_status_test.go's split of concerns:
// internal/web cannot import internal/miner, so the miner-side tests prove
// the provider fails closed, and this proves what the handler tells the
// operator when it does).
//
// A persistence failure on a LIVE authoritative generation is a server
// fault: the mutation was admitted, attempted, and rejected at the durable
// commit point. It must map to the generic 500 — NOT the fence's retryable
// 503 (nothing is draining; an immediate retry hits the same disk), NOT the
// lifecycle 409 (there is nothing for the operator to resume), and never a
// 200 re-render. The client body carries only the generic policy message;
// the wrapped internal error (which may name filesystem paths) stays in the
// server log.
func TestPolicyPersistenceFailureMapsToInternalError(t *testing.T) {
	// The sentinel carries a recognizable fake path so the body-sanitization
	// assertion below is meaningful.
	persistErr := errors.New("persist config: /private/data/config.json: no space left on device")

	cases := []struct {
		name string
		path string
		body string
	}{
		{name: "policy mode", path: "/api/policy/mode", body: "mode=SMART"},
		{name: "drop rule", path: "/api/policy/drop-rule", body: "rewardKey=some-key&skip=on"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := NewServer(config.AnalyticsSettings{Host: "127.0.0.1", Port: 0, Refresh: 5, DaysAgo: 30}, "tester", t.TempDir(), nil, nil)
			srv.SetPolicyProvider(&f3Policy{rules: map[string]config.DropRule{}, err: persistErr})

			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rec := httptest.NewRecorder()
			srv.handler().ServeHTTP(rec, req)

			if rec.Code == http.StatusOK {
				t.Errorf("POST %s with a failing-persist provider = 200 OK; a rejected mutation must never re-render as success", tc.path)
			}
			if rec.Code == http.StatusServiceUnavailable || rec.Code == http.StatusConflict {
				t.Errorf("POST %s = %d; a persistence failure on a live generation must not be misclassified as the fence's 503 or the lifecycle 409", tc.path, rec.Code)
			}
			if rec.Code != http.StatusInternalServerError {
				t.Errorf("POST %s = %d, want %d", tc.path, rec.Code, http.StatusInternalServerError)
			}
			if body := rec.Body.String(); strings.Contains(body, "/private/data") {
				t.Errorf("response body leaks the internal filesystem path: %q", body)
			}
		})
	}
}
