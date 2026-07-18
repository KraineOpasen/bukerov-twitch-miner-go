package analytics

import "sort"

// roi.go is the pure, deterministic ROI aggregator: it turns a slice of resolved
// BetRecords into a ROISummary of prediction-betting performance. It has no I/O,
// no globals, and no time.Now — the caller supplies the (already period-filtered)
// records and stamps the human-readable labels — so every metric is reproducible
// and unit-testable in isolation.
//
// Metric conventions (documented so the numbers are auditable in one place):
//   - Win rate, average wager, and average win are computed over SETTLED bets
//     only (WIN + LOSE). Refunds return the stake, so they neither win nor lose
//     and are excluded from those denominators; they are still counted and shown
//     separately.
//   - Total wagered is the stake put at risk on settled bets (WIN + LOSE). A
//     refunded stake was returned, so it is not "wagered".
//   - Net profit is the sum of Gained across all records (refunds contribute 0).
//   - ROI = net profit / total wagered * 100.
//   - Max drawdown is the largest peak-to-trough drop of the cumulative
//     net-profit curve, walked in chronological order (records arrive
//     oldest-first from GetBets). It is reported as a non-negative point amount.

// OddsBucket labels, in display order. A bet falls into the first bucket whose
// upper bound it is below; 5+ is the catch-all.
var oddsBuckets = []struct {
	label string
	max   float64 // exclusive upper bound; 0 means "no upper bound"
}{
	{"<1.5", 1.5},
	{"1.5–2", 2},
	{"2–3", 3},
	{"3–5", 5},
	{"5+", 0},
}

// oddsBucketLabel returns the display bucket for an odds value.
func oddsBucketLabel(odds float64) string {
	for _, b := range oddsBuckets {
		if b.max == 0 {
			return b.label
		}
		if odds < b.max {
			return b.label
		}
	}
	return oddsBuckets[len(oddsBuckets)-1].label
}

// GroupStat is the per-key rollup used for the by-streamer, by-strategy, and
// by-odds-bucket breakdowns.
type GroupStat struct {
	Key       string  `json:"key"`
	Count     int     `json:"count"`
	Wins      int     `json:"wins"`
	Losses    int     `json:"losses"`
	Refunds   int     `json:"refunds"`
	Wagered   int     `json:"wagered"`
	NetProfit int     `json:"netProfit"`
	ROI       float64 `json:"roi"`
}

// ROISummary is the full prediction-betting performance report for one filter
// selection. Labels (Streamer/Strategy/Period) are stamped by the caller; the
// aggregator fills every numeric field and the breakdowns.
type ROISummary struct {
	Streamer string `json:"streamer,omitempty"`
	Strategy string `json:"strategy,omitempty"`
	Period   string `json:"period"`

	Count   int `json:"count"`
	Wins    int `json:"wins"`
	Losses  int `json:"losses"`
	Refunds int `json:"refunds"`

	WinRate      float64 `json:"winRate"`      // percent, over WIN+LOSE
	TotalWagered int     `json:"totalWagered"` // over WIN+LOSE
	NetProfit    int     `json:"netProfit"`
	ROI          float64 `json:"roi"`         // percent
	AvgWager     float64 `json:"avgWager"`    // over WIN+LOSE
	AvgWin       float64 `json:"avgWin"`      // avg gained on wins
	MaxDrawdown  int     `json:"maxDrawdown"` // non-negative points

	ByStreamer   []GroupStat `json:"byStreamer"`
	ByStrategy   []GroupStat `json:"byStrategy"`
	ByOddsBucket []GroupStat `json:"byOddsBucket"`

	// Empty is true when there are no records for the selection, so the UI can
	// render an empty state instead of a wall of zeros.
	Empty bool `json:"empty"`
}

const (
	resultWin    = "WIN"
	resultLose   = "LOSE"
	resultRefund = "REFUND"
)

