package analytics

import (
	"sort"
	"strings"
)

// canonicalPointReason maps a raw reason — a points.event_type timeline value
// or a point_events.reason_code ledger value — to its canonical breakdown
// category. Timeline rows are written in the display form (underscores
// replaced with spaces, so a watch streak is stored as "WATCH STREAK"), the
// ledger keeps Twitch's raw reason_code ("WATCH_STREAK"); neither is ever
// rewritten, so the mapping happens here, at aggregation time only.
//
// The table is exact — no Contains/HasPrefix — so "WATCH" can never swallow
// "WATCH STREAK". TrimSpace/ToUpper apply to the lookup key only, never to
// stored data. PREDICTION is a first-class category: a prediction win is a
// genuine positive channel-point credit (Twitch's point-gain reason_code
// "PREDICTION"), so folding it into OTHER made betting winnings look like an
// unexplained "Other" slice in the earnings donut. Genuinely low-value reasons
// with no UX identity of their own ("WEEKLY REWARDS") are pooled into OTHER,
// as is any unknown or empty value — an unrecognized reason is still an exact
// earning, it just has no slice of its own.
func canonicalPointReason(raw string) string {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "WATCH STREAK", "WATCH_STREAK":
		return "WATCH_STREAK"
	case "WATCH":
		return "WATCH"
	case "CLAIM":
		return "CLAIM"
	case "RAID":
		return "RAID"
	case "PREDICTION":
		return "PREDICTION"
	default:
		return "OTHER"
	}
}

// spentReason is the timeline reason the miner records for a points-spent
// frame (Service.RecordPoints(streamer, "Spent")). A delta INTO such a sample
// is by definition a spend, never an earning: when it comes out positive the
// previous snapshot was merely stale, and attributing it would turn spent
// points into earned points.
const spentReason = "Spent"

func isSpentReason(raw string) bool {
	return strings.EqualFold(strings.TrimSpace(raw), spentReason)
}

// timelineReason is the display form of a Twitch reason code as stored in
// points.event_type ("WATCH_STREAK" -> "WATCH STREAK") by every timeline
// write since the first analytics build. The exact ledger stores the raw
// reason_code; this form exists for the chart tooltip and for legacy rows.
func timelineReason(reasonCode string) string {
	return strings.ReplaceAll(reasonCode, "_", " ")
}

// accumulateShare adds one earning (amount over count events) to the
// canonical category of rawReason.
func accumulateShare(gained map[string]*ReasonShare, rawReason string, amount, count int) {
	reason := canonicalPointReason(rawReason)
	share, ok := gained[reason]
	if !ok {
		share = &ReasonShare{Reason: reason}
		gained[reason] = share
	}
	share.Gained += amount
	share.Count += count
}

// sortedShares flattens an accumulator into the deterministic rendering
// order: Gained descending, ties by Reason ascending. Nil when empty.
func sortedShares(gained map[string]*ReasonShare) []ReasonShare {
	if len(gained) == 0 {
		return nil
	}
	out := make([]ReasonShare, 0, len(gained))
	for _, share := range gained {
		out = append(out, *share)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Gained != out[j].Gained {
			return out[i].Gained > out[j].Gained
		}
		return out[i].Reason < out[j].Reason
	})
	return out
}

// EstimateLegacyBreakdown is the LEGACY ESTIMATOR: it attributes each positive
// balance delta INTO a legacy earning sample to that sample's canonical reason.
// It exists only so history recorded before the exact point-event ledger (or
// by a pre-ledger binary) stays visible; its output is an estimate, never
// exact event accounting, because a delta between two mutable balance
// snapshots is not the event-local amount Twitch granted — production showed
// three 450-point streak grants recorded as 462/450/462 deltas.
//
// Samples must be the raw, consecutive balance timeline in ascending order
// (GetPointSamples' order) so every delta is between adjacent snapshots. A
// sample is skipped as an earning candidate when:
//
//   - it is Exact: an exact ledger event produced it, so its delta is already
//     accounted for exactly and must not be estimated a second time;
//   - it is a points-spent snapshot: a spend is never an earning, whatever
//     the delta's sign.
//
// Non-positive deltas are ignored. The first sample only establishes the
// baseline: its own delta cannot be known without a pre-window sample, which
// is deliberately not fetched. Samples counts the legacy earning samples seen
// (baseline included), which tells a caller that pre-ledger history is present
// even when no delta could be attributed. Called on the raw series before
// downsampling — a downsampled series drops rows and would misattribute the
// skipped deltas.
func EstimateLegacyBreakdown(samples []PointSample) LegacyEstimate {
	var est LegacyEstimate
	gained := make(map[string]*ReasonShare)
	for i, sample := range samples {
		if sample.Exact || isSpentReason(sample.Reason) {
			continue
		}
		est.Samples++
		if i == 0 {
			continue
		}
		diff := sample.Balance - samples[i-1].Balance
		if diff <= 0 {
			continue
		}
		accumulateShare(gained, sample.Reason, diff, 1)
	}
	est.Breakdown = sortedShares(gained)
	return est
}

// ComposeEarnings decides what a points-history response reports as its
// primary Breakdown, its separate LegacyBreakdown, and the EarningsAccounting
// metadata that qualifies both. The rules are conservative: exact coverage is
// claimed only when the range holds exact events and NO legacy earning sample
// and the legacy side is fully known; the two accountings are never summed.
//
//   - exact events present, no legacy sample, series complete → "exact":
//     Breakdown is the exact aggregation.
//   - exact events present, legacy samples present or series truncated →
//     "mixed": Breakdown is the exact aggregation, LegacyBreakdown the
//     estimate for the legacy part (absent when unavailable or when nothing
//     could be attributed).
//   - no exact event, legacy samples present → "legacy": Breakdown IS the
//     estimate (Exact == false); LegacyBreakdown is not repeated, so no
//     consumer can add the two and double-count.
//   - no exact event, series truncated → "unavailable": nothing can be said.
//   - nothing → "none".
func ComposeEarnings(exact ExactEarnings, legacy LegacyEstimate, rawTruncated bool) (breakdown, legacyBreakdown []ReasonShare, acc EarningsAccounting) {
	exactPresent := exact.Events > 0

	switch {
	case rawTruncated:
		acc.LegacyStatus = LegacyStatusUnavailable
	case legacy.Samples > 0:
		acc.LegacyStatus = LegacyStatusEstimated
	default:
		acc.LegacyStatus = LegacyStatusNone
	}

	acc.Exact = exactPresent
	switch {
	case exactPresent && acc.LegacyStatus == LegacyStatusNone:
		acc.Coverage = EarningsCoverageExact
	case exactPresent:
		acc.Coverage = EarningsCoverageMixed
	case acc.LegacyStatus == LegacyStatusEstimated:
		acc.Coverage = EarningsCoverageLegacy
	case acc.LegacyStatus == LegacyStatusUnavailable:
		acc.Coverage = EarningsCoverageUnavailable
	default:
		acc.Coverage = EarningsCoverageNone
	}

	switch {
	case exactPresent:
		breakdown = exact.Breakdown
		acc.ExactSince = exact.Since
		if acc.LegacyStatus == LegacyStatusEstimated {
			legacyBreakdown = legacy.Breakdown
		}
	case acc.LegacyStatus == LegacyStatusEstimated:
		breakdown = legacy.Breakdown
	}
	return breakdown, legacyBreakdown, acc
}
