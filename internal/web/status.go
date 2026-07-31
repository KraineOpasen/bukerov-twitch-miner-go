package web

import (
	"sync"
)

type MinerStatus string

const (
	StatusInitializing     MinerStatus = "initializing"
	StatusAuthRequired     MinerStatus = "auth_required"
	StatusAuthWaiting      MinerStatus = "auth_waiting"
	StatusLoadingStreamers MinerStatus = "loading_streamers"
	StatusRunning          MinerStatus = "running"
	StatusError            MinerStatus = "error"

	// Lifecycle-driven statuses (Ф4c): published exclusively by the
	// internal/app lifecycle status adapter over internal/lifecycle's
	// Observed states, never by the miner's own startup progression. See
	// SetGeneration and StatusInfo.Generation for the discriminator that
	// tells the client whether a startup-phase status belongs to the very
	// first boot (show the blocking overlay) or a later lifecycle
	// transition (never show it — the lifecycle panel is the surface).
	StatusPaused     MinerStatus = "paused"
	StatusStopped    MinerStatus = "stopped"
	StatusRestarting MinerStatus = "restarting"
	StatusFailed     MinerStatus = "failed"
	StatusDegraded   MinerStatus = "degraded"
)

type AuthInfo struct {
	VerificationURI string `json:"verificationUri,omitempty"`
	UserCode        string `json:"userCode,omitempty"`
	ExpiresIn       int    `json:"expiresIn,omitempty"`
}

type StatusInfo struct {
	Status       MinerStatus `json:"status"`
	Message      string      `json:"message,omitempty"`
	Auth         *AuthInfo   `json:"auth,omitempty"`
	StreamerInfo string      `json:"streamerInfo,omitempty"`

	// ReauthRequired/ConnectionLost are system-wide health signals, independent
	// of Status above: they drive a persistent dashboard banner rather than the
	// blocking startup overlay, and are preserved across SetStatus/SetAuthRequired/
	// SetStreamerProgress calls (which only touch the startup-overlay fields).
	ReauthRequired    bool   `json:"reauthRequired,omitempty"`
	ReauthMessage     string `json:"reauthMessage,omitempty"`
	ConnectionLost    bool   `json:"connectionLost,omitempty"`
	ConnectionMessage string `json:"connectionMessage,omitempty"`

	// ConnectionDegraded flags an impaired-but-not-lost link (frequent PubSub
	// reconnects or repeated GQL failures within the watchdog window). Like
	// ConnectionLost it is a system-wide health signal, preserved across
	// SetStatus/SetAuthRequired/SetStreamerProgress, and drives the Overview
	// network indicator (yellow).
	ConnectionDegraded        bool   `json:"connectionDegraded,omitempty"`
	ConnectionDegradedMessage string `json:"connectionDegradedMessage,omitempty"`

	// Generation is the lifecycle controller's monotonically increasing
	// generation token (design v6 §10), set by SetGeneration BEFORE that
	// generation's Run is launched — so it never lags the status it labels.
	// Like the fields above it is preserved across SetStatus/SetAuthRequired/
	// SetStreamerProgress. omitempty means a process with no lifecycle
	// controller wired (or one that has not yet started a generation) simply
	// omits the field; the client treats an absent value as 1
	// ((status.generation || 1) <= 1), i.e. "still the first boot".
	Generation uint64 `json:"generation,omitempty"`
}

type StatusBroadcaster struct {
	status    StatusInfo
	listeners []chan StatusInfo
	mu        sync.RWMutex
}

func NewStatusBroadcaster() *StatusBroadcaster {
	return &StatusBroadcaster{
		status: StatusInfo{
			Status:  StatusInitializing,
			Message: "Starting up...",
		},
	}
}

func (b *StatusBroadcaster) GetStatus() StatusInfo {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.status
}

func (b *StatusBroadcaster) SetStatus(status MinerStatus, message string) {
	b.mu.Lock()
	b.status.Status = status
	b.status.Message = message
	b.status.Auth = nil
	b.status.StreamerInfo = ""
	current := b.status
	b.mu.Unlock()

	b.broadcast(current)
}

func (b *StatusBroadcaster) SetAuthRequired(verificationURI, userCode string, expiresIn int) {
	b.mu.Lock()
	b.status.Status = StatusAuthRequired
	b.status.Message = "Please authorize with Twitch"
	b.status.Auth = &AuthInfo{
		VerificationURI: verificationURI,
		UserCode:        userCode,
		ExpiresIn:       expiresIn,
	}
	b.status.StreamerInfo = ""
	current := b.status
	b.mu.Unlock()

	b.broadcast(current)
}

func (b *StatusBroadcaster) SetStreamerProgress(current, total int, name string) {
	b.mu.Lock()
	b.status.Status = StatusLoadingStreamers
	b.status.Message = "Loading streamers..."
	b.status.Auth = nil
	b.status.StreamerInfo = name
	current2 := b.status
	b.mu.Unlock()

	b.broadcast(current2)
}

// SetReauthRequired sets/clears the system-wide "Twitch reauthorization
// required" banner shown on the dashboard, independent of the startup Status.
func (b *StatusBroadcaster) SetReauthRequired(required bool, message string) {
	b.mu.Lock()
	b.status.ReauthRequired = required
	b.status.ReauthMessage = message
	current := b.status
	b.mu.Unlock()

	b.broadcast(current)
}

// SetConnectionLost sets/clears the system-wide "connection lost" banner
// shown on the dashboard, independent of the startup Status.
func (b *StatusBroadcaster) SetConnectionLost(lost bool, message string) {
	b.mu.Lock()
	b.status.ConnectionLost = lost
	b.status.ConnectionMessage = message
	current := b.status
	b.mu.Unlock()

	b.broadcast(current)
}

// SetConnectionDegraded sets/clears the system-wide "connection degraded"
// signal (impaired but not lost), independent of the startup Status. Mirrors
// SetConnectionLost; drives the Overview network indicator's yellow state.
func (b *StatusBroadcaster) SetConnectionDegraded(degraded bool, message string) {
	b.mu.Lock()
	b.status.ConnectionDegraded = degraded
	b.status.ConnectionDegradedMessage = message
	current := b.status
	b.mu.Unlock()

	b.broadcast(current)
}

// SetGeneration publishes the current lifecycle generation token, preserving
// every other field (mirrors SetConnectionDegraded's set-field/copy/broadcast
// shape) — it must never clear Status/Message/Auth/StreamerInfo, since it can
// be called independently of any startup-overlay transition.
func (b *StatusBroadcaster) SetGeneration(gen uint64) {
	b.mu.Lock()
	b.status.Generation = gen
	current := b.status
	b.mu.Unlock()

	b.broadcast(current)
}

func (b *StatusBroadcaster) Subscribe() chan StatusInfo {
	b.mu.Lock()
	defer b.mu.Unlock()

	ch := make(chan StatusInfo, 10)
	b.listeners = append(b.listeners, ch)
	ch <- b.status
	return ch
}

func (b *StatusBroadcaster) Unsubscribe(ch chan StatusInfo) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for i, listener := range b.listeners {
		if listener == ch {
			b.listeners = append(b.listeners[:i], b.listeners[i+1:]...)
			close(ch)
			return
		}
	}
}

func (b *StatusBroadcaster) broadcast(status StatusInfo) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	for _, ch := range b.listeners {
		select {
		case ch <- status:
		default:
		}
	}
}
