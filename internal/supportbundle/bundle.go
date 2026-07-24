// Package supportbundle builds a downloadable, redacted diagnostic ZIP (the
// "support bundle") from a typed, allowlisted snapshot of the miner's
// operational state. It is deliberately stdlib-only: it imports nothing from
// this module, so it cannot accidentally reach into a live component, a
// config secret, or the filesystem. Its ONLY inputs are the plain values
// placed on Input by the caller (internal/web maps those from
// debug.Snapshot); its ONLY output is an in-memory ZIP plus a filename.
//
// Security model (BKM-016):
//
//   - The PRIMARY boundary is the typed allowlist: Input and its nested DTOs
//     hold ONLY the fields this package chose to expose. There is no generic
//     marshal of a live struct anywhere in this package or its caller -
//     internal/web copies each field across by hand. A field added to
//     debug.Snapshot tomorrow is invisible here until someone deliberately
//     wires it into Input.
//   - Redact (redact.go) is DEFENSE IN DEPTH ONLY, applied to every surviving
//     string as a second layer in case a "safe" field (a game name, a reason
//     code, a campaign label) ever carries something it shouldn't.
//   - Every list-shaped section is capped at a documented bound (see the
//     max* constants below); journals keep the NEWEST records and report
//     what was omitted. A section that still doesn't fit fails the whole
//     build with a generic bounded error rather than emitting a partial or
//     oversized document.
//   - Construction is fully deterministic given the same Input and the same
//     injected clock (Options.Now): no time.Now(), no randomness, no map
//     iteration in the output (encoding/json sorts map keys, and every list
//     is built from ordered input), no filesystem or network access.
package supportbundle

import "time"

// Bundle format bookkeeping. Bump schemaVersion for a breaking document-shape
// change; bundleFormatVersion tracks the container (ZIP layout) separately so
// a document-only change doesn't have to look like a container change.
const (
	schemaVersion       = 1
	bundleFormatVersion = 1
)

// Bounds. Each is a hard, documented cap enforced during Build: a section
// holding more items than its bound keeps only the newest/first N (see
// build.go) and reports {truncated, included, omitted}. maxStringLen bounds
// every individual string (via Redact); the byte bounds are a last-resort
// safety net in case the per-item caps above still produced something too
// large to hand to a browser.
const (
	maxJournalRecords  = 200 // per journal (slots, health); newest kept
	maxStreamers       = 200 // watching.json streamers/slots/waiting
	maxDropCampaigns   = 100 // drops.json campaigns
	maxPolicyDecisions = 100 // drops.json policy decisions
	maxPubSubConns     = 64  // watching.json pubsub connections
	maxAvoidedChannels = 200 // drops.json progress-watchdog avoided list; drawn from the same channel universe as maxStreamers

	maxStringLen = 512 // every string field, after Redact's pattern check

	maxSectionBytes           = 1 << 20 // 1 MiB - one JSON entry's marshaled size
	maxTotalUncompressedBytes = 8 << 20 // 8 MiB - sum of all entries before compression
	maxZipBytes               = 4 << 20 // 4 MiB - the final ZIP Result.Bytes
)

// Options controls the parts of Build that would otherwise be
// nondeterministic. Bounds are fixed package constants, not configurable
// here, so a bundle's safety properties can't be weakened by a caller.
type Options struct {
	// Now supplies the clock used for manifest.generatedAt, the ZIP entries'
	// Modified timestamp, and the returned Filename. Nil defaults to
	// time.Now. Every OTHER timestamp in the bundle comes from Input itself
	// (the historical event/observation time the caller already had) -
	// Options.Now never touches those.
	Now func() time.Time
}

// Result is the built bundle: the ready-to-serve ZIP bytes and the filename
// the caller should offer it under (Content-Disposition).
type Result struct {
	Bytes    []byte
	Filename string
}

