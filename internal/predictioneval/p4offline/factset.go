package p4offline

import (
	"errors"
	"math"
	"strconv"
	"unicode/utf8"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
)

// SEAMS 4–5: the outcome-free common factset and the exact P2 per-case
// configuration binding.
//
// The common factset is what BOTH policies are evaluated over: everything the
// selected attempt READ, and nothing it produced. It is built from the P2
// projection's [predictioneval.DecisionInputs] — the half of the envelope the
// causal cut already separated from the results — and is then serialized and
// digested by VALUE under its own domain string. The native pe-cid/v1 is not
// reused: it hashes the terminal row's store witness, and that row carries the
// results, so it is not outcome-free.
//
// Completeness is decided from the inputs alone. A missing input stays
// missing (never zero), and stealth must be PROVEN off by the factual
// settings — settings that are absent cannot prove it.
//
// # Verification is re-derivation
//
// The digest detects CHANGE, not origin: anyone who can build the struct can
// compute its digest. So [VerifyCommonFactset] also re-derives the labels —
// completeness, the stealth proof — from the factset's own values and refuses
// a factset whose labels say more than its values do. And every function
// that needs the factset to BE the dataset's factset ([ProjectFactualPlacement],
// [AssessCaseQuality]) rebuilds it from the dataset and compares digests
// rather than trusting a selection's flags.

// FactsetCompleteness is the closed completeness vocabulary of a factset.
type FactsetCompleteness string

// Completeness of a factset.
const (
	// FactsetComplete — every input both policies need is present and stealth
	// is proven off. Only a COMPLETE factset is evaluable.
	FactsetComplete FactsetCompleteness = "COMPLETE"
	// FactsetPreDecisionExit — the attempt ended before the policy ran. There
	// is no decision-time input to evaluate; the case is coverage only.
	FactsetPreDecisionExit FactsetCompleteness = "PRE_DECISION_EXIT"
	// FactsetIncomplete — an input is missing, unrepresentable or
	// inconsistent, or stealth is not proven off. Never repaired.
	FactsetIncomplete FactsetCompleteness = "INCOMPLETE"
)

// StealthProof is the closed stealth-proof vocabulary.
type StealthProof string

// Stealth proof values.
const (
	// StealthProofOff is the protocol's FACTUAL_STEALTH_OFF: the factual
	// settings are present and stealth mode is false.
	StealthProofOff StealthProof = FactualStealthOff
	// StealthProofOn — the factual settings say stealth mode was on.
	StealthProofOn StealthProof = "STEALTH_ON"
	// StealthProofUnknown — no factual settings, so nothing proves it off.
	StealthProofUnknown StealthProof = "STEALTH_UNKNOWN"
)

// Incompleteness reasons of this package's own. Every other reason a
// factset carries is one of the core's eligibility reasons or a
// [StealthProof] label.
const (
	// FactsetReasonMinimumStakeNotPinned names a factset whose minimum stake
	// is not [predictioneval.PinnedMinimumStake]. The projection pins it on
	// every case it hands to this seam, so only a factset digested by hand
	// can carry another; such a factset is INCOMPLETE by its values.
	FactsetReasonMinimumStakeNotPinned = "MINIMUM_STAKE_NOT_PINNED"
)

// Factset refusals.
var (
	// ErrEpisodeNotSelected is an episode that is not in the dataset, is
	// excluded, has no usable first opportunity, or has no proven boundary.
	ErrEpisodeNotSelected = errors.New("p4offline: episode is not a selected opportunity")
	// ErrFactsetNotEvaluable is a factset that is not COMPLETE.
	ErrFactsetNotEvaluable = errors.New("p4offline: common factset is not evaluable")
	// ErrFactsetDigest is a factset whose contract or digest does not match
	// its values.
	ErrFactsetDigest = errors.New("p4offline: common factset digest does not verify")
	// ErrFactsetInconsistent is a factset whose labels — completeness, the
	// stealth proof, the incompleteness reasons — are not what its own
	// values derive, or which holds a value its own encoding cannot express
	// (see checkFactsetValuesExpressible). Both are the same fault at
	// different depths: the artifact does not support what carrying it claims.
	ErrFactsetInconsistent = errors.New("p4offline: common factset labels are not derived from its values")
	// ErrFactsetNotDerived is a factset that verifies on its own but is not
	// the one the dataset derives for its episode.
	ErrFactsetNotDerived = errors.New("p4offline: common factset is not what the dataset derives for its episode")
	// ErrConfigBindingMissing is a factset with no factual settings to bind.
	ErrConfigBindingMissing = errors.New("p4offline: no factual settings to bind")
)

