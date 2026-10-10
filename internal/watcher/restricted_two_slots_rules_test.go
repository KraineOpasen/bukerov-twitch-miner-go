package watcher

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/policy"
)

// These cases pin the exact limits of owner rule PA-B1 (configured channels
// holding channel-restricted Drops may occupy both watch slots) through the
// real processWatching pipeline, reading the published BrokerSnapshot, the
// debug decisions and the rotation bookkeeping as the oracle. They use the
// fixture restricted_two_slots_test.go uses — two, four or five configured
// channels on the loop fakes with a real SQLite watch-time store — where
// streamera and streamerb are the least-watched ordinary channels unless a case
// says otherwise. One unit-level case drives admitRestrictedDrops directly for
// two open ordinary seats, which the pipeline does not produce today.

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
// without closing the channel, so a test can inspect the sends of one tick.
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

// assertTickSendsMatchSlots fails unless this tick sent exactly one
// minute-watched report to every committed slot and to nothing else, which
// also bounds the tick by the slot cap and rules out a second send to one
// channel.
func assertTickSendsMatchSlots(t *testing.T, f *residenceFixture) {
	t.Helper()
	sent := takeTickSends(f)
	sort.Strings(sent)
	if slots := restrictedSlotLogins(t, f); !sameLoginSet(sent, slots...) {
		t.Fatalf("tick sent minute-watched to %v, want exactly the committed slots %v", sent, slots)
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

// waitingForRestrictedSeats is the selection reason a qualifying channel gets
// when both seats already hold channel-restricted drops.
const waitingForRestrictedSeats = "waiting: both watch slots are held by channel-restricted drops"

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
// channel-restricted drops qualify — its latch ends in the single boost, which
// hands the latch to the stronger drop before the PA-B1 admission runs — and it
// competes for the boost seat again as soon as their work ends.
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
	boostTarget := restrictedIndex(t, f.w, "streamerc")
	boostVictim := restrictedIndex(t, f.w, "streamerb")
	if !f.w.rotation.boostLatched || f.w.rotation.boostTarget != boostTarget || f.w.rotation.boostVictim != boostVictim {
		t.Fatalf("tick 2: latch=%v target=%d victim=%d, want the single boost's target %d and victim %d",
			f.w.rotation.boostLatched, f.w.rotation.boostTarget, f.w.rotation.boostVictim, boostTarget, boostVictim)
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

// T1(d), R7: a plain streak whose latch persists while persisted fairness seats
// it IN the base pair is a non-restricted seat, so a second channel-restricted
// drop takes it. The admission leaves the latch as the boost left it, and once
// the drop's work ends the latched streak is back exactly as before.
func TestRestrictedAdmissionLeavesInPairStreakLatch(t *testing.T) {
	f := newResidenceFixture(t, 4)
	byLogin := streamersByLogin(f.w.streamers)
	admissionPursuingStreak(byLogin["streamerb"])
	f.seedWeights(t, time.Now(), map[string]float64{
		"streamera": 5, "streamerb": 30, "streamerc": 80, "streamerd": 10,
	})
	streak := restrictedIndex(t, f.w, "streamerb")
	assertInPairLatch := func(tick int) {
		t.Helper()
		if !f.w.rotation.boostLatched || f.w.rotation.boostTarget != streak || f.w.rotation.boostVictim != -1 {
			t.Fatalf("tick %d: latch=%v target=%d victim=%d, want the streak %d latched in the base pair (victim -1)",
				tick, f.w.rotation.boostLatched, f.w.rotation.boostTarget, f.w.rotation.boostVictim, streak)
		}
	}

	f.w.processWatching(tickCtx(f.w))
	if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamera", "streamerb") {
		t.Fatalf("tick 1: slots=%v, want the boosted streak beside streamera", got)
	}

	// streamerd falls behind in the ranking, so fairness itself brings the
	// latched streak into the base pair.
	f.seedWeights(t, time.Now(), map[string]float64{"streamerd": 100})
	f.w.processWatching(tickCtx(f.w))
	if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamera", "streamerb") {
		t.Fatalf("tick 2: slots=%v, want the latched streak held in the base pair", got)
	}
	assertInPairLatch(2)

	makeDropCandidate(byLogin["streamera"], true)
	makeDropCandidate(byLogin["streamerc"], true)
	f.w.processWatching(tickCtx(f.w))
	if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamera", "streamerc") {
		t.Fatalf("tick 3: slots=%v, want the second channel-restricted drop in the streak's seat", got)
	}
	assertInPairLatch(3)

	endDropWork(byLogin["streamerc"], true)
	f.w.processWatching(tickCtx(f.w))
	if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamera", "streamerb") {
		t.Fatalf("tick 4: slots=%v, want the latched streak back in its base-pair seat", got)
	}
	assertInPairLatch(4)
}

