package predictioneval

import (
	"errors"
	"strconv"
)

// THE PROJECTION.
//
// [ProjectOrderedRulesStream] turns an explicitly supplied source into the
// bounded, detached prefix the model may evaluate. It does three things and
// refuses to do a fourth:
//
//  1. it CHECKS the supplied data against the contract — identity, order,
//     episode membership, declared interval, coverage, resource bounds;
//  2. it ESTABLISHES the factual boundary — the first relevant placement call
//     in the declared episode — and drops every candidate at or after it;
//  3. it DETACHES what survives, so nothing downstream aliases the caller.
//
// It never adds a value that was not supplied. There is no back-fill, no
// forward-fill, no defaulting and no repair: a projection that could invent a
// pool, a balance or a position would make every guarantee below it decorative.

// Projection refusals. Each is a statement that the SUPPLIED DATA does not meet
// the contract — never a statement about the mechanism, which has not run yet.
var (
	// ErrOrderedRulesScopeIncomplete is a scope missing a field the contract
	// requires to identify what was supplied.
	ErrOrderedRulesScopeIncomplete = errors.New("predictioneval: ordered-rules scope is incomplete")
	// ErrOrderedRulesUnknownCoverage is a source that declines to characterise
	// its own coverage. It is refused because an unstated coverage makes "no
	// intervention was supplied" indistinguishable from "the intervention was
	// never collected", and the model would then read the second as the first.
	ErrOrderedRulesUnknownCoverage = errors.New("predictioneval: ordered-rules source declares unknown coverage")
	// ErrOrderedRulesAdmissionIncomplete is a common-admission manifest that
	// does not say which population, view or order it selected.
	ErrOrderedRulesAdmissionIncomplete = errors.New("predictioneval: common-admission manifest is incomplete")
	// ErrOrderedRulesDuplicateIdentity is the same source identity supplied
	// twice. It is refused rather than deduplicated in either direction.
	ErrOrderedRulesDuplicateIdentity = errors.New("predictioneval: ordered-rules source repeats a candidate identity")
	// ErrOrderedRulesAmbiguousOrder is a causal order the caller does not
	// actually know: positions that do not strictly increase.
	ErrOrderedRulesAmbiguousOrder = errors.New("predictioneval: ordered-rules candidates are not in strictly increasing causal order")
	// ErrOrderedRulesMembershipUnproven is a candidate whose membership in the
	// declared episode was not established.
	ErrOrderedRulesMembershipUnproven = errors.New("predictioneval: ordered-rules candidate episode membership is not proven")
	// ErrOrderedRulesOutsideDeclaredInterval is a fact outside the interval the
	// caller itself declared.
	ErrOrderedRulesOutsideDeclaredInterval = errors.New("predictioneval: ordered-rules fact lies outside the declared interval")
	// ErrOrderedRulesBackdatedValue is a value attached to a candidate it could
	// not have been available at.
	ErrOrderedRulesBackdatedValue = errors.New("predictioneval: ordered-rules value became available after the candidate it is attached to")
	// ErrOrderedRulesOverBound is a supplied input past an offline resource
	// bound. It is refused whole; nothing is truncated.
	ErrOrderedRulesOverBound = errors.New("predictioneval: ordered-rules source exceeds an offline resource bound")
	// ErrOrderedRulesVocabulary is a value outside a closed vocabulary. It is
	// never mapped onto a plausible member of that vocabulary.
	ErrOrderedRulesVocabulary = errors.New("predictioneval: ordered-rules source uses a value outside a closed vocabulary")
	// ErrOrderedRulesNotEncodable is a retained string that would not survive
	// the stream's own JSON round trip. It is separate from the vocabulary and
	// bound errors because it is a fact about ENCODING rather than about the
	// value being wrong or too large: the value is refused precisely so that
	// what a caller supplies is what the digest witnesses.
	ErrOrderedRulesNotEncodable = errors.New("predictioneval: ordered-rules source carries text that is not valid UTF-8")
	// ErrOrderedRulesViewMismatch is a manifest whose declared view disagrees
	// with the kind of candidates actually supplied — a CALCULATE_ONLY slice
	// presented as a channel update stream, or the reverse.
	ErrOrderedRulesViewMismatch = errors.New("predictioneval: ordered-rules candidates do not match the declared admission view")
)

// Qualification texts. They travel from the stream into every evaluation
// computed from it, so a limitation cannot be lost between the two.
const (
	qualCoverageNotComplete = "SOURCE_COVERAGE_NOT_COMPLETE: the declared interval is not complete, so the absence of an earlier factual intervention is NOT established"
	qualConservativeCutoff  = "CUTOFF_FROM_AMBIGUOUS_ASSOCIATION: the boundary was taken at an intervention whose association with this episode could not be established; the earlier, conservative boundary was chosen"
	qualCalculateOnlyView   = "CALCULATE_ONLY_VIEW: these candidates are decision-time model snapshots, NOT a recovered wire create/update stream"
	qualNoInterventionSeen  = "NO_INTERVENTION_IN_DECLARED_INTERVAL: no relevant placement call was supplied inside the declared interval"
	// qualBoundaryRemovedCandidates is completed with the count.
	qualBoundaryRemovedCandidates = "BOUNDARY_REMOVED_CANDIDATES: the factual boundary excluded supplied candidates, so any absence of an attempt describes only what preceded it; count="
)