// CommonFactset is the outcome-free input set of one selected opportunity.
//
// Every field is an INPUT the decision read, or the identity of where it was
// read. No recorded choice, amount, verdict, stake, clamp, terminal DECISION
// reason, placement, payout or observed realization has a field here.
//
// ONE VALUE COMES FROM THE TERMINAL SIDE, and the earlier wording of this
// sentence -- "terminal reason", unqualified -- denied it. PreDecisionExit is
// the terminal fact's ReasonCode, carried verbatim by the P2 projector, which
// places it on the INPUTS side of its causal cut: it is the witnessed reason
// an attempt ended BEFORE Calculate, so there was no decision for it to leak.
// It is non-empty only on a factset whose Completeness is PRE_DECISION_EXIT,
// which [EvaluateP2Case] and [ProjectP3bSingleCandidate] both refuse with
// ErrFactsetNotEvaluable, so it reaches no policy input and no entropy
// coordinate. The exclusion above is about the reason a DECIDED attempt
// recorded; this is the reason there was no decision.
type CommonFactset struct {
	ContractVersion string `json:"contractVersion"`
	Protocol        string `json:"protocol"`
	// ProjectorRevision is the P2 model that performed the causal cut.
	ProjectorRevision string `json:"projectorRevision"`

	Episode               EpisodeIdentity           `json:"episode"`
	Attempt               predictioneval.AttemptKey `json:"attempt"`
	TerminalObservationID string                    `json:"terminalObservationId"`
	// CutoffPosition is C: where this factset became available.
	CutoffPosition int64 `json:"cutoffPosition"`

	Completeness      FactsetCompleteness `json:"completeness"`
	IncompleteReasons []string            `json:"incompleteReasons,omitempty"`

	ReachedDecision bool         `json:"reachedDecision"`
	PreDecisionExit string       `json:"preDecisionExit,omitempty"`
	StealthProof    StealthProof `json:"stealthProof"`

	Settings        *predictioneval.BetSettingsInput `json:"settings,omitempty"`
	Balance         int64                            `json:"balance"`
	BalancePresent  bool                             `json:"balancePresent"`
	Outcomes        []predictioneval.OutcomeInput    `json:"outcomes"`
	OutcomesPresent bool                             `json:"outcomesPresent"`
	BetTotalUsers   *int64                           `json:"betTotalUsers,omitempty"`
	BetTotalPoints  *int64                           `json:"betTotalPoints,omitempty"`

	RiskPresent         bool `json:"riskPresent"`
	RiskMaxStakePercent int  `json:"riskMaxStakePercent"`
	RiskReservePoints   int  `json:"riskReservePoints"`

	HealthState  string `json:"healthState"`
	HealthReason string `json:"healthReason,omitempty"`
	MinimumStake int    `json:"minimumStake"`

	// Digest is the SHA-256 of [SerializeCommonFactset] under
	// [CommonFactsetDigestVersion].
	Digest string `json:"digest"`
}

// P2ConfigBinding is the exact configuration one P2 evaluation ran under:
// the case's own decision-time settings, its risk gates, the pinned minimum,
// and the model/policy revision that interpreted them.
type P2ConfigBinding struct {
	ContractVersion     string                          `json:"contractVersion"`
	Model               predictioneval.ModelProvenance  `json:"model"`
	Settings            predictioneval.BetSettingsInput `json:"settings"`
	RiskPresent         bool                            `json:"riskPresent"`
	RiskMaxStakePercent int                             `json:"riskMaxStakePercent"`
	RiskReservePoints   int                             `json:"riskReservePoints"`
	MinimumStake        int                             `json:"minimumStake"`
	Digest              string                          `json:"digest"`
}

