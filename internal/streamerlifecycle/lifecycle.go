// Package streamerlifecycle owns the one high-level operation that fully removes
// a streamer's bot-owned PERSISTED state from SQLite: a single atomic
// transaction that purges every login-keyed store (analytics history,
// notification rules + config lists, watch-time rows) together with an in-memory
// resurrection fence that stops a late event from recreating any of it.
//
// Identity model. The stable Twitch channel ID is the PRIMARY deletion identity
// — the caller resolves it from the runtime streamer registry (which is
// ID-first) and passes it here for logging/audit. The current login is the
// resolved LOOKUP key for the persisted stores, which are all login-keyed (no
// channel_id column exists in any table). A config-driven rename repoints EVERY
// login-keyed store to the new login before the runtime commit — analytics and
// config.json in the miner's fail-closed rename transaction, notifications
// point_rules/config-lists and watch_time rows in the miner's finishApply — so
// the runtime login always matches the persisted login and deleting by the
// current login is complete for a renamed streamer too (no old-login orphans).
//
// Boundary. This package deletes only bot-owned MUTABLE persisted state. It does
// NOT touch shared/global data (the drop-campaign catalog, campaign-keyed
// notification dedupe), already-sent Discord messages, or external/immutable
// logs. Runtime teardown (PubSub topics, IRC presence, watcher maps, streak
// cache) is the caller's responsibility and is coordinated around this call.
package streamerlifecycle

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
)

// Purger deletes every row of one lowercase login within a caller-supplied
// transaction and reports whether anything existed for that login in this store.
// It must operate only on the passed *sql.Tx (never commit/rollback it) so the
// coordinator can span several stores in one atomic transaction.
type Purger interface {
	DeleteStreamerTx(tx *sql.Tx, login string) (bool, error)
}

// Fencer arms and clears a store's in-memory resurrection fence for a login.
// While a login is tombstoned, that store's write paths refuse to recreate the
// streamer's rows, so a late event that lost the race with deletion cannot
// resurrect it.
type Fencer interface {
	Tombstone(login string)
	Reinstate(login string)
}

// Outcome classifies a completed deletion.
type Outcome string

const (
	// OutcomeDeleted: at least one store held rows for the login and they were
	// removed.
	OutcomeDeleted Outcome = "deleted"
	// OutcomeAlreadyAbsent: no store held anything for the login (a repeated or
	// never-recorded deletion) — a deterministic idempotent success.
	OutcomeAlreadyAbsent Outcome = "already_absent"
)

// Result is the typed disposition of a Delete call.
type Result struct {
	Outcome   Outcome
	ChannelID string
	Login     string
}

// Coordinator runs the atomic persisted purge across a fixed set of stores.
type Coordinator struct {
	db      *database.DB
	purgers []Purger
	fencers []Fencer
}

// New builds a coordinator over the shared handle and the stores to purge/fence.
// purgers and fencers are typically the same objects (each store implements
// both); nil entries are ignored so a coordinator degrades gracefully when a
// subsystem (e.g. analytics) is disabled.
func New(db *database.DB, purgers []Purger, fencers []Fencer) *Coordinator {
	ps := make([]Purger, 0, len(purgers))
	for _, p := range purgers {
		if p != nil {
			ps = append(ps, p)
		}
	}
	fs := make([]Fencer, 0, len(fencers))
	for _, f := range fencers {
		if f != nil {
			fs = append(fs, f)
		}
	}
	return &Coordinator{db: db, purgers: ps, fencers: fs}
}

// Delete removes all of one streamer's persisted bot-owned state.
//
// Ordering (the resurrection fence + atomic purge):
//  1. Tombstone the login in every store. Each Tombstone is a barrier: once it
//     returns, every in-flight write for that login has completed and every
//     later write is refused, so no event can slip a row past the purge.
//  2. Purge every store in ONE transaction. If any store errors the whole
//     transaction rolls back — there is never a partial deletion.
//  3. On success return Deleted (something existed) or AlreadyAbsent (nothing
//     did); the tombstones stay armed until Reinstate, so any still-in-flight
//     event remains fenced.
//
// Failure contract. On a transaction error nothing is deleted (atomic rollback)
// and the tombstones are LEFT ARMED: the caller has already removed the streamer
// from the runtime roster and config, so it is inert (not watched, earning
// nothing, sending nothing) and cannot be resurrected — it is "removed but its
// history purge is pending", NOT a false success. The caller logs the typed
// error and may retry Delete (idempotent). This never leaves a half-active
// streamer, and it makes no "traceless" claim: purge-pending history rows remain
// until a retry succeeds.
func (c *Coordinator) Delete(ctx context.Context, channelID, login string) (Result, error) {
	login = strings.ToLower(strings.TrimSpace(login))
	res := Result{ChannelID: channelID, Login: login}
	if login == "" {
		return res, fmt.Errorf("streamerlifecycle: empty login for channel %q", channelID)
	}

	// 1. Fence first (barrier) so no concurrent write can recreate a purged row.
	for _, f := range c.fencers {
		f.Tombstone(login)
	}

	// 2. Atomic purge across every store.
	existed := false
	err := c.db.WithTx(ctx, func(tx *sql.Tx) error {
		for _, p := range c.purgers {
			e, perr := p.DeleteStreamerTx(tx, login)
			if perr != nil {
				return perr
			}
			existed = existed || e
		}
		return nil
	})
	if err != nil {
		// Rolled back: nothing deleted. Keep tombstones armed (inert + fenced).
		return res, fmt.Errorf("streamerlifecycle: purge streamer %q (channel %q): %w", login, channelID, err)
	}

	if existed {
		res.Outcome = OutcomeDeleted
	} else {
		res.Outcome = OutcomeAlreadyAbsent
	}
	return res, nil
}

// Reinstate clears the fence for a login in every store, so re-adding the same
// login (or channel ID) after a deletion starts a clean lifecycle: the old rows
// are already gone and fresh writes are allowed again. Idempotent and safe for a
// login that was never deleted.
func (c *Coordinator) Reinstate(login string) {
	login = strings.ToLower(strings.TrimSpace(login))
	if login == "" {
		return
	}
	for _, f := range c.fencers {
		f.Reinstate(login)
	}
}
