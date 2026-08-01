package drops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// THE DEFECT THIS FILE FIXES (see SPECIFICATIONS.md "Claim History Check"):
// extractClaimedRewards (drops.go) builds every account-wide claim-history
// record with EntitlementWindow{} (Known=false), InstanceID="" and DropID="".
// Every confirmation path in models.MatchIdentity requires one of exactly
// those. So Campaign.ApplyClaimHistoryRecords can NEVER return a non-empty
// ConfirmedNames against the proven Twitch response shape (see
// claim_history_test.go's TestClaimHistoryFailOpenNoWindow) -- the
// account-wide claim-history dedup is a structural no-op. An already-awarded
// reward that Twitch re-offers with progress reset to 0 / isClaimed=false
// (a "ghost") is re-farmed forever, and nothing about a claim survives a
// restart.
//
// This file does NOT try to make claim history confirm (that data shape
// cannot support it -- see the comment above and models/reward_identity.go,
// both frozen). Instead it adds a durable, evidence-ranked ledger fed
// exclusively by evidence THIS MINER ITSELF witnesses:
//
//   - E1 claim_accepted    (rank 3): our own ClaimDrop mutation returned a
//     fresh, authoritative acceptance.
//   - E2 claim_already     (rank 2): our own ClaimDrop mutation returned an
//     authoritative already-claimed reconciliation.
//   - E3 inventory_claimed (rank 1): the raw inventory reports
//     self.isClaimed==true for a drop we may not have claimed ourselves this
//     run (e.g. claimed from another session, or before a restart).
//
// A ledger row is created ONLY when the evidence carries an instance ID, or a
// full campaign+drop composite (CREATION INVARIANT -- benefit-only/name-only
// evidence can enrich an existing row but never creates one, matching the
// schema's two unique indexes, both of which are total over such rows).
//
// The ledger gates ONLY future broker-facing drop *assignment*
// (updateStreamerCampaigns -- see brokerView/S6): it never gates Drop.CanClaim
// or TwitchClient.ClaimDrop, and any failure to read or write it fails
// OPEN -- a disabled/unreachable ledger farms exactly as if this feature did
// not exist. See SPECIFICATIONS.md "Drop Skip Ledger Module Schema" for the
// full schema/state-machine writeup.

// SkipLedger is the durable, evidence-ranked record of drop rewards this
// account has authoritatively been granted (or Twitch's inventory reports as
// already claimed), scoped to one account_key. It is consulted ONLY to decide
// whether a re-offered "ghost" of an already-granted reward should be
// excluded from broker-facing assignment (see brokerView); it never affects
// the claim mutation itself.
type SkipLedger struct {
	db         *database.DB
	accountKey string
	// now is injectable so tests can control observation timestamps, exactly
	// like CampaignCatalog.now (see catalog.go).
	now func() time.Time
}

type skipLedgerModule struct{}

func (skipLedgerModule) Name() string { return "drop_skip_ledger" }

func (skipLedgerModule) Migrations() []database.Migration {
	return []database.Migration{
		{
			Version:     1,
			Description: "Create drop_reward_skips ghost-skip ledger",
			// Additive, standalone table. instance_id is deliberately part of the
			// composite unique index's tuple (not just its own index): two
			// server-minted instances of the same campaign+drop+occurrence are two
			// distinct grants and must never collapse onto one row (see the
			// account-isolation/instance-isolation tests in skipledger_test.go).
			SQL: `
				CREATE TABLE IF NOT EXISTS drop_reward_skips (
					id             INTEGER PRIMARY KEY AUTOINCREMENT,
					account_key    TEXT    NOT NULL,
					game_id        TEXT    NOT NULL DEFAULT '',
					instance_id    TEXT    NOT NULL DEFAULT '',
					benefit_id     TEXT    NOT NULL DEFAULT '',
					campaign_id    TEXT    NOT NULL DEFAULT '',
					drop_id        TEXT    NOT NULL DEFAULT '',
					reward_name    TEXT    NOT NULL DEFAULT '',
					occ_start_ms   INTEGER NOT NULL DEFAULT 0,
					occ_end_ms     INTEGER NOT NULL DEFAULT 0,
					occ_source     INTEGER NOT NULL DEFAULT 0,
					occ_known      INTEGER NOT NULL DEFAULT 0,
					evidence_class TEXT    NOT NULL,
					evidence_rank  INTEGER NOT NULL,
					state          TEXT    NOT NULL DEFAULT 'active',
					state_reason   TEXT    NOT NULL DEFAULT '',
					created_at_ms  INTEGER NOT NULL,
					updated_at_ms  INTEGER NOT NULL,
					resolved_at_ms INTEGER NOT NULL DEFAULT 0
				);

				CREATE UNIQUE INDEX IF NOT EXISTS ux_drop_skips_instance
					ON drop_reward_skips(account_key, instance_id) WHERE instance_id <> '';

				CREATE UNIQUE INDEX IF NOT EXISTS ux_drop_skips_composite
					ON drop_reward_skips(account_key, campaign_id, drop_id, occ_start_ms, occ_end_ms, instance_id)
					WHERE campaign_id <> '' AND drop_id <> '';

				CREATE INDEX IF NOT EXISTS idx_drop_skips_benefit
					ON drop_reward_skips(account_key, benefit_id) WHERE benefit_id <> '';
				CREATE INDEX IF NOT EXISTS idx_drop_skips_composite_lookup
					ON drop_reward_skips(account_key, campaign_id, drop_id);
				CREATE INDEX IF NOT EXISTS idx_drop_skips_state
					ON drop_reward_skips(account_key, state);
			`,
		},
	}
}