// lookupEpisode re-runs seams 1–3 over the dataset and returns the named
// episode's selection. found reports whether the episode exists in an
// admitted session at all; err is non-nil whenever the episode is not a
// selected opportunity (absent, session refused, excluded, unusable or
// unproven). The error is a caller-contract error for an unordered dataset, and
// a typed RESOURCE refusal -- ErrEvidenceRetention -- when SelectEpisodes
// declines the dataset's shape; see its documentation. The two differ in whose
// fault they are and a caller must not read one as the other, but they agree on
// the thing that matters downstream: NEITHER PRODUCES A SELECTION, so neither
// establishes anything about this episode -- the causal-order check refuses
// before anything is read, and the resource refusal abandons the loop part-way
// and returns the zero selection rather than a partial one. Only ErrEpisodeNotSelected
// is the verdict of a selection that DID run, and that is the distinction
// selectionRan turns into [QualityRecord.ProcessingComplete]. Errors from
// SelectEpisodes are returned VERBATIM so errors.Is reaches their sentinels
// through this seam.
func lookupEpisode(ds predictioneval.SourceDataset, episode EpisodeIdentity) (EpisodeSelection, bool, error) {
	sel, err := SelectEpisodes(ds)
	if err != nil {
		return EpisodeSelection{}, false, err
	}
	if !sel.SessionAdmitted {
		return EpisodeSelection{}, false, errors.Join(ErrEpisodeNotSelected,
			errors.New("p4offline: session refused: "+reasonListExtent(sel.SessionRefusals)+
				": "+joinReasons(sessionRefusalKinds(sel.SessionRefusals))))
	}
	for i := range sel.Episodes {
		ep := sel.Episodes[i]
		if ep.Episode != episode {
			continue
		}
		if ep.Excluded || ep.Attempt == nil || !ep.FirstOpportunityUsable || !ep.Boundary.Proven {
			detail := "episode excluded"
			if len(ep.ExclusionReasons) > 0 {
				detail = joinReasons(ep.ExclusionReasons)
			}
			return ep, true, errors.Join(ErrEpisodeNotSelected, errors.New("p4offline: "+detail))
		}
		return ep, true, nil
	}
	return EpisodeSelection{}, false, errors.Join(ErrEpisodeNotSelected, errors.New("p4offline: episode is not in the dataset"))
}

// selectedOpportunity is lookupEpisode for callers that need the selected
// opportunity and nothing else.
func selectedOpportunity(ds predictioneval.SourceDataset, episode EpisodeIdentity) (EpisodeSelection, error) {
	ep, _, err := lookupEpisode(ds, episode)
	if err != nil {
		return EpisodeSelection{}, err
	}
	return ep, nil
}

// derivedOpportunity re-derives the factset's episode from the dataset and
// requires the factset to be exactly what the dataset derives for it.
func derivedOpportunity(ds predictioneval.SourceDataset, fs CommonFactset) (EpisodeSelection, error) {
	if err := VerifyCommonFactset(fs); err != nil {
		return EpisodeSelection{}, err
	}
	ep, err := selectedOpportunity(ds, fs.Episode)
	if err != nil {
		return EpisodeSelection{}, err
	}
	rebuilt, err := buildFactset(ep)
	if err != nil {
		// THE DATASET IS WHAT FAILED TO DERIVE, not the caller's factset: that
		// one verified three lines up. Returning the rebuild's own sentinel
		// would blame the wrong artifact -- and would withdraw
		// ErrFactsetNotDerived for an input class its own doc describes
		// exactly, so a caller switching on it would stop seeing a case it
		// used to see. The cause is joined rather than dropped, so an auditor
		// still reads why the dataset derives nothing.
		return EpisodeSelection{}, errors.Join(ErrFactsetNotDerived,
			errors.New("p4offline: the dataset derives no factset for this episode"), err)
	}
	if rebuilt.Digest != fs.Digest {
		return EpisodeSelection{}, errors.Join(ErrFactsetNotDerived,
			errors.New("p4offline: the dataset derives "+rebuilt.Digest+", the factset says "+fs.Digest))
	}
	return ep, nil
}

// BuildCommonFactset re-derives the episode's selection from the dataset and
// projects the selected opportunity's inputs into a digested, outcome-free
// factset. It takes the DATASET, not a selection: a selection is an exported
// value anyone can build, and nothing downstream may rest on its flags.
//
// It can return ErrFactsetInconsistent, which the build path could not return
// before the value gate existed. The consequence is larger than the value and
// is named here rather than left to be met: an episode whose inputs hold a
// non-finite float yields NO factset rather than an INCOMPLETE one, so the
// CASE leaves the cohort with the value. That is fail-closed and deliberate —
// a label derived from a value the artifact cannot carry would be a label
// about nothing — but it is a case lost, not merely a field refused.
func BuildCommonFactset(ds predictioneval.SourceDataset, episode EpisodeIdentity) (CommonFactset, error) {
	ep, err := selectedOpportunity(ds, episode)
	if err != nil {
		return CommonFactset{}, err
	}
	return buildFactset(ep)
}