// T2(a), R1/R3 (control, same seats without PA-B1): a channel-restricted
// channel retained in its fair seat through an UNKNOWN blip is a restricted
// occupant. Two channel-restricted drops off the pair then get only the other
// seat, which goes to the stronger one, and the other is told why it waits.
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
	if reason := decisionReason(f.w, "streamere"); !strings.Contains(reason, waitingForRestrictedSeats) ||
		!strings.Contains(reason, "streamera status unconfirmed") {
		t.Fatalf("streamere reason=%q, want it told both seats hold channel-restricted drops, one unconfirmed", reason)
	}
}

// T2(a'), R1/R3: the same protection for the boost seat. A latched restricted
// streak that goes UNKNOWN is retained in its boost seat (its streak keeps it
// boost-eligible but it no longer qualifies). Two channel-restricted drops then
// get only the other seat, which goes to the stronger one; the other waits and
// is told why.
func TestRestrictedPairKeepsRetainedUnknownBoostOccupant(t *testing.T) {
	f := newResidenceFixture(t, 5)
	byLogin := streamersByLogin(f.w.streamers)
	makeDropCandidate(byLogin["streamerc"], true)
	admissionPursuingStreak(byLogin["streamerc"])
	f.seedWeights(t, time.Now(), map[string]float64{
		"streamera": 10, "streamerb": 20, "streamerc": 70, "streamerd": 80, "streamere": 90,
	})

	f.w.processWatching(tickCtx(f.w))
	if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamera", "streamerc") {
		t.Fatalf("tick 1: slots=%v, want the latched restricted streak beside streamera", got)
	}

	byLogin["streamerc"].SetUnknown(models.ReasonTransportError)
	makeDropCandidate(byLogin["streamerd"], true)
	makeDropCandidate(byLogin["streamere"], true)
	f.w.processWatching(tickCtx(f.w))
	if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamerc", "streamerd") {
		t.Fatalf("tick 2: slots=%v, want the retained boost occupant plus the stronger waiting drop", got)
	}
	if reason := decisionReason(f.w, "streamere"); !strings.Contains(reason, waitingForRestrictedSeats) ||
		!strings.Contains(reason, "streamerc status unconfirmed") {
		t.Fatalf("streamere reason=%q, want it told both seats hold channel-restricted drops, one unconfirmed", reason)
	}
}

// T2(b), R3/R13: a channel-restricted drop that goes UNKNOWN while holding a
// stronger seat — the single boost's seat or the PA-B1 admission's — is
// released at the next evaluation and does not count as a qualifying channel:
// at tick 2 it can still be retained as a candidate yet is not re-seated, and
// tick 3 confirms it stays out once retention no longer applies.
func TestRestrictedSeatGoingUnknownIsReleasedAsBefore(t *testing.T) {
	cases := []struct {
		name    string
		unknown string
		want    []string
	}{
		{name: "boost seat", unknown: "streamerc", want: []string{"streamera", "streamerd"}},
		{name: "admitted seat", unknown: "streamerd", want: []string{"streamera", "streamerc"}},
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

			f.w.processWatching(tickCtx(f.w))
			if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamerc", "streamerd") {
				t.Fatalf("tick 1: slots=%v, want both channel-restricted drops", got)
			}

			byLogin[tc.unknown].SetUnknown(models.ReasonTransportError)
			for tick := 2; tick <= 3; tick++ {
				f.w.processWatching(tickCtx(f.w))
				if got := restrictedSlotLogins(t, f); !sameLoginSet(got, tc.want...) {
					t.Fatalf("tick %d: slots=%v, want %s released and %v seated", tick, got, tc.unknown, tc.want)
				}
			}
		})
	}
}

