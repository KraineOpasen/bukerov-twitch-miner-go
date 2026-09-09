package predictioneval

import "math"

// The pinned policy, re-derived.
//
// This file re-implements the decision arithmetic of internal/models/bet.go at
// [PolicyRevision]. It is a DELIBERATE second implementation, for two reasons
// that both matter:
//
//   - the production Calculate consumes the global RNG under stealth mode, and
//     a replay that called it would be drawing a NEW random number and then
//     presenting it as the one the original decision used. Calling production
//     here is therefore not "more faithful", it is the specific mistake this
//     package must not make.
//   - production Calculate MUTATES the Bet it is called on. A replay must be a
//     function of its inputs.
//
// A second copy of an algorithm is worth nothing as its own oracle, so it is
// not used as one: the parity tests drive the REAL models package over the
// deterministic (non-stealth) domain and require byte-equal agreement, and the
// property tests generate inputs rather than reusing the examples. This file
// is the thing under test, never the thing that certifies itself.
//
// Fidelity notes that are easy to get wrong and are pinned by tests:
//
//   - returnChoice compares with STRICT >, so a tie keeps the LOWEST index.
//   - SMART compares the gap with STRICT <, so difference == gap takes the
//     total-users branch, not the odds branch.
//   - Skip's switch has NO default: an unrecognised comparison operator falls
//     through to "skip". Likewise Calculate's switch has no default, so an
//     unrecognised strategy leaves the choice at -1. Both are why a value the
//     store recorded as UNKNOWN still replays exactly: every string outside
//     the closed vocabulary takes the same branch as every other.
//   - getOutcomeValue returns 0 for an index at or past the end, but INDEXES
//     the slice for a negative one. That is a real panic in the pinned policy,
//     and it is modelled here as a typed failure rather than reproduced.

// Strategy, condition and outcome-key spellings of the pinned policy. They are
// re-declared rather than imported for the reasons in records.go, and pinned
// against internal/models by TestPolicyVocabularyMatchesTheDomain.
const (
	StrategyMostVoted  = "MOST_VOTED"
	StrategyHighOdds   = "HIGH_ODDS"
	StrategyPercentage = "PERCENTAGE"
	StrategySmartMoney = "SMART_MONEY"
	StrategySmart      = "SMART"
	StrategyNumber1    = "NUMBER_1"
	StrategyNumber2    = "NUMBER_2"
	StrategyNumber3    = "NUMBER_3"
	StrategyNumber4    = "NUMBER_4"
	StrategyNumber5    = "NUMBER_5"
	StrategyNumber6    = "NUMBER_6"
	StrategyNumber7    = "NUMBER_7"
	StrategyNumber8    = "NUMBER_8"

	ConditionGT  = "GT"
	ConditionLT  = "LT"
	ConditionGTE = "GTE"
	ConditionLTE = "LTE"

	OutcomePercentageUsers = "percentage_users"
	OutcomeOddsPercentage  = "odds_percentage"
	OutcomeOdds            = "odds"
	OutcomeTopPoints       = "top_points"
	OutcomeTotalUsers      = "total_users"
	OutcomeTotalPoints     = "total_points"
	OutcomeDecisionUsers   = "decision_users"
	OutcomeDecisionPoints  = "decision_points"

	GateNone             = ""
	GatePercent          = "max_stake_percent"
	GateReserveViolation = "reserve_violation"
)

// SupportedStrategies is every strategy the pinned policy dispatches on. A
// strategy outside it is not "unsupported input" — the pinned policy has no
// default branch, so it deterministically chooses nothing, and this model
// reproduces that rather than refusing the case.
func SupportedStrategies() []string {
	return []string{
		StrategyMostVoted, StrategyHighOdds, StrategyPercentage, StrategySmartMoney,
		StrategySmart,
		StrategyNumber1, StrategyNumber2, StrategyNumber3, StrategyNumber4,
		StrategyNumber5, StrategyNumber6, StrategyNumber7, StrategyNumber8,
	}
}

// LegacyFailure names a way the PINNED POLICY ITSELF fails on an input, as
// opposed to a way this model cannot read one.
//
// The pinned policy panics on these. This model does not: a replay that
// crashed the reader would destroy every other case in the batch, and a replay
// that silently substituted a value would report a decision the policy could
// never have produced. Both are worse than saying exactly which panic the
// input reproduces, so that is what happens. Nothing here is a production fix;
// production is untouched and still panics.
type LegacyFailure string

