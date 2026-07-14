package watcher

import (
	"testing"
	"time"
)

// TestGetOnlineStreamersExcludesDisabled verifies DisableWatch is a hard
// exclusion from the watch-candidate set (even though the streamer stays
// online), unlike PreferenceAvoid which is only a soft exclusion.
func TestGetOnlineStreamersExcludesDisabled(t *testing.T) {
	w, _ := newTestWatcher(3)
	for _, s := range w.streamers {
		s.SetOnline()
		// Backdate so the 30s "settle" guard doesn't exclude them.
		s.OnlineAt = time.Now().Add(-time.Minute)
	}

	ds := w.streamers[1].GetSettings()
	ds.DisableWatch = true
	w.streamers[1].SetSettings(ds)

	online := w.getOnlineStreamers(nil)
	if len(online) != 2 {
		t.Fatalf("expected 2 watch candidates, got %d (%v)", len(online), online)
	}
	for _, idx := range online {
		if idx == 1 {
			t.Fatal("disabled streamer must be excluded from watch candidates")
		}
	}
}

// TestDisabledExcludedEvenWhenOnlyOnline confirms the hard nature of the
// exclusion: a disabled streamer is dropped even when it's the only one
// online (this is what distinguishes it from "avoid").
func TestDisabledExcludedEvenWhenOnlyOnline(t *testing.T) {
	w, _ := newTestWatcher(1)
	w.streamers[0].SetOnline()
	w.streamers[0].OnlineAt = time.Now().Add(-time.Minute)

	ds := w.streamers[0].GetSettings()
	ds.DisableWatch = true
	w.streamers[0].SetSettings(ds)

	if online := w.getOnlineStreamers(nil); len(online) != 0 {
		t.Fatalf("expected no watch candidates, got %v", online)
	}
}
