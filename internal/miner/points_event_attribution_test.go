package miner

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/pubsub"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/web"
)

var pointEventTestSequence atomic.Uint64

// parsedPointsEarned builds a community-points-user-v1 points-earned message
// through the REAL parse layer (pubsub.ParsePubSubMessage), so the message
// carries the production EventFingerprint and the exact wire shape Twitch
// sends: point_gain.total_points is the event-local grant, balance.balance is
// the balance Twitch reports in that same frame. Distinct timestamps make
// every frame a distinct event identity.
func parsedPointsEarned(t *testing.T, channelID, reason string, totalPoints, balance int, ts string) *pubsub.PubSubMessage {
	t.Helper()
	raw := fmt.Sprintf(`{"type":"points-earned","data":{"timestamp":%q,"channel_id":%q,`+
		`"point_gain":{"user_id":"999","channel_id":%q,"total_points":%d,"baseline_points":%d,"reason_code":%q,"multipliers":[]},`+
		`"balance":{"user_id":"999","channel_id":%q,"balance":%d}}}`,
		ts, channelID, channelID, totalPoints, totalPoints, reason, channelID, balance)
	msg, err := pubsub.ParsePubSubMessage(&pubsub.WSData{Topic: "community-points-user-v1.999", Message: raw})
	if err != nil {
		t.Fatalf("ParsePubSubMessage: %v", err)
	}
	if msg.EventFingerprint == "" {
		t.Fatal("parsed message carries no EventFingerprint")
	}
	return msg
}

// deliverPointsEarned mirrors the observable linearization of
// WebSocketPool.handleCommunityPointsUser for one accepted points-earned
// frame: WATCH_STREAK admission first (a replay is NOT newly accepted and the
// balance is NOT applied), then the frame's wire balance is applied to the
// shared, mutable Streamer, then the miner callback runs with the admission
// outcome. This is exactly the state the miner observes in production.
func deliverPointsEarned(t *testing.T, m *Miner, s *models.Streamer, msg *pubsub.PubSubMessage) pubsub.MessageOutcome {
	t.Helper()
	var outcome pubsub.MessageOutcome
	gain, _ := msg.Data["point_gain"].(map[string]interface{})
	reason, _ := gain["reason_code"].(string)
	earned := 0
	if pts, ok := gain["total_points"].(float64); ok {
		earned = int(pts)
	}
	if reason == "WATCH_STREAK" {
		outcome.WatchStreak = s.ApplyWatchStreakGrant(models.WatchStreakGrantEvent{
			EventID:    msg.EventFingerprint,
			AcceptedAt: time.Now(),
		}, earned)
		if !outcome.WatchStreak.NewlyAccepted() {
			m.handlePubSubMessage(msg, s, outcome)
			return outcome
		}
	}
	if balance, ok := msg.Data["balance"].(map[string]interface{}); ok {
		if bal, ok := balance["balance"].(float64); ok {
			s.SetChannelPoints(int(bal))
		}
	}
	m.handlePubSubMessage(msg, s, outcome)
	return outcome
}

// newPointEventMiner builds a miner with one tracked streamer and a live
// analytics service on the shared test database, ready for handlePubSubMessage.
func newPointEventMiner(t *testing.T, prefix string) (*Miner, *models.Streamer, *analytics.Service) {
	t.Helper()
	login := fmt.Sprintf("%s-%d", prefix, pointEventTestSequence.Add(1))
	m, _, _ := newCapabilityMiner(t, login)
	s := m.streamers.Get(login)
	if s == nil {
		t.Fatal("streamer not found")
	}
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := analytics.NewService(db, t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	m.analyticsSvc = svc
	return m, s, svc
}

// pointsHistoryViaAPI starts the real dashboard server on a loopback probe
// port and fetches the Statistics points-history response for one streamer,
// exactly as the Statistics page does.
func pointsHistoryViaAPI(t *testing.T, m *Miner, svc *analytics.Service, streamer, rng string) analytics.PointsHistory {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()

	m.config.EnableAnalytics = true
	m.config.Analytics.Host = "127.0.0.1"
	m.config.Analytics.Port = port
	ws := web.NewServerEarly(m.config.Analytics, m.config.Username, m.config.StorageKey(), svc)
	if ws == nil {
		t.Fatal("web.NewServerEarly returned nil")
	}
	if err := ws.Start(); err != nil {
		t.Fatalf("web server Start: %v", err)
	}
	t.Cleanup(ws.Stop)

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, derr := net.DialTimeout("tcp", addr, time.Second)
		if derr == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("web server not reachable: %v", derr)
		}
		time.Sleep(5 * time.Millisecond)
	}

	resp, err := http.Get("http://" + addr + "/api/points-history?streamer=" + streamer + "&range=" + rng)
	if err != nil {
		t.Fatalf("GET points-history: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("points-history status = %d, want 200", resp.StatusCode)
	}
	var got analytics.PointsHistory
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode points-history: %v", err)
	}
	return got
}