// T3, Q-channel definition: DisableWatch, watchdog-avoided and "avoid"
// restricted channels never qualify, so the single boost seat stays; only when
// every online channel is avoided is the exclusion lifted.
func TestRestrictedQualificationRespectsCandidateExclusions(t *testing.T) {
	cases := []struct {
		name      string
		exclude   func(f *residenceFixture, byLogin map[string]*models.Streamer)
		want      []string
		admitted  string
		displaced string
	}{
		{
			name: "DisableWatch",
			exclude: func(_ *residenceFixture, byLogin map[string]*models.Streamer) {
				byLogin["streamerd"].Settings.DisableWatch = true
			},
			want: []string{"streamera", "streamerc"},
		},
		{
			name: "watchdog avoided",
			exclude: func(f *residenceFixture, _ map[string]*models.Streamer) {
				f.w.SetAvoidChecker(&staticAvoid{avoided: map[string]bool{"streamerd": true}})
			},
			want: []string{"streamera", "streamerc"},
		},
		{
			name: "preference avoid",
			exclude: func(_ *residenceFixture, byLogin map[string]*models.Streamer) {
				byLogin["streamerd"].Settings.Preference = models.PreferenceAvoid
			},
			want: []string{"streamera", "streamerc"},
		},
		{
			name: "every online channel avoided lifts the exclusion",
			exclude: func(_ *residenceFixture, byLogin map[string]*models.Streamer) {
				for _, s := range byLogin {
					s.Settings.Preference = models.PreferenceAvoid
				}
			},
			want:      []string{"streamerc", "streamerd"},
			admitted:  "streamerd",
			displaced: "streamera",
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
			if got := restrictedSlotLogins(t, f); !sameLoginSet(got, tc.want...) {
				t.Fatalf("slots=%v, want %v", got, tc.want)
			}
			// The admission's reason replaces the note that the avoid
			// preference was ignored.
			if tc.admitted != "" {
				if reason := decisionReason(f.w, tc.admitted); !strings.Contains(reason, "admitted beside another channel-restricted drop, displacing") {
					t.Fatalf("%s reason=%q, want the admission's reason", tc.admitted, reason)
				}
			}
			if tc.displaced != "" {
				if reason := decisionReason(f.w, tc.displaced); !strings.Contains(reason,
					"displaced by channel-restricted drop "+tc.admitted+" (channel-restricted drops may hold both slots") {
					t.Fatalf("%s reason=%q, want the ordinary displacement reason", tc.displaced, reason)
				}
			}
		})
	}
}

// T4, R6: when one channel-restricted drop stops qualifying — its work ends,
// its assignment goes away, it goes offline or it is avoided — its seat returns
// to the existing selection at the next evaluation. While both drops held the
// seats the ordinary cohort was invalidated (C=0); the returning ordinary seat
// then starts its own cohort at C=1.
func TestRestrictedSeatEndingWorkReanchorsOrdinaryCohort(t *testing.T) {
	cases := []struct {
		name string
		stop func(s *models.Streamer)
	}{
		{name: "drop claimed", stop: func(s *models.Streamer) { endDropWork(s, true) }},
		{name: "no remaining minutes", stop: func(s *models.Streamer) { endDropWork(s, false) }},
		{name: "unassigned", stop: func(s *models.Streamer) {
			s.Stream.SetCampaignIDs(nil)
			s.Stream.SetCampaigns(nil)
		}},
		{name: "offline", stop: func(s *models.Streamer) { s.SetConfirmedOffline() }},
		{name: "avoided", stop: func(s *models.Streamer) { s.Settings.Preference = models.PreferenceAvoid }},
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

			f.w.processWatching(tickCtx(f.w))
			if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamerc", "streamerd") {
				t.Fatalf("tick 1: slots=%v, want both channel-restricted drops", got)
			}
			if !f.w.rotation.cohortSince.IsZero() || f.w.rotation.committedCohort != nil || f.w.rotation.cohortCapacity != 0 {
				t.Fatalf("tick 1: cohort=%v since=%v capacity=%d, want the ordinary cohort invalidated at C=0",
					f.w.rotation.committedCohort, f.w.rotation.cohortSince, f.w.rotation.cohortCapacity)
			}

			tc.stop(byLogin["streamerd"])
			f.w.processWatching(tickCtx(f.w))
			if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamera", "streamerc") {
				t.Fatalf("tick 2: slots=%v, want streamerd released and the single boost [streamera streamerc]", got)
			}
			cohort := f.w.rotation.committedCohort
			if _, ok := cohort["streamera"]; !ok || len(cohort) != 1 || f.w.rotation.cohortCapacity != 1 {
				t.Fatalf("tick 2: cohort=%v capacity=%d, want exactly the returning ordinary seat streamera at C=1",
					cohort, f.w.rotation.cohortCapacity)
			}
		})
	}
}

