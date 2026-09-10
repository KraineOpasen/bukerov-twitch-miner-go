package predictioneval

import "math"

// THE ORDERED-RULES MECHANISM.
//
// One pure function, [EvaluateOrderedRules], re-deriving the donor's
// ordered-rules decision over a projected stream. The shape is the donor's own,
// and every part of that shape is load-bearing:
//
//	for each candidate:                 # each is one entry into the logic
//	  if fewer than two outcomes: next candidate
//	  T = checked sum of the outcome points
//	  for each outcome, IN VECTOR ORDER:          # OUTCOME is the outer loop
//	    p = 1 / (T / points)                      # reciprocal, not points/T
//	    for each detailed rule, IN CONFIG ORDER:  # RULE is the inner loop
//	      if comparator does not match: next rule, NO draw
//	      if participation draw succeeds: CHOOSE this outcome, this rule's points
//	      else: next rule           # a failed draw FALLS THROUGH, never breaks
//	    if no rule admitted and p is within THIS outcome's default bounds:
//	      CHOOSE this outcome, the default's points, consuming NO draw
//	    else: next outcome
//
// Four inversions of that shape each produce a different, plausible, wrong
// model, and each has its own falsifier:
//
//   - rules outside, outcomes inside — TestOrderedRulesOutcomeBeforeRulePriority;
//   - the default applied once after all outcomes rather than per outcome —
//     TestOrderedRulesDefaultPreemptsLaterOutcomeDetailed;
//   - a failed draw ending the rule scan instead of falling through —
//     TestOrderedRulesFailedDrawFallsThroughOverlap;
//   - the pool share computed as a direct share instead of the reciprocal —
//     TestOrderedRulesReciprocalRatioGe90Boundary.
//
// # Where this model STOPS, and why it is not zero
//
// The traversal ends at the EARLIEST of: the first admission with a computable
// stake; the first admission whose stake is not computable; the projected
// factual boundary, which the stream already applied; a required input it
// actually reached and could not read; or a resource bound.
//
// After an admission the model stops rather than continuing, because everything
// past that point belongs to a world in which this bet was never placed. There
// is no counterfactual placement success, no retry, no pool mutation and no
// balance update here — none of those transitions is established by anything
// this package can read, and inventing them would turn a bounded mechanism into
// an unbounded simulation.
//
// A stop with an unknown stake is NOT a stake of zero and NOT an abstention.
// The model knows WHICH outcome it would have bet on and does not know HOW
// MUCH; those are different facts and are reported as different fields. The
// four vectors in which a missing balance must behave differently are pinned by
// TestOrderedRulesMissingBalanceFailedDrawContinues,
// TestOrderedRulesMissingBalanceNoMatchContinues,
// TestOrderedRulesMissingBalanceAdmittedDrawStopsWithUnknownStake and
// TestOrderedRulesMissingBalanceDefaultStopsWithoutDraw.

// Donor arithmetic constants, mirrored exactly.
const (
	// bernoulliAlwaysTrue is rand 0.8.5's ALWAYS_TRUE sentinel: u64::MAX. A
	// distribution holding it returns true WITHOUT drawing, which is why a
	// participation rate of exactly one consumes no entropy.
	bernoulliAlwaysTrue = ^uint64(0)
	// bernoulliScale is rand 0.8.5's SCALE: 2^64, written as the float64 it is.
	bernoulliScale = 18446744073709551616.0
	// u32Ceiling is 2^32, the first float64 above the u32 domain.
	u32Ceiling = 4294967296.0
	// percentDivisor is the donor's single normalization step.
	percentDivisor = 100.0
)

// ReasonStreamDigestMismatch is a stream whose contents no longer match the
// selection digest its projection computed — a stream mutated after its
// boundary was established.
const ReasonStreamDigestMismatch = "STREAM_SELECTION_DIGEST_MISMATCH"

// OrderedRulesCandidateVerdict is what the traversal did with one candidate.
type OrderedRulesCandidateVerdict string

