package eligibility

import (
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// TestAccountLinkEligibilityTruthTable is the authoritative BKM-026 truth table.
// The rule: skip iff AccountConnection == Disconnected AND the reward requires the
// publisher link. Every other combination — including every Unknown and every
// non-link reward — is eligible (fail open).
func TestAccountLinkEligibilityTruthTable(t *testing.T) {
	// requiresLink=true models a DIRECT_ENTITLEMENT ("linked") reward;
	// requiresLink=false models BADGE/EMOTE/unknown/missing reward types.
	cases := []struct {
		name         string
		conn         models.AccountConnection
		requiresLink bool
		wantEligible bool
		wantReason   Reason
	}{
		// 1/10: Connected + linked reward -> eligible.
		{"connected+linked", models.AccountConnectionConnected, true, true, ReasonEligible},
		// 2: Connected + BADGE/EMOTE (non-link) -> eligible.
		{"connected+nonlink", models.AccountConnectionConnected, false, true, ReasonEligible},
		// 4/11: Disconnected + linked reward -> INELIGIBLE.
		{"disconnected+linked", models.AccountConnectionDisconnected, true, false, ReasonAccountLinkRequired},
		// 5/6/13/14: Disconnected + BADGE/EMOTE (non-link) -> eligible.
		{"disconnected+nonlink", models.AccountConnectionDisconnected, false, true, ReasonEligible},
		// 7/12: Unknown + linked reward -> eligible (fail open).
		{"unknown+linked", models.AccountConnectionUnknown, true, true, ReasonEligible},
		// 8/9: Unknown + BADGE/EMOTE -> eligible.
		{"unknown+nonlink", models.AccountConnectionUnknown, false, true, ReasonEligible},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := AccountLinkEligible(tc.conn, tc.requiresLink)
			if d.Eligible != tc.wantEligible {
				t.Errorf("Eligible = %v, want %v", d.Eligible, tc.wantEligible)
			}
			if d.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", d.Reason, tc.wantReason)
			}
			wantState := StateEligible
			if !tc.wantEligible {
				wantState = StateIneligible
			}
			if d.State != wantState {
				t.Errorf("State = %v, want %v", d.State, wantState)
			}
		})
	}
}

// E20: the exclusion reason is the typed, stable code account_link_required, and
// it is produced ONLY on an authoritative Disconnected + link-requiring reward.
func TestAccountLinkReasonTyped(t *testing.T) {
	if ReasonAccountLinkRequired != Reason("account_link_required") {
		t.Fatalf("reason code drifted: %q", ReasonAccountLinkRequired)
	}
	d := AccountLinkEligible(models.AccountConnectionDisconnected, true)
	if d.Reason != ReasonAccountLinkRequired || d.Eligible {
		t.Fatalf("disconnected+linked must be blocked with account_link_required, got %+v", d)
	}
	// Any fail-open path must NOT carry the account-link reason.
	for _, d := range []Decision{
		AccountLinkEligible(models.AccountConnectionUnknown, true),
		AccountLinkEligible(models.AccountConnectionConnected, true),
		AccountLinkEligible(models.AccountConnectionDisconnected, false),
	} {
		if d.Reason == ReasonAccountLinkRequired {
			t.Fatalf("fail-open decision must not carry account_link_required: %+v", d)
		}
	}
}

// E21: the reason is privacy-safe — it carries no account/token/publisher/raw
// payload data. It is a fixed slug with no interpolated identifiers.
func TestAccountLinkReasonPrivacySafe(t *testing.T) {
	r := string(ReasonAccountLinkRequired)
	if r != "account_link_required" {
		t.Fatalf("reason must be a fixed privacy-safe slug, got %q", r)
	}
	for _, banned := range []string{"token", "oauth", "bearer", "http", "://", "user", "id=", "@"} {
		if strings.Contains(strings.ToLower(r), banned) {
			t.Fatalf("reason %q leaks %q", r, banned)
		}
	}
}
