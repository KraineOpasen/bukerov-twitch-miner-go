package watcher

import (
	"strings"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// Owner decision FB-OD-10 (R12): a channel-restricted campaign whose current
// drop carries an operator Skip rule is not restricted work for the PA-B1
// admission, so a channel holding only such campaigns is an ordinary channel
// there: it neither takes a seat through the admission nor keeps one as a
// restricted occupant, while a channel with restricted work that no rule
// excludes still does. The channels below have channel points, so a skipped
// reward alone does not keep them out of the candidate list.

// skipRuledRestrictedCampaign is a channel-restricted campaign with real
// remaining work whose current drop is the one skipRuledRewards excludes.
func skipRuledRestrictedCampaign(id string, channels ...*models.Streamer) *models.Campaign {
	campaign := restrictedTestCampaign(id, channels...)
	campaign.Game = &models.Game{ID: "game-skip"}
	campaign.Drops[0].Name = "Skipped Reward"
	return campaign
}

// skipRuledRewards is the operator's Skip rule for skipRuledRestrictedCampaign.
func skipRuledRewards() *models.RewardSkips {
	return models.NewRewardSkips([]string{models.NormalizeRewardKey("game-skip", "Skipped Reward")})
}

// R12 repaired, R1/R14(a): beside a channel with restricted work, a channel
// whose only restricted campaign is skip-ruled takes no seat through the
// admission and gets none of its reasons; without the rule both are seated.
func TestRestrictedSkipRuledChannelGainsNoSeat(t *testing.T) {
	cases := []struct {
		name    string
		skipped bool
		want    []string
	}{
		{name: "skip-ruled campaign brings no restricted work", skipped: true},
		{name: "control: the same campaign without the rule", want: []string{"streamerc", "streamerd"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newResidenceFixture(t, 4)
			byLogin := streamersByLogin(f.w.streamers)
			c, d := byLogin["streamerc"], byLogin["streamerd"]
			assignRestricted(c, restrictedTestCampaign("camp-streamerc", c))
			assignRestricted(d, skipRuledRestrictedCampaign("camp-streamerd", d))
			if tc.skipped {
				f.w.SetRewardSkips(skipRuledRewards())
			}
			f.seedWeights(t, time.Now(), map[string]float64{
				"streamera": 5, "streamerb": 10, "streamerc": 60, "streamerd": 80,
			})
			if !containsIndex(f.w.getOnlineStreamers(nil), restrictedIndex(t, f.w, "streamerd")) {
				t.Fatalf("precondition: streamerd must stay an online candidate through its channel points")
			}

			f.w.processWatching(tickCtx(f.w))

			got := restrictedSlotLogins(t, f)
			if !tc.skipped {
				if !sameLoginSet(got, tc.want...) {
					t.Fatalf("slots=%v, want %v", got, tc.want)
				}
				return
			}
			if !sameLoginSet(got, "streamera", "streamerc") && !sameLoginSet(got, "streamerb", "streamerc") {
				t.Fatalf("slots=%v, want streamerc beside a fair-pair channel and the skip-ruled streamerd unseated", got)
			}
			reason := decisionReason(f.w, "streamerd")
			for _, admission := range []string{
				"admitted beside another channel-restricted drop", "(owner rule PA-B1a)", waitingForRestrictedSeats,
				"a channel-restricted assignment changed",
			} {
				if strings.Contains(reason, admission) {
					t.Fatalf("streamerd reason=%q carries the admission's %q", reason, admission)
				}
			}
		})
	}
}

// R12 repaired, R3/R1: a skip-ruled channel that fairness seated is not a
// restricted occupant, so two channels with distinct restricted work take both
// seats and it is told it was displaced as an ordinary seat; without the rule
// it keeps its seat and the second channel waits.
func TestRestrictedSkipRuledOccupantKeepsNoSeat(t *testing.T) {
	cases := []struct {
		name    string
		skipped bool
		want    []string
	}{
		{name: "skip-ruled occupant is ordinary", skipped: true, want: []string{"streamerc", "streamerd"}},
		{name: "control: the same occupant without the rule", want: []string{"streamerb", "streamerc"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newResidenceFixture(t, 4)
			byLogin := streamersByLogin(f.w.streamers)
			b, c, d := byLogin["streamerb"], byLogin["streamerc"], byLogin["streamerd"]
			assignRestricted(b, skipRuledRestrictedCampaign("camp-streamerb", b))
			assignRestricted(c, restrictedTestCampaign("camp-streamerc", c))
			assignRestricted(d, restrictedTestCampaign("camp-streamerd", d))
			if tc.skipped {
				f.w.SetRewardSkips(skipRuledRewards())
			}
			f.seedWeights(t, time.Now(), map[string]float64{
				"streamera": 5, "streamerb": 10, "streamerc": 60, "streamerd": 70,
			})

			f.w.processWatching(tickCtx(f.w))

			if got := restrictedSlotLogins(t, f); !sameLoginSet(got, tc.want...) {
				t.Fatalf("slots=%v, want %v", got, tc.want)
			}
			if !tc.skipped {
				return
			}
			reason := decisionReason(f.w, "streamerb")
			if !strings.Contains(reason, "displaced by channel-restricted drop streamerc (channel-restricted drops may hold both slots") {
				t.Fatalf("streamerb reason=%q, want the ordinary seat's displacement by streamerc", reason)
			}
		})
	}
}

// R12 repaired, R4/R9: between an ordinary seat and a skip-ruled one, both
// unprotected, the victim order no longer counts the skip-ruled campaign as
// restricted, so the less owed of the two is given up whichever it is;
// streamerd, carried by streamerc, waits. Beside a partner seat without
// restricted work — ordinary or skip-ruled — streamerc is not described as
// seated beside another channel-restricted drop, and the channel it displaced
// is not told that channel-restricted drops hold both slots. Without the rule
// the skip-ruled channel is a restricted occupant and the ordinary seat goes.
func TestRestrictedSkipRuledSeatGivenUpAsOrdinary(t *testing.T) {
	const competesAgain = " (it competes again at the next evaluation)"
	cases := []struct {
		name      string
		skipped   bool
		seeds     map[string]float64
		want      []string
		admitted  string
		displaced string
		reason    string
	}{
		{
			name: "skip-ruled seat competes as ordinary", skipped: true,
			seeds:     map[string]float64{"streamera": 5, "streamerb": 10, "streamerc": 60, "streamerd": 70},
			want:      []string{"streamera", "streamerc"},
			admitted:  "watched: admitted as a channel-restricted drop, displacing streamerb",
			displaced: "streamerb", reason: "displaced by channel-restricted drop streamerc" + competesAgain,
		},
		{
			name: "more owed skip-ruled seat kept as ordinary", skipped: true,
			seeds:     map[string]float64{"streamera": 10, "streamerb": 5, "streamerc": 60, "streamerd": 70},
			want:      []string{"streamerb", "streamerc"},
			admitted:  "watched: admitted as a channel-restricted drop, displacing streamera",
			displaced: "streamera", reason: "displaced by channel-restricted drop streamerc" + competesAgain,
		},
		{
			name:      "control: the same seat without the rule",
			seeds:     map[string]float64{"streamera": 5, "streamerb": 10, "streamerc": 60, "streamerd": 70},
			want:      []string{"streamerb", "streamerc"},
			admitted:  "watched: admitted beside another channel-restricted drop, displacing streamera",
			displaced: "streamera", reason: "displaced by channel-restricted drop streamerc (channel-restricted drops may hold both slots",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newResidenceFixture(t, 4)
			byLogin := streamersByLogin(f.w.streamers)
			b, c, d := byLogin["streamerb"], byLogin["streamerc"], byLogin["streamerd"]
			assignRestricted(b, skipRuledRestrictedCampaign("camp-streamerb", b))
			shared := restrictedTestCampaign("camp-shared", c, d)
			assignRestricted(c, shared)
			assignRestricted(d, shared)
			if tc.skipped {
				f.w.SetRewardSkips(skipRuledRewards())
			}
			f.seedWeights(t, time.Now(), tc.seeds)

			f.w.processWatching(tickCtx(f.w))

			if got := restrictedSlotLogins(t, f); !sameLoginSet(got, tc.want...) {
				t.Fatalf("slots=%v, want %v", got, tc.want)
			}
			if reason := decisionReason(f.w, "streamerd"); !strings.Contains(reason, "streamerc already farms the same channel-restricted drop campaign") {
				t.Fatalf("streamerd reason=%q, want its carrier streamerc", reason)
			}
			if reason := decisionReason(f.w, "streamerc"); !strings.HasPrefix(reason, tc.admitted) {
				t.Fatalf("streamerc reason=%q, want prefix %q", reason, tc.admitted)
			}
			if reason := decisionReason(f.w, tc.displaced); !strings.Contains(reason, tc.reason) {
				t.Fatalf("%s reason=%q, want %q", tc.displaced, reason, tc.reason)
			}
		})
	}
}

