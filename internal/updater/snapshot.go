package updater

import (
	"time"
	"unicode/utf8"
)

// Phase is the updater's live, in-process state machine - what the running
// goroutine is doing RIGHT NOW, as opposed to Outcome (what the last
// completed cycle concluded) or the durable handoff row in store.go (what
// survives a restart). Nothing outside this package consumes Phase yet;
// Snapshot/Snapshot() are the seam a future dashboard surface (Ф5a3) reads.
type Phase string

const (
	// PhaseDormant is entered once, by Run, when the running binary is not a
	// clean release version (a dev/dirty build) - the updater never checks
	// at all and stays in this phase forever.
	PhaseDormant Phase = "dormant"
	// PhaseIdle is the resting state between cycles (or the terminal state
	// of any cycle that did not end up applying an update).
	PhaseIdle Phase = "idle"
	// PhaseChecking covers latestRelease + the version comparison, from the
	// top of checkAndMaybeUpdate until a newer release is found (or not).
	PhaseChecking Phase = "checking"
	// PhaseDownloading covers the asset GET in applyUpdate.
	PhaseDownloading Phase = "downloading"
	// PhaseVerifying covers verifyChecksum (fetching checksums.txt and
	// comparing the sha256).
	PhaseVerifying Phase = "verifying"
	// PhaseSwapping covers replaceExecutable - the atomic rename onto the
	// running binary's path.
	PhaseSwapping Phase = "swapping"
	// PhaseRestartPending is entered once applyUpdate has succeeded and
	// OnUpdate is about to (or has just) requested a clean shutdown; the
	// process is expected to exit and be restarted by its supervisor onto
	// the new binary shortly after this phase is observed.
	PhaseRestartPending Phase = "restart_pending"
)

// Outcome is what the most recently COMPLETED cycle concluded - "" (never
// run / a cycle is currently in flight) until the first cycle finishes.
type Outcome string

const (
	// OutcomeNone is the zero value: no cycle has completed yet.
	OutcomeNone Outcome = ""
	// OutcomeUpToDate: the latest release is not newer than CurrentVersion.
	OutcomeUpToDate Outcome = "up_to_date"
	// OutcomeUpdateAvailable: a newer release exists but was not applied,
	// either because Enabled is false or (see OutcomeGateBlocked) because
	// the Gate withheld it - OutcomeUpdateAvailable specifically covers the
	// Enabled=false case.
	OutcomeUpdateAvailable Outcome = "update_available"
	// OutcomeGateBlocked: a newer release exists, self-update is Enabled,
	// but Options.Gate refused to allow applying it this cycle.
	OutcomeGateBlocked Outcome = "gate_blocked"
	// OutcomeCheckFailed: the release check itself failed (network error,
	// or the current/latest versions could not be compared).
	OutcomeCheckFailed Outcome = "check_failed"
	// OutcomeApplyFailed: a newer release was applied but the attempt
	// failed (download error, fail-closed checksum refusal, checksum
	// mismatch, or a failed binary swap).
	OutcomeApplyFailed Outcome = "apply_failed"
	// OutcomeApplied: the binary was replaced successfully this cycle.
	OutcomeApplied Outcome = "applied"
)

// maxLastErrorLen bounds Snapshot.LastError so an unusually long (or, in
// principle, adversarially long - a malformed release response, a deeply
// wrapped filesystem error) message can never make the snapshot unbounded.
const maxLastErrorLen = 512

