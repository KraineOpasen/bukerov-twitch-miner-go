package watcher

import (
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/policy"
)

// Owner rule PA-B1a: the second seat goes to a channel with channel-restricted
// Drops only when it brings restricted Drops work that the restricted channel in
// the other seat does not already carry, so a channel whose restricted
// campaigns the other seat already carries never takes the second seat. Most
// cases drive the real processWatching pipeline on the same fixture as
// restricted_two_slots_test.go and read the published BrokerSnapshot and debug
// decisions as the oracle; the unit-level cases drive admitRestrictedDrops
// directly for open seats the pipeline does not produce today, and the
// property case checks the admission's invariants over random assignments.

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
// shared restricted campaign stops qualifying — offline, unassigned or avoided —
// another online channel of that campaign takes a seat at the next evaluation.
func TestRestrictedSharedCampaignContinuesOnOtherChannel(t *testing.T) {
	cases := []struct {
		name string
		stop func(s *models.Streamer)
	}{
		{name: "offline", stop: func(s *models.Streamer) { s.SetConfirmedOffline() }},
		{name: "unassigned", stop: func(s *models.Streamer) {
			s.Stream.SetCampaignIDs(nil)
			s.Stream.SetCampaigns(nil)
		}},
		{name: "avoided", stop: func(s *models.Streamer) { s.Settings.Preference = models.PreferenceAvoid }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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

			tc.stop(c)
			f.w.processWatching(tickCtx(f.w))
			got := restrictedSlotLogins(t, f)
			if containsLogin(got, "streamerc") || !containsLogin(got, "streamerd") {
				t.Fatalf("tick 2: slots=%v, want streamerd continuing the shared campaign", got)
			}
		})
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

// R14(a) "or, within this evaluation, taken", unit level: with two open
// ordinary seats and two channels of one shared campaign, the first step seats
// streamerc and the second open seat is judged against it, so streamerd is not
// admitted and the seat stays with its occupant.
func TestRestrictedSecondOpenSeatJudgedAgainstFirstAdmission(t *testing.T) {
	f := newResidenceFixture(t, 4)
	byLogin := streamersByLogin(f.w.streamers)
	c, d := byLogin["streamerc"], byLogin["streamerd"]
	shared := restrictedTestCampaign("camp-shared", c, d)
	assignRestricted(c, shared)
	assignRestricted(d, shared)
	f.w.rotation.deficitMinutes = map[string]float64{
		"streamera": 10, "streamerb": 20, "streamerc": 70, "streamerd": 75,
	}
	online := []int{0, 1, 2, 3}
	a, b := restrictedIndex(t, f.w, "streamera"), restrictedIndex(t, f.w, "streamerb")

	got := f.w.admitRestrictedDrops([2]int{a, b}, online, time.Now())

	if want := [2]int{a, restrictedIndex(t, f.w, "streamerc")}; got != want {
		t.Fatalf("pair=%v, want %v: one seat for the shared campaign, streamera keeps the other", got, want)
	}
	if reason := f.w.selectionReasons[restrictedIndex(t, f.w, "streamerd")]; !strings.Contains(reason, "streamerc "+alreadyFarmedReason) {
		t.Fatalf("streamerd reason=%q, want it told streamerc already farms its campaign", reason)
	}
}

// R14(a) "taken within this evaluation", pipeline: the boost seats streamerc
// {C1}; the admission seats streamerd {C1,C2} beside it; streamerc is then a
// duplicate, but streamere {C2} brings nothing streamerd does not carry, so
// streamerc keeps its seat and streamere waits.
func TestRestrictedAdmissionJudgesAgainstChannelJustSeated(t *testing.T) {
	f := newResidenceFixture(t, 5)
	byLogin := streamersByLogin(f.w.streamers)
	c, d, e := byLogin["streamerc"], byLogin["streamerd"], byLogin["streamere"]
	c1 := restrictedTestCampaign("camp-c1", c, d)
	c2 := restrictedTestCampaign("camp-c2", d, e)
	assignRestricted(c, c1)
	assignRestricted(d, c1, c2)
	assignRestricted(e, c2)
	f.seedWeights(t, time.Now(), map[string]float64{
		"streamera": 5, "streamerb": 10, "streamerc": 70, "streamerd": 80, "streamere": 90,
	})

	f.w.processWatching(tickCtx(f.w))

	if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamerc", "streamerd") {
		t.Fatalf("slots=%v, want [streamerc streamerd]; streamere brings nothing streamerd does not carry", got)
	}
	if reason := decisionReason(f.w, "streamere"); !strings.Contains(reason, "streamerd "+alreadyFarmedReason) {
		t.Fatalf("streamere reason=%q, want it told streamerd already farms its campaign", reason)
	}
}