// FB-OD-10 per campaign: a Skip rule removes only the campaign it rules, so a
// channel keeps the seat its other, unskipped restricted campaign brings, and
// a skip-ruled campaign never makes a channel bring distinct work.
func TestRestrictedSkipRuleExcludesOnlyThatCampaign(t *testing.T) {
	cases := []struct {
		name    string
		shared  bool
		want    []string
		carried bool
	}{
		{name: "unskipped campaign beside a skip-ruled one still brings work", want: []string{"streamerc", "streamerd"}},
		{name: "skip-ruled campaign beside a shared one brings no distinct work", shared: true, carried: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newResidenceFixture(t, 4)
			byLogin := streamersByLogin(f.w.streamers)
			c, d := byLogin["streamerc"], byLogin["streamerd"]
			own := restrictedTestCampaign("camp-streamerd", d)
			if tc.shared {
				own = restrictedTestCampaign("camp-shared", c, d)
				assignRestricted(c, own)
			} else {
				assignRestricted(c, restrictedTestCampaign("camp-streamerc", c))
			}
			// The skip-ruled campaign is listed first, so it must not hide the
			// campaign after it.
			assignRestricted(d, skipRuledRestrictedCampaign("camp-skip-streamerd", d), own)
			f.w.SetRewardSkips(skipRuledRewards())
			f.seedWeights(t, time.Now(), map[string]float64{
				"streamera": 5, "streamerb": 10, "streamerc": 60, "streamerd": 70,
			})

			f.w.processWatching(tickCtx(f.w))

			got := restrictedSlotLogins(t, f)
			if !tc.carried {
				if !sameLoginSet(got, tc.want...) {
					t.Fatalf("slots=%v, want %v", got, tc.want)
				}
				return
			}
			if !sameLoginSet(got, "streamera", "streamerc") && !sameLoginSet(got, "streamerb", "streamerc") {
				t.Fatalf("slots=%v, want streamerc beside a fair-pair channel and streamerd waiting", got)
			}
			if reason := decisionReason(f.w, "streamerd"); !strings.Contains(reason, "streamerc already farms the same channel-restricted drop campaign") {
				t.Fatalf("streamerd reason=%q, want its carrier streamerc", reason)
			}
		})
	}
}

