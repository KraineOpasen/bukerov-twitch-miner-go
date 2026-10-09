package watcher

import (
	"sort"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
)

// Owner rule PA-B1: configured channels holding channel-restricted Drops may
// occupy both watch slots, because such a campaign progresses only on that exact
// channel. These cases drive the real processWatching pipeline with the
// package's loop fakes and a real SQLite watch-time store, and read the
// published BrokerSnapshot as the oracle.

func restrictedSlotLogins(t *testing.T, f *residenceFixture) []string {
	t.Helper()
	snap := f.w.BrokerSnapshot()
	if len(snap.Slots) != constants.MaxSimultaneousStreams {
		t.Fatalf("committed %d slots, want the full cap %d: %v",
			len(snap.Slots), constants.MaxSimultaneousStreams, brokerChannels(snap))
	}
	out := make([]string, 0, len(snap.Slots))
	for _, s := range snap.Slots {
		out = append(out, s.Channel)
	}
	sort.Strings(out)
	return out
}

func sameLoginSet(got []string, want ...string) bool {
	sort.Strings(want)
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// Both restricted channels sit outside the fair base pair.
func TestRestrictedDropsOffPairTakeBothSlots(t *testing.T) {
	f := newResidenceFixture(t, 4)
	byLogin := streamersByLogin(f.w.streamers)
	makeDropCandidate(byLogin["streamerc"], true)
	makeDropCandidate(byLogin["streamerd"], true)
	f.seedWeights(t, time.Now(), map[string]float64{
		"streamera": 10, "streamerb": 20, "streamerc": 80, "streamerd": 90,
	})
	for tick := 0; tick < 3; tick++ {
		f.w.processWatching(tickCtx(f.w))
		if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamerc", "streamerd") {
			t.Fatalf("tick %d: slots=%v, want both channel-restricted channels [streamerc streamerd]", tick, got)
		}
	}
}

// One restricted channel holds a fair seat; the other restricted channel is
// off-pair while the remaining seat is ordinary.
func TestRestrictedDropInPairAdmitsSecondRestricted(t *testing.T) {
	f := newResidenceFixture(t, 4)
	byLogin := streamersByLogin(f.w.streamers)
	makeDropCandidate(byLogin["streamerc"], true)
	makeDropCandidate(byLogin["streamerd"], true)
	f.seedWeights(t, time.Now(), map[string]float64{
		"streamera": 10, "streamerb": 20, "streamerd": 90,
	})
	f.w.processWatching(tickCtx(f.w))
	if got := restrictedSlotLogins(t, f); !sameLoginSet(got, "streamerc", "streamerd") {
		t.Fatalf("slots=%v, want both channel-restricted channels [streamerc streamerd]", got)
	}
}

// Three restricted channels and one ordinary channel: both seats go to
// restricted channels and no third slot appears.
func TestThreeRestrictedDropsFillBothSlots(t *testing.T) {
	f := newResidenceFixture(t, 4)
	byLogin := streamersByLogin(f.w.streamers)
	for _, login := range []string{"streamerb", "streamerc", "streamerd"} {
		makeDropCandidate(byLogin[login], true)
	}
	f.seedWeights(t, time.Now(), map[string]float64{
		"streamerb": 50, "streamerc": 60, "streamerd": 70,
	})
	f.w.processWatching(tickCtx(f.w))
	got := restrictedSlotLogins(t, f)
	for _, login := range got {
		if login == "streamera" {
			t.Fatalf("slots=%v: an ordinary channel holds a seat while a restricted channel waits", got)
		}
	}
}

// Control: with one restricted channel the single boost seat is unchanged.
func TestOneRestrictedDropKeepsSingleBoostSeat(t *testing.T) {
	f := newResidenceFixture(t, 4)
	byLogin := streamersByLogin(f.w.streamers)
	makeDropCandidate(byLogin["streamerd"], true)
	f.seedWeights(t, time.Now(), map[string]float64{
		"streamera": 10, "streamerb": 20, "streamerc": 30, "streamerd": 90,
	})
	f.w.processWatching(tickCtx(f.w))
	got := restrictedSlotLogins(t, f)
	if !sameLoginSet(got, "streamera", "streamerd") && !sameLoginSet(got, "streamerb", "streamerd") {
		t.Fatalf("slots=%v, want the restricted channel plus one fair-pair channel", got)
	}
}

// Control: two unrestricted active drops still share one boost seat; PA-B1
// covers channel-restricted Drops only.
func TestTwoUnrestrictedDropsKeepSingleBoostSeat(t *testing.T) {
	f := newResidenceFixture(t, 4)
	byLogin := streamersByLogin(f.w.streamers)
	makeDropCandidate(byLogin["streamerc"], false)
	makeDropCandidate(byLogin["streamerd"], false)
	f.seedWeights(t, time.Now(), map[string]float64{
		"streamera": 10, "streamerb": 20, "streamerc": 80, "streamerd": 90,
	})
	f.w.processWatching(tickCtx(f.w))
	got := restrictedSlotLogins(t, f)
	drops := 0
	for _, login := range got {
		if login == "streamerc" || login == "streamerd" {
			drops++
		}
	}
	if drops != 1 {
		t.Fatalf("slots=%v: want exactly one unrestricted-drop boost seat, got %d", got, drops)
	}
}
