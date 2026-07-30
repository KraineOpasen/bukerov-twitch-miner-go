package web

import (
	"fmt"
	"hash/fnv"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/events"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/health"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/util"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/version"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/watcher"
)

// statsTTL bounds how often the Overview recomputes per-streamer
// analytics-derived figures (points-today / points-per-hour) from SQLite, so
// the 30s live poll doesn't add DB load beyond what the old dashboard did.
const statsTTL = 60 * time.Second

// overviewPollSeconds is the single source of truth for how often the
// Overview live partial polls (#overview-live's hx-trigger "every Ns") AND
// for the F2.5 staleness thresholds derived from it (fresh <= 2x, delayed
// <= 4x, stale > 4x). Keeping one constant means the client-side clock and
// the server-rendered trigger interval can never drift apart (I1).
const overviewPollSeconds = 30

// OverviewCardView wraps a streamer card with the raw (unformatted)
// points-today figure the F2.2 numeric sort needs. It embeds StreamerInfo so
// every existing `.Field` reference in overview_card keeps working unchanged
// — this is purely additive. PointsTodayRaw/HasTodayRaw come straight from
// the memoised streamerStats cache (never parsed back out of a formatted
// string — I13).
type OverviewCardView struct {
	StreamerInfo
	PointsTodayRaw int
	HasTodayRaw    bool
}

// OverviewHealthView is the aggregated health chip shown on the Overview
// header. State drives the chip's icon/class (never color alone — I18); Label
// and Detail are both localized, human strings (Detail carries the deciding
// signal(s) or the reason a verdict couldn't be reached, e.g. "no provider").
type OverviewHealthView struct {
	State  string // "healthy" | "degraded" | "unhealthy" | "unknown"
	Label  string
	Detail string
}

// OverviewPageData is the Overview page/partial view model. It embeds the
// existing OverviewData so every current `.Field` reference (including in
// handleDashboard, which is not touched by this change) keeps compiling, and
// adds the F2 card-hierarchy/health/predictions/polling additions alongside
// it. NOTE: as of the F2 card-hierarchy work, OverviewData's own
// TrackedLive/TrackedUnknown/TrackedOffline/Untracked fields (declared in
// viewmodels.go) are no longer populated by buildOverviewData below — every
// Overview template consumer now reads the *Cards slices instead. The fields
// stay declared (viewmodels.go is out of scope here, and StreamerGridData —
// a distinct type — still uses same-named fields for the Streamers page) but
// are write-only dead weight on this path; left as an intentional zero value
// rather than duplicating card-building work nobody reads.
type OverviewPageData struct {
	OverviewData
	LiveCards      []OverviewCardView
	UnknownCards   []OverviewCardView
	OfflineCards   []OverviewCardView
	UntrackedCards []OverviewCardView
	Health         OverviewHealthView
	// PredictionsState is the technical (not betting-outcome) status of the
	// live-predictions board: "active" (>=1 round), "idle" (board reachable,
	// nothing on it right now), or "unavailable" (no provider, or the miner
	// isn't running). There is no "degraded" state — nothing in the system
	// honestly distinguishes it (documented omission, design.md §1).
	PredictionsState      string
	PredictionsStateLabel string
	// PollSeconds is overviewPollSeconds, rendered into the partial's
	// hx-trigger so the client-side stale-clock thresholds and the actual
	// poll interval are always derived from the same number.
	PollSeconds int
}

// handleAPIOverview renders the live Overview content partial (header stats,
// events ticker, live-predictions board and the streamer grid) plus an
// out-of-band update for the sidebar "Now Watching" block. Everything is built
// from in-memory state; the only SQLite reads (points-today / per-hour) are
// memoised behind statsTTL.
func (s *Server) handleAPIOverview(w http.ResponseWriter, r *http.Request) {
	data := s.buildOverviewData(s.langFromRequest(r))

	s.renderPartial(w, r, "overview_live", data)
}