const (
	// LegacyFailureNone means the policy completed.
	LegacyFailureNone LegacyFailure = ""

	// LegacyFailureNegativeChoiceIndex is bet.go's Skip() indexing the outcome
	// slice with the -1 that Calculate leaves behind when no strategy branch
	// chose anything.
	//
	// It is reachable: SMART with fewer than two outcomes, or any strategy
	// string outside the closed set, leaves the choice at -1, and any filter
	// key other than total_users/total_points then reads
	// getOutcomeValue(-1, ...) — whose guard only rejects indexes at or PAST
	// the end, never negative ones. Confirmed by
	// TestNegativeChoiceIndexPanicsThePinnedPolicy, which drives the real
	// models package and recovers the panic.
	LegacyFailureNegativeChoiceIndex LegacyFailure = "PINNED_POLICY_PANIC_NEGATIVE_CHOICE_INDEX"

	// LegacyFailureAbsentOutcome is a nil entry in the model's outcome vector
	// being dereferenced. The pinned producer never writes one, so this is not
	// reachable from a stored obs-v2 fact; it is modelled because the stored
	// shape can express it and a hole must never be read as a zero-point
	// outcome.
	LegacyFailureAbsentOutcome LegacyFailure = "PINNED_POLICY_PANIC_ABSENT_OUTCOME"
)

// policyOutcome is one outcome as the pinned policy sees it: in int, because
// models.Outcome counts in int and the stake arithmetic depends on that width.
type policyOutcome struct {
	present bool
	id      string

	totalUsers  int
	totalPoints int
	topPoints   int

	percentageUsers float64
	odds            float64
	oddsPercentage  float64
}

// outcomeValue mirrors (*models.Bet).getOutcomeValue exactly, including both
// of its edge behaviours: an index at or past the end returns 0 WITHOUT
// touching the slice, and any key outside the closed set returns 0 through the
// switch's default.
//
// The one thing it does not mirror is the panic. A negative index and an
// absent outcome are returned as typed failures instead.
func outcomeValue(outs []policyOutcome, index int, key string) (float64, LegacyFailure) {
	if index >= len(outs) {
		return 0, LegacyFailureNone
	}
	if index < 0 {
		return 0, LegacyFailureNegativeChoiceIndex
	}
	o := outs[index]
	if !o.present {
		return 0, LegacyFailureAbsentOutcome
	}
	switch key {
	case OutcomePercentageUsers:
		return o.percentageUsers, LegacyFailureNone
	case OutcomeOddsPercentage:
		return o.oddsPercentage, LegacyFailureNone
	case OutcomeOdds:
		return o.odds, LegacyFailureNone
	case OutcomeTopPoints:
		return float64(o.topPoints), LegacyFailureNone
	case OutcomeTotalUsers:
		return float64(o.totalUsers), LegacyFailureNone
	case OutcomeTotalPoints:
		return float64(o.totalPoints), LegacyFailureNone
	default:
		return 0, LegacyFailureNone
	}
}

// returnChoice mirrors (*models.Bet).returnChoice: argmax with a STRICT
// comparison, so the FIRST index wins a tie, and an empty vector answers 0
// without reading anything.
func returnChoice(outs []policyOutcome, key string) (int, LegacyFailure) {
	largest := 0
	for i := 1; i < len(outs); i++ {
		vi, fail := outcomeValue(outs, i, key)
		if fail != LegacyFailureNone {
			return 0, fail
		}
		vl, fail := outcomeValue(outs, largest, key)
		if fail != LegacyFailureNone {
			return 0, fail
		}
		if vi > vl {
			largest = i
		}
	}
	return largest, LegacyFailureNone
}

// returnNumberChoice mirrors (*models.Bet).returnNumberChoice: the requested
// slot when the vector is long enough, and slot 0 otherwise — NOT "no choice".
func returnNumberChoice(outs []policyOutcome, number int) int {
	if len(outs) > number {
		return number
	}
	return 0
}