// ProjectOrderedRulesStream projects a supplied source into the bounded,
// detached candidate prefix strictly before the factual boundary.
//
// The boundary is the FIRST relevant placement call in the declared episode,
// automatic or manual. Manual calls carry no attempt discriminator and may
// carry a different or empty round incarnation, so they are supplied as
// first-class interventions rather than discovered by grouping — grouping is
// what would miss them. The call's own stake, result and success are not
// inputs and have no field here: a call that FAILED is still a boundary,
// because the world it acted on is the one that continued.
func ProjectOrderedRulesStream(source OrderedRulesSource, admission CommonAdmission) (OrderedRulesStream, error) {
	// LENGTH BEFORE MEANING, for every value that can reach an error formatter,
	// and THE TWO COUNTS FIRST, ahead of every scan below, because both are a
	// slice length against a constant and neither reads a supplied byte.
	//
	// They used to sit after the validators, and validateAdmission walks up to
	// MaxOrderedRulesSourceReferences strings of MaxOrderedRulesIdentifierBytes
	// each — so a source with one candidate too many paid a 4 MiB scan of text
	// the refusal does not depend on before anyone counted its candidates.
	// Measured on the identical input: 3.140461 ms to 220 ns.
	//
	// This is the same length-before-meaning rule the file applies everywhere
	// else, applied to the one pair that was exempt from it.
	if len(source.Candidates) > MaxOrderedRulesCandidates {
		return OrderedRulesStream{}, errors.Join(ErrOrderedRulesOverBound,
			errors.New("predictioneval: "+strconv.Itoa(len(source.Candidates))+
				" candidates exceed the bound of "+strconv.Itoa(MaxOrderedRulesCandidates)))
	}
	if len(source.Interventions) > MaxOrderedRulesInterventions {
		return OrderedRulesStream{}, errors.Join(ErrOrderedRulesOverBound,
			errors.New("predictioneval: "+strconv.Itoa(len(source.Interventions))+
				" interventions exceed the bound of "+strconv.Itoa(MaxOrderedRulesInterventions)))
	}

	// AND THE SOURCE CONTRACT, third, above every loop in this function.
	//
	// A source declaring a contract this package does not project is not a
	// source whose candidates, interventions or byte total mean anything, so
	// nothing below is evidence for or against the refusal. The word is
	// length-bounded first because the comparison's error QUOTES it, and
	// strconv.Quote scans the whole string and allocates an expanded copy:
	// measured before that gate existed, a 64 MiB contract version took a
	// second to refuse and built a 67 MB error message — the refusal was the
	// denial of service. Bounded, the scan is at most
	// MaxOrderedRulesIdentifierBytes, a constant, which is why it may sit ahead
	// of the constant-bounded tier without breaking that tier's own rule.
	//
	// THIS IS THE SECOND PLACE IT HAS BEEN, and the first was wrong for a
	// reason worth keeping. The comparison first moved out of validateScope to
	// just below the vocabulary bounds, on the claim that what remained was the
	// count-and-length budget tier and could not be avoided. A reviewer
	// falsified that: the tier below also walks every intervention and every
	// candidate, checking positions, causal order, interval membership, outcome
	// counts and nested presence declarations, and building a diagnostic label
	// per candidate. None of that is length arithmetic and none of it is free.
	// Measured at the widest admitted counts — 128 candidates x 64 outcomes,
	// 1024 interventions — the same four-byte fault: 1.024333 ms in
	// validateScope, 479.662 µs below the vocabulary bounds, 222 ns here.
	// The floor inside this function is 160 ns, the two count checks above.
	//
	// The lesson is the one this file keeps paying for: a cost claim is a claim,
	// and "what remains is inherent" is the easiest kind to assert and the
	// hardest to notice being wrong.
	if err := checkFreeText(source.Scope.SourceContractVersion, "scope source contract version"); err != nil {
		return OrderedRulesStream{}, err
	}
	if err := checkSourceContractVersion(source.Scope); err != nil {
		return OrderedRulesStream{}, err
	}

	// THE CONSTANT-BOUNDED TIER, entire, before any caller-scaled read anywhere
	// in this function. Everything from here to the ceiling check is an
	// emptiness test, an integer comparison, a slice length, arithmetic over
	// len(), or a comparison against a short constant; no message below quotes
	// a caller-supplied value.
	//
	// It was called the zero-byte tier, and that was not quite true: checkPresenceShape
	// compares a presence word against SuppliedKnown, and a supplied word of
	// matching LENGTH is compared byte for byte. The evaluator's own tier had
	// the same overstatement and was corrected first; this one was missed in
	// that pass and a reviewer caught it, which makes it the fifth time a rule
	// has been fixed on one of these two paths and not the other.
	//
	// What the name claims is narrower than "independent of the input", and the
	// broader version was the NEXT thing a reviewer had to correct: this tier
	// walks every supplied reference, intervention, candidate and outcome, so a
	// caller chooses how many times each check runs, within the count ceilings
	// above. What a caller cannot do is make any single one of them cost more by
	// supplying a LONGER string. Per-element cost is fixed here; element count
	// is bounded there.
	//
	// Splitting the function by what a check COSTS rather than by what it is
	// about is what makes this tier possible at all. Each earlier repair had
	// moved one cheap decision ahead of one expensive scan and the next review
	// found the next pair — a scope walk before a candidate's declared
	// position, an identity hash before an interval test, a provenance scan
	// before an availability flag. Measured for the last of those, an admission
	// of 1,024 references beside a candidate that declares no position:
	// 3.062608 ms to 550 ns.
	//
	// AN EARLIER REVISION CLAIMED THIS CLOSED THE AXIS, and it does not. The
	// claim was that after the ceiling below every remaining check is a bounded
	// scan over a source already known to fit, so no reordering could change an
	// asymptotic cost again. That is true and beside the point: this axis has
	// never been about asymptotics. Every finding on it has been a constant
	// factor between 100x and 20,000x, and the text tier below still holds a
	// cost gradient — four short vocabulary words per candidate against 4 MiB
	// of admission references against 8 MiB of intervention text against the
	// outcome payload. Ordering within that gradient is what the tier below
	// now does as far as it goes, and no claim is made that it goes far enough.
	if err := validateScopeShape(source.Scope); err != nil {
		return OrderedRulesStream{}, err
	}
	if err := validateAdmissionShape(admission); err != nil {
		return OrderedRulesStream{}, err
	}

	// Every caller-supplied string the projection RETAINS is counted, not just
	// the identifiers: coverage detail, the admission manifest and each
	// intervention's detail all survive into the stream, so a budget that
	// skipped them would bound the wrong thing.
	bytes := chargedWidth(len(source.Scope.Namespace) + len(source.Scope.EpisodeID) +
		len(source.Scope.AccountContext) + len(source.Scope.AssociationEvidence) +
		len(source.Scope.CoverageDetail) + len(source.Scope.SourceContractVersion) +
		len(source.Scope.Coverage))
	bytes += chargedWidth(len(admission.ManifestID) + len(admission.Population) + len(admission.OrderBasis) +
		len(admission.ViewKind))
	// Count bounded by validateAdmissionShape above; each ELEMENT is validated
	// by validateAdmission in the text tier below. Only the charge belongs
	// here, and it is length arithmetic.
	for _, ref := range admission.SourceReferences {
		bytes += chargedWidth(len(ref))
	}
	// The interventions' charge is length arithmetic and belongs here; only the
	// Detail SCAN is deferred to the text tier below.
	for i := range source.Interventions {
		bytes += chargedWidth(len(source.Interventions[i].Identity) + len(source.Interventions[i].Detail))
	}
	if err := checkInterventionStructure(source); err != nil {
		return OrderedRulesStream{}, err
	}

	if bytes > orderedRulesTextCeiling {
		return OrderedRulesStream{}, errors.Join(ErrOrderedRulesOverBound,
			errors.New("predictioneval: supplied scope, admission and intervention bytes exceed the aggregate budget"))
	}

	// THE STRUCTURAL PASS FIRST, and the distinction from the pass below is
	// what a supplied BYTE costs.
	//
	// Every check here is an integer comparison or a slice length: a declared
	// position, the causal order, the declared interval, the outcome count.
	// None of them reads a caller-supplied byte, and none of their messages
	// quotes one.
	//
	// The pass below is bounded but not free — checkIdentifier scans up to
	// MaxOrderedRulesIdentifierBytes and the duplicate map hashes the same
	// bytes, and four vocabulary fields are scanned beside them. So a LAST
	// candidate declaring no position was reached only after every earlier
	// candidate's identity had been scanned and hashed: measured with 128
	// candidates carrying 4,000-byte identities, 429.551 µs against 22.586 µs.
	//
	// That is the correction to the claim the two-pass split made. It said
	// every candidate's SHAPE preceded any candidate's PAYLOAD, and treated an
	// identity as shape; an identity is bytes, and the split it needed is this
	// one. Three passes now: what costs nothing, what costs a bounded scan,
	// and what costs the budget.
	var lastPosition int64
	for i := range source.Candidates {
		c := &source.Candidates[i]
		where := "candidate " + strconv.Itoa(i)
		if err := checkIdentifierPresent(c.Identity, where+" identity"); err != nil {
			return OrderedRulesStream{}, err
		}
		if !c.HasPosition {
			return OrderedRulesStream{}, errors.Join(ErrOrderedRulesScopeIncomplete,
				errors.New("predictioneval: "+where+" does not declare its causal position as supplied; "+
					"zero is a legitimate position, so an omitted one would be admitted as the earliest "+
					"candidate and take the opportunity from whichever candidate really was first"))
		}
		if i > 0 && c.Position <= lastPosition {
			return OrderedRulesStream{}, errors.Join(ErrOrderedRulesAmbiguousOrder,
				errors.New("predictioneval: "+where+" is at position "+strconv.FormatInt(c.Position, 10)+
					" after position "+strconv.FormatInt(lastPosition, 10)+
					"; equal or decreasing positions mean the causal order is not known and must not be sorted away"))
		}
		lastPosition = c.Position
		if c.Position < source.Scope.IntervalFromPosition || c.Position > source.Scope.IntervalToPosition {
			return OrderedRulesStream{}, errors.Join(ErrOrderedRulesOutsideDeclaredInterval,
				errors.New("predictioneval: "+where+" at position "+strconv.FormatInt(c.Position, 10)+
					" lies outside the declared interval"))
		}
		if len(c.Outcomes) > MaxOrderedRulesOutcomes {
			return OrderedRulesStream{}, errors.Join(ErrOrderedRulesOverBound,
				errors.New("predictioneval: "+where+" carries "+strconv.Itoa(len(c.Outcomes))+
					" outcomes, past the bound of "+strconv.Itoa(MaxOrderedRulesOutcomes)))
		}

		// The NESTED declarations belong to this tier too, and leaving them out
		// was the same mistake one level down. A KNOWN value that omits the
		// position it became available at is refused by a flag test, but the
		// check sat beside the text scans in the payload pass — so a last
		// candidate's balance omitting it waited on every preceding provenance
		// and reason. A source comfortably inside the ceiling can put
		// 20,480,000 bytes of provenance ahead of that flag: measured
		// 20.07073 ms.
		for j := range c.Outcomes {
			o := &c.Outcomes[j]
			ow := where + " outcome " + strconv.Itoa(j)
			if err := checkIdentifierPresent(o.Identity, ow+" identity"); err != nil {
				return OrderedRulesStream{}, err
			}
			if err := checkPresenceShape(o.Points, ow+" points", c.Position); err != nil {
				return OrderedRulesStream{}, err
			}
			bytes += chargedWidth(len(o.Identity)+len(o.Points.Presence)) + suppliedTextBytes(o.Points)
		}
		if err := checkPresenceShape(c.Balance, where+" balance", c.Position); err != nil {
			return OrderedRulesStream{}, err
		}

		// AND THE WHOLE CHARGE, which is what makes this tier a boundary rather
		// than one more step.
		//
		// Every term is chargedWidth over a len(), so the aggregate is
		// computable without reading a byte. Computing it here means an
		// over-ceiling source is refused for its SIZE before anything is
		// scanned at all, instead of scanning its way up to the limit first,
		// and it settles the accounting in one place: the passes below no
		// longer charge, so there is nothing to double-count. The total is the
		// same sum over the same fields, so which sources are ADMITTED does not
		// move; only where an over-ceiling one is stopped.
		bytes += chargedWidth(len(c.Identity) + len(c.Provenance) + len(c.OutcomesReason))
		bytes += chargedWidth(len(c.SourceKind) + len(c.EpisodeMembership) +
			len(c.OutcomesPresence) + len(c.Balance.Presence))
		bytes += suppliedTextBytes(c.Balance)
		if bytes > orderedRulesTextCeiling {
			return OrderedRulesStream{}, errors.Join(ErrOrderedRulesOverBound,
				errors.New("predictioneval: supplied identifier bytes exceed the aggregate budget"))
		}
	}

	if err := checkFreeText(string(source.Scope.Coverage), "scope coverage"); err != nil {
		return OrderedRulesStream{}, err
	}
	if err := checkFreeText(string(admission.ViewKind), "admission view kind"); err != nil {
		return OrderedRulesStream{}, err
	}
	// AND THEIR CLOSED SETS, here rather than only in validateScope and
	// validateAdmission below.
	//
	// Both words are bounded by the two lines above, which is the only reason
	// their refusals may quote them; settling them now costs one comparison
	// against a short constant. Leaving it to the validators meant a four-byte
	// unsupported view kind was refused only after the candidate vocabulary
	// tier AND the identity pass had walked every candidate — 3.28 µs on two
	// candidates against 459.124 µs on 128 holding 4 KiB each; it is 15.646 µs
	// now. Coverage was the same defect one step earlier, at 1.527 µs against
	// 75.238 µs and 15.9 µs now, the gap smaller only because validateScope
	// precedes the identity pass and validateAdmission does not. What remains
	// in both is the aggregate charge and the constant-bounded tier walking the
	// candidates, which the ceilings bound.
	//
	// The evaluator's constant-bounded tier already compared both. This is the
	// sixth time a rule has been present on one of these two paths and absent
	// from the other, and the first where the projection was the one missing
	// it — the previous five went the other way. A reviewer found it, as with
	// the five before.
	if err := checkCoverageVocabulary(source.Scope); err != nil {
		return OrderedRulesStream{}, err
	}
	if err := checkViewKindVocabulary(admission); err != nil {
		return OrderedRulesStream{}, err
	}
	// THE CANDIDATE VOCABULARY NEXT.
	//
	// Four closed-set fields per candidate. Each is length-bounded first —
	// their refusals quote the value, which is why they cannot join the
	// constant-bounded tier — and then compared against a short constant, so the tier
	// reads at most MaxOrderedRulesCandidates x 4 x MaxOrderedRulesIdentifierBytes
	// and in practice a few words per candidate.
	//
	// It runs before validateScope and validateAdmission, before the
	// interventions, and before any identity, because those carry the large
	// text and a candidate whose source kind is outside its vocabulary depends
	// on none of them: such a candidate used to be reached only after roughly
	// 12 MB of unrelated text — the intervention detail scan and the candidate
	// identity and payload passes further down.
	//
	// It does NOT run before the three vocabulary checks immediately above, and
	// an earlier version of this comment said it did. Those three are each
	// bounded to MaxOrderedRulesIdentifierBytes before anything quotes them, so
	// they are small on either side of this loop and moving them would buy
	// nothing: the error was in the description, not in the order.
	//
	// This does NOT close the ordering axis, and the previous revision claimed
	// it did. See the note above the text tier.
	for i := range source.Candidates {
		c := &source.Candidates[i]
		where := "candidate " + strconv.Itoa(i)
		// Same rule as above, for the fields the checks below quote.
		for _, v := range [...]string{string(c.SourceKind), string(c.EpisodeMembership),
			string(c.OutcomesPresence), string(c.Balance.Presence)} {
			if err := checkFreeText(v, where+" vocabulary"); err != nil {
				return OrderedRulesStream{}, err
			}
		}
		if c.EpisodeMembership != MembershipProven {
			return OrderedRulesStream{}, errors.Join(ErrOrderedRulesMembershipUnproven,
				errors.New("predictioneval: "+where+" declares membership "+strconv.Quote(string(c.EpisodeMembership))+
					"; a shared event id, pool or nearby timestamp does not prove an episode or an account"))
		}
		switch c.SourceKind {
		case SourceKindChannelUpdate, SourceKindCalculateSnapshot:
		default:
			return OrderedRulesStream{}, errors.Join(ErrOrderedRulesVocabulary,
				errors.New("predictioneval: "+where+" has source kind "+strconv.Quote(string(c.SourceKind))))
		}
		if admission.ViewKind == ViewCalculateOnly && c.SourceKind != SourceKindCalculateSnapshot {
			return OrderedRulesStream{}, errors.Join(ErrOrderedRulesViewMismatch,
				errors.New("predictioneval: "+where+" is a "+string(c.SourceKind)+
					" inside a CALCULATE_ONLY view"))
		}
		if admission.ViewKind == ViewChannelCandidateStream && c.SourceKind != SourceKindChannelUpdate {
			return OrderedRulesStream{}, errors.Join(ErrOrderedRulesViewMismatch,
				errors.New("predictioneval: "+where+" is a "+string(c.SourceKind)+
					" inside a CHANNEL_CANDIDATE_STREAM view; a decision-time snapshot is not a wire frame"))
		}
		switch c.OutcomesPresence {
		case SuppliedKnown, SuppliedMissing, SuppliedInvalid:
		default:
			return OrderedRulesStream{}, errors.Join(ErrOrderedRulesVocabulary,
				errors.New("predictioneval: "+where+" declares outcome-vector presence "+
					strconv.Quote(string(c.OutcomesPresence))+
					"; a caller must say whether it recovered the ordered vector whole, because a short "+
					"vector the donor declines and a vector nobody could recover are different facts"))
		}
		// The NESTED presence words belong to this tier for the same reason.
		// Each is a short closed-set field whose validity depends on no
		// payload, and leaving the outcomes out meant a last outcome's invalid
		// word was reached only after every earlier candidate's provenance and
		// reason had been scanned.
		if err := checkPresenceVocabulary(c.Balance, where+" balance"); err != nil {
			return OrderedRulesStream{}, err
		}
		for j := range c.Outcomes {
			if err := checkPresenceVocabulary(c.Outcomes[j].Points,
				where+" outcome "+strconv.Itoa(j)+" points"); err != nil {
				return OrderedRulesStream{}, err
			}
		}
	}

	if err := checkInterventionVocabulary(source); err != nil {
		return OrderedRulesStream{}, err
	}

	if err := validateScope(source.Scope); err != nil {
		return OrderedRulesStream{}, err
	}
	// THE CANDIDATE IDENTITIES HERE, ahead of the three payload scans below.
	//
	// Uniqueness is the cheapest fact about the candidate list that can refuse
	// it outright, and it depends on nothing else: not the admission, not the
	// boundary, not a single byte of intervention detail. It used to be settled
	// after all three, so two candidates repeating a three-byte identity were
	// refused only once the admission references, the cutoff and every Detail
	// had been walked — attacker-supplied text the decision does not read.
	// Measured on the identical input, 1,024 references of 4 KiB beside 1,024
	// interventions carrying 4 KiB of identity and 4 KiB of detail each:
	// 7.981383 ms to report a repeated identity, against 127.186 µs here.
	//
	// This is a reordering and not a free win, which is the same thing the
	// Detail move below had to say about itself. Where the fault is in the
	// ADMISSION and the candidates are well-formed, that refusal now runs after
	// a full identity pass rather than before it — bounded by
	// MaxOrderedRulesCandidates x MaxOrderedRulesIdentifierBytes, so 512 KiB at
	// the ceiling and nothing at all for the short identities a real source
	// carries. Measured on the identical input, 128 candidates each holding a
	// near-ceiling identity beside an admission whose view kind is not in the
	// vocabulary: 67.3 µs before, 675.213 µs here.
	//
	// The trade is taken because the two envelopes are not the same size. What
	// this pass can be made to read is capped at 512 KiB; what it now refuses
	// to read first — the admission references, the cutoff's identities and the
	// intervention details — is three separate MaxOrderedRulesInterventions x
	// MaxOrderedRulesIdentifierBytes envelopes, 12 MiB between them. Paying a
	// bounded half-megabyte to stop paying an unbounded-in-practice twelve is
	// the same bargain every tier above this one makes.
	//
	// It does NOT move ahead of the vocabulary tier, and the case below pins
	// that: a closed-set word is one comparison against a short string, so a
	// source that fails it should not first pay for 128 identities either.
	seen := make(map[string]bool, len(source.Candidates))
	for i := range source.Candidates {
		c := &source.Candidates[i]
		where := "candidate " + strconv.Itoa(i)
		if err := checkIdentifier(c.Identity, where+" identity"); err != nil {
			return OrderedRulesStream{}, err
		}
		if seen[c.Identity] {
			return OrderedRulesStream{}, errors.Join(ErrOrderedRulesDuplicateIdentity,
				errors.New("predictioneval: "+where+" repeats identity "+strconv.Quote(c.Identity)+
					"; two candidates may carry identical VALUES at different positions, but never the same identity"))
		}
		seen[c.Identity] = true

	}

	if err := validateAdmission(admission); err != nil {
		return OrderedRulesStream{}, err
	}

	// THE BOUNDARY BEFORE THE PAYLOAD, and the order is the point.
	//
	// establishCutoff reads only the interventions and the declared interval.
	// It is self-bounding: every string it quotes in a refusal passes
	// checkIdentifier or checkFreeText inside establishCutoff itself first, so
	// it does not depend on any earlier scan for that — only on the
	// intervention COUNT, which is bounded at the top of this function. The
	// candidate walk below is the opposite: it is the bulk of the admitted
	// budget, and it runs entirely on caller-supplied text. Establishing the
	// boundary second meant an intervention rejectable on its shape alone — an
	// undeclared position, a kind outside the vocabulary, an empty identity —
	// was refused only after every candidate and every outcome had been
	// validated and charged. Measured: 44 µs to refuse such a source with empty
	// candidate provenance, against 5.0 ms with 600 bytes on each of 8,192
	// outcomes, and that fixture is a fraction of what the ceiling admits.
	//
	// It now also precedes the INTERVENTION payload, for the same reason one
	// step down. Detail is free text that no decision reads: nothing about the
	// boundary depends on it, and scanning it first meant the same shape
	// refusal was reached only after up to 4 MiB of it. Measured on the
	// identical input: 3.2537 ms to 21.686 µs.
	//
	// That second move is a reordering, not a free win, and saying so is the
	// honest form of it. Where the DETAIL is what is wrong and every
	// intervention is otherwise well-shaped, the scan below now runs after a
	// full establishCutoff pass rather than before it — at most one extra
	// bounded pass over the same MaxOrderedRulesInterventions x
	// MaxOrderedRulesIdentifierBytes envelope, since a vocabulary kind and a
	// relevance are short when they are valid and refused on length when they
	// are not. The trade buys an unbounded-in-practice saving on the shape path
	// for a bounded cost on the text path, and both paths stay inside the same
	// ceiling.
	//
	// The refusal it produces is the same either way; only its cost changes.
	cutoff, err := establishCutoff(source)
	if err != nil {
		return OrderedRulesStream{}, err
	}

	// The Detail SCAN, its charge already counted in the constant-bounded tier above.
	for i := range source.Interventions {
		if err := checkFreeText(source.Interventions[i].Detail, "intervention "+strconv.Itoa(i)+" detail"); err != nil {
			return OrderedRulesStream{}, err
		}
	}

	// THE PAYLOAD OF ANY CANDIDATE LAST, after the structure of all of them and
	// then the bounded strings of all of them. This is the third tier, and the
	// only one of the three that touches the budget.
	//
	// It began as a two-way split, and the header it carried then — the shape
	// of every candidate before the payload of any — was not quite true, which
	// is worth leaving on the record because the imprecision was the defect.
	// It counted a candidate IDENTITY as shape. An identity is bytes:
	// checkIdentifier scans up to MaxOrderedRulesIdentifierBytes of it and the
	// duplicate map hashes the same bytes, so a last candidate declaring no
	// position still waited on 127 identity scans. The structural pass above
	// is what the claim actually needed.
	//
	// Within one candidate the cheap checks already came first. Across
	// candidates they did not: candidate 127 declaring no causal position was
	// refused only after candidates 0 through 126 had had every outcome
	// identity, every provenance note and every presence reason walked and
	// charged, and that prefix is the bulk of what the ceiling admits.
	// Measured on the identical input, 128 candidates of 64 outcomes each
	// carrying a 512-byte identity: 5.533517 ms to 24.191 µs. The control in
	// the same run says the same thing a second way — with tiny outcome
	// identities the refusal now costs 23.92 µs, statistically the same, so it
	// is decided by shape rather than scaled by payload.
	//
	// The two passes above are bounded by MaxOrderedRulesCandidates times a
	// fixed number of strings each bounded by MaxOrderedRulesIdentifierBytes,
	// and every value either quotes has passed checkIdentifier or checkFreeText
	// first, so neither can become the cost it prevents.
	//
	// Splitting adds NO rule. Every check was already performed here, in this
	// order, on the same values; the loop was cut and nothing crossed the cut.
	// That is what keeps the invariant pass in ordered_rules.go correct without
	// a matching change — it re-establishes the same rules and answers yes or
	// no, and the divergences in this model have all been a rule on one path
	// and not the other, never a rule in a different place on the same path.
	//
	// What DOES change is which refusal a doubly-faulty source gets: a fault
	// from an earlier tier in a later candidate now wins over a later-tier
	// fault in an earlier one. Both are refusals, neither admits anything, and
	// two cases in the suite pin the order by which sentinel fires.
	for i := range source.Candidates {
		c := &source.Candidates[i]
		where := "candidate " + strconv.Itoa(i)
		if err := checkFreeText(c.Provenance, where+" provenance"); err != nil {
			return OrderedRulesStream{}, err
		}
		if err := checkFreeText(c.OutcomesReason, where+" outcomes reason"); err != nil {
			return OrderedRulesStream{}, err
		}
		// Outcome identities are unique WITHIN a candidate, for the same reason
		// candidate and intervention identities are unique within the source: a
		// pool naming the same outcome twice is not a pool that can exist, and
		// the model must not compute shares over one. It was the only identity
		// in the model without this rule.
		outcomesSeen := make(map[string]bool, len(c.Outcomes))
		for j := range c.Outcomes {
			o := &c.Outcomes[j]
			ow := where + " outcome " + strconv.Itoa(j)
			if err := checkIdentifier(o.Identity, ow+" identity"); err != nil {
				return OrderedRulesStream{}, err
			}
			if outcomesSeen[o.Identity] {
				return OrderedRulesStream{}, errors.Join(ErrOrderedRulesDuplicateIdentity,
					errors.New("predictioneval: "+ow+" repeats identity "+strconv.Quote(o.Identity)+
						"; two outcomes may carry identical POINTS, but never the same identity"))
			}
			outcomesSeen[o.Identity] = true
			if err := checkFreeText(string(o.Points.Presence), ow+" points vocabulary"); err != nil {
				return OrderedRulesStream{}, err
			}
			if err := checkPresence(o.Points, ow+" points", c.Position); err != nil {
				return OrderedRulesStream{}, err
			}
		}
		if err := checkPresence(c.Balance, where+" balance", c.Position); err != nil {
			return OrderedRulesStream{}, err
		}
	}

	stream := OrderedRulesStream{
		ContractVersion: OrderedRulesStreamContractVersion,
		Scope:           source.Scope,
		Admission:       detachAdmission(admission),
		Cutoff:          cutoff,
	}

	// The cut, then the detach. Candidates at or after the boundary are not
	// carried at all — not carried-and-marked — because a value present in the
	// stream is a value the traversal could read.
	for i := range source.Candidates {
		c := &source.Candidates[i]
		if cutoff.Established && c.Position >= cutoff.Position {
			stream.Cutoff.DroppedAtOrAfter++
			continue
		}
		stream.Candidates = append(stream.Candidates, detachCandidate(c))
	}

	if source.Scope.Coverage != CoverageCompleteDeclared {
		stream.Qualifications = append(stream.Qualifications, qualCoverageNotComplete)
	}
	if cutoff.Basis == CutoffConservativeAmbiguous {
		stream.Qualifications = append(stream.Qualifications, qualConservativeCutoff)
	}
	if cutoff.Basis == CutoffNoInterventionDeclared {
		stream.Qualifications = append(stream.Qualifications, qualNoInterventionSeen)
	}
	if admission.ViewKind == ViewCalculateOnly {
		stream.Qualifications = append(stream.Qualifications, qualCalculateOnlyView)
	}
	if stream.Cutoff.DroppedAtOrAfter > 0 {
		// A traversal that finds nothing in what SURVIVED the boundary is a
		// different piece of evidence from one over a source that held nothing.
		// Saying so here is what stops the two from reading alike downstream.
		stream.Qualifications = append(stream.Qualifications,
			qualBoundaryRemovedCandidates+strconv.Itoa(stream.Cutoff.DroppedAtOrAfter))
	}

	stream.SelectionDigest = orderedRulesStreamDigest(stream)
	return stream, nil
}

