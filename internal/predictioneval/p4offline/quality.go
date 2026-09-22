package p4offline

import (
	"errors"
)

// SEAM 12: typed quality, presence and exclusion handling.
//
// Three rules hold everywhere in this package and are enforced by these
// types rather than by convention:
//
//   - a value that is not KNOWN carries WHY, and its Value field is not read;
//   - quality is only ever lowered — there is no operation that raises it, so
//     a later fact cannot repair an earlier missing value;
//   - a value outside a closed vocabulary is refused, never mapped onto a
//     plausible member of it — including a quality record's OWN quality,
//     which a zero value or a decoded document can leave empty: such a record
//     is EXCLUDED the moment anything touches it.
//
// Every operation on a record works on a private copy of its reason and
// history lists, so two records branched from one parent cannot rewrite each
// other's audit trail through a shared backing array.

// Presence is the closed presence vocabulary of a fact.
type Presence string

// Presence values.
const (
	// PresenceKnown is a value established by evidence; Value may be read.
	PresenceKnown Presence = "KNOWN"
	// PresenceUnknown is a value the evidence does not establish. It is never
	// zero, never false, never skip.
	PresenceUnknown Presence = "UNKNOWN"
	// PresenceNotApplicable is a value that does not exist for this case by
	// the protocol's own definition — a POLICY_SKIP has no placement.
	PresenceNotApplicable Presence = "NOT_APPLICABLE"
	// PresenceNotReached is a stage the evaluation ended before. Its value
	// does not exist and no zero stands in for it.
	PresenceNotReached Presence = "NOT_REACHED"
)

// Int64Fact is one signed quantity with its presence and reason.
//
// Value is meaningful ONLY when Presence is [PresenceKnown]; every consumer in
// this package checks the presence first, and every producer sets Value to
// zero beside a non-KNOWN presence so a leaked read is at least visible.
type Int64Fact struct {
	Presence Presence `json:"presence"`
	Value    int64    `json:"value"`
	Reason   string   `json:"reason,omitempty"`
}

// KnownInt64 is a KNOWN fact.
func KnownInt64(v int64) Int64Fact { return Int64Fact{Presence: PresenceKnown, Value: v} }

// UnknownInt64 is an UNKNOWN fact carrying its reason.
func UnknownInt64(reason string) Int64Fact {
	return Int64Fact{Presence: PresenceUnknown, Reason: reason}
}

// NotApplicableInt64 is a NOT_APPLICABLE fact carrying its reason.
func NotApplicableInt64(reason string) Int64Fact {
	return Int64Fact{Presence: PresenceNotApplicable, Reason: reason}
}

// Known reports whether the value may be read.
func (f Int64Fact) Known() bool { return f.Presence == PresenceKnown }

// Quality is the closed case-quality vocabulary, ordered.
type Quality string

// Quality values.
const (
	// QualityPrimaryScorable — the case is admitted for COUNTING: its
	// evidence is complete, its boundary is proven, both policies are
	// derived decisions mapping to a determinate native action, any choice
	// made lies inside the resolution's outcome set, and the resolution
	// names a winner. It is the only quality any denominator counts
	// ([AssessDenominatorMembership]). Whether EACH policy's decision then
	// enters a denominator is a per-policy fact stated by the payout seam:
	// a POLICY_SKIP or a NO_ATTEMPT_IN_SUPPLIED_PREFIX is a visible
	// non-member of a scorable case, never a reason to drop the case or to
	// count the policy wrong.
	QualityPrimaryScorable Quality = "PRIMARY_SCORABLE"
	// QualityDescriptiveOnly — the case is admitted evidence but cannot
	// contribute to the primary metric; it is described, never counted.
	QualityDescriptiveOnly Quality = "DESCRIPTIVE_ONLY"
	// QualityExcluded — the case is not evidence at all.
	QualityExcluded Quality = "EXCLUDED"
)

// rank orders qualities; an unrecognised quality ranks BELOW excluded so that
// a value outside the vocabulary can never win a merge.
func (q Quality) rank() int {
	switch q {
	case QualityPrimaryScorable:
		return 2
	case QualityDescriptiveOnly:
		return 1
	case QualityExcluded:
		return 0
	default:
		return -1
	}
}

// QualityStep records one downgrade.
type QualityStep struct {
	Quality Quality `json:"quality"`
	Reason  string  `json:"reason"`
}