// NewSkipLedger registers the drop_skip_ledger module against db and returns
// a store scoped to accountKey (config.StorageKey(), never the mutable
// login). A migration/registration failure is returned to the caller as a
// clean (nil, error) -- never a partially initialized *SkipLedger -- who MUST
// fail open rather than block startup on it -- see the S1 wiring in
// internal/miner/miner.go (mirrors drops.NewCampaignCatalog exactly).
//
// No injectable seam guards db.RegisterModule here -- none is needed:
// database.DB embeds an exported *sql.DB (internal/database/database.go), so
// a test can reach a REAL RegisterModule/migration failure directly -- open a
// private, non-singleton *database.DB over its own temp file (the
// internal/miner/srap_test.go openRawMinerDB / internal/streamerlifecycle
// openRawDB pattern, already used in 13+ places in this repo) and seed it
// with a conflicting object before ever calling NewSkipLedger. See
// TestNewSkipLedgerMigrationFailureNoPartialState.
func NewSkipLedger(db *database.DB, accountKey string) (*SkipLedger, error) {
	if err := db.RegisterModule(skipLedgerModule{}); err != nil {
		return nil, fmt.Errorf("failed to register drop_skip_ledger module: %w", err)
	}
	return &SkipLedger{db: db, accountKey: accountKey, now: time.Now}, nil
}

// ---------------------------------------------------------------------------
// Evidence classes and the Observe() write path (S2/S3/S4 integration seams).
// ---------------------------------------------------------------------------

// skipEvidenceClass identifies which observation produced a ledger row/update.
// Rank is monotone non-decreasing on a row: a later, weaker observation can
// enrich empty fields but never downgrades evidence_class/evidence_rank.
type skipEvidenceClass string

const (
	evidenceClaimAccepted    skipEvidenceClass = "claim_accepted"
	evidenceClaimAlready     skipEvidenceClass = "claim_already"
	evidenceInventoryClaimed skipEvidenceClass = "inventory_claimed"
)

// rank maps each evidence class to its strength (3 strongest); an unknown
// class ranks 0 so it can never accidentally out-rank a real observation.
func (c skipEvidenceClass) rank() int {
	switch c {
	case evidenceClaimAccepted:
		return 3
	case evidenceClaimAlready:
		return 2
	case evidenceInventoryClaimed:
		return 1
	default:
		return 0
	}
}

// Ledger row lifecycle states (see SPECIFICATIONS.md "Drop Skip Ledger Module
// Schema" for the full state machine). Time alone never transitions a row --
// every transition is driven by a fresh observation (Observe) or a self-heal
// pass over the currently tracked candidates (Reconcile).
const (
	skipStateActive      = "active"
	skipStateReleased    = "released"
	skipStateConflicting = "conflicting"
)

// skipEvidence is one caller-supplied observation: our own authoritative
// claim outcome (E1/E2) or a raw-inventory isClaimed sighting (E3), plus the
// identity bundle it carries. Fields mirror models.RewardIdentity's IDs so
// Observe can be built directly from a Drop.Identity() result.
type skipEvidence struct {
	class      skipEvidenceClass
	gameID     string
	benefitID  string
	instanceID string
	campaignID string
	dropID     string
	// name is DIAGNOSTICS ONLY (stored in reward_name for debugging) -- it is
	// NEVER consulted for matching (see decideSkip).
	name   string
	window models.EntitlementWindow
}

// skipEvidenceFromIdentity builds a skipEvidence from a models.RewardIdentity
// (the shape Drop.Identity returns), used by the claim seams (S2/S3).
func skipEvidenceFromIdentity(class skipEvidenceClass, id models.RewardIdentity) skipEvidence {
	return skipEvidence{
		class:      class,
		gameID:     id.GameID,
		benefitID:  id.BenefitID,
		instanceID: id.InstanceID,
		campaignID: id.CampaignID,
		dropID:     id.DropID,
		name:       id.CanonicalName,
		window:     id.Window,
	}
}

// skipRowColumns is the fixed column list every SELECT against
// drop_reward_skips uses, matched positionally by scanSkipRow -- keeping this
// in one place means findRow/matchingActiveRows/Snapshot can never drift out
// of sync with each other's scan order.
const skipRowColumns = "id, instance_id, benefit_id, campaign_id, drop_id, game_id, occ_start_ms, occ_end_ms, occ_source, occ_known, state"

// skipRow is one ledger row's decision-relevant columns (never reward_name,
// timestamps, or state_reason -- those are diagnostics only and irrelevant to
// Observe/Reconcile/Decide).
type skipRow struct {
	id         int64
	instanceID string
	benefitID  string
	campaignID string
	dropID     string
	gameID     string
	window     models.EntitlementWindow
	state      string
}

// scanSkipRow scans one skipRowColumns-shaped row via either *sql.Row.Scan or
// *sql.Rows.Scan (both share this signature), reconstructing the
// EntitlementWindow from its three stored columns.
func scanSkipRow(scan func(dest ...any) error) (skipRow, error) {
	var (
		r                   skipRow
		startMs, endMs      int64
		sourceInt, knownInt int
	)
	if err := scan(&r.id, &r.instanceID, &r.benefitID, &r.campaignID, &r.dropID, &r.gameID,
		&startMs, &endMs, &sourceInt, &knownInt, &r.state); err != nil {
		return skipRow{}, err
	}
	r.window = models.EntitlementWindow{
		Start:  msToTime(startMs),
		End:    msToTime(endMs),
		Source: models.WindowSource(sourceInt),
		Known:  knownInt != 0,
	}
	return r, nil
}