// Input is the complete typed allowlist a caller may place into a support
// bundle. Every field here is a plain value (string/int/bool/time.Time or a
// slice/struct of those) - there is no pointer to a live component, no
// interface, no channel, nothing that could let Build reach back into the
// application. Optional sections are *pointers so a caller can signal "this
// subsystem doesn't apply right now" (nil) versus "it applies and has
// nothing to report" (non-nil, empty slices).
type Input struct {
	AppVersion    string
	GoVersion     string
	OS            string
	Arch          string
	UptimeSeconds int64
	// MinerStatus is one of the small set of bounded status codes
	// (debug.StatusRunning / StatusPaused / StatusAuthError, or "" if
	// unknown) - never a free-form message.
	MinerStatus string

	Runtime  RuntimeInfo
	Health   *HealthSection
	Watching WatchingSection
	Drops    *DropsSection
	Journals JournalsSection
}

// RuntimeInfo is the dashboard/runtime-facts section (runtime.json).
type RuntimeInfo struct {
	// DashboardAuthMode is "authenticated" | "insecure_bypass" | "disabled".
	DashboardAuthMode string
	FeatureFlags      FeatureFlags
	Intervals         Intervals
	Counts            Counts
	Notifications     NotificationsInfo
}

// FeatureFlags summarizes which optional subsystems are currently doing
// something, so a support request doesn't need a config dump to know what's
// active.
type FeatureFlags struct {
	DiscoveryEnabled        bool
	DropsTracked            bool
	ProgressWatchdogEnabled bool
	PolicyEngineActive      bool
	NotificationsEnabled    bool
}

// Intervals carries the handful of operator-tunable timing facts that are
// useful for support (not the full settings/config document).
type Intervals struct {
	CampaignSyncMinutes  int
	WatchTimeWindowHours float64
}

// Counts carries small aggregate counts (never per-item identity beyond
// what's already in the Watching section).
type Counts struct {
	ConfiguredStreamers int
	DiscoveredChannels  int
}

// NotificationsInfo is deliberately boolean/enumerable only - never a
// webhook URL, bot token, or destination (channel ID, DM target, email).
type NotificationsInfo struct {
	Enabled bool
	// Providers holds provider NAMES only (e.g. "discord") - never a
	// destination or credential.
	Providers   []string
	ConfigValid bool
}

// HealthSection is the Health Center's aggregated signals (health.json).
// Detail is intentionally absent - see the package doc and BKM-016 design:
// health.Signal.Detail is free-form human text and is dropped at the
// debug.Snapshot boundary, never mapped into this package at all.
type HealthSection struct {
	// ActiveClient is the bounded GQL client label (TV/Browser/Mobile/
	// Unknown) - the caller allowlist-filters this before setting it.
	ActiveClient string
	Signals      []HealthSignal
}

type HealthSignal struct {
	Name      string
	Status    string
	CheckedAt time.Time
	Stage     string
	ErrorCode string
}

// WatchingSection is the watch-slot/rotation view (watching.json).
type WatchingSection struct {
	Mode                 string
	EvaluatedAt          time.Time
	WatchTimeWindowHours float64
	Slots                []WatchSlot
	Waiting              []WaitingSlot
	Streamers            []StreamerEntry
	PubSub               PubSubSection
}

type WatchSlot struct {
	Slot       int
	Channel    string
	Source     string
	ReasonCode string
	Reason     string
	Campaign   string
}

type WaitingSlot struct {
	Channel    string
	Source     string
	ReasonCode string
	Reason     string
}

// StreamerEntry is one configured/discovered streamer's diagnostic state.
// Deliberately narrower than debug.StreamerState: no title, no channel
// points, no online/offline timestamps, no drop-campaign or prediction
// detail - just enough to explain the watch decision.
type StreamerEntry struct {
	Channel              string
	Status               string
	StatusReason         string
	Watching             bool
	Reason               string
	WatchedMinutesWindow float64
	HasBroadcastID       bool
	Game                 string
}

type PubSubSection struct {
	TotalTopics int
	Connections []PubSubConn
}

type PubSubConn struct {
	Index        int
	Topics       int
	LastPong     time.Time
	Reconnecting bool
	Closed       bool
}