// policyChoice re-derives the strategy's chosen index, and only the index.
//
// It stops exactly where the pinned policy stops: the choice is a function of
// the outcome vector and the strategy alone, with no balance and no stake
// involved. -1 means the policy chose nothing.
func policyChoice(outs []policyOutcome, strategy string, percentageGap int) (int, LegacyFailure) {
	switch strategy {
	case StrategyMostVoted:
		return returnChoice(outs, OutcomeTotalUsers)
	case StrategyHighOdds:
		return returnChoice(outs, OutcomeOdds)
	case StrategyPercentage:
		return returnChoice(outs, OutcomeOddsPercentage)
	case StrategySmartMoney:
		return returnChoice(outs, OutcomeTopPoints)
	case StrategyNumber1:
		return returnNumberChoice(outs, 0), LegacyFailureNone
	case StrategyNumber2:
		return returnNumberChoice(outs, 1), LegacyFailureNone
	case StrategyNumber3:
		return returnNumberChoice(outs, 2), LegacyFailureNone
	case StrategyNumber4:
		return returnNumberChoice(outs, 3), LegacyFailureNone
	case StrategyNumber5:
		return returnNumberChoice(outs, 4), LegacyFailureNone
	case StrategyNumber6:
		return returnNumberChoice(outs, 5), LegacyFailureNone
	case StrategyNumber7:
		return returnNumberChoice(outs, 6), LegacyFailureNone
	case StrategyNumber8:
		return returnNumberChoice(outs, 7), LegacyFailureNone
	case StrategySmart:
		// SMART needs two outcomes to compare and leaves the choice at -1
		// otherwise. It reads slots 0 and 1 DIRECTLY rather than through
		// getOutcomeValue, so an absent outcome there is dereferenced before
		// any guard can see it.
		if len(outs) >= 2 {
			if !outs[0].present || !outs[1].present {
				return -1, LegacyFailureAbsentOutcome
			}
			difference := math.Abs(outs[0].percentageUsers - outs[1].percentageUsers)
			// STRICT <: an exactly-equal gap takes the total-users branch.
			if difference < float64(percentageGap) {
				return returnChoice(outs, OutcomeOdds)
			}
			return returnChoice(outs, OutcomeTotalUsers)
		}
		return -1, LegacyFailureNone
	default:
		// The pinned switch has no default, so the choice stays at its
		// initial -1. Every strategy string outside the closed set — including
		// one the store recorded as UNKNOWN, and one differing only in case —
		// lands here, which is why an UNKNOWN strategy still replays exactly.
		return -1, LegacyFailureNone
	}
}

// baseStake re-derives the pre-stealth stake: the configured percentage of the
// balance, truncated toward zero by the int conversion, then capped by
// MaxPoints.
//
// representable is false when the float product cannot be converted to int on
// this machine. Go leaves that conversion implementation-defined, so the model
// refuses it instead of recording whatever this particular build happens to
// produce. The bound is exact rather than conservative: every float64 strictly
// below 2^63 converts exactly, and -2^63 is itself representable.
func baseStake(balance, percentage, maxPoints int) (stake int, representable bool) {
	product := float64(balance) * (float64(percentage) / 100)
	if math.IsNaN(product) || product < float64(math.MinInt) || product >= -float64(math.MinInt) {
		return 0, false
	}
	amount := int(product)
	// The absolute cap is applied here and NOT again in the stake gate: the
	// pinned policy enforces MaxPoints as the single per-streamer source.
	if amount > maxPoints {
		amount = maxPoints
	}
	return amount, true
}

// stealthApplies mirrors the pinned policy's stealth condition: stealth mode is
// on AND the pre-stealth stake is at least the chosen outcome's top single
// stake. The comparison uses the PRE-stealth amount.
func stealthApplies(stealthMode bool, preStealthStake, topPoints int) bool {
	return stealthMode && preStealthStake >= topPoints
}

// Stealth draw bounds. The pinned policy computes
// `rand.Float64()*4 + 1` and truncates it, so the subtracted amount is one of
// exactly four integers; the realized stake is topPoints minus one of them.
const (
	stealthMinReduction = 1
	stealthMaxReduction = 4
)

