package runtimeconfig

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/updater"
)

// secretSentinel is a unique password value used across the formatting matrix:
// if it ever appears in ANY rendered form of a config type, that form leaks the
// secret. It is deliberately distinctive so a substring match is unambiguous.
const secretSentinel = "bkm021-secret-sentinel-never-print"

// snapshotWithSecret resolves a RuntimeConfig whose dashboard password is the
// sentinel (with a few non-secret fields populated so the redacted forms still
// carry visible structure).
func snapshotWithSecret() RuntimeConfig {
	return Resolve(Flags{ConfigPath: "config.json", Debug: true}, envMap(map[string]string{
		"DASHBOARD_HOST":            "0.0.0.0",
		"DASHBOARD_USERNAME":        "admin",
		"DASHBOARD_PASSWORD":        secretSentinel,
		"DASHBOARD_TRUSTED_ORIGINS": "proxy.lan:8443",
	}))
}

// TestSecretNeverLeaksInFormatting is the secret-formatting matrix: the
// sentinel password must not appear in ANY fmt verb, Stringer/GoStringer output,
// fmt.Sprint, a wrong verb, a pointer render, a nested-struct render, an error,
// or slog (text and JSON) output — for both RuntimeConfig and the nested
// Dashboard formatted directly.
func TestSecretNeverLeaksInFormatting(t *testing.T) {
	rc := snapshotWithSecret()
	d := rc.Dashboard

	// wrapExported embeds a Dashboard in an EXPORTED field: fmt reaches it
	// interfaceably and dispatches Dashboard's fmt hooks.
	type wrapExported struct{ Inner Dashboard }
	we := wrapExported{Inner: d}

	// wrapUnexported embeds a Dashboard / RuntimeConfig in an UNEXPORTED field —
	// the exact layout of Server.dashboard and Miner.dashboard. Here fmt CANNOT
	// invoke the fmt hooks (the value is not interfaceable) and falls back to
	// reflection; the password stays safe only because it is a Secret whose value
	// lives in a closure reflection cannot read. This is the regression the fix
	// closes and the previous matrix missed.
	type wrapUnexportedDash struct{ inner Dashboard }
	type wrapUnexportedRC struct{ inner RuntimeConfig }
	wud := wrapUnexportedDash{inner: d}
	wur := wrapUnexportedRC{inner: rc}

	renders := map[string]string{
		"rc %v":                      fmt.Sprintf("%v", rc),
		"rc %+v":                     fmt.Sprintf("%+v", rc),
		"rc %#v":                     fmt.Sprintf("%#v", rc),
		"rc %s":                      fmt.Sprintf("%s", rc),
		"rc %q":                      fmt.Sprintf("%q", rc),
		"rc ptr %+v":                 fmt.Sprintf("%+v", &rc),
		"rc ptr %#v":                 fmt.Sprintf("%#v", &rc),
		"rc Sprint":                  fmt.Sprint(rc),
		"rc String()":                rc.String(),
		"rc GoString()":              rc.GoString(),
		"dash %v":                    fmt.Sprintf("%v", d),
		"dash %+v":                   fmt.Sprintf("%+v", d),
		"dash %#v":                   fmt.Sprintf("%#v", d),
		"dash %s":                    fmt.Sprintf("%s", d),
		"dash %q":                    fmt.Sprintf("%q", d),
		"dash %d wrongverb":          fmt.Sprintf("%d", d),
		"dash ptr %#v":               fmt.Sprintf("%#v", &d),
		"dash Sprint":                fmt.Sprint(d),
		"dash String()":              d.String(),
		"dash GoString()":            d.GoString(),
		"exported-nested %v":         fmt.Sprintf("%v", we),
		"exported-nested %+v":        fmt.Sprintf("%+v", we),
		"exported-nested %#v":        fmt.Sprintf("%#v", we),
		"UNEXPORTED-nested dash %v":  fmt.Sprintf("%v", wud),
		"UNEXPORTED-nested dash %+v": fmt.Sprintf("%+v", wud),
		"UNEXPORTED-nested dash %#v": fmt.Sprintf("%#v", wud),
		"UNEXPORTED-nested rc %v":    fmt.Sprintf("%v", wur),
		"UNEXPORTED-nested rc %+v":   fmt.Sprintf("%+v", wur),
		"UNEXPORTED-nested rc %#v":   fmt.Sprintf("%#v", wur),
		"secret %v":                  fmt.Sprintf("%v", d.Password),
		"secret %+v":                 fmt.Sprintf("%+v", d.Password),
		"secret %#v":                 fmt.Sprintf("%#v", d.Password),
		"secret %s":                  fmt.Sprintf("%s", d.Password),
		"secret String()":            d.Password.String(),
		"error %v":                   fmt.Errorf("boom: %v", d).Error(),
		"error %+v rc":               fmt.Errorf("boom: %+v", rc).Error(),
		"logvalue rc":                rc.LogValue().String(),
		"logvalue dash":              d.LogValue().String(),
	}

	for name, out := range renders {
		if strings.Contains(out, secretSentinel) {
			t.Errorf("LEAK in %q:\n  %s", name, out)
		}
	}

	// slog text + JSON handlers must not leak either (both types implement
	// slog.LogValuer, and the nested dashboard is embedded as a redacted group).
	for _, h := range []struct {
		name string
		make func(*bytes.Buffer) slog.Handler
	}{
		{"text", func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) }},
		{"json", func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) }},
	} {
		var buf bytes.Buffer
		slog.New(h.make(&buf)).Info("cfg", "rc", rc, "dashboard", d)
		if strings.Contains(buf.String(), secretSentinel) {
			t.Errorf("LEAK in slog %s handler:\n  %s", h.name, buf.String())
		}
	}

	// The renders that carry password structure must show the redaction marker,
	// proving redaction actually ran (not that the field was merely omitted).
	for _, name := range []string{"dash %#v", "dash String()", "rc %#v"} {
		if !strings.Contains(renders[name], "***") {
			t.Errorf("%s should show the redaction marker ***: %s", name, renders[name])
		}
	}
}

