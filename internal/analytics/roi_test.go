package analytics

import (
	"math"
	"testing"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func win(streamer, strategy string, placed, won int, odds float64) BetRecord {
	return BetRecord{Streamer: streamer, Strategy: strategy, ResultType: "WIN", Placed: placed, Won: won, Gained: won - placed, Odds: odds}
}
func lose(streamer, strategy string, placed int, odds float64) BetRecord {
	return BetRecord{Streamer: streamer, Strategy: strategy, ResultType: "LOSE", Placed: placed, Won: 0, Gained: -placed, Odds: odds}
}
func refund(streamer, strategy string, placed int, odds float64) BetRecord {
	// A refund returns the stake: Gained is 0 even though a stake was placed.
	return BetRecord{Streamer: streamer, Strategy: strategy, ResultType: "REFUND", Placed: placed, Won: 0, Gained: 0, Odds: odds}
}

func TestSummarizeBetsEmptyIsEmpty(t *testing.T) {
	s := SummarizeBets(nil)
	if !s.Empty {
		t.Fatal("empty input must set Empty=true")
	}
	if s.Won != 0 || s.Staked != 0 || s.Refunded != 0 || s.Net != 0 || s.Wins != 0 || s.Losses != 0 || s.Refunds != 0 {
		t.Fatalf("empty summary must be all-zero, got %+v", s)
	}
}

func TestSummarizeBets(t *testing.T) {
	cases := []struct {
		name string
		recs []BetRecord
		want BetSummary
	}{
		{
			// Won=250, Staked=100(win)+100(loss)=200, Net=(250-100)+(-100)=50.
			// Net must equal Won-Staked (250-200=50) and be positive.
			name: "positive net: winnings exceed stake",
			recs: []BetRecord{
				win("a", "SMART", 100, 250, 2.5),
				lose("a", "SMART", 100, 1.8),
			},
			want: BetSummary{Wins: 1, Losses: 1, Refunds: 0, Won: 250, Staked: 200, Refunded: 0, Net: 50},
		},
		{
			// Two losses, one small win. Won=120, Staked=100+100+100=300, Net=120-300=-180.
			name: "negative net: losses dominate, never a positive figure",
			recs: []BetRecord{
				lose("a", "SMART", 100, 2.0),
				lose("a", "SMART", 100, 2.0),
				win("a", "SMART", 100, 120, 1.2),
			},
			want: BetSummary{Wins: 1, Losses: 2, Refunds: 0, Won: 120, Staked: 300, Refunded: 0, Net: -180},
		},
		{
			// One win exactly returns the stake (odds 1.0 payout == stake) plus a
			// loss of the same size: Won=100, Staked=200, Net=-100... adjust so net==0.
			name: "zero net",
			recs: []BetRecord{
				win("a", "SMART", 100, 200, 2.0), // +100
				lose("a", "SMART", 100, 2.0),     // -100
			},
			want: BetSummary{Wins: 1, Losses: 1, Refunds: 0, Won: 200, Staked: 200, Refunded: 0, Net: 0},
		},
		{
			// A refund returns the stake: counted and reported separately, never
			// part of Staked, and net-neutral.
			name: "refund is net-neutral and separately reported",
			recs: []BetRecord{
				win("a", "SMART", 100, 250, 2.5),
				refund("a", "SMART", 400, 3.5),
			},
			want: BetSummary{Wins: 1, Losses: 0, Refunds: 1, Won: 250, Staked: 100, Refunded: 400, Net: 150},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SummarizeBets(tc.recs)
			got.Empty = false // set implicitly false by non-empty input; compare the rest
			if got != tc.want {
				t.Fatalf("SummarizeBets() = %+v, want %+v", got, tc.want)
			}
			// Invariant the UI relies on: Net == Won - Staked.
			if got.Net != got.Won-got.Staked {
				t.Errorf("invariant broken: Net(%d) != Won(%d) - Staked(%d)", got.Net, got.Won, got.Staked)
			}
		})
	}
}

