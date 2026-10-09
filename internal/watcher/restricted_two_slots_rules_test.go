package watcher

import (
	"strings"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/policy"
)

// These cases pin the exact limits of owner rule PA-B1 (configured channels
// holding channel-restricted Drops may occupy both watch slots) through the
// real processWatching pipeline, reading the published BrokerSnapshot, the
// debug decisions and the rotation bookkeeping as the oracle. The fixtures are
// the ones restricted_two_slots_test.go uses: four or five configured channels
// on the loop fakes with a real SQLite watch-time store, where streamera and
// streamerb are the least-watched ordinary channels unless a case says
// otherwise.

// restrictedIndex returns the configured index of login.
func restrictedIndex(t *testing.T, w *MinuteWatcher, login string) int {
	t.Helper()
	for i, s := range w.streamers {
		if s.GetUsername() == login {
			return i
		}
	}
	t.Fatalf("no configured streamer %q", login)
	return -1
}

// takeTickSends drains the minute-watched reports sent since the last call
// without closing the channel, so a test can count sends per tick.
func takeTickSends(f *residenceFixture) []string {
	var sent []string
	for {
		select {
		case login := <-f.sender.sent:
			sent = append(sent, login)
		default:
			return sent
		}
	}
}

// assertTickSends fails when one tick reported more than the slot cap or the
// same channel twice.
func assertTickSends(t *testing.T, f *residenceFixture) {
	t.Helper()
	sent := takeTickSends(f)
	if len(sent) > constants.MaxSimultaneousStreams {
		t.Fatalf("one tick sent %d minute-watched reports, cap is %d: %v", len(sent), constants.MaxSimultaneousStreams, sent)
	}
	seen := make(map[string]bool, len(sent))
	for _, login := range sent {
		if seen[login] {
			t.Fatalf("channel %s was sent minute-watched twice in one tick: %v", login, sent)
		}
		seen[login] = true
	}
}

// decisionReason returns the published selection reason for login.
func decisionReason(w *MinuteWatcher, login string) string {
	for _, d := range w.GetDebugState().Decisions {
		if d.Username == login {
			return d.Reason
		}
	}
	return ""
}

// endDropWork leaves the channel's assigned campaign without real remaining
// work, either by claiming its only drop or by completing its minutes.
func endDropWork(s *models.Streamer, claimed bool) {
	campaign := watchSlotTestCampaign("camp-"+s.GetUsername(), s.ChannelID, true)
	if claimed {
		campaign.Drops[0].IsClaimed = true
	} else {
		campaign.Drops[0].CurrentMinutesWatched = campaign.Drops[0].MinutesRequired
	}
	s.Stream.SetCampaigns([]*models.Campaign{campaign})
}

// T1(a), R4: a watch streak in progress in a fair seat yields to two
// channel-restricted drops; it does not protect its seat.
func TestRestrictedPairDisplacesPursuingStreakSeat(t *testing.T) {
	f := newResidenceFixture(t, 4)
	byLogin := streamersByLogin(f.w.streamers)
	admissionPursuingStreak(byLogin["streamera"])
	makeDropCandidate(byLogin["streamerc"], true)
	makeDropCandidate(byLogin["streamerd"], true)
	f.seedWeights(t, time.Now(), map[string]float64{
		"streamera": 10, "streamerb": 20, "streamerc": 80, "streamerd": 90,
	})
	if !f.w.streakInProgress(restrictedIndex(t, f.w, "streamera")) {
		t.Fatal("precondition: streamera must be pursuing its watch streak")
	}

	f.w.processWatching(tickCtx(f.w))

	if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamerc", "streamerd") {
		t.Fatalf("slots=%v, want the two channel-restricted drops over the pursuing streak", got)
	}
}

