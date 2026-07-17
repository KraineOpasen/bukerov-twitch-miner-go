package web

import (
	"strings"
	"testing"
)

// classifyCase is one table entry: a realistic on-disk slog line and the
// class/emoji the Logs page must assign it.
type classifyCase struct {
	name  string
	line  string
	class string
	emoji string
}

// TestClassifyLogLineTable drives the classifier over the miner's real log
// messages, covering every mandatory event → class → emoji mapping plus the
// strict ERROR > WARN > event > INFO priority.
func TestClassifyLogLineTable(t *testing.T) {
	const ts = "time=2026-07-14T10:00:00.000+03:00 "

	cases := []classifyCase{
		// Levels and fallbacks.
		{"ordinary INFO", ts + `level=INFO msg="Streamer stats" streamer=xqc`, "log-info", "ℹ️"},
		{"ordinary DEBUG", ts + `level=DEBUG msg="Watching streams" count=2`, "log-info", "ℹ️"},
		{"ordinary WARN", ts + `level=WARN msg="GQL request failed, retrying" attempt=2`, "log-warn", "⚠️"},
		{"ordinary ERROR", ts + `level=ERROR msg="Failed to join raid" error=boom`, "log-error", "❌"},
		{"unknown line without level", `some free-form line`, "log-info", "✨"},

		// Startup / service / auth.
		{"startup banner", ts + `level=INFO msg="Twitch Channel Points Miner" version=1.0.0`, "log-startup", "🚀"},
		{"initializing miner", ts + `level=INFO msg="Initializing Twitch Channel Points Miner"`, "log-startup", "🚀"},
		{"starting mining", ts + `level=INFO msg="Starting mining operations"`, "log-startup", "🚀"},
		{"authenticating", ts + `level=INFO msg="Authenticating with Twitch"`, "log-auth", "🔐"},
		{"auth successful", ts + `level=INFO msg="Authentication successful" username=bukerov`, "log-auth-success", "🔐"},
		{"loading streamers", ts + `level=INFO msg="Loading streamers" count=5`, "log-service", "🔧"},
		{"loaded streamer", ts + `level=INFO msg="Loaded streamer" username=xqc`, "log-service", "🔧"},
		{"subscribing pubsub", ts + `level=INFO msg="Subscribing to PubSub topics" topics=12`, "log-service", "🔧"},
		{"web server starting", ts + `level=INFO msg="Web server starting" url=http://127.0.0.1:5000/`, "log-service", "🔧"},
		{"web server bind", ts + `level=INFO msg="Web server bind resolved" host=127.0.0.1`, "log-service", "🔧"},

		// Streamer / connections.
		{"streamer online", ts + `level=INFO msg="Streamer is online" streamer=shroud`, "log-streamer-online", "🟢"},
		{"streamer offline", ts + `level=INFO msg="Streamer went offline" streamer=shroud`, "log-streamer-offline", "🔴"},
		{"websocket connected", ts + `level=INFO msg="WebSocket connected" index=0`, "log-connected", "🔌"},
		{"discord connected", ts + `level=INFO msg="Discord notification provider connected"`, "log-connected", "🤖"},
		{"joined irc", ts + `level=INFO msg="Joined IRC chat" channel=shroud`, "log-chat", "💬"},
		{"discord reconfigured", ts + `level=INFO msg="Discord configuration updated and reconnected"`, "log-reconnect", "🔄"},
		{"websocket reconnecting", ts + `level=INFO msg="Reconnecting WebSocket" index=0`, "log-reconnect", "🔄"},
		{"websocket reconnect requested", ts + `level=INFO msg="WebSocket reconnect requested" index=1`, "log-reconnect", "🔄"},
		{"irc reconnected", ts + `level=INFO msg="Reconnected to IRC chat" channel=xqc`, "log-reconnect", "🔄"},

		// Points earned by reason.
		{"points WATCH", ts + `level=INFO msg="Points earned" streamer=xqc points=10 reason=WATCH balance=1000`, "log-points-watch", "👀"},
		{"points WATCH_STREAK", ts + `level=INFO msg="Points earned" streamer=xqc points=450 reason=WATCH_STREAK balance=1450`, "log-points-streak", "🔥"},
		{"points CLAIM", ts + `level=INFO msg="Points earned" streamer=xqc points=50 reason=CLAIM balance=1500`, "log-points-claim", "🎁"},
		{"points RAID", ts + `level=INFO msg="Points earned" streamer=xqc points=250 reason=RAID balance=1750`, "log-points-raid", "🚀"},
		{"points other reason", ts + `level=INFO msg="Points earned" streamer=xqc points=5 reason=PREDICTION balance=1755`, "log-points-gain", "💰"},
		{"points no reason", ts + `level=INFO msg="Points earned" streamer=xqc points=5`, "log-points-gain", "💰"},
		{"claiming bonus", ts + `level=INFO msg="Claiming bonus" streamer=xqc`, "log-bonus", "🎉"},
		{"bonus claimed via fallback", ts + `level=INFO msg="Claimed channel points bonus via GQL fallback poll (PubSub missed the claim-available event)" streamer=xqc`, "log-bonus", "🎉"},

		// Predictions.
		{"prediction WIN", ts + `level=INFO msg="Prediction result" event="Will they win?" result=WIN gained=500`, "log-bet-win", "🏆"},
		{"prediction LOSE", ts + `level=INFO msg="Prediction result" event="Close game" result=LOSE gained=-200`, "log-bet-lose", "💥"},
		{"prediction REFUND", ts + `level=INFO msg="Prediction result" event="Cancelled" result=REFUND gained=0`, "log-bet-refund", "↩️"},
		{"prediction filtered", ts + `level=INFO msg="Skipping bet" streamer=xqc reason="filter condition"`, "log-bet-filter", "🧲"},
		{"bet amount too low", ts + `level=INFO msg="Bet amount too low" amount=3`, "log-bet-filter", "🧲"},
		{"placing prediction", ts + `level=INFO msg="Placing prediction bet" outcome=YES amount=500`, "log-bet-general", "🔮"},
		{"prediction confirmed", ts + `level=INFO msg="Prediction confirmed" event="Will they win?"`, "log-bet-general", "🔮"},
		{"prediction scheduled", ts + `level=INFO msg="Prediction event scheduled" event="Will they win?"`, "log-bet-general", "🔮"},
		{"prediction failed INFO", ts + `level=INFO msg="Not enough points for prediction" needed=100 have=20`, "log-bet-failed", "🚫"},
		{"auto-bet gated INFO", ts + `level=INFO msg="Auto-bet gated" reason=health`, "log-bet-gated", "🛡️"},

		// Watch lifecycle.
		{"watch slot assigned", ts + `level=INFO msg="Watch slot assigned" slot=1 channel=xqc`, "log-watch-assigned", "🎯"},
		{"watch slot released", ts + `level=INFO msg="Watch slot released" slot=1 channel=xqc`, "log-watch-released", "📴"},
		{"watch slot reason changed", ts + `level=INFO msg="Watch slot reason changed" slot=1 reason=DROPS`, "log-watch-changed", "🔀"},
		{"rotating watch pair", ts + `level=INFO msg="Rotating watch pair" pair="[a b]"`, "log-rotation", "🔄"},
		{"pursuing streak", ts + `level=INFO msg="Pursuing watch streak" streamer=xqc`, "log-pursuing-streak", "🔥"},

		// Drops / discovery.
		{"drops sync complete", ts + `level=INFO msg="Drops sync complete: tracking active drop campaigns" count=3`, "log-drops", "🎁"},
		{"drop skipped blacklist", ts + `level=INFO msg="Skipping drop campaign: matched drop-name blacklist" campaign=X`, "log-drop-skipped", "🚫"},
		{"drop skipped claimed", ts + `level=INFO msg="Skipping drop campaign: already claimed" campaign=X`, "log-drop-skipped", "🚫"},
		{"drop claimed", ts + `level=INFO msg="Claimed drop" drop="Cool Skin"`, "log-drop-complete", "✅"},
		{"discovery pool empty", ts + `level=INFO msg="Discovery pool empty: no live drops-enabled channel available right now"`, "log-discovery", "🔎"},
		{"discovered channel", ts + `level=INFO msg="Discovered channel selected" channel=abc`, "log-discovery", "🔎"},

		// Auto-update.
		{"auto-update watcher", ts + `level=INFO msg="Auto-update watcher started" interval=6h`, "log-update", "🔄"},
		{"update available", ts + `level=INFO msg="Auto-update: newer release available" current=1.0 latest=1.1`, "log-update", "⬆️"},
		{"update downloading", ts + `level=INFO msg="Auto-update: downloading new binary" version=1.1`, "log-update", "⬆️"},
		{"update success", ts + `level=INFO msg="Auto-update: binary replaced successfully, restarting to load the new version"`, "log-update-success", "✅"},

		// Settings / health / database.
		{"settings saved", ts + `level=INFO msg="Settings saved to config file"`, "log-settings", "⚙️"},
		{"runtime settings", ts + `level=INFO msg="Runtime settings updated"`, "log-settings", "⚙️"},
		{"health recovered", ts + `level=INFO msg="Connection restored - harvesting resumed"`, "log-health-ok", "💚"},
		{"health stabilized", ts + `level=INFO msg="Connection stabilized"`, "log-health-ok", "💚"},
		{"analytics pruned", ts + `level=INFO msg="Pruned old analytics history" rows=100`, "log-database", "💾"},

		// Priority: ERROR/WARN always beat event categories.
		{"WARN beats points", ts + `level=WARN msg="Points earned" reason=WATCH points=10`, "log-warn", "⚠️"},
		{"ERROR beats rotation", ts + `level=ERROR msg="Rotating watch pair" error=x`, "log-error", "❌"},
		{"ERROR beats prediction WIN", ts + `level=ERROR msg="Prediction result" result=WIN`, "log-error", "❌"},
		{"WARN beats gated", ts + `level=WARN msg="Auto-bet gated" reason=risk`, "log-warn", "⚠️"},
		{"WARN beats offline", ts + `level=WARN msg="Streamer went offline" streamer=x`, "log-warn", "⚠️"},

		// Robustness: quoting, attribute order, substring poisoning.
		{"unquoted single-word msg", ts + `level=INFO msg=tick`, "log-info", "ℹ️"},
		{"reason after extra attrs", ts + `level=INFO msg="Points earned" balance=99 streamer=abc reason=CLAIM`, "log-points-claim", "🎁"},
		{"WATCH substring in username", ts + `level=INFO msg="Points earned" streamer=WATCH_STREAKfan reason=WATCH`, "log-points-watch", "👀"},
		{"win substring in username", ts + `level=INFO msg="Prediction result" event="winter arc" streamer=winner result=LOSE`, "log-bet-lose", "💥"},
		{"reason inside quoted msg ignored", ts + `level=INFO msg="user typed reason=WATCH in chat" streamer=x`, "log-info", "ℹ️"},
		{"result missing", ts + `level=INFO msg="Prediction result" event="odd"`, "log-bet-general", "🔮"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyLogLine(tc.line)
			if got.Class != tc.class {
				t.Errorf("class = %q, want %q (line %q)", got.Class, tc.class, tc.line)
			}
			if got.Emoji != tc.emoji {
				t.Errorf("emoji = %q, want %q (line %q)", got.Emoji, tc.emoji, tc.line)
			}
			if got.HasLeadingEmoji {
				t.Errorf("HasLeadingEmoji = true for emoji-less line %q", tc.line)
			}
		})
	}
}

