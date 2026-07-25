package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/resources"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/runtimeconfig"
)

func populatedSnapshot() resources.Snapshot {
	return resources.Snapshot{
		Available: true,
		SampledAt: "2026-07-25T10:00:00Z",
		CPU:       resources.CPU{Available: true, Percent: 12.5, LimitCores: 20, History: []float64{0.1, 0.2}},
		Memory:    resources.Memory{Available: true, UsedBytes: 75812044, LimitBytes: 67310755840, Percent: 0.11, History: []float64{0.0011}},
		Network:   resources.Network{Available: true, RxBytesPerSec: 123456, TxBytesPerSec: 23456, History: []float64{0.5, 1}},
		Disk:      resources.Disk{Available: true, ReadBytesPerSec: 345678, WriteBytesPerSec: 45678, History: []float64{0.3}},
	}
}

// TestResourcesEndpointSchemaShape: the JSON has exactly the allowlisted
// top-level keys and each section's numeric fields.
func TestResourcesEndpointSchemaShape(t *testing.T) {
	s := newRenderServer(t)
	s.SetResourceSnapshotProvider(populatedSnapshot)
	h := s.handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, ResourcesPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", ResourcesPath, rec.Code)
	}

	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	wantTop := map[string]struct{}{"available": {}, "sampledAt": {}, "cpu": {}, "memory": {}, "network": {}, "disk": {}}
	for k := range doc {
		if _, ok := wantTop[k]; !ok {
			t.Errorf("unexpected top-level key %q", k)
		}
	}
	for k := range wantTop {
		if _, ok := doc[k]; !ok {
			t.Errorf("missing top-level key %q", k)
		}
	}
	cpu, _ := doc["cpu"].(map[string]any)
	if cpu["available"] != true || cpu["percent"].(float64) != 12.5 || cpu["limitCores"].(float64) != 20 {
		t.Errorf("cpu section wrong: %+v", cpu)
	}
}

// TestResourcesEndpointCacheAndSecurityHeaders: no-store, JSON, and the shared
// security header (proves it sits behind securityHeadersMiddleware).
func TestResourcesEndpointCacheAndSecurityHeaders(t *testing.T) {
	s := newRenderServer(t)
	s.SetResourceSnapshotProvider(populatedSnapshot)
	h := s.handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, ResourcesPath, nil))
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("security headers missing on resources route")
	}
}

// TestResourcesEndpointNoProvider: with no provider wired, 200 with a typed
// all-unavailable snapshot — NOT 404, NOT 500.
func TestResourcesEndpointNoProvider(t *testing.T) {
	s := newRenderServer(t)
	h := s.handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, ResourcesPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET without provider = %d, want 200 (all-unavailable)", rec.Code)
	}
	var doc struct {
		Available bool `json:"available"`
		CPU       struct {
			Available bool `json:"available"`
		} `json:"cpu"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if doc.Available || doc.CPU.Available {
		t.Error("no-provider snapshot must be all-unavailable")
	}
}

// TestResourcesEndpointGETOnly: non-GET methods are rejected 405.
func TestResourcesEndpointGETOnly(t *testing.T) {
	s := newRenderServer(t)
	s.SetResourceSnapshotProvider(populatedSnapshot)
	h := s.handler()

	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(m, ResourcesPath, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", m, ResourcesPath, rec.Code)
		}
	}
}

// TestResourcesEndpointAuth: inherits the dashboard's Basic Auth exactly like
// every other read endpoint — 401 without creds when configured, 200 with them.
func TestResourcesEndpointAuth(t *testing.T) {
	s := newRenderServer(t)
	s.SetDashboardConfig(runtimeconfig.Dashboard{Username: "admin", Password: runtimeconfig.NewSecret("hunter2")})
	s.SetResourceSnapshotProvider(populatedSnapshot)
	h := s.handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, ResourcesPath, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET without credentials = %d, want 401", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, ResourcesPath, nil)
	req.SetBasicAuth("admin", "hunter2")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET with credentials = %d, want 200", rec.Code)
	}
}

// TestResourcesEndpointOpenWhenAuthDisabled: no creds configured -> open, like
// every other read route.
func TestResourcesEndpointOpenWhenAuthDisabled(t *testing.T) {
	// No dashboard credentials configured -> auth disabled (zero Dashboard).
	s := newRenderServer(t)
	s.SetResourceSnapshotProvider(populatedSnapshot)
	h := s.handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, ResourcesPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET (auth disabled) = %d, want 200", rec.Code)
	}
}

// TestResourcesEndpointDoesNotSample: the handler only reads the provider; N
// requests call the provider N times and never trigger a separate sample.
func TestResourcesEndpointDoesNotSample(t *testing.T) {
	calls := 0
	s := newRenderServer(t)
	s.SetResourceSnapshotProvider(func() resources.Snapshot { calls++; return populatedSnapshot() })
	h := s.handler()

	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, ResourcesPath, nil))
	}
	if calls != 3 {
		t.Errorf("provider called %d times, want 3 (one pure read per request, no extra sampling)", calls)
	}
}

// TestResourcesEndpointProviderPanicDegrades: a panicking provider yields a clean
// 200 all-unavailable with no internals leaked, and the server keeps serving.
func TestResourcesEndpointProviderPanicDegrades(t *testing.T) {
	s := newRenderServer(t)
	s.SetResourceSnapshotProvider(func() resources.Snapshot { panic("secret internal state") })
	h := s.handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, ResourcesPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("panicking provider = %d, want 200 all-unavailable", rec.Code)
	}
	body := rec.Body.String()
	for _, leak := range []string{"goroutine", "panic", "secret", "runtime"} {
		if strings.Contains(body, leak) {
			t.Errorf("response leaked internals (%q): %s", leak, body)
		}
	}
	var doc struct {
		Available bool `json:"available"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &doc)
	if doc.Available {
		t.Error("panic degrade must be all-unavailable")
	}

	// Still serving afterwards.
	s.SetResourceSnapshotProvider(populatedSnapshot)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, ResourcesPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("request after panic = %d, want 200", rec.Code)
	}
}

// TestResourcesEndpointPrivacyScan: even when the provider is fed a snapshot,
// the wire form carries only numbers/booleans — no identity substrings.
func TestResourcesEndpointPrivacyScan(t *testing.T) {
	s := newRenderServer(t)
	s.SetResourceSnapshotProvider(populatedSnapshot)
	h := s.handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, ResourcesPath, nil))
	body := rec.Body.String()
	for _, banned := range []string{"eth", "lo", "/proc", "/sys", "cgroup", "hostname", "127.0.0.1", "pid"} {
		if strings.Contains(strings.ToLower(body), banned) {
			t.Errorf("resource JSON contains banned substring %q: %s", banned, body)
		}
	}
}