// envMap builds a Lookup backed by a plain map, so Resolve is tested without
// touching the real process environment. A key absent from the map reports
// (", false), exactly like os.LookupEnv for an unset variable.
func envMap(m map[string]string) Lookup {
	return func(key string) (string, bool) {
		v, ok := m[key]
		return v, ok
	}
}

func TestResolveDefaults(t *testing.T) {
	rc := Resolve(Flags{ConfigPath: "config.json"}, envMap(nil))

	if rc.ConfigPath != "config.json" {
		t.Errorf("ConfigPath = %q, want config.json", rc.ConfigPath)
	}
	if rc.Debug {
		t.Error("Debug should default false")
	}
	if rc.AutoUpdateEnabled {
		t.Error("AutoUpdateEnabled should default false")
	}
	if rc.AutoUpdateInterval != updater.DefaultCheckInterval {
		t.Errorf("AutoUpdateInterval = %s, want default %s", rc.AutoUpdateInterval, updater.DefaultCheckInterval)
	}
	if rc.LifecycleForceRunning {
		t.Error("LifecycleForceRunning should default false")
	}
	if rc.Dashboard.AuthEnabled() {
		t.Error("no credentials -> AuthEnabled should be false")
	}
	if rc.Dashboard.HostOverride != "" || rc.Dashboard.InsecureNoAuth ||
		rc.Dashboard.DevPredictions || len(rc.Dashboard.TrustedOrigins) != 0 {
		t.Errorf("empty env should yield a zero Dashboard, got %+v", rc.Dashboard)
	}
	if rc.Dashboard.TrustedLANCIDRs != nil || rc.Dashboard.TrustedLANCIDRsErr != "" {
		t.Errorf("empty env should yield no trusted-LAN CIDRs and no error, got %+v", rc.Dashboard)
	}
}