const (
	// CandidateNoMatch is a candidate fully traversed with nothing admitted.
	// The traversal continues to the next candidate: repeated values at
	// different source positions are genuinely new opportunities.
	CandidateNoMatch OrderedRulesCandidateVerdict = "NO_MATCH"
	// CandidateTooFewOutcomes is a pool the donor declines to bet on at all.
	CandidateTooFewOutcomes OrderedRulesCandidateVerdict = "TOO_FEW_OUTCOMES"
	// CandidateAdmitted is a candidate on which participation was admitted.
	CandidateAdmitted OrderedRulesCandidateVerdict = "ADMITTED"
	// CandidateUnknownInput is a candidate carrying a required input the
	// traversal reached and could not read.
	CandidateUnknownInput OrderedRulesCandidateVerdict = "UNKNOWN_INPUT"
	// CandidateRefused is a candidate at which a resource bound was reached.
	CandidateRefused OrderedRulesCandidateVerdict = "REFUSED"
)

// OrderedRulesBalanceUse is how the model treated ONE candidate's balance.
//
// This exists because "the balance was missing" is four different pieces of
// evidence depending on whether the model ever needed it, and collapsing them
// would lose the distinction the four missing-balance vectors turn on.
type OrderedRulesBalanceUse string

const (
	// BalanceNotEvaluated is a balance the model never needed, because nothing
	// on this candidate admitted. It is NOT "missing": the model did not look.
	BalanceNotEvaluated OrderedRulesBalanceUse = "NOT_EVALUATED"
	// BalanceUsed is a known balance actually used to size a stake.
	BalanceUsed OrderedRulesBalanceUse = "USED"
	// BalanceRequiredButMissing is admission reached with no balance supplied.
	BalanceRequiredButMissing OrderedRulesBalanceUse = "REQUIRED_BUT_MISSING"
	// BalanceRequiredButInvalid is admission reached with a balance the caller
	// declared unusable.
	BalanceRequiredButInvalid OrderedRulesBalanceUse = "REQUIRED_BUT_INVALID"
	// BalanceRequiredButOutOfDomain is admission reached with a balance outside
	// the donor's u32 stake domain.
	BalanceRequiredButOutOfDomain OrderedRulesBalanceUse = "REQUIRED_BUT_OUT_OF_DOMAIN"
)

// OrderedRulesCandidateVisit records what happened at one candidate.
//
// It is per-candidate rather than per-run because the run-level status only
// describes the STOP, and three of the four missing-balance vectors are about
// candidates the traversal walked THROUGH.
type OrderedRulesCandidateVisit struct {
	CandidateIndex    int                          `json:"candidateIndex"`
	CandidateIdentity string                       `json:"candidateIdentity"`
	CandidatePosition int64                        `json:"candidatePosition"`
	Verdict           OrderedRulesCandidateVerdict `json:"verdict"`
	BalanceUse        OrderedRulesBalanceUse       `json:"balanceUse"`
	// PoolTotalKnown separates a pool that summed to zero from one that could
	// not be summed at all.
	PoolTotalKnown bool  `json:"poolTotalKnown"`
	PoolTotal      int64 `json:"poolTotal"`
	// RawWordsConsumedHere is how much entropy this candidate alone spent.
	RawWordsConsumedHere int `json:"rawWordsConsumedHere"`
}

// normalized* are the config AFTER its single division by 100.
//
// They are unexported on purpose. The exported contract accepts raw
// percentages only, so there is no way for a caller to hand in an
// already-normalized config and have it divided a second time — which would
// silently turn a 50% participation rate into 0.5%. See
// TestOrderedRulesNormalizeExactlyOnce.
type normalizedPoints struct {
	maxValue uint32
	percent  float64
}

type normalizedRule struct {
	comparator  OrderedRuleComparator
	threshold   float64
	attemptRate float64
	points      normalizedPoints
}

type normalizedConfig struct {
	detailed  []normalizedRule
	defMin    float64
	defMax    float64
	defPoints normalizedPoints
}