func TestComputeROIEmptyIsNeutralNotDivByZero(t *testing.T) {
	s := ComputeROI(nil)
	if !s.Empty {
		t.Fatal("empty input must set Empty=true")
	}
	if s.Count != 0 || s.WinRate != 0 || s.ROI != 0 || s.AvgWager != 0 || s.AvgWin != 0 || s.MaxDrawdown != 0 {
		t.Fatalf("empty summary must be all-zero, got %+v", s)
	}
	// Breakdowns must be non-nil empty slices (JSON renders [] not null).
	if s.ByStreamer == nil || s.ByStrategy == nil || s.ByOddsBucket == nil {
		t.Fatal("breakdowns must be non-nil empty slices")
	}
}

func TestComputeROICoreMetrics(t *testing.T) {
	// 2 wins, 1 loss. Wagered = 100+100+100 = 300. Net = (250-100)+(150-100)+(-100)=100.
	recs := []BetRecord{
		win("a", "SMART", 100, 250, 2.5),
		lose("a", "SMART", 100, 1.8),
		win("b", "HIGH_ODDS", 100, 150, 1.5),
	}
	s := ComputeROI(recs)

	if s.Count != 3 || s.Wins != 2 || s.Losses != 1 || s.Refunds != 0 {
		t.Fatalf("counts wrong: %+v", s)
	}
	if s.TotalWagered != 300 {
		t.Errorf("wagered = %d, want 300", s.TotalWagered)
	}
	if s.NetProfit != 100 {
		t.Errorf("net profit = %d, want 100", s.NetProfit)
	}
	if !approx(s.ROI, 100.0/300.0*100) {
		t.Errorf("ROI = %v, want %v", s.ROI, 100.0/300.0*100)
	}
	if !approx(s.WinRate, 2.0/3.0*100) {
		t.Errorf("win rate = %v, want %v", s.WinRate, 2.0/3.0*100)
	}
	if !approx(s.AvgWager, 100) {
		t.Errorf("avg wager = %v, want 100", s.AvgWager)
	}
	// avg win = (150 + 50) / 2 = 100
	if !approx(s.AvgWin, 100) {
		t.Errorf("avg win = %v, want 100", s.AvgWin)
	}
}

func TestComputeROIRefundsExcludedFromRatesButCounted(t *testing.T) {
	// Only-refunds selection: wagered must be 0 (stakes returned), ROI/win-rate
	// must not divide by zero, and the refund must still be counted.
	s := ComputeROI([]BetRecord{refund("a", "SMART", 500, 2.0), refund("a", "SMART", 300, 3.0)})
	if s.Refunds != 2 || s.Count != 2 {
		t.Fatalf("refunds must be counted: %+v", s)
	}
	if s.Wins != 0 || s.Losses != 0 {
		t.Fatalf("refunds are neither win nor loss: %+v", s)
	}
	if s.TotalWagered != 0 {
		t.Errorf("refunded stakes are not wagered, got %d", s.TotalWagered)
	}
	if s.WinRate != 0 || s.ROI != 0 || s.AvgWager != 0 {
		t.Errorf("no settled bets → rates must be 0 (no div-by-zero), got %+v", s)
	}
	if s.NetProfit != 0 {
		t.Errorf("refunds are net-zero, got %d", s.NetProfit)
	}
}

func TestComputeROIWinRateExcludesRefundFromDenominator(t *testing.T) {
	// 1 win, 1 loss, 1 refund → win rate is 1/2 = 50%, not 1/3.
	s := ComputeROI([]BetRecord{
		win("a", "SMART", 100, 300, 3.0),
		lose("a", "SMART", 100, 2.0),
		refund("a", "SMART", 100, 2.0),
	})
	if !approx(s.WinRate, 50) {
		t.Fatalf("win rate = %v, want 50 (refund excluded from denominator)", s.WinRate)
	}
}

func TestMaxDrawdownMonotonicIsZero(t *testing.T) {
	// A strictly rising equity curve has no drawdown.
	s := ComputeROI([]BetRecord{
		win("a", "SMART", 100, 300, 3.0),
		win("a", "SMART", 100, 250, 2.5),
		win("a", "SMART", 100, 400, 4.0),
	})
	if s.MaxDrawdown != 0 {
		t.Fatalf("monotonic gains → drawdown 0, got %d", s.MaxDrawdown)
	}
}