// buildFactset projects a selected opportunity. Callers reach it only through
// a selection this package derived itself.
func buildFactset(ep EpisodeSelection) (CommonFactset, error) {
	if ep.Excluded || ep.Attempt == nil || !ep.FirstOpportunityUsable || !ep.Boundary.Proven {
		return CommonFactset{}, ErrEpisodeNotSelected
	}
	dc, err := predictioneval.ProjectDecisionCase(*ep.Attempt)
	if err != nil {
		return CommonFactset{}, err
	}
	terminal := ep.Attempt.CommonInputSlice[ep.Attempt.TerminalIndex]
	if ep.Boundary.CutoffPosition != terminal.CollectorSequence || ep.Boundary.CutoffObservationID != terminal.ObservationID {
		return CommonFactset{}, errors.Join(ErrEpisodeNotSelected, errors.New("p4offline: boundary cutoff is not the terminal envelope"))
	}
	in := dc.Inputs
	fs := CommonFactset{
		ContractVersion:       CommonFactsetDigestVersion,
		Protocol:              ProtocolVersion,
		ProjectorRevision:     dc.Model.ModelVersion,
		Episode:               ep.Episode,
		Attempt:               ep.Attempt.Key,
		TerminalObservationID: terminal.ObservationID,
		CutoffPosition:        ep.Boundary.CutoffPosition,
		ReachedDecision:       in.ReachedDecision,
		PreDecisionExit:       in.PreDecisionExit,
		Settings:              copySettings(in.Settings),
		Balance:               int64(in.Balance),
		BalancePresent:        in.BalancePresent,
		Outcomes:              append([]predictioneval.OutcomeInput(nil), in.Outcomes...),
		OutcomesPresent:       in.OutcomesPresent,
		BetTotalUsers:         copyInt64(in.BetTotalUsers),
		BetTotalPoints:        copyInt64(in.BetTotalPoints),
		RiskPresent:           in.RiskPresent,
		RiskMaxStakePercent:   in.RiskMaxStakePercent,
		RiskReservePoints:     in.RiskReservePoints,
		HealthState:           in.HealthState,
		HealthReason:          in.HealthReason,
		MinimumStake:          in.MinimumStake,
	}
	if fs.OutcomesPresent && fs.Outcomes == nil {
		fs.Outcomes = []predictioneval.OutcomeInput{}
	}
	// AHEAD OF EVERY DERIVED LABEL AND OF THE DIGEST: this package does not
	// issue a certificate for a value the artifact it certifies cannot carry.
	// See checkFactsetValuesExpressible.
	if err := checkFactsetValuesExpressible(fs); err != nil {
		return CommonFactset{}, err
	}
	fs.StealthProof = deriveStealthProof(fs.Settings)

	var reasons []string
	for _, r := range dc.Eligibility.Reasons {
		// The two RESULT-side reasons are not about inputs: an attempt whose
		// recorded choice amount is missing still read a complete factset.
		if r == predictioneval.IneligibleMissingChoiceAmount || r == predictioneval.IneligibleMissingSkipResult {
			continue
		}
		reasons = appendOnce(reasons, r)
	}
	switch {
	case !fs.ReachedDecision:
		fs.Completeness = FactsetPreDecisionExit
	default:
		for _, r := range valueDerivedReasons(fs) {
			reasons = appendOnce(reasons, r)
		}
		if len(reasons) == 0 {
			fs.Completeness = FactsetComplete
		} else {
			fs.Completeness = FactsetIncomplete
		}
	}
	fs.IncompleteReasons = reasons
	fs.Digest = commonFactsetDigest(fs)
	return fs, nil
}

// deriveStealthProof is the ONLY way a stealth proof is established: from the
// factual settings, or from their absence.
func deriveStealthProof(s *predictioneval.BetSettingsInput) StealthProof {
	switch {
	case s == nil:
		return StealthProofUnknown
	case s.StealthMode:
		return StealthProofOn
	default:
		return StealthProofOff
	}
}