// TestResolveAutoUpdatePrecedence locks the exact flag/env precedence carried
// over from main.autoUpdateEnabled.
func TestResolveAutoUpdatePrecedence(t *testing.T) {
	cases := []struct {
		name string
		flag bool
		env  map[string]string
		want bool
	}{
		{"flag on, env absent", true, nil, true},
		{"flag on, env false (flag wins)", true, map[string]string{"AUTO_UPDATE": "false"}, true},
		{"flag off, env true", false, map[string]string{"AUTO_UPDATE": "true"}, true},
		{"flag off, env 1", false, map[string]string{"AUTO_UPDATE": "1"}, true},
		{"flag off, env false", false, map[string]string{"AUTO_UPDATE": "false"}, false},
		{"flag off, env absent", false, nil, false},
		{"flag off, env garbage", false, map[string]string{"AUTO_UPDATE": "sometimes"}, false},
		{"flag off, env empty", false, map[string]string{"AUTO_UPDATE": ""}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rc := Resolve(Flags{AutoUpdate: tc.flag}, envMap(tc.env))
			if rc.AutoUpdateEnabled != tc.want {
				t.Errorf("AutoUpdateEnabled = %v, want %v", rc.AutoUpdateEnabled, tc.want)
			}
		})
	}
}

func TestResolveAutoUpdateInterval(t *testing.T) {
	cases := []struct {
		raw  string
		want time.Duration
	}{
		{"", updater.DefaultCheckInterval},
		{"8h", 8 * time.Hour},
		{"12", 12 * time.Hour},
		{"1m", 15 * time.Minute}, // clamped up to the minimum
		{"garbage", updater.DefaultCheckInterval},
	}
	for _, tc := range cases {
		env := map[string]string{}
		if tc.raw != "" {
			env["AUTO_UPDATE_CHECK_INTERVAL"] = tc.raw
		}
		rc := Resolve(Flags{}, envMap(env))
		if rc.AutoUpdateInterval != tc.want {
			t.Errorf("interval(%q) = %s, want %s", tc.raw, rc.AutoUpdateInterval, tc.want)
		}
	}
}

func TestResolveDashboard(t *testing.T) {
	rc := Resolve(Flags{}, envMap(map[string]string{
		"DASHBOARD_HOST":             "  0.0.0.0  ",
		"DASHBOARD_USERNAME":         "admin",
		"DASHBOARD_PASSWORD":         "  spaced-secret  ",
		"DASHBOARD_INSECURE_NO_AUTH": "true",
		"DASHBOARD_TRUSTED_ORIGINS":  "https://miner.example.com, proxy.lan:8443",
		"MINER_DEV_PREDICTIONS":      "on",
	}))

	d := rc.Dashboard
	if d.HostOverride != "0.0.0.0" {
		t.Errorf("HostOverride = %q, want trimmed 0.0.0.0", d.HostOverride)
	}
	if d.Username != "admin" {
		t.Errorf("Username = %q", d.Username)
	}
	// Credentials are captured verbatim (surrounding spaces preserved) — the
	// raw value is reachable only via Reveal().
	if d.Password.Reveal() != "  spaced-secret  " {
		t.Errorf("Password must be captured verbatim, got %q", d.Password.Reveal())
	}
	if !d.AuthEnabled() {
		t.Error("both credentials set -> AuthEnabled should be true")
	}
	if !d.InsecureNoAuth {
		t.Error("DASHBOARD_INSECURE_NO_AUTH=true should resolve InsecureNoAuth")
	}
	if !d.DevPredictions {
		t.Error("MINER_DEV_PREDICTIONS=on should enable DevPredictions")
	}
	want := []string{"miner.example.com", "proxy.lan:8443"}
	if got := d.TrustedOriginHosts(); !equalStrings(got, want) {
		t.Errorf("TrustedOriginHosts = %v, want %v", got, want)
	}
}

func TestResolveInsecureNoAuthOnlyTruthy(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{"true", true}, {"1", true}, {"TRUE", true},
		{"false", false}, {"0", false}, {"", false}, {"garbage", false},
	} {
		rc := Resolve(Flags{}, envMap(map[string]string{"DASHBOARD_INSECURE_NO_AUTH": tc.raw}))
		if rc.Dashboard.InsecureNoAuth != tc.want {
			t.Errorf("InsecureNoAuth(%q) = %v, want %v", tc.raw, rc.Dashboard.InsecureNoAuth, tc.want)
		}
	}
}

