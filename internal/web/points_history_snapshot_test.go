package web

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database/dbtest"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// splitSnapshotRig is the deterministic race harness for the Statistics
// history/export responses. The Server's analytics service runs on a private
// database opened through the hooked driver, so every statement of a request
// is reported to the rig BEFORE it executes; a second, plain handle on the
// same file plays the concurrent writer (the pubsub goroutine committing an
// accepted point event) at exactly the statement boundary the rig chooses.
//
// The writer uses its own connection, so its commit is a real, separate
// commit point: with the response read as three statements outside a
// transaction (the bug), the commit lands between them and the response mixes
// two database states; with the reads inside one transaction, SQLite's
// SHARED lock makes the commit fail with SQLITE_BUSY until the transaction
// ends (the driver has no busy timeout), so the response is one state.
type splitSnapshotRig struct {
	t        *testing.T
	srv      *Server
	writer   *analytics.SQLiteRepository
	writerDB *database.DB
	login    string

	mu         sync.Mutex
	armed      bool
	sawSamples bool // the samples read has run; later reads are the split points
	inject     analytics.PointEvent
	attempts   []error // outcome of every concurrent write attempt, in order
	landed     bool
	commits    []int // response bytes already written at each observed commit
	events     []dbtest.Event
	rec        *httptest.ResponseRecorder
}

func newSplitSnapshotRig(t *testing.T) *splitSnapshotRig {
	t.Helper()
	rig := &splitSnapshotRig{t: t, login: uniqueLogin("snapshot")}
	dir := t.TempDir()
	path := filepath.Join(dir, "miner.db")

	db := dbtest.OpenHooked(path, rig.observe)
	t.Cleanup(func() { _ = db.Close() })
	svc, err := analytics.NewService(db, dir, 0)
	if err != nil {
		t.Fatalf("analytics service: %v", err)
	}
	rig.srv = NewServerEarly(config.AnalyticsSettings{Refresh: 5, DaysAgo: 7}, "tester", dir, svc)

	// The concurrent writer: a separate connection to the same file, with
	// the driver's default of no busy timeout (a blocked commit fails at
	// once instead of waiting, which keeps the rig free of timing).
	plain, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open writer db: %v", err)
	}
	plain.SetMaxOpenConns(1)
	rig.writerDB = &database.DB{DB: plain}
	t.Cleanup(func() { _ = rig.writerDB.Close() })
	rig.writer, err = analytics.NewSQLiteRepository(rig.writerDB, dir)
	if err != nil {
		t.Fatalf("writer repo: %v", err)
	}

	// Seed one accepted streak event (sample + ledger row + marker) two
	// minutes ago, well inside every request window.
	st := models.NewStreamer(rig.login, models.DefaultStreamerSettings())
	seeded, err := svc.RecordPointEvent(st, analytics.PointEvent{
		EventID: "sha256:" + rig.login + "-1", Timestamp: time.Now().Add(-2 * time.Minute).UnixMilli(),
		ReasonCode: "WATCH_STREAK", TotalPoints: 450, BalanceAfter: 1450, BalanceKnown: true,
	})
	if err != nil || !seeded {
		t.Fatalf("seed event: recorded=%v err=%v", seeded, err)
	}
	// The event the writer will commit mid-request: stamped one minute ago,
	// i.e. before the request's window end, exactly the shape a real event
	// has when its frame arrived before the request and its commit lands
	// during it.
	rig.inject = analytics.PointEvent{
		EventID: "sha256:" + rig.login + "-2", Timestamp: time.Now().Add(-time.Minute).UnixMilli(),
		ReasonCode: "WATCH_STREAK", TotalPoints: 450, BalanceAfter: 1900, BalanceKnown: true,
	}
	return rig
}