// handleAPINowWatching renders just the pinned sidebar "Now Watching" block.
// It's polled from every page's sidebar, so it's kept cheap: in-memory watch
// state plus the memoised per-streamer stats, no fresh SQLite reads beyond the
// shared cache.
func (s *Server) handleAPINowWatching(w http.ResponseWriter, r *http.Request) {
	streamers := s.snapshotStreamers()
	stats, _, _ := s.ensureStats(streamers)

	s.mu.RLock()
	provider := s.overviewProvider
	s.mu.RUnlock()

	status := s.status.GetStatus()
	var slots WatchSlotsView
	if provider != nil {
		slots = provider.WatchSlots()
	}
	if slots.Watching == nil {
		slots.Watching = map[string]bool{}
	}
	view := s.buildNowWatching(streamers, slots, stats, status.ConnectionLost)

	s.renderPartial(w, r, "now_watching", view)
}

// snapshotStreamers returns a stable slice of the tracked streamers under the
// read lock, so the rest of the assembly runs lock-free.
func (s *Server) snapshotStreamers() []*models.Streamer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*models.Streamer, len(s.streamers))
	copy(out, s.streamers)
	return out
}

// ensureStats refreshes the cached per-streamer analytics figures if the cache
// is older than statsTTL. Returns the cache map, total points and today total.
func (s *Server) ensureStats(streamers []*models.Streamer) (map[string]streamerStats, int, int) {
	s.mu.RLock()
	fresh := s.statsCache != nil && time.Since(s.statsAt) < statsTTL
	cache := s.statsCache
	s.mu.RUnlock()

	total, today := 0, 0
	if fresh {
		for _, st := range streamers {
			if cs, ok := cache[st.GetUsername()]; ok {
				today += cs.pointsToday
			}
			total += st.GetChannelPoints()
		}
		return cache, total, today
	}

	repo := s.analytics.Repository()
	newCache := make(map[string]streamerStats, len(streamers))
	todayStart := time.Now().Truncate(24 * time.Hour).UnixMilli()
	hoursToday := time.Since(time.Now().Truncate(24 * time.Hour)).Hours()

	for _, st := range streamers {
		points := st.GetChannelPoints()
		total += points

		cs := streamerStats{}
		data, err := repo.GetStreamerData(st.GetUsername())
		if err == nil && len(data.Series) > 0 {
			base := -1
			for i := len(data.Series) - 1; i >= 0; i-- {
				if data.Series[i].X < todayStart {
					base = data.Series[i].Y
					break
				}
			}
			if base < 0 {
				base = data.Series[0].Y // all points are from today
			}
			cs.pointsToday = points - base
			if cs.pointsToday < 0 {
				cs.pointsToday = 0
			}
			if hoursToday >= 0.5 {
				cs.pointsPerHour = int(float64(cs.pointsToday) / hoursToday)
				cs.hasRate = true
			}
		}
		newCache[st.GetUsername()] = cs
		today += cs.pointsToday
	}

	s.mu.Lock()
	s.statsCache = newCache
	s.statsAt = time.Now()
	s.mu.Unlock()

	return newCache, total, today
}