// T1(b), R6/R7: a plain streak latched in the boost seat gives way while two
// channel-restricted drops qualify, and competes for the boost seat again as
// soon as their work ends.
func TestRestrictedPairReleasesToLatchedPlainStreak(t *testing.T) {
	f := newResidenceFixture(t, 5)
	byLogin := streamersByLogin(f.w.streamers)
	admissionPursuingStreak(byLogin["streamere"])
	f.seedWeights(t, time.Now(), map[string]float64{
		"streamera": 10, "streamerb": 20, "streamerc": 80, "streamerd": 90, "streamere": 95,
	})
	streak := restrictedIndex(t, f.w, "streamere")

	f.w.processWatching(tickCtx(f.w))
	if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamera", "streamere") {
		t.Fatalf("tick 1: slots=%v, want the latched streak beside streamera", got)
	}
	if !f.w.rotation.boostLatched || f.w.rotation.boostTarget != streak {
		t.Fatalf("tick 1: boost latch=%v target=%d, want the streak %d latched",
			f.w.rotation.boostLatched, f.w.rotation.boostTarget, streak)
	}

	makeDropCandidate(byLogin["streamerc"], true)
	makeDropCandidate(byLogin["streamerd"], true)
	f.w.processWatching(tickCtx(f.w))
	if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamerc", "streamerd") {
		t.Fatalf("tick 2: slots=%v, want both channel-restricted drops", got)
	}

	endDropWork(byLogin["streamerc"], true)
	endDropWork(byLogin["streamerd"], true)
	f.w.processWatching(tickCtx(f.w))
	got := restrictedSlotLogins(t, f)
	if !sameLoginSet(got, "streamera", "streamere") {
		t.Fatalf("tick 3: slots=%v, want the streak back in the boost seat beside streamera", got)
	}
	if !f.w.rotation.boostLatched || f.w.rotation.boostTarget != streak {
		t.Fatalf("tick 3: boost latch=%v target=%d, want the streak %d latched again",
			f.w.rotation.boostLatched, f.w.rotation.boostTarget, streak)
	}
}

// T1(c), R7: a channel-restricted drop that is also pursuing its watch streak
// holds the boost latch. Admitting a second channel-restricted drop beside it
// leaves the latch target and victim untouched, so when the second drop's work
// ends the allocation is exactly the single-boost result.
func TestRestrictedPairKeepsRestrictedStreakLatchBookkeeping(t *testing.T) {
	f := newResidenceFixture(t, 4)
	byLogin := streamersByLogin(f.w.streamers)
	makeDropCandidate(byLogin["streamerc"], true)
	admissionPursuingStreak(byLogin["streamerc"])
	makeDropCandidate(byLogin["streamerd"], true)
	f.seedWeights(t, time.Now(), map[string]float64{
		"streamera": 10, "streamerb": 20, "streamerc": 80, "streamerd": 90,
	})
	latched := restrictedIndex(t, f.w, "streamerc")
	boostVictim := restrictedIndex(t, f.w, "streamerb")

	f.w.processWatching(tickCtx(f.w))
	if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamerc", "streamerd") {
		t.Fatalf("tick 1: slots=%v, want both channel-restricted drops", got)
	}
	if !f.w.rotation.boostLatched || f.w.rotation.boostTarget != latched || f.w.rotation.boostVictim != boostVictim {
		t.Fatalf("tick 1: latch=%v target=%d victim=%d, want target %d and victim %d from the boost alone",
			f.w.rotation.boostLatched, f.w.rotation.boostTarget, f.w.rotation.boostVictim, latched, boostVictim)
	}

	endDropWork(byLogin["streamerd"], false)
	f.w.processWatching(tickCtx(f.w))
	if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamera", "streamerc") {
		t.Fatalf("tick 2: slots=%v, want the single-boost result [streamera streamerc]", got)
	}
	if !f.w.rotation.boostLatched || f.w.rotation.boostTarget != latched || f.w.rotation.boostVictim != boostVictim {
		t.Fatalf("tick 2: latch=%v target=%d victim=%d, want target %d and victim %d kept",
			f.w.rotation.boostLatched, f.w.rotation.boostTarget, f.w.rotation.boostVictim, latched, boostVictim)
	}
}