// QualityRecord is a case's quality together with every reason it was lowered.
//
// It starts at [QualityPrimaryScorable] and can only go down. That is not
// optimism: every seam that can disqualify a case records its downgrade here,
// and [AssessCaseQuality] assembles the MINIMUM over all of them. A record
// read before that assembly is a partial verdict, not the case's quality.
type QualityRecord struct {
	Quality Quality       `json:"quality"`
	Reasons []string      `json:"reasons,omitempty"`
	History []QualityStep `json:"history,omitempty"`
	// ProcessingComplete reports that the evidence this verdict rests on was
	// actually PRODUCED. It is a separate axis from Quality, and it is the one
	// field on this record that is not about the episode at all.
	//
	// AN EXHAUSTED IMPLEMENTATION BUDGET IS NOT A COHORT EXCLUSION. When
	// SelectEpisodes aborts on a dataset's shape, it establishes that
	// processing did not complete -- not that the source episode is invalid.
	// Both outcomes are EXCLUDED and fail-closed, so Quality alone cannot tell
	// them apart, and a consumer that reads only Quality would count an
	// operational abort as an ordinary exclusion and publish a denominator as
	// though the dataset had been read.
	//
	// THE SENSE IS DELIBERATE: the zero value is FALSE, so a record that was
	// never populated -- a zero value, a decoded document from an older
	// producer, a struct built by a caller -- reads as NOT complete. Unknown
	// processing status must never silently assert completion.
	ProcessingComplete bool `json:"processingComplete"`
}

// selectionRan reports whether an error from [lookupEpisode] came from a
// selection that ACTUALLY RAN over the dataset and reached a verdict about this
// episode, as opposed to one that never produced a selection at all.
//
// THE PREDICATE IS THE WAY ROUND IT IS ON PURPOSE. Only [ErrEpisodeNotSelected]
// is an outcome of a completed selection -- the session was refused, or the
// episode is absent, excluded, unusable or unproven -- and lookupEpisode raises
// it at exactly the three sites that run AFTER SelectEpisodes returned no
// error.
// Every other error reaches that seam verbatim from SelectEpisodes itself: the
// caller-contract violation for records out of causal order, and the resource
// refusal. NEITHER PRODUCES A SELECTION, so neither establishes anything about
// this episode. (Only the first of them is literally unread: the causal-order
// check runs before anything else. The resource refusal abandons the episode
// loop part-way and returns the ZERO selection, which is the same thing from
// here -- no verdict, not a partial one.)
//
// An earlier version enumerated the ABORT sentinels instead and matched only
// ErrEvidenceRetention, which left the causal-order error asserting completion
// -- the exact fail-open this field exists to close, found by an independent
// review lane. Enumerating the complete case rather than the incomplete one
// fails safe on an error kind nobody has thought of yet, which is the only
// direction worth being wrong in here.
func selectionRan(err error) bool {
	return err == nil || errors.Is(err, ErrEpisodeNotSelected)
}

// QualityReasonOutsideVocabulary marks a record whose own quality was not in
// the vocabulary when it was touched — a zero value or a decoded document.
const QualityReasonOutsideVocabulary = "QUALITY_OUTSIDE_VOCABULARY"

// NewQualityRecord starts a record at the top of the ladder, and is the ONLY
// thing in this package that asserts ProcessingComplete. A record a caller
// composes by hand carries the zero value and is therefore incomplete, so it
// lowers every record it is merged with. That direction is deliberate — see
// [QualityRecord.ProcessingComplete] — and a caller that did read its evidence
// starts here rather than from a struct literal.
func NewQualityRecord() QualityRecord {
	return QualityRecord{Quality: QualityPrimaryScorable, ProcessingComplete: true}
}

// normalised returns a DETACHED copy of the record — its own reason and
// history arrays — with an out-of-vocabulary quality turned into EXCLUDED, so
// the zero value cannot be immune to a downgrade or win a merge.
func (r QualityRecord) normalised() QualityRecord {
	r.Reasons = cloneStrings(r.Reasons)
	r.History = append([]QualityStep(nil), r.History...)
	if r.Quality.rank() < 0 {
		r.Quality = QualityExcluded
		r.Reasons = appendOnce(r.Reasons, QualityReasonOutsideVocabulary)
		r.History = append(r.History, QualityStep{Quality: QualityExcluded, Reason: QualityReasonOutsideVocabulary})
	}
	return r
}

// Downgrade RETURNS a lowered record; it does not modify the receiver. THE
// RESULT MUST BE ASSIGNED. `rec.Downgrade(QualityExcluded, reason)` in
// statement position compiles, `go vet` says nothing, and the record stays
// where it was — which for this type means it stays at the TOP of the ladder,
// the exact inverse of the ladder's own safety property. The value semantics
// are deliberate (normalised() detaches the slices so a record can branch),
// and the name is kept because the seam is approved; the obligation is stated
// here instead, and a test asserts the receiver is unchanged so the decision
// cannot drift silently.
//
// A call that would RAISE the quality changes nothing except the recorded
// reason, which is kept so the attempt is visible; an unrecognised quality —
// as the target or as the receiver's own — is treated as EXCLUDED.
func (r QualityRecord) Downgrade(q Quality, reason string) QualityRecord {
	r = r.normalised()
	if q.rank() < 0 {
		q = QualityExcluded
	}
	r.Reasons = appendOnce(r.Reasons, reason)
	if q.rank() < r.Quality.rank() {
		r.Quality = q
		r.History = append(r.History, QualityStep{Quality: q, Reason: reason})
	}
	return r
}

