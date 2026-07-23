package miner

import (
	"log/slog"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/pubsub"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/streamer"
)

// topicReconciler is the slice of the WebSocketPool the runtime capability
// reconciliation needs. *pubsub.WebSocketPool satisfies it; tests inject a
// recording fake so the desired-state plan is verified without a network.
type topicReconciler interface {
	EnsureTopic(topic pubsub.Topic, desired bool) error
}

// chatToggler is the slice of the ChatManager the reconciliation needs.
// *chat.ChatManager satisfies it.
type chatToggler interface {
	ToggleChat(streamer *models.Streamer)
	Leave(username string)
}

// capabilityChangeReason explains why a roster member appears in a
// reconciliation plan. It is diagnostic only — execution is identical for
// every non-removed entry, which is what makes a repeated identical apply heal
// a previously failed subscription (repair) instead of skipping it.
type capabilityChangeReason string

const (
	capabilityAdded           capabilityChangeReason = "added"
	capabilityRemoved         capabilityChangeReason = "removed"
	capabilitySettingsChanged capabilityChangeReason = "settings_changed"
	capabilityRepair          capabilityChangeReason = "repair"
)

// capabilityTopicOrder fixes a deterministic execution order for the
// per-channel topics. The always-on playback topic reconciles first.
var capabilityTopicOrder = []pubsub.TopicType{
	pubsub.TopicVideoPlaybackByID,
	pubsub.TopicRaid,
	pubsub.TopicPredictionsChannel,
	pubsub.TopicCommunityMomentsChannel,
	pubsub.TopicCommunityPointsChannel,
}

// desiredCapabilityTopics maps a settings snapshot to the desired per-channel
// topic state. TopicVideoPlaybackByID is unconditionally desired for every
// tracked streamer — no optional flag may remove it — and the global user
// topics (community-points-user, predictions-user) are deliberately absent:
// they are account-scoped, not per-streamer capabilities.
func desiredCapabilityTopics(settings models.StreamerSettings) map[pubsub.TopicType]bool {
	return map[pubsub.TopicType]bool{
		pubsub.TopicVideoPlaybackByID:       true,
		pubsub.TopicRaid:                    settings.FollowRaid,
		pubsub.TopicPredictionsChannel:      settings.MakePredictions,
		pubsub.TopicCommunityMomentsChannel: settings.ClaimMoments,
		pubsub.TopicCommunityPointsChannel:  settings.CommunityGoals,
	}
}

// runtimeCapabilityAction is one roster member's immutable reconciliation
// intent: the exact ChannelID its PubSub topics are keyed by, the current
// username (IRC is the one protocol that needs the login), the desired topic
// state, and whether the streamer is leaving the roster entirely.
type runtimeCapabilityAction struct {
	channelID string
	username  string
	streamer  *models.Streamer
	removed   bool
	desired   map[pubsub.TopicType]bool
	reason    capabilityChangeReason
}

// runtimeCapabilityPlan is the full immutable plan one settings apply builds
// BEFORE executing any side effect. No miner/manager/streamer lock is held
// while the plan is executed.
type runtimeCapabilityPlan struct {
	actions []runtimeCapabilityAction
}

// runtimeCapabilityResult aggregates what actually happened, so drift after a
// failure is visible (and healed by the next identical apply) instead of being
// reported as success.
type runtimeCapabilityResult struct {
	reconciled int
	failures   int
}

// capabilityTopicReconciler resolves the topic sink: the test seam when
// injected, otherwise the production pool (nil when the pool is not wired).
func (m *Miner) capabilityTopicReconciler() topicReconciler {
	if m.capabilityTopics != nil {
		return m.capabilityTopics
	}
	if m.wsPool != nil {
		return m.wsPool
	}
	return nil
}

// chatPresenceReconciler resolves the chat sink analogously.
func (m *Miner) chatPresenceReconciler() chatToggler {
	if m.chatPresence != nil {
		return m.chatPresence
	}
	if m.chatManager != nil {
		return m.chatManager
	}
	return nil
}

// buildCapabilityPlan derives the desired-state plan for one settings apply:
// one action per removed streamer (all per-channel topics down, IRC leave) and
// one action per CURRENT roster member (exact desired topic state from its
// settings snapshot). Covering the whole roster — not just the old/new diff —
// is what makes a repeated identical apply re-attempt a previously failed
// subscription: the reconcile target is the pool's actual state, so drift
// heals on the next apply with no extra bookkeeping.
func (m *Miner) buildCapabilityPlan(added, removed []*models.Streamer, changed []streamer.SettingsChange) runtimeCapabilityPlan {
	addedSet := make(map[*models.Streamer]bool, len(added))
	for _, s := range added {
		addedSet[s] = true
	}
	changedSet := make(map[*models.Streamer]bool, len(changed))
	for _, c := range changed {
		changedSet[c.Streamer] = true
	}

	var plan runtimeCapabilityPlan
	for _, s := range removed {
		plan.actions = append(plan.actions, runtimeCapabilityAction{
			channelID: s.ChannelID,
			username:  s.GetUsername(),
			streamer:  s,
			removed:   true,
			reason:    capabilityRemoved,
		})
	}
	for _, s := range m.streamers.All() {
		reason := capabilityRepair
		switch {
		case addedSet[s]:
			reason = capabilityAdded
		case changedSet[s]:
			reason = capabilitySettingsChanged
		}
		plan.actions = append(plan.actions, runtimeCapabilityAction{
			channelID: s.ChannelID,
			username:  s.GetUsername(),
			streamer:  s,
			desired:   desiredCapabilityTopics(s.GetSettings()),
			reason:    reason,
		})
	}
	return plan
}

