package models

import "testing"

// self builds an inventory `self` object from the given keys; a nil value is
// omitted so a test can model Twitch NOT supplying a field at all (distinct from
// supplying it as false).
func self(fields map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	for k, v := range fields {
		if v == nil {
			continue
		}
		out[k] = v
	}
	return out
}

// TestDropUpdateClaimability is the authoritative-claim-boundary matrix for
// Drops. The invariant under test: claimability is derived ONLY from Twitch's
// authoritative signals (an explicit isClaimable, hasPreconditionsMet, a minted
// dropInstanceID, isClaimed) and NEVER from locally-counted watch minutes.
func TestDropUpdateClaimability(t *testing.T) {
	cases := []struct {
		name            string
		minutesRequired int
		self            map[string]interface{}
		wantClaimab     Claimability
		wantCanClaim    bool
		wantIsClaimable bool
		wantIsClaimed   bool
		wantHasPreNil   bool // HasPreconditionsMet should remain nil (never forced)
	}{
		{
			name:            "local minutes complete but no instance and no flag -> unknown, no claim",
			minutesRequired: 120,
			self:            self(map[string]interface{}{"currentMinutesWatched": float64(120)}),
			wantClaimab:     ClaimabilityUnknown,
			wantCanClaim:    false,
			wantIsClaimable: false,
			wantHasPreNil:   true,
		},
		{
			name:            "local minutes complete + server isClaimable=false -> known false, no claim",
			minutesRequired: 120,
			self: self(map[string]interface{}{
				"currentMinutesWatched": float64(120), "dropInstanceID": "inst-1", "isClaimable": false,
			}),
			wantClaimab:     ClaimabilityKnownFalse,
			wantCanClaim:    false,
			wantIsClaimable: false,
			wantHasPreNil:   true,
		},
		{
			name:            "local minutes complete + missing isClaimable, instance minted -> known true, claim",
			minutesRequired: 120,
			self: self(map[string]interface{}{
				"currentMinutesWatched": float64(120), "dropInstanceID": "inst-1",
			}),
			wantClaimab:     ClaimabilityKnownTrue,
			wantCanClaim:    true,
			wantIsClaimable: true,
			wantHasPreNil:   true,
		},
		{
			name:            "explicit isClaimable=true + valid instance -> known true, claim",
			minutesRequired: 120,
			self: self(map[string]interface{}{
				"currentMinutesWatched": float64(50), "dropInstanceID": "inst-1", "isClaimable": true,
			}),
			wantClaimab:     ClaimabilityKnownTrue,
			wantCanClaim:    true,
			wantIsClaimable: true,
			wantHasPreNil:   true,
		},
		{
			name:            "explicit isClaimable=true + missing instance -> known true but NOT claimable",
			minutesRequired: 120,
			self: self(map[string]interface{}{
				"currentMinutesWatched": float64(120), "isClaimable": true,
			}),
			wantClaimab:     ClaimabilityKnownTrue,
			wantCanClaim:    false, // no dropInstanceID to submit
			wantIsClaimable: true,
			wantHasPreNil:   true,
		},
		{
			name:            "explicit isClaimable=false + valid instance -> known false, no claim",
			minutesRequired: 120,
			self: self(map[string]interface{}{
				"dropInstanceID": "inst-1", "isClaimable": false,
			}),
			wantClaimab:     ClaimabilityKnownFalse,
			wantCanClaim:    false,
			wantIsClaimable: false,
			wantHasPreNil:   true,
		},
		{
			name:            "hasPreconditionsMet=false blocks even with a minted instance",
			minutesRequired: 120,
			self: self(map[string]interface{}{
				"currentMinutesWatched": float64(120), "dropInstanceID": "inst-1", "hasPreconditionsMet": false,
			}),
			wantClaimab:     ClaimabilityKnownFalse,
			wantCanClaim:    false,
			wantIsClaimable: false,
		},
		{
			name:            "hasPreconditionsMet=true does not block; instance -> known true",
			minutesRequired: 120,
			self: self(map[string]interface{}{
				"dropInstanceID": "inst-1", "hasPreconditionsMet": true,
			}),
			wantClaimab:     ClaimabilityKnownTrue,
			wantCanClaim:    true,
			wantIsClaimable: true,
		},
		{
			name:            "missing hasPreconditionsMet stays nil (never forced false)",
			minutesRequired: 120,
			self: self(map[string]interface{}{
				"dropInstanceID": "inst-1",
			}),
			wantClaimab:     ClaimabilityKnownTrue,
			wantCanClaim:    true,
			wantIsClaimable: true,
			wantHasPreNil:   true,
		},
		{
			name:            "already claimed -> known false, no claim",
			minutesRequired: 120,
			self: self(map[string]interface{}{
				"currentMinutesWatched": float64(120), "dropInstanceID": "inst-1", "isClaimed": true,
			}),
			wantClaimab:     ClaimabilityKnownFalse,
			wantCanClaim:    false,
			wantIsClaimable: false,
			wantIsClaimed:   true,
			wantHasPreNil:   true,
		},
		{
			name:            "server false wins over local 100% progress",
			minutesRequired: 120,
			self: self(map[string]interface{}{
				"currentMinutesWatched": float64(200), "dropInstanceID": "inst-1", "isClaimable": false,
			}),
			wantClaimab:     ClaimabilityKnownFalse,
			wantCanClaim:    false,
			wantIsClaimable: false,
			wantHasPreNil:   true,
		},
		{
			name:            "server unknown wins over local 100% progress",
			minutesRequired: 120,
			self:            self(map[string]interface{}{"currentMinutesWatched": float64(240)}),
			wantClaimab:     ClaimabilityUnknown,
			wantCanClaim:    false,
			wantIsClaimable: false,
			wantHasPreNil:   true,
		},
		{
			name:            "server true wins even when local minutes are below required",
			minutesRequired: 120,
			self: self(map[string]interface{}{
				"currentMinutesWatched": float64(5), "dropInstanceID": "inst-1", "isClaimable": true,
			}),
			wantClaimab:     ClaimabilityKnownTrue,
			wantCanClaim:    true,
			wantIsClaimable: true,
			wantHasPreNil:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &Drop{MinutesRequired: tc.minutesRequired}
			d.Update(tc.self)

			if d.Claimability != tc.wantClaimab {
				t.Errorf("Claimability = %v, want %v", d.Claimability, tc.wantClaimab)
			}
			if got := d.CanClaim(); got != tc.wantCanClaim {
				t.Errorf("CanClaim() = %v, want %v", got, tc.wantCanClaim)
			}
			if d.IsClaimable != tc.wantIsClaimable {
				t.Errorf("IsClaimable = %v, want %v", d.IsClaimable, tc.wantIsClaimable)
			}
			if d.IsClaimed != tc.wantIsClaimed {
				t.Errorf("IsClaimed = %v, want %v", d.IsClaimed, tc.wantIsClaimed)
			}
			if tc.wantHasPreNil && d.HasPreconditionsMet != nil {
				t.Errorf("HasPreconditionsMet should stay nil when Twitch omits it, got %v", *d.HasPreconditionsMet)
			}
			// IsClaimable must always mirror ClaimabilityKnownTrue.
			if d.IsClaimable != (d.Claimability == ClaimabilityKnownTrue) {
				t.Errorf("IsClaimable (%v) must mirror ClaimabilityKnownTrue", d.IsClaimable)
			}
		})
	}
}

