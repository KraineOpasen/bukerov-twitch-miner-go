package streamer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// fakeStreamerAPI satisfies twitchClient so manager load/add paths run
// without HTTP.
type fakeStreamerAPI struct{}

func (fakeStreamerAPI) GetChannelID(username string) (string, error)    { return "chan-" + username, nil }
func (fakeStreamerAPI) LoadChannelPointsContext(*models.Streamer) error { return nil }
func (fakeStreamerAPI) CheckStreamerOnline(*models.Streamer) models.StatusTransition {
	return models.StatusTransition{}
}

func TestStreakCacheRoundTripsTimeoutBoundAndUnboundFacts(t *testing.T) {
	cache, _ := newCacheAt(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	state := models.WatchStreakPersistence{
		Revision: 3,
		Timeout:  &models.WatchStreakTimeout{BroadcastID: "b1", TimedOutAt: now.Add(-time.Minute)},
		Grants: []models.WatchStreakGrantFact{
			{EventID: "unbound", Binding: models.WatchStreakGrantUnbound, AcceptedAt: now.Add(-30 * time.Second)},
			{EventID: "bound", Binding: models.WatchStreakGrantBound, BroadcastID: "b0", AcceptedAt: now.Add(-2 * time.Minute)},
		},
	}
	if !cache.Record("Alpha", state, now) {
		t.Fatal("full snapshot was not persisted")
	}
	got := cache.Load(now)["alpha"]
	if got.Revision != 3 || got.Timeout == nil || got.Timeout.BroadcastID != "b1" || len(got.Grants) != 2 {
		t.Fatalf("round-trip state=%+v", got)
	}
	if got.Grants[0].EventID != "bound" || got.Grants[1].EventID != "unbound" {
		t.Fatalf("grant ledger is not deterministic: %+v", got.Grants)
	}
}

func TestStreakCacheRejectsStaleRevision(t *testing.T) {
	cache, _ := newCacheAt(t)
	now := time.Now()
	newer := models.WatchStreakPersistence{
		Revision: 2,
		Grants: []models.WatchStreakGrantFact{{
			EventID: "newer", Binding: models.WatchStreakGrantUnbound, AcceptedAt: now,
		}},
	}
	if !cache.Record("alpha", newer, now) {
		t.Fatal("newer snapshot was not persisted")
	}
	stale := models.WatchStreakPersistence{
		Revision: 1,
		Timeout:  &models.WatchStreakTimeout{BroadcastID: "wrong", TimedOutAt: now},
	}
	if cache.Record("alpha", stale, now) {
		t.Fatal("stale revision overwrote newer ledger")
	}
	got := cache.Load(now)["alpha"]
	if got.Revision != 2 || got.Timeout != nil || len(got.Grants) != 1 || got.Grants[0].EventID != "newer" {
		t.Fatalf("stale write changed cache: %+v", got)
	}
}

func TestStreakCacheLegacyGrantMigratesExplicitlyUnbound(t *testing.T) {
	cache, path := newCacheAt(t)
	now := time.Now().UTC()
	legacy := map[string]map[string]interface{}{
		"alpha": {"broadcastId": "arrival-time-guess", "grantedAt": now},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	got := cache.Load(now)["alpha"]
	if got.Revision != 1 || len(got.Grants) != 1 {
		t.Fatalf("legacy migration=%+v, want one retained grant", got)
	}
	grant := got.Grants[0]
	if grant.Binding != models.WatchStreakGrantUnbound || grant.BroadcastID != "" || grant.EventID == "" {
		t.Fatalf("legacy grant=%+v, must be explicit GRANTED_UNBOUND", grant)
	}

	mgr := loadedManager(t, cache)
	s := mgr.Get("alpha")
	s.Stream.Update("arrival-time-guess", "t", nil, nil, 1)
	decision := s.Stream.EvaluateWatchStreak(now)
	if !decision.PursuitEligible || decision.State != models.WatchStreakEligible {
		t.Fatalf("legacy guessed BroadcastID silently bound current pursuit: %+v", decision)
	}
}

func newCacheAt(t *testing.T) (*StreakCache, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "streak_cache.json")
	return NewStreakCache(path), path
}

func boundPersistence(eventID, broadcastID string, at time.Time, revision uint64) models.WatchStreakPersistence {
	return models.WatchStreakPersistence{
		Revision: revision,
		Grants: []models.WatchStreakGrantFact{{
			EventID: eventID, Binding: models.WatchStreakGrantBound,
			BroadcastID: broadcastID, AcceptedAt: at,
		}},
	}
}

// loadedManager builds a manager over the given cache and loads one streamer
// ("alpha") through the REAL LoadFromConfig path.
func loadedManager(t *testing.T, cache *StreakCache) *Manager {
	t.Helper()
	mgr := NewManager(fakeStreamerAPI{}, models.DefaultStreamerSettings())
	mgr.SetStreakCache(cache)
	if err := mgr.LoadFromConfig([]config.StreamerConfig{{Username: "alpha"}}, nil); err != nil {
		t.Fatalf("LoadFromConfig: %v", err)
	}
	return mgr
}

// T4: restart mid-broadcast with a persisted grant, driven through the REAL
// sequence — streamer created by LoadFromConfig (hydration), then the
// CheckStreamerOnline order: Stream.Update populates the broadcast ID BEFORE
// SetOnline. The already-granted broadcast must not be pursued.
func TestRestartHydrationBlocksSameBroadcast(t *testing.T) {
	cache, _ := newCacheAt(t)
	cache.Record("alpha", boundPersistence("grant-live", "bid-live", time.Now().Add(-30*time.Minute), 1), time.Now())

	mgr := loadedManager(t, cache)
	s := mgr.Get("alpha")

	// Mirror api.CheckStreamerOnline: UpdateStream -> Stream.Update -> SetOnline.
	s.Stream.Update("bid-live", "t", &models.Game{ID: "g"}, nil, 5)
	s.SetConfirmedOnline()

	if s.Stream.StreakPending() {
		t.Fatal("restart + persisted grant on the still-live broadcast must not re-pursue")
	}

	// Control: the same sequence WITHOUT a cache pursues normally.
	bare := NewManager(fakeStreamerAPI{}, models.DefaultStreamerSettings())
	if err := bare.LoadFromConfig([]config.StreamerConfig{{Username: "alpha"}}, nil); err != nil {
		t.Fatal(err)
	}
	b := bare.Get("alpha")
	b.Stream.Update("bid-live", "t", &models.Game{ID: "g"}, nil, 5)
	b.SetConfirmedOnline()
	if !b.Stream.StreakPending() {
		t.Fatal("control: without a cache the pursuit must start (historical behavior)")
	}
}

// T4b: before the first Update the broadcast is unidentified — with a fresh
// hydrated grant the pursuit is DEFERRED, then resolved by the first Update:
// blocked on the granted broadcast, released on a new one.
func TestRestartHydrationDefersUntilIdentified(t *testing.T) {
	cache, _ := newCacheAt(t)
	cache.Record("alpha", boundPersistence("grant-old", "bid-old", time.Now().Add(-time.Hour), 1), time.Now())

	mgr := loadedManager(t, cache)
	s := mgr.Get("alpha")
	s.SetConfirmedOnline() // online before any stream-info fetch (id still empty)

	if s.Stream.StreakPending() {
		t.Fatal("unidentified broadcast + fresh grant: pursuit must be deferred, not started blind")
	}

	s.Stream.Update("bid-old", "t", nil, nil, 1)
	if s.Stream.StreakPending() {
		t.Fatal("identified as the granted broadcast: must stay blocked")
	}
	s.Stream.Update("bid-new", "t", nil, nil, 1)
	if !s.Stream.StreakPending() {
		t.Fatal("identified as a NEW broadcast: pursuit must start")
	}
}

// T5: no cache file at all -> exact historical behavior.
func TestRestartWithoutCachePursues(t *testing.T) {
	cache, _ := newCacheAt(t) // file never written
	mgr := loadedManager(t, cache)
	s := mgr.Get("alpha")
	s.Stream.Update("bid-live", "t", nil, nil, 1)
	s.SetConfirmedOnline()
	if !s.Stream.StreakPending() {
		t.Fatal("empty cache must degrade to the historical pursue-on-restart behavior")
	}
}

// Terminal facts and exact replay identities cannot expire by process-local
// wall-clock age: only an observed new BroadcastID can re-arm pursuit.
func TestStreakCacheTerminalFactsDoNotExpire(t *testing.T) {
	cache, path := newCacheAt(t)
	now := time.Now().UTC()
	old := now.Add(-365 * 24 * time.Hour)
	state := boundPersistence("grant-ancient", "bid-ancient", old, 2)
	state.Timeout = &models.WatchStreakTimeout{BroadcastID: "bid-timeout", TimedOutAt: old}
	if !cache.Record("alpha", state, now) {
		t.Fatal("terminal snapshot was not recorded")
	}

	loaded := cache.Load(now.Add(365 * 24 * time.Hour))["alpha"]
	if loaded.Timeout == nil || loaded.Timeout.BroadcastID != "bid-timeout" || len(loaded.Grants) != 1 {
		t.Fatalf("terminal facts expired by wall-clock age: %+v", loaded)
	}

	var hydrated models.Stream
	hydrated.HydrateWatchStreak(loaded)
	hydrated.Update("bid-timeout", "t", nil, nil, 1)
	if decision := hydrated.EvaluateWatchStreak(now.Add(365 * 24 * time.Hour)); decision.State != models.WatchStreakTimedOutUnknown {
		t.Fatalf("hydrated timeout=%+v, want TIMED_OUT_UNKNOWN", decision)
	}
	if replay := hydrated.AcceptWatchStreakGrant(models.WatchStreakGrantEvent{
		EventID: "grant-ancient", AcceptedAt: now.Add(365 * 24 * time.Hour), ProvenBroadcastID: "bid-ancient",
	}); replay.Admission != models.WatchStreakGrantDuplicate {
		t.Fatalf("old exact replay admission=%s, want DUPLICATE", replay.Admission)
	}

	// AcceptedAt/TimedOutAt are local metadata, not event provenance. A
	// backward wall-clock step across restart must not erase terminal truth.
	rollback := cache.Load(old.Add(-time.Hour))["alpha"]
	if rollback.Timeout == nil || len(rollback.Grants) != 1 {
		t.Fatalf("clock rollback dropped terminal facts: %+v", rollback)
	}

	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := cache.Load(time.Now()); len(got) != 0 {
		t.Fatalf("corrupt cache must load as empty (fail-safe), got %v", got)
	}
}

func TestStreakCacheCorruptionRecoversOnRecord(t *testing.T) {
	cache, path := newCacheAt(t)

	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := cache.Load(time.Now()); len(got) != 0 {
		t.Fatalf("corrupt cache must load as empty (fail-safe), got %v", got)
	}

	// A corrupt file must also not break subsequent Records.
	cache.Record("alpha", boundPersistence("grant-live", "bid-live", time.Now(), 1), time.Now())
	got := cache.Load(time.Now())
	if len(got["alpha"].Grants) != 1 || got["alpha"].Grants[0].BroadcastID != "bid-live" {
		t.Fatalf("record after corruption must recover the cache, got %v", got)
	}
}

// Empty broadcast IDs are never persisted — they cannot be matched after a
// restart and would only add noise.
func TestStreakCacheSkipsEmptyBroadcast(t *testing.T) {
	cache, path := newCacheAt(t)
	cache.Record("alpha", models.WatchStreakPersistence{}, time.Now())
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("empty-broadcast grant must not create a cache file")
	}
}