// Observe records one evidence sighting, called ONLY AFTER the network call
// (claim mutation or inventory read) that produced it has already returned --
// no network call ever happens inside Observe's transaction. Per the CREATION
// INVARIANT, benefit-only/name-only evidence (no instance ID and no full
// campaign+drop composite) can never create a row; it is silently dropped
// rather than erroring, since the proven claim/inventory contract this repo
// observes always carries at least one of those two alongside any evidence
// worth recording.
func (l *SkipLedger) Observe(ctx context.Context, ev skipEvidence) error {
	if ev.instanceID == "" && (ev.campaignID == "" || ev.dropID == "") {
		return nil
	}
	return l.db.WithTx(ctx, func(tx *sql.Tx) error {
		row, err := l.findRow(tx, ev)
		if err != nil {
			return err
		}
		now := l.now().UnixMilli()
		if row == nil {
			return l.insertRow(tx, ev, now)
		}
		return l.enrichRow(tx, row.id, ev, now)
	})
}

// findRow implements the two-step lookup: first an exact instance match (a
// server-minted instance ID is a unique per-grant handle), then -- only when
// that misses -- a composite match scoped to the EXACT occurrence bounds
// (occ_start_ms/occ_end_ms, matching the composite unique index's tuple
// exactly), restricted to instance-LESS rows only.
//
// That restriction is not a tie-break among several candidates: by the time
// this composite lookup runs, either ev.instanceID == "" (never restricted to
// begin with), or the instance lookup just above already searched the WHOLE
// account for ev.instanceID and found nothing -- and instance_id is globally
// unique per account (ux_drop_skips_instance), so that proves no row
// anywhere carries it. So a composite match on `instance_id = ev.instanceID`
// can never fire here; the composite unique index (ux_drop_skips_composite)
// further guarantees at most one instance-less row exists for this exact
// (campaign, drop, occurrence) tuple, so no ORDER BY/LIMIT tie-break is
// needed either. A row bearing a DIFFERENT non-empty instance never matches
// this query at all -- a second minted instance can never enrich, reuse, or
// overwrite the first instance's row (INVARIANT 2/6).
func (l *SkipLedger) findRow(tx *sql.Tx, ev skipEvidence) (*skipRow, error) {
	if ev.instanceID != "" {
		row := tx.QueryRow(`
			SELECT `+skipRowColumns+`
			FROM drop_reward_skips
			WHERE account_key = ? AND instance_id = ?`,
			l.accountKey, ev.instanceID)
		r, err := scanSkipRow(row.Scan)
		switch {
		case err == nil:
			return &r, nil
		case errors.Is(err, sql.ErrNoRows):
			// Fall through to the composite fallback below.
		default:
			return nil, err
		}
	}
	if ev.campaignID != "" && ev.dropID != "" {
		startMs, endMs := msOrZero(ev.window.Start), msOrZero(ev.window.End)
		row := tx.QueryRow(`
			SELECT `+skipRowColumns+`
			FROM drop_reward_skips
			WHERE account_key = ? AND campaign_id = ? AND drop_id = ?
			  AND occ_start_ms = ? AND occ_end_ms = ? AND instance_id = ''`,
			l.accountKey, ev.campaignID, ev.dropID, startMs, endMs)
		r, err := scanSkipRow(row.Scan)
		switch {
		case err == nil:
			return &r, nil
		case errors.Is(err, sql.ErrNoRows):
			return nil, nil
		default:
			return nil, err
		}
	}
	return nil, nil
}

