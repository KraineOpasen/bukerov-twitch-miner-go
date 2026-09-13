package p4offline

import (
	"errors"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
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
	// QualityPrimaryScorable — the case can contribute to the primary metric:
	// its evidence is complete, its boundary is proven, both policies are
	// derived decisions mapping to a determinate native action, both MADE A
	// CHOICE inside the resolution's outcome set, and the resolution names a
	// winner.
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
}

// QualityReasonOutsideVocabulary marks a record whose own quality was not in
// the vocabulary when it was touched — a zero value or a decoded document.
const QualityReasonOutsideVocabulary = "QUALITY_OUTSIDE_VOCABULARY"

// NewQualityRecord starts a record at the top of the ladder.
func NewQualityRecord() QualityRecord {
	return QualityRecord{Quality: QualityPrimaryScorable}
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

// Downgrade lowers the record to q with a reason. A call that would RAISE the
// quality changes nothing except the recorded reason, which is kept so the
// attempt is visible; an unrecognised quality — as the target or as the
// receiver's own — is treated as EXCLUDED.
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

// Merge combines two records: the LOWER quality wins, and every reason is kept.
func (r QualityRecord) Merge(other QualityRecord) QualityRecord {
	out := r.normalised()
	other = other.normalised()
	if other.Quality.rank() < out.Quality.rank() {
		out.Quality = other.Quality
	}
	for _, reason := range other.Reasons {
		out.Reasons = appendOnce(out.Reasons, reason)
	}
	out.History = append(out.History, other.History...)
	return out
}

// Quality reasons. Closed vocabulary.
const (
	QualityReasonEpisodeExcluded              = "EPISODE_EXCLUDED"
	QualityReasonFactsetDigestMismatch        = "FACTSET_DIGEST_MISMATCH"
	QualityReasonFactsetBindingMismatch       = "FACTSET_BINDING_MISMATCH"
	QualityReasonFactsetPrefix                = "FACTSET_"
	QualityReasonPolicyBindingMismatch        = "POLICY_BINDING_MISMATCH"
	QualityReasonNotDeterminateSuffix         = "_NOT_DETERMINATE:"
	QualityReasonNoChoiceSuffix               = "_NO_CHOICE:"
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

// madeAChoice reports whether a class is one under which the policy chose an
// outcome to bet on. A POLICY_SKIP and a NO_ATTEMPT_IN_SUPPLIED_PREFIX are
// determinate answers that bet on nothing.
func madeAChoice(c ActionClass) bool {
	return c == ActionWouldAttempt || c == ActionParticipationAdmittedStakeUnknown
}

// AssessCaseQuality is seam 12: the case's quality as the MINIMUM over every
// seam's verdict. It only ever lowers.
//
// It takes the DATASET, not a selection: the episode's admission and its
// boundary are re-derived from the raw evidence here, and the factset must be
// exactly what the dataset derives for its episode. A selection is an
// exported value anyone can build; its flags are never trusted.
//
// ASSUMPTION, stated for the owner: the primary metric is the accuracy of a
// policy's CHOICE, so a case where either policy made no choice — a
// POLICY_SKIP or a NO_ATTEMPT_IN_SUPPLIED_PREFIX — is DESCRIPTIVE_ONLY here,
// whatever choice the policy computed on the way to skipping. Whether a skip
// enters the primary denominator under D01–D16 is a question this session
// could not settle from the contract text it had; the conservative reading
// is applied and named.
func AssessCaseQuality(ds predictioneval.SourceDataset, fs CommonFactset, p2, p3b PolicyDecision, res ResolutionArtifact) QualityRecord {
	episode := NewQualityRecord()
	ep, found, err := lookupEpisode(ds, fs.Episode)
	if found {
		episode = episode.Merge(ep.Quality)
	}
	if err != nil {
		episode = episode.Downgrade(QualityExcluded, QualityReasonEpisodeExcluded)
	}

	factset := NewQualityRecord()
	if verr := VerifyCommonFactset(fs); verr != nil {
		factset = factset.Downgrade(QualityExcluded, QualityReasonFactsetDigestMismatch)
	} else if err == nil {
		rebuilt, berr := buildFactset(ep)
		if berr != nil || rebuilt.Digest != fs.Digest {
			factset = factset.Downgrade(QualityExcluded, QualityReasonFactsetBindingMismatch)
		}
	}
	if fs.Completeness != FactsetComplete {
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
	// be exactly what evaluating this factset yields. (P3b needs a ruleset,
	// a key and a trajectory the quality seam does not hold; its derivation
	// is attested by the decision's witness, which only a Decision method
	// over a genuine result sets, and by EvaluateP3bCase's own bindings.)
	if factset.Quality != QualityExcluded {
		if re, err := EvaluateP2Case(fs); err != nil || !sameMapping(re.Action, p2.Action) ||
			re.Choice != p2.Choice || re.Stake != p2.Stake {
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
		if !madeAChoice(m.Class) || !d.got.Choice.Present {
			policies = policies.Downgrade(QualityDescriptiveOnly, d.want+QualityReasonNoChoiceSuffix+string(m.Class))
			continue
		}
		if resolved && !containsID(res.OrderedOutcomeIDs, d.got.Choice.OutcomeID) {
			policies = policies.Downgrade(QualityDescriptiveOnly, d.want+QualityReasonChoiceOutsideSetSuffix)
		}
	}
	return episode.Merge(factset).Merge(resolution).Merge(policies)
}

// sameMapping reports whether two action mappings are equal field for field.
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
