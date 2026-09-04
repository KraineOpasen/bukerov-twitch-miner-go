package web

import (
	"bytes"
	"html"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/i18n"
)

// renderLogsLinesForTest renders the real logs_lines partial over the given
// views, so visibility/metadata assertions run through the same template the
// page and the /api/logs htmx refresh both use.
func renderLogsLinesForTest(t *testing.T, lines []LogLineView) string {
	t.Helper()
	var buf bytes.Buffer
	if err := testPartials(t).ExecuteTemplate(&buf, "logs_lines", LogsLinesData{FileLogging: true, Lines: lines}); err != nil {
		t.Fatalf("render logs_lines: %v", err)
	}
	return buf.String()
}

// renderLogsLinesLangForTest renders the partial in one explicit language, so
// assertions on localized copy do not depend on the default-language setting.
func renderLogsLinesLangForTest(t *testing.T, lang string, lines []LogLineView) string {
	t.Helper()
	var buf bytes.Buffer
	if err := testPartialsLang(t, lang).ExecuteTemplate(&buf, "logs_lines", LogsLinesData{FileLogging: true, Lines: lines}); err != nil {
		t.Fatalf("render logs_lines (%s): %v", lang, err)
	}
	return buf.String()
}

// This file holds the behavioral guards for the Logs page signal/noise
// contract: severity and subsystem are INDEPENDENT dimensions, low-value
// implementation chatter never reaches the dashboard list, and a WARN or an
// ERROR is never hidden just because its msg is not in the rule table.
//
// The classifier is the single authority for all four dimensions
// (class/emoji, level, subsystem, dashboard visibility) — the browser must
// not rebuild any of them from a CSS class.

// ---------------------------------------------------------------------
// Dashboard visibility: raw transport/debug chatter must not reach the page
// ---------------------------------------------------------------------

// TestDashboardHidesUnclassifiedStructuredChatter pins the visibility
// contract for structured DEBUG/INFO the classifier does not recognize as a
// user-facing event. These are real msg literals the miner emits (file
// logging defaults to DEBUG, so every one of them lands in the retained file
// the page reads) and they are exactly the "raw debug console" noise the
// Logs page must not become.
func TestDashboardHidesUnclassifiedStructuredChatter(t *testing.T) {
	const ts = "time=2026-07-14T10:00:00.000+03:00 "

	cases := []struct {
		name string
		line string
	}{
		// internal/twitch/client.go:689,970
		{"gql response", ts + `level=DEBUG msg="GQL response" operation=ChannelPointsContext status=200`},
		// internal/pubsub/websocket.go:749 (LISTEN/UNLISTEN frames)
		{"websocket send", ts + `level=DEBUG msg="WebSocket send" index=0 type=LISTEN`},
		// internal/pubsub/websocket.go:848 — this IS the PING keepalive traffic
		{"websocket send ping", ts + `level=DEBUG msg="WebSocket send" index=0 type=PING`},
		// internal/pubsub/websocket.go:1019 — this IS the PONG keepalive traffic
		{"websocket received pong", ts + `level=DEBUG msg="WebSocket received" index=0 type=PONG`},
		{"websocket received", ts + `level=DEBUG msg="WebSocket received" index=1 type=MESSAGE`},
		// internal/chat/client.go:575 — logs the whole raw IRC line
		{"irc message received", ts + `level=DEBUG msg="IRC message received" channel=xqc line="PING :tmi.twitch.tv"`},
		// internal/miner/miner.go:895 — boot-time flag dump
		{"chat logging config", ts + `level=DEBUG msg="Chat logging config" enableAnalytics=true enableChatLogs=false`},
		// internal/notifications/discord.go:234 — per-message success trace
		{"discord notification sent", ts + `level=DEBUG msg="Discord notification sent" channel=general type=points`},
		// internal/drops/drops.go:1431 — pure stage-counter dump
		{"drops pipeline counters", ts + `level=DEBUG msg="Drops sync: campaign counts through the pipeline" dashboardCount=4 afterInventory=4`},
		// A structured INFO nobody has given a meaning to yet.
		{"unknown structured info", ts + `level=INFO msg="some internal bookkeeping" n=3`},
		// A structured DEBUG nobody has given a meaning to yet.
		{"unknown structured debug", ts + `level=DEBUG msg="tick" n=3`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyLogLine(tc.line)
			if got.DashboardVisible {
				t.Errorf("DashboardVisible = true for unrecognized %s line %q; low-value implementation chatter must not reach the dashboard Logs list", got.Level, tc.line)
			}
		})
	}
}

// TestDashboardShowsMeaningfulEvents is the other half of the visibility
// contract: every event the classifier recognizes as user-facing stays on
// the page. A suppression rule that swallowed real operational signal would
// be far worse than the noise it removes.
func TestDashboardShowsMeaningfulEvents(t *testing.T) {
	const ts = "time=2026-07-14T10:00:00.000+03:00 "

	lines := []string{
		ts + `level=INFO msg="Points earned" streamer=xqc points=10 reason=WATCH balance=1000`,
		ts + `level=INFO msg="Claiming bonus" streamer=xqc`,
		ts + `level=INFO msg="Streamer is online" streamer=shroud`,
		ts + `level=INFO msg="Streamer went offline" streamer=shroud`,
		ts + `level=INFO msg="Watch slot assigned" streamer=xqc`,
		ts + `level=INFO msg="Watch slot released" streamer=xqc`,
		ts + `level=INFO msg="Rotating watch pair"`,
		ts + `level=INFO msg="Authentication successful" username=bukerov`,
		ts + `level=INFO msg="Reconnecting WebSocket" index=0`,
		ts + `level=INFO msg="Claimed drop" name="Tier 1"`,
		ts + `level=INFO msg="Auto-update: newer release available" version=1.2.3`,
		ts + `level=INFO msg="Prediction result" result=WIN points=500`,
		ts + `level=INFO msg="Placing prediction bet" streamer=xqc`,
	}
	for _, line := range lines {
		got := classifyLogLine(line)
		if !got.DashboardVisible {
			t.Errorf("DashboardVisible = false for meaningful event %q — real operational signal must stay on the page", line)
		}
	}
}