// TestClassifyLogLineNeverGray guards the "no gray walls" rule: no line may
// come back with a neutral/gray class — every class is one of the bright
// semantic classes styled in logs.html.
func TestClassifyLogLineNeverGray(t *testing.T) {
	allowed := make(map[string]bool)
	for _, c := range allLogLineClasses() {
		allowed[c] = true
	}
	lines := []string{
		`time=x level=INFO msg="Streamer stats"`,
		`time=x level=INFO msg="Points earned" reason=WATCH`,
		`time=x level=DEBUG msg="tick"`,
		`garbage`,
	}
	for _, line := range lines {
		got := classifyLogLine(line)
		if !allowed[got.Class] {
			t.Errorf("class %q for %q is not a known semantic class", got.Class, line)
		}
		if strings.Contains(got.Class, "neutral") || strings.Contains(got.Class, "gray") || strings.Contains(got.Class, "muted") {
			t.Errorf("class %q for %q looks like a gray/neutral class", got.Class, line)
		}
	}
}

// TestClassifyLogLineLeadingEmoji covers the double-emoji guard: a line whose
// text already opens with an emoji keeps its class but gets no second icon,
// and the classification itself is not skewed by the prefix.
func TestClassifyLogLineLeadingEmoji(t *testing.T) {
	cases := []struct {
		name  string
		line  string
		class string
	}{
		{"green dot online", `🟢 time=x level=INFO msg="Streamer is online" streamer=a`, "log-streamer-online"},
		{"warning sign warn", `⚠️ time=x level=WARN msg="retrying" attempt=2`, "log-warn"},
		{"rocket startup", `🚀 time=x level=INFO msg="Twitch Channel Points Miner"`, "log-startup"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyLogLine(tc.line)
			if !got.HasLeadingEmoji {
				t.Fatalf("HasLeadingEmoji = false for %q", tc.line)
			}
			if got.Emoji != "" {
				t.Errorf("emoji = %q, want empty (no double icon) for %q", got.Emoji, tc.line)
			}
			if got.Class != tc.class {
				t.Errorf("class = %q, want %q — leading emoji must not skew classification", got.Class, tc.class)
			}
		})
	}
}