// T4b, R4/R6: a committed ordinary seat inside its minimum residence does not
// delay the admission of a second channel-restricted drop; the admission
// invalidates the cohort (C=0), and when the drop's work ends the returning
// ordinary seat is anchored afresh instead of resuming the old residence.
func TestRestrictedAdmissionOverridesResidenceAndReanchors(t *testing.T) {
	f := newResidenceFixture(t, 4)
	byLogin := streamersByLogin(f.w.streamers)
	makeDropCandidate(byLogin["streamerc"], true)
	f.seedWeights(t, time.Now(), map[string]float64{
		"streamera": 10, "streamerb": 20, "streamerc": 80, "streamerd": 90,
	})
	resident := restrictedIndex(t, f.w, "streamera")

	f.w.processWatching(tickCtx(f.w))
	if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamera", "streamerc") {
		t.Fatalf("tick 1: slots=%v, want the single boost beside streamera", got)
	}
	firstAnchor := f.w.rotation.cohortSince
	if firstAnchor.IsZero() || !f.w.residentOrdinaryIndex(resident, time.Now()) {
		t.Fatalf("tick 1: anchor=%v, want streamera resident in a committed ordinary cohort", firstAnchor)
	}

	makeDropCandidate(byLogin["streamerd"], true)
	f.w.processWatching(tickCtx(f.w))
	if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamerc", "streamerd") {
		t.Fatalf("tick 2: slots=%v, want the resident streamera displaced by the second drop at once", got)
	}
	if !f.w.rotation.cohortSince.IsZero() || f.w.rotation.committedCohort != nil {
		t.Fatalf("tick 2: cohort=%v since=%v, want the ordinary cohort invalidated at C=0",
			f.w.rotation.committedCohort, f.w.rotation.cohortSince)
	}

	endDropWork(byLogin["streamerd"], true)
	f.w.processWatching(tickCtx(f.w))
	if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamera", "streamerc") {
		t.Fatalf("tick 3: slots=%v, want streamera back beside the single boost", got)
	}
	if !f.w.rotation.cohortSince.After(firstAnchor) {
		t.Fatalf("tick 3: anchor=%v, want a fresh anchor after the invalidated %v", f.w.rotation.cohortSince, firstAnchor)
	}
}

// R5 boundary (control, identical without PA-B1): exactly one qualifying
// channel beside a non-qualifying restricted occupant changes nothing. The
// latched restricted streak goes UNKNOWN and is retained in its boost seat; the
// single new qualifying channel stays waiting.
func TestRestrictedSingleQualifyingChannelBesideRetainedStreakIsUnchanged(t *testing.T) {
	f := newResidenceFixture(t, 4)
	byLogin := streamersByLogin(f.w.streamers)
	makeDropCandidate(byLogin["streamerc"], true)
	admissionPursuingStreak(byLogin["streamerc"])
	f.seedWeights(t, time.Now(), map[string]float64{
		"streamera": 10, "streamerb": 20, "streamerc": 80, "streamerd": 90,
	})

	f.w.processWatching(tickCtx(f.w))
	if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamera", "streamerc") {
		t.Fatalf("tick 1: slots=%v, want the latched restricted streak beside streamera", got)
	}

	byLogin["streamerc"].SetUnknown(models.ReasonTransportError)
	makeDropCandidate(byLogin["streamerd"], true)
	f.w.processWatching(tickCtx(f.w))
	if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamera", "streamerc") {
		t.Fatalf("tick 2: slots=%v, want the single-boost result kept with one qualifying channel", got)
	}
}

