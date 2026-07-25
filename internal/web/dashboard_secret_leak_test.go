package web

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/runtimeconfig"
)

// TestServerFormattingNeverLeaksDashboardPassword locks the real consumer
// layout: the Server holds the dashboard snapshot — including the Basic Auth
// password — in an UNEXPORTED field (s.dashboard). Formatting the whole Server
// with %v/%+v/%#v, or logging it, must never reveal the password. It cannot,
// because the password is a runtimeconfig.Secret whose value lives in a closure
// that reflection cannot read; and Reveal() still returns it for the auth path.
func TestServerFormattingNeverLeaksDashboardPassword(t *testing.T) {
	const sentinel = "web-server-secret-sentinel-DO-NOT-PRINT"
	s := newRenderServer(t)
	s.SetDashboardConfig(runtimeconfig.Dashboard{
		Username: "admin",
		Password: runtimeconfig.NewSecret(sentinel),
	})

	// Format the Server through its pointer (the struct holds a sync.Mutex, so it
	// must not be copied); fmt reflects the struct in place, descending into the
	// unexported dashboard field — the path that used to leak.
	for _, out := range []string{
		fmt.Sprintf("%v", s),
		fmt.Sprintf("%+v", s),
		fmt.Sprintf("%#v", s),
	} {
		if strings.Contains(out, sentinel) {
			t.Fatalf("formatting the Server leaked the dashboard password")
		}
	}

	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("server", "srv", s)
	if strings.Contains(buf.String(), sentinel) {
		t.Fatalf("logging the Server leaked the dashboard password: %s", buf.String())
	}

	// Redaction did not break auth: the credential is still enabled and the raw
	// value is still reachable via Reveal() for the Basic Auth comparison.
	if !s.AuthConfigured() {
		t.Error("AuthConfigured should be true with credentials set")
	}
	if got := s.dashboardConfig().Password.Reveal(); got != sentinel {
		t.Errorf("Reveal() must still return the raw password for auth, got %q", got)
	}
}