// TestEmojiCoverage guards that every classification path yields a non-empty
// emoji (so no line renders as a bare gray string) and that every rule in the
// message table carries both a class and an emoji.
func TestEmojiCoverage(t *testing.T) {
	for i, rule := range logMsgRules {
		if rule.class == "" {
			t.Errorf("logMsgRules[%d] has empty class", i)
		}
		if rule.emoji == "" {
			t.Errorf("logMsgRules[%d] (%s) has empty emoji", i, rule.class)
		}
		if len(rule.exact) == 0 && len(rule.prefix) == 0 {
			t.Errorf("logMsgRules[%d] (%s) matches nothing", i, rule.class)
		}
	}

	// Every displayed line must get an emoji unless the source text already
	// leads with one.
	lines := []string{
		`time=x level=INFO msg="Points earned" reason=WATCH`,
		`time=x level=INFO msg="Points earned" reason=WATCH_STREAK`,
		`time=x level=INFO msg="Prediction result" result=WIN`,
		`time=x level=WARN msg="careful"`,
		`time=x level=ERROR msg="boom"`,
		`time=x level=INFO msg="anything else"`,
		`no slog tokens at all`,
	}
	for _, line := range lines {
		got := classifyLogLine(line)
		if got.Emoji == "" && !got.HasLeadingEmoji {
			t.Errorf("line %q got empty emoji without a leading emoji of its own", line)
		}
	}
}

