package drops

import (
	"context"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// F-D2 tests that drive the refresh's own seams (the per-pass lifecycle
// context and the no-churn equality). They complement
// assignment_freshness_test.go, which uses only entry points that also exist
// on the pre-F-D2 base.

// An old pass bound to a cancelled loop context must not borrow the live
// context of a replacement lifecycle generation.
func TestFreshnessOldPassDoesNotBorrowReplacementContext(t *testing.T) {
	now := time.Now()
	d, s, _ := continuitySetup(&now)
	d.client = clientFor(freshnessResponses()[0])
	setAvailability(s, true, []string{"camp-1"}, now)
	d.updateStreamerCampaigns()
	old, cancelOld := context.WithCancel(context.Background())
	cancelOld()
	setLifecycle(d, context.Background()) // replacement generation is live
	setAvailability(s, true, nil, now)

	d.syncProgressWithContext(old)
	if !hasAssignment(s) {
		t.Fatal("an old cancelled pass must not run the refresh under the replacement's context")
	}
	d.syncProgressWithContext(context.Background())
	if hasAssignment(s) {
		t.Fatalf("a live pass must apply the refresh, got %v", assignedIDs(s))
	}
}

// Cancellation of the pass's own context during a blocked ledger read, while
// the tracker's (replacement) lifecycle context stays live: the read is
// abandoned promptly -- it does not keep waiting on the single SQLite
// connection -- nothing is published, and the abandonment is not misreported
// as a ledger outage.
func TestFreshnessCancelledDuringLedgerReadPublishesNothing(t *testing.T) {
	now := time.Now()
	d, s, _ := continuitySetup(&now)
	ledger, db := openFreshnessLedger(t)
	d.skipLedger = ledger
	d.client = clientFor(freshnessResponses()[0])
	setAvailability(s, true, []string{"camp-1"}, now)
	d.updateStreamerCampaigns()
	generation := d.BrokerCampaignSnapshot().Generation
	setLifecycle(d, context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	setAvailability(s, true, nil, now)
	h := installPauseOnMessage(t, "")
	release := holdLedgerConnection(t, db)
	waits := db.Stats().WaitCount

	done := make(chan struct{})
	go func() {
		defer close(done)
		d.syncProgressWithContext(ctx)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for db.Stats().WaitCount == waits {
		if time.Now().After(deadline) {
			release()
			<-done
			t.Fatal("the refresh never blocked on the ledger read")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		release()
		<-done
		t.Fatal("the pass did not abandon its ledger read on cancellation; the read must be bound to the pass's own context")
	}
	release()

	if !hasAssignment(s) {
		t.Fatal("a pass cancelled during its ledger read must not apply")
	}
	if g := d.BrokerCampaignSnapshot().Generation; g != generation {
		t.Fatalf("a pass cancelled during its ledger read must not publish: %d -> %d", generation, g)
	}
	if !h.logged(logRefreshCancelledInRead) || h.logged(logRefreshLedgerDeferred) {
		t.Fatalf("cancellation must be reported as cancellation, got %v", h.records)
	}
}

// The no-churn equality only reports "unchanged" when the published and fresh
// views provably derive from the same source objects with the same surviving
// drops; ambiguous identities (duplicate campaign or drop IDs, nil drops)
// count as changed rather than hiding a different occurrence.
func TestSameBrokerViewsRejectsAmbiguousIdentities(t *testing.T) {
	drop := func(id string, minutes int) *models.Drop {
		return &models.Drop{ID: id, Name: "R-" + id, MinutesRequired: 60, CurrentMinutesWatched: minutes}
	}
	views := func(cs ...*models.Campaign) []*models.Campaign { return cs }

	src := &models.Campaign{ID: "c", Drops: []*models.Drop{drop("a", 1), drop("b", 2)}}
	if !sameBrokerViews(views(src.Clone()), views(src.Clone()), views(src)) {
		t.Fatal("clones of the same source with the same drops must compare equal")
	}
	if !sameBrokerViews(views(src), views(src), views(src)) {
		t.Fatal("identical pointers must compare equal")
	}
	fewer := src.Clone()
	fewer.Drops = fewer.Drops[:1]
	if sameBrokerViews(views(src.Clone()), views(fewer), views(src)) {
		t.Fatal("a different surviving drop subset is a semantic change")
	}
	if sameBrokerViews(views(src.Clone()), views(), views(src)) {
		t.Fatal("a withheld campaign is a semantic change")
	}

	dupDrops := &models.Campaign{ID: "c", Drops: []*models.Drop{drop("a", 1), drop("a", 50)}}
	first, second := dupDrops.Clone(), dupDrops.Clone()
	first.Drops, second.Drops = first.Drops[:1], second.Drops[1:]
	if sameBrokerViews(views(first), views(second), views(dupDrops)) {
		t.Fatal("duplicate drop IDs in the source must not let a different occurrence compare equal")
	}

	twinA := &models.Campaign{ID: "c", Drops: []*models.Drop{drop("a", 1)}}
	twinB := &models.Campaign{ID: "c", Drops: []*models.Drop{drop("a", 50)}}
	if sameBrokerViews(views(twinA.Clone()), views(twinB.Clone()), views(twinA, twinB)) {
		t.Fatal("duplicate campaign IDs in the pool must not compare equal")
	}

	withNil := &models.Campaign{ID: "c", Drops: []*models.Drop{drop("a", 1), nil}}
	keptA := &models.Campaign{ID: "c", Drops: []*models.Drop{withNil.Drops[0]}}
	keptB := &models.Campaign{ID: "c", Drops: []*models.Drop{withNil.Drops[0]}}
	if sameBrokerViews(views(keptA), views(keptB), views(withNil)) {
		t.Fatal("a source with a nil drop cannot be matched unambiguously")
	}
}