// TestNewlyCategorizedMeaningfulEvents covers the DEBUG/INFO events that are
// genuinely user-facing but had no semantic rule, so they used to render as
// anonymous "log-info" lines indistinguishable from raw chatter. Each one is
// a real production literal (file:line in logclass.go's rule comments); each
// must now be visible AND land in its semantic subsystem.
func TestNewlyCategorizedMeaningfulEvents(t *testing.T) {
	const ts = "time=2026-07-14T10:00:00.000+03:00 "

	cases := []struct {
		name      string
		line      string
		subsystem string
	}{
		// internal/watcher/watcher.go:1078 — the heartbeat that actually earns points.
		{"sent minute watched", ts + `level=DEBUG msg="Sent minute watched" streamer=xqc origin=slot minutesWatched=12`, "watch"},
		// internal/watcher/watcher.go:994 — "who am I currently watching".
		{"watching streams", ts + `level=DEBUG msg="Watching streams" count=2 max=2 streamers="xqc,shroud"`, "watch"},
		// internal/watcher/session.go:524 — the spade/session refresh outcome.
		{"watch session refresh", ts + `level=INFO msg="Watch session refresh" channel=xqc success=true`, "watch"},
		// internal/watcher/watcher.go:2194 — the real literal is far longer than
		// the old exact-match rule, which therefore never fired.
		{"pursuing watch streak", ts + `level=INFO msg="Pursuing watch streak (holding a boost slot until Twitch grants it or the bounded watch window elapses)" streamer=xqc`, "watch"},
		// internal/miner/rewards.go:291 — the budget gate on point spending.
		{"auto-redeem over budget", ts + `level=DEBUG msg="Auto-redeem: over budget, skipping" streamer=xqc reward=Emote cost=500 remaining=100`, "points"},
		// internal/miner/rewards.go:328 / internal/miner/rewards.go:91
		{"auto-redeemed reward", ts + `level=INFO msg="Auto-redeemed custom reward" streamer=xqc reward=Emote`, "points"},
		// internal/twitch/client.go:1689 — an online/offline state-machine outcome.
		{"bring-online superseded", ts + `level=DEBUG msg="Bring-online session superseded by a newer observation; recording unknown (not online)" streamer=xqc`, "stream"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyLogLine(tc.line)
			if !got.DashboardVisible {
				t.Errorf("DashboardVisible = false for %q — this is a meaningful user-facing event", tc.line)
			}
			if got.Subsystem != tc.subsystem {
				t.Errorf("Subsystem = %q, want %q for %q", got.Subsystem, tc.subsystem, tc.line)
			}
		})
	}
}

// ---------------------------------------------------------------------
// WARN / ERROR safety
// ---------------------------------------------------------------------

// TestWarnErrorAlwaysVisible is the safety net: severity alone guarantees a
// place on the page. No suppression rule may ever hide a WARN or an ERROR,
// however unrecognized its msg.
func TestWarnErrorAlwaysVisible(t *testing.T) {
	const ts = "time=2026-07-14T10:00:00.000+03:00 "

	lines := []string{
		ts + `level=WARN msg="something nobody has classified yet" n=1`,
		ts + `level=ERROR msg="a brand new failure mode" error=boom`,
		ts + `level=WARN msg="GQL request failed, retrying" attempt=2`,
		ts + `level=ERROR msg="Failed to join raid" error=boom`,
		// The viewerDropCampaigns == null transition WARN: its UNKNOWN
		// semantics are out of scope here, but it must stay visible.
		ts + `level=WARN msg="Drops sync failed: could not fetch active drop campaigns from Twitch; keeping previously tracked campaigns" error=null`,
	}
	for _, line := range lines {
		got := classifyLogLine(line)
		if !got.DashboardVisible {
			t.Errorf("DashboardVisible = false for %q — a WARN/ERROR is never hidden", line)
		}
	}
}

