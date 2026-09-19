package p4offline_test

// THE WORK RULESET CANDIDATES, through the native digest.
//
// The owner's readiness package delivers two candidate P3b rulesets (C01,
// C02) as exact raw bytes with their SHA-256 and an INDEPENDENTLY DERIVED
// native pe-ors-config/v1 digest, and records that the native Go digest was
// NOT RUN on them (work/rulesets/RULESET_VALIDATION.json). This test runs it:
// both candidates must verify through seam 6 against the Work-derived native
// digest, and the native evaluator's own probe must report that digest. It
// binds NOTHING: neither candidate is chosen, ranked, tuned or compared here,
// and the owner's binding of a ruleset stays PENDING.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval/p4offline"
)

type workRulesetValidation struct {
	Candidates []struct {
		CandidateID       string `json:"candidate_id"`
		ConfigID          string `json:"config_id"`
		File              string `json:"file"`
		Bytes             int    `json:"bytes"`
		IndependentNative string `json:"independently_derived_native_config_digest"`
		NativeGoExecution string `json:"native_go_digest_execution"`
		OwnerBinding      string `json:"owner_binding"`
		RawBasis          string `json:"raw_basis"`
		RawBytesSHA256    string `json:"raw_bytes_sha256"`
	} `json:"candidates"`
	ComparativeExecution string `json:"comparative_execution"`
}

// workRulesetValidationSHA256 is the owner's package hash of the record, as
// recorded in work/WORK_PROVENANCE.md; the record is read only once the
// bytes are proven to be the owner's.
const workRulesetValidationSHA256 = "0d931de333a87e7b09bfc0998cb89a4e23746ce3e50dd285efdd180132a38573"

func TestWorkRulesetCandidatesVerifyUnderTheNativeDigest(t *testing.T) {
	raw := readWorkFile(t, "rulesets/RULESET_VALIDATION.json", workRulesetValidationSHA256)
	var val workRulesetValidation
	if err := json.Unmarshal(raw, &val); err != nil {
		t.Fatalf("decode Work ruleset validation: %v", err)
	}
	if len(val.Candidates) != 2 || val.ComparativeExecution != "NOT_RUN" {
		t.Fatalf("the Work package delivers two candidates and no comparison: %+v", val)
	}
	for _, c := range val.Candidates {
		t.Run(c.CandidateID+" "+c.ConfigID, func(t *testing.T) {
			if c.OwnerBinding != "PENDING" || c.NativeGoExecution != "NOT_RUN" || c.RawBasis != "pe-orc-raw/v1" {
				t.Fatalf("the Work record of %s is not what this test was written against: %+v", c.CandidateID, c)
			}
			rawCfg, err := os.ReadFile("testdata/synthetic/work/rulesets/" + c.File)
			if err != nil {
				t.Fatal(err)
			}
			if len(rawCfg) != c.Bytes {
				t.Fatalf("%s is %d bytes, the Work record says %d", c.File, len(rawCfg), c.Bytes)
			}
			sum := sha256.Sum256(rawCfg)
			if got := hex.EncodeToString(sum[:]); got != c.RawBytesSHA256 {
				t.Fatalf("%s hashes to %s, the Work record says %s", c.File, got, c.RawBytesSHA256)
			}
			var cfg predictioneval.OrderedRulesConfig
			if err := json.Unmarshal(rawCfg, &cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.ConfigID != c.ConfigID {
				t.Fatalf("config id %q, the Work record says %q", cfg.ConfigID, c.ConfigID)
			}
			// The NATIVE evaluator's own digest, obtained by the test's own
			// probe (not through p4offline), must be the Work-derived one.
			if got := nativeConfigDigest(t, cfg); got != c.IndependentNative {
				t.Fatalf("the native core digests %s to %s; the Work package derived %s", c.CandidateID, got, c.IndependentNative)
			}
			rs, err := p4offline.VerifyP3bRuleset(p4offline.P3bRuleset{
				RulesetID: c.ConfigID, RawBytes: rawCfg, RawSHA256: c.RawBytesSHA256,
				Config: cfg, NativeConfigDigest: c.IndependentNative,
			})
			if err != nil {
				t.Fatalf("seam 6 refuses %s: %v", c.CandidateID, err)
			}
			if rs.RulesetID != c.ConfigID || rs.RawSHA256 != c.RawBytesSHA256 || rs.NativeConfigDigest != c.IndependentNative {
				t.Fatalf("%+v", rs)
			}
			// A verified candidate evaluates one synthetic factset under
			// the approved entropy, so the whole seam-6 path is exercised
			// on real candidate bytes. The ANSWER is not asserted: this
			// package compares nothing.
			_, fs := selectedFactset(t, nil, nil)
			if _, err := p4offline.EvaluateP3bCase(fs, rs, synthCoords(fs, 0)); err != nil {
				t.Fatalf("a verified candidate must evaluate: %v", err)
			}
		})
	}
}