// Merge RETURNS the combination; it does not modify either receiver, and the
// result must be assigned — see [QualityRecord.Downgrade]. The LOWER quality
// wins, and every reason is kept.
func (r QualityRecord) Merge(other QualityRecord) QualityRecord {
	out := r.normalised()
	other = other.normalised()
	// AN OPERATIONAL ABORT SURVIVES EVERY MERGE. Quality takes the lower of the
	// two; completeness takes the AND, so merging an incomplete record with a
	// complete one yields incomplete. The asymmetry is the point: a later
	// successful read of some OTHER evidence cannot make a dataset that was
	// never read look as though it had been.
	out.ProcessingComplete = out.ProcessingComplete && other.ProcessingComplete
	if other.Quality.rank() < out.Quality.rank() {
		out.Quality = other.Quality
	}
	// appendOnce SCANS THE ACCUMULATED LIST ON EVERY CALL, so this loop is
	// O(n*m) in two slices the caller owns -- Reasons is exported and
	// caller-settable. Measured through this exported method at ~3.9x per 2x
	// input: 3.7 ms at 2,000 distinct reasons, 223 ms at 16,000 -- 60x over
	// three doublings, which an independent revert of this loop reproduced at
	// 3.94x per doubling. (An earlier version of this line said 4.6x, which its
	// own two anchors contradict; the shape and the repair were never in
	// question, only my arithmetic.) It is the
	// fourth superlinear accumulation found on this branch and the same shape
	// as the manual-signal one repaired in evidence.go, so it gets the same
	// repair rather than a note: one set built once. No production path
	// reaches a large n -- AssessCaseQuality merges only records it built, each
	// carrying a handful of closed-vocabulary reasons -- which is why this was
	// MINOR, not why it should stay.
	seen := make(map[string]bool, len(out.Reasons)+len(other.Reasons))
	for _, reason := range out.Reasons {
		seen[reason] = true
	}
	for _, reason := range other.Reasons {
		if seen[reason] {
			continue
		}
		seen[reason] = true
		out.Reasons = append(out.Reasons, reason)
	}
	out.History = append(out.History, other.History...)
	return out
}

// Quality reasons. Closed vocabulary.
const (
	QualityReasonEpisodeExcluded = "EPISODE_EXCLUDED"
	// QualityReasonSelectionUnavailable is recorded when the episode's
	// selection could not be RUN at all -- the dataset was refused on its
	// shape, or handed over out of causal order -- as distinct from an episode
	// the selection ran and excluded. Both are EXCLUDED and neither is
	// recoverable here; what differs is what an auditor can conclude. An
	// earlier version collapsed the two into EPISODE_EXCLUDED, so a refusal of
	// the whole dataset was indistinguishable from evidence about this episode.
	QualityReasonSelectionUnavailable         = "SELECTION_UNAVAILABLE"
	QualityReasonFactsetDigestMismatch        = "FACTSET_DIGEST_MISMATCH"
	QualityReasonFactsetBindingMismatch       = "FACTSET_BINDING_MISMATCH"
	QualityReasonFactsetPrefix                = "FACTSET_"
	QualityReasonPolicyBindingMismatch        = "POLICY_BINDING_MISMATCH"
	QualityReasonNotDeterminateSuffix         = "_NOT_DETERMINATE:"
	QualityReasonChoiceOutsideSetSuffix       = "_CHOICE_NOT_IN_RESOLUTION_OUTCOME_SET"
	QualityReasonPolicyDecisionNotDerived     = "POLICY_DECISION_NOT_DERIVED"
	QualityReasonDecisionOnNonEvaluable       = "POLICY_DECISION_ON_NON_EVALUABLE_FACTSET"
	QualityReasonResolutionDigestMismatch     = PayoutReasonResolutionDigestMismatch
	QualityReasonResolutionNotDerivable       = PayoutReasonResolutionNotDerivable
	QualityReasonResolutionRoundMismatch      = PayoutReasonResolutionRoundMismatch
	QualityReasonResolutionOutcomeSetMismatch = PayoutReasonResolutionOutcomeSetMismatch
	QualityReasonResolutionRefund             = "RESOLUTION_REFUND"
	QualityReasonResolutionUnknown            = PayoutReasonResolutionUnknown
)

// determinate reports whether a class is a settled policy answer: an attempt,
// a skip, an admission without a stake, or a no-attempt prefix. Anything
// else could not be decided.
func determinate(c ActionClass) bool {
	switch c {
	case ActionWouldAttempt, ActionPolicySkip, ActionParticipationAdmittedStakeUnknown, ActionNoAttemptInSuppliedPrefix:
		return true
	}
	return false
}