// establishCutoff finds the EARLIEST boundary among the supplied interventions.
//
// Earliest, not most convenient. An intervention whose association could not be
// established still cuts at its own position: choosing the later boundary
// because the earlier one is unproven is exactly how a model keeps evaluating a
// world that had already been acted on.
func establishCutoff(source OrderedRulesSource) (OrderedRulesCutoff, error) {
	out := OrderedRulesCutoff{Basis: CutoffNoInterventionDeclared}

	// THE STRUCTURAL PASS FIRST, for the same reason the candidate walk has
	// one: these two checks are integer comparisons whose messages quote only
	// integers, while the pass below scans an identity up to
	// MaxOrderedRulesIdentifierBytes and hashes the same bytes into the
	// duplicate map. A LAST intervention declaring no position was therefore
	// reached only after 1,023 identities had been scanned and hashed:
	// measured with 4,000-byte identities, 3.57799 ms against 163.451 µs.
	//
	// The interval endpoints are settled before this: validateScope runs above
	// establishCutoff and refuses a scope that omits them.
	if err := checkInterventionStructure(source); err != nil {
		return OrderedRulesCutoff{}, err
	}
	if err := checkInterventionVocabulary(source); err != nil {
		return OrderedRulesCutoff{}, err
	}

	seen := make(map[string]bool, len(source.Interventions))
	for i := range source.Interventions {
		in := &source.Interventions[i]
		where := "intervention " + strconv.Itoa(i)
		if err := checkIdentifier(in.Identity, where+" identity"); err != nil {
			return OrderedRulesCutoff{}, err
		}
		if seen[in.Identity] {
			return OrderedRulesCutoff{}, errors.Join(ErrOrderedRulesDuplicateIdentity,
				errors.New("predictioneval: "+where+" repeats identity "+strconv.Quote(in.Identity)))
		}
		seen[in.Identity] = true
		for _, v := range [...]string{string(in.Kind), string(in.Relevance)} {
			if err := checkFreeText(v, where+" vocabulary"); err != nil {
				return OrderedRulesCutoff{}, err
			}
		}
		switch in.Kind {
		case InterventionAutoCallStarted, InterventionManualCallStarted:
		default:
			return OrderedRulesCutoff{}, errors.Join(ErrOrderedRulesVocabulary,
				errors.New("predictioneval: "+where+" has kind "+strconv.Quote(string(in.Kind))))
		}
		var basis OrderedRulesCutoffBasis
		switch in.Relevance {
		case RelevanceExcluded:
			// Proven to belong elsewhere. It does not bound this episode.
			continue
		case RelevanceProven:
			basis = CutoffFactualIntervention
		case RelevanceAmbiguous:
			basis = CutoffConservativeAmbiguous
		default:
			return OrderedRulesCutoff{}, errors.Join(ErrOrderedRulesVocabulary,
				errors.New("predictioneval: "+where+" has relevance "+strconv.Quote(string(in.Relevance))))
		}

		switch {
		case !out.Established, in.Position < out.Position:
			out = OrderedRulesCutoff{Established: true, Position: in.Position,
				Kind: in.Kind, Identity: in.Identity, Basis: basis}
		case in.Position == out.Position && basis == CutoffFactualIntervention &&
			out.Basis == CutoffConservativeAmbiguous:
			// Same cut, stronger evidence for it. The position does not move;
			// only the honesty of the label improves.
			out = OrderedRulesCutoff{Established: true, Position: in.Position,
				Kind: in.Kind, Identity: in.Identity, Basis: basis}
		case in.Position == out.Position && basis == out.Basis && in.Identity < out.Identity:
			// Two equally strong interventions at the same position cut
			// identically, so the traversal cannot tell them apart — but the
			// one NAMED as the boundary is hashed into the result. Picking the
			// first in supply order would make the same set of facts produce
			// different evidence depending on the order they were handed over,
			// so the smaller identity wins and the choice is order-independent.
			out = OrderedRulesCutoff{Established: true, Position: in.Position,
				Kind: in.Kind, Identity: in.Identity, Basis: basis}
		}
	}
	return out, nil
}

