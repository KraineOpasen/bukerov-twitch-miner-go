package watcher

import (
	"strings"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// Owner rule PA-B1a: the second seat goes to a channel with channel-restricted
// Drops only when it brings restricted Drops work that the restricted channel in
// the other seat does not already carry, so one restricted campaign never holds
// both seats through PA-B1. These cases drive the real processWatching
// pipeline on the same fixture as restricted_two_slots_test.go and read the
// published BrokerSnapshot and debug decisions as the oracle.

// alreadyFarmedReason is the selection-reason fragment a qualifying channel
// gets when PA-B1a holds it back.
const alreadyFarmedReason = "already farms the same channel-restricted drop campaign"

// restrictedTestCampaign is one channel-restricted campaign with real remaining
// work whose allowlist names every given channel.
func restrictedTestCampaign(id string, channels ...*models.Streamer) *models.Campaign {
	campaign := watchSlotTestCampaign(id, "", false)
	for _, s := range channels {
		campaign.Channels = append(campaign.Channels, s.ChannelID)
	}
	return campaign
}

// assignRestricted makes s a drops channel whose assignment is exactly the
// given campaigns, with no watch-streak pursuit.
func assignRestricted(s *models.Streamer, campaigns ...*models.Campaign) {
	s.Settings.ClaimDrops = true
	s.Settings.WatchStreak = false
	ids := make([]string, 0, len(campaigns))
	for _, c := range campaigns {
		ids = append(ids, c.ID)
	}
	s.Stream.SetCampaignIDs(ids)
	s.Stream.SetCampaigns(campaigns)
}

// P1, R14(a): two off-pair channels assigned ONE shared restricted campaign get
// one seat between them; the other seat stays with the existing selection, the
// allocation holds over three ticks, and the waiting one is told which seated
// channel already farms its campaign.
func TestRestrictedSharedCampaignTakesOneSeat(t *testing.T) {
	f := newResidenceFixture(t, 4)
	byLogin := streamersByLogin(f.w.streamers)
	c, d := byLogin["streamerc"], byLogin["streamerd"]
	shared := restrictedTestCampaign("camp-shared", c, d)
	assignRestricted(c, shared)
	assignRestricted(d, shared)
	f.seedWeights(t, time.Now(), map[string]float64{
		"streamera": 10, "streamerb": 20, "streamerc": 80, "streamerd": 90,
	})

	for tick := range 3 {
		f.w.processWatching(tickCtx(f.w))
		if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamera", "streamerc") {
			t.Fatalf("tick %d: slots=%v, want one seat for the shared restricted campaign beside streamera", tick, got)
		}
		if reason := decisionReason(f.w, "streamerd"); !strings.Contains(reason, "streamerc "+alreadyFarmedReason) {
			t.Fatalf("tick %d: streamerd reason=%q, want it told streamerc already farms its campaign", tick, reason)
		}
	}
}

// P2, R14(a): when the two best-ranked qualifying channels share one restricted
// campaign, the second seat goes to the next channel that brings its own
// restricted campaign.
func TestRestrictedSharedCampaignYieldsToDistinctWork(t *testing.T) {
	f := newResidenceFixture(t, 5)
	byLogin := streamersByLogin(f.w.streamers)
	c, d, e := byLogin["streamerc"], byLogin["streamerd"], byLogin["streamere"]
	shared := restrictedTestCampaign("camp-shared", c, d)
	assignRestricted(c, shared)
	assignRestricted(d, shared)
	assignRestricted(e, restrictedTestCampaign("camp-streamere", e))
	f.seedWeights(t, time.Now(), map[string]float64{
		"streamera": 5, "streamerb": 10, "streamerc": 70, "streamerd": 80, "streamere": 90,
	})

	f.w.processWatching(tickCtx(f.w))

	if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamerc", "streamere") {
		t.Fatalf("slots=%v, want the distinct campaign streamere beside one channel of the shared campaign", got)
	}
	if reason := decisionReason(f.w, "streamerd"); !strings.Contains(reason, "streamerc "+alreadyFarmedReason) {
		t.Fatalf("streamerd reason=%q, want it told streamerc already farms its campaign", reason)
	}
}

