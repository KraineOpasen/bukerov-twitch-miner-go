package supportbundle

import "time"

// This file defines the exact JSON shape of each ZIP entry (schemaVersion 1).
// These types are private and exist ONLY to control wire field names/omission
// - they are populated from Input by build.go, never constructed or consumed
// outside this package.

// manifestDoc is manifest.json.
type manifestDoc struct {
	SchemaVersion       int                   `json:"schemaVersion"`
	BundleFormatVersion int                   `json:"bundleFormatVersion"`
	GeneratedAt         time.Time             `json:"generatedAt"`
	AppVersion          string                `json:"appVersion,omitempty"`
	GoVersion           string                `json:"goVersion,omitempty"`
	OS                  string                `json:"os,omitempty"`
	Arch                string                `json:"arch,omitempty"`
	UptimeSeconds       int64                 `json:"uptimeSeconds"`
	MinerStatus         string                `json:"minerStatus,omitempty"`
	IncludedFiles       []string              `json:"includedFiles"`
	Truncations         map[string]Truncation `json:"truncations"`
}

// runtimeDoc is runtime.json.
type runtimeDoc struct {
	DashboardAuthMode string           `json:"dashboardAuthMode"`
	FeatureFlags      featureFlagsDoc  `json:"featureFlags"`
	Intervals         intervalsDoc     `json:"intervals"`
	Counts            countsDoc        `json:"counts"`
	Notifications     notificationsDoc `json:"notifications"`
}

type featureFlagsDoc struct {
	DiscoveryEnabled        bool `json:"discoveryEnabled"`
	DropsTracked            bool `json:"dropsTracked"`
	ProgressWatchdogEnabled bool `json:"progressWatchdogEnabled"`
	PolicyEngineActive      bool `json:"policyEngineActive"`
	NotificationsEnabled    bool `json:"notificationsEnabled"`
}

type intervalsDoc struct {
	CampaignSyncMinutes  int     `json:"campaignSyncMinutes,omitempty"`
	WatchTimeWindowHours float64 `json:"watchTimeWindowHours,omitempty"`
}

type countsDoc struct {
	ConfiguredStreamers int `json:"configuredStreamers"`
	DiscoveredChannels  int `json:"discoveredChannels"`
}

type notificationsDoc struct {
	Enabled     bool     `json:"enabled"`
	Providers   []string `json:"providers,omitempty"`
	ConfigValid bool     `json:"configValid"`
}

// healthDoc is health.json.
type healthDoc struct {
	ActiveClient string            `json:"activeClient,omitempty"`
	Signals      []healthSignalDoc `json:"signals"`
}

type healthSignalDoc struct {
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	CheckedAt time.Time `json:"checkedAt,omitzero"`
	Stage     string    `json:"stage,omitempty"`
	ErrorCode string    `json:"errorCode,omitempty"`
}

// watchingDoc is watching.json.
type watchingDoc struct {
	Mode                 string                `json:"mode"`
	EvaluatedAt          time.Time             `json:"evaluatedAt,omitzero"`
	WatchTimeWindowHours float64               `json:"watchTimeWindowHours,omitempty"`
	Slots                []watchSlotDoc        `json:"slots,omitempty"`
	Waiting              []waitingSlotDoc      `json:"waiting,omitempty"`
	Streamers            []streamerEntryDoc    `json:"streamers,omitempty"`
	PubSub               pubSubDoc             `json:"pubsub"`
	Truncated            map[string]Truncation `json:"truncated"`
}