func validateScope(s OrderedRulesScope) error {
	// LENGTH BEFORE MEANING again, and for a second reason here. These four are
	// RETAINED and CHARGED, and until this gate existed they were the only
	// charged strings with no individual bound at all — so the per-string limit
	// the rest of the projection relies on simply did not apply to them, and a
	// single one of them could take most of the aggregate on its own. An 8 MiB
	// admission population was admitted outright.
	for _, v := range [...][2]string{
		{s.Namespace, "scope namespace"},
		{s.EpisodeID, "scope episode id"},
		{s.AccountContext, "scope account context"},
		{s.AssociationEvidence, "scope association evidence"},
		// CoverageDetail belongs HERE, not at the projection's call site. The
		// invariant pass re-establishes the projection's rules by calling these
		// validators, so a retained string checked only inline in
		// ProjectOrderedRulesStream is checked on one path — and a forged
		// stream carrying one reached WOULD_ATTEMPT through the digest oracle.
		{s.CoverageDetail, "scope coverage detail"},
	} {
		if err := checkFreeText(v[0], v[1]); err != nil {
			return err
		}
	}
	if err := validateScopeShape(s); err != nil {
		return err
	}
	if err := checkSourceContractVersion(s); err != nil {
		return err
	}
	return checkCoverageVocabulary(s)
}