// T2(a), R1/R3 (control, same result without PA-B1): a channel-restricted
// channel retained in its fair seat through an UNKNOWN blip is a restricted
// occupant. Two channel-restricted drops off the pair then get only the other
// seat, which goes to the stronger one.
func TestRestrictedPairKeepsRetainedUnknownOccupant(t *testing.T) {
	f := newResidenceFixture(t, 5)
	byLogin := streamersByLogin(f.w.streamers)
	makeDropCandidate(byLogin["streamera"], true)
	f.seedWeights(t, time.Now(), map[string]float64{
		"streamera": 5, "streamerb": 10, "streamerc": 50, "streamerd": 80, "streamere": 90,
	})

	f.w.processWatching(tickCtx(f.w))
	if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamera", "streamerb") {
		t.Fatalf("tick 1: slots=%v, want the fair pair [streamera streamerb]", got)
	}

	byLogin["streamera"].SetUnknown(models.ReasonTransportError)
	makeDropCandidate(byLogin["streamerd"], true)
	makeDropCandidate(byLogin["streamere"], true)
	f.w.processWatching(tickCtx(f.w))
	if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamera", "streamerd") {
		t.Fatalf("tick 2: slots=%v, want the retained restricted occupant plus the stronger waiting drop", got)
	}
}

// T2(b), R3/R13: a channel-restricted drop holding a stronger seat that goes
// UNKNOWN is released at the next evaluation, exactly as the single boost
// already releases it, and an UNKNOWN channel never gains a new seat.
func TestRestrictedSeatGoingUnknownIsReleasedAsBefore(t *testing.T) {
	f := newResidenceFixture(t, 4)
	byLogin := streamersByLogin(f.w.streamers)
	makeDropCandidate(byLogin["streamerc"], true)
	makeDropCandidate(byLogin["streamerd"], true)
	f.seedWeights(t, time.Now(), map[string]float64{
		"streamera": 10, "streamerb": 20, "streamerc": 80, "streamerd": 90,
	})

	f.w.processWatching(tickCtx(f.w))
	if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamerc", "streamerd") {
		t.Fatalf("tick 1: slots=%v, want both channel-restricted drops", got)
	}

	byLogin["streamerc"].SetUnknown(models.ReasonTransportError)
	for tick := 2; tick <= 3; tick++ {
		f.w.processWatching(tickCtx(f.w))
		got := restrictedSlotLogins(t, f)
		if !sameLoginSet(got, "streamera", "streamerd") {
			t.Fatalf("tick %d: slots=%v, want the UNKNOWN channel released and [streamera streamerd] seated", tick, got)
		}
	}
}

// T3, Q-channel definition: DisableWatch, watchdog-avoided and "avoid"
// restricted channels never qualify, so the single boost seat stays; only when
// every online channel is avoided is the exclusion lifted.
func TestRestrictedQualificationRespectsCandidateExclusions(t *testing.T) {
	cases := []struct {
		name    string
		exclude func(f *residenceFixture, byLogin map[string]*models.Streamer)
		want    []string // nil: streamerd is excluded and only streamerc boosts
	}{
		{
			name: "DisableWatch",
			exclude: func(_ *residenceFixture, byLogin map[string]*models.Streamer) {
				byLogin["streamerd"].Settings.DisableWatch = true
			},
		},
		{
			name: "watchdog avoided",
			exclude: func(f *residenceFixture, _ map[string]*models.Streamer) {
				f.w.SetAvoidChecker(&staticAvoid{avoided: map[string]bool{"streamerd": true}})
			},
		},
		{
			name: "preference avoid",
			exclude: func(_ *residenceFixture, byLogin map[string]*models.Streamer) {
				byLogin["streamerd"].Settings.Preference = models.PreferenceAvoid
			},
		},
		{
			name: "every online channel avoided lifts the exclusion",
			exclude: func(_ *residenceFixture, byLogin map[string]*models.Streamer) {
				for _, s := range byLogin {
					s.Settings.Preference = models.PreferenceAvoid
				}
			},
			want: []string{"streamerc", "streamerd"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newResidenceFixture(t, 4)
			byLogin := streamersByLogin(f.w.streamers)
			makeDropCandidate(byLogin["streamerc"], true)
			makeDropCandidate(byLogin["streamerd"], true)
			f.seedWeights(t, time.Now(), map[string]float64{
				"streamera": 10, "streamerb": 20, "streamerc": 80, "streamerd": 90,
			})
			tc.exclude(f, byLogin)

			f.w.processWatching(tickCtx(f.w))
			got := restrictedSlotLogins(t, f)
			if tc.want != nil {
				if !sameLoginSet(got, tc.want...) {
					t.Fatalf("slots=%v, want %v", got, tc.want)
				}
				return
			}
			if sameLoginSet(got, "streamerc", "streamerd") || !sameLoginSet(got, "streamera", "streamerc") {
				t.Fatalf("slots=%v, want the excluded streamerd unseated and the single boost [streamera streamerc]", got)
			}
		})
	}
}

