package web

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// This file classifies plain-text slog lines for the built-in Logs page. Each
// line gets four INDEPENDENT pieces of presentation metadata, all assigned
// server-side from the untouched on-disk log line:
//
//	Class/Emoji      — how the line looks (severity colour wins over category)
//	Level            — the severity filter dimension
//	Subsystem        — the semantic filter dimension
//	DashboardVisible — whether the human Logs page lists the line at all
//
// Severity and subsystem are deliberately orthogonal. The previous design
// derived both from one CSS class, so every WARN and every ERROR collapsed
// into the "other" subsystem bucket — precisely the lines an operator most
// wants to filter by subsystem. Level now decides the colour; the msg decides
// the bucket; neither erases the other.
//
// The classification is independent of the Logger "Colored" setting — that
// toggle only governs ANSI decoration of the container's stdout
// (internal/logger), while the on-disk file the page reads stays plain text.
//
// Nothing here touches the retained log itself. The file, the logger levels,
// the debug snapshot and the support bundle all keep the complete record;
// DashboardVisible is a presentation decision applied when the Logs list is
// rendered, so suppressed lines still reach every diagnostic consumer.

// Supported subsystem filter values. These are exactly the options the Logs
// page's subsystem <select> offers, so a classified line can never escape
// every filter bucket.
const (
	subsystemService     = "service"
	subsystemAuth        = "auth"
	subsystemStream      = "stream"
	subsystemPoints      = "points"
	subsystemPredictions = "predictions"
	subsystemWatch       = "watch"
	subsystemDrops       = "drops"
	subsystemUpdater     = "updater"
	subsystemSystem      = "system"
	subsystemOther       = "other"
)

// Severity filter values.
const (
	levelError   = "error"
	levelWarning = "warning"
	levelInfo    = "info"
)

// LogPresentation is the classification of one plain-text log line.
//
// Class is a semantic CSS class (always one of the values styled in
// input.css) and Emoji a decorative Unicode emoji rendered in a separate
// aria-hidden span before the untouched line text. When the source line
// already starts with an emoji of its own, HasLeadingEmoji is true and Emoji
// is empty so the page never shows a doubled icon.
//
// Level and Subsystem are the two filter dimensions, resolved independently
// of each other. Reconnect marks the reconnect/recovery family so the "show
// reconnect events" toggle stays orthogonal to both. DashboardVisible is
// false only for structured DEBUG/INFO the classifier does not recognize as
// a user-facing event — a WARN, an ERROR, and any line whose level cannot be
// read are always visible.
type LogPresentation struct {
	Class           string
	Emoji           string
	HasLeadingEmoji bool

	Level            string
	Subsystem        string
	Reconnect        bool
	DashboardVisible bool
}

// logMsgRule maps slog msg values (exact matches first, then prefixes) to a
// user-facing event: a semantic class, an emoji and a subsystem. Matching
// this table is what makes a DEBUG/INFO line worth showing on the human Logs
// page. Rules are evaluated in order and the first match wins, so more
// specific prefixes must precede broader ones (e.g. the "Auto-update: binary
// replaced successfully" rule before the bare "Auto-update" catch-all).
type logMsgRule struct {
	exact     []string
	prefix    []string
	class     string
	emoji     string
	subsystem string

	// match recognizes an event whose msg is assembled at runtime and so has
	// no fixed literal to list. Consulted after exact and prefix.
	match func(msg string) bool

	// quietAtDebug marks an event whose emitter logs the SAME line at INFO
	// when it matters and at DEBUG on every routine cycle. The INFO variant
	// is the signal and is listed; the DEBUG variant is the routine repeat
	// and stays off the page. Severity still decides colour, and a WARN or
	// ERROR is never affected by this.
	quietAtDebug bool
}

// dropProgressRe recognizes the drop-campaign progress line, which
// internal/drops builds at runtime as "<Game> [<streamer>] <Campaign>: --> N%"
// (the game segment is omitted when unknown). The emitter deliberately
// throttles it: INFO only when progress crosses a 5% checkpoint — including
// reaching 100% / claim-ready — and DEBUG on every other cycle. Anchored at
// both ends so only that exact shape matches.
var dropProgressRe = regexp.MustCompile(`^(?:[^\[\]]+ )?\[[^\[\]]+\] .+: -*> \d{1,3}%$`)