// ComputeROI aggregates resolved bets into a ROISummary. Records are expected in
// chronological order (as GetBets returns them) for an accurate drawdown curve.
// A nil/empty slice yields a zeroed summary with Empty=true.
func ComputeROI(records []BetRecord) ROISummary {
	sum := ROISummary{
		ByStreamer:   []GroupStat{},
		ByStrategy:   []GroupStat{},
		ByOddsBucket: []GroupStat{},
	}
	if len(records) == 0 {
		sum.Empty = true
		return sum
	}

	byStreamer := map[string]*GroupStat{}
	byStrategy := map[string]*GroupStat{}
	byBucket := map[string]*GroupStat{}

	var winGainedTotal int
	var cumulative, peak, maxDrawdown int

	for _, r := range records {
		sum.Count++
		sum.NetProfit += r.Gained

		switch r.ResultType {
		case resultWin:
			sum.Wins++
			sum.TotalWagered += r.Placed
			winGainedTotal += r.Gained
		case resultLose:
			sum.Losses++
			sum.TotalWagered += r.Placed
		case resultRefund:
			sum.Refunds++
		}

		// Cumulative net-profit curve for drawdown (chronological order).
		cumulative += r.Gained
		if cumulative > peak {
			peak = cumulative
		}
		if dd := peak - cumulative; dd > maxDrawdown {
			maxDrawdown = dd
		}

		accumulate(byStreamer, r.Streamer, r)
		accumulate(byStrategy, r.Strategy, r)
		accumulate(byBucket, oddsBucketLabel(r.Odds), r)
	}

	settled := sum.Wins + sum.Losses
	if settled > 0 {
		sum.WinRate = pct(sum.Wins, settled)
		sum.AvgWager = float64(sum.TotalWagered) / float64(settled)
	}
	if sum.Wins > 0 {
		sum.AvgWin = float64(winGainedTotal) / float64(sum.Wins)
	}
	if sum.TotalWagered > 0 {
		sum.ROI = float64(sum.NetProfit) / float64(sum.TotalWagered) * 100
	}
	sum.MaxDrawdown = maxDrawdown

	sum.ByStreamer = sortedGroups(byStreamer)
	sum.ByStrategy = sortedGroups(byStrategy)
	sum.ByOddsBucket = bucketOrderedGroups(byBucket)

	return sum
}

// SummarizeBets folds resolved bets into a BetSummary: gross winnings, stake
// risked on settled bets, refunded stake, and the net result. It is the compact
// sibling of ComputeROI used for the earnings-page betting summary, and shares
// its conventions so the two never disagree: refunds return the stake (counted
// but not "staked"), and Net == Won - Staked == Σ Gained. A nil/empty slice
// yields Empty=true. Pure and deterministic (no I/O, no time.Now).
func SummarizeBets(records []BetRecord) BetSummary {
	if len(records) == 0 {
		return BetSummary{Empty: true}
	}
	var s BetSummary
	for _, r := range records {
		s.Net += r.Gained
		switch r.ResultType {
		case resultWin:
			s.Wins++
			s.Won += r.Won
			s.Staked += r.Placed
		case resultLose:
			s.Losses++
			s.Staked += r.Placed
		case resultRefund:
			s.Refunds++
			s.Refunded += r.Placed
		}
	}
	return s
}

// accumulate folds one record into a keyed group, creating it on first sight.
func accumulate(m map[string]*GroupStat, key string, r BetRecord) {
	g := m[key]
	if g == nil {
		g = &GroupStat{Key: key}
		m[key] = g
	}
	g.Count++
	g.NetProfit += r.Gained
	switch r.ResultType {
	case resultWin:
		g.Wins++
		g.Wagered += r.Placed
	case resultLose:
		g.Losses++
		g.Wagered += r.Placed
	case resultRefund:
		g.Refunds++
	}
}

// finalizeROI sets the derived ROI on a group.
func finalizeROI(g *GroupStat) {
	if g.Wagered > 0 {
		g.ROI = float64(g.NetProfit) / float64(g.Wagered) * 100
	}
}

// sortedGroups returns the groups ordered by key for deterministic output
// (used for by-streamer and by-strategy, where keys have no natural order).
func sortedGroups(m map[string]*GroupStat) []GroupStat {
	out := make([]GroupStat, 0, len(m))
	for _, g := range m {
		finalizeROI(g)
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// bucketOrderedGroups returns the odds-bucket groups in fixed display order,
// omitting buckets with no bets.
func bucketOrderedGroups(m map[string]*GroupStat) []GroupStat {
	out := make([]GroupStat, 0, len(m))
	for _, b := range oddsBuckets {
		if g := m[b.label]; g != nil {
			finalizeROI(g)
			out = append(out, *g)
		}
	}
	return out
}

// pct returns n/d as a percentage; d must be > 0.
func pct(n, d int) float64 {
	return float64(n) / float64(d) * 100
}