// T4, R6: when one channel-restricted drop's work ends its seat returns to the
// existing selection at the next evaluation. While both drops held the seats
// the ordinary cohort was invalidated (C=0); the returning ordinary seat
// re-anchors a fresh cohort instead of inheriting stale residence.
func TestRestrictedSeatEndingWorkReanchorsOrdinaryCohort(t *testing.T) {
	for _, claimed := range []bool{true, false} {
		name := "no remaining minutes"
		if claimed {
			name = "drop claimed"
		}
		t.Run(name, func(t *testing.T) {
			f := newResidenceFixture(t, 4)
			byLogin := streamersByLogin(f.w.streamers)
			makeDropCandidate(byLogin["streamerc"], true)
			makeDropCandidate(byLogin["streamerd"], true)
			f.seedWeights(t, time.Now(), map[string]float64{
				"streamera": 10, "streamerb": 20, "streamerc": 80, "streamerd": 90,
			})

			f.w.processWatching(tickCtx(f.w))
			if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamerc", "streamerd") {
				t.Fatalf("tick 1: slots=%v, want both channel-restricted drops", got)
			}
			if !f.w.rotation.cohortSince.IsZero() || f.w.rotation.committedCohort != nil || f.w.rotation.cohortCapacity != 0 {
				t.Fatalf("tick 1: cohort=%v since=%v capacity=%d, want the ordinary cohort invalidated at C=0",
					f.w.rotation.committedCohort, f.w.rotation.cohortSince, f.w.rotation.cohortCapacity)
			}

			endDropWork(byLogin["streamerd"], claimed)
			beforeTick2 := time.Now()
			f.w.processWatching(tickCtx(f.w))
			if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamera", "streamerc") {
				t.Fatalf("tick 2: slots=%v, want streamerd released and the single boost [streamera streamerc]", got)
			}
			cohort := f.w.rotation.committedCohort
			if len(cohort) != 1 || f.w.rotation.cohortCapacity != 1 {
				t.Fatalf("tick 2: cohort=%v capacity=%d, want exactly the one returning ordinary seat at C=1",
					cohort, f.w.rotation.cohortCapacity)
			}
			if _, ok := cohort["streamera"]; !ok {
				t.Fatalf("tick 2: cohort=%v, want streamera as the returning ordinary seat", cohort)
			}
			if f.w.rotation.cohortSince.Before(beforeTick2) {
				t.Fatalf("tick 2: cohort anchor %v predates the evaluation at %v: stale residence",
					f.w.rotation.cohortSince, beforeTick2)
			}
		})
	}
}