// observe is the driver hook. Once armed, it tries to commit the injected
// event on the writer's connection before every response read that follows
// the samples read, and records what happened at every transaction commit.
func (r *splitSnapshotRig) observe(e dbtest.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.armed {
		return
	}
	r.events = append(r.events, e)
	switch e.Kind {
	case "commit":
		if r.rec != nil {
			r.commits = append(r.commits, r.rec.Body.Len())
		}
	case "begin":
		// A transaction that only begins AFTER the samples were read leaves
		// the samples outside the snapshot: that gap is a split point too.
		if r.sawSamples && !r.landed {
			r.attemptLocked()
		}
	case "query":
		// The samples query embeds an EXISTS over point_events, so it is
		// recognised by its own FROM clause and only marks the split points
		// that follow it: annotations, the exact aggregate, the bets.
		if strings.Contains(e.SQL, "FROM points p") {
			r.sawSamples = true
			return
		}
		if !r.sawSamples || r.landed {
			return
		}
		if strings.Contains(e.SQL, "FROM annotations") || strings.Contains(e.SQL, "FROM point_events") || strings.Contains(e.SQL, "FROM prediction_bets") {
			r.attemptLocked()
		}
	}
}

// attemptLocked commits the injected event through the writer's connection.
// A commit refused by the reader's open transaction leaves the writer's
// SQLite transaction open (COMMIT failed with SQLITE_BUSY), so it is rolled
// back explicitly to keep the writer usable for the next attempt.
func (r *splitSnapshotRig) attemptLocked() {
	ann := analytics.PointEventAnnotation{EventType: "WATCH_STREAK", Text: "+450 - Watch Streak", Color: "#8b7fd1"}
	recorded, err := r.writer.RecordPointEvent(r.login, r.inject, r.inject.BalanceAfter, &ann)
	r.attempts = append(r.attempts, err)
	if err == nil && recorded {
		r.landed = true
		return
	}
	_, _ = r.writerDB.Exec("ROLLBACK")
}

func (r *splitSnapshotRig) fetch(path string) analytics.PointsHistory {
	r.t.Helper()
	rec := httptest.NewRecorder()
	r.mu.Lock()
	r.rec = rec
	r.armed = true
	r.sawSamples = false
	r.mu.Unlock()
	r.srv.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	r.mu.Lock()
	r.armed = false
	r.mu.Unlock()
	if rec.Code != http.StatusOK {
		r.t.Fatalf("%s: status = %d, want 200; body=%s", path, rec.Code, rec.Body.String())
	}
	var got analytics.PointsHistory
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		r.t.Fatalf("decode: %v", err)
	}
	return got
}

// coherence reduces a response to the three counts that must agree: exact
// samples, streak markers and exact streak events. One event seeded → all 1;
// the injected event landed before the response → all 2; anything else is a
// response assembled from two database states.
func coherence(got analytics.PointsHistory) (exactSamples, markers, exactEvents int) {
	for _, p := range got.Points {
		if p.Exact {
			exactSamples++
		}
	}
	for _, a := range got.Annotations {
		if a.Type == "WATCH_STREAK" {
			markers++
		}
	}
	exactEvents = shareByReason(got.ExactBreakdown, "WATCH_STREAK").Count
	return
}

// observedRead reports whether a read statement containing clause ran
// during the armed request.
func (r *splitSnapshotRig) observedRead(clause string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e.Kind == "query" && strings.Contains(e.SQL, clause) {
			return true
		}
	}
	return false
}

func shareByReason(shares []analytics.ReasonShare, reason string) analytics.ReasonShare {
	for _, s := range shares {
		if s.Reason == reason {
			return s
		}
	}
	return analytics.ReasonShare{}
}