// EvaluateOrderedRules re-derives the donor mechanism over a projected stream.
//
// It returns no error: every refusal and every unknown is a TYPED STATUS on the
// result, because a bare error would lose the distinction between "this input
// is not evaluable", "the mechanism ran and admitted nothing" and "the
// mechanism admitted and the stake is unknown" — three outcomes a caller must
// not conflate.
//
// The stream must be one [ProjectOrderedRulesStream] produced and did not
// subsequently mutate; the config is RAW percentages; the draws are the only
// entropy this function will ever see. There is no clock, no RNG and no
// fallback: an exhausted trace is an explicit unknown, never a default draw.
func EvaluateOrderedRules(stream OrderedRulesStream, rules OrderedRulesConfig, draws SuppliedDrawTrace) OrderedRulesEvaluation {
	out := OrderedRulesEvaluation{
		EvidenceLabel:           OrderedRulesEvidenceLabel,
		ModelVersion:            OrderedRulesModelVersion,
		DonorRevision:           OrderedRulesDonorRevision,
		EntropySemanticsVersion: OrderedRulesEntropySemanticsVersion,
		Participation:           ParticipationNotAdmitted,
		Stake:                   SuppliedUint32{Presence: SuppliedMissing, Reason: ReasonBalanceNotEvaluated},
		StreamDigest:            orderedRulesStreamDigest(stream),
		ConfigDigest:            orderedRulesConfigDigest(rules),
		EntropyDigest:           orderedRulesEntropyDigest(draws),
	}
	if len(stream.Qualifications) > 0 {
		out.Qualifications = make([]string, len(stream.Qualifications))
		copy(out.Qualifications, stream.Qualifications)
	}

	// refuse reports a typed refusal witnessing the prefix ACTUALLY consumed.
	// A refusal reached mid-traversal has already read candidates and spent
	// words, and a digest claiming an empty prefix would misdescribe it.
	refuse := func(reason string, candidatesConsumed, wordsConsumed int) OrderedRulesEvaluation {
		out.Status = StatusRefused
		out.Reason = reason
		out.ConsumedInputDigest = orderedRulesConsumedDigest(stream, rules, draws,
			candidatesConsumed, wordsConsumed)
		return out
	}

	switch {
	case stream.ContractVersion != OrderedRulesStreamContractVersion:
		return refuse(ReasonStreamContractMismatch, 0, 0)
	case stream.SelectionDigest != out.StreamDigest:
		return refuse(ReasonStreamDigestMismatch, 0, 0)
	case draws.EntropySemanticsVersion != OrderedRulesEntropySemanticsVersion:
		return refuse(ReasonEntropySemanticsMismatch, 0, 0)
	case len(draws.Words) > MaxOrderedRulesDrawWords:
		return refuse(ReasonDrawWordsOverBound, 0, 0)
	case len(rules.Detailed) > MaxOrderedRulesRules:
		return refuse(ReasonRuleCountOverBound, 0, 0)
	}
	cfg, configReason := normalizeOrderedRulesConfig(rules)
	if configReason != "" {
		return refuse(configReason, 0, 0)
	}

	words := draws.Words
	cursor := 0
	work := 0

	for ci := range stream.Candidates {
		c := &stream.Candidates[ci]
		wordsBefore := cursor
		visit := OrderedRulesCandidateVisit{
			CandidateIndex:    ci,
			CandidateIdentity: c.Identity,
			CandidatePosition: c.Position,
			BalanceUse:        BalanceNotEvaluated,
		}
		out.CandidatesConsumed = ci + 1
		out.StoppedAtCandidate = c.Identity
		out.StoppedAtPosition = c.Position
		out.HasStopPosition = true

		// The donor declines a pool it cannot compare. This is checked BEFORE
		// the points are read, so a one-outcome pool with an unreadable total
		// is a decline rather than an unknown.
		if len(c.Outcomes) < 2 {
			visit.Verdict = CandidateTooFewOutcomes
			out.Visits = append(out.Visits, visit)
			continue
		}

		total, poolReason := checkedPoolSum(c.Outcomes)
		if poolReason != "" {
			visit.Verdict = CandidateUnknownInput
			visit.RawWordsConsumedHere = cursor - wordsBefore
			out.Visits = append(out.Visits, visit)
			out.Status = StatusUnknownInput
			out.Reason = poolReason
			out.ConsumedInputDigest = orderedRulesConsumedDigest(stream, rules, draws, ci+1, cursor)
			return out
		}
		visit.PoolTotalKnown = true
		visit.PoolTotal = total

		for oi := range c.Outcomes {
			out.OutcomesConsidered++
			share := donorShare(total, c.Outcomes[oi].Points.Value)
			shareBits := math.Float64bits(share)

			admittedRule := -1
			for ri := range cfg.detailed {
				work++
				if work > MaxOrderedRulesWork {
					visit.Verdict = CandidateRefused
					visit.RawWordsConsumedHere = cursor - wordsBefore
					out.Visits = append(out.Visits, visit)
					out.RawWordsConsumed = cursor
					return refuse(ReasonWorkBudgetExceeded, ci+1, cursor)
				}
				out.RulesConsidered++
				rule := &cfg.detailed[ri]

				var matched bool
				switch rule.comparator {
				case ComparatorLe:
					matched = share <= rule.threshold
				case ComparatorGe:
					matched = share >= rule.threshold
				}
				if !matched {
					// A comparator that did not match costs NO entropy. The
					// donor's `does_match && rng.gen_bool(..)` short-circuits,
					// so the draw is never reached.
					out.Trace = append(out.Trace, traceEntry(TraceStepRuleComparator, ci, c, oi,
						c.Outcomes[oi].Identity, shareBits, ri, false, false, false, -1, 0, false))
					continue
				}

				threshold, ok := bernoulliThreshold(rule.attemptRate)
				if !ok {
					// Unreachable while the admitted domain is 0..100: raw 100
					// normalizes to exactly one and everything below it lands in
					// [0,1). It is kept so that widening that domain later
					// surfaces here as a typed refusal instead of silently
					// producing a distribution rand would have refused to build.
					visit.Verdict = CandidateRefused
					visit.RawWordsConsumedHere = cursor - wordsBefore
					out.Visits = append(out.Visits, visit)
					out.RawWordsConsumed = cursor
					return refuse(ReasonAttemptRateOutOfDomain, ci+1, cursor)
				}

				rawIndex := -1
				var rawValue uint64
				var success bool
				if threshold == bernoulliAlwaysTrue {
					// Rate of exactly one: true, and NO word consumed.
					success = true
				} else {
					if cursor >= len(words) {
						// The draw was REQUIRED and the trace is spent. This is
						// an explicit unknown, never a default draw and never a
						// skip: guessing here would fabricate an opportunity.
						out.Trace = append(out.Trace, traceEntry(TraceStepRuleDraw, ci, c, oi,
							c.Outcomes[oi].Identity, shareBits, ri, true, false, false, -1, 0, false))
						visit.Verdict = CandidateUnknownInput
						visit.RawWordsConsumedHere = cursor - wordsBefore
						out.Visits = append(out.Visits, visit)
						out.RawWordsConsumed = cursor
						out.Status = StatusUnknownInput
						out.Reason = ReasonEntropyExhausted
						out.ConsumedInputDigest = orderedRulesConsumedDigest(stream, rules, draws, ci+1, cursor)
						return out
					}
					rawIndex = cursor
					rawValue = words[cursor]
					cursor++
					// Rate of exactly zero reaches here too: its threshold is 0,
					// so it consumes a word and then fails on `raw < 0`.
					success = rawValue < threshold
				}
				out.BernoulliEvaluations++
				out.RawWordsConsumed = cursor
				out.Trace = append(out.Trace, traceEntry(TraceStepRuleDraw, ci, c, oi,
					c.Outcomes[oi].Identity, shareBits, ri, true, true, success, rawIndex, rawValue, success))
				if success {
					admittedRule = ri
					break
				}
				// A matched comparator whose draw failed does NOT end the scan.
				// The donor's `find` keeps going, so a later overlapping rule
				// still gets its own chance on this same outcome.
			}

			if admittedRule >= 0 {
				out.RawWordsConsumed = cursor
				visit.RawWordsConsumedHere = cursor - wordsBefore
				admitOrderedRules(&out, &visit, stream, rules, draws, c, ci, oi, admittedRule,
					shareBits, cfg.detailed[admittedRule].points, SelectionDetailedRule, cursor)
				return out
			}

			// The CURRENT outcome's default, before any later outcome's rules.
			work++
			if work > MaxOrderedRulesWork {
				visit.Verdict = CandidateRefused
				visit.RawWordsConsumedHere = cursor - wordsBefore
				out.Visits = append(out.Visits, visit)
				out.RawWordsConsumed = cursor
				return refuse(ReasonWorkBudgetExceeded, ci+1, cursor)
			}
			// Inclusive on both ends, and never reordered when min > max.
			inDefault := share >= cfg.defMin && share <= cfg.defMax
			out.Trace = append(out.Trace, traceEntry(TraceStepDefaultBounds, ci, c, oi,
				c.Outcomes[oi].Identity, shareBits, -1, inDefault, false, false, -1, 0, inDefault))
			if inDefault {
				// The default consumes NO new draw. Words already spent by
				// failed detailed rules on this outcome stay spent.
				out.RawWordsConsumed = cursor
				visit.RawWordsConsumedHere = cursor - wordsBefore
				admitOrderedRules(&out, &visit, stream, rules, draws, c, ci, oi, -1,
					shareBits, cfg.defPoints, SelectionDefault, cursor)
				return out
			}
		}

		visit.Verdict = CandidateNoMatch
		visit.RawWordsConsumedHere = cursor - wordsBefore
		out.Visits = append(out.Visits, visit)
	}

	out.Status = StatusNoAttemptInSuppliedPrefix
	out.RawWordsConsumed = cursor
	out.ConsumedInputDigest = orderedRulesConsumedDigest(stream, rules, draws, len(stream.Candidates), cursor)
	return out
}