// valueDerivedReasons lists the incompleteness reasons the factset's own
// values imply for an attempt that reached its decision.
func valueDerivedReasons(fs CommonFactset) []string {
	var out []string
	if fs.StealthProof != StealthProofOff {
		out = append(out, string(fs.StealthProof))
	}
	if !fs.BalancePresent {
		out = append(out, predictioneval.IneligibleMissingBalance)
	}
	if !fs.OutcomesPresent {
		out = append(out, predictioneval.IneligibleOutcomeUnrepresentable)
	}
	if fs.Settings == nil {
		out = append(out, predictioneval.IneligibleMissingSettings)
	}
	// The health verdict is a witnessed value with a closed vocabulary, and
	// a reached decision witnesses one of the four gate states — never
	// NOT_REACHED. The projection refuses both on the dataset path; a
	// factset digested by hand is held to the same values here.
	switch fs.HealthState {
	case predictioneval.HealthDisabled, predictioneval.HealthNoGate, predictioneval.HealthAllowed, predictioneval.HealthDenied:
	case predictioneval.HealthNotReached:
		out = append(out, predictioneval.IneligibleInconsistentStageStates)
	default:
		out = append(out, predictioneval.IneligibleUnknownStageState)
	}
	// The minimum stake is the pinned one on every case the projection hands
	// to this seam; a factset digested by hand around it is held to the same
	// value.
	if fs.MinimumStake != predictioneval.PinnedMinimumStake {
		out = append(out, FactsetReasonMinimumStakeNotPinned)
	}
	return out
}

// SerializeCommonFactset renders the factset's VALUES canonically: every
// field except Digest, length-prefixed, in a fixed order. Nothing recorded
// after the decision has a field to be rendered.
func SerializeCommonFactset(fs CommonFactset) []byte {
	var c canonical
	c.str(CommonFactsetDigestVersion)
	c.str(fs.Protocol)
	c.str(fs.ProjectorRevision)
	c.str(fs.Episode.String())
	c.i64(fs.Attempt.CollectorEpoch)
	c.str(fs.Attempt.CollectorSessionID)
	c.str(fs.Attempt.PoolInstanceID)
	c.u64(fs.Attempt.AttemptID)
	c.str(fs.TerminalObservationID)
	c.i64(fs.CutoffPosition)
	c.str(string(fs.Completeness))
	c.count(len(fs.IncompleteReasons))
	for _, r := range fs.IncompleteReasons {
		c.str(r)
	}
	c.boolean(fs.ReachedDecision)
	c.str(fs.PreDecisionExit)
	c.str(string(fs.StealthProof))
	serializeSettings(&c, fs.Settings)
	c.boolean(fs.BalancePresent)
	c.i64(fs.Balance)
	c.boolean(fs.OutcomesPresent)
	c.count(len(fs.Outcomes))
	for _, o := range fs.Outcomes {
		c.i64(int64(o.Slot))
		c.boolean(o.Present)
		c.str(o.ID)
		c.i64(int64(o.TotalUsers))
		c.i64(int64(o.TotalPoints))
		c.i64(int64(o.TopPoints))
		c.f64(o.PercentageUsers)
		c.f64(o.Odds)
		c.f64(o.OddsPercentage)
	}
	serializeOptionalInt64(&c, fs.BetTotalUsers)
	serializeOptionalInt64(&c, fs.BetTotalPoints)
	c.boolean(fs.RiskPresent)
	c.i64(int64(fs.RiskMaxStakePercent))
	c.i64(int64(fs.RiskReservePoints))
	c.str(fs.HealthState)
	c.str(fs.HealthReason)
	c.i64(int64(fs.MinimumStake))
	return c.bytes()
}

func commonFactsetDigest(fs CommonFactset) string {
	return sha256Hex(SerializeCommonFactset(fs))
}

// VerifyCommonFactset refuses a value the artifact's own encoding cannot
// express, then recomputes the digest, then re-derives the labels from the
// values, and refuses any mismatch. The value gate is ahead of the digest on
// purpose; checkFactsetValuesExpressible says why, and names the precedence
// shift that placement carries.
func VerifyCommonFactset(fs CommonFactset) error {
	// THE FIRST GATE ON EVERY FACTSET PATH IN THIS PACKAGE, and it used to pay
	// for its own refusal: these three fields are plain exported strings that
	// nothing here bounds, and the comparison is the first statement of the
	// function. See suppliedTextExtent for the rule and the measurement.
	if fs.ContractVersion != CommonFactsetDigestVersion {
		return errors.Join(ErrFactsetDigest, errors.New("p4offline: factset contract is "+
			suppliedTextExtent(fs.ContractVersion)+", this package writes only "+strconv.Quote(CommonFactsetDigestVersion)))
	}
	if fs.Protocol != ProtocolVersion {
		return errors.Join(ErrFactsetDigest, errors.New("p4offline: factset protocol is "+
			suppliedTextExtent(fs.Protocol)+", this package writes only "+strconv.Quote(ProtocolVersion)))
	}
	// AHEAD OF THE DIGEST, but NOT for the reason the two gates above are ahead
	// of it. Those two are placed by the refusal-COST rule: they used to pay
	// for their own refusal, and a wrong contract version is not a value an
	// encoding cannot express. This one is placed for a semantic reason -- a
	// value that cannot be written down must not be hashed into a certificate
	// saying it verified. Both this gate and the build path call one
	// validator, so a factset cannot be refused by one and accepted by the
	// other.
	//
	// ONE CONSEQUENCE, a precedence shift rather than a behaviour change,
	// stated the way p3b.go states its own: a factset that is BOTH non-finite
	// AND carries a wrong digest used to come back as ErrFactsetDigest and now
	// comes back as ErrFactsetInconsistent. A caller switching on the sentinel
	// sees a different one for that class.
	if err := checkFactsetValuesExpressible(fs); err != nil {
		return err
	}
	if want := commonFactsetDigest(fs); fs.Digest != want {
		// `want` is this package's own 64 hex digits and is quoted; the
		// supplied digest is not, because nothing gates its length here.
		return errors.Join(ErrFactsetDigest, errors.New("p4offline: factset digest of "+
			suppliedTextExtent(fs.Digest)+" does not match its values, which digest to "+strconv.Quote(want)))
	}
	return checkFactsetConsistency(fs)
}

