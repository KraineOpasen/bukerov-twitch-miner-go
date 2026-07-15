package miner

import (
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/health"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/pubsub"
)

func TestPubsubSignal(t *testing.T) {
	now := time.Now()
	threshold := 5 * time.Minute
	fresh := now.Add(-time.Minute)
	stale := now.Add(-10 * time.Minute)

	tests := []struct {
		name       string
		conns      []pubsub.ConnState
		lastActive time.Time
		wantStatus string
		wantCode   string
	}{
		{
			name:       "no connections falls back to pool staleness (unknown on zero)",
			conns:      nil,
			lastActive: time.Time{},
			wantStatus: health.StatusUnknown,
		},
		{
			name: "all connections fresh is OK",
			conns: []pubsub.ConnState{
				{Index: 0, Topics: 50, LastPong: fresh},
				{Index: 1, Topics: 12, LastPong: fresh},
			},
			lastActive: fresh,
			wantStatus: health.StatusOK,
		},
		{
			name: "one stale-pong index is flagged despite a fresh sibling",
			// This is the max-PONG blind spot: index 0 keeps the pool-wide max
			// fresh, but index 1's socket is dead. Per-index catches it.
			conns: []pubsub.ConnState{
				{Index: 0, Topics: 50, LastPong: fresh},
				{Index: 1, Topics: 12, LastPong: stale},
			},
			lastActive: fresh,
			wantStatus: health.StatusFailed,
			wantCode:   "connection_stale",
		},
		{
			name: "a stale index that is mid-reconnect is not flagged",
			conns: []pubsub.ConnState{
				{Index: 0, Topics: 50, LastPong: fresh},
				{Index: 1, Topics: 12, LastPong: stale, Reconnecting: true},
			},
			lastActive: fresh,
			wantStatus: health.StatusOK,
		},
		{
			name: "topic-less zombie among multiple connections is flagged",
			// Socket alive (fresh PONG) but 0 topics after a reconnect — invisible
			// to any PONG check, caught by the topic count.
			conns: []pubsub.ConnState{
				{Index: 0, Topics: 50, LastPong: fresh},
				{Index: 1, Topics: 0, LastPong: fresh},
			},
			lastActive: fresh,
			wantStatus: health.StatusStalled,
			wantCode:   "topics_lost",
		},
		{
			name: "single connection with 0 topics is not a zombie (no overflow peer)",
			conns: []pubsub.ConnState{
				{Index: 0, Topics: 0, LastPong: fresh},
			},
			lastActive: fresh,
			wantStatus: health.StatusOK,
		},
		{
			name: "closed connection is skipped, fresh sibling keeps it OK",
			conns: []pubsub.ConnState{
				{Index: 0, Topics: 50, LastPong: fresh},
				{Index: 1, Topics: 0, LastPong: stale, Closed: true},
			},
			lastActive: fresh,
			wantStatus: health.StatusOK,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sig := pubsubSignal(tc.conns, tc.lastActive, now, threshold)
			if sig.Name != health.SignalPubSub {
				t.Errorf("Name = %q, want %q", sig.Name, health.SignalPubSub)
			}
			if sig.Status != tc.wantStatus {
				t.Fatalf("Status = %q, want %q (detail=%q)", sig.Status, tc.wantStatus, sig.Detail)
			}
			if tc.wantCode != "" && sig.ErrorCode != tc.wantCode {
				t.Errorf("ErrorCode = %q, want %q", sig.ErrorCode, tc.wantCode)
			}
		})
	}
}
