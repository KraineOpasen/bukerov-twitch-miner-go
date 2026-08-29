package supportbundle

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// filenameLayout renders the bundle filename's timestamp component:
// bukerov-support-YYYYMMDDTHHMMSSZ.zip. Always UTC (opts.Now().UTC()), so the
// trailing "Z" is always accurate.
const filenameLayout = "20060102T150405Z"

// Build assembles the deterministic, redacted support-bundle ZIP from in. It
// touches no network, filesystem, database, or live application state -
// every value it writes came in on Input, was passed through Redact, and was
// capped at its section's documented bound. Given the same Input and the
// same Options.Now, Build's output is byte-for-byte reproducible.
func Build(in Input, opts Options) (Result, error) {
	clock := time.Now
	if opts.Now != nil {
		clock = opts.Now
	}
	generatedAt := clock().UTC()

	names := entryNames(in)
	b := &builder{truncations: map[string]Truncation{}}

	runtimeDocV := buildRuntime(in.Runtime)
	watchingDocV := b.buildWatching(in.Watching)
	slotsDocV, healthJournalDocV := b.buildJournals(in.Journals)

	var healthDocV healthDoc
	if in.Health != nil {
		healthDocV = b.buildHealth(in.Health)
	}
	var dropsDocV *dropsDoc
	if in.Drops != nil {
		dropsDocV = b.buildDrops(in.Drops)
	}

	manifest := manifestDoc{
		SchemaVersion:       schemaVersion,
		BundleFormatVersion: bundleFormatVersion,
		GeneratedAt:         generatedAt,
		AppVersion:          Redact(in.AppVersion),
		GoVersion:           Redact(in.GoVersion),
		OS:                  Redact(in.OS),
		Arch:                Redact(in.Arch),
		UptimeSeconds:       in.UptimeSeconds,
		MinerStatus:         Redact(in.MinerStatus),
		IncludedFiles:       names,
		Truncations:         b.truncations,
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	var totalUncompressed int64
	writeBytes := func(name string, data []byte) error {
		if len(data) > maxSectionBytes {
			return fmt.Errorf("supportbundle: section %q exceeds the %d byte bound", name, maxSectionBytes)
		}
		totalUncompressed += int64(len(data))
		if totalUncompressed > maxTotalUncompressedBytes {
			return fmt.Errorf("supportbundle: bundle exceeds the %d byte uncompressed bound", maxTotalUncompressedBytes)
		}
		hdr := &zip.FileHeader{
			Name:     name,
			Method:   zip.Deflate,
			Modified: generatedAt,
		}
		// Explicit, non-executable regular-file mode: no symlink bit, no
		// exec bits, regardless of the zip package's zero-value default.
		hdr.SetMode(0o644)
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return fmt.Errorf("supportbundle: create entry %q: %w", name, err)
		}
		if _, err := w.Write(data); err != nil {
			return fmt.Errorf("supportbundle: write entry %q: %w", name, err)
		}
		return nil
	}
	writeJSON := func(name string, v any) error {
		data, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return fmt.Errorf("supportbundle: marshal %q: %w", name, err)
		}
		return writeBytes(name, data)
	}

	if err := writeJSON("manifest.json", manifest); err != nil {
		return Result{}, err
	}
	if err := writeJSON("runtime.json", runtimeDocV); err != nil {
		return Result{}, err
	}
	if in.Health != nil {
		if err := writeJSON("health.json", healthDocV); err != nil {
			return Result{}, err
		}
	}
	if err := writeJSON("watching.json", watchingDocV); err != nil {
		return Result{}, err
	}
	if in.Drops != nil {
		if err := writeJSON("drops.json", dropsDocV); err != nil {
			return Result{}, err
		}
	}
	if err := writeJSON("journals/slots.json", slotsDocV); err != nil {
		return Result{}, err
	}
	if err := writeJSON("journals/health.json", healthJournalDocV); err != nil {
		return Result{}, err
	}
	if err := writeBytes("README.txt", buildReadme(generatedAt)); err != nil {
		return Result{}, err
	}

	if err := zw.Close(); err != nil {
		return Result{}, fmt.Errorf("supportbundle: close zip: %w", err)
	}
	if buf.Len() > maxZipBytes {
		return Result{}, fmt.Errorf("supportbundle: zip exceeds the %d byte bound", maxZipBytes)
	}

	return Result{
		Bytes:    buf.Bytes(),
		Filename: fmt.Sprintf("bukerov-support-%s.zip", generatedAt.Format(filenameLayout)),
	}, nil
}