// R4, unit level: the pipeline always lets the single boost seat one
// channel-restricted channel first, so two open ordinary seats never reach the
// admission today. Driven directly, the admission gives the strongest waiting channel the
// weakest seat in betterBoostVictim order and the next one the other seat.
// streamera is the less-owed seat (30 against 20 minutes), so persisted deficit
// alone gives it up first; when streamera is resident in the committed ordinary
// cohort, residence ranks above deficit and the non-resident streamerb goes
// first instead.
func TestRestrictedAdmissionVictimOrderAcrossTwoOpenSeats(t *testing.T) {
	cases := []struct {
		name             string
		residentA        bool
		residenceElapsed bool
		firstVictim      string
		secondVictim     string
		wantFirstSeat    string // the channel that ends in streamera's seat
		equalDeficit     bool   // both base seats equally owed
		recent           string // the base seat watched more recently, if any
	}{
		{name: "resident seat kept longer", residentA: true, firstVictim: "streamerb", secondVictim: "streamera", wantFirstSeat: "streamerd"},
		{name: "deficit alone", residentA: false, firstVictim: "streamera", secondVictim: "streamerb", wantFirstSeat: "streamerc"},
		{name: "residence elapsed", residentA: true, residenceElapsed: true, firstVictim: "streamera", secondVictim: "streamerb", wantFirstSeat: "streamerc"},
		{name: "recency at equal deficit, streamera more recent", equalDeficit: true, recent: "streamera", firstVictim: "streamera", secondVictim: "streamerb", wantFirstSeat: "streamerc"},
		{name: "recency at equal deficit, streamerb more recent", equalDeficit: true, recent: "streamerb", firstVictim: "streamerb", secondVictim: "streamera", wantFirstSeat: "streamerd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newResidenceFixture(t, 4)
			byLogin := streamersByLogin(f.w.streamers)
			makeDropCandidate(byLogin["streamerc"], true)
			makeDropCandidate(byLogin["streamerd"], true)
			now := time.Now()
			f.w.rotation.deficitMinutes = map[string]float64{
				"streamera": 30, "streamerb": 20, "streamerc": 70, "streamerd": 80,
			}
			if tc.equalDeficit {
				f.w.rotation.deficitMinutes["streamera"] = 20
			}
			if tc.recent != "" {
				older := "streamera"
				if tc.recent == older {
					older = "streamerb"
				}
				f.w.rotation.lastWatched = map[int]time.Time{
					restrictedIndex(t, f.w, tc.recent): now.Add(-time.Minute),
					restrictedIndex(t, f.w, older):     now.Add(-10 * time.Minute),
				}
			}
			if tc.residentA {
				f.w.rotation.committedCohort = map[string]string{
					"streamera": byLogin["streamera"].Stream.GetBroadcastID(),
				}
				f.w.rotation.cohortSince = now
				if tc.residenceElapsed {
					f.w.rotation.cohortSince = now.Add(-fairRotationResidence - time.Second)
				}
			}
			online := make([]int, len(f.w.streamers))
			for i := range online {
				online[i] = i
			}
			a, b := restrictedIndex(t, f.w, "streamera"), restrictedIndex(t, f.w, "streamerb")
			c, d := restrictedIndex(t, f.w, "streamerc"), restrictedIndex(t, f.w, "streamerd")

			got := f.w.admitRestrictedDrops([2]int{a, b}, [2]int{a, b}, online, now)

			wantA := restrictedIndex(t, f.w, tc.wantFirstSeat)
			want := [2]int{wantA, c + d - wantA}
			if got != want {
				t.Fatalf("pair=%v, want %v: streamerc takes %s's seat first, then streamerd takes %s's",
					got, want, tc.firstVictim, tc.secondVictim)
			}
			if reason := f.w.selectionReasons[c]; !strings.Contains(reason, "displacing "+tc.firstVictim) {
				t.Fatalf("streamerc reason=%q, want it to name the displaced %s", reason, tc.firstVictim)
			}
			if reason := f.w.selectionReasons[d]; !strings.Contains(reason, "displacing "+tc.secondVictim) {
				t.Fatalf("streamerd reason=%q, want it to name the displaced %s", reason, tc.secondVictim)
			}
		})
	}
}

