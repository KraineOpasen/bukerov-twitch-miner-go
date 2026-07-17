package analytics

import "sort"

// BreakdownFromSamples aggregates an earnings breakdown from a raw
// balance-over-time series (as stored in the points table: absolute balance
// snapshots, each tagged with the reason that caused the change).
//
// Each consecutive positive balance delta is attributed to the later sample's
// reason; non-positive deltas (bets placed, redemptions — reason "Spent" and
// friends) are ignored, because the breakdown answers "where did the points
// come from", not "where did they go". The first sample only establishes the
// baseline. Samples must be in ascending time order, which is how
// GetPointSamples returns them.
//
// The result is sorted by Gained descending (ties by Reason ascending) so the
// order is deterministic for rendering and tests. A reasonless sample is
// bucketed as "OTHER". Called on the raw series before downsampling —
// downsampled series drop rows, which would silently misattribute the skipped
// deltas.
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
		reason := samples[i].Reason
		if reason == "" {
			reason = "OTHER"
		}
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