// FB-OD-10 current drop: a Skip rule counts only when it rules the campaign's
// current drop (the one farmed next), exactly as the drops tracker's broker
// views judge it, not any other or the final drop.
func TestRestrictedSkipRuleKeysOnCurrentDrop(t *testing.T) {
	cases := []struct {
		name         string
		drops        []string
		firstWatched int
		current      string
		skipped      bool
	}{
		{name: "only a later drop is skip-ruled", drops: []string{"Reward", "Skipped Reward"}, current: "Reward"},
		{
			name: "the current drop is skip-ruled, a later one not", drops: []string{"Skipped Reward", "Reward"},
			current: "Skipped Reward", skipped: true,
		},
		{
			name: "a skip-ruled drop already watched through, the current one not", drops: []string{"Skipped Reward", "Reward"},
			firstWatched: 60, current: "Reward",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newResidenceFixture(t, 4)
			byLogin := streamersByLogin(f.w.streamers)
			c, d := byLogin["streamerc"], byLogin["streamerd"]
			assignRestricted(c, restrictedTestCampaign("camp-streamerc", c))
			campaign := skipRuledRestrictedCampaign("camp-streamerd", d)
			campaign.Drops = []*models.Drop{
				{ID: "camp-streamerd-drop-1", Name: tc.drops[0], MinutesRequired: 60, CurrentMinutesWatched: tc.firstWatched},
				{ID: "camp-streamerd-drop-2", Name: tc.drops[1], MinutesRequired: 120},
			}
			if current := campaign.CurrentDrop(); current == nil || current.Name != tc.current {
				t.Fatalf("precondition: current drop=%v, want %q", current, tc.current)
			}
			assignRestricted(d, campaign)
			f.w.SetRewardSkips(skipRuledRewards())
			f.seedWeights(t, time.Now(), map[string]float64{
				"streamera": 5, "streamerb": 10, "streamerc": 60, "streamerd": 80,
			})
			if !containsIndex(f.w.getOnlineStreamers(nil), restrictedIndex(t, f.w, "streamerd")) {
				t.Fatalf("precondition: streamerd must stay an online candidate")
			}

			f.w.processWatching(tickCtx(f.w))

			got := restrictedSlotLogins(t, f)
			if !tc.skipped {
				if !sameLoginSet(got, "streamerc", "streamerd") {
					t.Fatalf("slots=%v, want [streamerc streamerd]: a skip-ruled drop that is not the current one does not exclude the campaign", got)
				}
				return
			}
			if !sameLoginSet(got, "streamera", "streamerc") && !sameLoginSet(got, "streamerb", "streamerc") {
				t.Fatalf("slots=%v, want streamerc beside a fair-pair channel and streamerd unseated", got)
			}
		})
	}
}

