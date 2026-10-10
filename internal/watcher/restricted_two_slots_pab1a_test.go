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
// decisions as the oracle; the unit-level cases drive admitRestrictedDrops, or
// its waiting-reason helper, directly for states the pipeline does not produce
// deterministically or only through a long setup (two open seats, a carrier
// confirmed offline after the online list was read, an assignment changing
// between two reads, carriers in a chosen seat order), and the property case
// checks the admission's invariants over random assignments.

// alreadyFarmedReason is the selection-reason fragment a qualifying channel
// gets when PA-B1a holds it back.
const alreadyFarmedReason = "already farms the same channel-restricted drop campaign"

// boostVictimReason is the reason the single boost writes for the base member
// whose seat it takes.
const boostVictimReason = "not watched this tick: displaced by a DROPS/STREAK boost (keeps its rotation slot and returns when the boost ends)"

// restrictedTestCampaign is one channel-restricted campaign with real remaining
// work whose allowlist names every given channel. Every such campaign shares
// one display name, so only its ID tells campaigns apart, as RW requires.
func restrictedTestCampaign(id string, channels ...*models.Streamer) *models.Campaign {
	campaign := watchSlotTestCampaign(id, "", false)
	campaign.Name = "Partner Drops"
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

	got := f.w.admitRestrictedDrops([2]int{a, b}, [2]int{a, b}, online, time.Now())

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

	got := f.w.admitRestrictedDrops([2]int{a, b}, [2]int{a, b}, online, time.Now())

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

// R14(b) chain of three steps with R9 reasons from the final pair: at each step
// the displaced channel's work is carried by the channel then holding the other
// seat, so both seats are replaced and streamerd is admitted and replaced again
// in the same evaluation. Every reason names the final seats.
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

// R4 with R14(b): of two equal duplicates holding the fair pair, the victim
// order decides by hard and semantic class before persisted deficit, so the
// stronger duplicate keeps its seat even when it is the less owed one: the one
// with the better Campaign Policy utility, or the one pursuing its watch
// streak.
func TestRestrictedEqualDuplicatesVictimOrderUsesHardAndSemanticClass(t *testing.T) {
	cases := []struct {
		name      string
		setup     func(f *residenceFixture, a *models.Streamer)
		seeds     map[string]float64
		kept      string
		displaced string
	}{
		{
			name: "better utility kept",
			setup: func(f *residenceFixture, _ *models.Streamer) {
				f.w.SetCampaignSemanticPolicy(map[string]policy.SemanticUtility{
					"streamera": {SemanticClass: 2}, "streamerb": {SemanticClass: 0}, "streamerd": {SemanticClass: 1},
				}, nil, nil)
			},
			seeds:     map[string]float64{"streamera": 5, "streamerb": 10, "streamerc": 50, "streamerd": 80},
			kept:      "streamerb",
			displaced: "streamera",
		},
		{
			name: "streak in progress kept",
			setup: func(_ *residenceFixture, a *models.Streamer) {
				admissionPursuingStreak(a)
			},
			seeds:     map[string]float64{"streamera": 10, "streamerb": 5, "streamerc": 50, "streamerd": 80},
			kept:      "streamera",
			displaced: "streamerb",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newResidenceFixture(t, 4)
			byLogin := streamersByLogin(f.w.streamers)
			a, b, d := byLogin["streamera"], byLogin["streamerb"], byLogin["streamerd"]
			shared := restrictedTestCampaign("camp-shared", a, b)
			assignRestricted(a, shared)
			assignRestricted(b, shared)
			assignRestricted(d, restrictedTestCampaign("camp-streamerd", d))
			tc.setup(f, a)
			f.seedWeights(t, time.Now(), tc.seeds)

			f.w.processWatching(tickCtx(f.w))

			if got := restrictedSlotLogins(t, f); !sameLoginSet(got, tc.kept, "streamerd") {
				t.Fatalf("slots=%v, want the stronger duplicate %s kept beside streamerd", got, tc.kept)
			}
			want := "displaced by channel-restricted drop streamerd; " + tc.kept + " already farms its channel-restricted campaigns"
			if reason := decisionReason(f.w, tc.displaced); !strings.Contains(reason, want) {
				t.Fatalf("%s reason=%q, want %q", tc.displaced, reason, want)
			}
		})
	}
}

// R14(a) per step, owner question (E): each step is judged against the channel
// then holding the other seat, so streamerd {k1,k2} is admitted beside the
// boosted streamerc {k1}; streamerc is then a duplicate and streamere
// {k1,k2,k3} takes its seat, which leaves streamerd seated beside a channel
// that carries all of its work. Every campaign stays farmed and the allocation
// holds over three ticks.
func TestRestrictedAdmissionCanSeatChannelLaterCarriedByItsPartner(t *testing.T) {
	f := newResidenceFixture(t, 5)
	byLogin := streamersByLogin(f.w.streamers)
	c, d, e := byLogin["streamerc"], byLogin["streamerd"], byLogin["streamere"]
	k1 := restrictedTestCampaign("camp-k1", c, d, e)
	k2 := restrictedTestCampaign("camp-k2", d, e)
	assignRestricted(c, k1)
	assignRestricted(d, k1, k2)
	assignRestricted(e, k1, k2, restrictedTestCampaign("camp-k3", e))
	f.seedWeights(t, time.Now(), map[string]float64{
		"streamera": 5, "streamerb": 10, "streamerc": 60, "streamerd": 70, "streamere": 80,
	})

	for tick := range 3 {
		f.w.processWatching(tickCtx(f.w))
		if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamerd", "streamere") {
			t.Fatalf("tick %d: slots=%v, want [streamerd streamere] from the per-step chain", tick, got)
		}
		for login, want := range map[string]string{
			"streamerd": "admitted beside another channel-restricted drop, displacing streamera",
			"streamere": "admitted beside another channel-restricted drop, displacing streamerc",
			"streamerc": "displaced by channel-restricted drop streamere; streamerd already farms its channel-restricted campaigns",
		} {
			if reason := decisionReason(f.w, login); !strings.Contains(reason, want) {
				t.Fatalf("tick %d: %s reason=%q, want %q", tick, login, reason, want)
			}
		}
	}
}