// isDropProgressLine reports whether msg is a drop-campaign progress line.
func isDropProgressLine(msg string) bool { return dropProgressRe.MatchString(msg) }

// logMsgRules covers the miner's user-facing INFO/DEBUG events. It is
// consulted for every level: a WARN or ERROR whose msg matches keeps its
// severity colour but adopts the rule's subsystem.
var logMsgRules = []logMsgRule{
	// Startup banner / lifecycle milestones.
	{exact: []string{
		"Twitch Channel Points Miner",
		"Initializing Twitch Channel Points Miner",
		"Starting mining operations",
	}, class: "log-startup", emoji: "🚀", subsystem: subsystemService},

	// Authentication.
	{exact: []string{"Authenticating with Twitch"}, class: "log-auth", emoji: "🔐", subsystem: subsystemAuth},
	{exact: []string{
		"Authentication successful",
		// Credential lifecycle milestones an operator acts on.
		"Replaced Twitch credentials (external complete-set replacement)",
		"Migrated stored auth token to encrypted form (AES-256-GCM)",
		"Published rotated Twitch credentials",
		"Recovered Twitch session by validating the staged candidate pair",
		"Recovered Twitch session via refresh token",
		"Twitch credentials rotated; re-authorizing PubSub user topics",
		"Twitch authorization recovered; clearing the reauthorization-required state",
		"Owner Twitch login changed; adopting the new canonical login (storage key unchanged)",
		"Pinned the Twitch owner identity for rename-tolerant startups",
	}, class: "log-auth-success", emoji: "🔐", subsystem: subsystemAuth},

	// Streamer online/offline transitions.
	{exact: []string{"Streamer is online"}, class: "log-streamer-online", emoji: "🟢", subsystem: subsystemStream},
	{exact: []string{"Streamer went offline", "Streamer status confirmed offline"},
		class: "log-streamer-offline", emoji: "🔴", subsystem: subsystemStream},

	// Connections.
	{exact: []string{"WebSocket connected"}, class: "log-connected", emoji: "🔌", subsystem: subsystemStream},
	{exact: []string{"Discord notification provider connected"}, class: "log-connected", emoji: "🤖", subsystem: subsystemStream},
	{exact: []string{"Joined IRC chat", "Chat mention", "Left IRC chat"},
		class: "log-chat", emoji: "💬", subsystem: subsystemStream},
	{exact: []string{
		"Discord configuration updated and reconnected",
		"Reconnecting WebSocket",
		"WebSocket reconnect requested",
		"IRC reconnect requested by server",
		"Reconnected to IRC chat",
	}, class: "log-reconnect", emoji: "🔄", subsystem: subsystemStream},

	// Stream state-machine outcomes that explain a missing online transition.
	// internal/twitch/client.go:1683,1689,1704
	{prefix: []string{
		"Bring-online session superseded",
		"Cannot fetch Spade URL",
		"Cannot confirm stream status",
	}, class: "log-info", emoji: "ℹ️", subsystem: subsystemStream},

	// Predictions/bets. Outcomes (WIN/LOSE/REFUND) are attribute-driven and
	// classified in matchLogMsgRule; these are the fixed-message events.
	{exact: []string{"Skipping bet", "Bet amount too low"}, class: "log-bet-filter", emoji: "🧲", subsystem: subsystemPredictions},
	{exact: []string{"Auto-bet gated"}, class: "log-bet-gated", emoji: "🛡️", subsystem: subsystemPredictions},
	{exact: []string{"Not enough points for prediction"}, class: "log-bet-failed", emoji: "🚫", subsystem: subsystemPredictions},
	{exact: []string{
		"Placing prediction bet",
		"Prediction confirmed",
		"Prediction event scheduled",
		"Manual prediction bet placed",
		"Manual bet placed via dashboard",
		"Duplicate prediction result ignored",
	}, class: "log-bet-general", emoji: "🔮", subsystem: subsystemPredictions},

	// Point gains beyond the attribute-driven "Points earned".
	{exact: []string{"Claiming bonus", "Claiming moment"},
		prefix: []string{"Claimed channel points bonus"}, class: "log-bonus", emoji: "🎉", subsystem: subsystemPoints},
	{exact: []string{
		"Contributed to community goal",
		// Channel-point spending: the operator's points leaving the balance.
		// internal/miner/rewards.go:91,328 · internal/twitch/client.go:2498
		"Redeeming custom reward",
		"Redeemed custom reward",
		"Auto-redeemed custom reward",
	}, class: "log-points-gain", emoji: "💰", subsystem: subsystemPoints},
	{exact: []string{"Joining raid"}, class: "log-points-raid", emoji: "🚀", subsystem: subsystemPoints},
	// The budget gate on auto-redeem: explains a redemption that did NOT
	// happen. internal/miner/rewards.go:291
	{exact: []string{
		"Auto-redeem: over budget, skipping",
		"Skipping community goal contribution: configured limit resolves to zero",
	}, class: "log-info", emoji: "ℹ️", subsystem: subsystemPoints},

	// Watch-slot lifecycle.
	{exact: []string{"Watch slot assigned"}, class: "log-watch-assigned", emoji: "🎯", subsystem: subsystemWatch},
	{exact: []string{"Watch slot released"}, class: "log-watch-released", emoji: "📴", subsystem: subsystemWatch},
	{exact: []string{"Watch slot reason changed"}, class: "log-watch-changed", emoji: "🔀", subsystem: subsystemWatch},
	{exact: []string{"Rotating watch pair"}, class: "log-rotation", emoji: "🔄", subsystem: subsystemWatch},
	// The real emission is far longer than the msg itself, so this must be a
	// prefix — as an exact rule it never fired. internal/watcher/watcher.go:2194
	{prefix: []string{"Pursuing watch streak"}, class: "log-pursuing-streak", emoji: "🔥", subsystem: subsystemWatch},
	{prefix: []string{"Releasing the watch-streak boost slot"},
		class: "log-watch-released", emoji: "📴", subsystem: subsystemWatch},
	// The watching heartbeat: the mechanism that actually earns points, and
	// the roster it is earning them from.
	// internal/watcher/watcher.go:994,1078 · internal/watcher/session.go:524
	{exact: []string{"Sent minute watched", "Watching streams"},
		class: "log-points-watch", emoji: "👀", subsystem: subsystemWatch},
	{exact: []string{"Watch session refresh"}, class: "log-watch-changed", emoji: "🔀", subsystem: subsystemWatch},

	// Drops and discovery.
	{exact: []string{
		"Claiming drop",
		"Channel-restricted drop campaign assigned to streamer",
	}, prefix: []string{"Drops sync complete"}, class: "log-drops", emoji: "🎁", subsystem: subsystemDrops},
	{exact: []string{"Skipping already-claimed reward within active drop campaign"},
		prefix: []string{"Skipping drop campaign"}, class: "log-drop-skipped", emoji: "🚫", subsystem: subsystemDrops},
	// All-clear counterpart of the "no game names resolved" WARN
	// (internal/drops/drops.go:2643 -> :2652). A recovery event is listed
	// wherever its warning is, so a cleared condition never looks stuck.
	{exact: []string{"Drops game filter: unresolved game-name fail-open condition cleared"},
		class: "log-health-ok", emoji: "💚", subsystem: subsystemDrops},
	{exact: []string{"Claimed drop"}, class: "log-drop-complete", emoji: "✅", subsystem: subsystemDrops},
	// Campaign progress. internal/drops/drops.go logCampaignProgress emits the
	// same runtime-built line at INFO on a 5% checkpoint and at DEBUG on every
	// routine cycle, so quietAtDebug keeps the checkpoints and drops the
	// repeats — the emitter's own throttling, honoured on the page.
	{match: isDropProgressLine, quietAtDebug: true,
		class: "log-drops", emoji: "🎁", subsystem: subsystemDrops},
	{exact: []string{"Discovered channel selected", "Switching discovered channel"},
		prefix: []string{"Discovery pool empty"}, class: "log-discovery", emoji: "🔎", subsystem: subsystemDrops},

	// Auto-update. Specific milestones first, then the informational catch-all.
	{prefix: []string{"Auto-update: binary replaced successfully"}, class: "log-update-success", emoji: "✅", subsystem: subsystemUpdater},
	{exact: []string{"app: self-update applied successfully across restart"},
		class: "log-update-success", emoji: "✅", subsystem: subsystemUpdater},
	{exact: []string{"Auto-update: newer release available", "Auto-update: downloading new binary"},
		class: "log-update", emoji: "⬆️", subsystem: subsystemUpdater},
	{exact: []string{"Auto-update watcher started"}, prefix: []string{"Auto-update"},
		class: "log-update", emoji: "🔄", subsystem: subsystemUpdater},

	// Settings / runtime config.
	{exact: []string{
		"Settings saved to config file",
		"Runtime settings updated",
		"Health settings updated",
		"Updated auto-redeem config",
	}, class: "log-settings", emoji: "⚙️", subsystem: subsystemSystem},

	// Health recovery.
	{exact: []string{"Connection restored", "Connection stabilized"},
		class: "log-health-ok", emoji: "💚", subsystem: subsystemSystem},

	// Database / analytics bookkeeping.
	{exact: []string{
		"Pruned old analytics history",
		"Migration column already present, skipping (self-heal)",
	}, class: "log-database", emoji: "💾", subsystem: subsystemSystem},

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
		"Daily summary sent",
		"Shutting down...",
		// Streamer-removal lifecycle: "Added new streamer"/"Removed streamer"
		// were already listed, so their commit/purge counterparts must be too.
		"Streamer removal committed",
		"Purged deleted streamer's persisted state",
		// Read after a crash: what the miner reconciled at startup.
		"Arbitrated streamer removals recovered after an unclean shutdown",
		"Reconciled unfinished streamer deletions at startup",
		"Aborted uncommitted streamer removal recovered after unclean shutdown",
		"Promoted a committed streamer removal recovered after unclean shutdown; purge will be retried",
		"Reconciled streamer rename by stable Twitch channel ID",
		"Tightened config file permissions to 0600 (it may contain the Discord bot token)",
	}, prefix: []string{"Prepared streamer removal"},
		class: "log-service", emoji: "🔧", subsystem: subsystemService},
}