// T5, R8/R9: with exactly two channel-restricted drops, six unchanged ticks keep
// the same seats, both publish restricted_drop, both bank persisted watch time
// through the real delivery chain, and steady ticks log no slot changes.
func TestRestrictedPairIsStableAndBanksWatchTime(t *testing.T) {
	f := newResidenceFixture(t, 4)
	byLogin := streamersByLogin(f.w.streamers)
	makeDropCandidate(byLogin["streamerc"], true)
	makeDropCandidate(byLogin["streamerd"], true)
	f.seedWeights(t, time.Now(), map[string]float64{
		"streamera": 10, "streamerb": 20, "streamerc": 80, "streamerd": 90,
	})
	before := map[string]float64{
		"streamerc": f.windowMinutes(t, "streamerc"),
		"streamerd": f.windowMinutes(t, "streamerd"),
	}
	logs := captureWatcherLogs(t)

	const ticks = 6
	var afterFirstTick int
	for tick := 0; tick < ticks; tick++ {
		f.w.processWatching(tickCtx(f.w))
		if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamerc", "streamerd") {
			t.Fatalf("tick %d: slots=%v, want the same two channel-restricted drops", tick, got)
		}
		for _, slot := range f.w.BrokerSnapshot().Slots {
			if slot.ReasonCode != ReasonRestrictedDrop {
				t.Fatalf("tick %d: %s reasonCode=%q, want %q", tick, slot.Channel, slot.ReasonCode, ReasonRestrictedDrop)
			}
		}
		assertTickSends(t, f)
		if tick == 0 {
			afterFirstTick = logs.Len()
		}
	}

	for login, was := range before {
		if got := f.windowMinutes(t, login); got <= was {
			t.Fatalf("%s banked no persisted watch time across %d held ticks (before=%v after=%v)", login, ticks, was, got)
		}
	}
	steady := logs.String()[afterFirstTick:]
	for _, change := range []string{"Watch slot assigned", "Watch slot released", "Watch slot reason changed"} {
		if strings.Contains(steady, change) {
			t.Fatalf("steady ticks logged %q:\n%s", change, steady)
		}
	}
}

// T6, R8: the seat set does not depend on the configured roster order.
func TestRestrictedPairIgnoresRosterOrder(t *testing.T) {
	orders := map[string]func([]*models.Streamer) []*models.Streamer{
		"configured": func(s []*models.Streamer) []*models.Streamer { return s },
		"reversed": func(s []*models.Streamer) []*models.Streamer {
			out := make([]*models.Streamer, 0, len(s))
			for i := len(s) - 1; i >= 0; i-- {
				out = append(out, s[i])
			}
			return out
		},
		"rotated": func(s []*models.Streamer) []*models.Streamer {
			return append(append([]*models.Streamer{}, s[2:]...), s[:2]...)
		},
	}
	for name, permute := range orders {
		t.Run(name, func(t *testing.T) {
			f := newResidenceFixture(t, 5)
			f.w.streamers = permute(f.w.streamers)
			byLogin := streamersByLogin(f.w.streamers)
			for _, login := range []string{"streamerc", "streamerd", "streamere"} {
				makeDropCandidate(byLogin[login], true)
			}
			f.seedWeights(t, time.Now(), map[string]float64{
				"streamera": 5, "streamerb": 10, "streamerc": 70, "streamerd": 80, "streamere": 90,
			})

			f.w.processWatching(tickCtx(f.w))
			if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamerc", "streamerd") {
				t.Fatalf("slots=%v, want the two most-owed channel-restricted drops [streamerc streamerd]", got)
			}
		})
	}
}