// insertRow creates a fresh row for evidence that matched nothing existing.
// created_at_ms == updated_at_ms on insert; created_at_ms is never written
// again by any other method in this file.
func (l *SkipLedger) insertRow(tx *sql.Tx, ev skipEvidence, nowMs int64) error {
	startMs, endMs := msOrZero(ev.window.Start), msOrZero(ev.window.End)
	known := 0
	if ev.window.Known {
		known = 1
	}
	_, err := tx.Exec(`
		INSERT INTO drop_reward_skips
			(account_key, game_id, instance_id, benefit_id, campaign_id, drop_id, reward_name,
			 occ_start_ms, occ_end_ms, occ_source, occ_known, evidence_class, evidence_rank,
			 state, state_reason, created_at_ms, updated_at_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.accountKey, ev.gameID, ev.instanceID, ev.benefitID, ev.campaignID, ev.dropID, ev.name,
		startMs, endMs, int(ev.window.Source), known, string(ev.class), ev.class.rank(),
		skipStateActive, "", nowMs, nowMs)
	return err
}

// enrichRow applies the ENRICH-ONLY update to an existing row: every column
// is upgraded via a CASE guard that only ever fills an empty/unknown value,
// never overwrites a populated one, and evidence_rank only ever increases.
// The re-arm predicate (the `state` CASE) fires when this observation is
// authoritative for the row's EXACT instance, or adopts an instance-less
// composite row by a real instance -- both READ THE ROW'S PRE-UPDATE
// instance_id (standard SQL UPDATE semantics: every SET expression sees the
// row as it was before this statement), so it can never be satisfied by a
// DIFFERENT instance (INVARIANT 5/6). `state_reason` additionally requires
// the row's PRE-UPDATE state to NOT already be 'active': a repeat
// observation of an already-active row satisfies the same instance/adoption
// predicate but re-arms nothing, and state_reason must not claim it did (the
// column would otherwise lie about a transition that never happened) --
// see TestEnrichRowDoesNotClaimRearmWhenAlreadyActive. created_at_ms never
// appears here.
func (l *SkipLedger) enrichRow(tx *sql.Tx, id int64, ev skipEvidence, nowMs int64) error {
	startMs, endMs := msOrZero(ev.window.Start), msOrZero(ev.window.End)
	known := 0
	if ev.window.Known {
		known = 1
	}
	rearm := 0
	if ev.instanceID != "" {
		rearm = 1
	}
	reason := "rearmed_by_" + string(ev.class)

	_, err := tx.Exec(`
		UPDATE drop_reward_skips SET
			instance_id   = CASE WHEN instance_id = '' THEN ? ELSE instance_id END,
			benefit_id    = CASE WHEN benefit_id  = '' THEN ? ELSE benefit_id  END,
			campaign_id   = CASE WHEN campaign_id = '' THEN ? ELSE campaign_id END,
			drop_id       = CASE WHEN drop_id     = '' THEN ? ELSE drop_id     END,
			game_id       = CASE WHEN game_id     = '' THEN ? ELSE game_id     END,
			reward_name   = CASE WHEN reward_name = '' THEN ? ELSE reward_name END,
			occ_start_ms  = CASE WHEN occ_known = 0 AND ? = 1 THEN ? ELSE occ_start_ms END,
			occ_end_ms    = CASE WHEN occ_known = 0 AND ? = 1 THEN ? ELSE occ_end_ms   END,
			occ_source    = CASE WHEN occ_known = 0 AND ? = 1 THEN ? ELSE occ_source   END,
			occ_known     = CASE WHEN occ_known = 0 AND ? = 1 THEN ? ELSE occ_known    END,
			evidence_class = CASE WHEN ? > evidence_rank THEN ? ELSE evidence_class END,
			evidence_rank  = MAX(evidence_rank, ?),
			state = CASE WHEN ? = 1 AND (instance_id = ? OR instance_id = '') THEN 'active' ELSE state END,
			state_reason = CASE WHEN ? = 1 AND (instance_id = ? OR instance_id = '') AND state <> 'active' THEN ? ELSE state_reason END,
			updated_at_ms = ?
		WHERE id = ?`,
		ev.instanceID,
		ev.benefitID,
		ev.campaignID,
		ev.dropID,
		ev.gameID,
		ev.name,
		known, startMs,
		known, endMs,
		known, int(ev.window.Source),
		known, known,
		ev.class.rank(), string(ev.class),
		ev.class.rank(),
		rearm, ev.instanceID,
		rearm, ev.instanceID, reason,
		nowMs,
		id,
	)
	return err
}

// ---------------------------------------------------------------------------
// Snapshot + Decide: the pure, I/O-free broker-facing read path (S6).
// ---------------------------------------------------------------------------

// compositeKey groups ledger rows by (campaign_id, drop_id) for the composite
// lookup tier.
type compositeKey struct {
	campaignID, dropID string
}

// skipSnapshot is a point-in-time, read-only copy of one account's ledger
// rows, loaded ONCE per broker pass (updateStreamerCampaigns) so decideSkip
// runs as a pure, I/O-free function over a consistent view for every
// candidate evaluated that pass. A nil *skipSnapshot means "no ledger
// information available" (no ledger wired, or the snapshot failed to load)
// and every candidate fails open to FARM.
type skipSnapshot struct {
	byInstance  map[string]skipRow
	byComposite map[compositeKey][]skipRow
	byBenefit   map[string][]skipRow
}

// Snapshot loads every ledger row for this account into an in-memory
// skipSnapshot. ctx is honored so a caller can bound (or, in tests,
// deterministically fail) the read; a cancelled/expired ctx returns an error
// with NO partial snapshot, which the broker-facing caller (drops.go) treats
// identically to "no ledger" (fail open).
func (l *SkipLedger) Snapshot(ctx context.Context) (*skipSnapshot, error) {
	rows, err := l.db.QueryContext(ctx, `
		SELECT `+skipRowColumns+`
		FROM drop_reward_skips
		WHERE account_key = ?`, l.accountKey)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	snap := &skipSnapshot{
		byInstance:  make(map[string]skipRow),
		byComposite: make(map[compositeKey][]skipRow),
		byBenefit:   make(map[string][]skipRow),
	}
	for rows.Next() {
		r, err := scanSkipRow(rows.Scan)
		if err != nil {
			return nil, err
		}
		if r.instanceID != "" {
			snap.byInstance[r.instanceID] = r
		}
		if r.campaignID != "" && r.dropID != "" {
			key := compositeKey{r.campaignID, r.dropID}
			snap.byComposite[key] = append(snap.byComposite[key], r)
		}
		if r.benefitID != "" {
			snap.byBenefit[r.benefitID] = append(snap.byBenefit[r.benefitID], r)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return snap, nil
}

// gameConflict reports a hard mismatch: both sides carry a non-empty game ID
// and they differ. A missing game on either side is never a conflict --
// mirrors models.gamesConflict (internal/models/reward_identity.go, frozen).
func gameConflict(a, b string) bool {
	return a != "" && b != "" && a != b
}

// instanceRow looks up an exact instance match, applying INVARIANT 4 (game
// conflict excludes a row from ALL matching, applied before any decision
// step) at the lookup itself.
func (s *skipSnapshot) instanceRow(instanceID, gameID string) (skipRow, bool) {
	r, ok := s.byInstance[instanceID]
	if !ok || gameConflict(r.gameID, gameID) {
		return skipRow{}, false
	}
	return r, true
}

// activeComposite returns this snapshot's ACTIVE rows for (campaignID,
// dropID), excluding any game-conflicting row (INVARIANT 4).
func (s *skipSnapshot) activeComposite(campaignID, dropID, gameID string) []skipRow {
	var out []skipRow
	for _, r := range s.byComposite[compositeKey{campaignID, dropID}] {
		if r.state != skipStateActive || gameConflict(r.gameID, gameID) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// activeBenefit returns this snapshot's ACTIVE rows for benefitID, excluding
// any game-conflicting row (INVARIANT 4).
func (s *skipSnapshot) activeBenefit(benefitID, gameID string) []skipRow {
	var out []skipRow
	for _, r := range s.byBenefit[benefitID] {
		if r.state != skipStateActive || gameConflict(r.gameID, gameID) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// decide is the PURE, I/O-free ghost-skip decision (design doc "Decide(C) ->
// SKIP | FARM"): candidate c (a drop's RewardIdentity) plus whether Twitch
// currently authorizes claiming it (canClaim -- Drop.CanClaim() is on *Drop,
// not RewardIdentity, so callers pass it explicitly). A nil receiver (no
// snapshot -- no ledger wired, or the snapshot failed to load) always fails
// open to FARM. reward_name is NEVER consulted. The four tiers below run in
// order and each may return early; falling through a tier (e.g. an
// instance-minted candidate the ledger has no record of at all) is
// deliberate -- it lets a genuinely fresh instance still be caught by a
// strong composite/benefit-window match at a later tier instead of being
// forced to FARM.
func (s *skipSnapshot) decide(c models.RewardIdentity, canClaim bool) (skip bool, reason string) {
	if s == nil {
		return false, "no_ledger"
	}

	// 1. INSTANCE
	if c.InstanceID != "" {
		if r, ok := s.instanceRow(c.InstanceID, c.GameID); ok {
			switch r.state {
			case skipStateActive:
				return true, "same_instance"
			case skipStateConflicting:
				return false, "conflicting_evidence"
			default: // released
				return false, "released"
			}
		}

		// No record of THIS exact instance. Two situations still argue FARM
		// even though we fall through to weaker tiers below on a full miss:
		var composite, benefit []skipRow
		if c.CampaignID != "" && c.DropID != "" {
			composite = s.activeComposite(c.CampaignID, c.DropID, c.GameID)
		}
		if c.BenefitID != "" {
			benefit = s.activeBenefit(c.BenefitID, c.GameID)
		}
		// (a) some OTHER instance is already recorded active for this
		// composite/benefit: this is provably a new, distinct occurrence.
		for _, r := range composite {
			if r.instanceID != "" {
				return false, "new_minted_instance"
			}
		}
		for _, r := range benefit {
			if r.instanceID != "" {
				return false, "new_minted_instance"
			}
		}
		// (b) an instance-LESS composite/benefit row is on file and Twitch now
		// authorizes claiming a freshly-minted instance: the old row is stale
		// (superseded), never a reason to withhold a claimable fresh grant.
		if canClaim {
			for _, r := range composite {
				if r.instanceID == "" {
					return false, "composite_row_superseded"
				}
			}
			for _, r := range benefit {
				if r.instanceID == "" {
					return false, "composite_row_superseded"
				}
			}
		}
	}

	// 2. COMPOSITE
	if c.CampaignID != "" && c.DropID != "" {
		for _, r := range s.activeComposite(c.CampaignID, c.DropID, c.GameID) {
			if r.window.Decidable() && c.Window.Decidable() && r.window.DisjointFrom(c.Window) {
				continue // provably a different occurrence -- keep looking
			}
			return true, "same_composite"
		}
	}

	// 3. BENEFIT
	if c.BenefitID != "" {
		for _, r := range s.activeBenefit(c.BenefitID, c.GameID) {
			if r.window.Decidable() && c.Window.Decidable() && r.window.Overlaps(c.Window) {
				return true, "same_benefit_overlapping_window"
			}
		}
		return false, "benefit_window_undecidable"
	}

	// 4. No evidence anywhere.
	return false, "no_match"
}

// brokerView returns the campaign view fed to the streamer eligibility
// evaluator and (on success) to Stream.SetCampaigns: campaign.Clone() with
// every drop whose decide()==SKIP removed. On any failure to consult the
// ledger (snap == nil, meaning no ledger wired OR its snapshot failed to
// load) it fails OPEN, returning the ORIGINAL campaign unchanged -- no clone,
// no filtering -- so a ledger outage can never suppress real farming
// (INVARIANT 9/12). The source campaign, d.campaigns, the catalog, and every
// other published *models.Campaign are never mutated; only the clone
// returned here (when one is built) is filtered.
//
// Every suppression is logged at DEBUG (matching the pipeline's existing
// per-decision convention -- logDropIneligible in drops.go never fires above
// DEBUG either) with the decide() reason, so a wrongly-active row is
// diagnosable from the log instead of requiring an operator to open miner.db
// by hand. This runs once per campaign per broker pass (not once per
// streamer -- see updateStreamerCampaigns' views loop), so it is cheap
// regardless of streamer count.
//
// DEFENSIVE BOUNDARY HARDENING: models.Campaign.Clone (models/campaign.go)
// does `dc := *d` for every element of c.Drops, so a nil element would panic
// INSIDE Clone before the filtering loop below ever ran. This is not a claim
// that a nil *models.Drop is reachable from production today
// (models.NewDropFromGQL, the only production constructor, never returns
// one) -- it is that brokerView sits on the broker-assignment path and
// should not be the thing that panics if a nil ever does appear, and should
// agree with suppressedDrops/Reconcile (below in this file), which already
// skip nil drops via their own guards. See hasNilDrop below.
func brokerView(campaign *models.Campaign, snap *skipSnapshot) *models.Campaign {
	if snap == nil {
		return campaign
	}

	// Shallow-copy the Campaign struct -- this does NOT dereference any
	// drop, so it is safe even when campaign.Drops holds a nil element. Only
	// when a nil element is actually present do we build a NEW Drops slice
	// (a new slice header over the SAME, still non-nil, element pointers)
	// and assign it to the copy; the source campaign and its source Drops
	// slice are never touched either way. Clone() then runs on the
	// sanitized copy exactly as before, preserving its existing deep-copy
	// behaviour for Drops, Channels, ClaimedDropNames and ACL.
	sanitized := *campaign
	if hasNilDrop(campaign.Drops) {
		clean := make([]*models.Drop, 0, len(campaign.Drops))
		for _, d := range campaign.Drops {
			if d != nil {
				clean = append(clean, d)
			}
		}
		sanitized.Drops = clean
	}

	view := sanitized.Clone()
	gameID := campaignGameID(campaign)
	fallback := campaignFallbackWindow(campaign)

	kept := make([]*models.Drop, 0, len(view.Drops))
	for _, drop := range view.Drops {
		if drop == nil { // defense-in-depth; the sanitization above already excludes nils
			continue
		}
		id := drop.Identity(gameID, campaign.ID, fallback)
		if skip, reason := snap.decide(id, drop.CanClaim()); skip {
			slog.Debug("Drop suppressed by ghost-skip ledger",
				"campaign", campaign.Name, "campaignID", campaign.ID,
				"drop", drop.Name, "dropID", drop.ID, "reason", reason)
			continue
		}
		kept = append(kept, drop)
	}
	view.Drops = kept
	return view
}

// hasNilDrop reports whether drops contains any nil element, WITHOUT
// dereferencing any element. brokerView uses it to decide whether the
// pre-Clone sanitization pass (which allocates a new slice) is needed at
// all, so the common case -- no nil drops -- allocates nothing extra beyond
// the existing Clone() behaviour.
func hasNilDrop(drops []*models.Drop) bool {
	for _, d := range drops {
		if d == nil {
			return true
		}
	}
	return false
}

// SuppressedDrop is one drop currently excluded from broker-facing
// assignment by the skip ledger, for read-only diagnostics -- it is never
// consulted by any decision path (see DropsTracker.SuppressedDrops).
type SuppressedDrop struct {
	CampaignID   string
	CampaignName string
	DropID       string
	DropName     string
	Reason       string
}

// suppressedDrops computes, without building any campaign clones, which
// drops among campaigns are currently ghost-skipped by snap. Shared, pure,
// read-only logic behind both the full-sync pipeline's suppressed-drop
// counter and the DropsTracker.SuppressedDrops() diagnostics accessor. A nil
// snap (no ledger wired, or a failed snapshot load -- the same fail-open
// condition brokerView treats as "nothing filtered") always yields none.
func suppressedDrops(campaigns []*models.Campaign, snap *skipSnapshot) []SuppressedDrop {
	if snap == nil {
		return nil
	}
	var out []SuppressedDrop
	for _, campaign := range campaigns {
		if campaign == nil {
			continue
		}
		gameID := campaignGameID(campaign)
		fallback := campaignFallbackWindow(campaign)
		for _, drop := range campaign.Drops {
			if drop == nil {
				continue
			}
			id := drop.Identity(gameID, campaign.ID, fallback)
			if skip, reason := snap.decide(id, drop.CanClaim()); skip {
				out = append(out, SuppressedDrop{
					CampaignID: campaign.ID, CampaignName: campaign.Name,
					DropID: drop.ID, DropName: drop.Name, Reason: reason,
				})
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Reconcile: self-heal writes over the currently tracked candidates (S5).
// ---------------------------------------------------------------------------

// Reconcile self-heals ledger rows against the current full-sync candidate
// set in ONE transaction (so a partial pass can never half-apply): SH1-SH4
// (see reconcileTransition) plus the positive resolution that releases a
// stale instance-less composite row once a real instance-bearing row exists
// for the same composite. It never mutates any campaign -- read-only over
// candidates, writes only to the ledger.
//
// ctx is honored (concurrency.md: no blocking work that ignores ctx) --
// db.WithTx holds db.mu.RLock and the process's single SQLite connection
// (SetMaxOpenConns(1)) for the whole transaction, and every other DB user
// blocks behind that lock/connection until it releases, so a caller-bound or
// bounded ctx caps how long a slow/hung Reconcile can hold either: on
// cancellation/deadline the in-flight statement errors, the whole
// transaction rolls back, and the lock/connection are released -- callers
// (drops.go) derive this from the tracker's own lifecycle context with a
// bounded timeout (skipLedgerCtx), so Stop() is never blocked behind it
// indefinitely.
//
// This intentionally keeps ONE transaction for the whole candidate set
// rather than one per drop: on a timeout/cancellation the WHOLE pass rolls
// back atomically and is simply retried in full on the next sync, instead of
// leaving some drops reconciled and others not -- a half-applied self-heal
// pass would be strictly worse than "unchanged this cycle, try again next
// cycle" (the ledger's rows are never used for anything time-critical, only
// diagnostics and the next Decide, both fine with a whole-cycle delay).
func (l *SkipLedger) Reconcile(ctx context.Context, candidates []*models.Campaign) error {
	return l.db.WithTx(ctx, func(tx *sql.Tx) error {
		now := l.now().UnixMilli()
		for _, c := range candidates {
			if c == nil {
				continue
			}
			gameID := campaignGameID(c)
			fallback := campaignFallbackWindow(c)
			for _, drop := range c.Drops {
				if drop == nil {
					continue
				}
				id := drop.Identity(gameID, c.ID, fallback)
				if err := l.reconcileIdentity(tx, id, drop.CanClaim(), now); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// reconcileIdentity applies SH1-SH4 to every ACTIVE row matched (by composite
// or benefit, subject to the game-conflict filter) against candidate c, then
// runs the positive-resolution step for c's composite key.
func (l *SkipLedger) reconcileIdentity(tx *sql.Tx, c models.RewardIdentity, canClaim bool, nowMs int64) error {
	rows, err := l.matchingActiveRows(tx, c)
	if err != nil {
		return err
	}

	// SH4's "no row for C.instance" guard: true when NO row anywhere (active
	// or not) already carries c's exact instance -- checked first against the
	// matched set (cheap, no extra query in the common case), falling back to
	// a targeted existence check only when necessary.
	hasInstanceRow := false
	if c.InstanceID != "" {
		for _, r := range rows {
			if r.instanceID == c.InstanceID {
				hasInstanceRow = true
				break
			}
		}
		if !hasInstanceRow {
			exists, err := l.instanceRowExists(tx, c.InstanceID)
			if err != nil {
				return err
			}
			hasInstanceRow = exists
		}
	}

	for _, r := range rows {
		newState, reason := reconcileTransition(c, canClaim, r, hasInstanceRow)
		if newState == "" {
			continue
		}
		if err := l.transitionRow(tx, r.id, newState, reason, nowMs); err != nil {
			return err
		}
	}

	if c.CampaignID != "" && c.DropID != "" {
		if err := l.resolveConflictingComposite(tx, c.CampaignID, c.DropID, nowMs); err != nil {
			return err
		}
	}
	return nil
}

// reconcileTransition is the pure SH1-SH4 rule table (design doc
// "Reconcile(candidates)"), evaluated against one ACTIVE row r (matchingActiveRows
// already applies the game-conflict filter and the state=active restriction,
// so neither is re-checked here). Returns ("", "") when no rule applies.
func reconcileTransition(c models.RewardIdentity, canClaim bool, r skipRow, hasInstanceRow bool) (newState, reason string) {
	switch {
	case c.InstanceID != "" && r.instanceID != "" && r.instanceID != c.InstanceID:
		// SH1: a DIFFERENT minted instance now exists for the same composite/
		// benefit -- the recorded row's grant and this candidate are two
		// distinct occurrences.
		return skipStateReleased, "new_minted_instance"
	case r.window.Decidable() && c.Window.Decidable() && r.window.DisjointFrom(c.Window):
		// SH2: both windows are provably known and disjoint -- a new
		// occurrence, regardless of instance IDs.
		return skipStateReleased, "disjoint_occurrence"
	case c.InstanceID != "" && r.instanceID == c.InstanceID && canClaim:
		// SH3: Twitch is offering the EXACT instance we already have a ledger
		// row for as claimable again -- suspicious; flag for FARM/investigation
		// rather than silently trusting either side.
		return skipStateConflicting, "claimable_same_instance"
	case r.instanceID == "" && c.InstanceID != "" && !hasInstanceRow && canClaim:
		// SH4: an instance-less composite/benefit row is on file, and Twitch
		// now offers a freshly-minted, claimable instance -- the old row alone
		// must not withhold a real, currently-claimable grant.
		return skipStateConflicting, "minted_instance_over_composite_row"
	default:
		return "", ""
	}
}

// matchingActiveRows returns the deduplicated, deterministically-ordered
// union of this account's ACTIVE rows matched by composite (campaign_id +
// drop_id) or by benefit_id, excluding any row that game-conflicts with c
// (INVARIANT 4, applied before either match is returned).
func (l *SkipLedger) matchingActiveRows(tx *sql.Tx, c models.RewardIdentity) ([]skipRow, error) {
	byID := make(map[int64]skipRow)

	if c.CampaignID != "" && c.DropID != "" {
		rows, err := l.queryRows(tx, `
			SELECT `+skipRowColumns+`
			FROM drop_reward_skips
			WHERE account_key = ? AND state = ? AND campaign_id = ? AND drop_id = ?`,
			l.accountKey, skipStateActive, c.CampaignID, c.DropID)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			byID[r.id] = r
		}
	}
	if c.BenefitID != "" {
		rows, err := l.queryRows(tx, `
			SELECT `+skipRowColumns+`
			FROM drop_reward_skips
			WHERE account_key = ? AND state = ? AND benefit_id = ?`,
			l.accountKey, skipStateActive, c.BenefitID)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			byID[r.id] = r
		}
	}

	out := make([]skipRow, 0, len(byID))
	for _, r := range byID {
		if gameConflict(r.gameID, c.GameID) {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out, nil
}

// queryRows runs query against tx and scans every result row.
func (l *SkipLedger) queryRows(tx *sql.Tx, query string, args ...any) ([]skipRow, error) {
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []skipRow
	for rows.Next() {
		r, err := scanSkipRow(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// instanceRowExists reports whether ANY row (any state) already carries
// instanceID for this account -- SH4's fallback guard when the matched
// active-row set alone doesn't already answer the question.
func (l *SkipLedger) instanceRowExists(tx *sql.Tx, instanceID string) (bool, error) {
	var n int
	err := tx.QueryRow(`SELECT COUNT(1) FROM drop_reward_skips WHERE account_key = ? AND instance_id = ?`,
		l.accountKey, instanceID).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// transitionRow applies a state/state_reason transition. resolved_at_ms is
// stamped only when the new state is 'released' (a terminal-for-now
// resolution, purely diagnostic -- released rows can still be re-armed later
// by Observe, which would naturally produce a fresh resolved_at_ms on any
// LATER release).
func (l *SkipLedger) transitionRow(tx *sql.Tx, id int64, state, reason string, nowMs int64) error {
	if state == skipStateReleased {
		_, err := tx.Exec(`UPDATE drop_reward_skips SET state=?, state_reason=?, updated_at_ms=?, resolved_at_ms=? WHERE id=?`,
			state, reason, nowMs, nowMs, id)
		return err
	}
	_, err := tx.Exec(`UPDATE drop_reward_skips SET state=?, state_reason=?, updated_at_ms=? WHERE id=?`,
		state, reason, nowMs, id)
	return err
}

// resolveConflictingComposite is the "positive resolution" step: a
// conflicting, instance-less composite row for (campaignID, dropID) moves to
// released once ANY instance-bearing row exists for that same composite key
// -- the instance-bearing row is strictly more authoritative going forward.
func (l *SkipLedger) resolveConflictingComposite(tx *sql.Tx, campaignID, dropID string, nowMs int64) error {
	_, err := tx.Exec(`
		UPDATE drop_reward_skips
		SET state = ?, state_reason = ?, updated_at_ms = ?, resolved_at_ms = ?
		WHERE account_key = ? AND campaign_id = ? AND drop_id = ? AND instance_id = '' AND state = ?
		  AND EXISTS (
			SELECT 1 FROM drop_reward_skips r2
			WHERE r2.account_key = ? AND r2.campaign_id = ? AND r2.drop_id = ? AND r2.instance_id <> ''
		  )`,
		skipStateReleased, "superseded_by_instance_row", nowMs, nowMs,
		l.accountKey, campaignID, dropID, skipStateConflicting,
		l.accountKey, campaignID, dropID)
	return err
}

// ---------------------------------------------------------------------------
// Retention (storage-only; no automatic sweep is wired anywhere).
// ---------------------------------------------------------------------------

// Prune permanently deletes THIS ACCOUNT's RELEASED rows resolved before
// horizon -- scoped by account_key exactly like every other statement in
// this file, so ledgerA.Prune can never touch ledgerB's rows (the shared,
// process-wide DB backs every account's ledger in the same table; see
// TestPruneIsAccountScoped). It never touches active/conflicting rows (they
// are never pruned, matching drop_campaigns' catalog.go:17-19 "no automatic
// sweep" retention policy) and is never called automatically by this
// package -- an explicit, operator-driven maintenance action only. Decide is
// unaffected either way: a released row already always yields FARM, deleted
// or not.
func (l *SkipLedger) Prune(before time.Time) (int64, error) {
	res, err := l.db.Exec(`DELETE FROM drop_reward_skips WHERE account_key = ? AND state = ? AND resolved_at_ms > 0 AND resolved_at_ms < ?`,
		l.accountKey, skipStateReleased, before.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---------------------------------------------------------------------------
// Shared campaign-identity helpers (used by the claim seams, Reconcile, and
// brokerView -- all built ONLY from exported models.Campaign fields, since
// models is frozen and Campaign.campaignWindow is unexported).
// ---------------------------------------------------------------------------

// campaignGameID returns c's game ID, or "" when c.Game is nil. c itself must
// never be nil -- every caller (claimDropFnFor, Reconcile, suppressedDrops,
// brokerView) either already guards against a nil campaign before calling
// this, or (claimDropFnFor) treats a nil campaign as a caller bug that fails
// loudly before reaching here (see claimDropFnFor's own doc comment,
// drops.go) -- so there is no live path that needs c itself to be nil-safe.
func campaignGameID(c *models.Campaign) string {
	if c.Game == nil {
		return ""
	}
	return c.Game.ID
}

// campaignFallbackWindow mirrors the unexported Campaign.campaignWindow
// (models/campaign.go) exactly, built only from exported fields: a campaign
// with no dates of its own yields an inventory-sourced (if InInventory) or
// sourceless not-Known window; a campaign with dates yields a Known,
// campaign-sourced window. c itself must never be nil -- see
// campaignGameID's doc comment; the two agree on treating a nil c as a
// caller bug rather than a supported input.
func campaignFallbackWindow(c *models.Campaign) models.EntitlementWindow {
	if c.StartAt.IsZero() && c.EndAt.IsZero() {
		src := models.WindowSourceNone
		if c.InInventory {
			src = models.WindowSourceInventory
		}
		return models.EntitlementWindow{Source: src, Known: false}
	}
	return models.EntitlementWindow{
		Start:  c.StartAt,
		End:    c.EndAt,
		Source: models.WindowSourceCampaign,
		Known:  true,
	}
}
