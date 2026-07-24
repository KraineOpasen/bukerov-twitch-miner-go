package config

import "testing"

// TestStorageKey_COR2 pins the stable-storage-key semantics (BKM-006 Corrective
// Pass 1, COR-2): StorageKey() is ProfileKey when set (trimmed non-empty),
// otherwise Username — so legacy configs and installs before the first owner
// rename keep their existing cookies/db/log paths, while a renamed owner keeps
// storage pinned even though Username has moved to the new canonical login.
func TestStorageKey_COR2(t *testing.T) {
	// Legacy / pre-rename: no ProfileKey → falls back to Username.
	c := &Config{Username: "alice"}
	if got := c.StorageKey(); got != "alice" {
		t.Errorf("empty ProfileKey: StorageKey()=%q, want alice", got)
	}
	// Whitespace-only ProfileKey is treated as empty.
	c.ProfileKey = "   "
	if got := c.StorageKey(); got != "alice" {
		t.Errorf("whitespace ProfileKey: StorageKey()=%q, want alice", got)
	}
	// Pinned ProfileKey wins even after Username moves to the new login.
	c.ProfileKey = "alice"
	c.Username = "alice_renamed"
	if got := c.StorageKey(); got != "alice" {
		t.Errorf("pinned ProfileKey: StorageKey()=%q, want alice (storage must not follow the login)", got)
	}
}