// logSubsystemRule attributes a semantic subsystem to a line WITHOUT making
// it dashboard-visible. It exists so a WARN or an ERROR whose msg is not a
// recognized user-facing event still lands in the right filter bucket (a
// Drops failure under Drops, not under Other) — and, deliberately, so that
// giving raw DEBUG chatter a bucket never smuggles it back onto the page.
type logSubsystemRule struct {
	exact     []string
	prefix    []string
	subsystem string
}

// logSubsystemRules is consulted only when logMsgRules did not match. Order
// matters: the first match wins, so narrower families (watch, drops) precede
// the broad "Failed to send …" style prefixes of the system family.
var logSubsystemRules = []logSubsystemRule{
	// Watch / watch-time. internal/watcher/**, plus the beacon build failure
	// in internal/twitch that makes watch credit impossible.
	{prefix: []string{
		"Watcher ",
		"Watch ",
		"Watching ",
		"Sent minute watched",
		"Releasing the watch-streak",
		"Pursuing watch streak",
		"Minute-watched beacon",
		"Excluding avoided streamers from watch",
		"Streak cache",
		"Failed to write streak cache",
		"Failed to encode streak cache",
		"Failed to migrate watch-streak cache",
	}, exact: []string{
		"Failed to send minute watched",
		"Failed to record watch time",
		"Failed to prune old watch-time events",
		"Failed to load watch-time window",
		"Failed to load watch-time window for debug snapshot",
		"Failed to create watch-time store, rotation fairness will not persist across restarts",
		"Failed to simulate watching",
		"Skipped minute watched: session changed mid-send (stale)",
	}, subsystem: subsystemWatch},

	// Drops and discovery. "Drops…" and "Drop <space>…" are reliable family
	// markers; "Dropping stale channel-points context" deliberately matches
	// neither (it is a channel-points event, not a drop).
	{prefix: []string{
		"Drops",
		"Drop ",
		"Drop-transition",
		"Directory discovery",
		"Discovery pool",
		"Failed to initialize drop",
		"Failed to fetch channel drop campaign IDs",
		"Failed to load past drop campaign",
		"Channel-restricted drop campaign",
	}, exact: []string{
		"Failed to claim drop",
		"Failed to fetch inventory for claim history check",
		"Failed to record campaign in catalog",
		"Failed to record drop-claimed annotation",
	}, subsystem: subsystemDrops},

	// Predictions / bets.
	{prefix: []string{
		"Prediction",
		"Auto-bet",
		"Placing prediction",
		"Manual prediction",
		"Manual bet",
		"Duplicate prediction",
		"Skipping bet",
		"Bet amount",
		"Not enough points for prediction",
		"Failed to make prediction",
		"Failed to record prediction",
		"Clamped out-of-range prediction",
	}, subsystem: subsystemPredictions},

	// Channel points.
	{prefix: []string{
		"Channel points ",
		"Bonus claim",
		"Bonus poll",
		"Claiming bonus",
		"Claiming moment",
		"Claimed channel points bonus",
		"Auto-redeem",
		"Redeeming custom reward",
		"Redeemed custom reward",
		"Joining raid",
		"Contributed to community goal",
		"Skipping community goal contribution",
		"Skipping channel-points action",
		"Skipping bonus claim",
		"Dropping stale channel-points context",
		"Failed to load channel points",
	}, exact: []string{
		"Failed to claim bonus",
		"Failed to claim moment",
		"Failed to contribute to community goal",
		"Failed to join raid",
	}, subsystem: subsystemPoints},

	// Authentication and credential lifecycle. "Twitch …" is an auth marker
	// here only because the startup banner already matched logMsgRules.
	{prefix: []string{
		"Twitch ",
		"Stored Twitch",
		"Validated Twitch token",
		"Freshly granted Twitch",
		"Hourly",
		"Could not conclusively validate the stored Twitch token",
		"Auth rejection",
		"Authenticating",
		"Authentication",
		"Owner Twitch login changed",
		"Pinned the Twitch owner identity",
		"Migrated stored auth token",
		"Recovered Twitch session",
		"Published rotated Twitch credentials",
		"Replaced Twitch credentials",
		"Retried persisting Twitch credentials",
		"Re-authorizing user topic",
		"IRC login authentication failed",
		"Startup: could not resolve own Twitch user ID",
		"Removed stale auth temp file",
		"Directory sync after auth save",
	}, exact: []string{
		"Failed to save auth",
		"Failed to migrate plaintext auth token to encrypted form",
		"Failed to persist rotated Twitch credentials; keeping the new pair authoritative in memory and retrying at the next checkpoint",
		"Failed to persist the dropped (consumed) refresh token; will retry at the next checkpoint",
		"Failed to persist the dropped (possibly consumed) refresh token; will retry at the next checkpoint",
	}, subsystem: subsystemAuth},

	// Stream / connection transport: PubSub, IRC, GQL and stream state.
	{prefix: []string{
		"WebSocket",
		"PubSub",
		"IRC ",
		"GQL ",
		"Creating IRC client",
		"Rejected joinChat",
		"Streamer is online",
		"Streamer went offline",
		"Streamer status confirmed",
		"Bring-online session",
		"Cannot fetch Spade URL",
		"Cannot confirm stream status",
		"Cannot refresh stream info",
		"Metadata refresh after stream-up",
		"PlaybackAccessToken",
		"Reconciled streamer rename",
		"Reconnecting",
		"Reconnected",
		"Joined IRC chat",
		"Left IRC chat",
		"Chat mention",
	}, exact: []string{
		"Failed to reconnect",
		"Failed to establish IRC chat generation",
		"Failed to parse PubSub message",
		"Failed to parse WebSocket message",
		"Failed to log chat message",
		"Failed to unsubscribe removed streamer's topic",
	}, subsystem: subsystemStream},

	// Updater.
	{prefix: []string{
		"Auto-update",
		"app: self-update",
		"app: durable updater",
		"app: anomalous durable updater",
		"app: failed to consume updater",
	}, subsystem: subsystemUpdater},

	// Service: the streamer roster and process lifecycle.
	{prefix: []string{
		"Loading streamers",
		"Loaded streamer",
		"Added new streamer",
		"Removed streamer",
		"Streamer removal",
		"Streamer removed",
		"Prepared streamer removal",
		"Purged deleted streamer",
		"Arbitrated streamer removals",
		"Reconciled unfinished streamer deletions",
		"Reconciled duplicate config entries",
		"Aborted uncommitted streamer removal",
		"Promoted a committed streamer removal",
		"Re-added streamer's stale-history purge",
		"Some pending streamer deletions",
		"Some prepared streamer removals",
		"Streamer settings not applied",
		"Streamer reconciliation conflict",
		"Streamer not loaded",
		"Could not resolve channel ID",
		"Imported followed channels",
		"Sample configuration generated",
		"Shutting down",
		"=== Session Report",
		"Streamer stats",
		// Prefixes, not exact matches: the real literals carry a trailing
		// clause ("… until this is fixed") or are built by concatenation,
		// so an exact entry could never fire and the ERROR fell into "other".
		"Failed to build streamer-deletion coordinator",
		"Failed to compensate a prepared streamer removal",
	}, subsystem: subsystemService},

	// Everything else operational: settings, health, database, notifications,
	// the web/debug servers and the lifecycle observability codes.
	{prefix: []string{
		"Settings",
		"Runtime settings",
		"Runtime capabilit",
		"Health settings",
		"Connection ",
		"Pruned old analytics",
		"Migration",
		"Applying migration",
		"Web server",
		"Debug server",
		"debug_server",
		"dashboard_trusted_lan_cidrs",
		"lifecycle",
		"app:",
		"Daily summary",
		"Discord",
		"Notification",
		"Push notification",
		"Test ",
		"Invalid notification batching interval",
		"Miner background loops",
		"Tightened config file permissions",
		"Updated auto-redeem config",
		"Failed to save config",
		"Failed to save auto-redeem config",
		"Failed to create analytics",
		"Failed to create notification",
		"Failed to start notification manager",
		"Failed to update Discord config",
		"Failed to connect Discord provider",
		"Failed to disconnect Discord provider",
		"Failed to dispatch push notification",
		"Failed to flush notification batch",
		"Failed to get notification",
		"Failed to get point rules",
		"Failed to delete point rule",
		"Failed to mark point rule",
		"Failed to reset point rules",
		"Failed to load notification config",
		"Failed to send",
		"Failed to reverse a partially committed analytics rename",
		"Failed to reconcile capability topic",
		"Failed to persist owner-identity reconciliation",
		"Failed to read",
		"Failed to record ",
		"Failed to prune analytics",
		"Blocked cross-origin",
		"Invalid dashboard",
		"database.",
	}, subsystem: subsystemSystem},
}

