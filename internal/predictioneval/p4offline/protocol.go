package p4offline

// The frozen P4 protocol pins.
//
// These are the owner-approved, preregistered parameters of the P4 comparison,
// recorded as constants so a future runner consumes them from one place and
// so no code in this package can silently redesign them. NOTHING in this
// package executes the comparison these parameters govern; they are carried
// so the artifacts this package produces name the protocol they were produced
// under, and [FrozenProtocol] exposes them as one value a test pins against
// the contract's own figures.
const (
	// ProtocolVersion names the P4 offline adapter/scorer contract.
	ProtocolVersion = "p4-offline/v1"

	// PolicyP2 and PolicyP3b are the ONLY two policies the protocol compares.
	PolicyP2  = "P2"
	PolicyP3b = "P3B"

	// BoundaryRuleCommonCutoff: both policies are evaluated over the common
	// factset that became available at the persisted terminal envelope (C),
	// and C must strictly precede the earliest relevant raw CALL_STARTED (F).
	BoundaryRuleCommonCutoff = "COMMON_CUTOFF"

	// P3bProjectionMode: the P3b core is fed exactly ONE candidate — the
	// selected opportunity's decision-time calculate input — never a
	// reconstructed channel stream.
	P3bProjectionMode = "CALCULATE_INPUT_SINGLE_CANDIDATE"

	// FactualStealthOff: a case is admitted only when its factual settings
	// prove stealth mode was OFF. Stealth on, or unknown, excludes the case.
	// It is the same string [StealthProofOff] carries on every factset.
	FactualStealthOff = "FACTUAL_STEALTH_OFF"
	// NoStealthSignal: no observed stealth realization reaches any policy
	// input or any entropy; P2 consumes no entropy at all.
	NoStealthSignal = "NO_STEALTH_SIGNAL"

	// PrimaryMetric is the preregistered primary metric. Its denominator is,
	// per policy and run, the resolved WOULD_ATTEMPT decisions of the
	// PRIMARY_SCORABLE cases ([AssessDenominatorMembership]).
	PrimaryMetric = "POLICY_CHOICE_ACCURACY"

	// RequiredCompleteUTCWeeks is the preregistered observation window.
	RequiredCompleteUTCWeeks = 8
	// MinimumPrimaryScorableRounds is the preregistered floor on complete,
	// primary-scorable, unique source rounds.
	MinimumPrimaryScorableRounds = 200
	// TrajectoryCount is the fixed number of entropy trajectories.
	TrajectoryCount = 16384
	// MCSEToleranceBasisPoints is the preregistered Monte-Carlo standard
	// error ceiling on the primary metric: 0.1 percentage point, expressed
	// in basis points so it is an integer.
	MCSEToleranceBasisPoints = 10

	// EvidenceMode: every P4 claim is descriptive; nothing is causal.
	EvidenceMode = "DESCRIPTIVE_ONLY"
	// ManualEpisodesExcluded: an episode touched by a manual intervention is
	// excluded whole.
	ManualEpisodesExcluded = "MANUAL_EPISODES_EXCLUDED"
	// AdmittedSessionCloseState / AdmittedSessionReading are the ONLY session
	// classifications whose facts may become P4 evidence.
	AdmittedSessionCloseState = "COMPLETE"
	AdmittedSessionReading    = "AS_FINALIZED"
	// PlacementPayoutMode: placement and payout are read from evidence only.
	PlacementPayoutMode = "EVIDENCE_ONLY"
	// BankrollPathMetrics are out of scope.
	BankrollPathMetrics = "OUT_OF_SCOPE"
	// FaithfulFullPolicy is on HOLD: no result of this package is a replay of
	// either policy's full runtime behaviour.
	FaithfulFullPolicy = "HOLD"
)

// Contract identities of the artifacts this package produces. Each is a
// domain-separation string hashed FIRST into the artifact's digest, so a
// change to an encoding produces a visibly different digest rather than a
// silently incompatible one.
const (
	CommonFactsetDigestVersion   = "p4-common-factset/v1"
	P2ConfigBindingVersion       = "p4-p2-config-binding/v1"
	NativeActionMapVersion       = "p4-native-action-map/v1"
	ResolutionFactsDigestVersion = "p4-resolution-facts/v1"
	PlacementEvidenceVersion     = "p4-placement-evidence-only/v1"
	PayoutEvidenceVersion        = "p4-payout-evidence-only/v1"
	EntropyAlgorithmVersion      = "p4-hmac-sha256-u64/v1"
	P3bProjectionManifestID      = "p4-calculate-input-single-candidate/v1"
	SourceRoundRegistryVersion   = "p4-source-round-registry/v1"
)

// ProtocolPins is the frozen protocol as one value.
type ProtocolPins struct {
	Protocol                     string   `json:"protocol"`
	Policies                     []string `json:"policies"`
	BoundaryRule                 string   `json:"boundaryRule"`
	P3bProjectionMode            string   `json:"p3bProjectionMode"`
	StealthRule                  string   `json:"stealthRule"`
	EntropySignalRule            string   `json:"entropySignalRule"`
	PrimaryMetric                string   `json:"primaryMetric"`
	RequiredCompleteUTCWeeks     int      `json:"requiredCompleteUtcWeeks"`
	MinimumPrimaryScorableRounds int      `json:"minimumPrimaryScorableRounds"`
	TrajectoryCount              int      `json:"trajectoryCount"`
	MCSEToleranceBasisPoints     int      `json:"mcseToleranceBasisPoints"`
	EvidenceMode                 string   `json:"evidenceMode"`
	ManualEpisodes               string   `json:"manualEpisodes"`
	AdmittedSessionCloseState    string   `json:"admittedSessionCloseState"`
	AdmittedSessionReading       string   `json:"admittedSessionReading"`
	PlacementPayoutMode          string   `json:"placementPayoutMode"`
	BankrollPathMetrics          string   `json:"bankrollPathMetrics"`
	FaithfulFullPolicy           string   `json:"faithfulFullPolicy"`
	EntropyAlgorithm             string   `json:"entropyAlgorithm"`
	ResolutionObligations        string   `json:"resolutionObligations"`
}

// FrozenProtocol returns the pins. It allocates a fresh value on every call
// so no caller can alter the constants through it.
func FrozenProtocol() ProtocolPins {
	return ProtocolPins{
		Protocol:                     ProtocolVersion,
		Policies:                     []string{PolicyP2, PolicyP3b},
		BoundaryRule:                 BoundaryRuleCommonCutoff,
		P3bProjectionMode:            P3bProjectionMode,
		StealthRule:                  FactualStealthOff,
		EntropySignalRule:            NoStealthSignal,
		PrimaryMetric:                PrimaryMetric,
		RequiredCompleteUTCWeeks:     RequiredCompleteUTCWeeks,
		MinimumPrimaryScorableRounds: MinimumPrimaryScorableRounds,
		TrajectoryCount:              TrajectoryCount,
		MCSEToleranceBasisPoints:     MCSEToleranceBasisPoints,
		EvidenceMode:                 EvidenceMode,
		ManualEpisodes:               ManualEpisodesExcluded,
		AdmittedSessionCloseState:    AdmittedSessionCloseState,
		AdmittedSessionReading:       AdmittedSessionReading,
		PlacementPayoutMode:          PlacementPayoutMode,
		BankrollPathMetrics:          BankrollPathMetrics,
		FaithfulFullPolicy:           FaithfulFullPolicy,
		EntropyAlgorithm:             EntropyAlgorithmVersion,
		ResolutionObligations:        ResolutionObligationsRevision,
	}
}