// admitOrderedRules records an admission and sizes the stake if it can.
//
// Participation and stake are set INDEPENDENTLY. That separation is the whole
// point: the mechanism's choice is established by the traversal, while sizing
// needs a balance the traversal may not have. A missing balance therefore
// leaves a known participation beside an unknown stake, and never a zero.
func admitOrderedRules(out *OrderedRulesEvaluation, visit *OrderedRulesCandidateVisit,
	stream OrderedRulesStream, rules OrderedRulesConfig, draws SuppliedDrawTrace,
	c *OrderedRulesCandidate, ci, oi, ruleIndex int,
	shareBits uint64, points normalizedPoints, basis OrderedRulesSelectionBasis, cursor int) {

	visit.Verdict = CandidateAdmitted
	out.Participation = ParticipationAdmitted
	out.Selected = &OrderedRulesSelection{
		CandidateIdentity: c.Identity,
		CandidatePosition: c.Position,
		CandidateIndex:    ci,
		OutcomeIndex:      oi,
		OutcomeIdentity:   c.Outcomes[oi].Identity,
		Basis:             basis,
		RuleIndex:         ruleIndex,
		ShareBits:         shareBits,
	}

	b := c.Balance
	switch {
	case b.Presence == SuppliedInvalid:
		visit.BalanceUse = BalanceRequiredButInvalid
		out.Status = StatusParticipationAdmittedStakeUnknown
		out.Reason = ReasonBalanceInvalid
		out.Stake = SuppliedUint32{Presence: SuppliedInvalid, Reason: balanceReason(b, ReasonBalanceInvalid)}
	case b.Presence != SuppliedKnown:
		visit.BalanceUse = BalanceRequiredButMissing
		out.Status = StatusParticipationAdmittedStakeUnknown
		out.Reason = ReasonBalanceNotSupplied
		out.Stake = SuppliedUint32{Presence: SuppliedMissing, Reason: balanceReason(b, ReasonBalanceNotSupplied)}
	case b.Value < 0 || b.Value > math.MaxUint32:
		// The donor sizes stakes in u32. A balance outside that domain leaves
		// the admission standing and the sizing impossible — it is not clamped
		// into range, because a clamped stake would be a number the donor never
		// would have produced.
		visit.BalanceUse = BalanceRequiredButOutOfDomain
		out.Status = StatusParticipationAdmittedStakeUnknown
		out.Reason = ReasonBalanceOutOfDomain
		out.Stake = SuppliedUint32{Presence: SuppliedInvalid, Reason: ReasonBalanceOutOfDomain}
	default:
		// A known balance sizes the stake, INCLUDING a stake of zero. Zero is
		// what the donor would have requested; it is not a skip, and no minimum
		// is substituted for it.
		visit.BalanceUse = BalanceUsed
		out.Status = StatusWouldAttempt
		out.Stake = SuppliedUint32{Presence: SuppliedKnown, Value: pointsValue(points, uint32(b.Value))}
	}

	out.Visits = append(out.Visits, *visit)
	out.ConsumedInputDigest = orderedRulesConsumedDigest(stream, rules, draws, ci+1, cursor)
}