// entryNames is the constant, ordered list of ZIP entries this Input will
// produce - computed up front (before anything is written) purely from which
// optional sections are present, so manifest.json (the FIRST entry written)
// can accurately list every file in the archive, including itself.
func entryNames(in Input) []string {
	names := []string{"manifest.json", "runtime.json"}
	if in.Health != nil {
		names = append(names, "health.json")
	}
	names = append(names, "watching.json")
	if in.Drops != nil {
		names = append(names, "drops.json")
	}
	names = append(names, "journals/slots.json", "journals/health.json", "README.txt")
	return names
}

// builder accumulates the manifest-wide truncation summary (keyed
// "file.section", e.g. "watching.streamers") while each section is built.
type builder struct {
	truncations map[string]Truncation
}

// note records tr in the manifest-wide summary under key, but only when a
// truncation actually happened - an untruncated section adds no clutter.
func (b *builder) note(key string, tr Truncation) {
	if tr.Truncated {
		b.truncations[key] = tr
	}
}

// addIfTruncated is note's per-file-document counterpart: each JSON entry
// also carries its OWN "truncated" object scoped to just that file's
// sections, so a reader doesn't have to cross-reference the manifest.
func addIfTruncated(m map[string]Truncation, key string, tr Truncation) {
	if tr.Truncated {
		m[key] = tr
	}
}

func buildRuntime(r RuntimeInfo) runtimeDoc {
	providers := make([]string, 0, len(r.Notifications.Providers))
	for _, p := range r.Notifications.Providers {
		providers = append(providers, Redact(p))
	}
	return runtimeDoc{
		DashboardAuthMode: Redact(r.DashboardAuthMode),
		FeatureFlags: featureFlagsDoc{
			DiscoveryEnabled:        r.FeatureFlags.DiscoveryEnabled,
			DropsTracked:            r.FeatureFlags.DropsTracked,
			ProgressWatchdogEnabled: r.FeatureFlags.ProgressWatchdogEnabled,
			PolicyEngineActive:      r.FeatureFlags.PolicyEngineActive,
			NotificationsEnabled:    r.FeatureFlags.NotificationsEnabled,
		},
		Intervals: intervalsDoc{
			CampaignSyncMinutes:  r.Intervals.CampaignSyncMinutes,
			WatchTimeWindowHours: r.Intervals.WatchTimeWindowHours,
		},
		Counts: countsDoc{
			ConfiguredStreamers: r.Counts.ConfiguredStreamers,
			DiscoveredChannels:  r.Counts.DiscoveredChannels,
		},
		Notifications: notificationsDoc{
			Enabled:     r.Notifications.Enabled,
			Providers:   providers,
			ConfigValid: r.Notifications.ConfigValid,
		},
	}
}

func (b *builder) buildHealth(h *HealthSection) healthDoc {
	signals := make([]healthSignalDoc, 0, len(h.Signals))
	for _, s := range h.Signals {
		signals = append(signals, healthSignalDoc{
			Name:      Redact(s.Name),
			Status:    Redact(s.Status),
			CheckedAt: s.CheckedAt,
			Stage:     Redact(s.Stage),
			ErrorCode: Redact(s.ErrorCode),
		})
	}
	return healthDoc{ActiveClient: Redact(h.ActiveClient), Signals: signals}
}

