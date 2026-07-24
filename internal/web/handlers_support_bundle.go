package web

import (
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/debug"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/notifications"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/supportbundle"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/version"
)

// SupportBundlePath is the dashboard route serving the redacted support
// bundle: a downloadable ZIP of the miner's operational state, built entirely
// from an in-memory, typed allowlist (internal/supportbundle) for sharing
// with a support request. Unlike the debug snapshot, this route is wired
// UNCONDITIONALLY (not gated on Debug.Enabled) and additionally requires
// REAL dashboard authentication on every request, regardless of the global
// auth middleware's state - see requireRealDashboardAuth.
const SupportBundlePath = "/api/support-bundle"

// bundleSlots is a small, package-level, non-blocking concurrency limiter: at
// most two support bundles are ever being built at once. A generic 503 is
// returned instead of queuing, since bundle generation is a bounded,
// in-memory, sub-second operation - there is nothing worth waiting for.
var bundleSlots = make(chan struct{}, 2)

// SetSupportBundleSource wires the miner's in-process debug-snapshot builder
// as the support bundle's data source. Wired UNCONDITIONALLY (regardless of
// Debug.Enabled): the support bundle is an always-available diagnostic tool,
// not a debug-mode feature. A nil source (never wired, e.g. in tests) is
// handled gracefully by producing a bundle with empty operational sections
// rather than failing - see handleSupportBundle.
func (s *Server) SetSupportBundleSource(fn func() debug.Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.supportBundleSource = fn
}

// setSupportBundleClock is a test seam overriding the clock Build uses for
// generatedAt/the filename. Unexported: only tests in this package need it;
// production always leaves it nil (supportbundle.Build then defaults to
// time.Now().UTC()).
func (s *Server) setSupportBundleClock(fn func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.supportBundleClock = fn
}

// requireRealDashboardAuth reports whether this specific request carries
// valid HTTP Basic Auth credentials AND the dashboard actually has
// authentication configured. It is deliberately self-contained - independent
// of (in addition to, never a replacement for) the global basicAuthMiddleware
// wired in handler() - because that middleware is skipped entirely under
// DASHBOARD_INSECURE_NO_AUTH=true (authEnabled()==false), and the support
// bundle must NEVER be reachable in that mode: an insecure-bypass dashboard
// serves every other route unauthenticated, but a bundle download is
// sensitive enough to demand real credentials every time, with no exceptions.
func requireRealDashboardAuth(r *http.Request) bool {
	if !authEnabled() {
		return false
	}
	expectedUser, expectedPass := getAuthCredentials()
	user, pass, ok := r.BasicAuth()
	if !ok {
		return false
	}
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(expectedUser)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(expectedPass)) == 1
	return userOK && passOK
}