// R14(b) with a retained-UNKNOWN superset: the UNKNOWN channel is still a
// restricted occupant, so a confirmed channel whose work it fully carries is a
// duplicate and gives up its seat for distinct work. The reason reports the
// UNKNOWN carrier as unconfirmed; when the admitted channel carries that work
// too, the reason names it, the carrier confirmed online.
func TestRestrictedUnknownSupersetMakesConfirmedSubsetDuplicate(t *testing.T) {
	cases := []struct {
		name string
		// assign seats the shared campaign on streamera and streamerc and
		// returns the step that assigns streamerd before the second tick.
		assign func(a, c, d *models.Streamer) func()
		want   string
	}{
		{
			name: "only the UNKNOWN channel carries it",
			assign: func(a, c, d *models.Streamer) func() {
				shared := restrictedTestCampaign("camp-shared", a, c)
				assignRestricted(a, shared, restrictedTestCampaign("camp-streamera", a))
				assignRestricted(c, shared)
				return func() { assignRestricted(d, restrictedTestCampaign("camp-streamerd", d)) }
			},
			want: "displaced by channel-restricted drop streamerd; streamera, whose status is unconfirmed, holds a slot with its channel-restricted campaigns",
		},
		{
			name: "the online admitted channel carries it too",
			assign: func(a, c, d *models.Streamer) func() {
				shared := restrictedTestCampaign("camp-shared", a, c, d)
				assignRestricted(a, shared, restrictedTestCampaign("camp-streamera", a))
				assignRestricted(c, shared)
				return func() { assignRestricted(d, shared, restrictedTestCampaign("camp-streamerd", d)) }
			},
			want: "displaced by channel-restricted drop streamerd; streamerd already farms its channel-restricted campaigns",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newResidenceFixture(t, 4)
			byLogin := streamersByLogin(f.w.streamers)
			a, c, d := byLogin["streamera"], byLogin["streamerc"], byLogin["streamerd"]
			assignD := tc.assign(a, c, d)
			f.seedWeights(t, time.Now(), map[string]float64{
				"streamera": 5, "streamerb": 50, "streamerc": 10, "streamerd": 90,
			})

			f.w.processWatching(tickCtx(f.w))
			if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamera", "streamerc") {
				t.Fatalf("tick 1: slots=%v, want fairness holding streamera and streamerc", got)
			}

			a.SetUnknown(models.ReasonTransportError)
			assignD()
			f.w.processWatching(tickCtx(f.w))

			if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamera", "streamerd") {
				t.Fatalf("tick 2: slots=%v, want the confirmed subset streamerc displaced for streamerd", got)
			}
			if reason := decisionReason(f.w, "streamerc"); !strings.Contains(reason, tc.want) {
				t.Fatalf("streamerc reason=%q, want %q", reason, tc.want)
			}
		})
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
// boost's reason instead of the admission's both-seats waiting reason while the
// boost target still holds its seat, whichever seat that is and whether or not
// the admission changes the other seat. When the admission gives that seat away
// (TestRestrictedBoostVictimToldWhoHoldsItsSeat) or one seated channel carries
// all of the victim's restricted campaigns
// (TestRestrictedBoostVictimHeldBackByCarrierIsToldItsCarrier), the admission's
// reason replaces it.
func TestRestrictedWaitingReasonKeepsBoostVictimReason(t *testing.T) {
	distinct := func(classes map[string]policy.SemanticClass) func(f *residenceFixture, byLogin map[string]*models.Streamer) {
		return func(f *residenceFixture, byLogin map[string]*models.Streamer) {
			utilities := map[string]policy.SemanticUtility{}
			for login, class := range classes {
				makeDropCandidate(byLogin[login], true)
				utilities[login] = policy.SemanticUtility{SemanticClass: class}
			}
			f.w.SetCampaignSemanticPolicy(utilities, nil, nil)
		}
	}
	cases := []struct {
		name       string
		setup      func(f *residenceFixture, byLogin map[string]*models.Streamer)
		seeds      map[string]float64
		want       []string
		victim     string
		alsoReason map[string]string
	}{
		{
			name:   "victim in the second seat",
			setup:  distinct(map[string]policy.SemanticClass{"streamera": 2, "streamerb": 2, "streamerc": 0}),
			seeds:  map[string]float64{"streamera": 5, "streamerb": 10, "streamerc": 70, "streamerd": 90},
			want:   []string{"streamera", "streamerc"},
			victim: "streamerb",
		},
		{
			name:   "victim in the first seat",
			setup:  distinct(map[string]policy.SemanticClass{"streamera": 2, "streamerb": 1, "streamerc": 0}),
			seeds:  map[string]float64{"streamera": 5, "streamerb": 10, "streamerc": 50, "streamerd": 80},
			want:   []string{"streamerb", "streamerc"},
			victim: "streamera",
		},
		{
			name: "admission changes the other seat",
			setup: func(f *residenceFixture, byLogin map[string]*models.Streamer) {
				a, b, c, d := byLogin["streamera"], byLogin["streamerb"], byLogin["streamerc"], byLogin["streamerd"]
				k1 := restrictedTestCampaign("camp-k1", a, c)
				assignRestricted(a, k1)
				assignRestricted(b, restrictedTestCampaign("camp-streamerb", b))
				assignRestricted(c, k1, restrictedTestCampaign("camp-streamerc", c))
				assignRestricted(d, restrictedTestCampaign("camp-streamerd", d))
				f.w.SetCampaignSemanticPolicy(map[string]policy.SemanticUtility{
					"streamera": {SemanticClass: 1}, "streamerb": {SemanticClass: 2},
					"streamerc": {SemanticClass: 0}, "streamerd": {SemanticClass: 1},
				}, nil, nil)
			},
			seeds:  map[string]float64{"streamera": 5, "streamerb": 10, "streamerc": 50, "streamerd": 80},
			want:   []string{"streamerc", "streamerd"},
			victim: "streamerb",
			alsoReason: map[string]string{
				"streamera": "displaced by channel-restricted drop streamerd; streamerc already farms its channel-restricted campaigns",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newResidenceFixture(t, 4)
			byLogin := streamersByLogin(f.w.streamers)
			tc.setup(f, byLogin)
			f.seedWeights(t, time.Now(), tc.seeds)

			f.w.processWatching(tickCtx(f.w))

			if got := restrictedSlotLogins(t, f); !sameLoginSet(got, tc.want...) {
				t.Fatalf("slots=%v, want %v", got, tc.want)
			}
			if reason := decisionReason(f.w, tc.victim); reason != boostVictimReason {
				t.Fatalf("%s reason=%q, want the single boost's displacement reason kept", tc.victim, reason)
			}
			for login, want := range tc.alsoReason {
				if reason := decisionReason(f.w, login); !strings.Contains(reason, want) {
					t.Fatalf("%s reason=%q, want %q", login, reason, want)
				}
			}
		})
	}
}

