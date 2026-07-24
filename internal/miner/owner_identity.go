package miner

import (
	"strings"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
)

// ownerIdentityResult records what reconcileOwnerIdentity changed on the config
// so the caller can log and persist appropriately.
type ownerIdentityResult struct {
	renamed bool // config.Username adopted a new canonical login (owner rename)
	pinned  bool // config.OwnerUserID was backfilled
}

func (r ownerIdentityResult) changed() bool { return r.renamed || r.pinned }

// reconcileOwnerIdentity reconciles the persisted owner identity after a
// confirmed Login (BKM-006 Corrective Pass 1, C3 + COR-2). It is PURE — it
// mutates only cfg and performs no I/O — so every branch is directly
// unit-testable; the caller (authenticate) owns logging and persistence.
//
//   - canonicalLogin is the account's CURRENT Twitch login from an authoritative
//     validate (auth.GetCanonicalLogin; "" if none has been observed). It is
//     only ever a login that already passed the identity check, so it is never
//     foreign. When it differs (case-insensitively) from cfg.Username the owner
//     was renamed: cfg.ProfileKey is pinned to the pre-rename login FIRST (only
//     while still empty, so the stable storage key chosen at first bind is never
//     moved), then cfg.Username adopts the new canonical login. Storage —
//     everything keyed by cfg.StorageKey() — therefore never follows the mutable
//     Twitch login, while every Twitch-/user-facing use of cfg.Username tracks
//     the new name.
//   - confirmedUserID / userIDConfirmed are the auth session's user ID and
//     whether it was authoritatively confirmed THIS session. The OwnerUserID pin
//     is backfilled once — only when it is empty AND confirmed AND non-empty —
//     never from a merely disk-loaded (unconfirmed) ID, preserving the BKM-005
//     trust boundary.
func reconcileOwnerIdentity(cfg *config.Config, canonicalLogin, confirmedUserID string, userIDConfirmed bool) ownerIdentityResult {
	var res ownerIdentityResult
	if canonicalLogin != "" && !strings.EqualFold(canonicalLogin, cfg.Username) {
		if strings.TrimSpace(cfg.ProfileKey) == "" {
			cfg.ProfileKey = cfg.Username
		}
		cfg.Username = canonicalLogin
		res.renamed = true
	}
	if cfg.OwnerUserID == "" && userIDConfirmed && confirmedUserID != "" {
		cfg.OwnerUserID = confirmedUserID
		res.pinned = true
	}
	return res
}
