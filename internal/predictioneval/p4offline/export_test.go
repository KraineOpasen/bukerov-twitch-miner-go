package p4offline

import "github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"

// Test-only windows onto values the package deliberately does not export.
//
// The raw-ruleset ceiling is derived rather than constant, and the test that
// pins it has to name the same number the code uses. Recomputing the formula in
// the test would make the assertion tautological -- it would agree with the
// code by construction and could never disagree with it -- so the code's own
// value is exposed here instead, to the test binary only. Nothing in the
// package's exported API changes: an earlier draft DID export the ceiling as a
// constant, and exporting the wrong number is how this file came to exist.

// RulesetRawCeilingForTest is rulesetRawCeiling.
func RulesetRawCeilingForTest(cfg predictioneval.OrderedRulesConfig) int {
	return rulesetRawCeiling(cfg)
}

// RulesetStructuralAllowanceForTest is rulesetStructuralAllowance.
const RulesetStructuralAllowanceForTest = rulesetStructuralAllowance