// R12 repaired, R9: the single boost gives a skip-ruled channel its seat, and
// the admission then gives that seat to a channel with restricted work beside
// an ordinary partner; the boost's victim, the displaced skip-ruled channel
// and the admitted one are told so without claiming that channel-restricted
// drops hold both slots.
func TestRestrictedSkipRuledBoostSeatReasons(t *testing.T) {
	f := newResidenceFixture(t, 5)
	byLogin := streamersByLogin(f.w.streamers)
	c, d, e := byLogin["streamerc"], byLogin["streamerd"], byLogin["streamere"]
	assignRestricted(c, skipRuledRestrictedCampaign("camp-streamerc", c))
	shared := restrictedTestCampaign("camp-shared", d, e)
	assignRestricted(d, shared)
	assignRestricted(e, shared)
	f.w.SetRewardSkips(skipRuledRewards())
	f.seedWeights(t, time.Now(), map[string]float64{
		"streamera": 5, "streamerb": 10, "streamerc": 20, "streamerd": 60, "streamere": 70,
	})

	f.w.processWatching(tickCtx(f.w))

	if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamera", "streamerd") {
		t.Fatalf("slots=%v, want [streamera streamerd]", got)
	}
	for login, want := range map[string]string{
		"streamerb": "whose seat then went to channel-restricted drop streamerd (keeps its rotation slot and competes again at the next evaluation)",
		"streamerc": "displaced by channel-restricted drop streamerd (it competes again at the next evaluation)",
		"streamerd": "watched: admitted as a channel-restricted drop, displacing streamerc",
		"streamere": "streamerd already farms the same channel-restricted drop campaign",
	} {
		if reason := decisionReason(f.w, login); !strings.Contains(reason, want) {
			t.Fatalf("%s reason=%q, want %q", login, reason, want)
		}
	}
}

// R12 repaired, R4/R8: the admission's victim order gives up the same seat
// whichever seat holds the skip-ruled channel, since both seats are ordinary
// for it and the less owed one goes.
func TestRestrictedSkipRuledVictimOrderIgnoresSeatOrder(t *testing.T) {
	f := newResidenceFixture(t, 4)
	byLogin := streamersByLogin(f.w.streamers)
	assignRestricted(byLogin["streamerb"], skipRuledRestrictedCampaign("camp-streamerb", byLogin["streamerb"]))
	f.w.SetRewardSkips(skipRuledRewards())
	f.w.rotation.deficitMinutes = map[string]float64{"streamera": 5, "streamerb": 10, "streamerc": 60, "streamerd": 70}
	skips := f.w.rewardSkips.Load()
	workOf := func(idx int) map[string]bool { return restrictedWork(f.w.streamers[idx], skips) }
	a, b := restrictedIndex(t, f.w, "streamera"), restrictedIndex(t, f.w, "streamerb")
	now := time.Now()

	for _, pair := range [][2]int{{a, b}, {b, a}} {
		seat := f.w.selectRestrictedAdmissionSeat(pair, workOf, now)
		if seat < 0 || pair[seat] != b {
			t.Fatalf("pair %v: gave up seat %d, want the less owed skip-ruled streamerb", pair, seat)
		}
	}
}

// R5 unchanged for a skip-ruled seat: the single boost applies no Skip rule, so
// while a skip-ruled channel holds a fair seat it still counts as a
// channel-restricted seat there. A lone qualifying channel of equal strength
// gets neither seat, and one that is strictly stronger (pursuing its watch
// streak) is seated beside it by the single boost's own victim order, which
// gives up the ordinary seat.
func TestRestrictedSkipRuledSeatStillRestrictedForSingleBoost(t *testing.T) {
	cases := []struct {
		name   string
		streak bool
		want   []string
	}{
		{name: "equal lone qualifying channel gets neither seat", want: []string{"streamera", "streamerb"}},
		{name: "strictly stronger lone qualifying channel takes the ordinary seat", streak: true, want: []string{"streamerb", "streamerc"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newResidenceFixture(t, 4)
			byLogin := streamersByLogin(f.w.streamers)
			b, c := byLogin["streamerb"], byLogin["streamerc"]
			assignRestricted(b, skipRuledRestrictedCampaign("camp-streamerb", b))
			assignRestricted(c, restrictedTestCampaign("camp-streamerc", c))
			if tc.streak {
				admissionPursuingStreak(c)
			}
			f.w.SetRewardSkips(skipRuledRewards())
			f.seedWeights(t, time.Now(), map[string]float64{
				"streamera": 5, "streamerb": 10, "streamerc": 60, "streamerd": 70,
			})

			f.w.processWatching(tickCtx(f.w))

			if got := restrictedSlotLogins(t, f); !sameLoginSet(got, tc.want...) {
				t.Fatalf("slots=%v, want %v", got, tc.want)
			}
		})
	}
}
