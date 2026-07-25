package models

import "testing"

// --- Tri-state account-connection decode (BKM-026 E1-E5) -------------------

func TestParseAccountConnectionTrue(t *testing.T) { // E1
	data := map[string]interface{}{"self": map[string]interface{}{"isAccountConnected": true}}
	if got := ParseAccountConnection(data); got != AccountConnectionConnected {
		t.Fatalf("true must decode as Connected, got %v", got)
	}
}

func TestParseAccountConnectionFalse(t *testing.T) { // E2
	data := map[string]interface{}{"self": map[string]interface{}{"isAccountConnected": false}}
	if got := ParseAccountConnection(data); got != AccountConnectionDisconnected {
		t.Fatalf("false must decode as Disconnected, got %v", got)
	}
}

func TestParseAccountConnectionNull(t *testing.T) { // E3
	// self present, isAccountConnected explicitly null (a nil interface value).
	data := map[string]interface{}{"self": map[string]interface{}{"isAccountConnected": nil}}
	if got := ParseAccountConnection(data); got != AccountConnectionUnknown {
		t.Fatalf("null isAccountConnected must decode as Unknown, got %v", got)
	}
	// self itself null.
	if got := ParseAccountConnection(map[string]interface{}{"self": nil}); got != AccountConnectionUnknown {
		t.Fatalf("null self must decode as Unknown, got %v", got)
	}
}

func TestParseAccountConnectionAbsent(t *testing.T) { // E4
	// isAccountConnected field absent from an otherwise-present self.
	if got := ParseAccountConnection(map[string]interface{}{"self": map[string]interface{}{}}); got != AccountConnectionUnknown {
		t.Fatalf("absent isAccountConnected must decode as Unknown, got %v", got)
	}
	// self absent entirely.
	if got := ParseAccountConnection(map[string]interface{}{}); got != AccountConnectionUnknown {
		t.Fatalf("absent self must decode as Unknown, got %v", got)
	}
	// nil map.
	if got := ParseAccountConnection(nil); got != AccountConnectionUnknown {
		t.Fatalf("nil data must decode as Unknown, got %v", got)
	}
}

func TestParseAccountConnectionMalformedNotDisconnected(t *testing.T) { // E5
	// A malformed optional value must NEVER become a proven disconnection.
	malformed := []interface{}{"false", "true", float64(0), float64(1), 0, "", []interface{}{}, map[string]interface{}{}}
	for _, v := range malformed {
		data := map[string]interface{}{"self": map[string]interface{}{"isAccountConnected": v}}
		if got := ParseAccountConnection(data); got != AccountConnectionUnknown {
			t.Fatalf("malformed isAccountConnected %#v must decode as Unknown (never Disconnected), got %v", v, got)
		}
	}
	// self itself the wrong type.
	if got := ParseAccountConnection(map[string]interface{}{"self": "connected"}); got != AccountConnectionUnknown {
		t.Fatalf("malformed self must decode as Unknown, got %v", got)
	}
}

// --- Benefit type classification (BKM-026 E6-E9, E15) ----------------------

func TestBenefitTypeBadgeNoLink(t *testing.T) { // E6
	if ParseBenefitType("BADGE").RequiresPublisherLink() {
		t.Fatal("BADGE must not require a publisher link")
	}
}

func TestBenefitTypeEmoteNoLink(t *testing.T) { // E7
	if ParseBenefitType("EMOTE").RequiresPublisherLink() {
		t.Fatal("EMOTE must not require a publisher link")
	}
}

func TestBenefitTypeDirectEntitlementRequiresLink(t *testing.T) { // E8
	if !ParseBenefitType("DIRECT_ENTITLEMENT").RequiresPublisherLink() {
		t.Fatal("DIRECT_ENTITLEMENT (a proven linked reward) must require a publisher link")
	}
}

func TestBenefitTypeUnknownFailsOpen(t *testing.T) { // E9
	for _, dt := range []string{"", "SOMETHING_NEW", "direct entitlement", "unknown", "IN_GAME_ITEM"} {
		bt := ParseBenefitType(dt)
		if dt == "" && bt != BenefitTypeUnknown {
			t.Fatalf("empty type must be Unknown, got %v", bt)
		}
		if bt.RequiresPublisherLink() {
			t.Fatalf("unknown/new benefit type %q must fail open (no link required), got type=%v", dt, bt)
		}
	}
}