// A timeout is a broadcast-bound terminal fact, not process-local scheduling
// state. Persisting through the manager's existing streak-cache owner must keep
// the same broadcast TIMED_OUT_UNKNOWN after restart.
func TestRestartHydrationPreservesTimedOutBroadcast(t *testing.T) {
	cache, _ := newCacheAt(t)
	mgr := loadedManager(t, cache)
	s := mgr.Get("alpha")
	s.Stream.Update("bid-live", "t", nil, nil, 1)
	s.Stream.MinuteWatched = 20
	decision := s.Stream.EvaluateWatchStreak(time.Now())
	if decision.State != models.WatchStreakTimedOutUnknown || !decision.Transitioned {
		t.Fatalf("setup decision = %+v, want first TIMED_OUT_UNKNOWN transition", decision)
	}
	if !mgr.RecordWatchStreak(s, decision.Persistence) {
		t.Fatal("timeout snapshot was not persisted")
	}

	restarted := loadedManager(t, cache)
	r := restarted.Get("alpha")
	r.Stream.Update("bid-live", "t", nil, nil, 1)
	r.SetConfirmedOnline()

	restartedDecision := r.Stream.EvaluateWatchStreak(time.Now())
	if !r.Stream.StreakPursuitTimedOut() || restartedDecision.State != models.WatchStreakTimedOutUnknown || restartedDecision.PursuitEligible {
		t.Fatalf("same broadcast after restart: timedOut=%v decision=%+v, want TIMED_OUT_UNKNOWN",
			r.Stream.StreakPursuitTimedOut(), restartedDecision)
	}
}