// P3, R14(a) (control, same seats without PA-B1a): a channel that carries the
// seated channel's campaign plus a campaign of its own brings new restricted
// work, so both are seated.
func TestRestrictedSupersetWorkTakesSecondSeat(t *testing.T) {
	f := newResidenceFixture(t, 4)
	byLogin := streamersByLogin(f.w.streamers)
	c, d := byLogin["streamerc"], byLogin["streamerd"]
	shared := restrictedTestCampaign("camp-shared", c, d)
	assignRestricted(c, shared)
	assignRestricted(d, shared, restrictedTestCampaign("camp-streamerd", d))
	f.seedWeights(t, time.Now(), map[string]float64{
		"streamera": 10, "streamerb": 20, "streamerc": 80, "streamerd": 90,
	})

	for tick := range 2 {
		f.w.processWatching(tickCtx(f.w))
		if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamerc", "streamerd") {
			t.Fatalf("tick %d: slots=%v, want both seated because streamerd brings its own campaign", tick, got)
		}
	}
}

// P3b, R14(a): the converse order. With the superset channel seated first, the
// channel that carries only the shared campaign brings nothing new and waits.
func TestRestrictedSubsetWorkWaitsBesideSuperset(t *testing.T) {
	f := newResidenceFixture(t, 4)
	byLogin := streamersByLogin(f.w.streamers)
	c, d := byLogin["streamerc"], byLogin["streamerd"]
	shared := restrictedTestCampaign("camp-shared", c, d)
	assignRestricted(c, shared)
	assignRestricted(d, shared, restrictedTestCampaign("camp-streamerd", d))
	f.seedWeights(t, time.Now(), map[string]float64{
		"streamera": 10, "streamerb": 20, "streamerc": 90, "streamerd": 80,
	})

	f.w.processWatching(tickCtx(f.w))

	if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamera", "streamerd") {
		t.Fatalf("slots=%v, want the superset streamerd beside streamera and streamerc waiting", got)
	}
	if reason := decisionReason(f.w, "streamerc"); !strings.Contains(reason, "streamerd "+alreadyFarmedReason) {
		t.Fatalf("streamerc reason=%q, want it told streamerd already farms its campaign", reason)
	}
}

// P4, R14(b): two channels of one shared campaign hold both seats through
// ordinary selection; when a channel with a distinct restricted campaign comes
// online, it takes exactly one of their seats. With equal work the existing
// victim order picks the less-owed one; when one occupant's work is a strict
// subset of the other's, that subset occupant is the one displaced, even when
// the victim order alone would pick the other.
func TestRestrictedDuplicateOccupantYieldsToDistinctWork(t *testing.T) {
	cases := []struct {
		name      string
		superset  bool
		want      []string
		displaced string
		kept      string
	}{
		{name: "equal work", want: []string{"streamerc", "streamere"}, displaced: "streamerd", kept: "streamerc"},
		{name: "subset occupant", superset: true, want: []string{"streamerd", "streamere"}, displaced: "streamerc", kept: "streamerd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newResidenceFixture(t, 5)
			byLogin := streamersByLogin(f.w.streamers)
			c, d, e := byLogin["streamerc"], byLogin["streamerd"], byLogin["streamere"]
			shared := restrictedTestCampaign("camp-shared", c, d)
			assignRestricted(c, shared)
			if tc.superset {
				assignRestricted(d, shared, restrictedTestCampaign("camp-streamerd", d))
			} else {
				assignRestricted(d, shared)
			}
			assignRestricted(e, restrictedTestCampaign("camp-streamere", e))
			e.SetConfirmedOffline()
			f.seedWeights(t, time.Now(), map[string]float64{
				"streamera": 50, "streamerb": 60, "streamerc": 5, "streamerd": 10, "streamere": 90,
			})

			f.w.processWatching(tickCtx(f.w))
			if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamerc", "streamerd") {
				t.Fatalf("tick 1: slots=%v, want ordinary selection holding both channels of the shared campaign", got)
			}

			settleOnline(e)
			f.w.processWatching(tickCtx(f.w))
			if got := restrictedSlotLogins(t, f); !sameLoginSet(got, tc.want...) {
				t.Fatalf("tick 2: slots=%v, want %v", got, tc.want)
			}
			wantReason := tc.kept + " already farms its channel-restricted campaigns"
			if reason := decisionReason(f.w, tc.displaced); !strings.Contains(reason, wantReason) {
				t.Fatalf("tick 2: %s reason=%q, want it told %s still farms its campaign", tc.displaced, reason, tc.kept)
			}
			for tick := 3; tick <= 4; tick++ {
				f.w.processWatching(tickCtx(f.w))
				if got := restrictedSlotLogins(t, f); !sameLoginSet(got, tc.want...) {
					t.Fatalf("tick %d: slots=%v, want %v held", tick, got, tc.want)
				}
			}
		})
	}
}