func TestParseBenefitTypeCaseInsensitive(t *testing.T) {
	cases := map[string]BenefitType{
		"badge":              BenefitTypeBadge,
		" Emote ":            BenefitTypeEmote,
		"direct_entitlement": BenefitTypeDirectEntitlement,
		"DIRECT_ENTITLEMENT": BenefitTypeDirectEntitlement,
	}
	for in, want := range cases {
		if got := ParseBenefitType(in); got != want {
			t.Errorf("ParseBenefitType(%q) = %v, want %v", in, got, want)
		}
	}
}

// --- Normalization wiring (NewCampaignFromGQL / NewDropFromGQL) ------------

func TestNewCampaignFromGQLDecodesAccountConnection(t *testing.T) {
	connected := NewCampaignFromGQL(map[string]interface{}{
		"id": "c1", "self": map[string]interface{}{"isAccountConnected": true},
	})
	if connected.AccountConnection != AccountConnectionConnected {
		t.Errorf("connected campaign: got %v", connected.AccountConnection)
	}
	disc := NewCampaignFromGQL(map[string]interface{}{
		"id": "c2", "self": map[string]interface{}{"isAccountConnected": false},
	})
	if disc.AccountConnection != AccountConnectionDisconnected {
		t.Errorf("disconnected campaign: got %v", disc.AccountConnection)
	}
	// Old/absent shape -> Unknown (backward compatibility, E4/E26).
	old := NewCampaignFromGQL(map[string]interface{}{"id": "c3"})
	if old.AccountConnection != AccountConnectionUnknown {
		t.Errorf("campaign with no self: got %v, want Unknown", old.AccountConnection)
	}
}

func TestNewDropFromGQLDecodesBenefitType(t *testing.T) {
	drop := func(dt string) *Drop {
		return NewDropFromGQL(map[string]interface{}{
			"id": "d", "name": "R",
			"benefitEdges": []interface{}{
				map[string]interface{}{"benefit": map[string]interface{}{
					"name": "Reward", "distributionType": dt,
				}},
			},
		})
	}
	if drop("DIRECT_ENTITLEMENT").BenefitType != BenefitTypeDirectEntitlement {
		t.Error("DIRECT_ENTITLEMENT not decoded on drop")
	}
	if !drop("DIRECT_ENTITLEMENT").RequiresPublisherLink() {
		t.Error("DIRECT_ENTITLEMENT drop must require link")
	}
	if drop("BADGE").RequiresPublisherLink() || drop("EMOTE").RequiresPublisherLink() {
		t.Error("BADGE/EMOTE drop must not require link")
	}
	// E15: a drop with no benefitEdges (missing benefit type) fails open.
	noBenefit := NewDropFromGQL(map[string]interface{}{"id": "d", "name": "R"})
	if noBenefit.BenefitType != BenefitTypeUnknown || noBenefit.RequiresPublisherLink() {
		t.Errorf("missing benefit type must be Unknown and fail open, got type=%v requires=%v",
			noBenefit.BenefitType, noBenefit.RequiresPublisherLink())
	}
	// Malformed benefit edge -> Unknown, never a false requirement.
	malformed := NewDropFromGQL(map[string]interface{}{
		"id": "d", "benefitEdges": []interface{}{"not-a-map"},
	})
	if malformed.RequiresPublisherLink() {
		t.Error("malformed benefit edge must not require link")
	}
}

// E26: cloning and re-decoding never turn Unknown into a proven disconnection.
func TestAccountConnectionCloneAndReparseStable(t *testing.T) {
	// A campaign whose connection is Unknown stays Unknown across Clone.
	c := NewCampaignFromGQL(map[string]interface{}{"id": "c"})
	c.Drops = []*Drop{{ID: "d"}}
	clone := c.Clone()
	if clone.AccountConnection != AccountConnectionUnknown {
		t.Fatalf("clone changed Unknown connection to %v", clone.AccountConnection)
	}
	// A proven Disconnected survives Clone unchanged too.
	d := NewCampaignFromGQL(map[string]interface{}{"id": "c2", "self": map[string]interface{}{"isAccountConnected": false}})
	if d.Clone().AccountConnection != AccountConnectionDisconnected {
		t.Fatal("clone lost a proven Disconnected connection")
	}
}

func TestAccountConnectionStrings(t *testing.T) {
	if AccountConnectionUnknown.String() != "unknown" ||
		AccountConnectionConnected.String() != "connected" ||
		AccountConnectionDisconnected.String() != "disconnected" {
		t.Fatal("AccountConnection.String mismatch")
	}
}