// AssessCaseQuality is seam 12: the case's quality as the MINIMUM over every
// seam's verdict. It only ever lowers.
//
// It takes a PREPARED dataset, not a selection a caller built: the episode's
// admission and its boundary were re-derived from the raw evidence by
// [PrepareDataset], and the factset must still be exactly what that dataset
// derives for its episode, which is re-checked here. An EvidenceSelection is
// an exported value anyone can build, and that is why no seam accepts one --
// the handle owns a selection this package produced and hands out no view of
// it. An unprepared handle is refused as [ErrSelectionNotPrepared], which
// selectionRan reads as a selection that did not run.
//
// The approved denominator semantics are per policy: the primary
// POLICY_CHOICE_ACCURACY denominator is the resolved WOULD_ATTEMPT decisions
// of each policy, so a case in which one policy skipped and the other
// attempted is PRIMARY_SCORABLE — the attempt counts for its policy, the skip
// is a visible non-member ([PayoutEvidence.PrimaryDenominatorMember]) and is
// never counted wrong. This function therefore judges the CASE: whether its
// evidence, its boundary, both decisions and the resolution are what the
// primary metric may read at all.
//
// THE RETURNED RECORD CARRIES TWO INDEPENDENT AXES. Quality is about the
// episode; [QualityRecord.ProcessingComplete] is about whether the evidence
// behind that verdict was produced at all. When the evidence seam aborts on a
// dataset's shape ([ErrEvidenceRetention]) the verdict is EXCLUDED and
// ProcessingComplete is false -- the same Quality an ordinarily excluded
// episode gets, which is why the second axis exists.
//
// A CALLER THAT AGGREGATES MUST FAIL STOP ON AN INCOMPLETE RECORD. This
// function is per case and never sees the set, so it cannot enforce it: a
// future evaluation that assesses many datasets and then counts, averages or
// publishes over them must stop and report the incomplete dataset, not drop it
// and present the remainder's numbers as the run's. Skipping it silently
// produces a completed-looking metric over a cohort nobody chose.
func AssessCaseQuality(src PreparedDataset, fs CommonFactset, p2, p3b PolicyDecision, res ResolutionArtifact) QualityRecord {
	episode := NewQualityRecord()
	ep, found, err := lookupEpisode(src, fs.Episode)
	if found {
		episode = episode.Merge(ep.Quality)
	}
	if err != nil {
		// A selection that could not RUN says nothing about this episode, and
		// recording it as though it did is a claim about evidence that was never
		// read. Both verdicts are EXCLUDED and fail-closed; only the reason
		// differs, which is exactly what an auditor reads.
		reason := QualityReasonEpisodeExcluded
		if !selectionRan(err) {
			// NOT A COHORT EXCLUSION. The selection never produced a verdict --
			// the budget was exhausted, or the caller handed the records over
			// out of causal order -- so nothing was established about this
			// episode. The reason says so in prose and ProcessingComplete says
			// so to a machine, because prose alone cannot stop a consumer
			// counting it.
			reason = QualityReasonSelectionUnavailable
			episode.ProcessingComplete = false
		}
		episode = episode.Downgrade(QualityExcluded, reason)
	}

	factset := NewQualityRecord()
	verr := VerifyCommonFactset(fs)
	if verr != nil {
		factset = factset.Downgrade(QualityExcluded, QualityReasonFactsetDigestMismatch)
	} else if err == nil {
		rebuilt, berr := buildFactset(ep)
		if berr != nil || rebuilt.Digest != fs.Digest {
			factset = factset.Downgrade(QualityExcluded, QualityReasonFactsetBindingMismatch)
		}
	}
	// The completeness label is echoed into the reasons only from a VERIFIED
	// factset, whose labels were re-derived from its values; an unverifiable
	// factset's label is not this package's and never reaches the reasons.
	// This function reads the label once more, to refuse a decision on a
	// factset that is not COMPLETE; that branch appends at most one fixed
	// constant. A factset that fails verification is EXCLUDED above and the
	// merge floors there, so its label cannot raise the verdict — what it
	// can still change is WHICH of this package's own reasons are recorded.
	if verr == nil && fs.Completeness != FactsetComplete {
		factset = factset.Downgrade(QualityDescriptiveOnly, QualityReasonFactsetPrefix+string(fs.Completeness))
	}

	resolution := NewQualityRecord()
	resolved := false
	if rerr := VerifyResolutionArtifact(res); rerr != nil {
		why := QualityReasonResolutionDigestMismatch
		if errors.Is(rerr, ErrResolutionNotDerivable) {
			why = QualityReasonResolutionNotDerivable
		}
		resolution = resolution.Downgrade(QualityExcluded, why)
	} else if res.Round.EventID == "" || res.Round.EventID != fs.Episode.EventID {
		resolution = resolution.Downgrade(QualityExcluded, QualityReasonResolutionRoundMismatch)
	} else if fs.OutcomesPresent && !sameIDs(outcomeIDs(fs), res.OrderedOutcomeIDs) {
		resolution = resolution.Downgrade(QualityExcluded, QualityReasonResolutionOutcomeSetMismatch)
	} else {
		switch res.Outcome {
		case ResolutionWinnerKnown:
			resolved = true
		case ResolutionRefund:
			resolution = resolution.Downgrade(QualityDescriptiveOnly, QualityReasonResolutionRefund)
		default:
			resolution = resolution.Downgrade(QualityDescriptiveOnly, QualityReasonResolutionUnknown)
		}
	}

	policies := NewQualityRecord()
	// A factset that is not COMPLETE is evaluable by neither policy, so no
	// decision can exist for it: the policy slots are not read, and a
	// decision handed in anyway is named as such. The case is descriptive
	// by its factset already.
	if fs.Completeness != FactsetComplete {
		if p2.Policy != "" || p3b.Policy != "" {
			policies = policies.Downgrade(QualityDescriptiveOnly, QualityReasonDecisionOnNonEvaluable)
		}
		return episode.Merge(factset).Merge(resolution).Merge(policies)
	}
	// P2 is a pure function of the factset, so the P2 decision handed in must
	// be exactly what evaluating this factset yields — action, choice, stake
	// and the binding it names. (P3b needs a ruleset and entropy coordinates
	// the quality seam does not hold; its derivation is attested by the
	// result's own witness, which only the evaluator sets, and by the
	// decision's witness, which only a Decision method over such a result
	// sets.)
	if factset.Quality != QualityExcluded {
		if re, err := EvaluateP2Case(fs); err != nil || !sameMapping(re.Action, p2.Action) ||
			re.Choice != p2.Choice || re.Stake != p2.Stake || p2.Derivation != p2Derivation(re.Binding.Digest) {
			policies = policies.Downgrade(QualityExcluded, QualityReasonPolicyDecisionNotDerived)
		}
	}
	for _, d := range []struct {
		want string
		got  PolicyDecision
	}{{PolicyP2, p2}, {PolicyP3b, p3b}} {
		m := d.got.Action
		// Every refusal of the decision is named and excludes the case. An
		// illegal native shape is one this map version refuses — a
		// contradicted stage shape, or a native action it does not
		// recognise (a newer core: see NativeActionMapVersion); an edited or
		// underived decision is not evidence either way.
		refusal := decisionRefusal(d.got)
		switch {
		case d.got.Policy != d.want || m.Policy != d.want || m.MapVersion != NativeActionMapVersion ||
			d.got.FactsetDigest != fs.Digest || d.got.Attempt != fs.Attempt || d.got.EventID != fs.Episode.EventID:
			policies = policies.Downgrade(QualityExcluded, QualityReasonPolicyBindingMismatch)
			continue
		case refusal != "":
			policies = policies.Downgrade(QualityExcluded, d.want+"_"+refusal)
			continue
		}
		if !determinate(m.Class) {
			policies = policies.Downgrade(QualityDescriptiveOnly, d.want+QualityReasonNotDeterminateSuffix+string(m.Class))
			continue
		}
		// A choice, when one was made, must lie in the resolution's set. A
		// determinate decision that made none is a per-policy non-member of
		// the denominator, not a case downgrade.
		if resolved && d.got.Choice.Present && !containsID(res.OrderedOutcomeIDs, d.got.Choice.OutcomeID) {
			policies = policies.Downgrade(QualityDescriptiveOnly, d.want+QualityReasonChoiceOutsideSetSuffix)
		}
	}
	return episode.Merge(factset).Merge(resolution).Merge(policies)
}