// T5, R8/R9: with exactly two channel-restricted drops, six unchanged ticks keep
// the same seats, both publish restricted_drop, both bank persisted watch time
// through the real delivery chain, every tick reports exactly the two seats,
// and only the first tick logs the slot assignments.
func TestRestrictedPairIsStableAndBanksWatchTime(t *testing.T) {
	f := newResidenceFixture(t, 4)
	byLogin := streamersByLogin(f.w.streamers)
	makeDropCandidate(byLogin["streamerc"], true)
	makeDropCandidate(byLogin["streamerd"], true)
	f.seedWeights(t, time.Now(), map[string]float64{
		"streamera": 10, "streamerb": 20, "streamerc": 80, "streamerd": 90,
	})
	logs := captureWatcherLogs(t)

	const ticks = 6
	var afterFirstTick int
	banked := map[string]float64{}
	for tick := range ticks {
		f.w.processWatching(tickCtx(f.w))
		if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamerc", "streamerd") {
			t.Fatalf("tick %d: slots=%v, want the same two channel-restricted drops", tick, got)
		}
		for _, slot := range f.w.BrokerSnapshot().Slots {
			if slot.ReasonCode != ReasonRestrictedDrop {
				t.Fatalf("tick %d: %s reasonCode=%q, want %q", tick, slot.Channel, slot.ReasonCode, ReasonRestrictedDrop)
			}
		}
		assertTickSendsMatchSlots(t, f)
		// The first report anchors continuous accounting; every later held tick
		// must bank more persisted watch time for both seats.
		for _, login := range []string{"streamerc", "streamerd"} {
			got := f.windowMinutes(t, login)
			if tick > 0 && got <= banked[login] {
				t.Fatalf("tick %d: %s banked no persisted watch time (before=%v after=%v)", tick, login, banked[login], got)
			}
			banked[login] = got
		}
		if tick == 0 {
			for _, login := range []string{"streamerc", "streamerd"} {
				if logLineContaining(logs, "Watch slot assigned", "channel="+login) == "" {
					t.Fatalf("tick 0 logged no slot assignment for %s:\n%s", login, logs.String())
				}
			}
			afterFirstTick = logs.Len()
		}
	}

	steady := logs.String()[afterFirstTick:]
	for _, change := range []string{"Watch slot assigned", "Watch slot released", "Watch slot reason changed"} {
		if strings.Contains(steady, change) {
			t.Fatalf("steady ticks logged %q:\n%s", change, steady)
		}
	}
}

// T6, R8: the seat set does not depend on the configured roster order, both
// when persisted deficit separates the channels and when only the recency and
// login tie-breaks do, including when two of the channels share one restricted
// campaign (PA-B1a).
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
	distinct := func(byLogin map[string]*models.Streamer) {
		for _, login := range []string{"streamerc", "streamerd", "streamere"} {
			makeDropCandidate(byLogin[login], true)
		}
	}
	sharedCD := func(byLogin map[string]*models.Streamer) {
		c, d, e := byLogin["streamerc"], byLogin["streamerd"], byLogin["streamere"]
		shared := restrictedTestCampaign("camp-shared", c, d)
		assignRestricted(c, shared)
		assignRestricted(d, shared)
		assignRestricted(e, restrictedTestCampaign("camp-streamere", e))
	}
	separated := map[string]float64{"streamera": 5, "streamerb": 10, "streamerc": 70, "streamerd": 80, "streamere": 90}
	equal := map[string]float64{"streamera": 5, "streamerb": 10, "streamerc": 80, "streamerd": 80, "streamere": 80}
	cases := map[string]struct {
		seed   map[string]float64
		assign func(byLogin map[string]*models.Streamer)
		want   []string
	}{
		"separated deficits":                  {seed: separated, assign: distinct, want: []string{"streamerc", "streamerd"}},
		"equal deficits":                      {seed: equal, assign: distinct, want: []string{"streamerc", "streamerd"}},
		"shared campaign, separated deficits": {seed: separated, assign: sharedCD, want: []string{"streamerc", "streamere"}},
		"shared campaign, equal deficits":     {seed: equal, assign: sharedCD, want: []string{"streamerc", "streamere"}},
	}
	for caseName, tc := range cases {
		for orderName, permute := range orders {
			t.Run(caseName+"/"+orderName, func(t *testing.T) {
				f := newResidenceFixture(t, 5)
				f.w.streamers = permute(f.w.streamers)
				byLogin := streamersByLogin(f.w.streamers)
				tc.assign(byLogin)
				f.seedWeights(t, time.Now(), tc.seed)

				f.w.processWatching(tickCtx(f.w))
				if got := restrictedSlotLogins(t, f); !sameLoginSet(got, tc.want...) {
					t.Fatalf("slots=%v, want %v for every roster order", got, tc.want)
				}
			})
		}
	}
}

