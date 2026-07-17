package analytics

import (
	"sort"
	"strings"
)

// canonicalPointReason maps a raw points.event_type value to its canonical
// breakdown category. Raw rows are written by Service.RecordPoints, which
// replaces underscores with spaces before persisting (so the on-disk value
// for a watch streak is "WATCH STREAK"); the stored history is never
// rewritten, so the mapping happens here, at aggregation time only.
//
// The table is exact — no Contains/HasPrefix — so "WATCH" can never swallow
// "WATCH STREAK". TrimSpace/ToUpper apply to the lookup key only, never to
// stored data. Low-volume reasons with no UX value of their own ("WEEKLY
// REWARDS", "PREDICTION") are deliberately pooled into OTHER, as is any
// unknown or empty value.
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
	default:
		return "OTHER"
	}
}

// BreakdownFromSamples aggregates an earnings breakdown from a raw
// balance-over-time series (as stored in the points table: absolute balance
// snapshots, each tagged with the reason that caused the change).
//
// Each consecutive positive balance delta is attributed to the later sample's
// canonical reason (see canonicalPointReason); non-positive deltas (bets
// placed, redemptions — reason "Spent" and friends) are ignored, because the
// breakdown answers "where did the points come from", not "where did they
// go". The first sample only establishes the baseline: its own delta cannot
// be known without a pre-window sample, which is deliberately not fetched —
// that data-quality improvement is deferred (see PR notes). Samples must be
// in ascending time order, which is how GetPointSamples returns them.
//
// The result is sorted by Gained descending (ties by Reason ascending) so the
// order is deterministic for rendering and tests. Called on the raw series
// before downsampling — downsampled series drop rows, which would silently
// misattribute the skipped deltas.
func BreakdownFromSamples(samples []PointSample) []ReasonShare {
	if len(samples) < 2 {
		return nil
	}

	gained := make(map[string]*ReasonShare)
	for i := 1; i < len(samples); i++ {
		diff := samples[i].Balance - samples[i-1].Balance
		if diff <= 0 {
			continue
		}
		reason := canonicalPointReason(samples[i].Reason)
		share, ok := gained[reason]
		if !ok {
			share = &ReasonShare{Reason: reason}
			gained[reason] = share
		}
		share.Gained += diff
		share.Count++
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