// P5, R14(c) (control, same seats without PA-B1a): when the seated channel of a
// shared restricted campaign goes offline, another online channel of that
// campaign takes a seat at the next evaluation.
func TestRestrictedSharedCampaignContinuesOnOtherChannel(t *testing.T) {
	f := newResidenceFixture(t, 4)
	byLogin := streamersByLogin(f.w.streamers)
	c, d := byLogin["streamerc"], byLogin["streamerd"]
	shared := restrictedTestCampaign("camp-shared", c, d)
	assignRestricted(c, shared)
	assignRestricted(d, shared)
	f.seedWeights(t, time.Now(), map[string]float64{
		"streamera": 10, "streamerb": 20, "streamerc": 80, "streamerd": 90,
	})

	f.w.processWatching(tickCtx(f.w))
	if got := restrictedSlotLogins(t, f); !containsLogin(got, "streamerc") {
		t.Fatalf("tick 1: slots=%v, want streamerc farming the shared campaign", got)
	}

	c.SetConfirmedOffline()
	f.w.processWatching(tickCtx(f.w))
	got := restrictedSlotLogins(t, f)
	if containsLogin(got, "streamerc") || !containsLogin(got, "streamerd") {
		t.Fatalf("tick 2: slots=%v, want streamerd continuing the shared campaign after streamerc went offline", got)
	}
}

// R14 RW basis: only channel-restricted campaigns that still have remaining
// unclaimed work count, compared by campaign ID. A game-wide drop, a finished
// restricted drop, or a second copy of the same campaign object brings nothing
// new, so the channel waits beside the one already farming the shared campaign.
func TestRestrictedWorkCountsOnlyUnfinishedRestrictedCampaignIDs(t *testing.T) {
	cases := []struct {
		name   string
		assign func(c, d *models.Streamer)
	}{
		{name: "game-wide drop", assign: func(c, d *models.Streamer) {
			shared := restrictedTestCampaign("camp-shared", c, d)
			assignRestricted(c, shared)
			assignRestricted(d, shared, watchSlotTestCampaign("camp-gamewide", "", false))
		}},
		{name: "finished restricted drop", assign: func(c, d *models.Streamer) {
			shared := restrictedTestCampaign("camp-shared", c, d)
			finished := restrictedTestCampaign("camp-streamerd", d)
			finished.Drops[0].IsClaimed = true
			assignRestricted(c, shared)
			assignRestricted(d, shared, finished)
		}},
		{name: "same campaign ID in separate objects", assign: func(c, d *models.Streamer) {
			assignRestricted(c, restrictedTestCampaign("camp-shared", c, d))
			assignRestricted(d, restrictedTestCampaign("camp-shared", c, d))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newResidenceFixture(t, 4)
			byLogin := streamersByLogin(f.w.streamers)
			tc.assign(byLogin["streamerc"], byLogin["streamerd"])
			f.seedWeights(t, time.Now(), map[string]float64{
				"streamera": 10, "streamerb": 20, "streamerc": 80, "streamerd": 90,
			})

			f.w.processWatching(tickCtx(f.w))

			if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamera", "streamerc") {
				t.Fatalf("slots=%v, want streamerd to bring no new restricted work", got)
			}
			if reason := decisionReason(f.w, "streamerd"); !strings.Contains(reason, "streamerc "+alreadyFarmedReason) {
				t.Fatalf("streamerd reason=%q, want it told streamerc already farms its campaign", reason)
			}
		})
	}
}