// buildOverviewData assembles the full Overview view model from in-memory
// state (streamers, watcher slots, pool predictions, the events ring) plus the
// memoised analytics figures.
func (s *Server) buildOverviewData(lang string) OverviewPageData {
	tr := func(key string) string { return s.i18n.T(lang, key) }
	streamers := s.snapshotStreamers()
	stats, total, today := s.ensureStats(streamers)

	s.mu.RLock()
	refresh := s.refresh
	discordEnabled := s.discordEnabled
	debugURL := s.debugURL
	provider := s.overviewProvider
	// healthProvider is read under the same RLock as the other providers
	// above (server.go's SetHealthProvider writes it under s.mu.Lock()).
	healthProvider := s.healthProvider
	s.mu.RUnlock()

	status := s.status.GetStatus()

	var slots WatchSlotsView
	var predictions []LivePrediction
	if provider != nil {
		slots = provider.WatchSlots()
		predictions = provider.LivePredictions()
	}
	if slots.Watching == nil {
		slots.Watching = map[string]bool{}
	}

	// Which streamers have a live prediction (for the card marker).
	predByStreamer := make(map[string]bool, len(predictions))
	for _, p := range predictions {
		predByStreamer[p.Streamer] = true
	}

	live, unknown, offline, untracked, ticker := s.buildCards(streamers, slots, stats, predByStreamer, tr)

	// LiveCount counts CONFIRMED-online streamers only; a card in the live group
	// that is merely holding its slot during a transient unknown (Unconfirmed) is
	// not counted as live.
	liveCount := 0
	for _, c := range live {
		if c.IsLive {
			liveCount++
		}
	}

	// Health Center snapshot read is a lock-free atomic load (health.Center),
	// not SQLite and not a fresh Twitch call — safe to do on every request.
	var healthSnap health.Snapshot
	if healthProvider != nil {
		healthSnap = healthProvider.HealthSnapshot()
	}
	healthView := buildOverviewHealth(tr, healthProvider != nil, healthSnap, status.ConnectionLost)

	predState := predictionsState(provider != nil, status.Status == StatusRunning, len(predictions))

	data := OverviewData{
		Username:       s.username,
		RefreshMinutes: refresh,
		Version:        version.Version,
		DiscordEnabled: discordEnabled,
		DebugURL:       debugURL,
		BotStatus:      string(status.Status),
		BotStatusLabel: botStatusLabel(tr, status.Status),
		Connected:      status.Status == StatusRunning && !status.ConnectionLost && !status.ReauthRequired,
		Stale:          status.ConnectionLost,
		ReauthRequired: status.ReauthRequired,
		ConnectionLost: status.ConnectionLost,
		NetState:       netState(status),
		TotalPoints:    util.FormatNumber(total),
		StreamerCount:  len(streamers),
		LiveCount:      liveCount,
		PointsToday:    util.FormatNumber(today),
		Ticker:         ticker,
		Predictions:    buildPredictionViews(predictions),
		NowWatching:    s.buildNowWatching(streamers, slots, stats, status.ConnectionLost),
		// TrackedLive/TrackedUnknown/TrackedOffline/Untracked are deliberately
		// left unset: no Overview template consumer reads them anymore (see
		// the OverviewPageData doc comment above) — every render goes through
		// the *Cards slices below, built from the same live/unknown/offline/
		// untracked slices without a second pass over the streamers.
		GeneratedUnix: time.Now().Unix(),
	}

	return OverviewPageData{
		OverviewData:          data,
		LiveCards:             toCardViews(live, stats),
		UnknownCards:          toCardViews(unknown, stats),
		OfflineCards:          toCardViews(offline, stats),
		UntrackedCards:        toCardViews(untracked, stats),
		Health:                healthView,
		PredictionsState:      predState,
		PredictionsStateLabel: tr("ov.pred_state." + predState),
		PollSeconds:           overviewPollSeconds,
	}
}

// toCardViews wraps each StreamerInfo with its raw points-today figure looked
// up from the memoised streamerStats cache (nil-safe: a nil/empty stats map
// simply leaves HasTodayRaw false for every card). Returns nil for an empty
// input so an empty group renders identically to before (no group heading).
func toCardViews(infos []StreamerInfo, stats map[string]streamerStats) []OverviewCardView {
	if len(infos) == 0 {
		return nil
	}
	out := make([]OverviewCardView, len(infos))
	for i, info := range infos {
		cv := OverviewCardView{StreamerInfo: info}
		if cs, ok := stats[info.Name]; ok {
			cv.PointsTodayRaw = cs.pointsToday
			cv.HasTodayRaw = true
		}
		out[i] = cv
	}
	return out
}

// AvatarInitial returns the uppercased first rune of the trimmed streamer
// name for the F2.4 avatar-initial fallback, or "?" for an empty/whitespace
// name. Twitch exposes no profile-image URL anywhere in this in-memory model
// (design.md §1), so this deterministic initial is the only honest fallback —
// no network call is ever made for it.
func (si StreamerInfo) AvatarInitial() string {
	name := strings.TrimSpace(si.Name)
	if name == "" {
		return "?"
	}
	r, size := utf8.DecodeRuneInString(name)
	if size == 0 || r == utf8.RuneError {
		return "?"
	}
	return string(unicode.ToUpper(r))
}