// R9 with F1 of the review: when the single boost took a base member's seat and
// the admission then gives that seat to another channel, the base member is
// told which channel now holds it instead of keeping the boost's reason. A
// qualifying base member gets its waiting reason, or the admission's own
// reason when the admission seats it again; an ordinary one names the channel
// in its seat.
func TestRestrictedBoostVictimToldWhoHoldsItsSeat(t *testing.T) {
	cases := []struct {
		name       string
		setup      func(t *testing.T, f *residenceFixture, byLogin map[string]*models.Streamer)
		want       []string
		wantReason string
	}{
		{
			name: "latched streak displaced as a duplicate",
			setup: func(t *testing.T, f *residenceFixture, byLogin map[string]*models.Streamer) {
				a, c, d := byLogin["streamera"], byLogin["streamerc"], byLogin["streamerd"]
				shared := restrictedTestCampaign("camp-shared", a, c)
				assignRestricted(a, shared, restrictedTestCampaign("camp-streamera", a))
				assignRestricted(c, shared)
				admissionPursuingStreak(c)
				assignRestricted(d, restrictedTestCampaign("camp-streamerd", d))
				f.seedWeights(t, time.Now(), map[string]float64{
					"streamera": 5, "streamerb": 10, "streamerc": 70, "streamerd": 80,
				})
			},
			want:       []string{"streamera", "streamerd"},
			wantReason: "displaced by a DROPS/STREAK boost whose seat then went to channel-restricted drop streamerd",
		},
		{
			name: "stronger utility displaced as a duplicate",
			setup: func(t *testing.T, f *residenceFixture, byLogin map[string]*models.Streamer) {
				a, c, d := byLogin["streamera"], byLogin["streamerc"], byLogin["streamerd"]
				shared := restrictedTestCampaign("camp-shared", a, c)
				assignRestricted(a, shared, restrictedTestCampaign("camp-streamera", a))
				assignRestricted(c, shared)
				assignRestricted(d, restrictedTestCampaign("camp-streamerd", d))
				f.w.SetCampaignSemanticPolicy(map[string]policy.SemanticUtility{
					"streamera": {SemanticClass: 2}, "streamerc": {SemanticClass: 0}, "streamerd": {SemanticClass: 1},
				}, nil, nil)
				f.seedWeights(t, time.Now(), map[string]float64{
					"streamera": 5, "streamerb": 10, "streamerc": 70, "streamerd": 80,
				})
			},
			want:       []string{"streamera", "streamerd"},
			wantReason: "displaced by a DROPS/STREAK boost whose seat then went to channel-restricted drop streamerd",
		},
		{
			name: "qualifying base member left waiting",
			setup: func(t *testing.T, f *residenceFixture, byLogin map[string]*models.Streamer) {
				a, b, c, d := byLogin["streamera"], byLogin["streamerb"], byLogin["streamerc"], byLogin["streamerd"]
				shared := restrictedTestCampaign("camp-shared", a, c)
				assignRestricted(a, shared, restrictedTestCampaign("camp-streamera", a))
				assignRestricted(b, restrictedTestCampaign("camp-streamerb", b))
				assignRestricted(c, shared)
				assignRestricted(d, restrictedTestCampaign("camp-streamerd", d))
				f.w.SetCampaignSemanticPolicy(map[string]policy.SemanticUtility{
					"streamera": {SemanticClass: 2}, "streamerb": {SemanticClass: 2},
					"streamerc": {SemanticClass: 0}, "streamerd": {SemanticClass: 1},
				}, nil, nil)
				f.seedWeights(t, time.Now(), map[string]float64{
					"streamera": 5, "streamerb": 10, "streamerc": 70, "streamerd": 90,
				})
			},
			want:       []string{"streamera", "streamerd"},
			wantReason: waitingForRestrictedSeats + " (streamera, streamerd)",
		},
		{
			name: "boost victim re-seated by the admission",
			setup: func(t *testing.T, f *residenceFixture, byLogin map[string]*models.Streamer) {
				a, b, c := byLogin["streamera"], byLogin["streamerb"], byLogin["streamerc"]
				k1 := restrictedTestCampaign("camp-k1", a, b, c)
				assignRestricted(a, k1, restrictedTestCampaign("camp-streamera", a))
				assignRestricted(b, k1, restrictedTestCampaign("camp-streamerb", b))
				assignRestricted(c, k1)
				f.w.SetCampaignSemanticPolicy(map[string]policy.SemanticUtility{
					"streamera": {SemanticClass: 1}, "streamerb": {SemanticClass: 2}, "streamerc": {SemanticClass: 0},
				}, nil, nil)
				f.seedWeights(t, time.Now(), map[string]float64{
					"streamera": 5, "streamerb": 10, "streamerc": 50, "streamerd": 80,
				})
			},
			want:       []string{"streamera", "streamerb"},
			wantReason: "watched: admitted beside another channel-restricted drop, displacing streamerc",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newResidenceFixture(t, 4)
			byLogin := streamersByLogin(f.w.streamers)
			tc.setup(t, f, byLogin)

			for tick := range 2 {
				f.w.processWatching(tickCtx(f.w))
				got := restrictedSlotLogins(t, f)
				if !sameLoginSet(got, tc.want...) {
					t.Fatalf("tick %d: slots=%v, want %v", tick, got, tc.want)
				}
				reason := decisionReason(f.w, "streamerb")
				if !strings.Contains(reason, tc.wantReason) || strings.Contains(reason, "returns when the boost ends") {
					t.Fatalf("tick %d: streamerb reason=%q, want %q", tick, reason, tc.wantReason)
				}
				for _, login := range got {
					if reason := decisionReason(f.w, login); !strings.HasPrefix(reason, "watched") {
						t.Fatalf("tick %d: seated %s reason=%q, want a watched reason", tick, login, reason)
					}
				}
			}
		})
	}
}