// TestWarnErrorKeepSemanticSubsystem is the orthogonality guard. Before this
// change severity was resolved BEFORE the semantic rules, so every WARN and
// every ERROR collapsed into the "other" subsystem bucket and the subsystem
// filter was useless for exactly the lines an operator cares about most.
// Severity and subsystem are now independent: the level decides the colour,
// the msg decides the bucket.
func TestWarnErrorKeepSemanticSubsystem(t *testing.T) {
	const ts = "time=2026-07-14T10:00:00.000+03:00 "

	cases := []struct {
		name      string
		line      string
		level     string
		subsystem string
		class     string
	}{
		{
			"drops warn keeps drops",
			ts + `level=WARN msg="Drops sync failed: could not fetch active drop campaigns from Twitch; keeping previously tracked campaigns" error=timeout`,
			"warning", "drops", "log-warn",
		},
		{
			"drop claim error keeps drops",
			ts + `level=ERROR msg="Failed to claim drop" drop="Tier 1" error=boom`,
			"error", "drops", "log-error",
		},
		{
			"watcher error keeps watch",
			ts + `level=ERROR msg="Watcher loop did not finish within the stop timeout; teardown is dirty and the watch generation is still live"`,
			"error", "watch", "log-error",
		},
		{
			"watch prune warn keeps watch",
			ts + `level=WARN msg="Failed to prune old watch-time events" error=locked`,
			"warning", "watch", "log-warn",
		},
		{
			"auth warn keeps auth",
			ts + `level=WARN msg="Twitch refresh token authoritatively rejected; falling back to device login"`,
			"warning", "auth", "log-warn",
		},
		{
			"websocket error keeps stream",
			ts + `level=ERROR msg="WebSocket read error; reconnecting" index=0 error=eof`,
			"error", "stream", "log-error",
		},
		{
			"bonus claim error keeps points",
			ts + `level=ERROR msg="Failed to claim bonus" streamer=xqc error=boom`,
			"error", "points", "log-error",
		},
		{
			"prediction error keeps predictions",
			ts + `level=ERROR msg="Failed to make prediction" streamer=xqc error=boom`,
			"error", "predictions", "log-error",
		},
		// These two literals carry a trailing clause the rule table cannot
		// spell out exactly (one ends "… until this is fixed", the other is
		// built by concatenation), so they must be matched by prefix. An
		// exact rule silently never fires and the ERROR falls back to
		// "other" — the precise failure this contract forbids.
		// internal/miner/streamer_deletion.go:70
		{
			"streamer-deletion coordinator error keeps service",
			ts + `level=ERROR msg="Failed to build streamer-deletion coordinator; removals will not purge persisted history until this is fixed" error=boom`,
			"error", "service", "log-error",
		},
		// internal/miner/miner.go:2394 (msg built by concatenation)
		{
			"compensate-removal error keeps service",
			ts + `level=ERROR msg="Failed to compensate a prepared streamer removal after a crash; startup arbitration will resolve it on the next restart" error=boom`,
			"error", "service", "log-error",
		},
		// internal/web/handlers_drops.go:150 — the contract's named case.
		{
			"past drop campaigns error keeps drops",
			ts + `level=ERROR msg="Failed to load past drop campaigns" error=boom`,
			"error", "drops", "log-error",
		},
		// internal/streamer/streakcache.go — the watch-streak cache family.
		{
			"streak cache warn keeps watch",
			ts + `level=WARN msg="Streak cache is corrupt; ignoring it without manufacturing streak success" error=boom`,
			"warning", "watch", "log-warn",
		},
		{
			"streak cache write error keeps watch",
			ts + `level=ERROR msg="Failed to write streak cache; terminal state will not survive restart" error=boom`,
			"error", "watch", "log-error",
		},
		// internal/streamer/manager.go — reconciliation conflicts.
		{
			"reconciliation conflict warn keeps service",
			ts + `level=WARN msg="Streamer reconciliation conflict: configured login already resolves to a different tracked channel; skipping" login=a`,
			"warning", "service", "log-warn",
		},
		// internal/analytics/service.go:115
		{
			"prediction bet record error keeps predictions",
			ts + `level=ERROR msg="Failed to record prediction bet" error=boom`,
			"error", "predictions", "log-error",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyLogLine(tc.line)
			if got.Level != tc.level {
				t.Errorf("Level = %q, want %q", got.Level, tc.level)
			}
			if got.Subsystem != tc.subsystem {
				t.Errorf("Subsystem = %q, want %q — severity must not erase the semantic bucket", got.Subsystem, tc.subsystem)
			}
			if !got.DashboardVisible {
				t.Error("DashboardVisible = false; a WARN/ERROR is never hidden")
			}
		})
	}

	// Severity owns the COLOUR so a WARN still reads as a WARN at a glance;
	// the subsystem asserted above is what stays independent of it.
	for _, tc := range cases {
		got := classifyLogLine(tc.line)
		if got.Class != tc.class {
			t.Errorf("%s: Class = %q, want %q", tc.name, got.Class, tc.class)
		}
	}
}

// TestUnknownWarnErrorFallsBackToOther pins the documented fallback: an
// unrecognized WARN/ERROR is visible under "Other" rather than being
// silently dropped or forced into a bucket it does not belong to.
func TestUnknownWarnErrorFallsBackToOther(t *testing.T) {
	const ts = "time=2026-07-14T10:00:00.000+03:00 "

	cases := []struct {
		line  string
		level string
	}{
		{ts + `level=WARN msg="a completely novel warning" n=1`, "warning"},
		{ts + `level=ERROR msg="a completely novel error" n=1`, "error"},
	}
	for _, tc := range cases {
		got := classifyLogLine(tc.line)
		if got.Level != tc.level {
			t.Errorf("Level = %q, want %q for %q", got.Level, tc.level, tc.line)
		}
		if got.Subsystem != "other" {
			t.Errorf("Subsystem = %q, want %q for %q", got.Subsystem, "other", tc.line)
		}
		if !got.DashboardVisible {
			t.Errorf("DashboardVisible = false for %q", tc.line)
		}
	}
}

// TestMalformedLineIsShownNotSilentlyDropped pins the conservative policy for
// lines the parser cannot read a level from (a legacy format, a panic dump, a
// line mangled by a partial write). Such a line is NOT assumed harmless: it
// stays visible under "Other" so nothing disappears without a trace.
func TestMalformedLineIsShownNotSilentlyDropped(t *testing.T) {
	lines := []string{
		`some free-form line with no slog tokens at all`,
		`panic: runtime error: invalid memory address`,
		`goroutine 1 [running]:`,
		`time=2026-07-14T10:00:00.000+03:00 msg="level attribute is missing entirely"`,
		`level=TRACE msg="an unrecognized level name"`,
	}
	for _, line := range lines {
		got := classifyLogLine(line)
		if !got.DashboardVisible {
			t.Errorf("DashboardVisible = false for severity-unknown line %q — a line whose level cannot be read must not be silently discarded", line)
		}
		if got.Subsystem != "other" {
			t.Errorf("Subsystem = %q, want %q for %q", got.Subsystem, "other", line)
		}
	}
}