// R14(b) chain of three steps with R9 reasons from the final pair: each
// admitted channel carries all the work of the one before it, so both seats are
// replaced and streamerd is admitted and replaced again in the same
// evaluation. Every reason names the final seats.
func TestRestrictedAdmissionThreeStepChainReasons(t *testing.T) {
	f := newResidenceFixture(t, 6)
	byLogin := streamersByLogin(f.w.streamers)
	c, d, e, g := byLogin["streamerc"], byLogin["streamerd"], byLogin["streamere"], byLogin["streamerf"]
	k1 := restrictedTestCampaign("camp-k1", c, d, e)
	k2 := restrictedTestCampaign("camp-k2", d, e)
	assignRestricted(c, k1)
	assignRestricted(d, k1, k2)
	assignRestricted(e, k1, k2, restrictedTestCampaign("camp-k3", e))
	assignRestricted(g, restrictedTestCampaign("camp-k4", g))
	f.seedWeights(t, time.Now(), map[string]float64{
		"streamera": 5, "streamerb": 10, "streamerc": 60, "streamerd": 70, "streamere": 80, "streamerf": 90,
	})

	f.w.processWatching(tickCtx(f.w))

	if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamere", "streamerf") {
		t.Fatalf("slots=%v, want [streamere streamerf] after the chain", got)
	}
	for login, want := range map[string]string{
		"streamerf": "admitted beside another channel-restricted drop, displacing streamera",
		"streamere": "admitted beside another channel-restricted drop, displacing streamerc",
		"streamera": "displaced by channel-restricted drop streamerf (channel-restricted drops may hold both slots",
		"streamerc": "displaced by channel-restricted drop streamere; streamere already farms its channel-restricted campaigns",
		"streamerd": "waiting: streamere " + alreadyFarmedReason,
	} {
		if reason := decisionReason(f.w, login); !strings.Contains(reason, want) {
			t.Fatalf("%s reason=%q, want %q", login, reason, want)
		}
	}
}

// R14(b), per step: two equal duplicates hold the fair pair. The first step
// gives up exactly one of them (the less owed) for streamerc, which carries
// their campaign and more; the remaining one is then a duplicate of streamerc,
// so the next step gives it up for streamerd's own campaign. The shared
// campaign stays farmed by streamerc.
func TestRestrictedEqualDuplicatesReplacedStepByStep(t *testing.T) {
	f := newResidenceFixture(t, 4)
	byLogin := streamersByLogin(f.w.streamers)
	a, b, c, d := byLogin["streamera"], byLogin["streamerb"], byLogin["streamerc"], byLogin["streamerd"]
	shared := restrictedTestCampaign("camp-shared", a, b, c)
	assignRestricted(a, shared)
	assignRestricted(b, shared)
	assignRestricted(c, shared, restrictedTestCampaign("camp-streamerc", c))
	assignRestricted(d, restrictedTestCampaign("camp-streamerd", d))
	f.seedWeights(t, time.Now(), map[string]float64{
		"streamera": 5, "streamerb": 10, "streamerc": 70, "streamerd": 80,
	})

	f.w.processWatching(tickCtx(f.w))

	if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamerc", "streamerd") {
		t.Fatalf("slots=%v, want [streamerc streamerd]", got)
	}
	for _, login := range []string{"streamera", "streamerb"} {
		if reason := decisionReason(f.w, login); !strings.Contains(reason, "streamerc already farms its channel-restricted campaigns") {
			t.Fatalf("%s reason=%q, want it told streamerc carries its campaign", login, reason)
		}
	}
}