type watchSlotDoc struct {
	Slot       int    `json:"slot"`
	Channel    string `json:"channel"`
	Source     string `json:"source,omitempty"`
	ReasonCode string `json:"reasonCode,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Campaign   string `json:"campaign,omitempty"`
}

type waitingSlotDoc struct {
	Channel    string `json:"channel"`
	Source     string `json:"source,omitempty"`
	ReasonCode string `json:"reasonCode,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

type streamerEntryDoc struct {
	Channel              string  `json:"channel"`
	Status               string  `json:"status"`
	StatusReason         string  `json:"statusReason,omitempty"`
	Watching             bool    `json:"watching"`
	Reason               string  `json:"reason,omitempty"`
	WatchedMinutesWindow float64 `json:"watchedMinutesWindow"`
	HasBroadcastID       bool    `json:"hasBroadcastID"`
	Game                 string  `json:"game,omitempty"`
}

type pubSubDoc struct {
	TotalTopics int             `json:"totalTopics"`
	Connections []pubSubConnDoc `json:"connections,omitempty"`
}

type pubSubConnDoc struct {
	Index        int       `json:"index"`
	Topics       int       `json:"topics"`
	LastPong     time.Time `json:"lastPong,omitzero"`
	Reconnecting bool      `json:"reconnecting,omitempty"`
	Closed       bool      `json:"closed,omitempty"`
}

// dropsDoc is drops.json.
type dropsDoc struct {
	SyncStatus       dropsSyncStatusDoc    `json:"syncStatus"`
	Campaigns        []dropCampaignDoc     `json:"campaigns,omitempty"`
	ProgressWatchdog *progressWatchdogDoc  `json:"progressWatchdog,omitempty"`
	Policy           *policyDoc            `json:"policy,omitempty"`
	Truncated        map[string]Truncation `json:"truncated"`
}

type dropsSyncStatusDoc struct {
	LastSyncAt             time.Time `json:"lastSyncAt,omitzero"`
	LastSuccessAt          time.Time `json:"lastSuccessAt,omitzero"`
	IntervalMinutes        int       `json:"intervalMinutes,omitempty"`
	SyncRuns               int       `json:"syncRuns"`
	DashboardCampaigns     int       `json:"dashboardCampaigns"`
	TrackedCampaigns       int       `json:"trackedCampaigns"`
	RecoveredFromInventory int       `json:"recoveredFromInventory"`
	FilteredByBlacklist    int       `json:"filteredByBlacklist"`
	FilteredByGame         int       `json:"filteredByGame"`
	LastSyncFailed         bool      `json:"lastSyncFailed"`
	Revision               uint64    `json:"revision"`
	BackendUpdatedAt       time.Time `json:"backendUpdatedAt,omitzero"`
	UpdateSource           string    `json:"updateSource,omitempty"`
}

type dropCampaignDoc struct {
	Name              string    `json:"name"`
	Game              string    `json:"game,omitempty"`
	GameID            string    `json:"gameID,omitempty"`
	EndAt             time.Time `json:"endAt,omitzero"`
	RemainingDrops    int       `json:"remainingDrops"`
	OverallPercent    int       `json:"overallPercent"`
	ClaimStatus       string    `json:"claimStatus,omitempty"`
	ChannelRestricted bool      `json:"channelRestricted"`
	InInventory       bool      `json:"inInventory"`
}

type progressWatchdogDoc struct {
	Enabled     bool                `json:"enabled"`
	EvaluatedAt time.Time           `json:"evaluatedAt,omitzero"`
	Drops       []dropProgressDoc   `json:"drops,omitempty"`
	Avoided     []avoidedChannelDoc `json:"avoided,omitempty"`
}

type dropProgressDoc struct {
	Campaign             string    `json:"campaign"`
	Drop                 string    `json:"drop"`
	Channel              string    `json:"channel,omitempty"`
	Status               string    `json:"status"`
	LastMinutes          int       `json:"lastMinutes"`
	LastProgressAt       time.Time `json:"lastProgressAt,omitzero"`
	ReportsSinceProgress int       `json:"reportsSinceProgress"`
	NoProgressObs        int       `json:"noProgressObservations"`
	RecoveryStage        int       `json:"recoveryStage,omitempty"`
	RecoveryStageName    string    `json:"recoveryStageName,omitempty"`
	LastRecoveryAt       time.Time `json:"lastRecoveryAt,omitzero"`
}

type avoidedChannelDoc struct {
	Login  string    `json:"login"`
	Until  time.Time `json:"until,omitzero"`
	Reason string    `json:"reason,omitempty"`
}

type policyDoc struct {
	Mode      string              `json:"mode"`
	Decisions []policyDecisionDoc `json:"decisions,omitempty"`
}

type policyDecisionDoc struct {
	Campaign      string               `json:"campaign"`
	Status        string               `json:"status"`
	Total         int                  `json:"total"`
	Excluded      bool                 `json:"excluded,omitempty"`
	ExcludeReason string               `json:"excludeReason,omitempty"`
	Feasibility   policyFeasibilityDoc `json:"feasibility"`
	Factors       []policyFactorDoc    `json:"factors,omitempty"`
}

type policyFeasibilityDoc struct {
	MinutesToNextReward   int  `json:"minutesToNextReward"`
	MinutesToCompleteAll  int  `json:"minutesToCompleteAll"`
	CanCompleteNextReward bool `json:"canCompleteNextReward"`
	CanCompleteAll        bool `json:"canCompleteAll"`
}

type policyFactorDoc struct {
	Label  string `json:"label"`
	Points int    `json:"points"`
}

// journalDoc is the shape of both journals/slots.json and journals/health.json.
type journalDoc[T any] struct {
	Capacity  int    `json:"capacity"`
	LastSeq   uint64 `json:"lastSeq"`
	Included  int    `json:"included"`
	Omitted   int    `json:"omitted"`
	Truncated bool   `json:"truncated"`
	Records   []T    `json:"records"`
}

type slotEventDoc struct {
	Seq       uint64    `json:"seq"`
	At        time.Time `json:"at"`
	Type      string    `json:"type"`
	Channel   string    `json:"channel,omitempty"`
	ChannelID string    `json:"channelId,omitempty"`
	Broadcast string    `json:"broadcast,omitempty"`
	Origin    string    `json:"origin,omitempty"`
	SlotIndex int       `json:"slotIndex,omitempty"`

	Reason     string `json:"reason,omitempty"`
	PrevReason string `json:"prevReason,omitempty"`

	Victim   string `json:"victim,omitempty"`
	VictimID string `json:"victimId,omitempty"`

	ResidenceSeconds float64 `json:"residenceSeconds,omitempty"`
	Successes        int     `json:"successes,omitempty"`
	Failures         int     `json:"failures,omitempty"`

	Stage     string `json:"stage,omitempty"`
	Status    int    `json:"status,omitempty"`
	ErrorCode string `json:"errorCode,omitempty"`

	ResetReason string `json:"resetReason,omitempty"`
}

type healthEventDoc struct {
	Seq    uint64    `json:"seq"`
	At     time.Time `json:"at"`
	Type   string    `json:"type"`
	Domain string    `json:"domain,omitempty"`

	PrevLevel string `json:"prevLevel,omitempty"`
	NewLevel  string `json:"newLevel,omitempty"`

	APIState       string `json:"apiState,omitempty"`
	PubSubDown     bool   `json:"pubsubDown,omitempty"`
	PubSubDegraded bool   `json:"pubsubDegraded,omitempty"`

	Evidence string `json:"evidence,omitempty"`
	Recovery string `json:"recovery,omitempty"`
	Reason   string `json:"reason,omitempty"`

	NotificationRequested bool `json:"notificationRequested,omitempty"`
	SuppressedDuplicates  int  `json:"suppressedDuplicates,omitempty"`
}