// TestClassifierNeverDowngradesSeverity guards against the lazy way to quiet
// the dashboard: reclassifying a noisy WARN as an informational line.
func TestClassifierNeverDowngradesSeverity(t *testing.T) {
	const ts = "time=2026-07-14T10:00:00.000+03:00 "

	cases := []struct {
		line  string
		level string
	}{
		{ts + `level=ERROR msg="Rotating watch pair"`, "error"},
		{ts + `level=WARN msg="Streamer went offline" streamer=a`, "warning"},
		{ts + `level=WARN msg="Auto-bet gated" streamer=a`, "warning"},
		{ts + `level=ERROR msg="Prediction result" result=WIN`, "error"},
		{ts + `level=WARN msg="Points earned" reason=WATCH`, "warning"},
		{ts + `level=INFO msg="Streamer is online" streamer=a`, "info"},
		{ts + `level=DEBUG msg="Sent minute watched" streamer=a`, "info"},
	}
	for _, tc := range cases {
		got := classifyLogLine(tc.line)
		if got.Level != tc.level {
			t.Errorf("Level = %q, want %q for %q", got.Level, tc.level, tc.line)
		}
	}
}

// ---------------------------------------------------------------------
// Taxonomy integrity
// ---------------------------------------------------------------------

// TestEverySubsystemIsASupportedFilterValue pins the taxonomy closed: every
// subsystem the classifier can emit must be one of the values the Logs page
// subsystem <select> actually offers, or a line would silently escape every
// filter bucket.
func TestEverySubsystemIsASupportedFilterValue(t *testing.T) {
	supported := map[string]bool{}
	for _, s := range allLogSubsystems() {
		if supported[s] {
			t.Errorf("allLogSubsystems() lists %q more than once", s)
		}
		supported[s] = true
	}

	// The taxonomy is fixed by the owner-approved filter list.
	want := []string{
		"service", "auth", "stream", "points", "predictions",
		"watch", "drops", "updater", "system", "other",
	}
	if len(supported) != len(want) {
		t.Errorf("allLogSubsystems() has %d entries, want %d (%v)", len(supported), len(want), allLogSubsystems())
	}
	for _, s := range want {
		if !supported[s] {
			t.Errorf("allLogSubsystems() is missing the supported filter value %q", s)
		}
	}

	// Every rule in both tables must resolve to a supported value.
	for i, rule := range logMsgRules {
		if !supported[rule.subsystem] {
			t.Errorf("logMsgRules[%d] (%s) has unsupported subsystem %q", i, rule.class, rule.subsystem)
		}
	}
	for i, rule := range logSubsystemRules {
		if !supported[rule.subsystem] {
			t.Errorf("logSubsystemRules[%d] has unsupported subsystem %q", i, rule.subsystem)
		}
	}
}

// TestSubsystemRulesNeverGrantVisibility keeps the two tables' jobs apart.
// logSubsystemRules exists only to give a line a semantic bucket (so a WARN
// lands under Drops instead of Other); it must never be a back door that
// makes raw DEBUG chatter visible again.
func TestSubsystemRulesNeverGrantVisibility(t *testing.T) {
	const ts = "time=2026-07-14T10:00:00.000+03:00 "

	// These match logSubsystemRules (subsystem: drops / stream) but no
	// meaningful-event rule, so they stay off the page.
	cases := []struct {
		line      string
		subsystem string
	}{
		{ts + `level=DEBUG msg="Drops sync: fetched active campaigns from dashboard" count=4`, "drops"},
		{ts + `level=DEBUG msg="Drops sync: campaign counts through the pipeline" afterInventory=4`, "drops"},
		{ts + `level=DEBUG msg="WebSocket send" index=0 type=PING`, "stream"},
		{ts + `level=DEBUG msg="IRC message received" channel=a line=x`, "stream"},
	}
	for _, tc := range cases {
		got := classifyLogLine(tc.line)
		if got.DashboardVisible {
			t.Errorf("DashboardVisible = true for %q — a subsystem-only rule must not grant dashboard visibility", tc.line)
		}
		if got.Subsystem != tc.subsystem {
			t.Errorf("Subsystem = %q, want %q for %q", got.Subsystem, tc.subsystem, tc.line)
		}
	}
}

