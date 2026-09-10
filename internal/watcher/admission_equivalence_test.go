package watcher

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/policy"

	"database/sql"

	_ "modernc.org/sqlite"
)

// This file is the ADMISSION-EQUIVALENCE differential. It pins, as one table,
// what the EXISTING stronger-admission policy decides on a matrix of exact
// fixtures: which channels are admitted, under which reason class, who is left
// waiting and why, and that one channel never holds two slots.
//
// It is written to compile and pass UNCHANGED on the pre-change tree as well as
// on this one. That is the whole point: the committed-ordinary-residence work
// may change WHICH ordinary channel keeps a contested seat over time, and it may
// change nothing else. Every case below is therefore built so the admission
// answer is unique — the strong contenders and the reason classes are fully
// determined by the fixture, and no case depends on which ordinary competitor
// wins an ordinary tie-break, because that is precisely the decision D1 is
// allowed to move.
//
// TestAdmissionEquivalenceMatrix evaluates each case on the FIRST tick, where no
// ordinary cohort has been committed yet, so every residence consult site is
// inert by construction. That is a real limit and is stated here rather than
// glossed: on its own it certifies only that the change is side-effect-free on
// tick one, not that admission is unaffected once residence is live.
//
// TestAdmissionEquivalenceUnderLiveResidence closes that gap. It runs the same
// fixtures for several evaluations — long enough that the committed cohort
// exists and every consult site is active on this tree while doing nothing at
// all on the pre-change tree — and asserts the facts that must hold identically
// on BOTH: the stronger candidate is admitted on every evaluation, the cap never
// changes, and every seat keeps its reason class. It deliberately says nothing
// about WHICH ordinary channel holds the residual seat, because that is exactly
// the decision this change is allowed to move.

// admissionOutcome is the canonical, order-independent record of one
// arbitration: committed "channel:reasonCode" pairs and waiting
// "channel:reasonCode" pairs, each sorted.
type admissionOutcome struct {
	slots   []string
	waiting []string
}

func (o admissionOutcome) String() string {
	return "slots=[" + strings.Join(o.slots, " ") + "] waiting=[" + strings.Join(o.waiting, " ") + "]"
}

func captureAdmission(w *MinuteWatcher) admissionOutcome {
	snap := w.BrokerSnapshot()
	out := admissionOutcome{}
	for _, s := range snap.Slots {
		out.slots = append(out.slots, s.Channel+":"+s.ReasonCode)
	}
	for _, c := range snap.Waiting {
		out.waiting = append(out.waiting, c.Channel+":"+c.ReasonCode)
	}
	sort.Strings(out.slots)
	sort.Strings(out.waiting)
	return out
}

// newAdmissionWatcher builds n configured channels on the existing loop fakes
// with a real watch-time store. Everything is ordinary until a case says
// otherwise: no drops, no streaks.
func newAdmissionWatcher(t *testing.T, n int) (*MinuteWatcher, map[string]*models.Streamer, *WatchTimeStore) {
	t.Helper()

	sender := &countingSender{sent: make(chan string, 64)}
	w, streamers := newLoopWatcher(n, sender, &staticChecker{checked: make(chan string, 64)})
	w.priorities = []config.Priority{config.PriorityOrder}
	for _, s := range streamers {
		s.ChannelID = "ch-" + s.GetUsername()
		s.Settings.ClaimDrops = false
		s.Settings.WatchStreak = false
		s.Stream.Update("broadcast-"+s.GetUsername(), "", nil, nil, 1)
	}

	sqlDB, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "admission.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	store, err := NewWatchTimeStore(&database.DB{DB: sqlDB})
	if err != nil {
		t.Fatalf("create watch time store: %v", err)
	}
	w.store = store

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	w.ctx = ctx

	return w, streamersByLogin(streamers), store
}

func admissionSeed(t *testing.T, store *WatchTimeStore, minutes map[string]float64) {
	t.Helper()
	now := time.Now()
	for login, m := range minutes {
		if m == 0 {
			continue
		}
		if err := store.RecordMinutes(login, m, now); err != nil {
			t.Fatalf("seed %s: %v", login, err)
		}
	}
}

