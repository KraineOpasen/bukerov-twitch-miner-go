package models

import (
	"testing"
	"time"
)

// fixedClock returns a Clock pinned to t for deterministic window/identity tests.
func fixedClock(t time.Time) Clock { return func() time.Time { return t } }

var testNow = time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)

// knownWindow builds a decidable per-drop window.
func knownWindow(start, end time.Time) EntitlementWindow {
	return EntitlementWindow{Start: start, End: end, Source: WindowSourceDrop, Known: true}
}

func hoursFromNow(h int) time.Time { return testNow.Add(time.Duration(h) * time.Hour) }

// ---------------------------------------------------------------------------
// EntitlementWindow

func TestEntitlementWindowState(t *testing.T) {
	clock := fixedClock(testNow)
	cases := []struct {
		name string
		w    EntitlementWindow
		want WindowState
	}{
		{"dateless unknown", EntitlementWindow{}, WindowStateUnknown},
		{"not known but bounds set", EntitlementWindow{Start: hoursFromNow(-1), End: hoursFromNow(1)}, WindowStateUnknown},
		{"active", knownWindow(hoursFromNow(-1), hoursFromNow(1)), WindowStateActive},
		{"upcoming", knownWindow(hoursFromNow(1), hoursFromNow(2)), WindowStateUpcoming},
		{"expired", knownWindow(hoursFromNow(-2), hoursFromNow(-1)), WindowStateExpired},
		{"open-ended start only, active", knownWindow(hoursFromNow(-1), time.Time{}), WindowStateActive},
		{"open-ended end only, active", knownWindow(time.Time{}, hoursFromNow(1)), WindowStateActive},
		{"open-ended end only, expired", knownWindow(time.Time{}, hoursFromNow(-1)), WindowStateExpired},
		{"inverted/malformed -> unknown", knownWindow(hoursFromNow(1), hoursFromNow(-1)), WindowStateUnknown},
		{"half-open boundary: now==end is expired", knownWindow(hoursFromNow(-1), testNow), WindowStateExpired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.w.State(clock); got != tc.want {
				t.Fatalf("State = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEntitlementWindowDisjoint(t *testing.T) {
	a := knownWindow(hoursFromNow(-4), hoursFromNow(-2))
	b := knownWindow(hoursFromNow(-1), hoursFromNow(1))
	if !a.DisjointFrom(b) {
		t.Error("non-overlapping known windows should be disjoint")
	}
	overlap := knownWindow(hoursFromNow(-3), hoursFromNow(0))
	if a.DisjointFrom(overlap) {
		t.Error("overlapping windows should not be disjoint")
	}
	// Unknown windows are never provably disjoint (fail open).
	if a.DisjointFrom(EntitlementWindow{}) {
		t.Error("unknown window must not be provably disjoint")
	}
}

// ---------------------------------------------------------------------------
// MatchIdentity — the core evidence policy

func TestMatchIdentity(t *testing.T) {
	clock := fixedClock(testNow)
	winA := knownWindow(hoursFromNow(-2), hoursFromNow(2))
	winLater := knownWindow(hoursFromNow(24), hoursFromNow(48)) // disjoint later occurrence

	// helper builders
	benefit := func(gameID, benefitID, name string, w EntitlementWindow) RewardIdentity {
		return NewRewardIdentity(gameID, benefitID, "", "", "", name, 0, w)
	}
	composite := func(gameID, campaignID, dropID, name string, w EntitlementWindow) RewardIdentity {
		return NewRewardIdentity(gameID, "", "", dropID, campaignID, name, 0, w)
	}
	nameOnly := func(gameID, name string, w EntitlementWindow) RewardIdentity {
		return NewRewardIdentity(gameID, "", "", "", "", name, 0, w)
	}

	cases := []struct {
		name          string
		claimed, cand RewardIdentity
		want          IdentityMatch
	}{
		{
			// 1. Exact BenefitID + same occurrence -> confirmed.
			name:    "benefit same occurrence confirmed",
			claimed: benefit("g1", "ben-1", "Skin", winA),
			cand:    benefit("g1", "ben-1", "Skin", winA),
			want:    IdentityMatchConfirmed,
		},
		{
			// 2. Exact BenefitID + disjoint windows -> no match.
			name:    "benefit disjoint windows no match",
			claimed: benefit("g1", "ben-1", "Skin", winA),
			cand:    benefit("g1", "ben-1", "Skin", winLater),
			want:    IdentityNoMatch,
		},
		{
			// B1: Exact BenefitID + one unknown window -> ambiguous (benefit id
			// proves a reward family, not a specific occurrence).
			name:    "benefit unknown window ambiguous",
			claimed: benefit("g1", "ben-1", "Skin", EntitlementWindow{}),
			cand:    benefit("g1", "ben-1", "Skin", winA),
			want:    IdentityMatchAmbiguous,
		},
		{
			// B1: Exact BenefitID + both unknown windows -> ambiguous.
			name:    "benefit both unknown windows ambiguous",
			claimed: benefit("g1", "ben-1", "Skin", EntitlementWindow{}),
			cand:    benefit("g1", "ben-1", "Skin", EntitlementWindow{}),
			want:    IdentityMatchAmbiguous,
		},
		{
			// 3. Same display name + different BenefitID -> no match.
			name:    "same name different benefit no match",
			claimed: benefit("g1", "ben-1", "Skin", winA),
			cand:    benefit("g1", "ben-2", "Skin", winA),
			want:    IdentityNoMatch,
		},
		{
			// 7. Same name in different games -> no match.
			name:    "same name different game no match",
			claimed: benefit("g1", "ben-1", "Skin", winA),
			cand:    benefit("g2", "ben-1", "Skin", winA),
			want:    IdentityNoMatch,
		},
		{
			// 8. Localized/renamed name + same strong ID/window -> match.
			name:    "renamed name same benefit confirmed",
			claimed: benefit("g1", "ben-1", "Legendary Skin", winA),
			cand:    benefit("g1", "ben-1", "Skin Legendaire", winA),
			want:    IdentityMatchConfirmed,
		},
		{
			// 9. Different name + no strong ID -> no fuzzy match.
			name:    "different name no strong id no match",
			claimed: nameOnly("g1", "Legendary Skin", winA),
			cand:    nameOnly("g1", "Emote Pack", winA),
			want:    IdentityNoMatch,
		},
		{
			// 10. Missing game id + name only -> ambiguous, retained.
			name:    "missing game name only ambiguous",
			claimed: nameOnly("", "Skin", EntitlementWindow{}),
			cand:    nameOnly("", "Skin", EntitlementWindow{}),
			want:    IdentityMatchAmbiguous,
		},
		{
			// same game + same name only (no strong id) -> ambiguous (never confirmed).
			name:    "same game same name ambiguous",
			claimed: nameOnly("g1", "Skin", EntitlementWindow{}),
			cand:    nameOnly("g1", "Skin", EntitlementWindow{}),
			want:    IdentityMatchAmbiguous,
		},
		{
			// composite same campaign+drop -> confirmed.
			name:    "composite same campaign drop confirmed",
			claimed: composite("g1", "camp-1", "drop-1", "Skin", winA),
			cand:    composite("g1", "camp-1", "drop-1", "Skin", winA),
			want:    IdentityMatchConfirmed,
		},
		{
			// same drop id different campaign -> ambiguous (drop ids not eternal).
			name:    "same drop different campaign ambiguous",
			claimed: composite("g1", "camp-1", "drop-1", "Skin", winA),
			cand:    composite("g1", "camp-2", "drop-1", "Skin", winA),
			want:    IdentityMatchAmbiguous,
		},
		{
			// composite disjoint windows -> no match (new occurrence).
			name:    "composite disjoint windows no match",
			claimed: composite("g1", "camp-1", "drop-1", "Skin", winA),
			cand:    composite("g1", "camp-1", "drop-1", "Skin", winLater),
			want:    IdentityNoMatch,
		},
		{
			// 14. Repeatable reward later occurrence (disjoint window) stays farmable.
			name:    "repeatable later occurrence no match",
			claimed: benefit("g1", "ben-1", "Weekly Coin", winA),
			cand:    benefit("g1", "ben-1", "Weekly Coin", winLater),
			want:    IdentityNoMatch,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MatchIdentity(tc.claimed, tc.cand, clock); got != tc.want {
				t.Fatalf("MatchIdentity = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNewRewardIdentityEvidenceClass(t *testing.T) {
	cases := []struct {
		name string
		id   RewardIdentity
		want IdentityEvidence
	}{
		{"instance", NewRewardIdentity("g", "b", "inst", "d", "c", "n", 0, EntitlementWindow{}), EvidenceInstance},
		{"benefit", NewRewardIdentity("g", "b", "", "d", "c", "n", 0, EntitlementWindow{}), EvidenceBenefit},
		{"composite", NewRewardIdentity("g", "", "", "d", "c", "n", 0, EntitlementWindow{}), EvidenceComposite},
		{"name only", NewRewardIdentity("g", "", "", "", "", "n", 0, EntitlementWindow{}), EvidenceNameOnly},
		{"none", NewRewardIdentity("g", "", "", "", "", "", 0, EntitlementWindow{}), EvidenceNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.id.Evidence != tc.want {
				t.Fatalf("evidence = %v, want %v", tc.id.Evidence, tc.want)
			}
		})
	}
}

// 15. Duplicate claim-history rows dedup deterministically.
func TestDedupeClaimedRewards(t *testing.T) {
	rec := func(benefitID, name string) ClaimedReward {
		return ClaimedReward{Identity: NewRewardIdentity("g1", benefitID, "", "", "", name, 0, EntitlementWindow{})}
	}
	in := []ClaimedReward{
		rec("ben-1", "Skin"),
		rec("ben-1", "Skin"), // exact dup
		rec("", "Emote"),
		rec("", "Emote"), // name dup
		rec("ben-2", "Other"),
	}
	out := DedupeClaimedRewards(in)
	if len(out) != 3 {
		t.Fatalf("expected 3 deduped records, got %d: %+v", len(out), out)
	}
}

// 18. New identity code does not affect Claimability/CanClaim.
func TestIdentityDoesNotAffectClaimability(t *testing.T) {
	d := &Drop{ID: "drop-1", Name: "Skin", BenefitID: "ben-1", MinutesRequired: 60}
	// Not claimable until Twitch mints an instance.
	if d.CanClaim() {
		t.Fatal("drop without instance should not be claimable")
	}
	d.Update(map[string]interface{}{
		"currentMinutesWatched": float64(60),
		"dropInstanceID":        "inst-xyz",
	})
	if d.Claimability != ClaimabilityKnownTrue || !d.CanClaim() {
		t.Fatalf("minted instance should be claimable: claimability=%v canClaim=%v", d.Claimability, d.CanClaim())
	}
	// Building an identity must not mutate claim state.
	_ = d.Identity("g1", "camp-1", EntitlementWindow{})
	if !d.CanClaim() {
		t.Fatal("Identity() must not change CanClaim")
	}
}

// B2: Drop.Identity propagates DropInstanceID as the strongest evidence when
// present, without fabricating it when absent, and distinct instances do not
// confirm sameness.
func TestDropIdentityPropagatesInstanceID(t *testing.T) {
	withInstance := &Drop{ID: "d1", Name: "Skin", DropInstanceID: "inst-abc", MinutesRequired: 60}
	id := withInstance.Identity("g1", "camp-1", EntitlementWindow{})
	if id.InstanceID != "inst-abc" {
		t.Fatalf("Identity did not carry DropInstanceID, got %q", id.InstanceID)
	}
	if id.Evidence != EvidenceInstance {
		t.Fatalf("expected EvidenceInstance, got %v", id.Evidence)
	}

	noInstance := &Drop{ID: "d1", Name: "Skin", MinutesRequired: 60}
	id2 := noInstance.Identity("g1", "camp-1", EntitlementWindow{})
	if id2.InstanceID != "" {
		t.Fatal("empty DropInstanceID must not fabricate an instance id")
	}
	if id2.Evidence == EvidenceInstance {
		t.Fatal("no instance id must not claim instance evidence")
	}

	// Exact same non-empty instance id on both sides confirms, even when the
	// composite (campaign+drop) differs — the instance handle is the strongest
	// evidence.
	same := MatchIdentity(
		NewRewardIdentity("g1", "", "inst-1", "d1", "c1", "Skin", 0, EntitlementWindow{}),
		NewRewardIdentity("g1", "", "inst-1", "d2", "c2", "Skin", 0, EntitlementWindow{}),
		fixedClock(testNow),
	)
	if same != IdentityMatchConfirmed {
		t.Fatal("same instance id must confirm")
	}
	// Different instance ids with no other strong evidence do NOT confirm.
	diff := MatchIdentity(
		NewRewardIdentity("g1", "", "inst-1", "", "", "Skin", 0, EntitlementWindow{}),
		NewRewardIdentity("g1", "", "inst-2", "", "", "Skin", 0, EntitlementWindow{}),
		fixedClock(testNow),
	)
	if diff == IdentityMatchConfirmed {
		t.Fatal("different instance ids (no other strong evidence) must not confirm")
	}
}

// 13. A dateless (inventory) drop is treated as active (window unknown, in list).
func TestDatelessDropWindowUnknownButActive(t *testing.T) {
	d := &Drop{ID: "d1", Name: "Skin", MinutesRequired: 60}
	w := d.Window()
	if w.Known {
		t.Fatal("a dateless drop must have an unknown window")
	}
	if w.State(fixedClock(testNow)) != WindowStateUnknown {
		t.Fatal("unknown window must classify as unknown, not expired")
	}
	// InActiveWindow (legacy helper) still treats it as active.
	if !d.InActiveWindow() {
		t.Fatal("dateless drop should be active in the legacy helper too")
	}
}

// B1: a per-grant instance ID is TERMINAL when present on both sides. Two
// different instances are two different grants and must NEVER be re-confirmed as
// the same reward by weaker benefit/composite/name evidence.
func TestMatchIdentityInstanceTerminal(t *testing.T) {
	clock := fixedClock(testNow)
	win := knownWindow(hoursFromNow(-2), hoursFromNow(2))

	cases := []struct {
		name          string
		claimed, cand RewardIdentity
		want          IdentityMatch
	}{
		{
			// different instance + same benefit + overlapping window -> NoMatch.
			name:    "diff instance same benefit overlapping window",
			claimed: NewRewardIdentity("g1", "ben-1", "inst-a", "", "", "Skin", 0, win),
			cand:    NewRewardIdentity("g1", "ben-1", "inst-b", "", "", "Skin", 0, win),
			want:    IdentityNoMatch,
		},
		{
			// different instance + same campaign/drop -> NoMatch.
			name:    "diff instance same campaign drop",
			claimed: NewRewardIdentity("g1", "", "inst-a", "drop-1", "camp-1", "Skin", 0, win),
			cand:    NewRewardIdentity("g1", "", "inst-b", "drop-1", "camp-1", "Skin", 0, win),
			want:    IdentityNoMatch,
		},
		{
			// different instance + same canonical name -> NoMatch.
			name:    "diff instance same name",
			claimed: NewRewardIdentity("g1", "", "inst-a", "", "", "Skin", 0, EntitlementWindow{}),
			cand:    NewRewardIdentity("g1", "", "inst-b", "", "", "Skin", 0, EntitlementWindow{}),
			want:    IdentityNoMatch,
		},
		{
			// same instance + different weaker fields -> Confirmed.
			name:    "same instance different weaker fields",
			claimed: NewRewardIdentity("g1", "ben-1", "inst-a", "drop-1", "camp-1", "Skin", 0, win),
			cand:    NewRewardIdentity("g1", "ben-2", "inst-a", "drop-9", "camp-9", "Other", 0, EntitlementWindow{}),
			want:    IdentityMatchConfirmed,
		},
		{
			// one-sided instance + same benefit id with known overlapping windows ->
			// decided by the BenefitID/window policy (Confirmed).
			name:    "one-sided instance benefit overlap confirmed",
			claimed: NewRewardIdentity("g1", "ben-1", "", "", "", "Skin", 0, win),
			cand:    NewRewardIdentity("g1", "ben-1", "inst-b", "", "", "Skin", 0, win),
			want:    IdentityMatchConfirmed,
		},
		{
			// one-sided instance + unknown occurrence (name only, no window) -> not
			// Confirmed (Ambiguous — instance alone on one side proves nothing).
			name:    "one-sided instance unknown occurrence ambiguous",
			claimed: NewRewardIdentity("g1", "", "inst-a", "", "", "Skin", 0, EntitlementWindow{}),
			cand:    NewRewardIdentity("g1", "", "", "", "", "Skin", 0, EntitlementWindow{}),
			want:    IdentityMatchAmbiguous,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MatchIdentity(tc.claimed, tc.cand, clock); got != tc.want {
				t.Fatalf("MatchIdentity = %v, want %v", got, tc.want)
			}
		})
	}
}