func TestMaxDrawdownSawtooth(t *testing.T) {
	// Cumulative: +200, then -100 -100 -100 (trough -100 from peak +200 => dd 300),
	// then +50 recover. Peak 200, trough -100, max drawdown = 300.
	s := ComputeROI([]BetRecord{
		win("a", "SMART", 100, 300, 3.0), // +200  cum=200 peak=200
		lose("a", "SMART", 100, 2.0),     // -100  cum=100 dd=100
		lose("a", "SMART", 100, 2.0),     // -100  cum=0   dd=200
		lose("a", "SMART", 100, 2.0),     // -100  cum=-100 dd=300
		win("a", "SMART", 100, 150, 1.5), // +50   cum=-50
	})
	if s.MaxDrawdown != 300 {
		t.Fatalf("max drawdown = %d, want 300", s.MaxDrawdown)
	}
}

func TestOddsBucketBoundaries(t *testing.T) {
	// Exactly on a boundary goes to the higher bucket (strict < upper bound):
	// 1.5 → "1.5–2", 2.0 → "2–3", 3.0 → "3–5", 5.0 → "5+".
	cases := map[float64]string{
		1.0: "<1.5", 1.49: "<1.5",
		1.5: "1.5–2", 1.99: "1.5–2",
		2.0: "2–3", 2.99: "2–3",
		3.0: "3–5", 4.99: "3–5",
		5.0: "5+", 12.0: "5+",
	}
	for odds, want := range cases {
		if got := oddsBucketLabel(odds); got != want {
			t.Errorf("odds %v bucketed as %q, want %q", odds, got, want)
		}
	}
}

func TestComputeROIBreakdowns(t *testing.T) {
	recs := []BetRecord{
		win("alice", "SMART", 100, 250, 2.5),   // bucket 2–3
		lose("bob", "HIGH_ODDS", 200, 6.0),     // bucket 5+
		win("bob", "HIGH_ODDS", 100, 150, 1.5), // bucket 1.5–2
		refund("alice", "SMART", 50, 1.2),      // bucket <1.5
	}
	s := ComputeROI(recs)

	// By streamer: sorted by key → alice, bob.
	if len(s.ByStreamer) != 2 || s.ByStreamer[0].Key != "alice" || s.ByStreamer[1].Key != "bob" {
		t.Fatalf("by-streamer keys/order wrong: %+v", s.ByStreamer)
	}
	// alice: 1 win (+150), 1 refund; net +150, wagered 100.
	if s.ByStreamer[0].NetProfit != 150 || s.ByStreamer[0].Wagered != 100 || s.ByStreamer[0].Refunds != 1 {
		t.Errorf("alice group wrong: %+v", s.ByStreamer[0])
	}
	// bob: 1 win (+50), 1 loss (-200); net -150, wagered 300.
	if s.ByStreamer[1].NetProfit != -150 || s.ByStreamer[1].Wagered != 300 {
		t.Errorf("bob group wrong: %+v", s.ByStreamer[1])
	}

	// By strategy sorted: HIGH_ODDS, SMART.
	if len(s.ByStrategy) != 2 || s.ByStrategy[0].Key != "HIGH_ODDS" || s.ByStrategy[1].Key != "SMART" {
		t.Fatalf("by-strategy keys/order wrong: %+v", s.ByStrategy)
	}

	// By odds bucket: fixed display order, only non-empty buckets present:
	// <1.5, 1.5–2, 2–3, 5+.
	wantBuckets := []string{"<1.5", "1.5–2", "2–3", "5+"}
	if len(s.ByOddsBucket) != len(wantBuckets) {
		t.Fatalf("odds buckets = %+v, want %v", s.ByOddsBucket, wantBuckets)
	}
	for i, w := range wantBuckets {
		if s.ByOddsBucket[i].Key != w {
			t.Errorf("odds bucket %d = %q, want %q", i, s.ByOddsBucket[i].Key, w)
		}
	}
}