// admissionDrop gives a configured channel an assigned campaign with real
// remaining work: an active drop, or a channel-restricted one.
func admissionDrop(s *models.Streamer, restricted bool) {
	s.Settings.ClaimDrops = true
	campaign := &models.Campaign{
		ID:          "camp-" + s.GetUsername(),
		Name:        "camp-" + s.GetUsername(),
		ClaimStatus: models.CampaignClaimStatusInProgress,
		Drops: []*models.Drop{{
			ID:              "drop-" + s.GetUsername(),
			Name:            "Reward",
			MinutesRequired: 60,
		}},
	}
	if restricted {
		campaign.Channels = []string{s.ChannelID}
	}
	s.Stream.SetCampaignIDs([]string{campaign.ID})
	s.Stream.SetCampaigns([]*models.Campaign{campaign})
}

// admissionStreak makes a channel a FRESH pursuit-eligible watch streak: no
// banked continuous minutes, so it is the "fresh streak" side of the strict
// comparison against a plain active drop.
func admissionStreak(s *models.Streamer) {
	s.Settings.WatchStreak = true
	s.Stream.MinuteWatched = 0
}

// admissionPursuingStreak makes a channel a genuinely in-progress watch streak:
// banked continuous minutes against an exact broadcast.
func admissionPursuingStreak(s *models.Streamer) {
	s.Settings.WatchStreak = true
	s.Stream.Update("broadcast-"+s.GetUsername(), "", nil, nil, 1)
	s.Stream.MinuteWatched = 5
}