// DropsSection is the drops sync/progress/policy view (drops.json).
type DropsSection struct {
	SyncStatus       DropsSyncStatus
	Campaigns        []DropCampaign
	ProgressWatchdog *ProgressWatchdogSection
	Policy           *PolicySection
}

// DropsSyncStatus mirrors debug.DropsSyncInfo except LastError: instead of
// the raw (potentially arbitrary) error string, only the derived
// LastSyncFailed bool is carried.
type DropsSyncStatus struct {
	LastSyncAt             time.Time
	LastSuccessAt          time.Time
	IntervalMinutes        int
	SyncRuns               int
	DashboardCampaigns     int
	TrackedCampaigns       int
	RecoveredFromInventory int
	FilteredByBlacklist    int
	FilteredByGame         int
	LastSyncFailed         bool
	Revision               uint64
	BackendUpdatedAt       time.Time
	UpdateSource           string
}

type DropCampaign struct {
	Name              string
	Game              string
	GameID            string
	EndAt             time.Time
	RemainingDrops    int
	OverallPercent    int
	ClaimStatus       string
	ChannelRestricted bool
	InInventory       bool
}

type ProgressWatchdogSection struct {
	Enabled     bool
	EvaluatedAt time.Time
	Drops       []DropProgress
	Avoided     []AvoidedChannel
}

// DropProgress mirrors debug.DropProgressState except Detail, which is
// dropped for the same free-form-text reason as everywhere else.
type DropProgress struct {
	Campaign             string
	Drop                 string
	Channel              string
	Status               string
	LastMinutes          int
	LastProgressAt       time.Time
	ReportsSinceProgress int
	NoProgressObs        int
	RecoveryStage        int
	RecoveryStageName    string
	LastRecoveryAt       time.Time
}

type AvoidedChannel struct {
	Login  string
	Until  time.Time
	Reason string
}

type PolicySection struct {
	Mode      string
	Decisions []PolicyDecision
}

type PolicyDecision struct {
	Campaign      string
	Status        string
	Total         int
	Excluded      bool
	ExcludeReason string
	Feasibility   PolicyFeasibility
	Factors       []PolicyFactor
}

type PolicyFeasibility struct {
	MinutesToNextReward   int
	MinutesToCompleteAll  int
	CanCompleteNextReward bool
	CanCompleteAll        bool
}

type PolicyFactor struct {
	Label  string
	Points int
}

// JournalsSection carries the bounded diagnostic journals (BKM-013 slot
// lifecycle + BKM-014 health transitions). Both record types hold only
// value-typed, privacy-safe fields by construction (see internal/journal's
// package doc) - this package re-declares them locally (rather than
// importing internal/journal) to stay stdlib-only and to keep its own
// allowlist independent of that package's evolution.
type JournalsSection struct {
	Slots  []SlotEventRecord
	Health []HealthEventRecord
}

// SlotEventRecord mirrors journal.Record[journal.SlotEvent] field-for-field.
type SlotEventRecord struct {
	Seq       uint64
	At        time.Time
	Type      string
	Channel   string
	ChannelID string
	Broadcast string
	Origin    string
	SlotIndex int

	Reason     string
	PrevReason string

	Victim   string
	VictimID string

	ResidenceSeconds float64
	Successes        int
	Failures         int

	Stage     string
	Status    int
	ErrorCode string

	ResetReason string
}

// HealthEventRecord mirrors journal.Record[journal.HealthEvent] field-for-field.
type HealthEventRecord struct {
	Seq    uint64
	At     time.Time
	Type   string
	Domain string

	PrevLevel string
	NewLevel  string

	APIState       string
	PubSubDown     bool
	PubSubDegraded bool

	Evidence string
	Recovery string
	Reason   string

	NotificationRequested bool
	SuppressedDuplicates  int
}

// Truncation reports whether a bounded list section was cut down to its
// documented cap, and by how much.
type Truncation struct {
	Truncated bool
	Included  int
	Omitted   int
}