// R14(b) with a retained-UNKNOWN superset: the UNKNOWN channel is still a
// restricted occupant, so a confirmed channel whose work it fully carries is a
// duplicate and gives up its seat for distinct work; the reason reports the
// carrier as unconfirmed.
func TestRestrictedUnknownSupersetMakesConfirmedSubsetDuplicate(t *testing.T) {
	f := newResidenceFixture(t, 4)
	byLogin := streamersByLogin(f.w.streamers)
	a, c, d := byLogin["streamera"], byLogin["streamerc"], byLogin["streamerd"]
	shared := restrictedTestCampaign("camp-shared", a, c)
	assignRestricted(a, shared, restrictedTestCampaign("camp-streamera", a))
	assignRestricted(c, shared)
	f.seedWeights(t, time.Now(), map[string]float64{
		"streamera": 5, "streamerb": 50, "streamerc": 10, "streamerd": 90,
	})

	f.w.processWatching(tickCtx(f.w))
	if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamera", "streamerc") {
		t.Fatalf("tick 1: slots=%v, want fairness holding streamera and streamerc", got)
	}

	a.SetUnknown(models.ReasonTransportError)
	assignRestricted(d, restrictedTestCampaign("camp-streamerd", d))
	f.w.processWatching(tickCtx(f.w))

	if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamera", "streamerd") {
		t.Fatalf("tick 2: slots=%v, want the confirmed subset streamerc displaced for streamerd", got)
	}
	want := "streamera, whose status is unconfirmed, holds a slot with its channel-restricted campaigns"
	if reason := decisionReason(f.w, "streamerc"); !strings.Contains(reason, want) {
		t.Fatalf("streamerc reason=%q, want %q", reason, want)
	}
}

// R7 with R14(b): a latched restricted streak whose work the fairly seated
// restricted channel fully carries is a duplicate, so the admission gives its
// seat to distinct work on every tick; the latch bookkeeping is left exactly
// as the single boost wrote it.
func TestRestrictedLatchedDuplicateKeepsLatchBookkeeping(t *testing.T) {
	f := newResidenceFixture(t, 4)
	byLogin := streamersByLogin(f.w.streamers)
	a, c, d := byLogin["streamera"], byLogin["streamerc"], byLogin["streamerd"]
	shared := restrictedTestCampaign("camp-shared", a, c)
	assignRestricted(a, shared, restrictedTestCampaign("camp-streamera", a))
	assignRestricted(c, shared)
	admissionPursuingStreak(c)
	assignRestricted(d, restrictedTestCampaign("camp-streamerd", d))
	f.seedWeights(t, time.Now(), map[string]float64{
		"streamera": 5, "streamerb": 10, "streamerc": 70, "streamerd": 80,
	})
	latched := restrictedIndex(t, f.w, "streamerc")
	victim := restrictedIndex(t, f.w, "streamerb")

	for tick := range 3 {
		f.w.processWatching(tickCtx(f.w))
		if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamera", "streamerd") {
			t.Fatalf("tick %d: slots=%v, want the duplicate streak's seat given to streamerd", tick, got)
		}
		if !f.w.rotation.boostLatched || f.w.rotation.boostTarget != latched || f.w.rotation.boostVictim != victim {
			t.Fatalf("tick %d: latch=%v target=%d victim=%d, want the single boost's target %d and victim %d",
				tick, f.w.rotation.boostLatched, f.w.rotation.boostTarget, f.w.rotation.boostVictim, latched, victim)
		}
	}
}

// R9 precedence: a qualifying channel that the single boost displaced keeps the
// boost's reason instead of the admission's waiting reason.
func TestRestrictedWaitingReasonKeepsBoostVictimReason(t *testing.T) {
	f := newResidenceFixture(t, 4)
	byLogin := streamersByLogin(f.w.streamers)
	utilities := map[string]policy.SemanticUtility{}
	for login, class := range map[string]policy.SemanticClass{"streamera": 2, "streamerb": 2, "streamerc": 0} {
		makeDropCandidate(byLogin[login], true)
		utilities[login] = policy.SemanticUtility{SemanticClass: class}
	}
	f.w.SetCampaignSemanticPolicy(utilities, nil, nil)
	f.seedWeights(t, time.Now(), map[string]float64{
		"streamera": 5, "streamerb": 10, "streamerc": 70, "streamerd": 90,
	})

	f.w.processWatching(tickCtx(f.w))

	if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamera", "streamerc") {
		t.Fatalf("slots=%v, want the stronger streamerc boosted beside streamera", got)
	}
	if reason := decisionReason(f.w, "streamerb"); !strings.Contains(reason, "displaced by a DROPS/STREAK boost") {
		t.Fatalf("streamerb reason=%q, want the single boost's displacement reason kept", reason)
	}
}