// R9 add: a qualifying base member that the single boost displaced and whose
// restricted work one seated channel fully carries is held back by owner rule
// PA-B1a, so it is told which seated channel already farms its campaign rather
// than keeping the boost's reason.
func TestRestrictedBoostVictimHeldBackByCarrierIsToldItsCarrier(t *testing.T) {
	f := newResidenceFixture(t, 4)
	byLogin := streamersByLogin(f.w.streamers)
	a, b, c := byLogin["streamera"], byLogin["streamerb"], byLogin["streamerc"]
	c1 := restrictedTestCampaign("camp-c1", b, c)
	assignRestricted(a, restrictedTestCampaign("camp-streamera", a))
	assignRestricted(b, c1)
	assignRestricted(c, c1, restrictedTestCampaign("camp-c2", c))
	f.w.SetCampaignSemanticPolicy(map[string]policy.SemanticUtility{
		"streamera": {SemanticClass: 2}, "streamerb": {SemanticClass: 2}, "streamerc": {SemanticClass: 0},
	}, nil, nil)
	f.seedWeights(t, time.Now(), map[string]float64{
		"streamera": 5, "streamerb": 10, "streamerc": 70, "streamerd": 90,
	})

	f.w.processWatching(tickCtx(f.w))

	if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamera", "streamerc") {
		t.Fatalf("slots=%v, want the boosted streamerc beside streamera", got)
	}
	reason := decisionReason(f.w, "streamerb")
	if !strings.Contains(reason, "waiting: streamerc "+alreadyFarmedReason) || strings.Contains(reason, "DROPS/STREAK boost") {
		t.Fatalf("streamerb reason=%q, want it told streamerc already farms its campaign", reason)
	}
}

// R9 when every online channel is avoided: the avoid exclusion is lifted and
// each online channel is first noted that its "avoid" preference was ignored;
// a qualifying channel left waiting gets the admission's waiting reason in its
// place.
func TestRestrictedWaitingReasonWhenEveryChannelAvoided(t *testing.T) {
	cases := []struct {
		name    string
		size    int
		assign  func(byLogin map[string]*models.Streamer)
		seeds   map[string]float64
		want    []string
		waiting string
		reason  string
	}{
		{
			name: "carried by a seated channel",
			size: 4,
			assign: func(byLogin map[string]*models.Streamer) {
				c, d := byLogin["streamerc"], byLogin["streamerd"]
				shared := restrictedTestCampaign("camp-shared", c, d)
				assignRestricted(c, shared)
				assignRestricted(d, shared)
			},
			seeds:   map[string]float64{"streamera": 10, "streamerb": 20, "streamerc": 80, "streamerd": 90},
			want:    []string{"streamera", "streamerc"},
			waiting: "streamerd",
			reason:  "waiting: streamerc " + alreadyFarmedReason,
		},
		{
			name: "both seats held by distinct drops",
			size: 5,
			assign: func(byLogin map[string]*models.Streamer) {
				for _, login := range []string{"streamerc", "streamerd", "streamere"} {
					makeDropCandidate(byLogin[login], true)
				}
			},
			seeds:   map[string]float64{"streamera": 5, "streamerb": 10, "streamerc": 70, "streamerd": 80, "streamere": 90},
			want:    []string{"streamerc", "streamerd"},
			waiting: "streamere",
			reason:  waitingForRestrictedSeats,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newResidenceFixture(t, tc.size)
			byLogin := streamersByLogin(f.w.streamers)
			tc.assign(byLogin)
			for _, s := range byLogin {
				s.Settings.Preference = models.PreferenceAvoid
			}
			f.seedWeights(t, time.Now(), tc.seeds)

			f.w.processWatching(tickCtx(f.w))

			if got := restrictedSlotLogins(t, f); !sameLoginSet(got, tc.want...) {
				t.Fatalf("slots=%v, want %v", got, tc.want)
			}
			if reason := decisionReason(f.w, tc.waiting); !strings.Contains(reason, tc.reason) {
				t.Fatalf("%s reason=%q, want %q", tc.waiting, reason, tc.reason)
			}
		})
	}
}

// R14(a) with a retained-UNKNOWN carrier on the waiting path: streamera keeps
// its fair seat while UNKNOWN and still carries streamerd's campaign, so
// streamerd brings nothing new and waits; its reason reports streamera as
// unconfirmed, not as farming.
func TestRestrictedWaitingBesideUnknownCarrier(t *testing.T) {
	f := newResidenceFixture(t, 4)
	byLogin := streamersByLogin(f.w.streamers)
	a, c, d := byLogin["streamera"], byLogin["streamerc"], byLogin["streamerd"]
	shared := restrictedTestCampaign("camp-shared", a, c)
	ownA := restrictedTestCampaign("camp-streamera", a, d)
	assignRestricted(a, shared, ownA)
	f.seedWeights(t, time.Now(), map[string]float64{
		"streamera": 5, "streamerb": 10, "streamerc": 50, "streamerd": 80,
	})

	f.w.processWatching(tickCtx(f.w))
	if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamera", "streamerb") {
		t.Fatalf("tick 1: slots=%v, want the fair pair", got)
	}

	a.SetUnknown(models.ReasonTransportError)
	assignRestricted(c, shared)
	assignRestricted(d, ownA)
	f.w.processWatching(tickCtx(f.w))

	if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamera", "streamerc") {
		t.Fatalf("tick 2: slots=%v, want streamerd held back by the UNKNOWN carrier", got)
	}
	want := "waiting: streamera, whose status is unconfirmed, holds a slot with the same channel-restricted drop campaign"
	if reason := decisionReason(f.w, "streamerd"); !strings.Contains(reason, want) {
		t.Fatalf("streamerd reason=%q, want %q", reason, want)
	}
}

