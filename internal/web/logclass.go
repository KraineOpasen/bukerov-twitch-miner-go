package web

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// This file classifies plain-text slog lines for the built-in Logs page: each
// line gets exactly one semantic CSS class and one decorative emoji, assigned
// server-side from the untouched on-disk log line. The classification is
// deliberately independent of the Logger "Colored" setting — that toggle only
// governs ANSI decoration of the container's stdout (internal/logger), while
// the on-disk file the page reads stays plain text.

// LogPresentation is the visual classification of one plain-text log line.
// Class is a semantic CSS class (always one of the values styled in
// logs.html), Emoji a decorative Unicode emoji rendered in a separate
// aria-hidden span before the untouched line text. When the source line
// already starts with an emoji of its own, HasLeadingEmoji is true and Emoji
// is empty so the page never shows a doubled icon.
type LogPresentation struct {
	Class           string
	Emoji           string
	HasLeadingEmoji bool
}

// logMsgRule maps slog msg values (exact matches first, then prefixes) to a
// semantic class and emoji. Rules are evaluated in order; the first match
// wins, so more specific prefixes must precede broader ones (e.g. the
// "Auto-update: binary replaced successfully" rule before the bare
// "Auto-update" catch-all).
type logMsgRule struct {
	exact  []string
	prefix []string
	class  string
	emoji  string
}

// logMsgRules covers the miner's real INFO/DEBUG msg values. WARN and ERROR
// never reach this table — classifyLogLine resolves them by level first — and
// the two attribute-driven messages ("Points earned", "Prediction result")
// are handled separately in classifyLogLine before this table is consulted.
var logMsgRules = []logMsgRule{
	// Startup banner / lifecycle milestones.
	{exact: []string{
		"Twitch Channel Points Miner",
		"Initializing Twitch Channel Points Miner",
		"Starting mining operations",
	}, class: "log-startup", emoji: "🚀"},

	// Authentication.
	{exact: []string{"Authenticating with Twitch"}, class: "log-auth", emoji: "🔐"},
	{exact: []string{"Authentication successful"}, class: "log-auth-success", emoji: "🔐"},

	// Streamer online/offline transitions.
	{exact: []string{"Streamer is online"}, class: "log-streamer-online", emoji: "🟢"},
	{exact: []string{"Streamer went offline"}, class: "log-streamer-offline", emoji: "🔴"},

	// Connections.
	{exact: []string{"WebSocket connected"}, class: "log-connected", emoji: "🔌"},
	{exact: []string{"Discord notification provider connected"}, class: "log-connected", emoji: "🤖"},
	{exact: []string{"Joined IRC chat", "Chat mention"}, class: "log-chat", emoji: "💬"},
	{exact: []string{
		"Discord configuration updated and reconnected",
		"Reconnecting WebSocket",
		"WebSocket reconnect requested",
		"IRC reconnect requested by server",
		"Reconnected to IRC chat",
	}, class: "log-reconnect", emoji: "🔄"},

	// Predictions/bets. Outcomes (WIN/LOSE/REFUND) are attribute-driven and
	// classified in classifyLogLine; these are the fixed-message events.
	{exact: []string{"Skipping bet", "Bet amount too low"}, class: "log-bet-filter", emoji: "🧲"},
	{exact: []string{"Auto-bet gated"}, class: "log-bet-gated", emoji: "🛡️"},
	{exact: []string{"Not enough points for prediction"}, class: "log-bet-failed", emoji: "🚫"},
	{exact: []string{
		"Placing prediction bet",
		"Prediction confirmed",
		"Prediction event scheduled",
		"Manual prediction bet placed",
		"Manual bet placed via dashboard",
		"Duplicate prediction result ignored",
	}, class: "log-bet-general", emoji: "🔮"},

	// Point gains beyond the attribute-driven "Points earned".
	{exact: []string{"Claiming bonus", "Claiming moment"},
		prefix: []string{"Claimed channel points bonus"}, class: "log-bonus", emoji: "🎉"},
	{exact: []string{"Contributed to community goal"}, class: "log-points-gain", emoji: "💰"},
	{exact: []string{"Joining raid"}, class: "log-points-raid", emoji: "🚀"},

	// Watch-slot lifecycle.
	{exact: []string{"Watch slot assigned"}, class: "log-watch-assigned", emoji: "🎯"},
	{exact: []string{"Watch slot released"}, class: "log-watch-released", emoji: "📴"},
	{exact: []string{"Watch slot reason changed"}, class: "log-watch-changed", emoji: "🔀"},
	{exact: []string{"Rotating watch pair"}, class: "log-rotation", emoji: "🔄"},
	{exact: []string{"Pursuing watch streak"}, class: "log-pursuing-streak", emoji: "🔥"},

	// Drops and discovery.
	{exact: []string{
		"Claiming drop",
		"Claiming all drops from inventory on startup",
		"Channel-restricted drop campaign assigned to streamer",
	}, prefix: []string{"Drops sync complete"}, class: "log-drops", emoji: "🎁"},
	{exact: []string{"Skipping already-claimed reward within active drop campaign"},
		prefix: []string{"Skipping drop campaign"}, class: "log-drop-skipped", emoji: "🚫"},
	{exact: []string{"Claimed drop"}, class: "log-drop-complete", emoji: "✅"},
	{exact: []string{"Discovered channel selected", "Switching discovered channel"},
		prefix: []string{"Discovery pool empty"}, class: "log-discovery", emoji: "🔎"},

	// Auto-update. Specific milestones first, then the informational catch-all.
	{prefix: []string{"Auto-update: binary replaced successfully"}, class: "log-update-success", emoji: "✅"},
	{exact: []string{"Auto-update: newer release available", "Auto-update: downloading new binary"},
		class: "log-update", emoji: "⬆️"},
	{exact: []string{"Auto-update watcher started"}, prefix: []string{"Auto-update"},
		class: "log-update", emoji: "🔄"},

	// Settings / runtime config.
	{exact: []string{
		"Settings saved to config file",
		"Runtime settings updated",
		"Health settings updated",
		"Updated auto-redeem config",
	}, class: "log-settings", emoji: "⚙️"},

	// Health recovery.
	{exact: []string{"Connection restored - harvesting resumed", "Connection stabilized"},
		class: "log-health-ok", emoji: "💚"},

	// Database / analytics bookkeeping.
	{exact: []string{
		"Pruned old analytics history",
		"Migration column already present, skipping (self-heal)",
	}, class: "log-database", emoji: "💾"},

	// Ordinary service lifecycle.
	{exact: []string{
		"Loading streamers",
		"Loaded streamer",
		"Subscribing to PubSub topics",
		"Web server starting",
		"Web server bind resolved",
		"Web server authentication enabled",
		"Debug server listening (localhost only)",
		"Added new streamer",
		"Removed streamer",
		"Imported followed channels into the tracked list",
		"Discord notifications enabled",
		"Discord notifications disabled",
		"Push notification provider configured",
		"Daily summary enabled",
		"Shutting down...",
	}, class: "log-service", emoji: "🔧"},
}