// T7, R5: Phase B is unchanged. A discovery channel-restricted drop with equal
// semantics waits instead of displacing either configured seat; a strictly
// stronger one takes exactly one seat under the existing displacement rule,
// which gives up the less-owed streamerd; every tick reports exactly the
// committed slots.
func TestRestrictedPairAgainstDiscoveryContender(t *testing.T) {
	cases := []struct {
		name           string
		discoveryClass policy.SemanticClass
		want           []string
		waiting        string
	}{
		{name: "equal semantics waits", discoveryClass: 1, want: []string{"streamerc", "streamerd"}, waiting: "disco"},
		{name: "strictly stronger takes one seat", discoveryClass: 0, want: []string{"disco", "streamerc"}, waiting: "streamerd"},
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

			for tick := range 2 {
				f.w.processWatching(tickCtx(f.w))
				if got := restrictedSlotLogins(t, f); !sameLoginSet(got, tc.want...) {
					t.Fatalf("tick %d: slots=%v, want %v", tick, got, tc.want)
				}
				assertTickSendsMatchSlots(t, f)
				waiting := f.w.BrokerSnapshot().Waiting
				if len(waiting) != 1 || waiting[0].Channel != tc.waiting || waiting[0].ReasonCode != ReasonLowerPriority {
					t.Fatalf("tick %d: waiting=%+v, want only %s waiting as lower priority", tick, waiting, tc.waiting)
				}
			}
		})
	}
}

// R2, recency at equal deficit, unit level: of two waiting channel-restricted
// drops that are equally owed, the one watched longer ago takes the open seat.
// Without a watch-time store every deficit ties, so this order decides the
// admission in the pipeline as well.
func TestRestrictedAdmissionOrdersEqualDeficitByRecency(t *testing.T) {
	cases := []struct {
		name   string
		recent string
		want   string
	}{
		{name: "streamerd watched more recently", recent: "streamerd", want: "streamere"},
		{name: "streamere watched more recently", recent: "streamere", want: "streamerd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newResidenceFixture(t, 5)
			byLogin := streamersByLogin(f.w.streamers)
			for _, login := range []string{"streamerc", "streamerd", "streamere"} {
				makeDropCandidate(byLogin[login], true)
			}
			now := time.Now()
			f.w.rotation.deficitMinutes = map[string]float64{
				"streamera": 10, "streamerb": 20, "streamerc": 70, "streamerd": 80, "streamere": 80,
			}
			older := "streamerd"
			if tc.recent == older {
				older = "streamere"
			}
			f.w.rotation.lastWatched = map[int]time.Time{
				restrictedIndex(t, f.w, tc.recent): now.Add(-time.Minute),
				restrictedIndex(t, f.w, older):     now.Add(-10 * time.Minute),
			}
			online := []int{0, 1, 2, 3, 4}
			a, c := restrictedIndex(t, f.w, "streamera"), restrictedIndex(t, f.w, "streamerc")

			got := f.w.admitRestrictedDrops([2]int{a, c}, [2]int{a, c}, online, now)

			if want := [2]int{restrictedIndex(t, f.w, tc.want), c}; got != want {
				t.Fatalf("pair=%v, want %v: the one watched longer ago takes streamera's seat", got, want)
			}
		})
	}
}

// R2, in-progress streak first: among waiting channel-restricted drops, two
// pursuing their watch streak are seated ahead of a more-owed one that is not.
func TestRestrictedAdmissionPrefersInProgressStreak(t *testing.T) {
	f := newResidenceFixture(t, 5)
	byLogin := streamersByLogin(f.w.streamers)
	for _, login := range []string{"streamerc", "streamerd", "streamere"} {
		makeDropCandidate(byLogin[login], true)
	}
	admissionPursuingStreak(byLogin["streamerc"])
	admissionPursuingStreak(byLogin["streamerd"])
	f.seedWeights(t, time.Now(), map[string]float64{
		"streamera": 5, "streamerb": 10, "streamerc": 60, "streamerd": 70, "streamere": 50,
	})

	f.w.processWatching(tickCtx(f.w))

	if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamerc", "streamerd") {
		t.Fatalf("slots=%v, want the two streaking channel-restricted drops", got)
	}
	if reason := decisionReason(f.w, "streamerd"); !strings.Contains(reason, "admitted beside another channel-restricted drop") {
		t.Fatalf("streamerd reason=%q, want the admission's reason", reason)
	}
}