// T7, R5: Phase B is unchanged. A discovery channel-restricted drop with equal
// semantics waits instead of displacing either configured seat; a strictly
// stronger one takes exactly one seat under the existing displacement rule;
// no tick ever sends more than two reports or one channel twice.
func TestRestrictedPairAgainstDiscoveryContender(t *testing.T) {
	cases := []struct {
		name           string
		discoveryClass policy.SemanticClass
		displaces      bool
	}{
		{name: "equal semantics waits", discoveryClass: 1},
		{name: "strictly stronger takes one seat", discoveryClass: 0, displaces: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newResidenceFixture(t, 4)
			byLogin := streamersByLogin(f.w.streamers)
			makeDropCandidate(byLogin["streamerc"], true)
			makeDropCandidate(byLogin["streamerd"], true)
			f.seedWeights(t, time.Now(), map[string]float64{
				"streamera": 10, "streamerb": 20, "streamerc": 80, "streamerd": 90,
			})
			f.w.SetCampaignSemanticPolicy(map[string]policy.SemanticUtility{
				"streamerc": {SemanticClass: 1},
				"streamerd": {SemanticClass: 1},
			}, nil, nil)
			f.w.AddSource(&staticSource{name: OriginDiscovery, cand: []Candidate{
				{Streamer: discoveryStreamer("disco", true), Origin: OriginDiscovery},
			}})
			f.w.SetDiscoveryCandidatePolicy("disco", CandidateCampaignPolicy{
				Utility:    policy.SemanticUtility{SemanticClass: tc.discoveryClass},
				Ranked:     true,
				Restricted: true,
			})

			for tick := 0; tick < 2; tick++ {
				f.w.processWatching(tickCtx(f.w))
				assertTickSends(t, f)
				snap := f.w.BrokerSnapshot()
				got := restrictedSlotLogins(t, f)
				if !tc.displaces {
					if !sameLoginSet(got, "streamerc", "streamerd") {
						t.Fatalf("tick %d: slots=%v, want the configured pair kept", tick, got)
					}
					waits := false
					for _, c := range snap.Waiting {
						waits = waits || (c.Channel == "disco" && c.ReasonCode == ReasonLowerPriority)
					}
					if !waits {
						t.Fatalf("tick %d: waiting=%+v, want disco waiting as lower priority", tick, snap.Waiting)
					}
					continue
				}
				configured := 0
				for _, login := range got {
					if login == "streamerc" || login == "streamerd" {
						configured++
					}
				}
				if !brokerHasChannel(snap, "disco") || configured != 1 {
					t.Fatalf("tick %d: slots=%v, want disco plus exactly one configured restricted drop", tick, got)
				}
			}
		})
	}
}

// T8, R2: five channels, the two least-watched ordinary ones hold the fair pair
// and three channel-restricted drops wait off-pair. The two strongest by
// Campaign Policy utility are seated; with equal utility the two most owed by
// persisted deficit are.
func TestRestrictedAdmissionPicksStrongestWaiting(t *testing.T) {
	cases := []struct {
		name    string
		classes map[string]policy.SemanticClass
		want    []string
	}{
		{
			name:    "different utility",
			classes: map[string]policy.SemanticClass{"streamerc": 2, "streamerd": 0, "streamere": 1},
			want:    []string{"streamerd", "streamere"},
		},
		{
			name:    "equal utility",
			classes: map[string]policy.SemanticClass{"streamerc": 1, "streamerd": 1, "streamere": 1},
			want:    []string{"streamerc", "streamere"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newResidenceFixture(t, 5)
			byLogin := streamersByLogin(f.w.streamers)
			utilities := make(map[string]policy.SemanticUtility, len(tc.classes))
			for login, class := range tc.classes {
				makeDropCandidate(byLogin[login], true)
				utilities[login] = policy.SemanticUtility{SemanticClass: class}
			}
			f.w.SetCampaignSemanticPolicy(utilities, nil, nil)
			f.seedWeights(t, time.Now(), map[string]float64{
				"streamera": 5, "streamerb": 10, "streamerc": 70, "streamerd": 90, "streamere": 80,
			})

			f.w.processWatching(tickCtx(f.w))
			if got := restrictedSlotLogins(t, f); !sameLoginSet(got, tc.want...) {
				t.Fatalf("slots=%v, want %v", got, tc.want)
			}
		})
	}
}