// checkSourceContractVersion settles the scope's source contract against the one
// constant this package projects.
//
// Extracted so ProjectOrderedRulesStream can settle it immediately after its
// length bound instead of waiting for validateScope, which runs below the
// candidate and intervention vocabulary tiers. One definition, two callers: the
// alternative is a second copy, and a rule living on one path and not the other
// is the defect this PR has now hit seven times.
//
// It quotes the supplied value, so it must stay BELOW the checkFreeText that
// bounds it — that gate exists because a 64 MiB contract version once took a
// second to refuse and built a 67 MB error message.
func checkSourceContractVersion(s OrderedRulesScope) error {
	if s.SourceContractVersion != OrderedRulesStreamContractVersion {
		return errors.Join(ErrOrderedRulesScopeIncomplete,
			errors.New("predictioneval: scope declares source contract "+strconv.Quote(s.SourceContractVersion)+
				", not "+strconv.Quote(OrderedRulesStreamContractVersion)))
	}
	return nil
}

// checkCoverageVocabulary settles the scope's coverage against its closed set.
//
// One definition, two callers, like every other vocabulary check here: the full
// validator runs it last, and the projection's vocabulary tier runs it as soon
// as checkFreeText has bounded the word. The bound has to come first because
// the refusal QUOTES the value.
func checkCoverageVocabulary(s OrderedRulesScope) error {
	switch s.Coverage {
	case CoverageCompleteDeclared, CoverageGapsPresent, CoverageTruncatedPrefix:
		return nil
	case CoverageUnknown, "":
		return ErrOrderedRulesUnknownCoverage
	default:
		return errors.Join(ErrOrderedRulesVocabulary,
			errors.New("predictioneval: scope coverage "+strconv.Quote(string(s.Coverage))))
	}
}