// DenominatorMembership is the composed, case-bound denominator verdict for
// one policy's decision on one case: the only place the payout seam's
// membership conditions and seam 12's case verdict meet.
type DenominatorMembership struct {
	// Policy is the payout evidence's policy, set only once the evidence has
	// been accepted as derived and named a policy of the protocol.
	Policy string `json:"policy"`
	// Quality is the case's quality, re-derived from the dataset.
	Quality Quality `json:"quality"`
	// ProcessingComplete carries [QualityRecord.ProcessingComplete] through to
	// the denominator verdict, with the same fail-safe sense: FALSE means the
	// evidence this verdict rests on was never produced.
	//
	// A verdict with ProcessingComplete false is NOT a non-member. It is not a
	// member either. It is a verdict about a dataset that was not read, and the
	// two membership flags below are false because of THAT, not because the
	// case was judged and found wanting.
	//
	// THE REASONS DO NOT SAY SO CLEANLY, and the gap is worth naming rather
	// than leaving for a reader to hit. Such a verdict carries
	// SELECTION_UNAVAILABLE from the quality record, and then whatever the gate
	// it stops at appends: CASE_NOT_PRIMARY_SCORABLE when it reaches the
	// quality gate, PAYOUT_NOT_DERIVED or a binding reason when it stops
	// earlier. Every one of those is a membership reason recorded about a
	// dataset that was never read, and which of them appears depends on the
	// other arguments rather than on the dataset. The reasons are a closed
	// vocabulary shared with cases that WERE judged, so they cannot carry this
	// distinction; this field is what a consumer reads for it, and this is
	// exactly why prose was not enough. An evaluation that sums
	// Primary or PlacedBet over a set of verdicts MUST FAIL STOP when any one
	// of them is incomplete: a denominator built from the remainder is a
	// denominator over a cohort no one chose, published as though it were the
	// whole. This package cannot enforce that -- it issues one verdict at a
	// time and never sees the set -- so the obligation is stated here and the
	// flag is what a caller checks; see the note on [AssessCaseQuality].
	ProcessingComplete bool `json:"processingComplete"`
	// Derivation and CounterpartDerivation name the decision this verdict is
	// about and the other policy's decision it was judged beside — the
	// PAIRING, recorded so an audit can check that the counterpart is the
	// one the comparison pairs (the same dataset binding and run).
	// Derivation is set once the evidence is bound to its own decision;
	// CounterpartDerivation only when the counterpart is itself a usable
	// (derived, legal) decision of THIS case by the OTHER policy — an
	// unusable, foreign or same-policy counterpart is named by the quality
	// reasons, never echoed.
	Derivation            string `json:"derivation,omitempty"`
	CounterpartDerivation string `json:"counterpartDerivation,omitempty"`
	// Primary is true ONLY when the decision counts in the
	// POLICY_CHOICE_ACCURACY denominator. The two conditions named here -- the
	// case is PRIMARY_SCORABLE and the payout evidence is a resolved
	// WOULD_ATTEMPT -- are necessary, NOT sufficient.
	//
	// NO COUNT IS GIVEN HERE. A count is the wrong instrument for this: the
	// gates move with the code and a number in a comment does not. What a
	// reader can check instead is the membership vocabulary declared below:
	// every way
	// [AssessDenominatorMembership] can withhold this flag names one of those
	// constants, so the vocabulary IS the list, and it is a closed const block
	// rather than a number in prose. A PRIMARY_SCORABLE case whose payout is a
	// resolved WOULD_ATTEMPT is still withheld when the registry does not
	// re-derive, or when the round is not this case's canonical claim;
	// denominator_test.go carries that counterexample beside the case that
	// does count. Two independent review lanes read this comment as a
	// biconditional, which is why it does not read as one.
	Primary bool `json:"primary"`
	// PlacedBet is true ONLY when the decision counts in the
	// PLACED_BET_WIN_RATE denominator, under the same five conditions with the
	// payout evidence instead a platform-proven bet that settled WIN or LOSE.
	// Gating the placed-bet denominator on PRIMARY_SCORABLE — and so on the
	// counterpart's determinacy — is this package's NARROWING of the
	// approved "proven accepted WIN+LOSE" rule: a DESCRIPTIVE_ONLY case is
	// described, never counted, in either denominator. A narrowing can only
	// withhold; the reasons say why.
	PlacedBet bool `json:"placedBet"`
	// Reasons carries every reason the case was lowered and every reason the
	// payout evidence could not be read.
	Reasons []string `json:"reasons,omitempty"`
	// RegistryDigest names the source-round registry this verdict was issued
	// under: the digest of the registry that re-derived from its own entries
	// ([VerifySourceRoundRegistry]) and was read for the round, whether the
	// case then counted or was refused as not canonical. It is empty when
	// the verdict was refused before the registry was read, or when the
	// registry did not re-derive — an unverified digest is never echoed. An
	// audit follows it to the registry, and from there to the runner's
	// record of which datasets were reconciled into it: the one obligation
	// no verdict can carry.
	RegistryDigest string `json:"registryDigest,omitempty"`
}