func (b *builder) buildWatching(w WatchingSection) watchingDoc {
	slots, trSlots := truncateSlice(w.Slots, maxStreamers)
	b.note("watching.slots", trSlots)
	waiting, trWaiting := truncateSlice(w.Waiting, maxStreamers)
	b.note("watching.waiting", trWaiting)
	streamers, trStreamers := truncateSlice(w.Streamers, maxStreamers)
	b.note("watching.streamers", trStreamers)
	conns, trConns := truncateSlice(w.PubSub.Connections, maxPubSubConns)
	b.note("watching.pubsubConnections", trConns)

	slotDocs := make([]watchSlotDoc, 0, len(slots))
	for _, s := range slots {
		slotDocs = append(slotDocs, watchSlotDoc{
			Slot:       s.Slot,
			Channel:    Redact(s.Channel),
			Source:     Redact(s.Source),
			ReasonCode: Redact(s.ReasonCode),
			Campaign:   Redact(s.Campaign),
		})
	}
	waitingDocs := make([]waitingSlotDoc, 0, len(waiting))
	for _, s := range waiting {
		waitingDocs = append(waitingDocs, waitingSlotDoc{
			Channel:    Redact(s.Channel),
			Source:     Redact(s.Source),
			ReasonCode: Redact(s.ReasonCode),
			Reason:     Redact(s.Reason),
		})
	}
	streamerDocs := make([]streamerEntryDoc, 0, len(streamers))
	for _, s := range streamers {
		streamerDocs = append(streamerDocs, streamerEntryDoc{
			Channel:              Redact(s.Channel),
			Status:               Redact(s.Status),
			StatusReason:         Redact(s.StatusReason),
			Watching:             s.Watching,
			WatchedMinutesWindow: s.WatchedMinutesWindow,
			HasBroadcastID:       s.HasBroadcastID,
			Game:                 Redact(s.Game),
		})
	}
	connDocs := make([]pubSubConnDoc, 0, len(conns))
	for _, c := range conns {
		// PubSubConn carries no strings needing Redact - every field is a
		// plain int/bool/time.Time, so a direct type conversion is equivalent
		// to (and clearer than) a field-by-field literal copy.
		connDocs = append(connDocs, pubSubConnDoc(c))
	}

	truncated := map[string]Truncation{}
	addIfTruncated(truncated, "slots", trSlots)
	addIfTruncated(truncated, "waiting", trWaiting)
	addIfTruncated(truncated, "streamers", trStreamers)
	addIfTruncated(truncated, "pubsubConnections", trConns)

	return watchingDoc{
		Mode:                 Redact(w.Mode),
		EvaluatedAt:          w.EvaluatedAt,
		WatchTimeWindowHours: w.WatchTimeWindowHours,
		Slots:                slotDocs,
		Waiting:              waitingDocs,
		Streamers:            streamerDocs,
		PubSub:               pubSubDoc{TotalTopics: w.PubSub.TotalTopics, Connections: connDocs},
		Truncated:            truncated,
	}
}

func (b *builder) buildDrops(d *DropsSection) *dropsDoc {
	campaigns, trCampaigns := truncateSlice(d.Campaigns, maxDropCampaigns)
	b.note("drops.campaigns", trCampaigns)

	campaignDocs := make([]dropCampaignDoc, 0, len(campaigns))
	for _, c := range campaigns {
		campaignDocs = append(campaignDocs, dropCampaignDoc{
			Name:              Redact(c.Name),
			Game:              Redact(c.Game),
			GameID:            Redact(c.GameID),
			EndAt:             c.EndAt,
			RemainingDrops:    c.RemainingDrops,
			OverallPercent:    c.OverallPercent,
			ClaimStatus:       Redact(c.ClaimStatus),
			ChannelRestricted: c.ChannelRestricted,
			InInventory:       c.InInventory,
		})
	}

	truncated := map[string]Truncation{}
	addIfTruncated(truncated, "campaigns", trCampaigns)

	doc := &dropsDoc{
		SyncStatus: dropsSyncStatusDoc{
			LastSyncAt:                  d.SyncStatus.LastSyncAt,
			LastSuccessAt:               d.SyncStatus.LastSuccessAt,
			IntervalMinutes:             d.SyncStatus.IntervalMinutes,
			SyncRuns:                    d.SyncStatus.SyncRuns,
			DashboardCampaigns:          d.SyncStatus.DashboardCampaigns,
			DashboardListingUnavailable: d.SyncStatus.DashboardListingUnavailable,
			TrackedCampaigns:            d.SyncStatus.TrackedCampaigns,
			RecoveredFromInventory:      d.SyncStatus.RecoveredFromInventory,
			FilteredByBlacklist:         d.SyncStatus.FilteredByBlacklist,
			FilteredByGame:              d.SyncStatus.FilteredByGame,
			LastSyncFailed:              d.SyncStatus.LastSyncFailed,
			Revision:                    d.SyncStatus.Revision,
			BackendUpdatedAt:            d.SyncStatus.BackendUpdatedAt,
			UpdateSource:                Redact(d.SyncStatus.UpdateSource),
		},
		Campaigns: campaignDocs,
		Truncated: truncated,
	}

	if d.ProgressWatchdog != nil {
		drops, trDrops := truncateSlice(d.ProgressWatchdog.Drops, maxDropCampaigns)
		b.note("drops.progressWatchdog.drops", trDrops)
		avoided, trAvoided := truncateSlice(d.ProgressWatchdog.Avoided, maxAvoidedChannels)
		b.note("drops.progressWatchdog.avoided", trAvoided)
		addIfTruncated(truncated, "progressWatchdogDrops", trDrops)
		addIfTruncated(truncated, "progressWatchdogAvoided", trAvoided)

		dropDocs := make([]dropProgressDoc, 0, len(drops))
		for _, p := range drops {
			dropDocs = append(dropDocs, dropProgressDoc{
				Campaign:             Redact(p.Campaign),
				Drop:                 Redact(p.Drop),
				Channel:              Redact(p.Channel),
				Status:               Redact(p.Status),
				LastMinutes:          p.LastMinutes,
				LastProgressAt:       p.LastProgressAt,
				ReportsSinceProgress: p.ReportsSinceProgress,
				NoProgressObs:        p.NoProgressObs,
				RecoveryStage:        p.RecoveryStage,
				RecoveryStageName:    Redact(p.RecoveryStageName),
				LastRecoveryAt:       p.LastRecoveryAt,
			})
		}
		avoidedDocs := make([]avoidedChannelDoc, 0, len(avoided))
		for _, a := range avoided {
			avoidedDocs = append(avoidedDocs, avoidedChannelDoc{
				Login:  Redact(a.Login),
				Until:  a.Until,
				Reason: Redact(a.Reason),
			})
		}
		doc.ProgressWatchdog = &progressWatchdogDoc{
			Enabled:     d.ProgressWatchdog.Enabled,
			EvaluatedAt: d.ProgressWatchdog.EvaluatedAt,
			Drops:       dropDocs,
			Avoided:     avoidedDocs,
		}
	}

	if d.Policy != nil {
		decisions, trDecisions := truncateSlice(d.Policy.Decisions, maxPolicyDecisions)
		b.note("drops.policy.decisions", trDecisions)
		addIfTruncated(truncated, "policyDecisions", trDecisions)

		decisionDocs := make([]policyDecisionDoc, 0, len(decisions))
		for _, dd := range decisions {
			factors := make([]policyFactorDoc, 0, len(dd.Factors))
			for _, f := range dd.Factors {
				factors = append(factors, policyFactorDoc{Label: Redact(f.Label), Points: f.Points})
			}
			decisionDocs = append(decisionDocs, policyDecisionDoc{
				Campaign:      Redact(dd.Campaign),
				Status:        Redact(dd.Status),
				Total:         dd.Total,
				Excluded:      dd.Excluded,
				ExcludeReason: Redact(dd.ExcludeReason),
				Feasibility: policyFeasibilityDoc{
					MinutesToNextReward:   dd.Feasibility.MinutesToNextReward,
					MinutesToCompleteAll:  dd.Feasibility.MinutesToCompleteAll,
					CanCompleteNextReward: dd.Feasibility.CanCompleteNextReward,
					CanCompleteAll:        dd.Feasibility.CanCompleteAll,
				},
				Factors: factors,
			})
		}
		doc.Policy = &policyDoc{Mode: Redact(d.Policy.Mode), Decisions: decisionDocs}
	}

	return doc
}