func shareByReason(shares []analytics.ReasonShare, reason string) analytics.ReasonShare {
	for _, s := range shares {
		if s.Reason == reason {
			return s
		}
	}
	return analytics.ReasonShare{Reason: reason}
}

// parsedPointsEarnedRawAmount is parsedPointsEarned with a verbatim JSON
// value for point_gain.total_points (or none when totalJSON is empty), to
// drive the malformed-amount paths through the real parser.
func parsedPointsEarnedRawAmount(t *testing.T, channelID, reason, totalJSON string, balance int, ts string) *pubsub.PubSubMessage {
	t.Helper()
	total := ""
	if totalJSON != "" {
		total = `"total_points":` + totalJSON + `,`
	}
	raw := fmt.Sprintf(`{"type":"points-earned","data":{"timestamp":%q,"channel_id":%q,`+
		`"point_gain":{"user_id":"999","channel_id":%q,%s"reason_code":%q,"multipliers":[]},`+
		`"balance":{"user_id":"999","channel_id":%q,"balance":%d}}}`,
		ts, channelID, channelID, total, reason, channelID, balance)
	msg, err := pubsub.ParsePubSubMessage(&pubsub.WSData{Topic: "community-points-user-v1.999", Message: raw})
	if err != nil {
		t.Fatalf("ParsePubSubMessage: %v", err)
	}
	return msg
}

// deliverPointsSpent mirrors the pool for a points-spent frame: the wire
// balance is applied to the Streamer, then the miner callback runs.
func deliverPointsSpent(t *testing.T, m *Miner, s *models.Streamer, balance int, ts string) {
	t.Helper()
	raw := fmt.Sprintf(`{"type":"points-spent","data":{"timestamp":%q,"balance":{"user_id":"999","channel_id":%q,"balance":%d}}}`, ts, s.ChannelID, balance)
	msg, err := pubsub.ParsePubSubMessage(&pubsub.WSData{Topic: "community-points-user-v1.999", Message: raw})
	if err != nil {
		t.Fatalf("ParsePubSubMessage: %v", err)
	}
	s.SetChannelPoints(balance)
	m.handlePubSubMessage(msg, s, pubsub.MessageOutcome{})
}

