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
	// LENGTH BEFORE MEANING, for every value that can reach an error formatter.
	//
	// The validators below quote the value they reject, and strconv.Quote scans
	// the whole string and allocates an expanded copy. A vocabulary field is a
	// closed set of short words, so an enormous one is not a near-miss: it is
	// an input whose only effect is the cost of refusing it. Measured before
	// this gate, a 64 MiB contract version took a second to refuse and built a
	// 67 MB error message — the refusal was the denial of service.
	if err := checkFreeText(source.Scope.SourceContractVersion, "scope source contract version"); err != nil {
		return OrderedRulesStream{}, err
	}
	if err := checkFreeText(string(source.Scope.Coverage), "scope coverage"); err != nil {
		return OrderedRulesStream{}, err
	}
	if err := checkFreeText(string(admission.ViewKind), "admission view kind"); err != nil {
		return OrderedRulesStream{}, err
	}
	if err := validateScope(source.Scope); err != nil {
		return OrderedRulesStream{}, err
	}
	if err := validateAdmission(admission); err != nil {
		return OrderedRulesStream{}, err
	}
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

	// Every caller-supplied string the projection RETAINS is counted, not just
	// the identifiers: coverage detail, the admission manifest and each
	// intervention's detail all survive into the stream, so a budget that
	// skipped them would bound the wrong thing.
	bytes := chargedWidth(len(source.Scope.Namespace) + len(source.Scope.EpisodeID) +
		len(source.Scope.AccountContext) + len(source.Scope.AssociationEvidence) +
		len(source.Scope.CoverageDetail) + len(source.Scope.SourceContractVersion) +
		len(source.Scope.Coverage))
	if err := checkFreeText(source.Scope.CoverageDetail, "scope coverage detail"); err != nil {
		return OrderedRulesStream{}, err
	}
	bytes += chargedWidth(len(admission.ManifestID) + len(admission.Population) + len(admission.OrderBasis) +
		len(admission.ViewKind))
	if len(admission.SourceReferences) > MaxOrderedRulesSourceReferences {
		return OrderedRulesStream{}, errors.Join(ErrOrderedRulesOverBound,
			errors.New("predictioneval: "+strconv.Itoa(len(admission.SourceReferences))+
				" admission source references exceed the bound of "+
				strconv.Itoa(MaxOrderedRulesSourceReferences)))
	}
	for i, ref := range admission.SourceReferences {
		if err := checkFreeText(ref, "admission source reference "+strconv.Itoa(i)); err != nil {
			return OrderedRulesStream{}, err
		}
		bytes += chargedWidth(len(ref))
	}
	for i := range source.Interventions {
		if err := checkFreeText(source.Interventions[i].Detail, "intervention "+strconv.Itoa(i)+" detail"); err != nil {
			return OrderedRulesStream{}, err
		}
		bytes += chargedWidth(len(source.Interventions[i].Identity) + len(source.Interventions[i].Detail))
	}
	if bytes > MaxOrderedRulesAggregateBytes {
		return OrderedRulesStream{}, errors.Join(ErrOrderedRulesOverBound,
			errors.New("predictioneval: supplied scope, admission and intervention bytes exceed the aggregate budget"))
	}

	seen := make(map[string]bool, len(source.Candidates))
	var lastPosition int64
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
		// Same rule as above, for the fields the checks below quote.
		for _, v := range [...]string{string(c.SourceKind), string(c.EpisodeMembership),
			string(c.OutcomesPresence), string(c.Balance.Presence)} {
			if err := checkFreeText(v, where+" vocabulary"); err != nil {
				return OrderedRulesStream{}, err
			}
			bytes += chargedWidth(len(v))
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
		if len(c.Outcomes) > MaxOrderedRulesOutcomes {
			return OrderedRulesStream{}, errors.Join(ErrOrderedRulesOverBound,
				errors.New("predictioneval: "+where+" carries "+strconv.Itoa(len(c.Outcomes))+
					" outcomes, past the bound of "+strconv.Itoa(MaxOrderedRulesOutcomes)))
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
		if err := checkFreeText(c.Provenance, where+" provenance"); err != nil {
			return OrderedRulesStream{}, err
		}
		if err := checkFreeText(c.OutcomesReason, where+" outcomes reason"); err != nil {
			return OrderedRulesStream{}, err
		}
		bytes += chargedWidth(len(c.Identity) + len(c.Provenance) + len(c.OutcomesReason))
		for j := range c.Outcomes {
			o := &c.Outcomes[j]
			ow := where + " outcome " + strconv.Itoa(j)
			if err := checkIdentifier(o.Identity, ow+" identity"); err != nil {
				return OrderedRulesStream{}, err
			}
			if err := checkFreeText(string(o.Points.Presence), ow+" points vocabulary"); err != nil {
				return OrderedRulesStream{}, err
			}
			if err := checkPresence(o.Points, ow+" points", c.Position); err != nil {
				return OrderedRulesStream{}, err
			}
			bytes += chargedWidth(len(o.Identity)+len(o.Points.Presence)) + suppliedTextBytes(o.Points)
		}
		if err := checkPresence(c.Balance, where+" balance", c.Position); err != nil {
			return OrderedRulesStream{}, err
		}
		bytes += suppliedTextBytes(c.Balance)
		if bytes > MaxOrderedRulesAggregateBytes {
			return OrderedRulesStream{}, errors.Join(ErrOrderedRulesOverBound,
				errors.New("predictioneval: supplied identifier bytes exceed the aggregate budget"))
		}
	}

	cutoff, err := establishCutoff(source)
	if err != nil {
		return OrderedRulesStream{}, err
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
		if !in.HasPosition {
			return OrderedRulesCutoff{}, errors.Join(ErrOrderedRulesScopeIncomplete,
				errors.New("predictioneval: "+where+" does not declare its position as supplied; an omitted "+
					"one would cut the stream at zero and remove every candidate after it"))
		}
		if in.Position < source.Scope.IntervalFromPosition || in.Position > source.Scope.IntervalToPosition {
			return OrderedRulesCutoff{}, errors.Join(ErrOrderedRulesOutsideDeclaredInterval,
				errors.New("predictioneval: "+where+" at position "+strconv.FormatInt(in.Position, 10)+
					" lies outside the declared interval"))
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
	case s.SourceContractVersion != OrderedRulesStreamContractVersion:
		return errors.Join(ErrOrderedRulesScopeIncomplete,
			errors.New("predictioneval: scope declares source contract "+strconv.Quote(s.SourceContractVersion)+
				", not "+strconv.Quote(OrderedRulesStreamContractVersion)))
	case !s.HasInterval:
		return errors.Join(ErrOrderedRulesScopeIncomplete,
			errors.New("predictioneval: scope does not declare its interval endpoints as supplied; an omitted "+
				"pair decodes to [0,0], which is a real interval and would silently admit only position zero"))
	case s.IntervalFromPosition > s.IntervalToPosition:
		return errors.Join(ErrOrderedRulesScopeIncomplete, errors.New("predictioneval: declared interval is empty"))
	}
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

func validateAdmission(a CommonAdmission) error {
	switch {
	case a.ManifestID == "":
		return errors.Join(ErrOrderedRulesAdmissionIncomplete, errors.New("predictioneval: admission manifest id is empty"))
	case a.Population == "":
		return errors.Join(ErrOrderedRulesAdmissionIncomplete, errors.New("predictioneval: admission names no population"))
	case a.OrderBasis == "":
		return errors.Join(ErrOrderedRulesAdmissionIncomplete, errors.New("predictioneval: admission names no order basis"))
	}
	switch a.ViewKind {
	case ViewChannelCandidateStream, ViewCalculateOnly:
		return nil
	default:
		return errors.Join(ErrOrderedRulesVocabulary,
			errors.New("predictioneval: admission view kind "+strconv.Quote(string(a.ViewKind))))
	}
}

// checkFreeText bounds a caller-chosen string the projection retains. Empty is
// allowed — these are optional — but unbounded is not.
func checkFreeText(v, where string) error {
	if len(v) > MaxOrderedRulesIdentifierBytes {
		return errors.Join(ErrOrderedRulesOverBound,
			errors.New("predictioneval: "+where+" is "+strconv.Itoa(len(v))+
				" bytes, past the bound of "+strconv.Itoa(MaxOrderedRulesIdentifierBytes)))
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

// suppliedTextBytes is the free text one supplied value contributes.
func suppliedTextBytes(v SuppliedInt64) int64 {
	return chargedWidth(len(v.Provenance) + len(v.Reason))
}

func checkIdentifier(id, where string) error {
	if id == "" {
		return errors.Join(ErrOrderedRulesScopeIncomplete, errors.New("predictioneval: "+where+" is empty"))
	}
	if len(id) > MaxOrderedRulesIdentifierBytes {
		return errors.Join(ErrOrderedRulesOverBound,
			errors.New("predictioneval: "+where+" is "+strconv.Itoa(len(id))+" bytes, past the bound of "+
				strconv.Itoa(MaxOrderedRulesIdentifierBytes)))
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
	switch v.Presence {
	case SuppliedMissing, SuppliedInvalid:
		return nil
	case SuppliedKnown:
	default:
		return errors.Join(ErrOrderedRulesVocabulary,
			errors.New("predictioneval: "+where+" has presence "+strconv.Quote(string(v.Presence))))
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