// classifyLogLine assigns the presentation metadata for one plain-text log
// line. It is deterministic, never mutates the line, and reads slog
// TextHandler tokens (level=…, msg=…, reason=…, result=…) with quote
// awareness so attribute order, quoting, or a decorative leading emoji cannot
// skew it.
func classifyLogLine(line string) LogPresentation {
	p := LogPresentation{HasLeadingEmoji: hasLeadingEmoji(line)}

	rawLevel := logAttr(line, "level")
	p.Level = normalizeLogLevel(rawLevel)
	msg := logAttr(line, "msg")

	rule, matched := matchLogMsgRule(line, msg)
	if matched {
		p.Class, p.Emoji = rule.class, rule.emoji
		p.Subsystem = rule.subsystem
		p.Reconnect = rule.class == "log-reconnect"
	} else {
		p.Subsystem = matchLogSubsystem(msg)
	}

	// Severity outranks category for the COLOUR only: a WARN reads as a WARN
	// at a glance. The subsystem resolved above is untouched, which is what
	// keeps the two filter dimensions independent.
	switch p.Level {
	case levelError:
		p.Class, p.Emoji = "log-error", "❌"
	case levelWarning:
		p.Class, p.Emoji = "log-warn", "⚠️"
	default:
		if !matched {
			// Unknown line shape (no readable slog level token) still gets a
			// visible marker rather than a bare neutral wall of text.
			if rawLevel == "" {
				p.Class, p.Emoji = "log-info", "✨"
			} else {
				p.Class, p.Emoji = "log-info", "ℹ️"
			}
		}
	}

	// Visibility. A WARN or an ERROR is never hidden, however unrecognized.
	// A line whose level cannot be read is not assumed harmless either: it
	// stays visible so nothing disappears without a trace. Only structured
	// DEBUG/INFO with no recognized user-facing meaning is suppressed.
	switch {
	case p.Level == levelError, p.Level == levelWarning:
		p.DashboardVisible = true
	case !isKnownLogLevel(rawLevel):
		p.DashboardVisible = true
	default:
		p.DashboardVisible = matched
		if matched && rule.quietAtDebug && strings.HasPrefix(rawLevel, "DEBUG") {
			// The routine DEBUG repeat of a throttled event. Its INFO
			// counterpart is the signal an operator wants; this is not.
			p.DashboardVisible = false
		}
	}

	if p.HasLeadingEmoji {
		// The source text already opens with its own emoji; suppress the
		// decorative one so the page never renders a doubled icon. The class
		// (colour) still applies.
		p.Emoji = ""
	}
	return p
}