// TestResolveLifecycleForceRunning locks LIFECYCLE_FORCE_RUNNING's resolution:
// truthy only (parseBool semantics, matching InsecureNoAuth), unset/garbage
// values default to false.
func TestResolveLifecycleForceRunning(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  bool
		raw  string
		want bool
	}{
		{"unset", false, "", false},
		{"true", true, "true", true},
		{"1", true, "1", true},
		{"TRUE", true, "TRUE", true},
		{"false", true, "false", false},
		{"0", true, "0", false},
		{"empty string set", true, "", false},
		{"garbage", true, "sometimes", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{}
			if tc.set {
				env["LIFECYCLE_FORCE_RUNNING"] = tc.raw
			}
			rc := Resolve(Flags{}, envMap(env))
			if rc.LifecycleForceRunning != tc.want {
				t.Errorf("LifecycleForceRunning(%q, set=%v) = %v, want %v", tc.raw, tc.set, rc.LifecycleForceRunning, tc.want)
			}
		})
	}
}

// TestDevPredictionsTruthySet locks the exact accepted set (matching the prior
// web.devPredictionsEnabled), including values ParseBool would reject.
func TestDevPredictionsTruthySet(t *testing.T) {
	on := []string{"1", "true", "TRUE", "yes", "on"}
	off := []string{"", "0", "false", "yes ", "On", "YES", "enabled", "2"}
	for _, v := range on {
		if !parseDevPredictions(v) {
			t.Errorf("%q should enable dev predictions", v)
		}
	}
	for _, v := range off {
		if parseDevPredictions(v) {
			t.Errorf("%q should NOT enable dev predictions", v)
		}
	}
}