// TestLogTextSurvivesThePipelineByteIdentical guards the raw-evidence rule
// where it can actually break: the text the page renders must be exactly the
// retained on-disk line. Asserting that classifyLogLine does not mutate its
// string argument would be tautological in Go (strings are immutable), so
// this drives the real seam instead — file -> readLogTail -> rendered HTML —
// and compares the decoded rendered text against the bytes on disk.
func TestLogTextSurvivesThePipelineByteIdentical(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll("logs", 0o755); err != nil {
		t.Fatal(err)
	}

	const ts = "time=2026-07-14T10:00:00.000+03:00 "
	lines := []string{
		ts + `level=INFO msg="Points earned" streamer=xqc points=10 reason=WATCH balance=1000`,
		ts + `level=WARN msg="quotes \"inside\" and <angle> & ampersand"`,
		`🟢 ` + ts + `level=INFO msg="Streamer is online" streamer=a`,
		ts + `level=ERROR msg="unicode: кириллица 漢字 emoji 🎉"`,
	}
	if err := os.WriteFile(filepath.Join("logs", "fidelity.log"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &Server{username: "fidelity"}
	views, _ := s.readLogTail()
	if len(views) != len(lines) {
		t.Fatalf("views=%d, want %d", len(views), len(lines))
	}
	for i, v := range views {
		if v.Text != lines[i] {
			t.Errorf("view %d text was rewritten:\n got %q\nwant %q", i, v.Text, lines[i])
		}
	}

	out := renderLogsLinesForTest(t, views)
	for _, want := range lines {
		// html/template escapes for transport; the DECODED text must match
		// the on-disk bytes exactly.
		if !strings.Contains(html.UnescapeString(out), want) {
			t.Errorf("rendered page does not carry the retained line byte-identically: %q", want)
		}
	}
	// The decorative emoji is presentation only: the already-decorated line
	// must not gain a second icon.
	if got := strings.Count(out, "🟢"); got != 1 {
		t.Errorf("rendered output contains %d 🟢, want exactly 1 (the source text's own)", got)
	}
}

// TestEveryMeaningfulFamilyLandsInItsSubsystem asserts the subsystem
// dimension for one representative of every family in the rule table,
// including the two attribute-driven events whose rules are built inline.
func TestEveryMeaningfulFamilyLandsInItsSubsystem(t *testing.T) {
	const ts = "time=2026-07-14T10:00:00.000+03:00 "

	cases := []struct {
		line      string
		subsystem string
	}{
		{ts + `level=INFO msg="Twitch Channel Points Miner" version=1`, "service"},
		{ts + `level=INFO msg="Loading streamers" count=3`, "service"},
		{ts + `level=INFO msg="Streamer removal committed" streamer=a`, "service"},
		{ts + `level=INFO msg="Authenticating with Twitch"`, "auth"},
		{ts + `level=INFO msg="Authentication successful" user=a`, "auth"},
		{ts + `level=INFO msg="Streamer is online" streamer=a`, "stream"},
		{ts + `level=INFO msg="WebSocket connected" index=0`, "stream"},
		{ts + `level=INFO msg="Joined IRC chat" channel=a`, "stream"},
		{ts + `level=INFO msg="Reconnecting WebSocket" index=0`, "stream"},
		// Attribute-driven: the rule is constructed inline, not a table row.
		{ts + `level=INFO msg="Points earned" reason=WATCH points=1`, "points"},
		{ts + `level=INFO msg="Points earned" reason=WATCH_STREAK points=450`, "points"},
		{ts + `level=INFO msg="Prediction result" result=WIN points=5`, "predictions"},
		{ts + `level=INFO msg="Prediction result" result=LOSE points=5`, "predictions"},
		{ts + `level=INFO msg="Claiming bonus" streamer=a`, "points"},
		{ts + `level=INFO msg="Joining raid" streamer=a`, "points"},
		{ts + `level=INFO msg="Contributed to community goal" points=100`, "points"},
		{ts + `level=INFO msg="Placing prediction bet" streamer=a`, "predictions"},
		{ts + `level=INFO msg="Skipping bet" reason=x`, "predictions"},
		{ts + `level=INFO msg="Auto-bet gated" streamer=a`, "predictions"},
		{ts + `level=INFO msg="Watch slot assigned" streamer=a`, "watch"},
		{ts + `level=INFO msg="Rotating watch pair"`, "watch"},
		{ts + `level=INFO msg="Claiming drop" drop=x`, "drops"},
		{ts + `level=INFO msg="Claimed drop" drop=x`, "drops"},
		{ts + `level=INFO msg="Discovered channel selected" channel=a`, "drops"},
		{ts + `level=INFO msg="Auto-update: newer release available" version=1`, "updater"},
		{ts + `level=INFO msg="Auto-update: binary replaced successfully"`, "updater"},
		{ts + `level=INFO msg="Settings saved to config file"`, "system"},
		{ts + `level=INFO msg="Connection restored"`, "system"},
		{ts + `level=INFO msg="Pruned old analytics history" rows=1`, "system"},
	}
	for _, tc := range cases {
		got := classifyLogLine(tc.line)
		if got.Subsystem != tc.subsystem {
			t.Errorf("Subsystem = %q, want %q for %q", got.Subsystem, tc.subsystem, tc.line)
		}
		if !got.DashboardVisible {
			t.Errorf("DashboardVisible = false for meaningful event %q", tc.line)
		}
	}
}

// TestReconnectIdentityIsOrthogonal pins the reconnect flag end to end. The
// filter used to key off the CSS class, which the severity switch now
// overwrites — so without an explicit flag a reconnect WARN would silently
// drop out of the "show reconnect events" toggle.
func TestReconnectIdentityIsOrthogonal(t *testing.T) {
	const ts = "time=2026-07-14T10:00:00.000+03:00 "

	cases := []struct {
		line      string
		reconnect bool
		level     string
	}{
		{ts + `level=INFO msg="Reconnecting WebSocket" index=0`, true, "info"},
		{ts + `level=INFO msg="Reconnected to IRC chat" channel=a`, true, "info"},
		{ts + `level=INFO msg="WebSocket reconnect requested" index=1`, true, "info"},
		// Severity must not erase reconnect identity.
		{ts + `level=WARN msg="Reconnecting WebSocket" index=0`, true, "warning"},
		{ts + `level=ERROR msg="Reconnecting WebSocket" index=0`, true, "error"},
		// Non-reconnect lines must not acquire it.
		{ts + `level=INFO msg="Streamer is online" streamer=a`, false, "info"},
		{ts + `level=INFO msg="WebSocket connected" index=0`, false, "info"},
		{ts + `level=WARN msg="something unclassified"`, false, "warning"},
	}
	for _, tc := range cases {
		got := classifyLogLine(tc.line)
		if got.Reconnect != tc.reconnect {
			t.Errorf("Reconnect = %v, want %v for %q", got.Reconnect, tc.reconnect, tc.line)
		}
		if got.Level != tc.level {
			t.Errorf("Level = %q, want %q for %q", got.Level, tc.level, tc.line)
		}
	}

	// ...and the partial must actually emit it, only when set.
	on := renderLogsLinesForTest(t, []LogLineView{{
		Class: "log-reconnect", Emoji: "🔄", Text: "reconnect line",
		Level: "info", Subsystem: "stream", Reconnect: true, DashboardVisible: true,
	}})
	if !strings.Contains(on, `data-reconnect="1"`) {
		t.Errorf("reconnect line did not render data-reconnect=\"1\":\n%s", on)
	}
	off := renderLogsLinesForTest(t, []LogLineView{{
		Class: "log-connected", Emoji: "🔌", Text: "ordinary line",
		Level: "info", Subsystem: "stream", DashboardVisible: true,
	}})
	if strings.Contains(off, "data-reconnect") {
		t.Errorf("non-reconnect line rendered a data-reconnect attribute:\n%s", off)
	}
}

// ---------------------------------------------------------------------
// Rendered page: the browser must consume server metadata, not CSS classes
// ---------------------------------------------------------------------

// TestLogsPartialCarriesIndependentMetadata asserts the rendered partial
// exposes level and subsystem as their own attributes. The old page derived
// BOTH from the single CSS class, which is why a WARN could never also be a
// Drops line.
func TestLogsPartialCarriesIndependentMetadata(t *testing.T) {
	out := renderLogsLinesForTest(t, []LogLineView{
		{
			Class: "log-warn", Emoji: "⚠️", Text: `level=WARN msg="Drops sync failed"`,
			Level: "warning", Subsystem: "drops", DashboardVisible: true,
		},
	})

	for _, want := range []string{
		`data-level="warning"`,
		`data-subsystem="drops"`,
		`class="log-line log-warn"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered log line is missing %q\ngot: %s", want, out)
		}
	}
}

// TestLogsPartialOmitsSuppressedLines pins suppression at the render seam:
// a line the classifier marked invisible is not in the DOM at all, so it
// cannot be counted, searched, or copied.
func TestLogsPartialOmitsSuppressedLines(t *testing.T) {
	out := renderLogsLinesForTest(t, []LogLineView{
		{Class: "log-info", Emoji: "ℹ️", Text: "RAW-CHATTER-MARKER", Level: "info", Subsystem: "other", DashboardVisible: false},
		{Class: "log-points-watch", Emoji: "👀", Text: "MEANINGFUL-MARKER", Level: "info", Subsystem: "points", DashboardVisible: true},
	})

	if strings.Contains(out, "RAW-CHATTER-MARKER") {
		t.Error("a suppressed line was rendered into the dashboard Logs list")
	}
	if !strings.Contains(out, "MEANINGFUL-MARKER") {
		t.Error("a dashboard-visible line was not rendered")
	}
}

// TestLogsControllerUsesServerMetadataNotClassTables asserts the browser
// filter reads the server's authoritative attributes and no longer carries a
// second, independent taxonomy of its own.
func TestLogsControllerUsesServerMetadataNotClassTables(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/logs", "en")

	for _, banned := range []string{
		"const SUBSYS = {",
		"function subsystemOf(",
		"function levelOf(",
		"function semanticClass(",
		"if (cls === 'log-error') return 'error';",
		"if (cls === 'log-warn') return 'warning';",
	} {
		if strings.Contains(body, banned) {
			t.Errorf("logs page still carries the browser-side taxonomy %q — the server is the single authority for level/subsystem", banned)
		}
	}

	for _, want := range []string{"dataset.level", "dataset.subsystem", "dataset.reconnect"} {
		if !strings.Contains(body, want) {
			t.Errorf("logs page filter does not read the server metadata %q", want)
		}
	}
}

// ---------------------------------------------------------------------
// Copy visible
// ---------------------------------------------------------------------

// TestLogsCopyHasNonSecureContextFallback is the root-cause guard for the
// reported failure. The dashboard is normally reached over plain LAN HTTP,
// which is not a secure context, so navigator.clipboard is frequently absent
// or rejects — and the old controller had no second path, so the button
// could only ever report the localized "Copy failed". A fallback that works
// under user activation on plain HTTP must exist.
func TestLogsCopyHasNonSecureContextFallback(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/logs", "en")

	for _, want := range []string{
		// The preferred path is still the async Clipboard API...
		"navigator.clipboard",
		// ...but it must be guarded rather than assumed present.
		"navigator.clipboard && navigator.clipboard.writeText",
		// ...and a real fallback must exist for plain HTTP.
		"document.execCommand('copy')",
		"createElement('textarea')",
		"setSelectionRange",
		// ...which must clean up after itself in every path.
		"removeChild",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("logs copy control is missing the fallback literal %q", want)
		}
	}

	// Existing is not enough: the fallback must actually be REACHED when the
	// Clipboard API is missing or rejected. A defined-but-unwired fallback
	// leaves the LAN dashboard exactly as broken as it was.
	if !strings.Contains(body, "function copyViaTextarea(text) {") {
		t.Error("logs copy control does not define copyViaTextarea")
	}
	if !strings.Contains(body, "if (!copied) copied = copyViaTextarea(text);") {
		t.Error("logs copy control never calls copyViaTextarea — the fallback is unreachable, so a non-secure context can still only report failure")
	}
	// Failure is reported only after BOTH mechanisms have been tried.
	clipboardAt := strings.Index(body, "await navigator.clipboard.writeText(text)")
	fallbackAt := strings.Index(body, "if (!copied) copied = copyViaTextarea(text);")
	failAt := strings.Index(body, "window.t('js.logs.copy_failed')")
	if clipboardAt < 0 || fallbackAt < 0 || failAt < 0 || clipboardAt >= fallbackAt || fallbackAt >= failAt {
		t.Error("copy must try the Clipboard API, then the fallback, and only then report failure")
	}
}

// TestLogsCopyCollectsOnlyVisibleText pins what the copy payload is built
// from: the text spans of the log lines the filters left visible — never the
// decorative emoji, the toolbar, the line count, or the status text.
func TestLogsCopyCollectsOnlyVisibleText(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/logs", "en")

	if !strings.Contains(body, `'.log-line:not([hidden]) .log-text'`) {
		t.Error("copy payload must be collected from '.log-line:not([hidden]) .log-text'")
	}
	// The emoji lives in its own span and must never be part of the payload.
	if strings.Contains(body, ".log-emoji').textContent") || strings.Contains(body, `'.log-line:not([hidden])'`) {
		t.Error("copy payload must not collect whole log lines or emoji spans")
	}
	// The payload is built from textContent of the text spans only: never the
	// count/status text, and never markup. ("innerHTML" is deliberately not
	// banned page-wide — hx-swap="innerHTML" is htmx's swap strategy for the
	// log list and has nothing to do with the copy payload.)
	for _, banned := range []string{"logs-count').textContent", "copyStatus.innerHTML", ".log-line').innerHTML"} {
		if strings.Contains(body, banned) {
			t.Errorf("copy payload must not include %q", banned)
		}
	}
}

// TestLogsCopyKeepsLocalizedStatuses guards the accessible status region:
// both outcomes stay localized, and the fallback reports failure only when
// BOTH mechanisms failed.
func TestLogsCopyKeepsLocalizedStatuses(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/logs", "en")

	for _, want := range []string{
		"window.t('js.logs.copy_ok')",
		"window.t('js.logs.copy_failed')",
		// Both outcome branches exist and are distinguishable.
		"if (copied) {",
		// The single owned auto-clear timer must survive the rewrite.
		"let copyStatusTimer = null;",
		"if (copyStatusTimer) clearTimeout(copyStatusTimer);",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("logs copy control is missing %q", want)
		}
	}
}

// ---------------------------------------------------------------------
// End-to-end: retained file -> readLogTail -> rendered partial
// ---------------------------------------------------------------------

// TestDashboardPipelineSuppressesChatterEndToEnd is the whole-seam guard. It
// writes a retained log file mixing real raw-transport DEBUG chatter with
// real meaningful events, drives the actual readLogTail + logs_lines render
// the page and the /api/logs htmx refresh both use, and asserts the rendered
// list holds the signal and none of the noise.
//
// File logging defaults to DEBUG (internal/config/config.go: FileLevel
// "DEBUG") while the console defaults to INFO, so every one of these DEBUG
// lines really does reach the file the dashboard reads.
func TestDashboardPipelineSuppressesChatterEndToEnd(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll("logs", 0o755); err != nil {
		t.Fatal(err)
	}

	const ts = "time=2026-07-14T10:00:00.000+03:00 "
	noise := []string{
		ts + `level=DEBUG msg="GQL response" operation=ChannelPointsContext status=200`,
		ts + `level=DEBUG msg="WebSocket send" index=0 type=PING`,
		ts + `level=DEBUG msg="WebSocket received" index=0 type=PONG`,
		ts + `level=DEBUG msg="IRC message received" channel=xqc line="PING :tmi.twitch.tv"`,
		ts + `level=DEBUG msg="Chat logging config" enableChatLogs=false`,
		ts + `level=DEBUG msg="Discord notification sent" channel=general`,
		ts + `level=DEBUG msg="Drops sync: campaign counts through the pipeline" afterInventory=4`,
	}
	signal := []string{
		ts + `level=INFO msg="Points earned" streamer=xqc points=10 reason=WATCH balance=1000`,
		ts + `level=INFO msg="Streamer is online" streamer=shroud`,
		ts + `level=INFO msg="Reconnecting WebSocket" index=0`,
		ts + `level=DEBUG msg="Sent minute watched" streamer=xqc origin=slot minutesWatched=12`,
		ts + `level=WARN msg="Drops sync failed: could not fetch active drop campaigns from Twitch; keeping previously tracked campaigns" error=null`,
		ts + `level=ERROR msg="a brand new failure mode" error=boom`,
	}

	var b strings.Builder
	for _, l := range append(append([]string{}, noise...), signal...) {
		b.WriteString(l)
		b.WriteString("\n")
	}
	if err := os.WriteFile(filepath.Join("logs", "e2e.log"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &Server{username: "e2e"}
	views, enabled := s.readLogTail()
	if !enabled {
		t.Fatal("readLogTail reported the retained file as disabled")
	}

	// readLogTail itself stays a faithful reader of the retained tail: it
	// classifies, it does not censor. Suppression is a presentation decision
	// applied at the render seam, so the raw evidence keeps flowing to every
	// other consumer of this function.
	if len(views) != len(noise)+len(signal) {
		t.Fatalf("readLogTail returned %d views, want %d — the retained tail must not be truncated by classification", len(views), len(noise)+len(signal))
	}

	out := renderLogsLinesForTest(t, views)
	for _, n := range []string{"GQL response", "WebSocket send", "WebSocket received", "IRC message received", "Chat logging config", "Discord notification sent", "campaign counts through the pipeline"} {
		if strings.Contains(out, n) {
			t.Errorf("raw implementation chatter %q reached the dashboard Logs list", n)
		}
	}
	for _, s := range []string{"Points earned", "Streamer is online", "Reconnecting WebSocket", "Sent minute watched", "Drops sync failed", "a brand new failure mode"} {
		if !strings.Contains(out, s) {
			t.Errorf("meaningful event %q is missing from the dashboard Logs list", s)
		}
	}

	// The classification readLogTail produced must reach the DOM as the
	// authoritative filter metadata. This is the seam the whole change turns
	// on: without it the browser has nothing to filter by.
	for _, want := range []string{
		`data-level="warning" data-subsystem="drops"`,
		`data-level="error" data-subsystem="other"`,
		`data-level="info" data-subsystem="watch"`,
		`data-level="info" data-subsystem="points"`,
		`data-level="info" data-subsystem="stream"`,
		// The fourth wired dimension: reconnect identity must survive the
		// readLogTail -> LogLineView -> partial hop like the other three.
		`data-subsystem="stream" data-reconnect="1"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("readLogTail -> rendered page did not carry %s\n%s", want, out)
		}
	}
}

// TestAPILogsPartialAppliesVisibility covers the htmx refresh endpoint itself.
// Every ten seconds the page replaces its list from /api/logs, so that handler
// — not just the template — must apply the same classification and visibility
// policy as the first paint. Without this the two paths could disagree and the
// first refresh would silently change what the page shows.
func TestAPILogsPartialAppliesVisibility(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll("logs", 0o755); err != nil {
		t.Fatal(err)
	}
	const ts = "time=2026-07-14T10:00:00.000+03:00 "
	content := strings.Join([]string{
		ts + `level=DEBUG msg="GQL response" operation=Op status=200 marker=APILOGS-CHATTER`,
		ts + `level=INFO msg="Points earned" streamer=xqc points=10 reason=WATCH marker=APILOGS-SIGNAL`,
		ts + `level=INFO msg="Reconnecting WebSocket" index=0 marker=APILOGS-RECONNECT`,
		ts + `level=WARN msg="Drops sync failed: could not fetch active drop campaigns from Twitch; keeping previously tracked campaigns" marker=APILOGS-WARN`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join("logs", "apilogs.log"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := newRenderServer(t)
	srv.username = "apilogs"

	rec := httptest.NewRecorder()
	srv.handleAPILogs(rec, httptest.NewRequest(http.MethodGet, "/api/logs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/logs = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	if strings.Contains(body, "APILOGS-CHATTER") {
		t.Error("the htmx refresh re-introduced suppressed chatter")
	}
	for _, want := range []string{
		"APILOGS-SIGNAL", "APILOGS-RECONNECT", "APILOGS-WARN",
		`data-level="info" data-subsystem="points"`,
		`data-subsystem="stream" data-reconnect="1"`,
		`data-level="warning" data-subsystem="drops"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/api/logs response is missing %q\n%s", want, body)
		}
	}
}

// TestDropCampaignProgressIsThrottledNotLost pins the one event the miner
// builds at runtime instead of from a literal. internal/drops logs the SAME
// progress line at INFO when it crosses a 5%% checkpoint (including reaching
// 100%% / claim-ready) and at DEBUG on every routine cycle in between. The
// checkpoint is real operator signal and stays; the routine repeat does not.
func TestDropCampaignProgressIsThrottledNotLost(t *testing.T) {
	const ts = "time=2026-07-14T10:00:00.000+03:00 "
	const progress = `World of Tanks [cyganzor] AMD Summer Arena Drops#2: -----------> 55%`

	info := classifyLogLine(ts + `level=INFO msg=` + strconv.Quote(progress))
	if !info.DashboardVisible {
		t.Error("the throttled INFO drop-progress checkpoint must stay on the page")
	}
	if info.Subsystem != "drops" {
		t.Errorf("Subsystem = %q, want %q for the drop-progress line", info.Subsystem, "drops")
	}

	debug := classifyLogLine(ts + `level=DEBUG msg=` + strconv.Quote(progress))
	if debug.DashboardVisible {
		t.Error("the routine DEBUG drop-progress repeat must not reach the page")
	}
	if debug.Subsystem != "drops" {
		t.Errorf("Subsystem = %q, want %q even when suppressed", debug.Subsystem, "drops")
	}

	// Severity always wins over the throttle: a progress line logged at
	// WARN/ERROR is never suppressed by it.
	for _, lvl := range []string{"WARN", "ERROR"} {
		got := classifyLogLine(ts + `level=` + lvl + ` msg=` + strconv.Quote(progress))
		if !got.DashboardVisible {
			t.Errorf("a %s drop-progress line must never be suppressed", lvl)
		}
	}
}

// TestLogsPageFirstPaintAppliesVisibility covers the OTHER render path. The
// full page renders LogsPageData through logs.html; the 10s htmx refresh
// renders LogsLinesData through the partial alone. Both must apply the same
// visibility policy, or the first paint would show the raw chatter that the
// first refresh then silently removes.
func TestLogsPageFirstPaintAppliesVisibility(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll("logs", 0o755); err != nil {
		t.Fatal(err)
	}
	const ts = "time=2026-07-14T10:00:00.000+03:00 "
	content := strings.Join([]string{
		ts + `level=DEBUG msg="GQL response" operation=Op status=200 marker=FIRSTPAINT-CHATTER`,
		ts + `level=INFO msg="Points earned" streamer=xqc points=10 reason=WATCH marker=FIRSTPAINT-SIGNAL`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join("logs", "firstpaint.log"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := newRenderServer(t)
	srv.username = "firstpaint"

	req := httptest.NewRequest(http.MethodGet, "/logs", nil)
	rec := httptest.NewRecorder()
	srv.handleLogsPage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /logs = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	if strings.Contains(body, "FIRSTPAINT-CHATTER") {
		t.Error("the full-page first paint rendered suppressed chatter (the htmx refresh would then remove it)")
	}
	if !strings.Contains(body, "FIRSTPAINT-SIGNAL") {
		t.Error("the full-page first paint did not render a meaningful event")
	}
	if !strings.Contains(body, `data-subsystem="points"`) {
		t.Error("the full-page first paint did not carry the subsystem metadata")
	}
}

// TestLogsEmptyStateWhenEverythingSuppressed pins the state the page shows
// when file logging IS on and lines exist, but none of them is dashboard
// material. It must be the localized empty state, not a blank card.
func TestLogsEmptyStateWhenEverythingSuppressed(t *testing.T) {
	suppressed := []LogLineView{
		{Class: "log-info", Emoji: "ℹ️", Text: "chatter one", Level: "info", Subsystem: "stream", DashboardVisible: false},
		{Class: "log-info", Emoji: "ℹ️", Text: "chatter two", Level: "info", Subsystem: "stream", DashboardVisible: false},
	}
	for _, lang := range []string{i18n.LangEN, i18n.LangRU} {
		out := renderLogsLinesLangForTest(t, lang, suppressed)
		if strings.Contains(out, "chatter one") || strings.Contains(out, "chatter two") {
			t.Errorf("[%s] suppressed lines were rendered", lang)
		}
		want := trFor(t, lang, "logs.empty")
		if !strings.Contains(out, want) {
			t.Errorf("[%s] an all-suppressed tail must render the localized empty state %q, got:\n%s", lang, want, out)
		}
		if strings.Contains(out, "log-line") {
			t.Errorf("[%s] an all-suppressed tail must render no log lines, got:\n%s", lang, out)
		}
	}
}

// trFor looks up one translation in an explicit language.
func trFor(t *testing.T, lang, key string) string {
	t.Helper()
	loc, err := i18n.New()
	if err != nil {
		t.Fatalf("i18n.New: %v", err)
	}
	return loc.T(lang, key)
}