// matchLogMsgRule resolves the user-facing event for a line, if any. The two
// attribute-driven messages are handled first: their msg alone does not
// identify the category.
func matchLogMsgRule(line, msg string) (logMsgRule, bool) {
	switch msg {
	case "Points earned":
		class, emoji := pointsEarnedClass(line)
		return logMsgRule{class: class, emoji: emoji, subsystem: subsystemPoints}, true
	case "Prediction result":
		class, emoji := predictionResultClass(line)
		return logMsgRule{class: class, emoji: emoji, subsystem: subsystemPredictions}, true
	}

	for _, rule := range logMsgRules {
		for _, exact := range rule.exact {
			if msg == exact {
				return rule, true
			}
		}
		for _, prefix := range rule.prefix {
			if strings.HasPrefix(msg, prefix) {
				return rule, true
			}
		}
		if rule.match != nil && rule.match(msg) {
			return rule, true
		}
	}
	return logMsgRule{}, false
}

// matchLogSubsystem attributes a semantic bucket to a msg that is not a
// recognized user-facing event, so severity-only lines still filter usefully.
// It never grants dashboard visibility.
func matchLogSubsystem(msg string) string {
	if msg == "" {
		return subsystemOther
	}
	for _, rule := range logSubsystemRules {
		for _, exact := range rule.exact {
			if msg == exact {
				return rule.subsystem
			}
		}
		for _, prefix := range rule.prefix {
			if strings.HasPrefix(msg, prefix) {
				return rule.subsystem
			}
		}
	}
	return subsystemOther
}