// T8, R2/R8: five channels, the two least-watched ordinary ones hold the fair
// pair and three channel-restricted drops wait off-pair. The two strongest by
// Campaign Policy utility are seated; with equal utility the two most owed by
// persisted deficit are. The third is told that both seats hold
// channel-restricted drops, and the allocation holds over several ticks.
func TestRestrictedAdmissionPicksStrongestWaiting(t *testing.T) {
	cases := []struct {
		name    string
		classes map[string]policy.SemanticClass
		want    []string
		waiting string
	}{
		{
			name:    "different utility",
			classes: map[string]policy.SemanticClass{"streamerc": 2, "streamerd": 0, "streamere": 1},
			want:    []string{"streamerd", "streamere"},
			waiting: "streamerc",
		},
		{
			name:    "equal utility",
			classes: map[string]policy.SemanticClass{"streamerc": 1, "streamerd": 1, "streamere": 1},
			want:    []string{"streamerc", "streamere"},
			waiting: "streamerd",
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

			for tick := range 3 {
				f.w.processWatching(tickCtx(f.w))
				if got := restrictedSlotLogins(t, f); !sameLoginSet(got, tc.want...) {
					t.Fatalf("tick %d: slots=%v, want %v", tick, got, tc.want)
				}
				if reason := decisionReason(f.w, tc.waiting); !strings.Contains(reason, waitingForRestrictedSeats) {
					t.Fatalf("tick %d: %s reason=%q, want it told both seats hold channel-restricted drops",
						tick, tc.waiting, reason)
				}
			}
		})
	}
}

// T8b, R2/R3/R6: a channel-restricted drop that persisted fairness seated keeps
// its seat even when it is the weakest qualifying channel, and the strongest
// waiting one takes the other seat. The fairly seated one stays an ordinary seat
// and the admitted one a stronger seat, so the cohort is that one channel at
// C=1. The weaker case is a control (the single boost already produces it); the
// equal case needs PA-B1.
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
			cohort := f.w.rotation.committedCohort
			if _, ok := cohort["streamera"]; !ok || len(cohort) != 1 || f.w.rotation.cohortCapacity != 1 {
				t.Fatalf("cohort=%v capacity=%d, want the fairly seated streamera as the only ordinary seat at C=1",
					cohort, f.w.rotation.cohortCapacity)
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
	assertTickSendsMatchSlots(t, f)
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

	if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamerc", "streamerd") {
		t.Fatalf("slots=%v, want both channel-restricted drops", got)
	}
	for _, slot := range f.w.BrokerSnapshot().Slots {
		if slot.ReasonCode != ReasonRestrictedDrop {
			t.Fatalf("%s reasonCode=%q, want %q", slot.Channel, slot.ReasonCode, ReasonRestrictedDrop)
		}
	}
	if reason := decisionReason(f.w, "streamerd"); !strings.Contains(reason, "admitted beside another channel-restricted drop") ||
		!strings.Contains(reason, "streamera") {
		t.Fatalf("streamerd reason=%q, want the PA-B1 admission naming the displaced streamera", reason)
	}
	if reason := decisionReason(f.w, "streamera"); !strings.Contains(reason, "displaced by channel-restricted drop streamerd") ||
		!strings.Contains(reason, "(channel-restricted drops may hold both slots; it competes again when one stops qualifying)") ||
		strings.Contains(reason, "already farms") {
		t.Fatalf("streamera reason=%q, want the ordinary seat's PA-B1 displacement reason naming streamerd", reason)
	}
	if reason := decisionReason(f.w, "streamerc"); !strings.Contains(reason, "boosted into a slot - channel-restricted drop") {
		t.Fatalf("streamerc reason=%q, want the unchanged single-boost reason", reason)
	}
	// streamerc still holds the seat the boost took from streamerb, so
	// streamerb keeps the boost's own reason.
	if reason := decisionReason(f.w, "streamerb"); !strings.Contains(reason,
		"displaced by a DROPS/STREAK boost (keeps its rotation slot and returns when the boost ends)") ||
		strings.Contains(reason, "whose seat then went to") {
		t.Fatalf("streamerb reason=%q, want the single boost's displacement reason", reason)
	}
}