func (b *builder) buildJournals(j JournalsSection) (journalDoc[slotEventDoc], journalDoc[healthEventDoc]) {
	slots, slotsTr := truncateNewest(j.Slots, maxJournalRecords, func(r SlotEventRecord) uint64 { return r.Seq })
	b.note("journals.slots", slotsTr.Truncation)

	slotDocs := make([]slotEventDoc, 0, len(slots))
	for _, s := range slots {
		slotDocs = append(slotDocs, slotEventDoc{
			Seq:              s.Seq,
			At:               s.At,
			Type:             Redact(s.Type),
			Channel:          Redact(s.Channel),
			ChannelID:        Redact(s.ChannelID),
			Broadcast:        Redact(s.Broadcast),
			Origin:           Redact(s.Origin),
			SlotIndex:        s.SlotIndex,
			Reason:           Redact(s.Reason),
			PrevReason:       Redact(s.PrevReason),
			Victim:           Redact(s.Victim),
			VictimID:         Redact(s.VictimID),
			ResidenceSeconds: s.ResidenceSeconds,
			Successes:        s.Successes,
			Failures:         s.Failures,
			Stage:            Redact(s.Stage),
			Status:           s.Status,
			ErrorCode:        Redact(s.ErrorCode),
			ResetReason:      Redact(s.ResetReason),
		})
	}

	health, healthTr := truncateNewest(j.Health, maxJournalRecords, func(r HealthEventRecord) uint64 { return r.Seq })
	b.note("journals.health", healthTr.Truncation)

	healthDocs := make([]healthEventDoc, 0, len(health))
	for _, h := range health {
		healthDocs = append(healthDocs, healthEventDoc{
			Seq:                   h.Seq,
			At:                    h.At,
			Type:                  Redact(h.Type),
			Domain:                Redact(h.Domain),
			PrevLevel:             Redact(h.PrevLevel),
			NewLevel:              Redact(h.NewLevel),
			APIState:              Redact(h.APIState),
			PubSubDown:            h.PubSubDown,
			PubSubDegraded:        h.PubSubDegraded,
			Evidence:              Redact(h.Evidence),
			Recovery:              Redact(h.Recovery),
			Reason:                Redact(h.Reason),
			NotificationRequested: h.NotificationRequested,
			SuppressedDuplicates:  h.SuppressedDuplicates,
		})
	}

	return journalDoc[slotEventDoc]{
			Capacity:  maxJournalRecords,
			LastSeq:   slotsTr.LastSeq,
			Included:  slotsTr.Included,
			Omitted:   slotsTr.Omitted,
			Truncated: slotsTr.Truncated,
			Records:   slotDocs,
		}, journalDoc[healthEventDoc]{
			Capacity:  maxJournalRecords,
			LastSeq:   healthTr.LastSeq,
			Included:  healthTr.Included,
			Omitted:   healthTr.Omitted,
			Truncated: healthTr.Truncated,
			Records:   healthDocs,
		}
}