// R14(a)+(b) within one evaluation: the channel bringing C2 takes the ordinary
// seat beside streamerb {C1}; streamerb is then a duplicate of it, so the next
// step gives streamerb's seat to streamerd {C3}. Both seats change in one
// evaluation, C1 stays farmed by streamerc, and the reasons name the final
// seats. The allocation holds over three ticks.
func TestRestrictedAdmissionChainsThroughDuplicate(t *testing.T) {
	f := newResidenceFixture(t, 4)
	byLogin := streamersByLogin(f.w.streamers)
	b, c, d := byLogin["streamerb"], byLogin["streamerc"], byLogin["streamerd"]
	c1 := restrictedTestCampaign("camp-c1", b, c)
	assignRestricted(b, c1)
	assignRestricted(c, c1, restrictedTestCampaign("camp-c2", c))
	assignRestricted(d, restrictedTestCampaign("camp-c3", d))
	f.seedWeights(t, time.Now(), map[string]float64{
		"streamera": 5, "streamerb": 10, "streamerc": 70, "streamerd": 80,
	})

	for tick := range 3 {
		f.w.processWatching(tickCtx(f.w))
		if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamerc", "streamerd") {
			t.Fatalf("tick %d: slots=%v, want [streamerc streamerd] after the duplicate step", tick, got)
		}
		if reason := decisionReason(f.w, "streamerb"); !strings.Contains(reason,
			"displaced by channel-restricted drop streamerd; streamerc already farms its channel-restricted campaigns") {
			t.Fatalf("tick %d: streamerb reason=%q, want its admitter and the carrier of its campaign", tick, reason)
		}
		if reason := decisionReason(f.w, "streamera"); !strings.Contains(reason, "displaced by channel-restricted drop streamerc") {
			t.Fatalf("tick %d: streamera reason=%q, want it displaced by streamerc", tick, reason)
		}
	}
}

// R14(b) with a retained-UNKNOWN duplicate: two channels of one shared
// campaign hold the fair pair and one goes UNKNOWN when a channel with its own
// campaign arrives. Between the two equal duplicates the existing victim order
// decides, by persisted deficit here: the less-owed one gives up its seat. When
// that is the UNKNOWN channel, the confirmed one keeps the shared campaign;
// when it is the confirmed one, the UNKNOWN channel keeps it and the reason
// says its status is unconfirmed instead of claiming it farms.
func TestRestrictedUnknownDuplicateFollowsVictimOrder(t *testing.T) {
	cases := []struct {
		name       string
		seeds      map[string]float64
		want       []string
		displaced  string
		wantReason string
	}{
		{
			name:       "unknown less owed is displaced",
			seeds:      map[string]float64{"streamera": 10, "streamerb": 50, "streamerc": 5, "streamerd": 90, "streamere": 60},
			want:       []string{"streamerc", "streamerd"},
			displaced:  "streamera",
			wantReason: "streamerc already farms its channel-restricted campaigns",
		},
		{
			name:       "unknown more owed keeps the campaign",
			seeds:      map[string]float64{"streamera": 5, "streamerb": 50, "streamerc": 10, "streamerd": 90, "streamere": 60},
			want:       []string{"streamera", "streamerd"},
			displaced:  "streamerc",
			wantReason: "streamera, whose status is unconfirmed, holds a slot with its channel-restricted campaigns",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newResidenceFixture(t, 5)
			byLogin := streamersByLogin(f.w.streamers)
			a, c, d := byLogin["streamera"], byLogin["streamerc"], byLogin["streamerd"]
			shared := restrictedTestCampaign("camp-shared", a, c)
			assignRestricted(a, shared)
			assignRestricted(c, shared)
			f.seedWeights(t, time.Now(), tc.seeds)

			f.w.processWatching(tickCtx(f.w))
			if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamera", "streamerc") {
				t.Fatalf("tick 1: slots=%v, want fairness holding both channels of the shared campaign", got)
			}

			a.SetUnknown(models.ReasonTransportError)
			assignRestricted(d, restrictedTestCampaign("camp-streamerd", d))
			f.w.processWatching(tickCtx(f.w))
			if got := restrictedSlotLogins(t, f); !sameLoginSet(got, tc.want...) {
				t.Fatalf("tick 2: slots=%v, want %v", got, tc.want)
			}
			if reason := decisionReason(f.w, tc.displaced); !strings.Contains(reason, tc.wantReason) {
				t.Fatalf("tick 2: %s reason=%q, want %q", tc.displaced, reason, tc.wantReason)
			}
		})
	}
}

