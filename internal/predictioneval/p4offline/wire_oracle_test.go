package p4offline_test

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval/p4offline"
)

// specFraming implements only PROVENANCE.md's LP(part) rule: an eight-byte
// big-endian byte length followed by the literal part. Its callers spell out
// the documented field order and fixture values, without using a production
// serializer, identity method or framing helper. The digest literals were
// independently calculated with Python struct.pack(">Q", len(part)) and SHA-256.
func specFraming(parts ...string) []byte {
	var out []byte
	for _, part := range parts {
		var prefix [8]byte
		binary.BigEndian.PutUint64(prefix[:], uint64(len(part)))
		out = append(out, prefix[:]...)
		out = append(out, part...)
	}
	return out
}

func checkPreDecisionExitWire(t *testing.T, fs p4offline.CommonFactset) {
	t.Helper()
	// These are the existing TestPreDecisionExitIsCoverageOnly fixture's
	// literal values in PROVENANCE.md's Common factset order. An absent
	// optional settings block or total has a false presence flag and no payload;
	// the non-optional balance/risk values and outcome count remain explicit.
	episode := hex.EncodeToString(specFraming("20260901", "p4-synth-session-a", "pool-a", "r1", "e1"))
	want := specFraming(
		"p4-common-factset/v1", "p4-offline/v1", "predictioneval/v1", episode,
		"20260901", "p4-synth-session-a", "pool-a", "1",
		"auto_decision-AUTO_SKIPPED-2", "2", "PRE_DECISION_EXIT", "0",
		"false", "NOT_ELIGIBLE", "STEALTH_UNKNOWN", // reached decision, exit, stealth
		"false",      // settings absent
		"false", "0", // balance absent, value
		"false", "0", // outcomes absent, count
		"false", "false", // optional bet totals absent
		"false", "0", "0", // risk absent, max stake percent, reserve
		"NOT_REACHED", "", "10", // health, reason, minimum stake
	)
	const wantDigest = "d9e3aef0c62f8cf7e4061a2198d1fdd5c9a62c382a05c0b7b086daba1a07cea2"
	if got := digestOf(want); got != wantDigest {
		t.Fatalf("spec-derived PRE_DECISION_EXIT oracle digest %s, want %s", got, wantDigest)
	}
	got := p4offline.SerializeCommonFactset(fs)
	if !bytes.Equal(got, want) {
		t.Errorf("PRE_DECISION_EXIT framed bytes differ from the independent oracle:\n got  %x\n want %x", got, want)
	}
	if gotDigest := digestOf(got); gotDigest != wantDigest || fs.Digest != wantDigest {
		t.Errorf("PRE_DECISION_EXIT digest: serialized %s, artifact %s, independent oracle %s", gotDigest, fs.Digest, wantDigest)
	}
}

func TestReconcileSourceRoundsOrdersClaimsByFramedAttemptSession(t *testing.T) {
	// All preceding key fields are identical. LP("b") sorts before LP("aa")
	// because its length is smaller, although ordinary byte order is opposite.
	// The identity and digest literals otherwise match the existing registry
	// golden fixture; supplied attempt sessions need not equal the episode's.
	ep := p4offline.EpisodeIdentity{CollectorEpoch: 1, CollectorSessionID: "s-a", PoolInstanceID: "p", RoundIncarnationID: "r", EventID: "e1"}
	short := p4offline.SourceRoundClaim{
		Episode: ep, Attempt: predictioneval.AttemptKey{CollectorEpoch: 1, CollectorSessionID: "b", PoolInstanceID: "p", AttemptID: 1}, FactsetDigest: "d1",
	}
	long := short
	long.Attempt.CollectorSessionID = "aa"

	// Derive both claim keys and the registry from the documented literal
	// field sequence, never from returned entries or production key functions.
	episode := hex.EncodeToString(specFraming("1", "s-a", "p", "r", "e1"))
	shortKey := hex.EncodeToString(specFraming(episode, "1", "b", "p", "1", "d1"))
	longKey := hex.EncodeToString(specFraming(episode, "1", "aa", "p", "1", "d1"))
	want := specFraming("p4-source-round-registry/v1", "1", "e1", "CONFLICT", "2", shortKey, longKey, "false")
	const wantDigest = "686753624d1739dc02db49ae4a815ae4d56f814fd0c3aef76114a44a4ac49f69"
	if got := digestOf(want); got != wantDigest {
		t.Fatalf("spec-derived registry oracle digest %s, want %s", got, wantDigest)
	}
	for _, tc := range []struct {
		name   string
		claims []p4offline.SourceRoundClaim
	}{
		{"short_first", []p4offline.SourceRoundClaim{short, long}},
		{"long_first", []p4offline.SourceRoundClaim{long, short}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := p4offline.ReconcileSourceRounds(tc.claims)
			if reg.Version != "p4-source-round-registry/v1" || len(reg.Entries) != 1 {
				t.Fatalf("unexpected registry: %+v", reg)
			}
			entry := reg.Entries[0]
			if entry.EventID != "e1" || entry.Status != "CONFLICT" || entry.Canonical != nil || len(entry.Claims) != 2 {
				t.Fatalf("expected one conflicted round without a canonical claim: %+v", entry)
			}
			if entry.Claims[0] != short || entry.Claims[1] != long {
				t.Errorf("claim order: got %+v, want attempt sessions [b aa]", entry.Claims)
			}
			if reg.Digest != wantDigest {
				t.Errorf("registry digest %s, independent oracle %s", reg.Digest, wantDigest)
			}
			if err := p4offline.VerifySourceRoundRegistry(reg); err != nil {
				t.Errorf("public reconciliation result does not verify: %v", err)
			}
		})
	}
}