// avatarBucketCount is the number of avatar color buckets, matching the
// input.css --chart-series-1..6 tokens (s-avatar-b0..b5).
const avatarBucketCount = 6

// AvatarBucket deterministically maps the streamer's lowercased, trimmed name
// to one of avatarBucketCount color buckets (FNV-1a 32 % 6) so the same name
// always renders the same avatar color and an empty name lands on bucket 0.
// Pure, no network, no dependency beyond the standard library.
func (si StreamerInfo) AvatarBucket() int {
	name := strings.ToLower(strings.TrimSpace(si.Name))
	if name == "" {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return int(h.Sum32() % avatarBucketCount)
}

// aggregateHealth reduces the Health Center snapshot to the single aggregate
// state shown by the F2.6 Overview health chip, following the precedence
// documented in design.md §1: failed|stalled signals win first (unhealthy),
// then degraded, then unknown-status signals or an empty snapshot (unknown),
// and only "all ok|idle with >=1 signal" reads healthy. connectionLost is
// checked first and always wins — a fully lost connection is never masked by
// a green chip (I14) — and a nil provider reads "unknown" (never a false
// "healthy"), distinguished from an empty snapshot only by reason.
//
// reason is the single source of truth for WHY that state was reached —
// exactly one of "connlost"/"noprovider"/"nosignals"/"signal"/"" (healthy) —
// so the only caller, buildOverviewHealth, can map it straight to an i18n key
// without re-deriving or re-testing any of the conditions above. offending
// lists the human-readable signal name(s) behind a "signal" reason (nil for
// every other reason).
func aggregateHealth(providerPresent bool, snap health.Snapshot, connectionLost bool) (state, reason string, offending []string) {
	if connectionLost {
		return "unhealthy", "connlost", nil
	}
	if !providerPresent {
		return "unknown", "noprovider", nil
	}
	if len(snap.Signals) == 0 {
		return "unknown", "nosignals", nil
	}

	var failed, degraded, unknown []string
	for _, sig := range snap.Signals {
		label := healthSignalLabels[sig.Name]
		if label == "" {
			label = sig.Name
		}
		switch sig.Status {
		case health.StatusFailed, health.StatusStalled:
			failed = append(failed, label)
		case health.StatusDegraded:
			degraded = append(degraded, label)
		case health.StatusUnknown:
			unknown = append(unknown, label)
		}
	}
	switch {
	case len(failed) > 0:
		return "unhealthy", "signal", failed
	case len(degraded) > 0:
		return "degraded", "signal", degraded
	case len(unknown) > 0:
		return "unknown", "signal", unknown
	default:
		return "healthy", "", nil
	}
}

// buildOverviewHealth turns aggregateHealth's verdict into the localized
// OverviewHealthView rendered by the F2.6 health chip (icon + text, title
// tooltip carrying the basis — never color alone, I18). It is a PURE
// reason -> i18n-key mapping: every branching decision already happened
// inside aggregateHealth, so this never re-tests connectionLost/
// providerPresent/snapshot contents itself.
func buildOverviewHealth(tr func(string) string, providerPresent bool, snap health.Snapshot, connectionLost bool) OverviewHealthView {
	state, reason, offending := aggregateHealth(providerPresent, snap, connectionLost)
	view := OverviewHealthView{State: state, Label: tr("ov.health." + state)}
	switch reason {
	case "connlost":
		view.Detail = tr("ov.health.detail.connlost")
	case "noprovider":
		view.Detail = tr("ov.health.detail.noprovider")
	case "nosignals":
		view.Detail = tr("ov.health.detail.nosignals")
	case "signal":
		view.Detail = tr("ov.health.detail.signal") + ": " + strings.Join(offending, ", ")
	}
	return view
}

// predictionsState is the F2.7 pure classifier for the predictions board's
// technical status (never the betting outcome): "unavailable" when there's no
// provider or the miner isn't currently running, "active" when >=1 round is
// on the board, otherwise "idle" (board reachable, nothing live right now —
// not an error). There's no "degraded" state; nothing in the system honestly
// distinguishes one (documented omission, design.md §1).
func predictionsState(providerPresent, minerRunning bool, predictionCount int) string {
	if !providerPresent || !minerRunning {
		return "unavailable"
	}
	if predictionCount > 0 {
		return "active"
	}
	return "idle"
}

// netState maps the miner status to the Overview network indicator's tri-state.
// "lost" (red) takes precedence over "degraded" (yellow) so a fully-down link
// never renders as merely impaired; a non-running miner (starting/error) also
// reads as "lost" for the network icon.
func netState(status StatusInfo) string {
	switch {
	case status.ConnectionLost || status.Status != StatusRunning:
		return "lost"
	case status.ConnectionDegraded:
		return "degraded"
	default:
		return "ok"
	}
}

func botStatusLabel(tr func(string) string, status MinerStatus) string {
	switch status {
	case StatusRunning:
		return tr("status.running")
	case StatusError:
		return tr("status.error")
	case StatusAuthRequired, StatusAuthWaiting:
		return tr("status.login_required")
	case StatusLoadingStreamers:
		return tr("status.loading")
	default:
		return tr("status.starting")
	}
}

// buildCards enriches every tracked/untracked streamer into an Overview card
// and collects ticker items (active community goals). Ordering mirrors the old
// dashboard: live first (config order), then offline, then untracked.
func (s *Server) buildCards(
	streamers []*models.Streamer,
	slots WatchSlotsView,
	stats map[string]streamerStats,
	predByStreamer map[string]bool,
	tr func(string) string,
) (live, unknown, offline, untracked []StreamerInfo, ticker []TickerItem) {
	watching := slots.Watching

	for _, st := range streamers {
		settings := st.GetSettings()
		status := st.GetStatus()
		online := status == models.StatusOnline
		watchingSlot := watching[st.GetUsername()]
		// A streamer that went online→unknown while still holding a watch slot is
		// shown in the live group as "watching" but flagged unconfirmed (never a
		// red offline); it is not counted in LiveCount.
		slottedUnconfirmed := status == models.StatusUnknown && watchingSlot
		card := StreamerInfo{
			Name:            st.GetUsername(),
			Points:          st.GetChannelPoints(),
			PointsFormatted: util.FormatNumber(st.GetChannelPoints()),
			Status:          status.String(),
			IsLive:          online,
			Unconfirmed:     status == models.StatusUnknown,
			Preference:      string(settings.Preference),
			DisableWatch:    settings.DisableWatch,
		}

		if cs, ok := stats[st.GetUsername()]; ok {
			card.PointsToday = util.FormatNumber(cs.pointsToday)
			if cs.hasRate {
				card.PointsPerHour = util.FormatNumber(cs.pointsPerHour)
			}
		}

		// Last notable event for this streamer from the in-memory ring.
		if text, ago := lastEventFor(tr, st.GetUsername()); text != "" {
			card.LastEventText = text
			card.LastEventAgo = ago
		}

		// Active community goal (also feeds the ticker).
		if goals := st.ActiveCommunityGoals(); len(goals) > 0 {
			g := goals[0]
			card.HasGoal = true
			card.GoalTitle = g.Title
			card.GoalPercent = g.Percent
			ticker = append(ticker, TickerItem{
				Streamer: st.GetUsername(),
				Kind:     "goal",
				Label:    g.Title,
				Percent:  g.Percent,
				HasPct:   true,
			})
		}

		card.HasActivePrediction = predByStreamer[st.GetUsername()]

		if online || slottedUnconfirmed {
			// Confirmed online, or holding a slot during a transient unknown blip.
			// The stream metadata is the last-known value (SetUnknown never clears
			// it), so it is safe to keep showing while unconfirmed.
			card.LiveDuration = util.FormatDuration(time.Since(st.GetOnlineAt()))
			card.GameName = st.Stream.GameName()
			card.Title = st.Stream.GetTitle()
			card.Tags = cardTags(st.Stream.GetTags())
			card.ViewersCount = st.Stream.GetViewersCount()
			card.ViewersCountFormatted = util.FormatNumber(card.ViewersCount)
			card.ChannelRestrictedDrop = st.HasChannelRestrictedCampaign()

			// Watch-streak progress across the bounded pursuit window (0..cap
			// continuously-watched minutes). The denominator is the watcher's hard
			// pursuit cap (StreakPursuitCapMinutes), not a fixed 7 — it tracks the
			// window the boost seat is pursued for, not a guaranteed reward minute.
			if settings.WatchStreak && st.Stream.StreakPending() {
				mins := int(st.Stream.GetMinuteWatched())
				card.StreakPending = true
				card.StreakMinutes = mins
				card.StreakCapMinutes = streakCapMinutes
				card.StreakPercent = streakProgressPercent(mins)
			}

			// Drop progress: only when farming (active campaign + measurable).
			if settings.ClaimDrops {
				if prog := st.ActiveCampaignProgress(); prog != nil {
					card.HasCampaign = true
					card.CampaignName = prog.CampaignName
					card.CampaignDropName = prog.DropName
					card.CampaignPercent = prog.Percent
					if prog.MinutesRequired > 0 {
						card.CampaignMinutesInfo = fmt.Sprintf("%d/%d min", prog.MinutesWatched, prog.MinutesRequired)
					}
				}
			}

			card.WatchReason = slots.Reason[st.GetUsername()]
			switch {
			case settings.DisableWatch:
				card.State = "disabled"
			case watchingSlot:
				card.State = "watching"
				card.Watching = true
			case online:
				card.State = "queued"
				card.Queued = true
			default:
				// Unknown but slotted: keep the "watching" chrome; the Unconfirmed
				// flag drives the distinct "unconfirmed" indication.
				card.State = "watching"
				card.Watching = true
			}
			live = append(live, card)
		} else if status == models.StatusUnknown {
			// Unknown and NOT holding a slot: its own group, never rendered as a red
			// offline card and never shown with an offline duration.
			card.WatchReason = slots.Reason[st.GetUsername()]
			if settings.DisableWatch {
				card.State = "disabled"
			} else {
				card.State = "unknown"
			}
			unknown = append(unknown, card)
		} else {
			// Confirmed offline.
			if off := st.GetOfflineAt(); !off.IsZero() {
				card.OfflineDuration = util.FormatDuration(time.Since(off))
			}
			if settings.DisableWatch {
				card.State = "disabled"
			} else {
				card.State = "offline"
			}
			offline = append(offline, card)
		}
	}

	// Sort ticker by completion desc so the most interesting goals lead.
	sort.SliceStable(ticker, func(i, j int) bool { return ticker[i].Percent > ticker[j].Percent })
	return live, unknown, offline, untracked, ticker
}

// streakCapMinutes is the watch-streak progress-bar denominator: the watcher's
// bounded pursuit cap (20 continuously-watched minutes) surfaced from its single
// backend source of truth, so the UI never hardcodes its own copy and stays in
// step if the cap ever changes. It replaced the obsolete fixed 7-minute
// threshold, which was never the real cap after PR #102.
const streakCapMinutes = int(watcher.StreakPursuitCapMinutes)

// streakProgressPercent maps continuously-watched minutes to a 0..100 progress
// value over the bounded pursuit window (streakCapMinutes). Watching past the cap
// clamps to a full bar rather than overflowing — the window is bounded, and the
// bar reflects pursuit progress, not a guaranteed reward at the cap.
func streakProgressPercent(mins int) int {
	pct := (mins * 100) / streakCapMinutes
	if pct > 100 {
		pct = 100
	}
	return pct
}

// maxCardTags caps how many stream tags a card shows, so a tag-heavy channel
// can't blow up the card layout.
const maxCardTags = 3

// cardTags maps the stream's tags to at most maxCardTags non-empty localized
// names for the card's chip row.
func cardTags(tags []models.Tag) []string {
	var out []string
	for _, tag := range tags {
		if tag.LocalizedName == "" {
			continue
		}
		out = append(out, tag.LocalizedName)
		if len(out) == maxCardTags {
			break
		}
	}
	return out
}

// manualMinBet is Twitch's minimum prediction stake, mirrored on the dashboard
// so the UI never offers a bet the backend would reject. Kept in sync with the
// pool's minPredictionBet.
const manualMinBet = 10

// fmtSeconds renders a seconds count as m:ss for prediction countdowns.
func fmtSeconds(sec int) string {
	if sec < 0 {
		sec = 0
	}
	return fmt.Sprintf("%d:%02d", sec/60, sec%60)
}

// lastEventFor returns a short human summary + relative time of the most recent
// event recorded for the given streamer, or ("","") if none.
func lastEventFor(tr func(string) string, username string) (text, ago string) {
	for _, e := range events.Recent(200) {
		if !strings.EqualFold(e.Streamer, username) {
			continue
		}
		return eventLabel(tr, e), util.FormatDuration(time.Since(e.Time)) + " " + tr("common.ago")
	}
	return "", ""
}

// eventTypeKeys maps each event type to its localization key. The Detail suffix
// (streamer/game-specific) is appended verbatim and not translated.
var eventTypeKeys = map[events.Type]string{
	events.TypeStreamerOnline:  "event.went_live",
	events.TypeStreamerOffline: "event.went_offline",
	events.TypeBonusClaimed:    "event.bonus_claimed",
	events.TypePointsEarned:    "event.points_earned",
	events.TypeBetPlaced:       "event.bet_placed",
	events.TypeBetResult:       "event.bet_result",
	events.TypeDropClaimed:     "event.drop_claimed",
	events.TypeMomentClaimed:   "event.moment_claimed",
	events.TypeRaidJoined:      "event.raid_joined",
	events.TypeRewardRedeemed:  "event.reward_redeemed",
}

// eventLabel maps an event to a compact, localized card label.
func eventLabel(tr func(string) string, e events.Event) string {
	label := string(e.Type)
	if key, ok := eventTypeKeys[e.Type]; ok {
		label = tr(key)
	}
	if e.Detail != "" {
		return label + " · " + e.Detail
	}
	return label
}

// buildNowWatching builds the pinned sidebar "Now Watching" block from the
// active watch slots.
func (s *Server) buildNowWatching(
	streamers []*models.Streamer,
	slots WatchSlotsView,
	stats map[string]streamerStats,
	stale bool,
) NowWatchingView {
	byName := make(map[string]*models.Streamer, len(streamers))
	for _, st := range streamers {
		byName[st.GetUsername()] = st
	}

	view := NowWatchingView{Mode: slots.Mode, Stale: stale}

	// Watched streamers first (stable by ActivePair order, then any extras).
	seen := map[string]bool{}
	order := append([]string(nil), slots.ActivePair...)
	for name := range slots.Watching {
		found := false
		for _, n := range order {
			if n == name {
				found = true
				break
			}
		}
		if !found {
			order = append(order, name)
		}
	}

	for _, name := range order {
		if !slots.Watching[name] {
			continue
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		slot := WatchSlotView{Name: name, Origin: slots.Origin[name]}

		st := byName[name]
		if st == nil {
			// A discovery-occupied slot: the channel is not on the configured
			// streamer list, so only the name, its game, and the discovery
			// badge are available (no points/streak/gain history).
			slot.Game = slots.Games[name]
			view.Slots = append(view.Slots, slot)
			continue
		}

		slot.Points = util.FormatNumber(st.GetChannelPoints())
		slot.Game = st.Stream.GameName()
		if st.GetSettings().WatchStreak && st.Stream.StreakPending() {
			mins := int(st.Stream.GetMinuteWatched())
			slot.StreakPending = true
			slot.StreakMinutes = mins
			slot.StreakCapMinutes = streakCapMinutes
			slot.StreakPercent = streakProgressPercent(mins)
		}
		if cs, ok := stats[name]; ok && cs.hasRate {
			slot.HasGain = true
			slot.GainPerHour = util.FormatNumber(cs.pointsPerHour)
		}
		view.Slots = append(view.Slots, slot)
	}

	// Queued = online, not watched, not disabled.
	for _, st := range streamers {
		if !st.GetIsOnline() || slots.Watching[st.GetUsername()] || st.GetSettings().DisableWatch {
			continue
		}
		view.QueuedNames = append(view.QueuedNames, st.GetUsername())
	}

	if !slots.NextRotationAt.IsZero() && len(view.Slots) > 0 {
		view.HasNextRotation = true
		view.NextRotationUnix = slots.NextRotationAt.Unix()
	}
	return view
}

// buildPredictionViews maps live predictions to board view models.
func buildPredictionViews(preds []LivePrediction) []PredictionView {
	views := make([]PredictionView, 0, len(preds))
	now := time.Now()
	for _, p := range preds {
		secondsLeft := int(p.PredictionWindowSeconds - now.Sub(p.CreatedAt).Seconds())
		if secondsLeft < 0 {
			secondsLeft = 0
		}
		locked := p.Status == string(models.PredictionLocked)
		// A manual bet is offered only when the round is genuinely bettable:
		// open, still within its window, the streamer online, no bet placed yet,
		// and the balance covers the minimum stake. This is a UI gate only — the
		// backend re-checks all of it before actually placing.
		manualAllowed := !locked &&
			p.Status == string(models.PredictionActive) &&
			!p.BetPlaced &&
			p.Online &&
			secondsLeft > 0 &&
			p.Balance >= manualMinBet
		pv := PredictionView{
			Streamer:         p.Streamer,
			Title:            p.Title,
			Status:           p.Status,
			Locked:           locked,
			SecondsLeft:      secondsLeft,
			SecondsLeftLabel: fmtSeconds(secondsLeft),
			BetPlaced:        p.BetPlaced,
			BetConfirmed:     p.BetConfirmed,
			WindowEndUnix:    p.CreatedAt.Add(time.Duration(p.PredictionWindowSeconds) * time.Second).Unix(),
			EventID:          p.EventID,
			ManualAllowed:    manualAllowed,
			ManualBet:        p.ManualBet,
			BetOutcomeTitle:  p.BetOutcomeTitle,
			AutoBetSkipped:   p.AutoBetSkipped,
			SkipUndoable:     p.AutoBetSkipped && !p.BetPlaced && p.Status == string(models.PredictionActive) && secondsLeft > 0,
			ManualPending:    p.ManualPending,
			ManualError:      p.ManualError,
			Balance:          p.Balance,
			BalanceLabel:     util.FormatNumber(p.Balance),
			MinBet:           manualMinBet,
		}
		if p.BetPlaced && p.BetAmount > 0 {
			pv.BetAmount = util.FormatNumber(p.BetAmount)
		}
		if p.TotalPoints > 0 {
			pv.PoolLabel = util.FormatNumber(p.TotalPoints)
		}
		for _, o := range p.Outcomes {
			ov := PredictionOutcomeView{
				ID:         o.ID,
				Title:      o.Title,
				Color:      o.Color,
				Percent:    int(o.PercentageUsers + 0.5),
				Chosen:     o.Chosen,
				Selectable: manualAllowed && o.ID != "",
			}
			if o.Odds > 0 {
				ov.Odds = fmt.Sprintf("%.2fx", o.Odds)
			}
			if o.TotalPoints > 0 {
				ov.PointsLabel = util.FormatNumber(o.TotalPoints)
			}
			pv.Outcomes = append(pv.Outcomes, ov)
		}
		views = append(views, pv)
	}
	// Soonest-closing first.
	sort.SliceStable(views, func(i, j int) bool { return views[i].SecondsLeft < views[j].SecondsLeft })
	return views
}

// handleAPIOverviewEvents returns the recent events for one streamer, rendered
// as the card drawer partial. Reads the in-memory ring buffer only.
func (s *Server) handleAPIOverviewEvents(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/overview/events/")
	if name == "" {
		writeBadRequest(w, "Streamer not specified")
		return
	}

	type eventRow struct {
		Label string
		Ago   string
	}
	tr := func(key string) string { return s.i18n.T(s.langFromRequest(r), key) }
	var rows []eventRow
	for _, e := range events.Recent(200) {
		if !strings.EqualFold(e.Streamer, name) {
			continue
		}
		rows = append(rows, eventRow{Label: eventLabel(tr, e), Ago: util.FormatDuration(time.Since(e.Time)) + " " + tr("common.ago")})
		if len(rows) >= 20 {
			break
		}
	}

	s.renderPartial(w, r, "events_drawer", map[string]interface{}{
		"Name":   name,
		"Events": rows,
	})
}