// R3 with R14(b): a seated restricted channel whose work the other seat does
// not carry is not a duplicate, so a waiting channel that carries its work and
// more does not take its seat; the waiting channel is told both seats hold
// channel-restricted drops.
func TestRestrictedSupersetWaitsBesideNonDuplicateOccupants(t *testing.T) {
	f := newResidenceFixture(t, 5)
	byLogin := streamersByLogin(f.w.streamers)
	c, d, e := byLogin["streamerc"], byLogin["streamerd"], byLogin["streamere"]
	k1 := restrictedTestCampaign("camp-k1", c, e)
	assignRestricted(c, k1)
	assignRestricted(d, restrictedTestCampaign("camp-k2", d))
	assignRestricted(e, k1, restrictedTestCampaign("camp-k3", e))
	f.seedWeights(t, time.Now(), map[string]float64{
		"streamera": 5, "streamerb": 10, "streamerc": 50, "streamerd": 60, "streamere": 90,
	})

	for tick := range 2 {
		f.w.processWatching(tickCtx(f.w))
		if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamerc", "streamerd") {
			t.Fatalf("tick %d: slots=%v, want [streamerc streamerd]; streamerc is not a duplicate", tick, got)
		}
		if reason := decisionReason(f.w, "streamere"); !strings.Contains(reason, waitingForRestrictedSeats) {
			t.Fatalf("tick %d: streamere reason=%q, want %q", tick, reason, waitingForRestrictedSeats)
		}
	}
}

// Restricted occupant definition: a channel whose drop claiming is switched off
// keeps a stale restricted assignment until the drops tracker clears it, but
// HasChannelRestrictedCampaign is false for it, so its seat is ordinary and a
// second qualifying channel takes it. Its reason is the ordinary seat's, also
// when a seated channel farms the stale campaign.
func TestRestrictedStaleAssignmentWithoutClaimDropsIsOrdinary(t *testing.T) {
	cases := []struct {
		name   string
		assign func(byLogin map[string]*models.Streamer)
	}{
		{name: "stale campaign of its own", assign: func(byLogin map[string]*models.Streamer) {
			a := byLogin["streamera"]
			assignRestricted(a, restrictedTestCampaign("camp-streamera", a))
			makeDropCandidate(byLogin["streamerc"], true)
		}},
		{name: "stale campaign a seated channel farms", assign: func(byLogin map[string]*models.Streamer) {
			a, c := byLogin["streamera"], byLogin["streamerc"]
			shared := restrictedTestCampaign("camp-shared", a, c)
			assignRestricted(a, shared)
			assignRestricted(c, shared)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newResidenceFixture(t, 4)
			byLogin := streamersByLogin(f.w.streamers)
			tc.assign(byLogin)
			byLogin["streamera"].Settings.ClaimDrops = false
			makeDropCandidate(byLogin["streamerd"], true)
			f.seedWeights(t, time.Now(), map[string]float64{
				"streamera": 5, "streamerb": 10, "streamerc": 50, "streamerd": 80,
			})

			f.w.processWatching(tickCtx(f.w))

			if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamerc", "streamerd") {
				t.Fatalf("slots=%v, want streamera's stale assignment to protect nothing", got)
			}
			reason := decisionReason(f.w, "streamera")
			if !strings.Contains(reason, "displaced by channel-restricted drop streamerd (channel-restricted drops may hold both slots") ||
				strings.Contains(reason, "already farms") {
				t.Fatalf("streamera reason=%q, want it displaced as an ordinary seat", reason)
			}
		})
	}
}

// P5 through the admission, R14(c): from the P2 seats {streamerc, streamere},
// when streamerc stops qualifying, streamerd of the same shared campaign
// re-enters at the next evaluation and the admission seats streamere beside it
// again, so both campaigns stay farmed.
func TestRestrictedSharedCampaignContinuesThroughAdmission(t *testing.T) {
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
				t.Fatalf("tick 1: slots=%v, want the P2 seats", got)
			}

			tc.stop(c)
			f.w.processWatching(tickCtx(f.w))
			if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamerd", "streamere") {
				t.Fatalf("tick 2: slots=%v, want streamerd continuing the shared campaign beside streamere", got)
			}
		})
	}
}

// R9, unit level: when both seats carry a waiting channel's work, the reason
// names the carrier confirmed online, whichever seat it holds and whether the
// other carrier is retained while UNKNOWN or confirmed offline after the
// online list was read.
func TestRestrictedWaitingReasonNamesOnlineCarrier(t *testing.T) {
	statuses := map[string]func(s *models.Streamer){
		"UNKNOWN": func(s *models.Streamer) { s.SetUnknown(models.ReasonTransportError) },
		"offline": func(s *models.Streamer) { s.SetConfirmedOffline() },
	}
	for statusName, setStatus := range statuses {
		for _, onlineFirst := range []bool{false, true} {
			name := statusName + " carrier first"
			if onlineFirst {
				name = statusName + " carrier second"
			}
			t.Run(name, func(t *testing.T) {
				f := newResidenceFixture(t, 4)
				byLogin := streamersByLogin(f.w.streamers)
				a, b, c := byLogin["streamera"], byLogin["streamerb"], byLogin["streamerc"]
				shared := restrictedTestCampaign("camp-shared", a, b, c)
				assignRestricted(a, shared)
				assignRestricted(b, shared, restrictedTestCampaign("camp-streamerb", b))
				assignRestricted(c, shared)
				setStatus(a)
				f.w.rotation.deficitMinutes = map[string]float64{
					"streamera": 10, "streamerb": 20, "streamerc": 70, "streamerd": 80,
				}
				online := []int{0, 1, 2, 3}
				pair := [2]int{restrictedIndex(t, f.w, "streamera"), restrictedIndex(t, f.w, "streamerb")}
				if onlineFirst {
					pair = [2]int{pair[1], pair[0]}
				}

				got := f.w.admitRestrictedDrops(pair, pair, online, time.Now())

				if got != pair {
					t.Fatalf("pair=%v, want %v: streamerc brings nothing new", got, pair)
				}
				if reason := f.w.selectionReasons[restrictedIndex(t, f.w, "streamerc")]; !strings.Contains(reason, "waiting: streamerb "+alreadyFarmedReason) {
					t.Fatalf("streamerc reason=%q, want it to name the online carrier streamerb", reason)
				}
			})
		}
	}
}