// checkFactsetValuesExpressible refuses a factset holding a value its own
// artifact form cannot carry.
//
// Every exported field of a [CommonFactset] carries a JSON tag, so JSON is a
// form this artifact is declared for; encoding/json refuses NaN and both
// infinities outright. A non-finite float therefore produced a factset this
// package CERTIFIED and no consumer could encode — the shape that makes a
// verified artifact useless rather than merely wrong.
//
// TWO HALVES, one for each way the encoding can fail the artifact.
//
//   - A NON-FINITE FLOAT cannot be encoded at all. The positions are exactly
//     the five the canonical framing hands to [canonical.f64]: the three
//     outcome ratios and the two settings values.
//   - AN INVALID UTF-8 STRING encodes, and encodes WRONG: encoding/json
//     substitutes U+FFFD, so the factset comes back a different value and
//     fails its OWN digest — a lossy round trip that presents as the one
//     signal this package reserves for tampering. That is the quieter failure
//     of the two, and coverage_test.go round-trips a factset through JSON, so
//     it is a supported path rather than a hypothetical one. Every string the
//     framing hashes is checked, in the framing's own order.
//
// The second half was recorded as a known limitation for one round and
// repaired only when an external lane ranked it P1 and named the two things
// that settle it: that round trip is a supported path here, and this package
// already requires valid UTF-8 of the entropy coordinates (see
// checkEntropyCoordinates). The precedent was three functions away.
//
// The framing and the digest are unchanged; only the refusal is new, and it
// runs ahead of BOTH digest computations this package makes over a
// CommonFactset — buildFactset's and VerifyCommonFactset's — so neither hashes
// a value it cannot carry. (The package digests other artifacts elsewhere; the
// only one that frames these same values is BindP2Config's, and it calls
// VerifyCommonFactset first, so it is gated through this one.)
//
// WHAT THAT DOES NOT SAY. [SerializeCommonFactset] is exported and ungated, so
// a caller may still frame such a factset by hand and hash the bytes itself:
// 1,030 bytes for the fixture carrying a +Inf. Those bytes leave the process
// perfectly well. What no longer exists is a factset THIS PACKAGE certifies
// that its own encoding would refuse or silently alter.
//
// NAMING THE OFFENDER. A float is the factset's own value and formats to at
// most 24 bytes, so the refusal names it. A string is caller-supplied text
// that nothing bounds here — and it is unrepresentable by hypothesis — so the
// refusal names the FIELD and never the value. See suppliedTextExtent.
//
// WHAT THIS DOES NOT DO. It bounds nothing else: every finite value, however
// large or small, and both zeroes, are accepted exactly as before, as is every
// valid UTF-8 string including one that already contains U+FFFD. It is not a
// plausibility check on an odds, a percentage or an identifier, and it says
// nothing about whether a value is the one the platform reported.
func checkFactsetValuesExpressible(fs CommonFactset) error {
	unexpressible := func(what string, v float64) error {
		return errors.Join(ErrFactsetInconsistent, errors.New("p4offline: "+what+" is "+
			strconv.FormatFloat(v, 'g', -1, 64)+", which this factset's own encoding cannot express"))
	}
	// THE STRING HALF, in the framing's own order so the correspondence can be
	// checked by eye as well as by the test that walks the type. The refusal
	// names the FIELD and never the value: the value is caller-supplied text
	// that nothing bounds here, and the whole point is that it is not
	// representable, so quoting it would be both unbounded and unreadable.
	// See suppliedTextExtent for the rule.
	lossy := func(what string) error {
		return errors.Join(ErrFactsetInconsistent, errors.New("p4offline: "+what+
			" is not valid UTF-8, so this factset's own encoding cannot carry it unchanged"))
	}
	for _, f := range [...]struct {
		what string
		v    string
	}{
		{"contract version", fs.ContractVersion},
		{"protocol", fs.Protocol},
		{"projector revision", fs.ProjectorRevision},
		{"episode collector session id", fs.Episode.CollectorSessionID},
		{"episode pool instance id", fs.Episode.PoolInstanceID},
		{"episode round incarnation id", fs.Episode.RoundIncarnationID},
		{"episode event id", fs.Episode.EventID},
		{"attempt collector session id", fs.Attempt.CollectorSessionID},
		{"attempt pool instance id", fs.Attempt.PoolInstanceID},
		{"terminal observation id", fs.TerminalObservationID},
		{"completeness", string(fs.Completeness)},
		{"pre-decision exit", fs.PreDecisionExit},
		{"stealth proof", string(fs.StealthProof)},
		{"health state", fs.HealthState},
		{"health reason", fs.HealthReason},
	} {
		if !utf8.ValidString(f.v) {
			return lossy(f.what)
		}
	}
	for i, r := range fs.IncompleteReasons {
		if !utf8.ValidString(r) {
			return lossy("incompleteness reason " + strconv.Itoa(i))
		}
	}
	if s := fs.Settings; s != nil {
		switch {
		case !utf8.ValidString(s.Strategy):
			return lossy("settings strategy")
		case !utf8.ValidString(s.DelayMode):
			return lossy("settings delay mode")
		}
		if fc := s.FilterCondition; fc != nil {
			switch {
			case !utf8.ValidString(fc.By):
				return lossy("settings filter condition subject")
			case !utf8.ValidString(fc.Where):
				return lossy("settings filter condition comparator")
			}
		}
	}
	for i, out := range fs.Outcomes {
		if !utf8.ValidString(out.ID) {
			return lossy("outcome " + strconv.Itoa(i) + " id")
		}
	}
	// The index is folded in HERE, inside the refusal, rather than into a label
	// built once per outcome above the switch. fs.Outcomes is a caller's slice
	// that nothing bounds before the digest gate, and such a label is
	// discarded on every iteration but the refusing one -- canonical.go's WORK
	// half, whose discriminator answers yes. Two independent review lanes
	// measured the version that got this wrong at 2.00x this function's
	// allocation count: 1,999,875 allocations against 999,975 on 1,000,000
	// outcomes refused by the digest gate.
	outcome := func(i int, what string, v float64) error {
		return unexpressible("outcome "+strconv.Itoa(i)+" "+what, v)
	}
	for i, o := range fs.Outcomes {
		switch {
		case !finiteFloat(o.PercentageUsers):
			return outcome(i, "percentage users", o.PercentageUsers)
		case !finiteFloat(o.Odds):
			return outcome(i, "odds", o.Odds)
		case !finiteFloat(o.OddsPercentage):
			return outcome(i, "odds percentage", o.OddsPercentage)
		}
	}
	if s := fs.Settings; s != nil {
		if !finiteFloat(s.Delay) {
			return unexpressible("settings delay", s.Delay)
		}
		if s.FilterCondition != nil && !finiteFloat(s.FilterCondition.Value) {
			return unexpressible("settings filter condition value", s.FilterCondition.Value)
		}
	}
	return nil
}