// policySkip mirrors (*models.Bet).Skip, including its two structural
// subtleties: the decision_* keys are REMAPPED to their total_* accessor but
// still take the chosen-outcome branch, and the comparison switch has no
// default, so an unrecognised operator skips.
func policySkip(outs []policyOutcome, choice int, fc *SourceFilterCondition) (skip bool, compared float64, fail LegacyFailure) {
	if fc == nil {
		// No condition is a real value: the policy answers "do not skip"
		// immediately and never looks at an outcome.
		return false, 0, LegacyFailureNone
	}

	key := fc.By
	fixedKey := key
	switch key {
	case OutcomeDecisionUsers:
		fixedKey = OutcomeTotalUsers
	case OutcomeDecisionPoints:
		fixedKey = OutcomeTotalPoints
	}

	// The branch is chosen on the ORIGINAL key, not the remapped one. That is
	// what makes decision_users read the chosen outcome while total_users
	// reads the first two summed.
	if key == OutcomeTotalUsers || key == OutcomeTotalPoints {
		if len(outs) >= 2 {
			v0, f := outcomeValue(outs, 0, fixedKey)
			if f != LegacyFailureNone {
				return false, 0, f
			}
			v1, f := outcomeValue(outs, 1, fixedKey)
			if f != LegacyFailureNone {
				return false, 0, f
			}
			compared = v0 + v1
		}
		// Fewer than two outcomes leaves compared at 0 — the policy's own
		// answer, not a substitution.
	} else {
		v, f := outcomeValue(outs, choice, fixedKey)
		if f != LegacyFailureNone {
			return false, 0, f
		}
		compared = v
	}

	switch fc.Where {
	case ConditionGT:
		if compared > fc.Value {
			return false, compared, LegacyFailureNone
		}
	case ConditionLT:
		if compared < fc.Value {
			return false, compared, LegacyFailureNone
		}
	case ConditionGTE:
		if compared >= fc.Value {
			return false, compared, LegacyFailureNone
		}
	case ConditionLTE:
		if compared <= fc.Value {
			return false, compared, LegacyFailureNone
		}
	}
	// Falling through means the condition did not hold — or was not one of the
	// four. Both skip.
	return true, compared, LegacyFailureNone
}

// policyEvaluateStake mirrors models.EvaluateStake.
//
// overflow reports that the pinned policy's own int arithmetic would wrap on
// this machine. The policy computes `balance * maxStakePercent / 100` in int
// and does not guard the product, so on a sufficiently large balance the
// result is a silently wrapped number. This model detects that instead of
// reproducing a value whose meaning depends on the word size of whoever runs
// the replay — and reports no verdict for the stage rather than guessing.
func policyEvaluateStake(proposed, balance, maxStakePercent, reservePoints int) (allowed int, reason string, limit int, overflow bool) {
	allowed = proposed
	reason = GateNone

	if maxStakePercent > 0 {
		product, over := mulInt(balance, maxStakePercent)
		if over {
			return 0, GateNone, 0, true
		}
		if capped := product / 100; capped < allowed {
			allowed, reason, limit = capped, GatePercent, capped
		}
	}
	// The clamp to zero does NOT reset the reason, so a negative proposal with
	// no percent gate leaves GateNone standing beside an allowance of 0. That
	// asymmetry is real and is pinned by test.
	if allowed < 0 {
		allowed = 0
	}

	if reservePoints > 0 {
		remaining, over := subInt(balance, allowed)
		if over {
			return 0, GateNone, 0, true
		}
		// The reserve is a FLOOR, not a cap: the stake is never shrunk to fit
		// under it, the bet is abandoned instead.
		if remaining < reservePoints {
			return allowed, GateReserveViolation, reservePoints, false
		}
	}
	return allowed, reason, limit, false
}

// mulInt multiplies in int and reports wrap-around. b is always positive at
// the one call site, so the MinInt/-1 division trap cannot be reached.
func mulInt(a, b int) (int, bool) {
	if a == 0 || b == 0 {
		return 0, false
	}
	p := a * b
	if p/b != a {
		return 0, true
	}
	return p, false
}

// subInt subtracts in int and reports wrap-around.
func subInt(a, b int) (int, bool) {
	d := a - b
	// A borrow escaped the word exactly when the operands' signs differ and
	// the result took the subtrahend's sign.
	if (a >= 0) != (b >= 0) && (d >= 0) != (a >= 0) {
		return 0, true
	}
	return d, false
}

// intFromStored narrows a stored int64 to the int width the pinned policy
// computes in. Out of range is a refusal, never a truncation: a silently
// wrapped balance would produce a stake the decision never proposed.
func intFromStored(v int64) (int, bool) {
	n := int(v)
	if int64(n) != v {
		return 0, false
	}
	return n, true
}