// TestPointsEarnedExactReDeliveryRecordsOnce: an exact re-delivery of a
// non-streak event (a replay outside the transport window, where no domain
// admission exists) is deduplicated by the ledger's event identity — one
// earning, one sample.
func TestPointsEarnedExactReDeliveryRecordsOnce(t *testing.T) {
	m, s, svc := newPointEventMiner(t, "dup-watch")
	msg := parsedPointsEarned(t, s.ChannelID, "WATCH", 12, 1012, "2026-09-01T10:00:00.000000000Z")
	deliverPointsEarned(t, m, s, msg)
	deliverPointsEarned(t, m, s, msg)

	exact, err := svc.Repository().ExactEarningsBetween(s.GetUsername(), time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if exact.Events != 1 || len(exact.Breakdown) != 1 || exact.Breakdown[0].Gained != 12 || exact.Breakdown[0].Count != 1 {
		t.Fatalf("exact earnings after re-delivery = %+v, want one WATCH event of 12", exact)
	}
	samples, _ := svc.Repository().GetPointSamples(s.GetUsername(), time.Time{}, time.Time{}, 0)
	if len(samples) != 1 {
		t.Fatalf("samples = %+v, want one (the replay must not add a timeline row)", samples)
	}
}

// TestWatchStreakNotNewlyAcceptedWritesNoExactEvent: the pool's WATCH_STREAK
// admission stays the linearization point. A replay the admission classified
// as DUPLICATE, and a callback carrying no admission at all, write nothing to
// the ledger, the timeline, or the markers.
func TestWatchStreakNotNewlyAcceptedWritesNoExactEvent(t *testing.T) {
	m, s, svc := newPointEventMiner(t, "streak-gate")
	login := s.GetUsername()
	streak := parsedPointsEarned(t, s.ChannelID, "WATCH_STREAK", 450, 1450, "2026-09-01T10:05:00.000000000Z")

	first := deliverPointsEarned(t, m, s, streak)
	if !first.WatchStreak.NewlyAccepted() {
		t.Fatalf("first admission = %s, want newly accepted", first.WatchStreak.Admission)
	}
	replay := deliverPointsEarned(t, m, s, streak)
	if replay.WatchStreak.Admission != models.WatchStreakGrantDuplicate {
		t.Fatalf("replay admission = %s, want DUPLICATE", replay.WatchStreak.Admission)
	}
	// A callback without any admission verdict (zero outcome) must be inert too.
	s.SetChannelPoints(1900)
	m.handlePubSubMessage(streak, s, pubsub.MessageOutcome{})

	exact, _ := svc.Repository().ExactEarningsBetween(login, time.Time{}, time.Time{})
	if exact.Events != 1 || exact.Breakdown[0].Gained != 450 || exact.Breakdown[0].Count != 1 {
		t.Fatalf("exact earnings = %+v, want exactly one 450 streak", exact)
	}
	samples, _ := svc.Repository().GetPointSamples(login, time.Time{}, time.Time{}, 0)
	anns, _ := svc.Repository().GetAnnotationRecords(login, time.Time{}, time.Time{})
	if len(samples) != 1 || len(anns) != 1 {
		t.Fatalf("samples=%d annotations=%d, want 1/1 after a duplicate and an unadmitted callback", len(samples), len(anns))
	}

	// The gate itself is load-bearing, not merely shadowed by the ledger's
	// UNIQUE identity: a streak the ledger has never seen but that admission
	// did NOT newly accept (an unadmitted zero outcome, and a DUPLICATE whose
	// first delivery predates analytics) must still write nothing.
	unseen := parsedPointsEarned(t, s.ChannelID, "WATCH_STREAK", 450, 2350, "2026-09-01T10:06:00.000000000Z")
	s.SetChannelPoints(2350)
	m.handlePubSubMessage(unseen, s, pubsub.MessageOutcome{})

	preAnalytics := parsedPointsEarned(t, s.ChannelID, "WATCH_STREAK", 450, 2800, "2026-09-01T10:07:00.000000000Z")
	saved := m.analyticsSvc
	m.analyticsSvc = nil
	if first := deliverPointsEarned(t, m, s, preAnalytics); !first.WatchStreak.NewlyAccepted() {
		t.Fatalf("pre-analytics admission = %s, want newly accepted", first.WatchStreak.Admission)
	}
	m.analyticsSvc = saved
	if again := deliverPointsEarned(t, m, s, preAnalytics); again.WatchStreak.Admission != models.WatchStreakGrantDuplicate {
		t.Fatalf("post-analytics replay admission = %s, want DUPLICATE", again.WatchStreak.Admission)
	}

	exact, _ = svc.Repository().ExactEarningsBetween(login, time.Time{}, time.Time{})
	if exact.Events != 1 {
		t.Fatalf("exact earnings = %+v, want still exactly one event: the admission gate, not the ledger, must stop unadmitted streaks", exact)
	}
	samples, _ = svc.Repository().GetPointSamples(login, time.Time{}, time.Time{}, 0)
	anns, _ = svc.Repository().GetAnnotationRecords(login, time.Time{}, time.Time{})
	if len(samples) != 1 || len(anns) != 1 {
		t.Fatalf("samples=%d annotations=%d, want 1/1 after unadmitted streaks the ledger never saw", len(samples), len(anns))
	}
}

// TestPointsEarnedMalformedAmountIsNeverAnExactEarning: a points-earned frame
// whose total_points is missing, non-numeric, non-integral or beyond exact
// float range is NOT coerced to a zero earning and never enters the exact
// ledger. Its balance still lands on the timeline, where the Statistics page
// reports it as a legacy ESTIMATE — explicitly not exact.
func TestPointsEarnedMalformedAmountIsNeverAnExactEarning(t *testing.T) {
	cases := []struct {
		name  string
		total string
	}{
		{"missing", ""},
		{"string", `"450"`},
		{"non-integral", "450.5"},
		{"beyond exact range", "1e300"},
		{"at the float64 integer bound", "9007199254740992"}, // 2^53: a wire 2^53+1 has already rounded to it
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, s, svc := newPointEventMiner(t, "malformed-"+strings.ReplaceAll(tc.name, " ", "-"))
			login := s.GetUsername()
			// A legacy baseline written the pre-ledger way, then the malformed frame.
			s.SetChannelPoints(1000)
			svc.RecordPoints(s, "WATCH")
			msg := parsedPointsEarnedRawAmount(t, s.ChannelID, "WATCH_STREAK", tc.total, 1450, "2026-09-01T10:10:00.000000000Z")
			deliverPointsEarned(t, m, s, msg)

			exact, err := svc.Repository().ExactEarningsBetween(login, time.Time{}, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			if exact.Events != 0 {
				t.Fatalf("malformed amount entered the exact ledger: %+v", exact)
			}
			samples, _ := svc.Repository().GetPointSamples(login, time.Time{}, time.Time{}, 0)
			if len(samples) != 2 || samples[1].Exact || samples[1].Balance != 1450 {
				t.Fatalf("samples = %+v, want two legacy samples with the frame's balance on the timeline", samples)
			}

			got := pointsHistoryViaAPI(t, m, svc, login, "7d")
			if got.Earnings.Exact || got.Earnings.Coverage != analytics.EarningsCoverageLegacy || got.Earnings.LegacyStatus != analytics.LegacyStatusEstimated {
				t.Fatalf("earnings = %+v, want a legacy-only estimate, never exact", got.Earnings)
			}
			est := shareByReason(got.Breakdown, "WATCH_STREAK")
			if est.Gained != 450 || est.Count != 1 {
				t.Fatalf("legacy estimate = %+v, want the +450 balance delta reported as an estimate (not dropped, not zero)", got.Breakdown)
			}
		})
	}
}