// R9, unit level: the both-seats waiting reason marks a seated channel that
// is not confirmed online, so a seat confirmed offline after the online list
// was read never reads as a channel farming its drop.
func TestRestrictedBothSeatsReasonMarksOfflineOccupant(t *testing.T) {
	f := newResidenceFixture(t, 5)
	byLogin := streamersByLogin(f.w.streamers)
	for _, login := range []string{"streamerc", "streamerd", "streamere"} {
		makeDropCandidate(byLogin[login], true)
	}
	byLogin["streamerc"].SetConfirmedOffline()
	f.w.rotation.deficitMinutes = map[string]float64{
		"streamera": 10, "streamerb": 20, "streamerc": 70, "streamerd": 75, "streamere": 80,
	}
	online := []int{0, 1, 2, 3, 4}
	c, a := restrictedIndex(t, f.w, "streamerc"), restrictedIndex(t, f.w, "streamera")

	got := f.w.admitRestrictedDrops([2]int{c, a}, [2]int{c, a}, online, time.Now())

	if want := [2]int{c, restrictedIndex(t, f.w, "streamerd")}; got != want {
		t.Fatalf("pair=%v, want %v", got, want)
	}
	want := waitingForRestrictedSeats + " (streamerc now confirmed offline, streamerd)"
	if reason := f.w.selectionReasons[restrictedIndex(t, f.w, "streamere")]; !strings.Contains(reason, want) {
		t.Fatalf("streamere reason=%q, want %q", reason, want)
	}
}

// R9, unit level: a carrier confirmed offline after the online list was read
// still holds its seat in this evaluation and still carries the shared
// campaign, but the waiting channel's reason reports it as offline — neither
// as farming nor as unconfirmed.
func TestRestrictedOfflineCarrierReportedAsOffline(t *testing.T) {
	f := newResidenceFixture(t, 5)
	byLogin := streamersByLogin(f.w.streamers)
	c, d, e := byLogin["streamerc"], byLogin["streamerd"], byLogin["streamere"]
	shared := restrictedTestCampaign("camp-shared", c, d)
	assignRestricted(c, shared)
	assignRestricted(d, shared)
	assignRestricted(e, restrictedTestCampaign("camp-streamere", e))
	c.SetConfirmedOffline()
	f.w.rotation.deficitMinutes = map[string]float64{
		"streamera": 10, "streamerb": 20, "streamerc": 70, "streamerd": 75, "streamere": 80,
	}
	online := []int{0, 1, 2, 3, 4}
	ci, a := restrictedIndex(t, f.w, "streamerc"), restrictedIndex(t, f.w, "streamera")

	got := f.w.admitRestrictedDrops([2]int{ci, a}, [2]int{ci, a}, online, time.Now())

	if want := [2]int{ci, restrictedIndex(t, f.w, "streamere")}; got != want {
		t.Fatalf("pair=%v, want %v", got, want)
	}
	reason := f.w.selectionReasons[restrictedIndex(t, f.w, "streamerd")]
	want := "waiting: streamerc, now confirmed offline, still holds a slot with the same channel-restricted drop campaign"
	if !strings.Contains(reason, want) {
		t.Fatalf("streamerd reason=%q, want %q", reason, want)
	}
}

// R9, unit level: the restricted work of a qualifying channel is read again
// after the qualifying check, so a concurrent assignment change can leave it
// empty, or leave an ordinary seat beside a channel no seat carries. No
// deterministic pipeline produces either, so the waiting reason is driven
// directly: with no carrier and an ordinary seat, or with the channel's work
// read empty, the reason says its assignment changed instead of blaming the
// seats; with both seats restricted and real work it keeps the both-seats
// reason. None of these is a carrier reason.
func TestRestrictedWaitingReasonWhenAssignmentChangedMidEvaluation(t *testing.T) {
	f := newResidenceFixture(t, 5)
	byLogin := streamersByLogin(f.w.streamers)
	for _, login := range []string{"streamerc", "streamerd", "streamere"} {
		makeDropCandidate(byLogin[login], true)
	}
	a, c := restrictedIndex(t, f.w, "streamera"), restrictedIndex(t, f.w, "streamerc")
	d, e := restrictedIndex(t, f.w, "streamerd"), restrictedIndex(t, f.w, "streamere")
	readEmpty := func(empty int) func(int) map[string]bool {
		return func(idx int) map[string]bool {
			if idx == empty {
				return nil
			}
			return restrictedWork(f.w.streamers[idx])
		}
	}
	const changed = "waiting: a channel-restricted assignment changed while this evaluation read it"

	cases := []struct {
		name   string
		pair   [2]int
		workOf func(int) map[string]bool
		want   string
	}{
		{name: "no carrier beside an ordinary first seat", pair: [2]int{a, c}, workOf: readEmpty(-1), want: changed},
		{name: "no carrier beside an ordinary second seat", pair: [2]int{c, a}, workOf: readEmpty(-1), want: changed},
		{name: "work read empty beside an ordinary seat", pair: [2]int{a, c}, workOf: readEmpty(d), want: changed},
		{name: "work read empty beside restricted seats", pair: [2]int{c, e}, workOf: readEmpty(d), want: changed},
		{name: "both seats restricted, distinct work", pair: [2]int{c, e}, workOf: readEmpty(-1), want: waitingForRestrictedSeats},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, carried := f.w.restrictedWaitingReason(tc.pair, d, tc.workOf)
			if !strings.HasPrefix(reason, tc.want) || carried {
				t.Fatalf("reason=%q carried=%v, want prefix %q and no carrier", reason, carried, tc.want)
			}
		})
	}
}