func TestRecordWatchStreakRejectsRetiredStreamerAfterForget(t *testing.T) {
	cache, _ := newCacheAt(t)
	mgr := loadedManager(t, cache)
	s := mgr.Get("alpha")
	state := models.WatchStreakPersistence{
		Revision: 1,
		Grants: []models.WatchStreakGrantFact{{
			EventID: "delayed", Binding: models.WatchStreakGrantUnbound,
			AcceptedAt: time.Now(),
		}},
	}

	_, removed, _, _ := mgr.ApplySettings(nil, models.DefaultStreamerSettings())
	if len(removed) != 1 || removed[0] != s {
		t.Fatalf("removed=%v, want the original streamer", removed)
	}
	mgr.ForgetStreak("alpha")

	if mgr.RecordWatchStreak(s, state) {
		t.Fatal("delayed callback persisted state for a retired streamer")
	}
	if got := cache.Load(time.Now()); len(got) != 0 {
		t.Fatalf("retired streamer cache was resurrected: %+v", got)
	}
}

func TestRecordWatchStreakRacingRemovalCannotResurrectCache(t *testing.T) {
	cache, _ := newCacheAt(t)
	mgr := loadedManager(t, cache)
	s := mgr.Get("alpha")
	state := models.WatchStreakPersistence{
		Revision: 1,
		Grants: []models.WatchStreakGrantFact{{
			EventID: "racing", Binding: models.WatchStreakGrantUnbound,
			AcceptedAt: time.Now(),
		}},
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		mgr.RecordWatchStreak(s, state)
	}()
	go func() {
		defer wg.Done()
		<-start
		mgr.ApplySettings(nil, models.DefaultStreamerSettings())
		mgr.ForgetStreak("alpha")
	}()
	close(start)
	wg.Wait()

	if got := cache.Load(time.Now()); len(got) != 0 {
		t.Fatalf("record/removal race resurrected retired cache: %+v", got)
	}
}

