// Package predictioneval is an OFFLINE, DETERMINISTIC replay of the automatic
// prediction-betting decisions this miner already made and already recorded.
//
// It answers one question and refuses every neighbouring one: given only what
// a decision was recorded to have READ, does the policy that was actually
// configured for it re-derive the choice, the stake and the stage-by-stage
// path that was recorded as its RESULT?
//
// # What this package is not
//
// It never places a bet, never proposes a strategy, never scores strategies
// against one another and never touches production behaviour. It is a reader.
// The only thing it produces is a versioned, machine-readable comparison.
//
// It is also NOT a universal-SMART substitute and NOT a plugin framework for
// alternative strategies. It reconstructs the CURRENT-CONFIGURED baseline —
// the strategy each individual decision was actually running under — and
// nothing else. A donor strategy, a counterfactual sweep or a portfolio
// comparison is a different concern and is deliberately absent.
//
// # The four seams
//
// The pipeline is four value-in / value-out functions:
//
//		MaterializePairedKnowledge  →  ProjectDecisionCase  →  Evaluate  →  Score
//
//	 1. [MaterializePairedKnowledge] takes a [SourceDataset] — persisted facts a
//	    reader already acquired and verified — and groups them into attempts,
//	    bounding each attempt's COMMON KNOWLEDGE SLICE: the causally-closed
//	    prefix of facts up to and including the attempt's terminal envelope.
//	    The slice is digested here, before any model projection, so appending
//	    later facts to the store cannot change an earlier attempt's inputs or
//	    its digest.
//
//	 2. [ProjectDecisionCase] splits that slice into the two halves the store
//	    persists together: the pre-decision INPUTS and the recorded RESULTS.
//	    This is the causal cut. Nothing downstream may treat a result as an
//	    input.
//
//	 3. [Evaluate] re-derives the decision from the inputs alone. Its signature
//	    is the enforcement: it receives [DecisionInputs], never a
//	    [RecordedResults], never a settlement. The single observed value it may
//	    read is the explicitly-named [ObservedRealization], which exists only to
//	    constrain the stealth-mode random draw and is labelled as such in the
//	    output.
//
//	 4. [Score] compares the two and is the ONLY stage allowed to see what
//	    happened afterwards — placement, resolution, payout. It cannot influence
//	    [Evaluate], because [Evaluate] already ran.
//
// # Purity
//
// Every one of the four stages is pure: no database, no network, no Twitch, no
// PubSub, no live settings, no environment, no wall clock, no global RNG, no
// goroutines, no I/O callbacks. Acquiring data is the separate job of
// [github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval/reader],
// which is the only part that touches SQLite. TestProductionCoreDependencyFence
// enforces the boundary mechanically over the whole transitive import graph of
// this package's non-test files.
//
// # What a passing comparison does and does not prove
//
// A decision whose stages all agree is evidence that the recorded inputs
// re-derive the recorded results under the pinned policy. It is NOT evidence
// that the miner should have bet, that the bet was profitable, or that any
// other strategy would have done better. Where the original decision consumed
// randomness (stealth mode), the replay cannot independently reproduce the
// draw at all; it reports that stage as
// [StealthConditionedOnObservedRealization] and excludes it from the
// independent tally. See [Scorecard] for the partition.
//
// # Empirical status
//
// This package is proven against controlled, persisted test datasets written
// through the real store and read back through the real reader. It has NOT
// been validated against a production observation dataset; collection and
// empirical replay are separate, unauthorized work. Nothing here should be
// described as live-validated.
package predictioneval
