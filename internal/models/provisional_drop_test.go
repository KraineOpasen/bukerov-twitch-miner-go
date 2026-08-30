package models

import (
	"testing"
	"time"
)

func testProvisionalCandidate() ProvisionalDropCandidate {
	return ProvisionalDropCandidate{
		CampaignID:           "campaign",
		DropID:               "drop",
		GameID:               "game",
		Login:                "channel",
		ChannelID:            "42",
		BroadcastID:          "broadcast",
		SessionGeneration:    7,
		AvailabilityObs:      3,
		AvailabilityKnownGen: 2,
		DirectoryObs:         2,
		Evidence:             ProvisionalEvidenceDirectory,
	}
}

func TestProvisionalProofIdentitySurvivesOnlyNonAuthoritativeRefresh(t *testing.T) {
	base := testProvisionalCandidate()
	refreshedUnknown := base
	refreshedUnknown.AvailabilityObs++
	refreshedUnknown.DirectoryObs++
	if base.SameLeaseIdentity(refreshedUnknown) {
		t.Fatal("routine observations reused the pre-proof lease identity")
	}
	if !base.SameProofIdentity(refreshedUnknown) {
		t.Fatal("routine non-authoritative observations revoked a positive proof")
	}

	knownEpoch := refreshedUnknown
	knownEpoch.AvailabilityKnownGen++
	if base.SameProofIdentity(knownEpoch) {
		t.Fatal("an intervening authoritative availability epoch retained an old proof")
	}

	changedSession := refreshedUnknown
	changedSession.SessionGeneration++
	if base.SameProofIdentity(changedSession) {
		t.Fatal("a changed playback session retained an old proof")
	}

	changedEvidence := refreshedUnknown
	changedEvidence.Evidence = ProvisionalEvidenceRestrictedACL
	changedEvidence.RestrictedACL = []string{"42"}
	if !base.SameProofIdentity(changedEvidence) {
		t.Fatal("a freshly revalidated evidence envelope revoked the same causal proof target")
	}
}

func TestCampaignAvailabilityKnownEpochFencesProvisionalProof(t *testing.T) {
	s := NewStream()
	firstUnknown := s.BeginCampaignAvailabilityObservation()
	if result := s.ApplyCampaignAvailability(firstUnknown, false, nil, time.Unix(1, 0)); !result.Applied {
		t.Fatalf("initial Unknown was not applied: %+v", result)
	}
	before := s.ProvisionalDropSnapshot().Availability
	if before.KnownGeneration != 0 {
		t.Fatalf("never-known epoch = %d, want 0", before.KnownGeneration)
	}

	knownEmpty := s.BeginCampaignAvailabilityObservation()
	if result := s.ApplyCampaignAvailability(knownEmpty, true, nil, time.Unix(2, 0)); !result.Applied {
		t.Fatalf("Known-empty was not applied: %+v", result)
	}
	afterKnown := s.ProvisionalDropSnapshot().Availability
	if afterKnown.KnownGeneration == before.KnownGeneration {
		t.Fatal("Known-empty did not advance authority epoch")
	}

	laterUnknown := s.BeginCampaignAvailabilityObservation()
	if result := s.ApplyCampaignAvailability(laterUnknown, false, nil, time.Unix(3, 0)); !result.Applied {
		t.Fatalf("later Unknown was not applied: %+v", result)
	}
	afterUnknown := s.ProvisionalDropSnapshot().Availability
	if afterUnknown.KnownGeneration != afterKnown.KnownGeneration {
		t.Fatal("Unknown observation advanced authority epoch")
	}
}