// Membership reasons. Closed vocabulary; the quality reasons are carried too.
const (
	MembershipReasonPayoutNotDerived = "PAYOUT_NOT_DERIVED"
	// MembershipReasonPayoutRefusedDecision names a payout this package
	// produced for a decision it REFUSED. Such an artifact withholds its
	// case identity deliberately, so without this value it reads here as a
	// payout about some other case -- the same string a genuine cross-case
	// splice earns, and the same string a decision whose witness does not
	// match earns. Three faults, one diagnosis.
	MembershipReasonPayoutRefusedDecision   = "PAYOUT_REFUSED_ITS_DECISION"
	MembershipReasonPayoutBindingMismatch   = "PAYOUT_BINDING_MISMATCH"
	MembershipReasonPayoutDecisionMismatch  = "PAYOUT_DECISION_MISMATCH"
	MembershipReasonPayoutResolutionUnbound = "PAYOUT_RESOLUTION_UNBOUND"
	MembershipReasonResolutionMismatch      = "PAYOUT_RESOLUTION_MISMATCH"
	MembershipReasonRegistryNotDerived      = "SOURCE_ROUND_REGISTRY_NOT_DERIVED"
	MembershipReasonCaseNotCanonical        = "CASE_NOT_CANONICAL_SOURCE_ROUND"
	MembershipReasonCaseNotPrimary          = "CASE_NOT_PRIMARY_SCORABLE"
)

