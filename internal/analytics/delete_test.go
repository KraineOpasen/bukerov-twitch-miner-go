package analytics

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestDeleteStreamerRemovesAllTablesPreservesOthers verifies DeleteStreamer
// scrubs every analytics table for the login while leaving unrelated streamers
// and the shared hidden drops bucket untouched.
func TestDeleteStreamerRemovesAllTablesPreservesOthers(t *testing.T) {
	r := newTestRepo(t)

	for _, s := range []string{"del-victim", "del-keep"} {
		if err := r.RecordPoints(s, 100, "WATCH"); err != nil {
			t.Fatalf("points %s: %v", s, err)
		}
		if err := r.RecordAnnotation(s, "WIN", "w", "#36b535"); err != nil {
			t.Fatalf("annotation %s: %v", s, err)
		}
		if err := r.RecordChatMessage(s, ChatMessage{Username: s, Message: "hi"}); err != nil {
			t.Fatalf("chat %s: %v", s, err)
		}
		if err := r.RecordBet(BetRecord{Streamer: s, EventID: s + "-e", Strategy: "SMART", ResultType: "WIN", Odds: 2}); err != nil {
			t.Fatalf("bet %s: %v", s, err)
		}
	}
	// Shared hidden drops bucket must never be deleted.
	if err := r.RecordAnnotation(DropsBucket, "WIN", "drop", "#36b535"); err != nil {
		t.Fatalf("drops bucket: %v", err)
	}

	existed, err := r.DeleteStreamer(context.Background(), "del-victim")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !existed {
		t.Fatal("delete reported nothing existed, but victim had rows")
	}

	// Victim: every table empty.
	if data, _ := r.GetStreamerData("del-victim"); len(data.Series) != 0 || len(data.Annotations) != 0 {
		t.Error("victim points/annotations not deleted")
	}
	if msgs, _ := r.GetChatMessages("del-victim", 100, 0); msgs.TotalCount != 0 {
		t.Error("victim chat messages not deleted")
	}
	if bets, _ := r.GetBets("del-victim", "", time.Time{}, time.Time{}); len(bets) != 0 {
		t.Error("victim bets not deleted")
	}
	// del-victim gone from ListStreamers; del-keep retained.
	seen := map[string]bool{}
	list, _ := r.ListStreamers()
	for _, info := range list {
		seen[info.Name] = true
	}
	if seen["del-victim"] {
		t.Error("deleted streamer still in ListStreamers")
	}
	if !seen["del-keep"] {
		t.Error("unrelated streamer dropped from ListStreamers")
	}
	// del-keep fully intact.
	if data, _ := r.GetStreamerData("del-keep"); len(data.Series) == 0 {
		t.Error("unrelated streamer lost its points")
	}
	// Drops bucket intact.
	if data, _ := r.GetStreamerData(DropsBucket); len(data.Annotations) == 0 {
		t.Error("shared drops bucket was deleted")
	}
}

// TestDeleteStreamerNeverDeletesDropsBucket: an explicit delete of the reserved
// bucket name is a guarded no-op.
func TestDeleteStreamerNeverDeletesDropsBucket(t *testing.T) {
	r := newTestRepo(t)
	if err := r.RecordAnnotation(DropsBucket, "WIN", "drop", "#36b535"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	existed, err := r.DeleteStreamer(context.Background(), DropsBucket)
	if err != nil {
		t.Fatalf("delete drops bucket: %v", err)
	}
	if existed {
		t.Error("delete of drops bucket reported deletion; it must be a no-op")
	}
	if data, _ := r.GetStreamerData(DropsBucket); len(data.Annotations) == 0 {
		t.Error("drops bucket rows were deleted")
	}
}

// TestDeleteUnknownStreamerIdempotent: deleting a login with no rows is (false, nil).
func TestDeleteUnknownStreamerIdempotent(t *testing.T) {
	r := newTestRepo(t)
	existed, err := r.DeleteStreamer(context.Background(), "never-existed")
	if err != nil {
		t.Fatalf("delete unknown: %v", err)
	}
	if existed {
		t.Error("unknown streamer reported as existing")
	}
}

// TestTombstoneBlocksEveryWritePath (D24/D25): while tombstoned, none of the four
// write paths may recreate the streamer's row.
func TestTombstoneBlocksEveryWritePath(t *testing.T) {
	r := newTestRepo(t)
	r.Tombstone("Fenced") // mixed case: fence is case-insensitive

	if err := r.RecordPoints("fenced", 1, "WATCH"); !errors.Is(err, ErrStreamerDeleted) {
		t.Errorf("RecordPoints: got %v, want ErrStreamerDeleted", err)
	}
	if err := r.RecordAnnotation("fenced", "WIN", "w", "#36b535"); !errors.Is(err, ErrStreamerDeleted) {
		t.Errorf("RecordAnnotation: got %v, want ErrStreamerDeleted", err)
	}
	if err := r.RecordChatMessage("fenced", ChatMessage{Username: "x"}); !errors.Is(err, ErrStreamerDeleted) {
		t.Errorf("RecordChatMessage: got %v, want ErrStreamerDeleted", err)
	}
	if err := r.RecordBet(BetRecord{Streamer: "fenced", EventID: "e"}); !errors.Is(err, ErrStreamerDeleted) {
		t.Errorf("RecordBet: got %v, want ErrStreamerDeleted", err)
	}
	// No row was created.
	if data, _ := r.GetStreamerData("fenced"); len(data.Series) != 0 {
		t.Error("a tombstoned write recreated the streamer row")
	}

	// Reinstate re-allows writes.
	r.Reinstate("fenced")
	if err := r.RecordPoints("fenced", 7, "WATCH"); err != nil {
		t.Fatalf("RecordPoints after reinstate: %v", err)
	}
	if data, _ := r.GetStreamerData("fenced"); len(data.Series) != 1 {
		t.Errorf("after reinstate got %d points, want 1", len(data.Series))
	}
}