// checkViewKindVocabulary settles the admission's view kind against its closed
// set, on the same one-definition-two-callers footing as the coverage check.
func checkViewKindVocabulary(a CommonAdmission) error {
	switch a.ViewKind {
	case ViewChannelCandidateStream, ViewCalculateOnly:
		return nil
	default:
		return errors.Join(ErrOrderedRulesVocabulary,
			errors.New("predictioneval: admission view kind "+strconv.Quote(string(a.ViewKind))))
	}
}

func validateAdmission(a CommonAdmission) error {
	// Bounded for the same reason as the scope's four: retained, charged, and
	// previously unbounded individually.
	for _, v := range [...][2]string{
		{a.ManifestID, "admission manifest id"},
		{a.Population, "admission population"},
		{a.OrderBasis, "admission order basis"},
	} {
		if err := checkFreeText(v[0], v[1]); err != nil {
			return err
		}
	}
	// The references are retained too, so they are validated HERE for the same
	// reason the scope's coverage detail is: the invariant pass reaches them
	// only through this function. The COUNT is bounded before the loop and not
	// merely alongside it — moving the element check here without the count
	// would put an unbounded loop ahead of the bound that makes it finite,
	// which is the length-before-work rule this file applies everywhere else.
	if err := validateAdmissionShape(a); err != nil {
		return err
	}
	// The view kind is settled after the scalars above and before the payload
	// below. It is compared against a closed set, but its error QUOTES the
	// value, so it needs checkFreeText to have run first — which is why it sits
	// here and not in the shape half the projection runs before any scan.
	if err := checkViewKindVocabulary(a); err != nil {
		return err
	}

	for i, ref := range a.SourceReferences {
		if err := checkFreeText(ref, "admission source reference "+strconv.Itoa(i)); err != nil {
			return err
		}
	}
	return nil
}

// invalidUTF8 reports a string Go's JSON encoder would not reproduce.
//
// The stream carries JSON tags because it is meant to be serialized, and the
// encoder does not fail on a byte that is not valid UTF-8 — it substitutes
// U+FFFD. So a stream holding one projects, digests over the ORIGINAL bytes,
// and then comes back from its own round trip holding different bytes and a
// digest that no longer matches: an admitted stream refused as a forgery, with
// nothing forged. Measured: a namespace of "ns-\xff-tail" evaluated to
// WOULD_ATTEMPT, and the same stream after json.Marshal and json.Unmarshal
// evaluated to STREAM_SELECTION_DIGEST_MISMATCH.
//
// Refusing is the repair rather than canonicalizing before hashing, because a
// digest taken over bytes the stream does not carry witnesses something the
// caller never supplied. Narrowing what is admitted is always allowed; quietly
// hashing a substitute is not.
//
// No decoder is hand-rolled and no import is added: ranging over a string is
// the language's own UTF-8 decode, and it yields U+FFFD for a byte that is not
// part of a valid sequence. A genuine U+FFFD occupies the three bytes EF BF BD
// at that index; an invalid one does not, which is the whole distinction.
func invalidUTF8(v string) bool {
	for i, r := range v {
		if r != '\uFFFD' {
			continue
		}
		if len(v)-i < 3 || v[i] != 0xef || v[i+1] != 0xbf || v[i+2] != 0xbd {
			return true
		}
	}
	return false
}

// checkFreeText bounds a caller-chosen string the projection retains. Empty is
// allowed — these are optional — but unbounded is not.
func checkFreeText(v, where string) error {
	if len(v) > MaxOrderedRulesIdentifierBytes {
		return errors.Join(ErrOrderedRulesOverBound,
			errors.New("predictioneval: "+where+" is "+strconv.Itoa(len(v))+
				" bytes, past the bound of "+strconv.Itoa(MaxOrderedRulesIdentifierBytes)))
	}
	// Length first, then encoding: this runs after the bound above so an
	// enormous string is refused for its size without being scanned.
	if invalidUTF8(v) {
		return errors.Join(ErrOrderedRulesNotEncodable,
			errors.New("predictioneval: "+where+" is not valid UTF-8; JSON encoding would "+
				"substitute U+FFFD and the stream would no longer match its own digest"))
	}
	return nil
}

// orderedRulesMaxJSONExpansion is the most bytes one supplied byte can occupy
// once the stream is JSON-encoded.
//
// Go's encoder writes a byte below 0x20, one of < > &, or any byte that is not
// valid UTF-8 as a six-byte \uXXXX escape; nothing expands further. Six is
// therefore an upper bound for every possible input byte, and the projection
// charges it rather than the raw length.
const orderedRulesMaxJSONExpansion = 6