// TestExactWirePointsBounds pins the exact-integer contract of the wire
// decoder: every integer below 2^53 is exact, 2^53 itself is not (a wire
// 2^53+1 has already rounded to it during JSON decoding), and nothing
// non-integral, non-finite or non-numeric is ever coerced.
func TestExactWirePointsBounds(t *testing.T) {
	// The largest exact value is 2^53-1 where int holds it and the platform
	// int bound on 32-bit builds, matching the decoder's second guard.
	largest := int(math.Min(float64(1<<53-1), float64(math.MaxInt)))
	cases := []struct {
		name string
		in   interface{}
		want int
		ok   bool
	}{
		{"typical grant", float64(450), 450, true},
		{"zero", float64(0), 0, true},
		{"negative integral", float64(-450), -450, true},
		{"largest exact", float64(largest), largest, true},
		{"float64 integer bound", float64(1 << 53), 0, false},
		{"negative bound", -float64(1 << 53), 0, false},
		{"non-integral", 450.5, 0, false},
		{"NaN", math.NaN(), 0, false},
		{"+Inf", math.Inf(1), 0, false},
		{"string", "450", 0, false},
		{"missing", nil, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := exactWirePoints(tc.in)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("exactWirePoints(%v) = (%d, %v), want (%d, %v)", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestPointsEarnedWithoutIdentityKeepsTimelineAndMarker: a frame that
// carries no event identity (theoretical in production — the parser always
// fingerprints a message) cannot be an exact ledger fact, but it still earned:
// the balance timeline sample and, for a streak, the chart marker are written
// exactly as before the ledger existed, so nothing silently disappears.
func TestPointsEarnedWithoutIdentityKeepsTimelineAndMarker(t *testing.T) {
	m, s, svc := newPointEventMiner(t, "no-identity")
	login := s.GetUsername()
	msg := parsedPointsEarned(t, s.ChannelID, "WATCH_STREAK", 450, 1450, "2026-09-01T10:20:00.000000000Z")
	msg.EventFingerprint = ""
	s.SetChannelPoints(1450)
	outcome := s.ApplyWatchStreakGrant(models.WatchStreakGrantEvent{EventID: "sha256:no-identity-grant", AcceptedAt: time.Now()}, 450)
	m.handlePubSubMessage(msg, s, pubsub.MessageOutcome{WatchStreak: outcome})

	exact, _ := svc.Repository().ExactEarningsBetween(login, time.Time{}, time.Time{})
	if exact.Events != 0 {
		t.Fatalf("identity-less frame entered the exact ledger: %+v", exact)
	}
	samples, _ := svc.Repository().GetPointSamples(login, time.Time{}, time.Time{}, 0)
	if len(samples) != 1 || samples[0].Exact || samples[0].Balance != 1450 || samples[0].Reason != "WATCH STREAK" {
		t.Fatalf("samples = %+v, want one legacy WATCH STREAK sample at 1450", samples)
	}
	anns, _ := svc.Repository().GetAnnotationRecords(login, time.Time{}, time.Time{})
	if len(anns) != 1 || anns[0].Type != "WATCH_STREAK" || anns[0].Reason != "+450 - Watch Streak" {
		t.Fatalf("annotations = %+v, want the streak marker from the frame's own amount", anns)
	}
}

// TestPointsEarnedTimestamplessFrameIsNotAnExactEvent: a payload without
// Twitch's event timestamp fingerprints identically for every equal grant —
// with or without a balance, since a balance is not monotonic across spends
// (earn to 1012, spend, earn back to 1012) — so it is not an event identity:
// two such WATCH frames both land on the timeline (never silently dropped as
// duplicates) and neither enters the exact ledger.
func TestPointsEarnedTimestamplessFrameIsNotAnExactEvent(t *testing.T) {
	cases := []struct {
		name        string
		raw         func(channelID string) string
		wantBalance int // the frame's own balance, or the streamer's when the frame has none
	}{
		{"neither timestamp nor balance", func(id string) string {
			return fmt.Sprintf(`{"type":"points-earned","data":{"channel_id":%q,"point_gain":{"total_points":12,"reason_code":"WATCH"}}}`, id)
		}, 999999},
		{"balance only, repeated after a spend", func(id string) string {
			return fmt.Sprintf(`{"type":"points-earned","data":{"channel_id":%q,"point_gain":{"total_points":12,"reason_code":"WATCH"},"balance":{"channel_id":%q,"balance":1012}}}`, id, id)
		}, 1012},
		{"timestamp present but not RFC 3339", func(id string) string {
			return fmt.Sprintf(`{"type":"points-earned","data":{"timestamp":"not-a-time","channel_id":%q,"point_gain":{"total_points":12,"reason_code":"WATCH"},"balance":{"channel_id":%q,"balance":1012}}}`, id, id)
		}, 1012},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, s, svc := newPointEventMiner(t, "timestampless-"+strings.ReplaceAll(tc.name, " ", "-"))
			login := s.GetUsername()
			raw := tc.raw(s.ChannelID)
			for i := 0; i < 2; i++ {
				msg, err := pubsub.ParsePubSubMessage(&pubsub.WSData{Topic: "community-points-user-v1.999", Message: raw})
				if err != nil {
					t.Fatal(err)
				}
				if msg.EventFingerprint == "" {
					t.Fatal("parser produced no fingerprint")
				}
				// The pool applied the frame's 1012, then a poll (or a later frame)
				// moved the shared balance before this callback: the timeline
				// sample must still carry the frame's own balance when it has one.
				s.SetChannelPoints(999999)
				m.handlePubSubMessage(msg, s, pubsub.MessageOutcome{})
			}

			exact, _ := svc.Repository().ExactEarningsBetween(login, time.Time{}, time.Time{})
			if exact.Events != 0 {
				t.Fatalf("timestamp-less frames entered the exact ledger: %+v", exact)
			}
			samples, _ := svc.Repository().GetPointSamples(login, time.Time{}, time.Time{}, 0)
			if len(samples) != 2 || samples[0].Exact || samples[1].Exact || samples[0].Balance != tc.wantBalance || samples[1].Balance != tc.wantBalance {
				t.Fatalf("samples = %+v, want two legacy timeline samples at %d (the second identical frame must not vanish, and a foreign balance must not be lent)", samples, tc.wantBalance)
			}
		})
	}
}

// TestPointsEarnedReasonCoverageUsesEventLocalAmounts: every reason Twitch
// sends — including one this code has never heard of — is an exact earning
// at its event-local amount; the balances deliberately disagree with the
// amounts so a delta-based attribution would be caught. RAID and WATCH_STREAK
// markers carry the same event-local amount.
func TestPointsEarnedReasonCoverageUsesEventLocalAmounts(t *testing.T) {
	m, s, svc := newPointEventMiner(t, "reasons")
	login := s.GetUsername()
	frames := []struct {
		reason  string
		wire    int
		balance int
	}{
		{"WATCH", 12, 1000},
		{"CLAIM", 50, 1500},
		{"RAID", 250, 1900},
		{"PREDICTION", 1000, 2000},
		{"WEEKLY_REWARDS", 7, 2500},
		{"WATCH_STREAK", 450, 2600},
		{"WATCH", 12, 2700},
	}
	for i, f := range frames {
		msg := parsedPointsEarned(t, s.ChannelID, f.reason, f.wire, f.balance, fmt.Sprintf("2026-09-01T11:%02d:00.000000000Z", i))
		deliverPointsEarned(t, m, s, msg)
	}

	got := pointsHistoryViaAPI(t, m, svc, login, "7d")
	if !got.Earnings.Exact || got.Earnings.Coverage != analytics.EarningsCoverageExact {
		t.Fatalf("earnings = %+v, want fully exact coverage", got.Earnings)
	}
	want := []analytics.ReasonShare{
		{Reason: "PREDICTION", Gained: 1000, Count: 1},
		{Reason: "WATCH_STREAK", Gained: 450, Count: 1},
		{Reason: "RAID", Gained: 250, Count: 1},
		{Reason: "CLAIM", Gained: 50, Count: 1},
		{Reason: "WATCH", Gained: 24, Count: 2},
		{Reason: "OTHER", Gained: 7, Count: 1},
	}
	if !reflect.DeepEqual(got.Breakdown, want) {
		t.Fatalf("exact breakdown = %+v, want %+v", got.Breakdown, want)
	}
	if got.LegacyBreakdown != nil {
		t.Fatalf("legacy breakdown = %+v, want none for a fully exact range", got.LegacyBreakdown)
	}
	byType := map[string]string{}
	for _, a := range got.Annotations {
		byType[a.Type] = a.Reason
	}
	if byType["RAID"] != "+250 - Raid" || byType["WATCH_STREAK"] != "+450 - Watch Streak" || len(got.Annotations) != 2 {
		t.Fatalf("annotations = %+v, want exactly the RAID and WATCH_STREAK markers at their event-local amounts", got.Annotations)
	}
}

// TestPointsSpentIsNeverAnEarning: points-spent frames stay on the balance
// timeline (charting) but never become earnings — not even when a spent
// snapshot is higher than the previous one because that one was stale.
func TestPointsSpentIsNeverAnEarning(t *testing.T) {
	m, s, svc := newPointEventMiner(t, "spent")
	login := s.GetUsername()
	deliverPointsEarned(t, m, s, parsedPointsEarned(t, s.ChannelID, "WATCH", 12, 1012, "2026-09-01T12:00:00.000000000Z"))
	deliverPointsSpent(t, m, s, 900, "2026-09-01T12:01:00.000000000Z")
	deliverPointsSpent(t, m, s, 1300, "2026-09-01T12:02:00.000000000Z") // stale previous snapshot: +400 delta
	deliverPointsEarned(t, m, s, parsedPointsEarned(t, s.ChannelID, "WATCH", 12, 1312, "2026-09-01T12:05:00.000000000Z"))

	got := pointsHistoryViaAPI(t, m, svc, login, "7d")
	if got.Earnings.Coverage != analytics.EarningsCoverageExact || !got.Earnings.Exact {
		t.Fatalf("earnings = %+v, want exact coverage (spent snapshots are not legacy earnings)", got.Earnings)
	}
	if len(got.Breakdown) != 1 || got.Breakdown[0].Reason != "WATCH" || got.Breakdown[0].Gained != 24 || got.Breakdown[0].Count != 2 {
		t.Fatalf("breakdown = %+v, want only WATCH 24 over 2 (no spend ever counted as earned)", got.Breakdown)
	}
	if len(got.Points) != 4 || got.Points[1].Exact || got.Points[2].Exact || got.Points[1].Reason != "Spent" {
		t.Fatalf("timeline = %+v, want four samples with the two spent snapshots kept for charting, not exact", got.Points)
	}
}

// TestMutableStreamerBalanceCannotChangeRecordedEarnings: the shared Streamer
// balance is mutated by other goroutines (a ChannelPointsContext poll, a later
// frame) — including BETWEEN the pool applying a frame's balance and the miner
// callback running. The recorded amount, balance and timeline sample must be
// the frame's own values, never whatever the Streamer holds at callback time.
func TestMutableStreamerBalanceCannotChangeRecordedEarnings(t *testing.T) {
	m, s, svc := newPointEventMiner(t, "mutable")
	login := s.GetUsername()

	// Frame 1 delivered normally, then a poll rewrites the shared balance.
	deliverPointsEarned(t, m, s, parsedPointsEarned(t, s.ChannelID, "WATCH_STREAK", 450, 11772, "2026-09-01T13:00:00.000000000Z"))
	s.SetChannelPoints(999_999)

	// Frame 2: the pool has applied the wire balance (11784) but a concurrent
	// poll overwrites it with a sentinel before the callback observes it.
	watch := parsedPointsEarned(t, s.ChannelID, "WATCH", 12, 11784, "2026-09-01T13:05:00.000000000Z")
	s.SetChannelPoints(11784)
	s.SetChannelPoints(424_242)
	m.handlePubSubMessage(watch, s, pubsub.MessageOutcome{})
	s.SetChannelPoints(1)

	got := pointsHistoryViaAPI(t, m, svc, login, "7d")
	if streak := shareByReason(got.Breakdown, "WATCH_STREAK"); streak.Gained != 450 || streak.Count != 1 {
		t.Fatalf("WATCH_STREAK = %+v, want the captured 450", streak)
	}
	if w := shareByReason(got.Breakdown, "WATCH"); w.Gained != 12 || w.Count != 1 {
		t.Fatalf("WATCH = %+v, want the captured 12", w)
	}
	if len(got.Points) != 2 || got.Points[0].Balance != 11772 || got.Points[1].Balance != 11784 {
		t.Fatalf("timeline = %+v, want the frames' own balances 11772/11784, never the mutated 999999/424242/1", got.Points)
	}
}

// TestMixedHistoryReportsExactAndLegacySeparately: a range that spans
// pre-ledger history and exact events reports the exact aggregation as the
// primary breakdown, the legacy estimate separately, the boundary timestamp,
// and never the sum of the two.
func TestMixedHistoryReportsExactAndLegacySeparately(t *testing.T) {
	m, s, svc := newPointEventMiner(t, "mixed")
	login := s.GetUsername()

	// Pre-ledger history, exactly as the previous release wrote it.
	s.SetChannelPoints(11310)
	svc.RecordPoints(s, "WATCH")
	s.SetChannelPoints(11772)
	svc.RecordPoints(s, "WATCH_STREAK") // legacy: estimated as +462

	// Exact events from now on.
	deliverPointsEarned(t, m, s, parsedPointsEarned(t, s.ChannelID, "WATCH_STREAK", 450, 14788, "2026-09-01T14:00:00.000000000Z"))
	deliverPointsEarned(t, m, s, parsedPointsEarned(t, s.ChannelID, "WATCH_STREAK", 450, 18152, "2026-09-01T14:30:00.000000000Z"))

	got := pointsHistoryViaAPI(t, m, svc, login, "7d")
	if got.Earnings.Coverage != analytics.EarningsCoverageMixed || !got.Earnings.Exact || got.Earnings.LegacyStatus != analytics.LegacyStatusEstimated {
		t.Fatalf("earnings = %+v, want mixed coverage with an estimated legacy part", got.Earnings)
	}
	exactStreak := shareByReason(got.Breakdown, "WATCH_STREAK")
	if exactStreak.Gained != 900 || exactStreak.Count != 2 {
		t.Fatalf("exact WATCH_STREAK = %+v, want 900 over 2 — never 1362 (900 + the 462 estimate)", exactStreak)
	}
	legacyStreak := shareByReason(got.LegacyBreakdown, "WATCH_STREAK")
	if legacyStreak.Gained != 462 || legacyStreak.Count != 1 {
		t.Fatalf("legacy WATCH_STREAK estimate = %+v, want 462 over 1, reported separately", legacyStreak)
	}
	if len(got.Points) != 4 || got.Points[0].Exact || got.Points[1].Exact || !got.Points[2].Exact || !got.Points[3].Exact {
		t.Fatalf("timeline exact flags = %+v, want [legacy, legacy, exact, exact]", got.Points)
	}
	if got.Earnings.ExactSince != got.Points[2].T {
		t.Fatalf("exactSince = %d, want the first exact sample's timestamp %d", got.Earnings.ExactSince, got.Points[2].T)
	}
}

// TestStatisticsWatchStreakEarningsAreExactEventAmounts is the production
// regression behind this change. It replays, at the accepted-PubSub-event →
// analytics → Statistics API seam, the owner's production evidence for
// talkto_megoose: exactly three accepted WATCH_STREAK grants whose event-local
// Twitch total_points were 450 each, delivered against the balance timeline
// Twitch actually reported in those frames:
//
//	WATCH_STREAK  11310 -> 11772  (+462 balance delta, +450 wire grant)
//	WATCH         11772 -> 11322  (-450: stale balance in the next frame)
//	WATCH         11322 -> 11784  (+462)
//	WATCH_STREAK  14338 -> 14788  (+450 balance delta, +450 wire grant)
//	WATCH_STREAK  17690 -> 18152  (+462 balance delta, +450 wire grant)
//	WATCH         18152 -> 17702  (-450)
//	WATCH         17702 -> 18164  (+462)
//
// The Statistics earnings for WATCH_STREAK must be the exact event-local sum
// of the three grants — 1350 over 3 events — never the absolute-balance-delta
// attribution 462+450+462 = 1374 that the dashboard produced in production.
// The expected values are the owner's independently observed literals, not a
// recomputation by either algorithm.
func TestStatisticsWatchStreakEarningsAreExactEventAmounts(t *testing.T) {
	m, s, svc := newPointEventMiner(t, "prod-streak-exact")
	login := s.GetUsername()

	frames := []struct {
		reason  string
		wire    int
		balance int
		ts      string
	}{
		{"WATCH", 12, 11310, "2026-08-26T21:00:12.000000000Z"}, // pre-streak baseline sample
		{"WATCH_STREAK", 450, 11772, "2026-08-26T21:05:39.000000000Z"},
		{"WATCH", 12, 11322, "2026-08-26T21:05:40.000000000Z"},
		{"WATCH", 12, 11784, "2026-08-26T21:10:41.000000000Z"},
		{"WATCH", 12, 14338, "2026-08-29T20:00:15.000000000Z"}, // pre-streak baseline sample
		{"WATCH_STREAK", 450, 14788, "2026-08-29T20:05:17.000000000Z"},
		{"WATCH", 12, 17690, "2026-08-31T21:03:02.000000000Z"}, // pre-streak baseline sample
		{"WATCH_STREAK", 450, 18152, "2026-08-31T21:08:04.000000000Z"},
		{"WATCH", 12, 17702, "2026-08-31T21:08:05.000000000Z"},
		{"WATCH", 12, 18164, "2026-08-31T21:13:06.000000000Z"},
	}
	for _, f := range frames {
		msg := parsedPointsEarned(t, s.ChannelID, f.reason, f.wire, f.balance, f.ts)
		outcome := deliverPointsEarned(t, m, s, msg)
		if f.reason == "WATCH_STREAK" && !outcome.WatchStreak.NewlyAccepted() {
			t.Fatalf("streak frame %s was not newly accepted: %s", f.ts, outcome.WatchStreak.Admission)
		}
	}

	got := pointsHistoryViaAPI(t, m, svc, login, "7d")

	streak := shareByReason(got.Breakdown, "WATCH_STREAK")
	if streak.Gained != 1350 || streak.Count != 3 {
		t.Fatalf("WATCH_STREAK earnings = gained %d over %d events, want exact 1350 over 3 (production showed 1374 = 462+450+462 balance deltas); breakdown=%+v",
			streak.Gained, streak.Count, got.Breakdown)
	}

	// Interleaving: the seven passive WATCH frames carried +12 each. A balance
	// jump between frames (the pre-streak baselines above) or a stale-balance
	// frame must never be absorbed into WATCH — or into any other reason.
	watch := shareByReason(got.Breakdown, "WATCH")
	if watch.Gained != 84 || watch.Count != 7 {
		t.Fatalf("WATCH earnings = gained %d over %d events, want exact 84 over 7; breakdown=%+v", watch.Gained, watch.Count, got.Breakdown)
	}
	for _, share := range got.Breakdown {
		if share.Reason != "WATCH_STREAK" && share.Reason != "WATCH" {
			t.Fatalf("unexpected earnings category %+v; only WATCH_STREAK and WATCH events were delivered", share)
		}
	}
}