// handleSupportBundle serves the redacted support bundle ZIP. GET-only;
// requires real dashboard authentication (see requireRealDashboardAuth) on
// every request regardless of the global middleware's state; bounded to two
// concurrent builds; never touches disk, the database, or the network - the
// entire response is built in memory from an allowlisted snapshot.
func (s *Server) handleSupportBundle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	if !requireRealDashboardAuth(r) {
		if !authEnabled() {
			// No real auth configured (disabled OR the explicit insecure
			// bypass): fail closed with 404, exactly like the debug
			// snapshot route, so the endpoint's existence isn't even
			// distinguishable from "not found" without credentials.
			http.NotFound(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="Twitch Miner Dashboard"`)
		writeError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	select {
	case bundleSlots <- struct{}{}:
		defer func() { <-bundleSlots }()
	default:
		writeServiceUnavailable(w, "support bundle generation is busy, try again shortly")
		return
	}

	s.mu.RLock()
	source := s.supportBundleSource
	clock := s.supportBundleClock
	s.mu.RUnlock()

	snap, err := safeSupportBundleSnapshot(source)
	if err != nil {
		slog.Error("Support bundle: snapshot provider panicked", "error", err)
		writeInternalError(w, "failed to build support bundle")
		return
	}

	in := s.buildSupportBundleInput(snap)
	result, err := supportbundle.Build(in, supportbundle.Options{Now: clock})
	if err != nil {
		slog.Error("Failed to build support bundle", "error", err)
		writeInternalError(w, "failed to build support bundle")
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, result.Filename))
	w.Header().Set("Cache-Control", "no-store, private")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(result.Bytes)
}

// safeSupportBundleSnapshot calls fn and recovers a panic into an error, the
// same convention marshalDebugSnapshot uses for the debug-snapshot route. A
// nil fn (source never wired) is NOT an error - it yields the zero Snapshot,
// so the bundle still builds successfully with empty operational sections.
func safeSupportBundleSnapshot(fn func() debug.Snapshot) (snap debug.Snapshot, err error) {
	if fn == nil {
		return debug.Snapshot{}, nil
	}
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("support bundle snapshot provider panicked: %v", p)
		}
	}()
	return fn(), nil
}

// activeClientAllowlist is the small, bounded set of GQL client labels
// api.TwitchClient.ActiveClientID ever returns ("TV", "Browser", "Mobile", or
// "Unknown" - see internal/api/client.go). Anything else (there shouldn't be
// anything else, but the allowlist boundary never trusts that) is dropped
// rather than passed through.
var activeClientAllowlist = map[string]bool{
	"TV":      true,
	"Browser": true,
	"Mobile":  true,
	"Unknown": true,
}

// buildSupportBundleInput is the field-by-field allowlist mapping from
// debug.Snapshot (plus a few web-level facts) into supportbundle.Input. This
// is the ONE place that decides what leaves the process in a support bundle:
// every field copied here was deliberately chosen; everything else on
// debug.Snapshot - Username (the authenticated account login), StatusDetail,
// every *.Detail, DropsSyncInfo.LastError, StreamerState.Title, RecentEvents,
// ChannelPoints - is simply never read from snap, so a field added to
// debug.Snapshot tomorrow does not appear in a bundle until someone
// deliberately wires it in here.
func (s *Server) buildSupportBundleInput(snap debug.Snapshot) supportbundle.Input {
	s.mu.RLock()
	notifMgr := s.notificationManager
	s.mu.RUnlock()

	in := supportbundle.Input{
		AppVersion:    version.Version,
		GoVersion:     runtime.Version(),
		OS:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		UptimeSeconds: snap.UptimeSeconds,
		MinerStatus:   snap.Status,
		Runtime:       buildSupportBundleRuntimeInfo(snap, notifMgr),
		Watching:      buildSupportBundleWatching(snap),
		Journals:      buildSupportBundleJournals(snap),
	}
	if snap.Health != nil {
		in.Health = buildSupportBundleHealth(snap.Health)
	}
	if snap.Drops != nil {
		in.Drops = buildSupportBundleDrops(snap)
	}
	return in
}

// buildSupportBundleRuntimeInfo derives the dashboard/runtime-facts section.
// Every value here is either a small bounded enum, a count, or a boolean -
// never a config value, a secret, or free text.
func buildSupportBundleRuntimeInfo(snap debug.Snapshot, notifMgr *notifications.Manager) supportbundle.RuntimeInfo {
	authMode := "disabled"
	switch {
	case authEnabled():
		authMode = "authenticated"
	case insecureNoAuthAllowed():
		authMode = "insecure_bypass"
	}

	var notifEnabled, notifConfigValid bool
	var providers []string
	if notifMgr != nil {
		notifEnabled = notifMgr.IsEnabled()
		if notifMgr.HasAnyProvider() {
			// Only provider today is Discord (see internal/notifications);
			// the manager exposes no per-provider name enumeration, so this
			// intentionally names the one provider that can be present
			// rather than reaching for an unexported field.
			providers = []string{"discord"}
		}
		notifConfigValid, _ = notifMgr.IsConfigValid()
	}

	discoveryEnabled := snap.Discovery != nil
	discoveredChannels := 0
	if snap.Discovery != nil {
		discoveredChannels = len(snap.Discovery.Channels)
	}

	// dropsTracked answers "is at least one campaign currently tracked" -
	// the drops tracker itself is always constructed, so a bare non-nil
	// Drops section would always be true and uninformative.
	dropsTracked := snap.Drops != nil && snap.Drops.TrackedCampaigns > 0
	progressWatchdogEnabled := snap.ProgressWatchdog != nil && snap.ProgressWatchdog.Enabled
	// policyEngineActive answers "did the policy engine rank any campaign
	// this cycle" - Policy is only populated when there are decisions.
	policyEngineActive := snap.Policy != nil

	campaignSyncMinutes := 0
	if snap.Drops != nil {
		campaignSyncMinutes = snap.Drops.IntervalMinutes
	}

	return supportbundle.RuntimeInfo{
		DashboardAuthMode: authMode,
		FeatureFlags: supportbundle.FeatureFlags{
			DiscoveryEnabled:        discoveryEnabled,
			DropsTracked:            dropsTracked,
			ProgressWatchdogEnabled: progressWatchdogEnabled,
			PolicyEngineActive:      policyEngineActive,
			NotificationsEnabled:    notifEnabled,
		},
		Intervals: supportbundle.Intervals{
			CampaignSyncMinutes:  campaignSyncMinutes,
			WatchTimeWindowHours: snap.Watching.WatchTimeWindowHours,
		},
		Counts: supportbundle.Counts{
			ConfiguredStreamers: len(snap.Streamers),
			DiscoveredChannels:  discoveredChannels,
		},
		Notifications: supportbundle.NotificationsInfo{
			Enabled:     notifEnabled,
			Providers:   providers,
			ConfigValid: notifConfigValid,
		},
	}
}

// buildSupportBundleHealth maps debug.HealthInfo into the bundle's health
// section. ActiveClientID is allowlist-filtered; Detail is never read (it
// doesn't even exist on debug.HealthSignal - see the final report's
// spec-discrepancy note on durationMillis).
func buildSupportBundleHealth(h *debug.HealthInfo) *supportbundle.HealthSection {
	out := &supportbundle.HealthSection{}
	if activeClientAllowlist[h.ActiveClientID] {
		out.ActiveClient = h.ActiveClientID
	}
	for _, sig := range h.Signals {
		out.Signals = append(out.Signals, supportbundle.HealthSignal{
			Name:      sig.Name,
			Status:    sig.Status,
			CheckedAt: sig.CheckedAt,
			Stage:     sig.Stage,
			ErrorCode: sig.ErrorCode,
		})
	}
	return out
}

// buildSupportBundleWatching maps the watch-slot/rotation view. Notably
// absent versus debug.StreamerState: Title, ChannelPoints, OnlineSince/
// OfflineSince, DropCampaigns, ActivePrediction, WatchStreak - the support
// bundle's streamer entries are deliberately narrower (see
// supportbundle.StreamerEntry's doc comment).
func buildSupportBundleWatching(snap debug.Snapshot) supportbundle.WatchingSection {
	w := supportbundle.WatchingSection{
		Mode:                 snap.Watching.Mode,
		EvaluatedAt:          snap.Watching.EvaluatedAt,
		WatchTimeWindowHours: snap.Watching.WatchTimeWindowHours,
	}
	for _, sl := range snap.Watching.Slots {
		w.Slots = append(w.Slots, supportbundle.WatchSlot{
			Slot:       sl.Slot,
			Channel:    sl.Channel,
			Source:     sl.Source,
			ReasonCode: sl.ReasonCode,
			Reason:     sl.Reason,
			Campaign:   sl.Campaign,
		})
	}
	for _, wc := range snap.Watching.Waiting {
		w.Waiting = append(w.Waiting, supportbundle.WaitingSlot{
			Channel:    wc.Channel,
			Source:     wc.Source,
			ReasonCode: wc.ReasonCode,
			Reason:     wc.Reason,
		})
	}
	for _, st := range snap.Streamers {
		w.Streamers = append(w.Streamers, supportbundle.StreamerEntry{
			Channel:              st.Username,
			Status:               st.Status,
			StatusReason:         st.StatusReason,
			Watching:             st.Watching,
			Reason:               st.Reason,
			WatchedMinutesWindow: st.WatchedMinutesWindow,
			HasBroadcastID:       st.BroadcastID != "",
			Game:                 st.Game,
		})
	}
	if snap.PubSub != nil {
		w.PubSub.TotalTopics = snap.PubSub.TotalTopics
		for _, c := range snap.PubSub.Connections {
			w.PubSub.Connections = append(w.PubSub.Connections, supportbundle.PubSubConn{
				Index:        c.Index,
				Topics:       c.Topics,
				LastPong:     c.LastPong,
				Reconnecting: c.Reconnecting,
				Closed:       c.Closed,
			})
		}
	}
	return w
}

// buildSupportBundleDrops maps the drops sync/progress/policy view.
// DropsSyncInfo.LastError (a raw, potentially arbitrary error string) is
// deliberately never read - only the derived LastSyncFailed bool crosses the
// boundary.
func buildSupportBundleDrops(snap debug.Snapshot) *supportbundle.DropsSection {
	d := snap.Drops
	out := &supportbundle.DropsSection{
		SyncStatus: supportbundle.DropsSyncStatus{
			LastSyncAt:             d.LastSyncAt,
			LastSuccessAt:          d.LastSuccessAt,
			IntervalMinutes:        d.IntervalMinutes,
			SyncRuns:               d.SyncRuns,
			DashboardCampaigns:     d.DashboardCampaigns,
			TrackedCampaigns:       d.TrackedCampaigns,
			RecoveredFromInventory: d.RecoveredFromInventory,
			FilteredByBlacklist:    d.FilteredByBlacklist,
			FilteredByGame:         d.FilteredByGame,
			LastSyncFailed:         d.LastError != "",
			Revision:               d.Revision,
			BackendUpdatedAt:       d.BackendUpdatedAt,
			UpdateSource:           d.UpdateSource,
		},
	}
	for _, c := range d.Campaigns {
		out.Campaigns = append(out.Campaigns, supportbundle.DropCampaign{
			Name:              c.Name,
			Game:              c.Game,
			GameID:            c.GameID,
			EndAt:             c.EndAt,
			RemainingDrops:    c.RemainingDrops,
			OverallPercent:    c.OverallPercent,
			ClaimStatus:       c.ClaimStatus,
			ChannelRestricted: c.ChannelRestricted,
			InInventory:       c.InInventory,
		})
	}

	if snap.ProgressWatchdog != nil {
		pw := &supportbundle.ProgressWatchdogSection{
			Enabled:     snap.ProgressWatchdog.Enabled,
			EvaluatedAt: snap.ProgressWatchdog.EvaluatedAt,
		}
		for _, p := range snap.ProgressWatchdog.Drops {
			pw.Drops = append(pw.Drops, supportbundle.DropProgress{
				Campaign:             p.Campaign,
				Drop:                 p.Drop,
				Channel:              p.Channel,
				Status:               p.Status,
				LastMinutes:          p.LastMinutes,
				LastProgressAt:       p.LastProgressAt,
				ReportsSinceProgress: p.ReportsSinceProgress,
				NoProgressObs:        p.NoProgressObs,
				RecoveryStage:        p.RecoveryStage,
				RecoveryStageName:    p.RecoveryStageName,
				LastRecoveryAt:       p.LastRecoveryAt,
			})
		}
		for _, a := range snap.ProgressWatchdog.Avoided {
			pw.Avoided = append(pw.Avoided, supportbundle.AvoidedChannel{
				Login:  a.Login,
				Until:  a.Until,
				Reason: a.Reason,
			})
		}
		out.ProgressWatchdog = pw
	}

	if snap.Policy != nil {
		pol := &supportbundle.PolicySection{Mode: snap.Policy.Mode}
		for _, dd := range snap.Policy.Decisions {
			pd := supportbundle.PolicyDecision{
				Campaign:      dd.Campaign,
				Status:        dd.Status,
				Total:         dd.Total,
				Excluded:      dd.Excluded,
				ExcludeReason: dd.ExcludeReason,
				Feasibility: supportbundle.PolicyFeasibility{
					MinutesToNextReward:   dd.Feasibility.MinutesToNextReward,
					MinutesToCompleteAll:  dd.Feasibility.MinutesToCompleteAll,
					CanCompleteNextReward: dd.Feasibility.CanCompleteNextReward,
					CanCompleteAll:        dd.Feasibility.CanCompleteAll,
				},
			}
			for _, f := range dd.Factors {
				pd.Factors = append(pd.Factors, supportbundle.PolicyFactor{Label: f.Label, Points: f.Points})
			}
			pol.Decisions = append(pol.Decisions, pd)
		}
		out.Policy = pol
	}

	return out
}

// buildSupportBundleJournals maps the bounded diagnostic journals
// (BKM-013/BKM-014) field-for-field. snap.Journal is nil whenever neither
// journal has ever recorded anything (see (*Miner).BuildDebugSnapshot).
func buildSupportBundleJournals(snap debug.Snapshot) supportbundle.JournalsSection {
	var out supportbundle.JournalsSection
	if snap.Journal == nil {
		return out
	}
	for _, rec := range snap.Journal.Slots {
		e := rec.Event
		out.Slots = append(out.Slots, supportbundle.SlotEventRecord{
			Seq:              rec.Seq,
			At:               rec.At,
			Type:             string(e.Type),
			Channel:          e.Channel,
			ChannelID:        e.ChannelID,
			Broadcast:        e.Broadcast,
			Origin:           e.Origin,
			SlotIndex:        e.SlotIndex,
			Reason:           e.Reason,
			PrevReason:       e.PrevReason,
			Victim:           e.Victim,
			VictimID:         e.VictimID,
			ResidenceSeconds: e.ResidenceSeconds,
			Successes:        e.Successes,
			Failures:         e.Failures,
			Stage:            e.Stage,
			Status:           e.Status,
			ErrorCode:        e.ErrorCode,
			ResetReason:      e.ResetReason,
		})
	}
	for _, rec := range snap.Journal.Health {
		e := rec.Event
		out.Health = append(out.Health, supportbundle.HealthEventRecord{
			Seq:                   rec.Seq,
			At:                    rec.At,
			Type:                  string(e.Type),
			Domain:                e.Domain,
			PrevLevel:             e.PrevLevel,
			NewLevel:              e.NewLevel,
			APIState:              e.APIState,
			PubSubDown:            e.PubSubDown,
			PubSubDegraded:        e.PubSubDegraded,
			Evidence:              e.Evidence,
			Recovery:              e.Recovery,
			Reason:                e.Reason,
			NotificationRequested: e.NotificationRequested,
			SuppressedDuplicates:  e.SuppressedDuplicates,
		})
	}
	return out
}