// TestAdmissionEquivalenceMatrix is the differential oracle. Every expectation
// below is the behaviour of the EXISTING admission policy; this test is run
// against both the pre-change and the post-change tree and must be identical.
func TestAdmissionEquivalenceMatrix(t *testing.T) {
	cases := []struct {
		name  string
		build func(t *testing.T, w *MinuteWatcher, by map[string]*models.Streamer, store *WatchTimeStore)
		size  int
		want  string
	}{
		{
			// One configured restricted drop off the base pair takes exactly ONE
			// seat. The other seat stays with the ordinary cohort, and the third
			// ordinary channel waits: a boost is never exclusivity.
			name: "configured single off-pair restricted boost",
			size: 4,
			build: func(t *testing.T, w *MinuteWatcher, by map[string]*models.Streamer, store *WatchTimeStore) {
				admissionDrop(by["streamerd"], true)
				admissionSeed(t, store, map[string]float64{"streamerc": 30, "streamerd": 90})
			},
			want: "slots=[streamera:fair_rotation streamerd:restricted_drop] waiting=[]",
		},
		{
			// TWO configured strong candidates still cannot exceed the cap, and
			// only ONE off-pair boost is admitted per tick.
			name: "two configured strong candidates cannot take both seats",
			size: 4,
			build: func(t *testing.T, w *MinuteWatcher, by map[string]*models.Streamer, store *WatchTimeStore) {
				admissionDrop(by["streamerc"], true)
				admissionDrop(by["streamerd"], true)
				admissionSeed(t, store, map[string]float64{"streamerc": 80, "streamerd": 90})
			},
			want: "slots=[streamera:fair_rotation streamerc:restricted_drop] waiting=[]",
		},
		{
			// A zero-minute pending streak has not begun: it carries no
			// in-progress class, so an off-pair ACTIVE drop is admitted over it
			// and the base member keeps the plain fair-rotation class.
			name: "a zero-minute pending streak does not outrank an active drop",
			size: 4,
			build: func(t *testing.T, w *MinuteWatcher, by map[string]*models.Streamer, store *WatchTimeStore) {
				admissionStreak(by["streamera"])
				admissionDrop(by["streamerd"], false)
				admissionSeed(t, store, map[string]float64{"streamerc": 30, "streamerd": 90})
			},
			want: "slots=[streamera:fair_rotation streamerd:active_drop] waiting=[]",
		},
		{
			// Same fixture with a channel-restricted off-pair drop: still
			// admitted, under the restricted class.
			name: "restricted drop is admitted over a zero-minute pending streak",
			size: 4,
			build: func(t *testing.T, w *MinuteWatcher, by map[string]*models.Streamer, store *WatchTimeStore) {
				admissionStreak(by["streamera"])
				admissionDrop(by["streamerd"], true)
				admissionSeed(t, store, map[string]float64{"streamerc": 30, "streamerd": 90})
			},
			want: "slots=[streamera:fair_rotation streamerd:restricted_drop] waiting=[]",
		},
		{
			// A genuinely PURSUING streak (banked continuous minutes on an exact
			// broadcast) carries the in-progress class and is not displaced by an
			// off-pair active drop, which does not strictly outrank it.
			name: "a pursuing streak is not displaced by an equal off-pair active drop",
			size: 4,
			build: func(t *testing.T, w *MinuteWatcher, by map[string]*models.Streamer, store *WatchTimeStore) {
				admissionPursuingStreak(by["streamera"])
				admissionDrop(by["streamerd"], false)
				admissionSeed(t, store, map[string]float64{"streamerc": 30, "streamerd": 90})
			},
			want: "slots=[streamera:streak streamerb:fair_rotation] waiting=[]",
		},
		{
			// A channel-restricted drop IS admitted alongside a pursuing streak.
			// The streak keeps its own seat — victim selection protects an
			// in-progress streak from an equal-or-weaker target — so the drop
			// displaces the plain ordinary member instead.
			name: "restricted drop is admitted while a pursuing streak keeps its seat",
			size: 4,
			build: func(t *testing.T, w *MinuteWatcher, by map[string]*models.Streamer, store *WatchTimeStore) {
				admissionPursuingStreak(by["streamera"])
				admissionDrop(by["streamerd"], true)
				admissionSeed(t, store, map[string]float64{"streamerc": 30, "streamerd": 90})
			},
			want: "slots=[streamera:streak streamerd:restricted_drop] waiting=[]",
		},
		{
			// A ordinary, B and C carrying EQUAL campaign semantics, C off-pair:
			// C cannot displace B on a semantic tie, and the pair is NOT declared
			// capacity-zero just because two members carry drop labels.
			name: "equal-semantic off-pair drop cannot displace its equal",
			size: 4,
			build: func(t *testing.T, w *MinuteWatcher, by map[string]*models.Streamer, store *WatchTimeStore) {
				admissionDrop(by["streamerb"], false)
				admissionDrop(by["streamerc"], false)
				w.SetCampaignSemanticClasses(map[string]policy.SemanticClass{
					"streamerb": 1,
					"streamerc": 1,
				})
				admissionSeed(t, store, map[string]float64{"streamerc": 30, "streamerd": 90})
			},
			want: "slots=[streamera:fair_rotation streamerb:active_drop] waiting=[]",
		},
		{
			// A discovery channel-restricted drop displaces an ordinary
			// configured occupant, and the displaced one is reported waiting.
			name: "discovery restricted drop displaces one ordinary configured seat",
			size: 4,
			build: func(t *testing.T, w *MinuteWatcher, by map[string]*models.Streamer, store *WatchTimeStore) {
				w.AddSource(&staticSource{name: OriginDiscovery, cand: []Candidate{
					{Streamer: discoveryStreamer("disco", true), Origin: OriginDiscovery},
				}})
				admissionSeed(t, store, map[string]float64{"streamerc": 30, "streamerd": 90})
			},
			want: "slots=[disco:restricted_drop streamerb:fair_rotation] waiting=[streamera:lower_priority]",
		},
		{
			// "Prefer tracked" forbids that displacement entirely: discovery may
			// fill an idle slot but never evict a configured channel.
			name: "prefer-configured blocks discovery displacement",
			size: 4,
			build: func(t *testing.T, w *MinuteWatcher, by map[string]*models.Streamer, store *WatchTimeStore) {
				w.SetPreferConfiguredOverDiscovery(true)
				w.AddSource(&staticSource{name: OriginDiscovery, cand: []Candidate{
					{Streamer: discoveryStreamer("disco", true), Origin: OriginDiscovery},
				}})
				admissionSeed(t, store, map[string]float64{"streamerc": 30, "streamerd": 90})
			},
			want: "slots=[streamera:fair_rotation streamerb:fair_rotation] waiting=[disco:lower_priority]",
		},
		{
			// Same channel, two intents: a configured channel that discovery also
			// proposes never occupies two slots.
			name: "same channel with two intents is deduplicated before capacity",
			size: 4,
			build: func(t *testing.T, w *MinuteWatcher, by map[string]*models.Streamer, store *WatchTimeStore) {
				w.AddSource(&staticSource{name: OriginDiscovery, cand: []Candidate{
					{Streamer: by["streamera"], Origin: OriginDiscovery},
				}})
				admissionSeed(t, store, map[string]float64{"streamerc": 30, "streamerd": 90})
			},
			want: "slots=[streamera:fair_rotation streamerb:fair_rotation] waiting=[]",
		},
		{
			// Direct mode, at the cap: both configured channels are watched and a
			// stronger discovery contender still takes one seat.
			name: "direct mode with an external stronger contender",
			size: 2,
			build: func(t *testing.T, w *MinuteWatcher, by map[string]*models.Streamer, store *WatchTimeStore) {
				w.AddSource(&staticSource{name: OriginDiscovery, cand: []Candidate{
					{Streamer: discoveryStreamer("disco", true), Origin: OriginDiscovery},
				}})
			},
			want: "", // filled below: the ordinary victim is the D1-mobile part
		},
		{
			// Below the cap: an idle slot is filled by discovery without any
			// displacement at all.
			name: "discovery fills an idle slot without displacing",
			size: 1,
			build: func(t *testing.T, w *MinuteWatcher, by map[string]*models.Streamer, store *WatchTimeStore) {
				w.AddSource(&staticSource{name: OriginDiscovery, cand: []Candidate{
					{Streamer: discoveryStreamer("disco", false), Origin: OriginDiscovery},
				}})
			},
			want: "slots=[disco:active_drop streamera:priority] waiting=[]",
		},
	}

	for _, tc := range cases {
		if tc.want == "" {
			continue // covered by TestAdmissionEquivalenceDirectModeCapAndClasses
		}
		t.Run(tc.name, func(t *testing.T) {
			w, by, store := newAdmissionWatcher(t, tc.size)
			tc.build(t, w, by, store)
			w.processWatching(tickCtx(w))
			if got := captureAdmission(w).String(); got != tc.want {
				t.Fatalf("admission decision changed for %q:\n got  %s\n want %s", tc.name, got, tc.want)
			}
		})
	}
}