func TestStreamerRenamePreservesStreakStateAcrossRestart(t *testing.T) {
	cache, _ := newCacheAt(t)
	defaults := models.DefaultStreamerSettings()
	client := renameFakeClient{ids: map[string]string{"oldname": "123", "newname": "123"}}
	mgr := NewManager(client, defaults)
	mgr.SetStreakCache(cache)
	if err := mgr.LoadFromConfig([]config.StreamerConfig{{Username: "oldname"}}, nil); err != nil {
		t.Fatal(err)
	}

	s := mgr.Get("oldname")
	s.Stream.Update("bid-timeout", "t", nil, nil, 1)
	s.Stream.MinuteWatched = models.WatchStreakPursuitCapMinutes
	timedOut := s.Stream.EvaluateWatchStreak(time.Now())
	bound := s.Stream.AcceptWatchStreakGrant(models.WatchStreakGrantEvent{
		EventID: "bound-event", AcceptedAt: time.Now(), ProvenBroadcastID: "bid-granted",
	})
	unbound := s.Stream.AcceptWatchStreakGrant(models.WatchStreakGrantEvent{
		EventID: "unbound-event", AcceptedAt: time.Now(),
	})
	if timedOut.State != models.WatchStreakTimedOutUnknown ||
		bound.Admission != models.WatchStreakGrantNewBound ||
		unbound.Admission != models.WatchStreakGrantNewUnbound {
		t.Fatalf("setup timeout=%+v bound=%+v unbound=%+v", timedOut, bound, unbound)
	}
	if !mgr.RecordWatchStreak(s, unbound.Persistence) {
		t.Fatal("combined terminal snapshot was not persisted")
	}

	_, _, _, renamed := mgr.ApplySettings(
		[]config.StreamerConfig{{Username: "newname"}}, defaults)
	if len(renamed) != 1 || renamed[0].Streamer != s {
		t.Fatalf("rename result=%+v, want same streamer", renamed)
	}
	keys := cache.Load(time.Now())
	if _, exists := keys["oldname"]; exists {
		t.Fatalf("old cache key survived rename: %+v", keys)
	}
	if _, exists := keys["newname"]; !exists {
		t.Fatalf("new cache key missing after rename: %+v", keys)
	}

	restarted := NewManager(client, defaults)
	restarted.SetStreakCache(cache)
	if err := restarted.LoadFromConfig([]config.StreamerConfig{{Username: "newname"}}, nil); err != nil {
		t.Fatal(err)
	}
	r := restarted.Get("newname")
	r.Stream.Update("bid-timeout", "t", nil, nil, 1)
	if got := r.Stream.EvaluateWatchStreak(time.Now()); got.State != models.WatchStreakTimedOutUnknown {
		t.Fatalf("renamed timeout after restart=%+v", got)
	}
	r.Stream.Update("bid-granted", "t", nil, nil, 1)
	if got := r.Stream.EvaluateWatchStreak(time.Now()); got.State != models.WatchStreakGranted {
		t.Fatalf("renamed bound grant after restart=%+v", got)
	}
	if replay := r.Stream.AcceptWatchStreakGrant(models.WatchStreakGrantEvent{
		EventID: "unbound-event", AcceptedAt: time.Now(),
	}); replay.Admission != models.WatchStreakGrantDuplicate {
		t.Fatalf("renamed replay admission=%s, want DUPLICATE", replay.Admission)
	}
}

func TestStreakCacheRenameUsesLiveOwnerOverStaleDestination(t *testing.T) {
	cache, _ := newCacheAt(t)
	now := time.Now()
	if !cache.Record("oldname", boundPersistence("old", "bid-old", now, 1), now) {
		t.Fatal("old snapshot was not recorded")
	}
	if !cache.Record("newname", boundPersistence("stale-other-identity", "bid-stale", now, 99), now) {
		t.Fatal("stale destination snapshot was not recorded")
	}
	owner := boundPersistence("owner", "bid-owner", now, 2)
	if !cache.Rename("oldname", "newname", owner, now) {
		t.Fatal("cache rename failed")
	}
	states := cache.Load(now)
	if _, exists := states["oldname"]; exists {
		t.Fatalf("old key survived: %+v", states)
	}
	got := states["newname"]
	if got.Revision != 2 || len(got.Grants) != 1 || got.Grants[0].EventID != "owner" {
		t.Fatalf("live owner snapshot did not replace stale destination: %+v", got)
	}
}
