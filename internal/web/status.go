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