// orderedRulesStructuralReserveBytes is the part of the aggregate ceiling held
// back for JSON syntax rather than sold to the caller as text.
//
// chargedWidth bounds the encoded width of the CONTENT of each supplied string.
// It does not cover the quotes around it, the field names beside it, or the
// braces and commas holding the document together — and none of that is
// charged, so at the widest admitted shape it lands entirely on top of a budget
// the caller has already filled. Measured: 1,034,944 bytes of pure structure at
// MaxOrderedRulesCandidates x MaxOrderedRulesOutcomes, which put an ADMITTED
// stream 762,930 bytes past the ceiling it is supposed to sit inside.
//
// The reserve is the measured worst case with room to spare, and
// TestOrderedRulesStructuralOverheadFitsItsReserve fails if a field added later
// grows the structure past it — the point being that this is checked rather
// than assumed, since assuming it is what went wrong.
const orderedRulesStructuralReserveBytes = 4 << 20

// orderedRulesTextCeiling is the aggregate actually available to supplied text.
const orderedRulesTextCeiling = MaxOrderedRulesAggregateBytes - orderedRulesStructuralReserveBytes

// chargedWidth is what n supplied bytes cost against the aggregate budget.
//
// It is the WORST-CASE encoded width, not the actual one, and the difference is
// deliberate. Computing the actual width means decoding UTF-8 to tell a valid
// multi-byte rune from an invalid byte, and this package's production files
// import from a six-entry allowlist that has no unicode/utf8 in it — so exact
// accounting would mean hand-rolling a decoder inside the budget code, which is
// the part of this package that has been wrong before. A bound that is provably
// never exceeded is worth more here than one that is tight.
//
// The cost is that ordinary ASCII is charged six times what it will encode to,
// so the aggregate admits about a sixth of its nominal byte count. That is a
// narrowing, which is always allowed; what is NOT allowed is admitting a source
// whose encoded form breaks the declared ceiling.
func chargedWidth(n int) int64 { return int64(n) * orderedRulesMaxJSONExpansion }

// orderedRulesDroppedCandidateMinimumBytes is the least SOURCE text one removed
// candidate can have cost the projection, in the view the stream declares.
//
// A stream declaring DroppedAtOrAfter > 0 asserts that a source existed with
// that many more candidates in it, and the projection charged every one of
// them: the loop that validates and charges candidates runs BEFORE the cut, so
// a candidate the boundary removes costs exactly what one it keeps costs. The
// shape gate never sees that text — but it does not have to, because the
// vocabulary the projection forces puts a floor under it.
//
// The view-independent part, for the cheapest candidate ProjectOrderedRulesStream
// will admit: membership PROVEN (6), the only accepted value; outcome-vector
// presence KNOWN (5), since MISSING and INVALID are 7; a balance declared
// KNOWN (5) plus the single provenance byte checkPresence then demands (1),
// which is cheaper than the 7-byte MISSING or INVALID that need none; and a
// non-empty identity (1). Outcomes, candidate provenance and the outcomes
// reason may all be absent.
//
// The source kind is NOT view-independent, and treating it as though it were
// undercharged a calculate-only stream by four bytes a removal. The projection
// couples the two strictly: inside a CALCULATE_ONLY view every candidate must
// be a CALCULATE_SNAPSHOT, and inside a CHANNEL_CANDIDATE_STREAM view every
// candidate must be a CHANNEL_UPDATE. So the floor is not merely a bound in
// either view — it is the exact cost, and taking the cheaper of the two for
// both let a forged calculate-only stream sit 24 charged bytes a removal past
// what any source could have projected.
//
// The lengths are read from the constants rather than written out, so renaming
// a vocabulary word cannot leave this silently wrong. An unrecognized view gets
// the smaller floor: validateAdmission refuses such a stream in the invariant
// pass regardless, and undercharging never refuses input the projection
// admitted, which is the direction that must not break.
//
// See TestOrderedRulesAClaimedRemovalIsChargedForTheSourceItImplies.
func orderedRulesDroppedCandidateMinimumBytes(view OrderedRulesViewKind) int64 {
	const withoutSourceKind = 1 + len(MembershipProven) + len(SuppliedKnown) + len(SuppliedKnown) + 1
	if view == ViewCalculateOnly {
		return int64(withoutSourceKind + len(SourceKindCalculateSnapshot))
	}
	return int64(withoutSourceKind + len(SourceKindChannelUpdate))
}

// suppliedTextBytes is the free text one supplied value contributes.
func suppliedTextBytes(v SuppliedInt64) int64 {
	return chargedWidth(len(v.Provenance) + len(v.Reason))
}

// checkIdentifierPresent is the zero-byte half of checkIdentifier: an emptiness
// test whose message quotes nothing, so the structural tier can run it over
// every identity before any identity is scanned or hashed.
func checkIdentifierPresent(id, where string) error {
	if id == "" {
		return errors.Join(ErrOrderedRulesScopeIncomplete, errors.New("predictioneval: "+where+" is empty"))
	}
	return nil
}

func checkIdentifier(id, where string) error {
	if err := checkIdentifierPresent(id, where); err != nil {
		return err
	}
	if len(id) > MaxOrderedRulesIdentifierBytes {
		return errors.Join(ErrOrderedRulesOverBound,
			errors.New("predictioneval: "+where+" is "+strconv.Itoa(len(id))+" bytes, past the bound of "+
				strconv.Itoa(MaxOrderedRulesIdentifierBytes)))
	}
	if invalidUTF8(id) {
		return errors.Join(ErrOrderedRulesNotEncodable,
			errors.New("predictioneval: "+where+" is not valid UTF-8; JSON encoding would "+
				"substitute U+FFFD and the stream would no longer match its own digest"))
	}
	return nil
}

// checkPresence validates one supplied value's presence vocabulary and its
// availability against the candidate it is attached to.
//
// A KNOWN value that only became available AFTER its candidate is refused. That
// is the shape a borrowed balance takes — a later schedule read, a manual
// validation, a decision-time envelope — and reading one as if the candidate had
// had it is how a replay silently acquires information the original never held.
func checkPresence(v SuppliedInt64, where string, candidatePosition int64) error {
	if err := checkFreeText(v.Provenance, where+" provenance"); err != nil {
		return err
	}
	if err := checkFreeText(v.Reason, where+" reason"); err != nil {
		return err
	}
	if err := checkPresenceVocabulary(v, where); err != nil {
		return err
	}
	if v.Presence != SuppliedKnown {
		return nil
	}
	return checkPresenceShape(v, where, candidatePosition)
}

// checkPresenceVocabulary bounds a supplied value's presence word and then
// compares it against its closed set.
//
// Split out so the projection can settle every nested presence — a candidate's
// balance AND every outcome's points — in one early pass, rather than reaching
// the last outcome's word only after every earlier candidate's provenance and
// reason have been scanned. The length bound comes first because the refusal
// quotes the value, which is also why this cannot join the constant-bounded tier.
func checkPresenceVocabulary(v SuppliedInt64, where string) error {
	if err := checkFreeText(string(v.Presence), where+" presence vocabulary"); err != nil {
		return err
	}
	switch v.Presence {
	case SuppliedKnown, SuppliedMissing, SuppliedInvalid:
		return nil
	default:
		return errors.Join(ErrOrderedRulesVocabulary,
			errors.New("predictioneval: "+where+" has presence "+strconv.Quote(string(v.Presence))))
	}
}