// TestAdmissionEquivalenceDirectModeCapAndClasses covers the direct-mode case
// whose ORDINARY victim is exactly the decision the residence work is allowed to
// move. It therefore asserts only the admission facts: the cap holds, the
// stronger external contender is admitted under its own reason class, exactly
// one configured channel keeps a seat, and the other is reported waiting.
func TestAdmissionEquivalenceDirectModeCapAndClasses(t *testing.T) {
	w, _, _ := newAdmissionWatcher(t, 2)
	w.AddSource(&staticSource{name: OriginDiscovery, cand: []Candidate{
		{Streamer: discoveryStreamer("disco", true), Origin: OriginDiscovery},
	}})

	w.processWatching(tickCtx(w))
	snap := w.BrokerSnapshot()

	if len(snap.Slots) != 2 {
		t.Fatalf("cap changed: %v", brokerChannels(snap))
	}
	if !brokerHasChannel(snap, "disco") {
		t.Fatalf("the stronger external contender was not admitted: %v", brokerChannels(snap))
	}
	configured := 0
	for _, s := range snap.Slots {
		if s.Channel == "disco" {
			if s.ReasonCode != ReasonRestrictedDrop {
				t.Fatalf("discovery reason class = %q, want %q", s.ReasonCode, ReasonRestrictedDrop)
			}
			continue
		}
		configured++
		if s.ReasonCode != ReasonPriority {
			t.Fatalf("configured direct-mode reason class = %q, want %q", s.ReasonCode, ReasonPriority)
		}
	}
	if configured != 1 {
		t.Fatalf("expected exactly one surviving configured seat, got %d: %v", configured, brokerChannels(snap))
	}
	if len(snap.Waiting) != 1 || snap.Waiting[0].ReasonCode != ReasonLowerPriority {
		t.Fatalf("displaced configured channel was not reported waiting: %+v", snap.Waiting)
	}
}