// truncateSlice caps items at max, keeping the first max entries (the input
// order is whatever the caller's mapping produced - stable but not
// recency-ordered, unlike the journals below).
func truncateSlice[T any](items []T, max int) ([]T, Truncation) {
	total := len(items)
	if total <= max {
		return items, Truncation{Included: total, Omitted: 0, Truncated: false}
	}
	return items[:max], Truncation{Included: max, Omitted: total - max, Truncated: true}
}

// journalTruncation adds the newest-retained sequence number to Truncation,
// for the two journal sections.
type journalTruncation struct {
	Truncation
	LastSeq uint64
}

// truncateNewest caps a journal's records at max, keeping the NEWEST ones.
// It assumes items is ordered oldest-first (the contract journal.Journal's
// own Snapshot method documents and that internal/miner's callers already
// follow), so the newest entries are the SUFFIX of the slice, and LastSeq -
// the highest sequence number the journal has ever assigned, whether or not
// that record survived truncation - is seqOf of the final element.
func truncateNewest[T any](items []T, max int, seqOf func(T) uint64) ([]T, journalTruncation) {
	total := len(items)
	if total == 0 {
		return nil, journalTruncation{}
	}
	lastSeq := seqOf(items[total-1])
	if total <= max {
		return items, journalTruncation{
			Truncation: Truncation{Included: total, Omitted: 0, Truncated: false},
			LastSeq:    lastSeq,
		}
	}
	kept := items[total-max:]
	return kept, journalTruncation{
		Truncation: Truncation{Included: max, Omitted: total - max, Truncated: true},
		LastSeq:    lastSeq,
	}
}

// buildReadme renders the plain-text README.txt entry.
func buildReadme(generatedAt time.Time) []byte {
	var sb strings.Builder
	sb.WriteString("Bukerov Twitch Miner -- Support Bundle\n")
	sb.WriteString("=======================================\n\n")
	fmt.Fprintf(&sb, "Generated: %s\n", generatedAt.Format(time.RFC3339Nano))
	fmt.Fprintf(&sb, "Schema version: %d\n\n", schemaVersion)
	sb.WriteString("This archive is a redacted snapshot of the miner's operational state,\n")
	sb.WriteString("meant to help diagnose a problem. It is built entirely from an in-memory,\n")
	sb.WriteString("typed allowlist: nothing here was read from disk, your config file, the\n")
	sb.WriteString("database, or the process environment.\n\n")
	sb.WriteString("Included:\n")
	sb.WriteString("  manifest.json          build/version info and what this archive contains\n")
	sb.WriteString("  runtime.json           feature flags, intervals, and coarse counts\n")
	sb.WriteString("  health.json            health-signal statuses (no raw error text)\n")
	sb.WriteString("  watching.json          current watch-slot allocation and per-streamer state\n")
	sb.WriteString("  drops.json             drop-campaign sync/progress/policy state\n")
	sb.WriteString("  journals/slots.json    bounded watch-slot lifecycle history (newest kept)\n")
	sb.WriteString("  journals/health.json   bounded connection-health transition history (newest kept)\n\n")
	sb.WriteString("Explicitly EXCLUDED -- never collected, never written here:\n")
	sb.WriteString("  - your Twitch account login/ID, OAuth tokens, cookies, or session state\n")
	sb.WriteString("  - passwords, client secrets, API keys, or Discord/webhook URLs\n")
	sb.WriteString("  - raw error messages, stack traces, or free-form log/event text\n")
	sb.WriteString("  - your config file, environment variables, or database contents\n")
	sb.WriteString("  - your channel-points balance\n\n")
	sb.WriteString("Even so: review the files above before sharing them publicly or with\n")
	sb.WriteString("anyone you don't already trust with your miner's operational details\n")
	sb.WriteString("(which channels you watch, when, and why).\n")
	return []byte(sb.String())
}