// checkInterventionVocabulary bounds and validates the interventions' two
// closed-set fields.
//
// Same shape as checkInterventionStructure: one definition, two callers. The
// projection runs it in the vocabulary tier, before the admission references
// and before any identity is scanned or hashed, because a kind or a relevance
// outside its set depends on neither — a last intervention with a short invalid
// Kind used to be reached only after roughly 4 MiB of references and another
// 4 MiB of identities. establishCutoff runs it again because it quotes these
// values in its own refusals.
func checkInterventionVocabulary(source OrderedRulesSource) error {
	for i := range source.Interventions {
		in := &source.Interventions[i]
		where := "intervention " + strconv.Itoa(i)
		for _, v := range [...]string{string(in.Kind), string(in.Relevance)} {
			if err := checkFreeText(v, where+" vocabulary"); err != nil {
				return err
			}
		}
		switch in.Kind {
		case InterventionAutoCallStarted, InterventionManualCallStarted:
		default:
			return errors.Join(ErrOrderedRulesVocabulary,
				errors.New("predictioneval: "+where+" has kind "+strconv.Quote(string(in.Kind))))
		}
		switch in.Relevance {
		case RelevanceExcluded, RelevanceProven, RelevanceAmbiguous:
		default:
			return errors.Join(ErrOrderedRulesVocabulary,
				errors.New("predictioneval: "+where+" has relevance "+strconv.Quote(string(in.Relevance))))
		}
	}
	return nil
}

// checkInterventionStructure is the interventions' zero-byte tier: a declared
// position and the declared interval, integer work quoting only integers.
//
// The projection runs it before any text is scanned, and establishCutoff runs
// it again because it depends on it — the same definition twice rather than a
// second copy, at the cost of one bounded pass of integer comparisons.
func checkInterventionStructure(source OrderedRulesSource) error {
	for i := range source.Interventions {
		in := &source.Interventions[i]
		where := "intervention " + strconv.Itoa(i)
		if err := checkIdentifierPresent(in.Identity, where+" identity"); err != nil {
			return err
		}
		if !in.HasPosition {
			return errors.Join(ErrOrderedRulesScopeIncomplete,
				errors.New("predictioneval: "+where+" does not declare its position as supplied; an omitted "+
					"one would cut the stream at zero and remove every candidate after it"))
		}
		if in.Position < source.Scope.IntervalFromPosition || in.Position > source.Scope.IntervalToPosition {
			return errors.Join(ErrOrderedRulesOutsideDeclaredInterval,
				errors.New("predictioneval: "+where+" at position "+strconv.FormatInt(in.Position, 10)+
					" lies outside the declared interval"))
		}
	}
	return nil
}

// validateScopeShape is the half of validateScope that reads no supplied byte.
//
// Emptiness tests and two integer comparisons, quoting nothing. It exists so
// ProjectOrderedRulesStream can settle the scope's structure — and in
// particular the declared INTERVAL, which the candidate and intervention
// structural passes compare against — before any text anywhere is scanned.
// validateScope still calls it, so the invariant pass in ordered_rules.go
// re-establishes exactly these rules through one definition rather than a copy.
//
// SourceContractVersion is NOT here: its refusal quotes the supplied value, so
// it needs the per-string bound to have run first.
func validateScopeShape(s OrderedRulesScope) error {
	switch {
	case s.Namespace == "":
		return errors.Join(ErrOrderedRulesScopeIncomplete, errors.New("predictioneval: scope namespace is empty"))
	case s.EpisodeID == "":
		return errors.Join(ErrOrderedRulesScopeIncomplete, errors.New("predictioneval: scope episode id is empty"))
	case s.AccountContext == "":
		return errors.Join(ErrOrderedRulesScopeIncomplete, errors.New("predictioneval: scope account context is empty"))
	case s.AssociationEvidence == "":
		return errors.Join(ErrOrderedRulesScopeIncomplete,
			errors.New("predictioneval: scope carries no association evidence; a pool or session id does not prove an account"))
	case !s.HasInterval:
		return errors.Join(ErrOrderedRulesScopeIncomplete,
			errors.New("predictioneval: scope does not declare its interval endpoints as supplied; an omitted "+
				"pair decodes to [0,0], which is a real interval and would silently admit only position zero"))
	case s.IntervalFromPosition > s.IntervalToPosition:
		return errors.Join(ErrOrderedRulesScopeIncomplete, errors.New("predictioneval: declared interval is empty"))
	}
	return nil
}

// validateAdmissionShape is the half of validateAdmission that reads no
// supplied byte: three emptiness tests and one slice length.
//
// The view kind is NOT here, for the same reason SourceContractVersion is not
// in the scope's half: its refusal quotes the supplied value.
func validateAdmissionShape(a CommonAdmission) error {
	if len(a.SourceReferences) > MaxOrderedRulesSourceReferences {
		return errors.Join(ErrOrderedRulesOverBound,
			errors.New("predictioneval: "+strconv.Itoa(len(a.SourceReferences))+
				" admission source references exceed the bound of "+
				strconv.Itoa(MaxOrderedRulesSourceReferences)))
	}
	switch {
	case a.ManifestID == "":
		return errors.Join(ErrOrderedRulesAdmissionIncomplete, errors.New("predictioneval: admission manifest id is empty"))
	case a.Population == "":
		return errors.Join(ErrOrderedRulesAdmissionIncomplete, errors.New("predictioneval: admission names no population"))
	case a.OrderBasis == "":
		return errors.Join(ErrOrderedRulesAdmissionIncomplete, errors.New("predictioneval: admission names no order basis"))
	}
	return nil
}

// checkPresenceShape is the half of checkPresence whose cost does not grow with
// the LENGTH of anything supplied.
//
// It is its own function so the projection's constant-bounded pass can run it
// over every nested value before any payload text is scanned, without a second
// copy of the rules to drift from. An emptiness test, a declared flag and an
// integer comparison, and the only free text any message carries is the
// caller-supplied `where` the projection builds itself.
//
// It does NOT read zero supplied bytes, and an earlier version of this comment
// said it did. The Presence comparison rejects an ENORMOUS word on length, but
// a word of the same length as SuppliedKnown is compared byte for byte — five
// bytes at most, fixed by this file rather than by the caller. That is the
// property worth claiming, and the vocabulary that refuses an invalid word
// still lives in checkPresence, after the bound.
func checkPresenceShape(v SuppliedInt64, where string, candidatePosition int64) error {
	if v.Presence != SuppliedKnown {
		return nil
	}
	if v.Provenance == "" {
		return errors.Join(ErrOrderedRulesScopeIncomplete,
			errors.New("predictioneval: "+where+" is KNOWN but carries no provenance"))
	}
	// Availability must be SUPPLIED, not inferred from the zero value. Without
	// this the back-dating check below compares against a position the caller
	// never declared, and a value decoded from a payload that simply omits the
	// field would clear it for every candidate at position zero or later.
	if !v.HasAvailableAtPosition {
		return errors.Join(ErrOrderedRulesScopeIncomplete,
			errors.New("predictioneval: "+where+" is KNOWN but does not declare the position it became "+
				"available at; position zero is a real position, so an omitted one cannot be read as it"))
	}
	if v.AvailableAtPosition > candidatePosition {
		return errors.Join(ErrOrderedRulesBackdatedValue,
			errors.New("predictioneval: "+where+" became available at position "+
				strconv.FormatInt(v.AvailableAtPosition, 10)+" but is attached to a candidate at position "+
				strconv.FormatInt(candidatePosition, 10)+
				"; declare it MISSING rather than back-dating a value the candidate could not have read"))
	}
	return nil
}

func detachCandidate(c *OrderedRulesCandidate) OrderedRulesCandidate {
	out := *c
	if c.Outcomes != nil {
		out.Outcomes = make([]OrderedRulesOutcome, len(c.Outcomes))
		copy(out.Outcomes, c.Outcomes)
	}
	return out
}

func detachAdmission(a CommonAdmission) CommonAdmission {
	out := a
	if a.SourceReferences != nil {
		out.SourceReferences = make([]string, len(a.SourceReferences))
		copy(out.SourceReferences, a.SourceReferences)
	}
	return out
}