// truncateError trims s to at most maxLastErrorLen bytes, never splitting a
// multi-byte UTF-8 rune at the cut point: if byte maxLastErrorLen would land
// mid-rune, the cut backs off to the start of that rune (dropping it
// whole, rather than emitting an invalid trailing partial encoding). Used
// only for the updater's own error text; not a general-purpose string
// helper.
func truncateError(s string) string {
	if len(s) <= maxLastErrorLen {
		return s
	}
	cut := maxLastErrorLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// Snapshot is a point-in-time, self-contained copy of an Updater's current
// status: every field is a plain value (no pointers/slices), so a copy taken
// under Updater.snapMu is safe to read and hold onto long after the lock is
// released.
type Snapshot struct {
	// CurrentVersion/Enabled/CheckInterval mirror the Options an Updater was
	// built with - they never change after New, but are included here so a
	// consumer only ever needs Snapshot() and never Options itself.
	CurrentVersion string
	Enabled        bool
	CheckInterval  time.Duration

	// LatestVersion/ReleaseURL are the newest release this Updater has ever
	// observed (stamped as soon as a newer-than-current release is seen,
	// before Notify/Gate/apply are even consulted); they persist across
	// cycles rather than resetting to "" once the release is applied or
	// rejected, so a consumer can always answer "what's the newest release
	// we've seen so far".
	LatestVersion string
	ReleaseURL    string

	// LastCheckAt/NextCheckAt bracket the check interval: stamped after every
	// completed checkAndMaybeUpdate call. NextCheckAt = LastCheckAt +
	// CheckInterval exactly, with no jitter added - unlike most other
	// interval-driven loops in this repo - as a FIELD relationship, but it
	// only APPROXIMATES when Run's actual ticker will next fire: the ticker
	// is created once, right after the initial check, and keeps firing on its
	// own fixed cadence anchored to that creation time regardless of how long
	// any individual cycle takes (a Go time.Ticker never queues missed ticks
	// to "catch up"), so a long-running cycle can make the real next tick
	// arrive up to that cycle's own duration earlier than this field's
	// LastCheckAt+CheckInterval arithmetic suggests.
	LastCheckAt time.Time
	NextCheckAt time.Time

	// Phase/LastOutcome/LastError are this Updater's live state machine and
	// the result of the most recently completed cycle - see the Phase/
	// Outcome doc comments above.
	Phase       Phase
	LastOutcome Outcome
	LastError   string

	// AppliedFrom/AppliedVersion/AppliedAt describe the most recent
	// successful apply this Updater knows about - either one it performed
	// itself this process generation, or one seeded from a durable handoff
	// row consumed at boot by internal/app (SeedApplied) describing the
	// PREVIOUS generation's own successful apply.
	AppliedFrom    string
	AppliedVersion string
	AppliedAt      time.Time
}

// Snapshot returns a point-in-time copy of u's current status. Safe for
// concurrent use from any goroutine (Ф5a1; the future Ф5a3 dashboard read
// seam - nothing else consumes it yet).
func (u *Updater) Snapshot() Snapshot {
	u.snapMu.Lock()
	defer u.snapMu.Unlock()
	return u.snap
}

// SeedApplied stamps AppliedFrom/AppliedVersion/AppliedAt from a durable
// handoff row consumed at boot (internal/app, design Ф5a1): the CURRENT
// process generation did not perform this swap itself - the PREVIOUS
// generation did, and this process is simply running as a result of it - so
// the snapshot can still answer "what was last applied" from a cold start.
//
// Must be called strictly BEFORE the lifecycle controller starts this
// Updater's own Run goroutine: internal/app calls SeedApplied synchronously
// during Build (single-threaded, no goroutine involved yet), and
// controller.Run - which is what eventually starts the goroutine that calls
// Run - only happens later, under App.Run. A goroutine's own start
// establishes a happens-before edge with everything that ran before the
// `go` statement that launched it, so this ordering is safe with no
// additional synchronization beyond that guarantee (snapMu is still taken
// here regardless, purely for consistency with every other snapshot mutation
// and to keep the race detector's bookkeeping straightforward).
func (u *Updater) SeedApplied(from, to string, at time.Time) {
	u.snapMu.Lock()
	u.snap.AppliedFrom = from
	u.snap.AppliedVersion = to
	u.snap.AppliedAt = at
	u.snapMu.Unlock()
}

// setPhase sets Phase alone, leaving every other Snapshot field untouched.
// Leaf lock (see the Updater.snapMu doc comment): no I/O, no callback, no
// other lock held while snapMu is held.
func (u *Updater) setPhase(p Phase) {
	u.snapMu.Lock()
	u.snap.Phase = p
	u.snapMu.Unlock()
}

// setLatest stamps the latest release this Updater has observed, called
// once per cycle as soon as a newer-than-current release is detected -
// before Notify/Gate/apply are even consulted.
func (u *Updater) setLatest(version, releaseURL string) {
	u.snapMu.Lock()
	u.snap.LatestVersion = version
	u.snap.ReleaseURL = releaseURL
	u.snapMu.Unlock()
}

// setIdleOutcome is the common "this cycle ended without applying an
// update" stamp: Phase returns to idle and LastOutcome/LastError record why.
// An empty errText clears LastError (e.g. OutcomeUpToDate wiping out a
// previous cycle's recorded failure); a non-empty one is truncated via
// truncateError.
func (u *Updater) setIdleOutcome(outcome Outcome, errText string) {
	u.snapMu.Lock()
	u.snap.Phase = PhaseIdle
	u.snap.LastOutcome = outcome
	u.snap.LastError = truncateError(errText)
	u.snapMu.Unlock()
}

// setAppliedOutcome stamps a successful apply performed by THIS Updater,
// THIS process generation: PhaseRestartPending/OutcomeApplied, a cleared
// LastError (a success trivially has none), and AppliedFrom/AppliedVersion/
// AppliedAt - the same three fields SeedApplied stamps for a PREVIOUS
// generation's apply, kept in sync here so a dashboard reading Snapshot()
// never needs to know which generation actually performed the swap.
func (u *Updater) setAppliedOutcome(to string) {
	at := u.opts.now()
	u.snapMu.Lock()
	u.snap.Phase = PhaseRestartPending
	u.snap.LastOutcome = OutcomeApplied
	u.snap.LastError = ""
	u.snap.AppliedFrom = u.opts.CurrentVersion
	u.snap.AppliedVersion = to
	u.snap.AppliedAt = at
	u.snapMu.Unlock()
}

// stampCheckTimes records LastCheckAt = now() and NextCheckAt =
// LastCheckAt+CheckInterval. Called once per completed checkAndMaybeUpdate
// cycle (via defer, so it runs after every phase/outcome stamp the cycle's
// body already made), using the exact CheckInterval with NO jitter - unlike
// most other interval-driven loops in this repo. This is a field-level
// relationship only, not a promise about Run's actual ticker: see the
// Snapshot.NextCheckAt doc comment for why the two can drift apart on a
// long-running cycle.
func (u *Updater) stampCheckTimes() {
	last := u.opts.now()
	u.snapMu.Lock()
	u.snap.LastCheckAt = last
	u.snap.NextCheckAt = last.Add(u.opts.CheckInterval)
	u.snapMu.Unlock()
}