// normalizeLogLevel maps a slog level token to a filter value. Anything that
// is not a recognized severity (absent, malformed, or a level name this build
// does not emit) reads as informational for styling purposes — but see
// isKnownLogLevel: such a line is never suppressed on that basis.
func normalizeLogLevel(raw string) string {
	switch {
	case strings.HasPrefix(raw, "ERROR"):
		return levelError
	case strings.HasPrefix(raw, "WARN"):
		return levelWarning
	default:
		return levelInfo
	}
}

// isKnownLogLevel reports whether the level token is one this build actually
// writes. A line failing this check is malformed or from a legacy format, and
// the visibility policy treats it conservatively: shown, never silently
// dropped.
func isKnownLogLevel(raw string) bool {
	for _, known := range []string{"ERROR", "WARN", "INFO", "DEBUG"} {
		if strings.HasPrefix(raw, known) {
			return true
		}
	}
	return false
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
// The CSS coverage test walks this list to guarantee each class is styled, so
// a classifier rule can never reference a class that renders unstyled.
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

// allLogSubsystems returns every subsystem value the classifier can emit,
// which must stay exactly the set the Logs page's subsystem filter offers.
func allLogSubsystems() []string {
	return []string{
		subsystemService,
		subsystemAuth,
		subsystemStream,
		subsystemPoints,
		subsystemPredictions,
		subsystemWatch,
		subsystemDrops,
		subsystemUpdater,
		subsystemSystem,
		subsystemOther,
	}
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