// R14(a), unit level: "taken within this evaluation". With two open ordinary
// seats the first step seats streamerc of the shared campaign; the second open
// seat is then judged against streamerc, so streamerd of the same campaign is
// passed over for streamere's own campaign.
func TestRestrictedAdmissionJudgesSeatTakenThisEvaluation(t *testing.T) {
	f := newResidenceFixture(t, 5)
	byLogin := streamersByLogin(f.w.streamers)
	c, d, e := byLogin["streamerc"], byLogin["streamerd"], byLogin["streamere"]
	shared := restrictedTestCampaign("camp-shared", c, d)
	assignRestricted(c, shared)
	assignRestricted(d, shared)
	assignRestricted(e, restrictedTestCampaign("camp-streamere", e))
	f.w.rotation.deficitMinutes = map[string]float64{
		"streamera": 10, "streamerb": 20, "streamerc": 70, "streamerd": 75, "streamere": 80,
	}
	online := make([]int, len(f.w.streamers))
	for i := range online {
		online[i] = i
	}
	a, b := restrictedIndex(t, f.w, "streamera"), restrictedIndex(t, f.w, "streamerb")

	got := f.w.admitRestrictedDrops([2]int{a, b}, online, time.Now())

	if want := [2]int{restrictedIndex(t, f.w, "streamere"), restrictedIndex(t, f.w, "streamerc")}; got != want {
		t.Fatalf("pair=%v, want %v: streamerc first, then streamere's distinct campaign", got, want)
	}
	if reason := f.w.selectionReasons[restrictedIndex(t, f.w, "streamerd")]; !strings.Contains(reason, "streamerc "+alreadyFarmedReason) {
		t.Fatalf("streamerd reason=%q, want it told streamerc already farms its campaign", reason)
	}
}

// R14(a)+(b) over time: the superset channel is seated first, so the subset
// channel waits (P3b). Once persisted deficit makes the subset channel the
// stronger boost candidate, the boost seats it and the superset channel is
// admitted beside it because it brings its own campaign (P3); the subset
// channel is then a duplicate occupant, which only a channel with distinct
// work displaces, so both stay seated.
func TestRestrictedSupersetPairSettlesWithBothSeated(t *testing.T) {
	f := newResidenceFixture(t, 4)
	byLogin := streamersByLogin(f.w.streamers)
	c, d := byLogin["streamerc"], byLogin["streamerd"]
	shared := restrictedTestCampaign("camp-shared", c, d)
	assignRestricted(c, shared)
	assignRestricted(d, shared, restrictedTestCampaign("camp-streamerd", d))
	f.seedWeights(t, time.Now(), map[string]float64{
		"streamera": 10, "streamerb": 20, "streamerc": 90, "streamerd": 80,
	})

	f.w.processWatching(tickCtx(f.w))
	if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamera", "streamerd") {
		t.Fatalf("tick 1: slots=%v, want the superset streamerd beside streamera", got)
	}

	f.seedWeights(t, time.Now(), map[string]float64{"streamerd": 50})
	for tick := 2; tick <= 4; tick++ {
		f.w.processWatching(tickCtx(f.w))
		if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamerc", "streamerd") {
			t.Fatalf("tick %d: slots=%v, want both once streamerc is boosted first", tick, got)
		}
	}
}