func balanceReason(b SuppliedInt64, fallback string) string {
	if b.Reason != "" {
		return b.Reason
	}
	return fallback
}

func traceEntry(step OrderedRulesTraceStep, ci int, c *OrderedRulesCandidate, oi int,
	outcomeID string, shareBits uint64, ruleIndex int,
	comparatorMatched, bernoulliEvaluated, bernoulliResult bool,
	rawIndex int, rawValue uint64, admitted bool) OrderedRulesTraceEntry {
	return OrderedRulesTraceEntry{
		Step:               step,
		CandidateIndex:     ci,
		CandidateIdentity:  c.Identity,
		CandidatePosition:  c.Position,
		OutcomeIndex:       oi,
		OutcomeIdentity:    outcomeID,
		ShareBits:          shareBits,
		RuleIndex:          ruleIndex,
		ComparatorMatched:  comparatorMatched,
		BernoulliEvaluated: bernoulliEvaluated,
		BernoulliResult:    bernoulliResult,
		RawWordIndex:       rawIndex,
		RawWordValue:       rawValue,
		Admitted:           admitted,
	}
}

// normalizeOrderedRulesConfig divides every raw percentage by 100 EXACTLY ONCE.
//
// It returns a closed reason rather than an error so the caller can report it
// as a typed status. The admitted domain mirrors the donor's own validation:
// each percentage field independently in 0..100, finite. Min above max is NOT
// corrected — the donor validates the two independently and never swaps them,
// so a min above a max is a configuration that admits nothing, which is a real
// configuration and not this model's to repair.
func normalizeOrderedRulesConfig(c OrderedRulesConfig) (normalizedConfig, string) {
	var out normalizedConfig
	if len(c.Detailed) > 0 {
		out.detailed = make([]normalizedRule, 0, len(c.Detailed))
	}
	for i := range c.Detailed {
		r := &c.Detailed[i]
		switch r.Comparator {
		case ComparatorLe, ComparatorGe:
		default:
			return normalizedConfig{}, ReasonConfigOutOfDomain
		}
		if !rawPercentInDomain(r.RawThresholdPercent) ||
			!rawPercentInDomain(r.RawAttemptRatePercent) ||
			!rawPercentInDomain(r.Points.RawPercent) {
			return normalizedConfig{}, ReasonConfigOutOfDomain
		}
		out.detailed = append(out.detailed, normalizedRule{
			comparator:  r.Comparator,
			threshold:   r.RawThresholdPercent / percentDivisor,
			attemptRate: r.RawAttemptRatePercent / percentDivisor,
			points: normalizedPoints{
				maxValue: r.Points.MaxValue,
				percent:  r.Points.RawPercent / percentDivisor,
			},
		})
	}
	if !rawPercentInDomain(c.Default.RawMinPercent) ||
		!rawPercentInDomain(c.Default.RawMaxPercent) ||
		!rawPercentInDomain(c.Default.Points.RawPercent) {
		return normalizedConfig{}, ReasonConfigOutOfDomain
	}
	out.defMin = c.Default.RawMinPercent / percentDivisor
	out.defMax = c.Default.RawMaxPercent / percentDivisor
	out.defPoints = normalizedPoints{
		maxValue: c.Default.Points.MaxValue,
		percent:  c.Default.Points.RawPercent / percentDivisor,
	}
	return out, ""
}

