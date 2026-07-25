package miner

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/runtimeconfig"
)

// TestMinerFormattingNeverLeaksDashboardPassword locks that the Miner's
// unexported dashboard field (held for the fallback web build) never reveals the
// Basic Auth password when the Miner struct is formatted or logged. The password
// is a runtimeconfig.Secret whose value lives in a closure reflection cannot
// read, so even %+v / %#v over the whole Miner is safe.
func TestMinerFormattingNeverLeaksDashboardPassword(t *testing.T) {
	const sentinel = "miner-secret-sentinel-DO-NOT-PRINT"
	m := &Miner{}
	m.SetDashboardConfig(runtimeconfig.Dashboard{
		Username: "admin",
		Password: runtimeconfig.NewSecret(sentinel),
	})

	// Format the Miner through its pointer (the struct holds locks/atomics, so it
	// must not be copied); fmt reflects it in place, descending into the
	// unexported dashboard field.
	for _, out := range []string{
		fmt.Sprintf("%v", m),
		fmt.Sprintf("%+v", m),
		fmt.Sprintf("%#v", m),
	} {
		if strings.Contains(out, sentinel) {
			t.Fatalf("formatting the Miner leaked the dashboard password")
		}
	}

	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("miner", "m", m)
	if strings.Contains(buf.String(), sentinel) {
		t.Fatalf("logging the Miner leaked the dashboard password: %s", buf.String())
	}

	if got := m.dashboard.Password.Reveal(); got != sentinel {
		t.Errorf("Reveal() must still return the raw password for the fallback web build, got %q", got)
	}
}
