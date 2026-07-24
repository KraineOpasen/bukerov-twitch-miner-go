package miner

import (
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
)

// BKM-006 Corrective Pass 1, COR-2 — reconcileOwnerIdentity separates the three
// identity concepts: an immutable local storage key (ProfileKey/StorageKey), a
// stable OwnerUserID pin, and the mutable canonical Twitch login (Username).

func TestReconcileOwnerIdentity_OwnerRename_AdoptsCanonicalLogin_PinsStorage_COR2(t *testing.T) {
	cfg := &config.Config{Username: "oldlogin", OwnerUserID: "uid-123"}
	res := reconcileOwnerIdentity(cfg, "newlogin", "uid-123", true)

	if !res.renamed || !res.changed() {
		t.Fatalf("expected a rename change, got %+v", res)
	}
	if cfg.Username != "newlogin" {
		t.Errorf("Username=%q, want newlogin (Twitch-/user-facing must track the new login)", cfg.Username)
	}
	if cfg.ProfileKey != "oldlogin" {
		t.Errorf("ProfileKey=%q, want oldlogin (storage pinned to the pre-rename login)", cfg.ProfileKey)
	}
	if got := cfg.StorageKey(); got != "oldlogin" {
		t.Errorf("StorageKey()=%q, want oldlogin (storage must NOT follow the mutable login)", got)
	}
}

func TestReconcileOwnerIdentity_CaseOnlyDifference_NotARename_COR2(t *testing.T) {
	cfg := &config.Config{Username: "samelogin", OwnerUserID: "uid-1"}
	res := reconcileOwnerIdentity(cfg, "SameLogin", "uid-1", true)
	if res.renamed || res.changed() {
		t.Error("a case-only login difference must not be treated as a rename")
	}
	if cfg.Username != "samelogin" || cfg.ProfileKey != "" {
		t.Errorf("unexpected mutation: Username=%q ProfileKey=%q", cfg.Username, cfg.ProfileKey)
	}
}

func TestReconcileOwnerIdentity_PinBackfill_TrustBoundary_COR2(t *testing.T) {
	// Empty pin + confirmed identity → backfill.
	cfg := &config.Config{Username: "u", OwnerUserID: ""}
	if res := reconcileOwnerIdentity(cfg, "u", "uid-9", true); !res.pinned || cfg.OwnerUserID != "uid-9" {
		t.Errorf("expected pin backfill, got res=%+v OwnerUserID=%q", res, cfg.OwnerUserID)
	}
	// Existing pin must never be overwritten (even by a different confirmed ID).
	cfg2 := &config.Config{Username: "u", OwnerUserID: "uid-existing"}
	if res := reconcileOwnerIdentity(cfg2, "u", "uid-different", true); res.pinned || cfg2.OwnerUserID != "uid-existing" {
		t.Error("an existing OwnerUserID pin must never be overwritten")
	}
	// Unconfirmed (disk-loaded) userID must NEVER backfill the pin (BKM-005).
	cfg3 := &config.Config{Username: "u", OwnerUserID: ""}
	if res := reconcileOwnerIdentity(cfg3, "u", "uid-disk", false); res.pinned || cfg3.OwnerUserID != "" {
		t.Error("an unconfirmed userID must never backfill the owner pin")
	}
}

// TestReconcileOwnerIdentity_TwoRestarts_StorageStable_COR2 is the two-restart
// proof: after an owner rename adopts the new canonical login and pins storage,
// a SECOND restart on the already-updated config is a no-op and the storage key
// (cookies/db/logs location) is byte-identical across both restarts — so
// credentials keep loading with no fresh Device Flow, while Username stays the
// new canonical login for all Twitch-/user-facing operations.
func TestReconcileOwnerIdentity_TwoRestarts_StorageStable_COR2(t *testing.T) {
	// Restart 1: owner renamed oldlogin -> newlogin; pin already established.
	cfg := &config.Config{Username: "oldlogin", OwnerUserID: "uid-123"}
	_ = reconcileOwnerIdentity(cfg, "newlogin", "uid-123", true)
	if cfg.Username != "newlogin" || cfg.ProfileKey != "oldlogin" {
		t.Fatalf("after restart 1: Username=%q ProfileKey=%q, want newlogin/oldlogin", cfg.Username, cfg.ProfileKey)
	}
	storageAfterRestart1 := cfg.StorageKey()
	if storageAfterRestart1 != "oldlogin" {
		t.Fatalf("restart-1 storage key = %q, want oldlogin", storageAfterRestart1)
	}

	// Restart 2: the persisted config reloads; validate again reports the same
	// canonical login for the same pinned user ID. No further change.
	res2 := reconcileOwnerIdentity(cfg, "newlogin", "uid-123", true)
	if res2.changed() {
		t.Error("the second restart must be a no-op (config already canonical)")
	}
	if cfg.StorageKey() != storageAfterRestart1 {
		t.Errorf("storage key drifted across restarts: %q -> %q (credentials would relocate)",
			storageAfterRestart1, cfg.StorageKey())
	}
	if cfg.Username != "newlogin" {
		t.Errorf("canonical login not retained across restart 2: %q", cfg.Username)
	}
}