// TestDropUpdateIdempotentReconciliation confirms repeated inventory syncs are
// idempotent: once Twitch reports the drop claimed, it stays known-false and
// never becomes claimable again on a later sync.
func TestDropUpdateIdempotentReconciliation(t *testing.T) {
	d := &Drop{MinutesRequired: 60}

	// First sync: claimable (minted instance, unclaimed).
	d.Update(self(map[string]interface{}{
		"currentMinutesWatched": float64(60), "dropInstanceID": "inst-1",
	}))
	if !d.CanClaim() {
		t.Fatal("drop with a minted instance should be claimable on the first sync")
	}

	// Second sync: Twitch now reports it claimed.
	d.Update(self(map[string]interface{}{
		"currentMinutesWatched": float64(60), "dropInstanceID": "inst-1", "isClaimed": true,
	}))
	if d.CanClaim() || d.IsClaimable || d.Claimability != ClaimabilityKnownFalse {
		t.Fatalf("claimed drop must be known-false and not claimable, got claimab=%v canClaim=%v",
			d.Claimability, d.CanClaim())
	}

	// Third sync (repeat): still not claimable — no oscillation.
	d.Update(self(map[string]interface{}{
		"currentMinutesWatched": float64(60), "dropInstanceID": "inst-1", "isClaimed": true,
	}))
	if d.CanClaim() {
		t.Fatal("repeated sync of a claimed drop must remain non-claimable (idempotent)")
	}
}