// executeCapabilityPlan applies the plan best-effort: every topic of every
// streamer is attempted independently, so one failed subscription never blocks
// another capability (or the chat reconciliation) — it is logged, counted, and
// left retryable for the next identical apply. Chat presence is reconciled
// immediately via the idempotent ToggleChat; removed streamers always leave.
// Runs with no miner lock held; only privacy-safe fields are logged.
func (m *Miner) executeCapabilityPlan(plan runtimeCapabilityPlan) runtimeCapabilityResult {
	topics := m.capabilityTopicReconciler()
	chatMgr := m.chatPresenceReconciler()

	var result runtimeCapabilityResult
	for _, action := range plan.actions {
		result.reconciled++

		if action.removed {
			if topics != nil && action.channelID != "" {
				for _, tt := range capabilityTopicOrder {
					if err := topics.EnsureTopic(pubsub.NewTopic(tt, action.channelID), false); err != nil {
						result.failures++
						slog.Warn("Failed to unsubscribe removed streamer's topic",
							"streamer", action.username, "topic", string(tt), "error", err)
					}
				}
			}
			if chatMgr != nil {
				chatMgr.Leave(action.username)
			}
			continue
		}

		// A roster member without a resolved ChannelID has nothing to key PubSub
		// reconciliation by; ChannelIDs are never guessed or synthesized.
		if topics != nil && action.channelID != "" {
			for _, tt := range capabilityTopicOrder {
				desired := action.desired[tt]
				if err := topics.EnsureTopic(pubsub.NewTopic(tt, action.channelID), desired); err != nil {
					result.failures++
					slog.Warn("Failed to reconcile capability topic; will retry on the next settings apply",
						"streamer", action.username, "topic", string(tt), "desired", desired,
						"reason", string(action.reason), "error", err)
				}
			}
		}
		if chatMgr != nil {
			chatMgr.ToggleChat(action.streamer)
		}
	}
	return result
}

// reconcileRuntimeCapabilities brings the runtime state (PubSub topic set, IRC
// presence) in line with the just-saved settings for the whole roster, without
// a restart. Called from ApplySettings AFTER the miner lock is released.
//
// renamed carries each config-driven rename ApplySettings just reconciled in
// place (BKM-006). PubSub needs no special handling for it — topics are keyed
// by ChannelID, which a rename never changes, so the normal per-streamer sweep
// below (now reading the streamer's CURRENT login) already reconciles the
// unchanged topic set with zero churn. IRC does need one explicit action: the
// old login's channel is joined under a name nothing will ever address again,
// so it is left exactly once, here, before the normal sweep's ToggleChat joins
// the new login if the streamer's Chat setting calls for it. A repeated apply
// with no new rename makes this a no-op (renamed is empty).
//
// reconcileMu serializes concurrent sweeps end-to-end (plan build + execution).
// Each sweep builds its plan from the CURRENT roster/settings snapshots, so
// whichever concurrent apply reconciles last converges the runtime onto the
// latest saved settings instead of leaving an out-of-order sweep's state
// behind. It is a dedicated lock: no miner/manager/streamer mutex is held
// across the side effects.
func (m *Miner) reconcileRuntimeCapabilities(added, removed []*models.Streamer, changed []streamer.SettingsChange, renamed []streamer.RenameEvent) {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()

	if len(renamed) > 0 {
		if chatMgr := m.chatPresenceReconciler(); chatMgr != nil {
			for _, r := range renamed {
				chatMgr.Leave(r.OldLogin)
			}
		}
	}

	plan := m.buildCapabilityPlan(added, removed, changed)
	result := m.executeCapabilityPlan(plan)
	if result.failures > 0 {
		slog.Warn("Runtime capability reconciliation finished with failures; drift remains until the next settings apply",
			"streamers", result.reconciled, "failures", result.failures,
			"added", len(added), "removed", len(removed), "changed", len(changed), "renamed", len(renamed))
		return
	}
	slog.Info("Runtime capabilities reconciled",
		"streamers", result.reconciled,
		"added", len(added), "removed", len(removed), "changed", len(changed), "renamed", len(renamed))
}