// TestPointsHistoryResponseIsOneSnapshot forces the split-snapshot race for
// both endpoints: an accepted event stamped before the request's window end
// commits on another connection between the response's reads. The response
// must represent one database state — every component before the commit, or
// every component after it — never a mixture where the exact aggregate
// counts an event whose sample and marker are absent.
func TestPointsHistoryResponseIsOneSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		readsBets  bool // the history presents a bet summary; the export does not
	}{
		{"history", "/api/points-history?streamer=%s&range=24h", true},
		{"export", "/api/points-history/export?streamer=%s&range=24h", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newSplitSnapshotRig(t)
			got := rig.fetch(fmt.Sprintf(tc.path, rig.login))
			// Evidence for the record (visible with -v): what the concurrent
			// writer saw at each split point. Under one snapshot transaction
			// the rollback-journal SHARED lock refuses its commit (SQLITE_BUSY)
			// until the response's reads are done.
			t.Logf("%s: concurrent writer attempts during the request: %v (landed=%v)", tc.name, rig.attempts, rig.landed)

			exactSamples, markers, exactEvents := coherence(got)
			if exactSamples != markers || markers != exactEvents || exactSamples < 1 || exactSamples > 2 {
				t.Fatalf("%s response assembled from two database states: exact samples=%d, streak markers=%d, exact streak events=%d (writer attempts=%v, landed=%v)",
					tc.name, exactSamples, markers, exactEvents, rig.attempts, rig.landed)
			}
			if len(rig.attempts) == 0 {
				t.Fatalf("%s: the rig never attempted the concurrent commit — no read after the samples was observed (events=%v)", tc.name, rig.events)
			}
			if readBets := rig.observedRead("FROM prediction_bets"); readBets != tc.readsBets {
				t.Fatalf("%s: bets read inside the snapshot = %v, want %v (events=%v)", tc.name, readBets, tc.readsBets, rig.events)
			}
			// The concurrent event is not lost, only ordered: once the
			// response is out it commits and the next response shows it
			// whole.
			if !rig.landed {
				rig.mu.Lock()
				rig.attemptLocked()
				landed := rig.landed
				rig.mu.Unlock()
				if !landed {
					t.Fatalf("%s: the concurrent event could not commit after the response either: %v", tc.name, rig.attempts)
				}
			}
			after := rig.fetch(fmt.Sprintf(tc.path, rig.login))
			if s, m, e := coherence(after); s != 2 || m != 2 || e != 2 {
				t.Fatalf("%s after the commit: exact samples=%d markers=%d events=%d, want 2/2/2", tc.name, s, m, e)
			}
		})
	}
}

// TestPointsHistoryResponseSerializedAfterSnapshot pins the transaction
// lifetime: the snapshot's read transaction commits before a single response
// byte is written, on both endpoints, so no database transaction — and no
// connection — is held while the response is encoded; and every statement
// of the request runs inside that one transaction, so nothing the response
// presents is read before it begins or after it commits.
func TestPointsHistoryResponseSerializedAfterSnapshot(t *testing.T) {
	for _, tc := range []struct{ name, path string }{
		{"history", "/api/points-history?streamer=%s&range=24h"},
		{"export", "/api/points-history/export?streamer=%s&range=24h"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newSplitSnapshotRig(t)
			rig.fetch(fmt.Sprintf(tc.path, rig.login))
			rig.mu.Lock()
			commits, events := rig.commits, rig.events
			rig.mu.Unlock()
			if len(commits) != 1 {
				t.Fatalf("%s: observed %d transaction commits during the request, want exactly one snapshot transaction (events=%v)", tc.name, len(commits), events)
			}
			if commits[0] != 0 {
				t.Fatalf("%s: %d response bytes were written while the snapshot transaction was still open", tc.name, commits[0])
			}
			var begins int
			for _, e := range events {
				if e.Kind == "begin" {
					begins++
				}
			}
			if begins != 1 {
				t.Fatalf("%s: observed %d transaction begins, want one", tc.name, begins)
			}
			var open, closed bool
			for _, e := range events {
				switch e.Kind {
				case "begin":
					open = true
				case "commit", "rollback":
					closed = true
				default:
					if !open || closed {
						t.Fatalf("%s: statement outside the snapshot transaction (before its begin=%v, after its commit=%v): %s %q", tc.name, !open, closed, e.Kind, e.SQL)
					}
				}
			}
		})
	}
}