// AssessDenominatorMembership composes seam 12 with seams 3 and 10 for one
// policy's decision on one case. The case quality is re-derived from the
// DATASET by [AssessCaseQuality] — never read from a supplied record — and
// the payout evidence must be [DerivePayout]'s own (its witness), bound to
// this case (attempt, factset digest, round), to the very decision it was
// derived from (that decision's own witness and derivation, so a payout of
// one P3b decision is never judged as another's), and to the resolution the
// case was assessed against. The case must then be the CANONICAL source
// round: the claim the dataset derives for it ([ClaimSourceRound]) must be
// the one canonical claim of its public round in the registry the caller
// reconciled (reg, re-derived from its own claims by
// [VerifySourceRoundRegistry]) — a round that registry holds in CONFLICT, a
// round it never reconciled, or a registry that does not re-derive counts
// nothing, however good a single dataset's evidence looks on its own. Only
// then are the payout seam's membership conditions read, and only for a
// PRIMARY_SCORABLE case. What this function cannot check is that reg was
// reconciled over EVERY dataset of the run: a registry reconciled over the
// one dataset in hand, or over a claim set a conflicting claim was dropped
// from, finds its own claim UNIQUE, so that obligation is the runner's. The
// verdict names the registry it was issued under
// ([DenominatorMembership.RegistryDigest]) so an audit can follow it to the
// registry, and to the runner's record of what was reconciled into it.
//
// The pairing is the CALLER's and is recorded, not inferred: the counterpart
// decision is the one handed in the other slot, and both derivations are
// returned so an audit can check that the counterpart is the decision the
// comparison actually pairs. What the case gate guarantees is narrower: a
// policy is never counted beside a counterpart that is not derived or not
// determinate.
//
// Each call reads the case from a PREPARED dataset and a PREPARED registry:
// seams 1–3 ran once, when the handle was built, and the registry was
// re-reconciled from its own entries once, when ITS handle was built. What is
// left per verdict is the case's own factset rebuild — twice, once for the
// quality and once for the source-round claim — and two O(1) lookups. That is
// the trusted path's cost now; it used to be both derivations and the whole
// re-reconciliation, per verdict, which made a run quadratic in its own size.
func AssessDenominatorMembership(src PreparedDataset, reg PreparedSourceRounds, fs CommonFactset, p2, p3b PolicyDecision,
	res ResolutionArtifact, pe PayoutEvidence) DenominatorMembership {
	q := AssessCaseQuality(src, fs, p2, p3b, res)
	out := DenominatorMembership{Quality: q.Quality, ProcessingComplete: q.ProcessingComplete, Reasons: cloneStrings(q.Reasons)}
	reason := func(r string) { out.Reasons = appendOnce(out.Reasons, r) }
	if !pe.derived() {
		reason(MembershipReasonPayoutNotDerived)
		return out
	}
	// A PAYOUT THAT REFUSED ITS DECISION SAYS SO HERE, ABOVE THE POLICY SWITCH
	// AND ABOVE THE CASE BINDING. DerivePayout withholds a refused decision's
	// identity deliberately -- the text was never verified and framing it cost
	// what the caller supplied -- so such an artifact carries no factset digest
	// at all, and the gates below would report it as evidence about ANOTHER
	// CASE. That is true and it is the wrong diagnosis: it is the same string a
	// genuine cross-case splice earns, and the one the witness gate earns for a
	// spliced decision, so three distinguishable faults would arrive as one.
	//
	// ABOVE THE SWITCH, AND THAT POSITION IS THE SECOND HALF OF THE REPAIR.
	// Placed below it, this gate answered for a refused decision whose policy
	// this package RECOGNIZES and missed the one whose policy it does not:
	// namedPolicy withholds an unrecognized name, so the switch's default arm
	// fired first and returned the very string this gate exists to replace.
	// A Codex review caught that on the published head -- the same failure the
	// gate itself was repairing, one gate earlier, which is why the position
	// is argued here rather than left to look arbitrary.
	//
	// THE DISCRIMINATOR IS EXACT AND NOT A HEURISTIC. decisionRefusal refuses
	// a decision whose FactsetDigest is empty, so every payout that got past
	// it carries a non-empty one; a DERIVED payout with none is therefore
	// exactly a payout this package refused. WHY the decision was refused is
	// on the artifact's own Reasons, in the refusal's own category -- which is
	// finer than the verdicts this gate replaces, not coarser. The policy is
	// named when the artifact carries one, because namedPolicy admits only
	// this package's own constants; nothing else of the case is.
	//
	// IT IS THE SAME REPAIR selectionRan IS, one seam over: a collapse that
	// makes a refusal of one thing indistinguishable from a verdict about
	// another is a lost signal even when no count moves. Neither Primary nor
	// PlacedBet moves on any of these paths.
	if pe.FactsetDigest == "" {
		out.Policy = namedPolicy(pe.Policy)
		reason(MembershipReasonPayoutRefusedDecision)
		return out
	}
	var decision, counterpart PolicyDecision
	switch pe.Policy {
	case PolicyP2:
		decision, counterpart = p2, p3b
	case PolicyP3b:
		decision, counterpart = p3b, p2
	default:
		reason(MembershipReasonPayoutBindingMismatch)
		return out
	}
	out.Policy = pe.Policy
	if pe.ContractVersion != PayoutEvidenceVersion || pe.Attempt != fs.Attempt || pe.FactsetDigest != fs.Digest ||
		pe.EventID != fs.Episode.EventID || decision.Policy != pe.Policy || decision.Attempt != pe.Attempt ||
		decision.FactsetDigest != pe.FactsetDigest || decision.EventID != pe.EventID {
		reason(MembershipReasonPayoutBindingMismatch)
		return out
	}
	// The evidence must have been derived from THIS decision, not merely
	// from some decision of this policy on this case.
	if pe.decisionWitness == "" || pe.decisionWitness != decision.witness || pe.Derivation != decision.Derivation {
		reason(MembershipReasonPayoutDecisionMismatch)
		return out
	}
	out.Derivation = decision.Derivation
	// The factset digest alone determines the attempt and the round for a
	// derived decision; the attempt and event clauses are defence in depth.
	if decisionRefusal(counterpart) == "" && counterpart.Policy != pe.Policy && counterpart.FactsetDigest == fs.Digest &&
		counterpart.Attempt == fs.Attempt && counterpart.EventID == fs.Episode.EventID {
		out.CounterpartDerivation = counterpart.Derivation
	}
	switch {
	case pe.ResolutionFactsDigest == "":
		// The evidence never read a resolution (it was refused before): it
		// is no member of anything, and it is not bound to this resolution.
		reason(MembershipReasonPayoutResolutionUnbound)
		return out
	case pe.ResolutionFactsDigest != res.ResolutionFactsDigest:
		reason(MembershipReasonResolutionMismatch)
		return out
	}
	if q.Quality != QualityPrimaryScorable {
		reason(MembershipReasonCaseNotPrimary)
		return out
	}
	// Seam 3, the last gate: only the canonical claim of the round in the
	// registry the caller reconciled may count. The registry is re-derived
	// from its own claims, and the case's claim from the dataset, never read
	// from the caller. A claim that cannot be derived is, by that fact, not
	// the round's canonical claim: the one reason names both (a case that
	// reached this gate is PRIMARY_SCORABLE, so its claim derives; the error
	// arm is defence in depth).
	//
	// THE VERIFICATION USED TO BE PER-VERDICT, AND THAT WAS QUADRATIC OVER A
	// RUN. Reported by a security review lane, reproduced here, and closed by
	// [PrepareSourceRounds]: a run that judged N cases against an N-claim
	// registry re-verified the whole registry N times -- hashing every claim,
	// flattening the entries and reconciling them again -- and then scanned
	// the entries linearly for one canonical claim. Doubling N multiplied the
	// run's total by about 3.8. THE ABSOLUTES ARE NOT REPEATED HERE: doc.go's
	// WHAT REMAINS owns them, because a figure maintained by hand in two
	// places is a figure that will disagree with itself.
	//
	// IT WAS NOT THE ONLY PER-CASE PASS OVER THE RUN, and the other two are
	// this function's as well: AssessCaseQuality above and ClaimSourceRound
	// below each re-ran seams 1-3 over the whole dataset, so one verdict
	// selected the dataset twice. [PrepareDataset] closes those, and closing
	// only this one would have left the run quadratic all the same.
	//
	// WHAT IS LEFT HERE IS O(1) IN THE REGISTRY: a flag and one map lookup.
	// The handle's construction ran the same VerifySourceRoundRegistry any
	// caller could run, once, so nothing about WHAT is proved has moved --
	// only how often. A zero handle -- which is what a registry this package
	// will not verify reduces to, since no other kind exists outside the
	// package -- is refused here, exactly as an unverifiable registry was.
	if !reg.derived() {
		reason(MembershipReasonRegistryNotDerived)
		return out
	}
	// The registry is verified and is read: the verdict names it from here
	// on, counted or refused alike.
	out.RegistryDigest = reg.Digest()
	if claim, err := ClaimSourceRound(src, fs); err != nil || !reg.isCanonical(claim) {
		reason(MembershipReasonCaseNotCanonical)
		return out
	}
	out.Primary = pe.PrimaryDenominatorMember
	out.PlacedBet = pe.PlacedBetDenominatorMember
	return out
}

// sameMapping reports whether two action mappings are equal field for field.
//
// Its per-field clauses are, individually, not discriminable from outside this
// package, and the reason is the same one that makes them safe: a
// PolicyDecision carries a producer-only witness that frames the whole action
// (see frameAction), so an action edited in Class or in Legal alone fails
// decisionRefusal one branch earlier and never reaches this comparison. What
// this function actually guards is re-evaluation DRIFT -- the same factset
// mapping differently on a second pass -- for which no input exists today.
// Stated here rather than left as an unexplained mutation survivor.
func sameMapping(a, b ActionMapping) bool {
	return a.MapVersion == b.MapVersion && a.Policy == b.Policy && a.NativeAction == b.NativeAction &&
		a.Class == b.Class && a.Legal == b.Legal && a.SkipReason == b.SkipReason && sameIDs(a.Illegality, b.Illegality)
}

// sameIDs reports whether two identity vectors are equal element for element.
func sameIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsID(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

func appendOnce(list []string, v string) []string {
	for _, existing := range list {
		if existing == v {
			return list
		}
	}
	return append(list, v)
}

func cloneStrings(s []string) []string {
	if s == nil {
		return nil
	}
	return append([]string(nil), s...)
}
