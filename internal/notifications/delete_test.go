package notifications

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestDeleteStreamerScrubsRulesAndConfigLists verifies a login's point rules and
// its presence in the three notification-config login-lists are removed (case-
// insensitively), while unrelated streamers are preserved.
func TestDeleteStreamerScrubsRulesAndConfigLists(t *testing.T) {
	r, err := NewRepository(testDBHandle)
	if err != nil {
		t.Fatalf("new repo: %v", err)
	}

	if err := r.AddPointRule(&PointRule{Streamer: "ndel-victim", Threshold: 100}); err != nil {
		t.Fatalf("add victim rule: %v", err)
	}
	if err := r.AddPointRule(&PointRule{Streamer: "ndel-keep", Threshold: 200}); err != nil {
		t.Fatalf("add keep rule: %v", err)
	}
	if err := r.SaveConfig(&NotificationConfig{
		OnlineStreamers:   []string{"ndel-victim", "ndel-keep"},
		OfflineStreamers:  []string{"NDEL-VICTIM"}, // uppercase: must still strip
		MentionsStreamers: []string{"ndel-keep"},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}

	existed, err := r.DeleteStreamer(context.Background(), "ndel-victim")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !existed {
		t.Fatal("delete reported nothing removed")
	}

	rules, _ := r.GetPointRules()
	victimRule, keepRule := false, false
	for _, rule := range rules {
		switch rule.Streamer {
		case "ndel-victim":
			victimRule = true
		case "ndel-keep":
			keepRule = true
		}
	}
	if victimRule {
		t.Error("deleted streamer's point rule survived (the reported notification-history defect)")
	}
	if !keepRule {
		t.Error("unrelated streamer's point rule was removed")
	}

	cfg, _ := r.GetConfig()
	for _, list := range [][]string{cfg.OnlineStreamers, cfg.OfflineStreamers, cfg.MentionsStreamers} {
		for _, name := range list {
			if name == "ndel-victim" || name == "NDEL-VICTIM" {
				t.Errorf("config list still contains deleted streamer: %q", name)
			}
		}
	}
	if len(cfg.OnlineStreamers) != 1 || cfg.OnlineStreamers[0] != "ndel-keep" {
		t.Errorf("unrelated streamer removed from online list: %v", cfg.OnlineStreamers)
	}
	if len(cfg.MentionsStreamers) != 1 || cfg.MentionsStreamers[0] != "ndel-keep" {
		t.Errorf("mentions list wrongly altered: %v", cfg.MentionsStreamers)
	}
}

// TestNotificationTombstoneBlocksAddPointRule (D24): a rule creation racing a
// deletion cannot recreate a notification record for the removed streamer.
func TestNotificationTombstoneBlocksAddPointRule(t *testing.T) {
	r, err := NewRepository(testDBHandle)
	if err != nil {
		t.Fatalf("new repo: %v", err)
	}

	// The point rule this test creates after Reinstate is durable in the
	// package-wide shared testDBHandle, so without removing it here a later
	// -count=N iteration would misread it as a rule created while tombstoned.
	t.Cleanup(func() {
		r.Reinstate("ntomb")
		rules, err := r.GetPointRules()
		if err != nil {
			t.Errorf("cleanup: get point rules: %v", err)
			return
		}
		for _, rule := range rules {
			if strings.EqualFold(rule.Streamer, "ntomb") {
				if err := r.DeletePointRule(rule.ID); err != nil {
					t.Errorf("cleanup: delete point rule %d: %v", rule.ID, err)
				}
			}
		}
	})

	r.Tombstone("Ntomb")
	if err := r.AddPointRule(&PointRule{Streamer: "ntomb", Threshold: 10}); !errors.Is(err, ErrStreamerDeleted) {
		t.Fatalf("AddPointRule while tombstoned: got %v, want ErrStreamerDeleted", err)
	}
	rules, _ := r.GetPointRules()
	for _, rule := range rules {
		if rule.Streamer == "ntomb" {
			t.Fatal("a tombstoned AddPointRule created a rule")
		}
	}

	r.Reinstate("ntomb")
	if err := r.AddPointRule(&PointRule{Streamer: "ntomb", Threshold: 10}); err != nil {
		t.Fatalf("AddPointRule after reinstate: %v", err)
	}
}

// TestRenameStreamerMovesNotificationState (D15/D16): a config-driven rename must
// repoint point rules and config-list membership to the new login, so a later
// deletion by the current login leaves no old-login orphan.
func TestRenameStreamerMovesNotificationState(t *testing.T) {
	r, err := NewRepository(testDBHandle)
	if err != nil {
		t.Fatalf("new repo: %v", err)
	}
	if err := r.AddPointRule(&PointRule{Streamer: "rnold", Threshold: 5}); err != nil {
		t.Fatalf("seed rule: %v", err)
	}
	if err := r.SaveConfig(&NotificationConfig{OnlineStreamers: []string{"rnold", "rnother"}}); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	if err := r.RenameStreamer("rnold", "rnnew"); err != nil {
		t.Fatalf("rename: %v", err)
	}

	rules, _ := r.GetPointRules()
	oldRule, newRule := false, false
	for _, rule := range rules {
		switch rule.Streamer {
		case "rnold":
			oldRule = true
		case "rnnew":
			newRule = true
		}
	}
	if oldRule {
		t.Error("point rule still under old login after rename")
	}
	if !newRule {
		t.Error("point rule not moved to new login")
	}
	cfg, _ := r.GetConfig()
	hasNew, hasOld, hasOther := false, false, false
	for _, s := range cfg.OnlineStreamers {
		switch s {
		case "rnnew":
			hasNew = true
		case "rnold":
			hasOld = true
		case "rnother":
			hasOther = true
		}
	}
	if hasOld || !hasNew || !hasOther {
		t.Errorf("online list after rename = %v, want [rnnew rnother]", cfg.OnlineStreamers)
	}
}