// classifyLogLine assigns the semantic class and emoji for one plain-text log
// line. Priority is strict: ERROR, then WARN, then event categories, then
// plain INFO, then an unknown-line fallback. The function is deterministic,
// never mutates the line, and classifies from slog TextHandler tokens
// (level=..., msg=..., reason=..., result=...) parsed with quote awareness so
// attribute order, quoting, or a decorative leading emoji cannot skew it.
func classifyLogLine(line string) LogPresentation {
	p := LogPresentation{HasLeadingEmoji: hasLeadingEmoji(line)}
	p.Class, p.Emoji = classifyLine(line)
	if p.HasLeadingEmoji {
		// The source text already opens with its own emoji; suppress the
		// decorative one so the page never renders a doubled icon. The class
		// (color) still applies.
		p.Emoji = ""
	}
	return p
}

// classifyLine resolves the (class, emoji) pair, level first.
func classifyLine(line string) (string, string) {
	level := logAttr(line, "level")
	switch {
	case strings.HasPrefix(level, "ERROR"):
		return "log-error", "❌"
	case strings.HasPrefix(level, "WARN"):
		return "log-warn", "⚠️"
	}

	msg := logAttr(line, "msg")

	// Attribute-driven events: the msg alone doesn't identify the category.
	switch msg {
	case "Points earned":
		return pointsEarnedClass(line)
	case "Prediction result":
		return predictionResultClass(line)
	}

	for _, rule := range logMsgRules {
		for _, exact := range rule.exact {
			if msg == exact {
				return rule.class, rule.emoji
			}
		}
		for _, prefix := range rule.prefix {
			if strings.HasPrefix(msg, prefix) {
				return rule.class, rule.emoji
			}
		}
	}

	if level == "" {
		// Unknown line shape (no slog level token): still give it a visible
		// marker rather than a bare neutral wall of text.
		return "log-info", "✨"
	}
	return "log-info", "ℹ️"
}