// rawPercentInDomain mirrors the donor's `range(min = 0.0, max = 100.0)`.
// NaN fails both comparisons and is rejected rather than normalized to zero.
func rawPercentInDomain(v float64) bool { return v >= 0 && v <= 100 }

// checkedPoolSum totals an outcome vector in int64 without wrapping.
//
// Every outcome's points must be KNOWN. A missing one is not a zero: the pool
// total feeds every share in the vector, so one absent value makes the whole
// candidate's arithmetic unknown rather than merely one outcome's.
func checkedPoolSum(outcomes []OrderedRulesOutcome) (int64, string) {
	var total int64
	for i := range outcomes {
		v := outcomes[i].Points
		if v.Presence != SuppliedKnown {
			return 0, ReasonOutcomePointsNotKnown
		}
		if v.Value < 0 {
			return 0, ReasonOutcomePointsOutOfDomain
		}
		if total > math.MaxInt64-v.Value {
			return 0, ReasonPoolSumOverflow
		}
		total += v.Value
	}
	return total, ""
}

// donorShare computes one outcome's pool share the way the donor does.
//
// The two divisions are NOT algebraically simplified to points/total. In
// binary64 they are different functions: for points=[9,1] the donor's
// 1/(10/9) is one ulp BELOW the nearest double to 0.9, while 9/10 rounds to the
// nearest double AT 0.9 — so a Ge rule with a 90% threshold matches under the
// shortcut and does not match under the donor. See
// TestOrderedRulesReciprocalRatioGe90Boundary.
func donorShare(total, points int64) float64 {
	if points == 0 {
		return 0
	}
	odds := float64(total) / float64(points)
	if odds == 0 {
		return 0
	}
	return 1.0 / odds
}