// finiteFloat is true for every value encoding/json can write: neither
// infinity and no NaN. Both zeroes are finite.
func finiteFloat(v float64) bool { return !math.IsInf(v, 0) && !math.IsNaN(v) }

// checkFactsetConsistency refuses a factset whose labels are not what its
// values derive: a stealth proof the settings do not give, a COMPLETE beside
// a missing input, a decision that was and was not reached.
func checkFactsetConsistency(fs CommonFactset) error {
	inconsistent := func(what string) error {
		return errors.Join(ErrFactsetInconsistent, errors.New("p4offline: "+what))
	}
	// SAME RULE, and this gate is why the rule is now stated by the OPERAND'S
	// PROVENANCE rather than by the statement's position: it is not a first
	// gate, it sits below the digest check, and it still reads a plain exported
	// typed string that nothing bounds. `want` is derived from the settings by
	// this package and is one of three short constants, so it is named; the
	// supplied label is not. See suppliedTextExtent.
	if want := deriveStealthProof(fs.Settings); fs.StealthProof != want {
		return inconsistent("stealth proof is " + suppliedTextExtent(string(fs.StealthProof)) +
			", the settings derive " + strconv.Quote(string(want)))
	}
	switch fs.Completeness {
	case FactsetComplete:
		if !fs.ReachedDecision || fs.PreDecisionExit != "" {
			return inconsistent("COMPLETE beside a decision that was not reached")
		}
		if len(fs.IncompleteReasons) != 0 {
			return inconsistent("COMPLETE beside incompleteness reasons")
		}
		if derived := valueDerivedReasons(fs); len(derived) != 0 {
			return inconsistent("COMPLETE beside " + joinReasons(derived))
		}
	case FactsetPreDecisionExit:
		if fs.ReachedDecision {
			return inconsistent("PRE_DECISION_EXIT beside a reached decision")
		}
	case FactsetIncomplete:
		if !fs.ReachedDecision {
			return inconsistent("INCOMPLETE beside a decision that was not reached")
		}
		if len(fs.IncompleteReasons) == 0 {
			return inconsistent("INCOMPLETE without a reason")
		}
		for _, r := range valueDerivedReasons(fs) {
			if !containsID(fs.IncompleteReasons, r) {
				return inconsistent("INCOMPLETE without the value-derived reason " + r)
			}
		}
	default:
		return inconsistent("completeness of " + suppliedTextExtent(string(fs.Completeness)) +
			" is outside the vocabulary")
	}
	return nil
}

