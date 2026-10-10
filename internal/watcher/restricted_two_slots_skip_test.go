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
			for _, admission := range []string{"admitted beside another channel-restricted drop", "(owner rule PA-B1a)", waitingForRestrictedSeats} {
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

// R12 repaired, R4: between an ordinary seat and a skip-ruled one, both
// unprotected, the victim order no longer counts the skip-ruled campaign as
// restricted, so the less owed of the two is given up; streamerd, carried by
// streamerc, waits. Without the rule the skip-ruled channel is a restricted
// occupant and the ordinary seat goes.
func TestRestrictedSkipRuledSeatGivenUpAsOrdinary(t *testing.T) {
	cases := []struct {
		name    string
		skipped bool
		want    []string
	}{
		{name: "skip-ruled seat competes as ordinary", skipped: true, want: []string{"streamera", "streamerc"}},
		{name: "control: the same seat without the rule", want: []string{"streamerb", "streamerc"}},
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
			f.seedWeights(t, time.Now(), map[string]float64{
				"streamera": 5, "streamerb": 10, "streamerc": 60, "streamerd": 70,
			})

			f.w.processWatching(tickCtx(f.w))

			if got := restrictedSlotLogins(t, f); !sameLoginSet(got, tc.want...) {
				t.Fatalf("slots=%v, want %v", got, tc.want)
			}
			if reason := decisionReason(f.w, "streamerd"); !strings.Contains(reason, "streamerc already farms the same channel-restricted drop campaign") {
				t.Fatalf("streamerd reason=%q, want its carrier streamerc", reason)
			}
		})
	}
}