// bernoulliThreshold mirrors rand 0.8.5's Bernoulli::new.
//
//	if !(0.0..1.0).contains(&p) { if p == 1.0 { ALWAYS_TRUE } else { Err } }
//	p_int = (p * 2^64) as u64
//
// The half-open range is the donor's, not a transcription slip: exactly one is
// special-cased to a sentinel that skips the draw entirely, which is why a
// hundred-percent participation rate consumes no entropy. NaN satisfies neither
// branch and is refused.
func bernoulliThreshold(p float64) (uint64, bool) {
	if !(p >= 0.0 && p < 1.0) {
		if p == 1.0 {
			return bernoulliAlwaysTrue, true
		}
		return 0, false
	}
	return saturatingUint64(p * bernoulliScale), true
}

// pointsValue mirrors the donor's Points::value.
//
// Multiply in float64, TRUNCATE to u32, and only then cap. The order matters:
// capping before the cast, or rounding instead of truncating, produces a
// different stake for the same inputs. A max_value of zero means NO CAP — it
// does not mean a stake of zero. See TestOrderedRulesPointsTruncateThenCap.
func pointsValue(p normalizedPoints, balance uint32) uint32 {
	value := saturatingUint32(p.percent * float64(balance))
	if p.maxValue == 0 {
		return value
	}
	if value < p.maxValue {
		return value
	}
	return p.maxValue
}

// saturatingUint64 mirrors Rust's float-to-integer `as` cast, which SATURATES
// rather than wrapping or trapping: NaN becomes zero, negatives become zero,
// and anything at or above the type's ceiling becomes its maximum. Go leaves
// the same conversion implementation-defined, so the clamping is explicit.
func saturatingUint64(f float64) uint64 {
	if math.IsNaN(f) || f <= 0 {
		return 0
	}
	if f >= bernoulliScale {
		return ^uint64(0)
	}
	return uint64(f)
}

func saturatingUint32(f float64) uint32 {
	if math.IsNaN(f) || f <= 0 {
		return 0
	}
	if f >= u32Ceiling {
		return math.MaxUint32
	}
	return uint32(f)
}