// TestLogCSSCoverage reads the embedded logs.html and asserts every semantic
// class the classifier can return is styled there, that the palette avoids
// gray for event colors, and that the emoji span has a stable width and no
// animation.
func TestLogCSSCoverage(t *testing.T) {
	raw, err := templatesFS.ReadFile("templates/logs.html")
	if err != nil {
		t.Fatalf("read logs.html: %v", err)
	}
	css := string(raw)

	for _, class := range allLogLineClasses() {
		if !strings.Contains(css, "."+class+" ") && !strings.Contains(css, "."+class+" {") {
			t.Errorf("class %q returned by the classifier is not styled in logs.html", class)
		}
	}

	for _, needle := range []string{".log-emoji", "width: 1.5em", "min-width: 1.5em", "aria-hidden"} {
		switch needle {
		case "aria-hidden":
			// aria-hidden lives in the partial, checked in the render test.
		default:
			if !strings.Contains(css, needle) {
				t.Errorf("logs.html missing %q", needle)
			}
		}
	}

	for _, banned := range []string{"animation", "blink", "@keyframes"} {
		if strings.Contains(css, banned) {
			t.Errorf("logs.html must not animate log lines, found %q", banned)
		}
	}

	// Event colors must come from the bright palette variables, not the gray
	// neutral scale.
	styleStart := strings.Index(css, "<style>")
	styleEnd := strings.Index(css, "</style>")
	if styleStart < 0 || styleEnd < 0 {
		t.Fatal("logs.html lost its <style> block")
	}
	style := css[styleStart:styleEnd]
	for _, banned := range []string{"--color-neutral-", "gray", "grey"} {
		if strings.Contains(style, banned) {
			t.Errorf("logs.html style block uses gray/neutral color %q for log lines", banned)
		}
	}
}

// TestHasLeadingEmoji pins the compact emoji detector on the icons the miner
// actually uses plus plain-text negatives.
func TestHasLeadingEmoji(t *testing.T) {
	for _, e := range []string{"🚀", "🟢", "🔴", "⚠️", "❌", "✅", "🔥", "🎁", "💰", "🏆", "💥", "↩️", "ℹ️", "✨", "⬆️", "⚙️", "😴", "🎮", "🚩", "🔄", "🖊️"} {
		if !hasLeadingEmoji(e + " rest of line") {
			t.Errorf("hasLeadingEmoji(%q...) = false, want true", e)
		}
	}
	for _, s := range []string{"", "time=x level=INFO", "plain text", "2026-07-14 log", "«кавычки»"} {
		if hasLeadingEmoji(s) {
			t.Errorf("hasLeadingEmoji(%q) = true, want false", s)
		}
	}
}

// TestLogAttr pins the quote-aware attribute extraction the classifier
// depends on.
func TestLogAttr(t *testing.T) {
	line := `time=2026-07-14T10:00:00Z level=INFO msg="Points earned" streamer=xqc reason=WATCH note="has reason=CLAIM inside"`
	cases := map[string]string{
		"level":    "INFO",
		"msg":      "Points earned",
		"streamer": "xqc",
		"reason":   "WATCH",
		"note":     "has reason=CLAIM inside",
		"absent":   "",
	}
	for key, want := range cases {
		if got := logAttr(line, key); got != want {
			t.Errorf("logAttr(%q) = %q, want %q", key, got, want)
		}
	}

	// A key inside a quoted value must never win over (or stand in for) a
	// real top-level attribute.
	poisoned := `time=x level=INFO msg="user typed result=WIN here" result=LOSE`
	if got := logAttr(poisoned, "result"); got != "LOSE" {
		t.Errorf("logAttr(result) = %q, want LOSE (quoted content must be skipped)", got)
	}

	// Leading decorative prefix before the first attribute.
	if got := logAttr(`🟢 time=x level=INFO msg=hi`, "level"); got != "INFO" {
		t.Errorf("logAttr(level) with emoji prefix = %q, want INFO", got)
	}
}