// Property, unit level: over random assignments of restricted campaigns,
// random statuses (confirmed online, retained UNKNOWN, confirmed offline),
// stale assignments with drop claiming switched off, game-wide campaigns,
// random starting pairs and, in some cases, a fair base pair that the single
// boost changed in one seat (its victim noted with the boost's reason), the
// admission keeps its invariants: the two seats stay
// distinct and come from the starting pair or the qualifying channels; every
// displaced restricted occupant's campaigns are still carried by a final seat;
// no seat the admission could still give up has a waiting channel that brings
// new work against the other seat; a seat whose partner never changed gave up
// only an ordinary channel or a duplicate of that partner (R3) and went to a
// channel bringing work the partner does not carry (R14(a)); every waiting
// qualifying channel gets a reason; no reason reports a seated channel that is
// not confirmed online as farming, or one confirmed offline as unconfirmed; a
// seated channel's reason is a watched one; no reason names an unseated channel
// as holding a seat; a displaced restricted occupant names its admitter and
// its carrier while a displaced ordinary seat gets the ordinary reason; a
// carrier reason names a carrier confirmed online when one exists, and the
// both-seats reason names exactly the final seats and marks one that is not
// confirmed online; and
// the boost's victim gets the right reason: an ordinary
// one is told which channel holds its seat once the admission gave it away and
// otherwise keeps the boost's reason, and a qualifying one gets its waiting
// reason when its seat was given away or one seated channel carries all of its
// restricted work, and otherwise keeps the boost's reason.
func TestRestrictedAdmissionInvariantsHoldForRandomAssignments(t *testing.T) {
	f := newResidenceFixture(t, 6)
	w := f.w
	ids := []string{"camp-k1", "camp-k2", "camp-k3"}
	rng := rand.New(rand.NewSource(20261010))
	online := []int{0, 1, 2, 3, 4, 5}

	for iteration := range 1000 {
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
			switch rng.Intn(8) {
			case 0, 1:
				s.SetUnknown(models.ReasonTransportError)
			case 2:
				s.SetConfirmedOffline()
			default:
				s.SetConfirmedOnline()
			}
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
			if rng.Intn(4) == 0 {
				campaigns = append(campaigns, watchSlotTestCampaign("camp-gamewide", "", false))
			}
			if len(campaigns) == 0 {
				s.Settings.ClaimDrops = false
				s.Stream.SetCampaignIDs(nil)
				s.Stream.SetCampaigns(nil)
				continue
			}
			assignRestricted(s, campaigns...)
			if rng.Intn(6) == 0 {
				s.Settings.ClaimDrops = false
			}
		}
		w.rotation.deficitMinutes = map[string]float64{}
		for _, s := range w.streamers {
			w.rotation.deficitMinutes[s.GetUsername()] = float64(rng.Intn(100))
		}
		w.selectionReasons = make(map[int]string)
		first := rng.Intn(len(w.streamers))
		second := (first + 1 + rng.Intn(len(w.streamers)-1)) % len(w.streamers)
		start := [2]int{first, second}
		// base is the fair pair before the single boost: in some cases the
		// boost took one of its seats from a channel that is now off the pair.
		base := start
		if rng.Intn(3) == 0 {
			for {
				if victim := rng.Intn(len(w.streamers)); victim != start[0] && victim != start[1] {
					base[rng.Intn(2)] = victim
					w.selectionReasons[victim] = boostVictimReason
					break
				}
			}
		}

		got := w.admitRestrictedDrops(base, start, online, time.Now())

		// occupied is the restricted work a seated channel protects or carries:
		// that of a restricted occupant, and nothing for any other channel.
		occupied := func(idx int) map[string]bool {
			if !w.streamers[idx].HasChannelRestrictedCampaign() {
				return nil
			}
			return restrictedWork(w.streamers[idx])
		}
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
			if out == got[0] || out == got[1] || len(occupied(out)) == 0 {
				continue
			}
			if !restrictedWorkSubset(occupied(out), occupied(got[0])) && !restrictedWorkSubset(occupied(out), occupied(got[1])) {
				t.Fatalf("iteration %d: displaced %d lost restricted work %v (final %v)", iteration, out, occupied(out), got)
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
			duplicate := len(occupied(seat)) > 0 && len(occupied(other)) > 0 && restrictedWorkSubset(occupied(seat), occupied(other))
			if len(occupied(seat)) > 0 && !duplicate {
				continue
			}
			for idx := range qualifying {
				if idx == got[0] || idx == got[1] {
					continue
				}
				if len(occupied(other)) == 0 || !restrictedWorkSubset(occupied(idx), occupied(other)) {
					t.Fatalf("iteration %d: seat %d of %v could still go to %d, which brings new work", iteration, seat, got, idx)
				}
			}
		}
		for i := range got {
			if got[i] == start[i] || got[1-i] != start[1-i] {
				continue
			}
			partner := occupied(start[1-i])
			if len(occupied(start[i])) > 0 && (len(partner) == 0 || !restrictedWorkSubset(occupied(start[i]), partner)) {
				t.Fatalf("iteration %d: non-duplicate restricted occupant %d displaced (start %v got %v)", iteration, start[i], start, got)
			}
			if len(partner) > 0 && restrictedWorkSubset(occupied(got[i]), partner) {
				t.Fatalf("iteration %d: %d admitted beside %d without new work (start %v got %v)", iteration, got[i], start[1-i], start, got)
			}
		}
		for idx := range qualifying {
			if idx != got[0] && idx != got[1] && w.selectionReasons[idx] == "" {
				t.Fatalf("iteration %d: waiting qualifying channel %d has no reason", iteration, idx)
			}
		}
		for idx, reason := range w.selectionReasons {
			if idx == got[0] || idx == got[1] {
				continue
			}
			for _, seat := range got {
				s := w.streamers[seat]
				login := s.GetUsername()
				if s.GetStatus() != models.StatusOnline && strings.Contains(reason, login+" already farms") {
					t.Fatalf("iteration %d: reason %q reports %s, not confirmed online, as farming", iteration, reason, login)
				}
				if s.GetStatus() == models.StatusOffline && strings.Contains(reason, login+", whose status is unconfirmed") {
					t.Fatalf("iteration %d: reason %q reports offline %s as unconfirmed", iteration, reason, login)
				}
			}
		}
		for _, seat := range got {
			if reason := w.selectionReasons[seat]; reason != "" && !strings.HasPrefix(reason, "watched") {
				t.Fatalf("iteration %d: seated %d has reason %q", iteration, seat, reason)
			}
		}
		for idx, s := range w.streamers {
			if idx == got[0] || idx == got[1] {
				continue
			}
			login := s.GetUsername()
			for _, holder := range []string{
				login + " already farms", login + ", whose status is unconfirmed, holds", login + ", now confirmed offline, still holds",
				"channel-restricted drop " + login + ";", "channel-restricted drop " + login + " (",
			} {
				for _, reason := range w.selectionReasons {
					if strings.Contains(reason, holder) {
						t.Fatalf("iteration %d: reason %q names unseated %s as holding a seat", iteration, reason, login)
					}
				}
			}
		}
		for i, out := range start {
			if out == got[0] || out == got[1] {
				continue
			}
			reason := w.selectionReasons[out]
			prefix := "displaced by channel-restricted drop " + w.streamers[got[i]].GetUsername()
			if w.streamers[out].HasChannelRestrictedCampaign() {
				if !strings.Contains(reason, prefix+"; ") || !strings.Contains(reason, "(owner rule PA-B1a)") {
					t.Fatalf("iteration %d: displaced restricted occupant %d reason=%q, want its admitter and carrier", iteration, out, reason)
				}
				continue
			}
			if !strings.Contains(reason, prefix+" (channel-restricted drops may hold both slots") {
				t.Fatalf("iteration %d: displaced ordinary seat %d reason=%q, want the ordinary displacement reason", iteration, out, reason)
			}
		}
		for idx, reason := range w.selectionReasons {
			if idx == got[0] || idx == got[1] {
				continue
			}
			if strings.Contains(reason, "(owner rule PA-B1a)") && len(occupied(idx)) > 0 {
				var onlineCarriers []string
				for _, seat := range got {
					if held := occupied(seat); w.streamers[seat].GetStatus() == models.StatusOnline && len(held) > 0 && restrictedWorkSubset(occupied(idx), held) {
						onlineCarriers = append(onlineCarriers, w.streamers[seat].GetUsername())
					}
				}
				named := len(onlineCarriers) == 0
				for _, login := range onlineCarriers {
					named = named || strings.Contains(reason, login+" already farms")
				}
				if !named {
					t.Fatalf("iteration %d: reason %q of %d does not name an online carrier %v", iteration, reason, idx, onlineCarriers)
				}
			}
			if strings.Contains(reason, "both watch slots are held by channel-restricted drops (") {
				label := func(seat int) string {
					switch st := w.streamers[seat]; st.GetStatus() {
					case models.StatusOnline:
						return st.GetUsername()
					case models.StatusOffline:
						return st.GetUsername() + " now confirmed offline"
					default:
						return st.GetUsername() + " status unconfirmed"
					}
				}
				if list := "(" + label(got[0]) + ", " + label(got[1]) + ")"; !strings.Contains(reason, list) {
					t.Fatalf("iteration %d: reason %q does not name the final seats %s", iteration, reason, list)
				}
				for _, seat := range got {
					s := w.streamers[seat]
					switch s.GetStatus() {
					case models.StatusOffline:
						if !strings.Contains(reason, s.GetUsername()+" now confirmed offline") {
							t.Fatalf("iteration %d: reason %q does not mark offline seat %d", iteration, reason, seat)
						}
					case models.StatusUnknown:
						if !strings.Contains(reason, s.GetUsername()+" status unconfirmed") {
							t.Fatalf("iteration %d: reason %q does not mark unconfirmed seat %d", iteration, reason, seat)
						}
					}
				}
			}
		}
		for i := range got {
			victim := base[i]
			if victim == start[i] || victim == got[0] || victim == got[1] {
				continue
			}
			reason := w.selectionReasons[victim]
			gaveAway := got[i] != start[i]
			carried := false
			for _, seat := range got {
				if held := occupied(seat); len(held) > 0 && len(occupied(victim)) > 0 && restrictedWorkSubset(occupied(victim), held) {
					carried = true
				}
			}
			switch {
			case !qualifying[victim] && gaveAway:
				if !strings.Contains(reason, "went to channel-restricted drop "+w.streamers[got[i]].GetUsername()+" (") {
					t.Fatalf("iteration %d: boost victim %d reason=%q, want it told %d holds its seat", iteration, victim, reason, got[i])
				}
			case gaveAway:
				if !strings.HasPrefix(reason, "waiting: ") || strings.Contains(reason, "DROPS/STREAK boost") {
					t.Fatalf("iteration %d: qualifying boost victim %d reason=%q, want its waiting reason", iteration, victim, reason)
				}
			case qualifying[victim] && carried:
				if !strings.HasPrefix(reason, "waiting: ") || !strings.Contains(reason, "(owner rule PA-B1a)") {
					t.Fatalf("iteration %d: carried boost victim %d reason=%q, want its carrier", iteration, victim, reason)
				}
			default:
				if reason != boostVictimReason {
					t.Fatalf("iteration %d: boost victim %d reason=%q, want the boost's reason kept", iteration, victim, reason)
				}
			}
		}
	}
}