// TestAdmissionEquivalenceUnderLiveResidence is the non-inert half of the
// differential: several consecutive evaluations, so the committed cohort exists
// and residence is actually consulted, asserting only the admission facts that
// must be identical on the pre-change tree and on this one.
func TestAdmissionEquivalenceUnderLiveResidence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		size     int
		build    func(t *testing.T, w *MinuteWatcher, by map[string]*models.Streamer, store *WatchTimeStore)
		admitted string
		reason   string
	}{
		{
			name: "configured off-pair restricted boost across evaluations",
			size: 4,
			build: func(t *testing.T, w *MinuteWatcher, by map[string]*models.Streamer, store *WatchTimeStore) {
				admissionDrop(by["streamerd"], true)
				admissionSeed(t, store, map[string]float64{"streamerc": 30, "streamerd": 90})
			},
			admitted: "streamerd",
			reason:   ReasonRestrictedDrop,
		},
		{
			name: "discovery restricted drop across evaluations",
			size: 4,
			build: func(t *testing.T, w *MinuteWatcher, by map[string]*models.Streamer, store *WatchTimeStore) {
				w.AddSource(&staticSource{name: OriginDiscovery, cand: []Candidate{
					{Streamer: discoveryStreamer("disco", true), Origin: OriginDiscovery},
				}})
				admissionSeed(t, store, map[string]float64{"streamerc": 30, "streamerd": 90})
			},
			admitted: "disco",
			reason:   ReasonRestrictedDrop,
		},
		{
			name: "direct mode with an external stronger contender across evaluations",
			size: 2,
			build: func(t *testing.T, w *MinuteWatcher, by map[string]*models.Streamer, store *WatchTimeStore) {
				w.AddSource(&staticSource{name: OriginDiscovery, cand: []Candidate{
					{Streamer: discoveryStreamer("disco", true), Origin: OriginDiscovery},
				}})
			},
			admitted: "disco",
			reason:   ReasonRestrictedDrop,
		},
		{
			name: "an equal-semantic off-pair drop is refused on every evaluation",
			size: 4,
			build: func(t *testing.T, w *MinuteWatcher, by map[string]*models.Streamer, store *WatchTimeStore) {
				admissionDrop(by["streamerb"], false)
				admissionDrop(by["streamerc"], false)
				w.SetCampaignSemanticClasses(map[string]policy.SemanticClass{
					"streamerb": 1,
					"streamerc": 1,
				})
				admissionSeed(t, store, map[string]float64{"streamerc": 30, "streamerd": 90})
			},
			admitted: "streamerb",
			reason:   ReasonActiveDrop,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, by, store := newAdmissionWatcher(t, tc.size)
			tc.build(t, w, by, store)

			const evaluations = 6
			for i := 0; i < evaluations; i++ {
				w.processWatching(tickCtx(w))
				snap := w.BrokerSnapshot()

				if len(snap.Slots) != 2 {
					t.Fatalf("evaluation %d: cap changed: %v", i, brokerChannels(snap))
				}
				found := false
				for _, s := range snap.Slots {
					if s.Channel != tc.admitted {
						continue
					}
					found = true
					if s.ReasonCode != tc.reason {
						t.Fatalf("evaluation %d: %s reason class = %q, want %q", i, tc.admitted, s.ReasonCode, tc.reason)
					}
				}
				if !found {
					t.Fatalf("evaluation %d: %s was not admitted: %v", i, tc.admitted, brokerChannels(snap))
				}
			}
		})
	}
}

// TestAdmissionEquivalenceStrictClassBeatsRecency pins the one admission
// predicate whose INPUT this change redefines. betterBoostCandidate breaks a tie
// between equally strong off-pair candidates on rotation recency, and recency
// now means "was actually granted" rather than "was proposed". That tie-break
// may legitimately pick differently — it is a fairness decision between equals —
// but it must never let recency override the strict class ordering above it.
func TestAdmissionEquivalenceStrictClassBeatsRecency(t *testing.T) {
	w, by, store := newAdmissionWatcher(t, 4)
	// Two off-pair contenders: streamerc carries a plain active drop, streamerd a
	// channel-restricted one, which is strictly stronger.
	admissionDrop(by["streamerc"], false)
	admissionDrop(by["streamerd"], true)
	admissionSeed(t, store, map[string]float64{"streamerc": 30, "streamerd": 90})

	// Give the strictly stronger candidate the LEAST favourable recency, so a
	// tie-break that leaked above the class ordering would pick the other one.
	w.rotation.lastWatched = make(map[int]time.Time, len(w.streamers))
	for idx, s := range w.streamers {
		switch s.GetUsername() {
		case "streamerd":
			w.rotation.lastWatched[idx] = time.Now()
		case "streamerc":
			w.rotation.lastWatched[idx] = time.Now().Add(-time.Hour)
		}
	}

	for i := 0; i < 4; i++ {
		w.processWatching(tickCtx(w))
		snap := w.BrokerSnapshot()
		if !brokerHasChannel(snap, "streamerd") {
			t.Fatalf("evaluation %d: recency displaced a strictly stronger class: %v", i, brokerChannels(snap))
		}
		if brokerHasChannel(snap, "streamerc") {
			t.Fatalf("evaluation %d: a strictly weaker off-pair drop was admitted alongside: %v", i, brokerChannels(snap))
		}
	}
}