func TestProvisionalDropCandidateValidationAndFences(t *testing.T) {
	base := testProvisionalCandidate()
	if !base.Valid() {
		t.Fatal("complete open-directory candidate is invalid")
	}

	restricted := base
	restricted.Evidence = ProvisionalEvidenceRestrictedACL
	restricted.DirectoryObs = 0
	restricted.RestrictedACL = []string{"42"}
	if !restricted.Valid() || !restricted.Restricted() {
		t.Fatal("complete restricted candidate is invalid")
	}
	restrictedWithDirectory := restricted
	restrictedWithDirectory.DirectoryObs = 1
	if restrictedWithDirectory.Valid() {
		t.Fatal("restricted candidate mixed an unrelated Directory observation into its authority")
	}

	for name, mutate := range map[string]func(*ProvisionalDropCandidate){
		"campaign":     func(p *ProvisionalDropCandidate) { p.CampaignID = "" },
		"drop":         func(p *ProvisionalDropCandidate) { p.DropID = "" },
		"game":         func(p *ProvisionalDropCandidate) { p.GameID = "" },
		"channel":      func(p *ProvisionalDropCandidate) { p.ChannelID = "" },
		"broadcast":    func(p *ProvisionalDropCandidate) { p.BroadcastID = "" },
		"session":      func(p *ProvisionalDropCandidate) { p.SessionGeneration = 0 },
		"availability": func(p *ProvisionalDropCandidate) { p.AvailabilityObs = 0 },
		"directory":    func(p *ProvisionalDropCandidate) { p.DirectoryObs = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := base
			mutate(&candidate)
			if candidate.Valid() {
				t.Fatal("incomplete candidate passed validation")
			}
		})
	}

	changedSession := base
	changedSession.SessionGeneration++
	if base.SameLeaseIdentity(changedSession) {
		t.Fatal("changed playback generation reused a lease identity")
	}
	if base.QuarantineKey() == changedSession.QuarantineKey() {
		t.Fatal("changed playback generation remained quarantined")
	}

	refreshedEvidence := base
	refreshedEvidence.AvailabilityObs++
	refreshedEvidence.DirectoryObs++
	if base.SameLeaseIdentity(refreshedEvidence) {
		t.Fatal("new source observations reused a baseline lease")
	}
	if base.QuarantineKey() != refreshedEvidence.QuarantineKey() {
		t.Fatal("source refresh bypassed a same-session quarantine")
	}
}

func TestProvisionalDropCandidateRequiresCanonicalExactIdentity(t *testing.T) {
	for name, mutate := range map[string]func(*ProvisionalDropCandidate){
		"campaign leading space": func(p *ProvisionalDropCandidate) { p.CampaignID = " campaign" },
		"drop trailing space":    func(p *ProvisionalDropCandidate) { p.DropID = "drop " },
		"game newline":           func(p *ProvisionalDropCandidate) { p.GameID = "game\n" },
		"login tab":              func(p *ProvisionalDropCandidate) { p.Login = "channel\t" },
		"channel leading space":  func(p *ProvisionalDropCandidate) { p.ChannelID = " 42" },
		"broadcast trailing tab": func(p *ProvisionalDropCandidate) { p.BroadcastID = "broadcast\t" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := testProvisionalCandidate()
			mutate(&candidate)
			if candidate.Valid() {
				t.Fatal("non-canonical causal identity passed validation")
			}
		})
	}

	base := testProvisionalCandidate()
	base.Evidence = ProvisionalEvidenceRestrictedACL
	base.DirectoryObs = 0
	base.RestrictedACL = []string{"41", "42"}
	if !base.Valid() {
		t.Fatal("canonical sorted restricted ACL was rejected")
	}
	for _, acl := range [][]string{
		{""},
		{" 42"},
		{"42", "41"},
		{"42", "42"},
	} {
		candidate := base
		candidate.RestrictedACL = acl
		if candidate.Valid() {
			t.Fatalf("non-canonical restricted ACL passed validation: %q", acl)
		}
	}

	labels := testProvisionalCandidate()
	labels.Campaign = " display label "
	labels.Drop = "\treward label\n"
	if !labels.Valid() {
		t.Fatal("non-causal display labels affected exact identity validation")
	}
}

func TestProvisionalDropSnapshotIsCoherentAndCallerOwned(t *testing.T) {
	s := NewStream()
	s.Update("broadcast", "title", &Game{ID: "game"}, nil, 1)
	obs := s.BeginCampaignAvailabilityObservation()
	s.ApplyCampaignAvailability(obs, true, []string{"campaign"}, time.Unix(100, 0))
	s.SetSpadeURL("https://example.invalid/spade")

	snap := s.ProvisionalDropSnapshot()
	if snap.BroadcastID != "broadcast" || snap.GameID != "game" || snap.SessionGeneration == 0 {
		t.Fatalf("incoherent playback snapshot: %+v", snap)
	}
	if snap.Availability.State != CampaignAvailabilityKnown || snap.Availability.ObservationID != obs ||
		len(snap.Availability.CampaignIDs) != 1 || snap.Availability.CampaignIDs[0] != "campaign" {
		t.Fatalf("incoherent availability snapshot: %+v", snap.Availability)
	}

	snap.Availability.CampaignIDs[0] = "mutated"
	if _, ids := s.CampaignAvailability(); len(ids) != 1 || ids[0] != "campaign" {
		t.Fatalf("caller mutated live CampaignIDs through snapshot: %v", ids)
	}
}