// T8b, R2/R3: a channel-restricted drop that persisted fairness seated keeps
// its seat even when it is the weakest qualifying channel, and the strongest
// waiting one takes the other seat. The weaker case is a control (the single
// boost already produces it); the equal case needs PA-B1.
func TestRestrictedOccupantInFairPairKeepsSeat(t *testing.T) {
	cases := []struct {
		name    string
		classes map[string]policy.SemanticClass
	}{
		{
			name:    "weaker fair-pair occupant",
			classes: map[string]policy.SemanticClass{"streamera": 2, "streamerc": 0, "streamerd": 1, "streamere": 1},
		},
		{
			name:    "equal fair-pair occupant",
			classes: map[string]policy.SemanticClass{"streamera": 1, "streamerc": 1, "streamerd": 1, "streamere": 1},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newResidenceFixture(t, 5)
			byLogin := streamersByLogin(f.w.streamers)
			utilities := make(map[string]policy.SemanticUtility, len(tc.classes))
			for login, class := range tc.classes {
				makeDropCandidate(byLogin[login], true)
				utilities[login] = policy.SemanticUtility{SemanticClass: class}
			}
			f.w.SetCampaignSemanticPolicy(utilities, nil, nil)
			f.seedWeights(t, time.Now(), map[string]float64{
				"streamerb": 5, "streamerc": 70, "streamerd": 80, "streamere": 90,
			})

			f.w.processWatching(tickCtx(f.w))
			if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamera", "streamerc") {
				t.Fatalf("slots=%v, want the fair-pair restricted occupant kept beside the strongest waiting one", got)
			}
		})
	}
}

// T9, R5 (control): direct mode with two candidates is unchanged.
func TestRestrictedPairDirectModeUnchanged(t *testing.T) {
	f := newResidenceFixture(t, 2)
	byLogin := streamersByLogin(f.w.streamers)
	makeDropCandidate(byLogin["streamera"], true)
	makeDropCandidate(byLogin["streamerb"], true)

	f.w.processWatching(tickCtx(f.w))

	if mode := f.w.GetDebugState().Mode; mode != ModeDirect {
		t.Fatalf("mode=%q, want %q", mode, ModeDirect)
	}
	if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamera", "streamerb") {
		t.Fatalf("slots=%v, want both direct-mode candidates", got)
	}
	assertTickSends(t, f)
}

// T10, R9: both seats publish restricted_drop, and the selection reasons name
// the PA-B1 admission and the channel it displaced.
func TestRestrictedPairPublishesReasons(t *testing.T) {
	f := newResidenceFixture(t, 4)
	byLogin := streamersByLogin(f.w.streamers)
	makeDropCandidate(byLogin["streamerc"], true)
	makeDropCandidate(byLogin["streamerd"], true)
	f.seedWeights(t, time.Now(), map[string]float64{
		"streamera": 10, "streamerb": 20, "streamerc": 80, "streamerd": 90,
	})

	f.w.processWatching(tickCtx(f.w))

	for _, slot := range f.w.BrokerSnapshot().Slots {
		if slot.ReasonCode != ReasonRestrictedDrop {
			t.Fatalf("%s reasonCode=%q, want %q", slot.Channel, slot.ReasonCode, ReasonRestrictedDrop)
		}
	}
	if reason := decisionReason(f.w, "streamerd"); !strings.Contains(reason, "admitted beside another channel-restricted drop") ||
		!strings.Contains(reason, "streamera") {
		t.Fatalf("streamerd reason=%q, want the PA-B1 admission naming the displaced streamera", reason)
	}
	if reason := decisionReason(f.w, "streamera"); !strings.Contains(reason, "displaced by channel-restricted drop streamerd") {
		t.Fatalf("streamera reason=%q, want it displaced by the PA-B1 admission of streamerd", reason)
	}
	if reason := decisionReason(f.w, "streamerc"); !strings.Contains(reason, "boosted into a slot - channel-restricted drop") {
		t.Fatalf("streamerc reason=%q, want the unchanged single-boost reason", reason)
	}
}