// TestDropUpdateRetainsAbsentAuthoritativeFields documents Drop.Update's
// set-if-present (snapshot-retention) semantics: an authoritative field absent
// from a later selfData keeps its prior value. Twitch's inventory `self` is a
// full snapshot (a minted dropInstanceID is not dropped mid-progress), so this
// retention is benign; and it is safe against a wrongful claim ONLY because every
// active-claim path rebuilds a FRESH Drop per pass — proven by
// drops.TestClaimPathBuildsFreshInstancelessDrops,
// drops.TestFullSyncNoClaimWithoutInstanceID, and, for the single object-reuse
// path, drops.TestLightweightProgressSyncNeverClaims. This test pins the
// retention behavior so a future in-place-reuse refactor cannot silently
// reintroduce a stale-field claim.
func TestDropUpdateRetainsAbsentAuthoritativeFields(t *testing.T) {
	// (1) dropInstanceID is retained when a later snapshot omits it.
	d := &Drop{MinutesRequired: 60}
	d.Update(self(map[string]interface{}{"currentMinutesWatched": float64(60), "dropInstanceID": "i1"}))
	if d.DropInstanceID != "i1" || !d.CanClaim() {
		t.Fatalf("setup: minted instance must be claimable, got instance=%q canClaim=%v", d.DropInstanceID, d.CanClaim())
	}
	d.Update(self(map[string]interface{}{"currentMinutesWatched": float64(60)})) // dropInstanceID absent
	if d.DropInstanceID != "i1" {
		t.Errorf("Update must retain an absent dropInstanceID (set-if-present semantics), got %q", d.DropInstanceID)
	}

	// (2) isClaimed is monotonic-safe: retained true across an absent isClaimed,
	// so a claimed drop can never flip back to claimable via an omitted field.
	d2 := &Drop{MinutesRequired: 60}
	d2.Update(self(map[string]interface{}{"dropInstanceID": "i1", "isClaimed": true}))
	d2.Update(self(map[string]interface{}{"dropInstanceID": "i1"})) // isClaimed absent
	if !d2.IsClaimed {
		t.Error("Update must retain isClaimed=true across an absent isClaimed (monotonic)")
	}
	if d2.CanClaim() {
		t.Error("a claimed drop must stay non-claimable across a later snapshot")
	}

	// (3) hasPreconditionsMet=false is retained (fail-closed) across an absent field.
	d3 := &Drop{MinutesRequired: 60}
	d3.Update(self(map[string]interface{}{"dropInstanceID": "i1", "hasPreconditionsMet": false}))
	if d3.HasPreconditionsMet == nil || *d3.HasPreconditionsMet || d3.CanClaim() {
		t.Fatal("setup: hasPreconditionsMet=false must block the claim")
	}
	d3.Update(self(map[string]interface{}{"dropInstanceID": "i1"})) // hasPreconditionsMet absent
	if d3.HasPreconditionsMet == nil || *d3.HasPreconditionsMet {
		t.Error("Update must retain hasPreconditionsMet=false across an absent field (fail-closed)")
	}
	if d3.CanClaim() {
		t.Error("retained hasPreconditionsMet=false must keep the drop non-claimable")
	}
}