// BindP2Config is seam 5: the exact per-case configuration binding.
func BindP2Config(fs CommonFactset) (P2ConfigBinding, error) {
	if err := VerifyCommonFactset(fs); err != nil {
		return P2ConfigBinding{}, err
	}
	if fs.Settings == nil {
		return P2ConfigBinding{}, ErrConfigBindingMissing
	}
	b := P2ConfigBinding{
		ContractVersion:     P2ConfigBindingVersion,
		Model:               predictioneval.CurrentModelProvenance(),
		Settings:            *copySettings(fs.Settings),
		RiskPresent:         fs.RiskPresent,
		RiskMaxStakePercent: fs.RiskMaxStakePercent,
		RiskReservePoints:   fs.RiskReservePoints,
		MinimumStake:        fs.MinimumStake,
	}
	var c canonical
	c.str(P2ConfigBindingVersion)
	c.str(b.Model.ModelVersion)
	c.str(b.Model.PolicyRevision)
	c.str(b.Model.SupportedProducerRevision)
	c.i64(int64(b.Model.PlatformIntBits))
	serializeSettings(&c, &b.Settings)
	c.boolean(b.RiskPresent)
	c.i64(int64(b.RiskMaxStakePercent))
	c.i64(int64(b.RiskReservePoints))
	c.i64(int64(b.MinimumStake))
	b.Digest = c.digest()
	return b, nil
}

// outcomeIDs is the factset's ordered outcome identity vector.
func outcomeIDs(fs CommonFactset) []string {
	out := make([]string, len(fs.Outcomes))
	for i, o := range fs.Outcomes {
		out[i] = o.ID
	}
	return out
}

func serializeSettings(c *canonical, s *predictioneval.BetSettingsInput) {
	c.boolean(s != nil)
	if s == nil {
		return
	}
	c.str(s.Strategy)
	c.i64(int64(s.Percentage))
	c.i64(int64(s.PercentageGap))
	c.i64(int64(s.MaxPoints))
	c.i64(int64(s.MinimumPoints))
	c.boolean(s.StealthMode)
	c.f64(s.Delay)
	c.str(s.DelayMode)
	c.boolean(s.FilterCondition != nil)
	if s.FilterCondition != nil {
		c.str(s.FilterCondition.By)
		c.str(s.FilterCondition.Where)
		c.f64(s.FilterCondition.Value)
	}
}

func serializeOptionalInt64(c *canonical, v *int64) {
	c.boolean(v != nil)
	if v != nil {
		c.i64(*v)
	}
}

func copySettings(s *predictioneval.BetSettingsInput) *predictioneval.BetSettingsInput {
	if s == nil {
		return nil
	}
	out := *s
	if s.FilterCondition != nil {
		fc := *s.FilterCondition
		out.FilterCondition = &fc
	}
	return &out
}

func copyInt64(v *int64) *int64 {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}