// Property, unit level: over random assignments of restricted campaigns and
// random starting pairs, the admission keeps its invariants: the two seats stay
// distinct and come from the starting pair or the qualifying channels; every
// displaced restricted channel's campaigns are still carried by a final seat;
// no seat the admission could still give up has a waiting channel that brings
// new work against the other seat; and every waiting qualifying channel gets a
// reason.
func TestRestrictedAdmissionInvariantsHoldForRandomAssignments(t *testing.T) {
	f := newResidenceFixture(t, 6)
	w := f.w
	ids := []string{"camp-k1", "camp-k2", "camp-k3"}
	rng := rand.New(rand.NewSource(20261010))
	online := []int{0, 1, 2, 3, 4, 5}

	for iteration := range 400 {
		// Random restricted work per channel; an empty set is an ordinary channel.
		work := make([][]string, len(w.streamers))
		for i := range w.streamers {
			for _, id := range ids {
				if rng.Intn(3) == 0 {
					work[i] = append(work[i], id)
				}
			}
		}
		for i, s := range w.streamers {
			var campaigns []*models.Campaign
			for _, id := range work[i] {
				var holders []*models.Streamer
				for j, other := range w.streamers {
					if containsLogin(work[j], id) {
						holders = append(holders, other)
					}
				}
				campaigns = append(campaigns, restrictedTestCampaign(id, holders...))
			}
			if len(campaigns) == 0 {
				s.Settings.ClaimDrops = false
				s.Stream.SetCampaignIDs(nil)
				s.Stream.SetCampaigns(nil)
				continue
			}
			assignRestricted(s, campaigns...)
		}
		w.rotation.deficitMinutes = map[string]float64{}
		for _, s := range w.streamers {
			w.rotation.deficitMinutes[s.GetUsername()] = float64(rng.Intn(100))
		}
		w.selectionReasons = make(map[int]string)
		first := rng.Intn(len(w.streamers))
		second := (first + 1 + rng.Intn(len(w.streamers)-1)) % len(w.streamers)
		start := [2]int{first, second}

		got := w.admitRestrictedDrops(start, online, time.Now())

		rwOf := func(idx int) map[string]bool { return restrictedWork(w.streamers[idx]) }
		qualifying := map[int]bool{}
		for i, s := range w.streamers {
			if s.DropsCondition() && s.HasChannelRestrictedCampaign() {
				qualifying[i] = true
			}
		}
		if got[0] == got[1] {
			t.Fatalf("iteration %d: pair %v holds one channel twice", iteration, got)
		}
		for _, seat := range got {
			if seat != start[0] && seat != start[1] && !qualifying[seat] {
				t.Fatalf("iteration %d: pair %v seats %d, which neither held a seat nor qualifies", iteration, got, seat)
			}
		}
		for _, out := range start {
			if out == got[0] || out == got[1] || len(rwOf(out)) == 0 {
				continue
			}
			if !restrictedWorkSubset(rwOf(out), rwOf(got[0])) && !restrictedWorkSubset(rwOf(out), rwOf(got[1])) {
				t.Fatalf("iteration %d: displaced %d lost restricted work %v (final %v)", iteration, out, rwOf(out), got)
			}
		}
		if len(qualifying) < 2 {
			if got != start {
				t.Fatalf("iteration %d: fewer than two qualifying channels changed %v to %v", iteration, start, got)
			}
			continue
		}
		for i, seat := range got {
			other := got[1-i]
			duplicate := len(rwOf(seat)) > 0 && len(rwOf(other)) > 0 && restrictedWorkSubset(rwOf(seat), rwOf(other))
			if len(rwOf(seat)) > 0 && !duplicate {
				continue
			}
			for idx := range qualifying {
				if idx == got[0] || idx == got[1] {
					continue
				}
				if len(rwOf(other)) == 0 || !restrictedWorkSubset(rwOf(idx), rwOf(other)) {
					t.Fatalf("iteration %d: seat %d of %v could still go to %d, which brings new work", iteration, seat, got, idx)
				}
			}
		}
		for idx := range qualifying {
			if idx != got[0] && idx != got[1] && w.selectionReasons[idx] == "" {
				t.Fatalf("iteration %d: waiting qualifying channel %d has no reason", iteration, idx)
			}
		}
	}
}