// pointsEarnedClass varies "Points earned" by its reason attribute. The
// WATCH_STREAK case is checked before WATCH so the streak can never be
// swallowed by its WATCH substring.
func pointsEarnedClass(line string) (string, string) {
	switch logAttr(line, "reason") {
	case "WATCH_STREAK":
		return "log-points-streak", "🔥"
	case "WATCH":
		return "log-points-watch", "👀"
	case "CLAIM":
		return "log-points-claim", "🎁"
	case "RAID":
		return "log-points-raid", "🚀"
	default:
		return "log-points-gain", "💰"
	}
}

// predictionResultClass varies "Prediction result" by its result attribute.
// Only the dedicated result attribute is consulted — a WIN substring in a
// title or username can never trigger the win styling.
func predictionResultClass(line string) (string, string) {
	switch logAttr(line, "result") {
	case "WIN":
		return "log-bet-win", "🏆"
	case "LOSE":
		return "log-bet-lose", "💥"
	case "REFUND":
		return "log-bet-refund", "↩️"
	default:
		return "log-bet-general", "🔮"
	}
}

// allLogLineClasses returns every semantic class classifyLogLine can emit.
// The CSS coverage test walks this list to guarantee each class is styled in
// logs.html, so a classifier rule can never reference a class that renders
// unstyled.
func allLogLineClasses() []string {
	seen := make(map[string]bool)
	var out []string
	add := func(c string) {
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	// Level classes and fallback.
	add("log-error")
	add("log-warn")
	add("log-info")
	// Attribute-driven "Points earned" / "Prediction result" classes.
	add("log-points-streak")
	add("log-points-watch")
	add("log-points-claim")
	add("log-points-raid")
	add("log-points-gain")
	add("log-bet-win")
	add("log-bet-lose")
	add("log-bet-refund")
	add("log-bet-general")
	for _, rule := range logMsgRules {
		add(rule.class)
	}
	return out
}

// hasLeadingEmoji reports whether the line's first rune sits in one of the
// Unicode blocks the miner's emoji come from (Miscellaneous Symbols,
// Dingbats, arrows/ℹ, and the SMP emoji planes). Deliberately compact: it is
// a duplicate-icon guard for decorated lines, not a general emoji parser.
func hasLeadingEmoji(line string) bool {
	r, size := utf8.DecodeRuneInString(line)
	if size == 0 {
		return false
	}
	switch {
	case r >= 0x1F000 && r <= 0x1FAFF: // emoticons, pictographs, symbols (🚀🟢🔥…)
		return true
	case r >= 0x2600 && r <= 0x27BF: // misc symbols & dingbats (⚠✅❌✨⚙…)
		return true
	case r >= 0x2B00 && r <= 0x2BFF: // arrows/stars (⬆⭐)
		return true
	case r == 0x2139, r == 0x21A9, r == 0x21AA, r == 0xFE0F: // ℹ ↩ ↪ + variation selector
		return true
	}
	return false
}

// logAttr returns the value of the first top-level key=value attribute in a
// slog TextHandler line, or "" when absent. It tokenizes with quote awareness
// so a key mentioned inside a quoted value (e.g. reason=WATCH inside a quoted
// msg) can never masquerade as the real attribute, and it tolerates a
// decorative prefix before the first attribute.
func logAttr(line, key string) string {
	rest := line
	for rest != "" {
		rest = strings.TrimLeft(rest, " ")
		if rest == "" {
			break
		}
		eq := strings.IndexByte(rest, '=')
		if eq < 0 {
			break
		}
		if sp := strings.LastIndexByte(rest[:eq], ' '); sp >= 0 {
			// Tokens without '=' precede the next attribute (e.g. a decorative
			// leading emoji); skip them and restart at the token holding '='.
			rest = rest[sp+1:]
			continue
		}
		k := rest[:eq]
		rest = rest[eq+1:]
		var v string
		v, rest = readLogValue(rest)
		if k == key {
			return v
		}
	}
	return ""
}

// readLogValue consumes one attribute value (quoted or bare) from the front
// of rest and returns the decoded value plus the remainder of the line.
func readLogValue(rest string) (string, string) {
	if !strings.HasPrefix(rest, `"`) {
		if sp := strings.IndexByte(rest, ' '); sp >= 0 {
			return rest[:sp], rest[sp+1:]
		}
		return rest, ""
	}
	// Quoted value: find the closing quote, honoring backslash escapes.
	for i := 1; i < len(rest); i++ {
		switch rest[i] {
		case '\\':
			i++
		case '"':
			token := rest[:i+1]
			if unquoted, err := strconv.Unquote(token); err == nil {
				return unquoted, rest[i+1:]
			}
			return token, rest[i+1:]
		}
	}
	// Unterminated quote: treat the rest as the value.
	return rest, ""
}