func TestParseTrustedOrigins(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"https://miner.example.com", []string{"miner.example.com"}},
		{"proxy.lan:8443", []string{"proxy.lan:8443"}},
		{"https://a.example.com, b.lan:8443 , ,https://c.example.com:9000",
			[]string{"a.example.com", "b.lan:8443", "c.example.com:9000"}},
	}
	for _, tc := range cases {
		got := ParseTrustedOrigins(tc.raw)
		if !equalStrings(got, tc.want) {
			t.Errorf("ParseTrustedOrigins(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

// TestTrustedOriginHostsDefensiveCopy proves the accessor never leaks the
// snapshot's internal slice: mutating the returned slice must not change the
// snapshot, satisfying the immutability contract.
func TestTrustedOriginHostsDefensiveCopy(t *testing.T) {
	rc := Resolve(Flags{}, envMap(map[string]string{
		"DASHBOARD_TRUSTED_ORIGINS": "a.example.com,b.example.com",
	}))
	got := rc.Dashboard.TrustedOriginHosts()
	got[0] = "evil.example.com"

	fresh := rc.Dashboard.TrustedOriginHosts()
	if fresh[0] != "a.example.com" {
		t.Fatalf("mutating the returned slice corrupted the snapshot: %v", fresh)
	}
}

// TestRedaction proves the dashboard password never appears in the snapshot's
// String() or LogValue() output, while its presence is still signalled.
func TestRedaction(t *testing.T) {
	rc := Resolve(Flags{}, envMap(map[string]string{
		"DASHBOARD_USERNAME": "admin",
		"DASHBOARD_PASSWORD": "super-secret-pw",
	}))

	s := rc.String()
	if strings.Contains(s, "super-secret-pw") {
		t.Errorf("String() leaked the password: %s", s)
	}
	if !strings.Contains(s, "***") {
		t.Errorf("String() should signal a configured password with ***: %s", s)
	}

	// LogValue must not carry the raw secret either.
	lv := rc.LogValue().String()
	if strings.Contains(lv, "super-secret-pw") {
		t.Errorf("LogValue() leaked the password: %s", lv)
	}

	// An unset password is reported as empty, not "***".
	empty := Resolve(Flags{}, envMap(nil))
	if strings.Contains(empty.String(), "***") {
		t.Errorf("unset password should not render as ***: %s", empty.String())
	}
}

// TestParseTrustedLANCIDRs is the seam-1 parser matrix for
// DASHBOARD_TRUSTED_LAN_CIDRS: valid IPv4/IPv6, multiple entries with
// whitespace and a trailing comma, Masked() normalization of host bits,
// not-configured cases, and malformed entries (each error naming the
// offending entry and the variable).
func TestParseTrustedLANCIDRs(t *testing.T) {
	t.Run("valid IPv4", func(t *testing.T) {
		got, err := ParseTrustedLANCIDRs("192.168.1.0/24")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 || got[0].String() != "192.168.1.0/24" {
			t.Fatalf("got %v, want [192.168.1.0/24]", got)
		}
	})

	t.Run("valid IPv6", func(t *testing.T) {
		got, err := ParseTrustedLANCIDRs("fd00::/64")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 || got[0].String() != "fd00::/64" {
			t.Fatalf("got %v, want [fd00::/64]", got)
		}
	})

	t.Run("multiple mixed with spaces and trailing comma", func(t *testing.T) {
		got, err := ParseTrustedLANCIDRs(" 192.168.1.0/24 , fd00::/64 ,")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []string{"192.168.1.0/24", "fd00::/64"}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i, p := range got {
			if p.String() != want[i] {
				t.Errorf("entry %d = %s, want %s", i, p.String(), want[i])
			}
		}
	})

	t.Run("canonical network forms still parse", func(t *testing.T) {
		// Corrective-pass regression: these already have zero host bits, so
		// they must NOT be rejected by the host-bits-set check below.
		for _, raw := range []string{"10.0.0.0/8", "0.0.0.0/0", "::/0", "fd00::/8"} {
			got, err := ParseTrustedLANCIDRs(raw)
			if err != nil {
				t.Errorf("raw %q: unexpected error: %v", raw, err)
				continue
			}
			if len(got) != 1 || got[0].String() != raw {
				t.Errorf("raw %q: got %v, want [%s]", raw, got, raw)
			}
		}
	})

	t.Run("host bits set are rejected, not silently normalized", func(t *testing.T) {
		// Corrective-pass hardening: a set host bit used to be silently
		// masked off (ParsePrefix + .Masked()), which let a typo'd prefix
		// length like "/2" instead of "/24" silently normalize to a vastly
		// wider network ("192.168.1.0/2" -> "192.0.0.0/2") rather than
		// failing loudly. Now every entry must already be its own canonical
		// (Masked) form.
		cases := []struct {
			raw         string
			wantCanonIn string // the suggested canonical form the error must name
		}{
			{"192.168.1.5/24", "192.168.1.0/24"},
			{"192.168.1.0/2", "192.0.0.0/2"}, // the exact typo example from the corrective pass
			{"fd12::1/64", "fd12::/64"},
		}
		for _, tc := range cases {
			got, err := ParseTrustedLANCIDRs(tc.raw)
			if err == nil {
				t.Errorf("raw %q: expected an error (host bits set), got prefixes %v", tc.raw, got)
				continue
			}
			if got != nil {
				t.Errorf("raw %q: fail-closed should return nil prefixes on error, got %v", tc.raw, got)
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.raw) {
				t.Errorf("raw %q: error %q must name the offending entry", tc.raw, msg)
			}
			if !strings.Contains(msg, tc.wantCanonIn) {
				t.Errorf("raw %q: error %q must suggest the canonical form %q", tc.raw, msg, tc.wantCanonIn)
			}
			if !strings.Contains(msg, "host bits set") {
				t.Errorf("raw %q: error %q must say what's wrong (host bits set)", tc.raw, msg)
			}
			if !strings.Contains(msg, "DASHBOARD_TRUSTED_LAN_CIDRS") {
				t.Errorf("raw %q: error %q must name DASHBOARD_TRUSTED_LAN_CIDRS", tc.raw, msg)
			}
		}
	})

	t.Run("IPv4-mapped IPv6 (4-in-6) prefixes are rejected", func(t *testing.T) {
		// Corrective-pass hardening: lifecycleLANTrust always Unmap()s the
		// connection address before matching, so a 4-in-6 prefix could
		// never match anything - accepting it would be silent dead
		// configuration rather than a fail-closed error. This entry is
		// already in canonical (host-bits-zero) form, so it must be caught
		// by the 4-in-6 check specifically, not the host-bits check above.
		got, err := ParseTrustedLANCIDRs("::ffff:192.168.0.0/112")
		if err == nil {
			t.Fatalf("expected an error for a 4-in-6 prefix, got prefixes %v", got)
		}
		if got != nil {
			t.Errorf("fail-closed should return nil prefixes on error, got %v", got)
		}
		msg := err.Error()
		for _, want := range []string{"::ffff:192.168.0.0/112", "DASHBOARD_TRUSTED_LAN_CIDRS", "IPv4"} {
			if !strings.Contains(msg, want) {
				t.Errorf("error %q must contain %q", msg, want)
			}
		}
	})

	t.Run("empty and whitespace-only are not configured", func(t *testing.T) {
		for _, raw := range []string{"", "   ", ",", " , ", " , , "} {
			got, err := ParseTrustedLANCIDRs(raw)
			if err != nil {
				t.Errorf("raw %q: unexpected error: %v", raw, err)
			}
			if got != nil {
				t.Errorf("raw %q: got %v, want nil", raw, got)
			}
		}
	})

	t.Run("malformed entries error and name the offending entry", func(t *testing.T) {
		cases := []struct {
			raw    string
			wantIn string // substring the offending entry text must contain
		}{
			{"foo", "foo"},
			{"192.168.1.0/33", "192.168.1.0/33"},
			{"10.0.0.1", "10.0.0.1"},                               // bare IP, no prefix length
			{"10.0.0.0/24 10.0.0.1/32", "10.0.0.0/24 10.0.0.1/32"}, // space inside one entry
			{"192.168.1.0/24,foo", "foo"},                          // one bad entry among good ones
		}
		for _, tc := range cases {
			got, err := ParseTrustedLANCIDRs(tc.raw)
			if err == nil {
				t.Errorf("raw %q: expected an error, got prefixes %v", tc.raw, got)
				continue
			}
			if got != nil {
				t.Errorf("raw %q: fail-closed should return nil prefixes on error, got %v", tc.raw, got)
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.wantIn) {
				t.Errorf("raw %q: error %q must name the offending entry %q", tc.raw, msg, tc.wantIn)
			}
			if !strings.Contains(msg, "DASHBOARD_TRUSTED_LAN_CIDRS") {
				t.Errorf("raw %q: error %q must name DASHBOARD_TRUSTED_LAN_CIDRS", tc.raw, msg)
			}
		}
	})
}

// TestResolveTrustedLANCIDRs covers Resolve's fail-closed pairing: unset ->
// nil prefixes + empty error; valid -> populated prefixes + empty error;
// invalid -> nil prefixes + non-empty error, with the rest of Dashboard still
// resolved normally alongside it.
func TestResolveTrustedLANCIDRs(t *testing.T) {
	t.Run("unset", func(t *testing.T) {
		rc := Resolve(Flags{}, envMap(nil))
		if rc.Dashboard.TrustedLANCIDRs != nil {
			t.Errorf("TrustedLANCIDRs = %v, want nil", rc.Dashboard.TrustedLANCIDRs)
		}
		if rc.Dashboard.TrustedLANCIDRsErr != "" {
			t.Errorf("TrustedLANCIDRsErr = %q, want empty", rc.Dashboard.TrustedLANCIDRsErr)
		}
	})

	t.Run("valid", func(t *testing.T) {
		rc := Resolve(Flags{}, envMap(map[string]string{
			"DASHBOARD_TRUSTED_LAN_CIDRS": "10.0.0.0/8,fd00::/64",
			"DASHBOARD_USERNAME":          "admin",
		}))
		if len(rc.Dashboard.TrustedLANCIDRs) != 2 {
			t.Fatalf("TrustedLANCIDRs = %v, want 2 entries", rc.Dashboard.TrustedLANCIDRs)
		}
		if rc.Dashboard.TrustedLANCIDRsErr != "" {
			t.Errorf("TrustedLANCIDRsErr = %q, want empty", rc.Dashboard.TrustedLANCIDRsErr)
		}
		// Sibling Dashboard field still resolved alongside the new one.
		if rc.Dashboard.Username != "admin" {
			t.Errorf("Username = %q, want admin", rc.Dashboard.Username)
		}
	})

	t.Run("invalid fails closed", func(t *testing.T) {
		rc := Resolve(Flags{}, envMap(map[string]string{
			"DASHBOARD_TRUSTED_LAN_CIDRS": "not-a-cidr",
			"DASHBOARD_HOST":              "0.0.0.0",
		}))
		if rc.Dashboard.TrustedLANCIDRs != nil {
			t.Errorf("TrustedLANCIDRs = %v, want nil on parse error", rc.Dashboard.TrustedLANCIDRs)
		}
		if rc.Dashboard.TrustedLANCIDRsErr == "" {
			t.Error("TrustedLANCIDRsErr should be set on parse error")
		}
		if !strings.Contains(rc.Dashboard.TrustedLANCIDRsErr, "DASHBOARD_TRUSTED_LAN_CIDRS") {
			t.Errorf("TrustedLANCIDRsErr = %q must name DASHBOARD_TRUSTED_LAN_CIDRS", rc.Dashboard.TrustedLANCIDRsErr)
		}
		// Sibling Dashboard field still resolved despite the parse error.
		if rc.Dashboard.HostOverride != "0.0.0.0" {
			t.Errorf("HostOverride = %q, want 0.0.0.0", rc.Dashboard.HostOverride)
		}
	})
}

// TestTrustedLANCIDRPrefixesDefensiveCopy mirrors
// TestTrustedOriginHostsDefensiveCopy (:340-351): mutating the returned slice
// must not corrupt the snapshot.
func TestTrustedLANCIDRPrefixesDefensiveCopy(t *testing.T) {
	rc := Resolve(Flags{}, envMap(map[string]string{
		"DASHBOARD_TRUSTED_LAN_CIDRS": "10.0.0.0/8,192.168.0.0/16",
	}))
	got := rc.Dashboard.TrustedLANCIDRPrefixes()
	got[0] = got[1]

	fresh := rc.Dashboard.TrustedLANCIDRPrefixes()
	if fresh[0].String() != "10.0.0.0/8" {
		t.Fatalf("mutating the returned slice corrupted the snapshot: %v", fresh)
	}
}

// TestDashboardFormattingCoversTrustedLANFields extends the formatting
// matrix (TestRedaction/TestSecretNeverLeaksInFormatting neighbors) to the
// new fields: String/LogValue render a count (never the raw CIDR list as a
// count substitute for secrecy - CIDRs are not secrets, this is just the
// established TrustedOrigins style) and GoString carries the full list.
func TestDashboardFormattingCoversTrustedLANFields(t *testing.T) {
	valid := Resolve(Flags{}, envMap(map[string]string{
		"DASHBOARD_TRUSTED_LAN_CIDRS": "10.0.0.0/8,192.168.0.0/16",
	})).Dashboard

	if !strings.Contains(valid.String(), "trustedLANCIDRs:2") {
		t.Errorf("String() should show the CIDR count: %s", valid.String())
	}
	if !strings.Contains(valid.String(), `trustedLANCIDRsErr:""`) {
		t.Errorf("String() should show an empty trustedLANCIDRsErr when unset: %s", valid.String())
	}

	// GoString() carries the full field (not merely a count) - assert only
	// on the exported field name, never on netip.Prefix's unexported
	// internal layout (e.g. its bitsPlusOne field), which is a stdlib
	// implementation detail, not this package's exported behavior.
	goStr := fmt.Sprintf("%#v", valid)
	if !strings.Contains(goStr, "TrustedLANCIDRs:") {
		t.Errorf("GoString() should carry the TrustedLANCIDRs field: %s", goStr)
	}

	lv := valid.LogValue().String()
	if !strings.Contains(lv, "trustedLANCIDRs=2") {
		t.Errorf("LogValue() should show the CIDR count: %s", lv)
	}
	if !strings.Contains(lv, "trustedLANCIDRsErr=") {
		t.Errorf("LogValue() should carry the trustedLANCIDRsErr attribute: %s", lv)
	}

	invalid := Resolve(Flags{}, envMap(map[string]string{
		"DASHBOARD_TRUSTED_LAN_CIDRS": "garbage",
	})).Dashboard
	if !strings.Contains(invalid.String(), "trustedLANCIDRsErr:") || strings.Contains(invalid.String(), `trustedLANCIDRsErr:""`) {
		t.Errorf("String() should surface a non-empty parse error: %s", invalid.String())
	}
	if lv := invalid.LogValue().String(); strings.Contains(lv, `trustedLANCIDRsErr=""`) {
		t.Errorf("LogValue() should surface a non-empty parse error, not an empty one: %s", lv)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
