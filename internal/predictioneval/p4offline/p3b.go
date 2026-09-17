package p4offline

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strconv"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
)

// SEAM 6 and the P3b plumbing.
//
// The P3b ruleset is SUPPLIED — raw bytes, their SHA-256, the typed config
// and the core's own config digest — and every one of those is verified
// against the others before a ruleset is used. Nothing here names a default
// ruleset: no candidate is hard-coded, ranked, tuned or chosen.
//
// The projection feeds the core exactly ONE candidate, built from the
// outcome-free common factset: the ordered points vector and the balance at
// the cutoff position, and nothing P2-only (no strategy, filter, users, top
// stake, odds, health, risk). The stream is declared what it is — a
// decision-time CALCULATE snapshot in a CALCULATE_ONLY view — and its
// coverage is COMPLETE_DECLARED only because seam 1 proved C < F first: the
// projection refuses a factset that did not come through that proof.
//
// Every evaluation entry point takes the FACTSET and projects it itself, so
// no caller can hand in a stream that names one factset and carries another;
// and the entropy coordinates must name the factset's own digest and its own
// round as the paired opportunity, so no opportunity is ever drawn under
// another opportunity's words. The ruleset is not an entropy coordinate — the
// approved framing draws per paired opportunity and run under the fixed
// P3B_ORDERED_RULES label — so every candidate ruleset of one comparison sees
// the same words; the ruleset is bound through the core's own config digest.

// Ruleset and P3b refusals.
var (
	// ErrRulesetIdentity is a ruleset whose identity is empty or differs from
	// its config's identity.
	ErrRulesetIdentity = errors.New("p4offline: ruleset identity is missing or does not match its config")
	// ErrRulesetRawSize is a raw document past the declared ceiling. It is
	// separate from ErrRulesetRawHash because it is refused BEFORE the hash:
	// the point of the ceiling is not to reject a wrong hash, it is to decline
	// to spend the hash at all.
	ErrRulesetRawSize = errors.New("p4offline: raw ruleset bytes are past the declared ceiling")
	// ErrRulesetRawHash is a ruleset whose raw bytes are absent or do not hash
	// to the declared SHA-256.
	ErrRulesetRawHash = errors.New("p4offline: raw ruleset bytes do not match the declared SHA-256")
	// ErrRulesetRawDecode is a raw document that is not exactly one ordered-
	// rules config spelled the way the contract spells it.
	ErrRulesetRawDecode = errors.New("p4offline: raw ruleset bytes do not decode to exactly one ordered-rules config")
	// ErrRulesetConfigMismatch is a typed config that is not what the raw
	// bytes decode to.
	ErrRulesetConfigMismatch = errors.New("p4offline: raw ruleset bytes decode to a config that differs from the supplied one")
	// ErrRulesetConfigUnusable is a config the P3b core itself refuses.
	ErrRulesetConfigUnusable = errors.New("p4offline: the P3b core refuses this ruleset")
	// ErrRulesetNativeDigest is a declared native digest that is not the one
	// the core reports for the config.
	ErrRulesetNativeDigest = errors.New("p4offline: the P3b core's config digest does not match the declared one")
	// ErrRulesetNotVerified is a VerifiedP3bRuleset that was not produced by
	// VerifyP3bRuleset, or was altered afterwards.
	ErrRulesetNotVerified = errors.New("p4offline: ruleset was not verified by this package or was altered after verification")
	// ErrP3bProjectionRefused is a factset the native projection refuses.
	ErrP3bProjectionRefused = errors.New("p4offline: the P3b projection refused the common factset")
	// ErrP3bBinding is an evaluation whose coordinates, stream or config are
	// not bound to the factset and ruleset it claims.
	ErrP3bBinding = errors.New("p4offline: the P3b evaluation is not bound to its inputs")
)

// P3bRuleset is the explicitly supplied ruleset with its hashes.
type P3bRuleset struct {
	// RulesetID names the ruleset and must equal Config.ConfigID.
	RulesetID string `json:"rulesetId"`
	// RawBytes is the ruleset document exactly as supplied.
	RawBytes []byte `json:"rawBytes"`
	// RawSHA256 is the declared lower-case hex SHA-256 of RawBytes.
	RawSHA256 string `json:"rawSha256"`
	// Config is the typed form the caller decoded from RawBytes.
	Config predictioneval.OrderedRulesConfig `json:"config"`
	// NativeConfigDigest is the declared pe-ors-config/v1 digest the core
	// reports for Config.
	NativeConfigDigest string `json:"nativeConfigDigest"`
}

// VerifiedP3bRuleset is a ruleset every binding of which has been checked.
// Only [VerifyP3bRuleset] produces a usable value: the witness it carries is
// unexported, is recomputed over the identity fields at every evaluation
// (the Rules count is read without it and settles nothing), and does
// not survive a JSON round trip — a stored ruleset is re-verified from its
// raw bytes, never trusted from its typed form.
type VerifiedP3bRuleset struct {
	RulesetID          string                            `json:"rulesetId"`
	RawSHA256          string                            `json:"rawSha256"`
	NativeConfigDigest string                            `json:"nativeConfigDigest"`
	Config             predictioneval.OrderedRulesConfig `json:"config"`
	witness            string
}

// Rules is the number of ordered detailed rules.
func (r VerifiedP3bRuleset) Rules() int { return len(r.Config.Detailed) }

// check refuses a value that VerifyP3bRuleset did not produce as it is: the
// identity fields must match the witness, and the config must still digest
// — in the core's own terms — to the verified native digest. The probe runs
// again here so a config mutated after verification into a shape the core
// refuses (which reports no digest at all) is caught as firmly as one
// mutated into a shape it accepts.
func (r VerifiedP3bRuleset) check() error {
	if r.witness == "" || r.witness != rulesetWitness(r.RulesetID, r.RawSHA256, r.NativeConfigDigest) {
		return ErrRulesetNotVerified
	}
	digest, err := nativeConfigDigest(r.Config)
	if err != nil || digest != r.NativeConfigDigest {
		return errors.Join(ErrRulesetNotVerified, errors.New("p4offline: the config no longer digests to the verified native digest"))
	}
	return nil
}

// nativeConfigDigest reads the core's own digest of a config through the
// fixed probe. A config the core refuses before the traversal has none.
func nativeConfigDigest(cfg predictioneval.OrderedRulesConfig) (string, error) {
	probe, err := p3bProbeStream()
	if err != nil {
		return "", err
	}
	ev := predictioneval.EvaluateOrderedRules(probe, cfg, predictioneval.SuppliedDrawTrace{
		RunID:                   "p4offline-ruleset-probe",
		EntropySemanticsVersion: predictioneval.OrderedRulesEntropySemanticsVersion,
	})
	if ev.ConfigDigest == "" {
		return "", errors.Join(ErrRulesetConfigUnusable,
			errors.New("p4offline: core status "+string(ev.Status)+" reason "+ev.Reason))
	}
	return ev.ConfigDigest, nil
}

func rulesetWitness(id, rawSHA256, nativeDigest string) string {
	var c canonical
	c.str("p4offline-verified-ruleset-witness")
	c.str(id)
	c.str(rawSHA256)
	c.str(nativeDigest)
	return c.digest()
}

// P3bProjection is the one-candidate stream projected from a factset.
type P3bProjection struct {
	Mode              string                            `json:"mode"`
	FactsetDigest     string                            `json:"factsetDigest"`
	EventID           string                            `json:"eventId"`
	CutoffPosition    int64                             `json:"cutoffPosition"`
	CandidateIdentity string                            `json:"candidateIdentity"`
	OutcomeCount      int                               `json:"outcomeCount"`
	Stream            predictioneval.OrderedRulesStream `json:"stream"`
}

// EntropyTraceBinding is the trace one P3b evaluation consumed, bound to its
// coordinates and to the core's own entropy digest. A refused projection
// draws no trace: its binding carries the coordinates and the identity of
// the EMPTY trace for them, with no words supplied or consumed and no
// entropy digest — a non-empty [P3bCaseResult.ProjectionRefusal] and an
// empty EntropyDigest, not the run identity, are what mark a projection
// refusal (the core's own REFUSED status maps to the same ActionRefused
// class on a path where a trace WAS consumed).
type EntropyTraceBinding struct {
	Coordinates   EntropyCoordinates `json:"coordinates"`
	RunID         string             `json:"runId"`
	WordsSupplied int                `json:"wordsSupplied"`
	WordsConsumed int                `json:"wordsConsumed"`
	EntropyDigest string             `json:"entropyDigest"`
}

// P3bCaseResult is the P3b side of one case, for one trajectory.
type P3bCaseResult struct {
	Policy             string                                `json:"policy"`
	FactsetDigest      string                                `json:"factsetDigest"`
	RulesetID          string                                `json:"rulesetId"`
	RulesetRawSHA256   string                                `json:"rulesetRawSha256"`
	NativeConfigDigest string                                `json:"nativeConfigDigest"`
	Projection         P3bProjection                         `json:"projection"`
	ProjectionRefusal  string                                `json:"projectionRefusal,omitempty"`
	Trace              EntropyTraceBinding                   `json:"trace"`
	Evaluation         predictioneval.OrderedRulesEvaluation `json:"evaluation"`
	Action             ActionMapping                         `json:"action"`
	Choice             PolicyChoice                          `json:"choice"`
	Stake              Int64Fact                             `json:"stake"`
	witness            string
}

// derived reports whether the value is exactly what the P3b evaluator
// produced.
func (r P3bCaseResult) derived() bool {
	return r.witness != "" && r.witness == p3bResultWitness(r)
}

// p3bResultWitness frames every field a decision is minted from — the
// action, the choice, the stake — and these fields an audit reads: the
// policy and factset digest; the ruleset's id, raw hash and native digest;
// the refusal text; the whole trace binding; the projection's mode, factset
// digest, event, cutoff position, candidate identity, outcome count and
// selection digest; the native evaluation's status, reason, stream, config,
// entropy and consumed-input digests, raw words consumed and Bernoulli
// evaluations.
//
// NOT framed, exactly: the projection's stream body (only its selection
// digest is framed; an edit to the body that leaves that digest untouched is
// not detected here) and, of the native evaluation, EvidenceLabel,
// ModelVersion, DonorRevision, EntropySemanticsVersion, StoppedAtCandidate,
// StoppedAtPosition, HasStopPosition, Selected, Participation, Stake,
// CandidatesConsumed, OutcomesConsidered, RulesConsidered, Cutoff, Trace,
// Visits and Qualifications. The framed digests bind the run's INPUTS, not
// its verdict; the verdict is bound only through the framed action, choice,
// stake, status and reason, so an edit confined to the unframed fields is
// not detected here. A stored result is re-evaluated, never trusted.
func p3bResultWitness(r P3bCaseResult) string {
	var c canonical
	c.str("p4offline-p3b-result-witness")
	c.str(r.Policy)
	c.str(r.FactsetDigest)
	c.str(r.RulesetID)
	c.str(r.RulesetRawSHA256)
	c.str(r.NativeConfigDigest)
	c.str(r.ProjectionRefusal)
	c.str(r.Trace.Coordinates.DatasetID)
	c.str(r.Trace.Coordinates.DatasetVersion)
	c.str(r.Trace.Coordinates.CommonFactsetDigest)
	c.str(r.Trace.Coordinates.PairedOpportunityID)
	c.u64(uint64(r.Trace.Coordinates.Trajectory))
	c.str(r.Trace.RunID)
	c.i64(int64(r.Trace.WordsSupplied))
	c.i64(int64(r.Trace.WordsConsumed))
	c.str(r.Trace.EntropyDigest)
	c.str(r.Projection.Mode)
	c.str(r.Projection.FactsetDigest)
	c.str(r.Projection.EventID)
	c.i64(r.Projection.CutoffPosition)
	c.str(r.Projection.CandidateIdentity)
	c.i64(int64(r.Projection.OutcomeCount))
	c.str(r.Projection.Stream.SelectionDigest)
	c.str(string(r.Evaluation.Status))
	c.str(r.Evaluation.Reason)
	c.str(r.Evaluation.StreamDigest)
	c.str(r.Evaluation.ConfigDigest)
	c.str(r.Evaluation.EntropyDigest)
	c.str(r.Evaluation.ConsumedInputDigest)
	c.i64(int64(r.Evaluation.RawWordsConsumed))
	c.i64(int64(r.Evaluation.BernoulliEvaluations))
	frameAction(&c, r.Action)
	frameChoice(&c, r.Choice)
	frameFact(&c, r.Stake)
	return c.digest()
}

// NativeActionP4ProjectionRefused is the pseudo-native action a P3b result
// carries when the projection itself refused the factset.
const NativeActionP4ProjectionRefused = "P4_PROJECTION_REFUSED"

// rulesetKeySets is the exact key spelling of the ordered-rules config
// document at every object path. The raw document must use these spellings
// and no key twice, and may hold objects only at these paths: encoding/json
// alone would accept a case-folded or a duplicated key and silently read a
// different config than a strict parser. The spelling is checked on the
// DECODED key, so a JSON escape of the same characters is the same key; the
// raw hash still pins the exact bytes for the audit.
var rulesetKeySets = map[string][]string{
	"":                {"configId", "hasDefault", "detailed", "default"},
	"detailed":        {"comparator", "rawThresholdPercent", "rawAttemptRatePercent", "points"},
	"detailed.points": {"maxValue", "rawPercent"},
	"default":         {"rawMinPercent", "rawMaxPercent", "points"},
	"default.points":  {"maxValue", "rawPercent"},
}

// rulesetOptionalKeys names, per object path, the keys a document may omit;
// every other key the contract spells is MANDATORY. Every field of the
// config has a legitimate zero, so a key the document omits — or spells
// with a null — would decode to a value the core cannot tell from an
// explicit one: a document without its default would be accepted as a
// resolved configuration whose default is [0,0]. The document must spell
// every mandatory key with a value. Only "detailed" is optional, at the
// root, and only there is null allowed: absent, null and empty all mean no
// detailed rules to the core.
var rulesetOptionalKeys = map[string][]string{
	"": {"detailed"},
}

// rulesetNullOK names the one path whose value may be null.
const rulesetNullOK = "detailed"

// rulesetStructuralAllowance is everything a raw ruleset document can hold
// APART from its identifier: the JSON envelope, the default object, and a full
// detailed list of MaxOrderedRulesRules rules, plus the widening a conformant
// encoder may apply: indentation, and every key and string token written as a
// six-byte \u escape. TestRulesetRawDocumentIsCappedBeforeItIsHashed builds that
// document and reports its size rather than leaving the figure here; at the time
// of writing it measures about 100 KB, a tenfold margin. It is stated as a
// margin and not as a bound: the test is what has to keep holding.
const rulesetStructuralAllowance = 1 << 20

// rulesetRawCeiling is the largest raw document the ruleset this call DECLARES
// could legitimately occupy. It is O(1) and is applied before the hash.
//
// WHY THE CEILING IS DERIVED AND NOT A CONSTANT. A flat ceiling was tried here
// first and was wrong in the direction that matters: it refused honest input.
// ConfigID carries NO per-string length bound -- the producer says so and gives
// the reason ("inventing a limit no other rule applies would refuse input
// nothing else refuses", ordered_rules.go), charges it only against
// MaxOrderedRulesAggregateBytes, and pins a mebibyte identifier as legal in its
// own suite. Larger ones are argued there rather than pinned, and the producer
// says so itself, so this does not claim more than its source does. A flat mebibyte therefore refused a document the
// contract admits, which is a fail-closed defect worse than the unbounded work
// it was added to stop. The ceiling now SCALES with the identifier the document
// declares, so it cannot refuse a large legal one, while a small identifier
// still buys only a small document.
//
// The six is JSON escaping's worst case per byte -- the same charge the
// producer applies when it bounds encoded rather than raw width.
//
// WHAT THIS DOES NOT DO, stated rather than discovered. The ceiling is derived
// from the SUPPLIED config, which is not yet known to match the raw bytes. A
// supplier can therefore raise their own ceiling by declaring a huge ConfigID
// -- but then they must actually carry those bytes, or configsEqual below
// refuses, and if they do carry them the document is legitimately that large.
// So the work this bounds is bounded by a size the contract already admits,
// not by a smaller number this package would have had to invent. It also does
// not bound what the CALLER already allocated: the raw bytes arrive
// materialized, so what is declined here is the linear work over them -- the
// hash, the key walk, the decode and the trailing scan -- not their memory.
// The multiplication cannot overflow an int on any platform this builds for: a
// 32-bit int would need an identifier past 358 MB, which the caller would have
// had to materialize first.
func rulesetRawCeiling(cfg predictioneval.OrderedRulesConfig) int {
	// Capped at the identifier the native core can still digest, so the ceiling
	// is bounded above by a constant rather than being unbounded in a field the
	// caller declares. The bound is MaxOrderedRulesAggregateBytes because that
	// is the ONLY limit that exists on ConfigID anywhere, and it is a RAW-length
	// one: the producer charges `b.other += len(cfg.ConfigID) + len(d.RunID)`
	// and compares `other` against the full constant. The sixfold JSON charge
	// applies to the stream total, not to this field -- "two inputs, two
	// ceilings" in the producer's own words.
	//
	// STATED, because it decides whether this is a repair or a defect: a tighter
	// absolute cap WOULD refuse honest input. An identifier at the aggregate
	// bound, written entirely as \u escapes, is a legal 805,306,353-byte
	// document that verifies; capping the term at anything below
	// 6*MaxOrderedRulesAggregateBytes refuses it. The sixfold is reachable by
	// legal input, not slack.
	//
	// ALSO STATED: this cap carries no behavioural test. Its whole effect is a
	// six-byte difference at an identifier one byte past the aggregate bound,
	// observable only on a document of roughly 806 MB, so no test at a sane cost
	// can reach it. It is adopted because it costs legal input exactly nothing
	// and makes the guarantee statable, not because a test proves it.
	id := len(cfg.ConfigID)
	if id > predictioneval.MaxOrderedRulesAggregateBytes {
		id = predictioneval.MaxOrderedRulesAggregateBytes
	}
	return rulesetStructuralAllowance + 6*id
}

// rulesetIdentityFault names the identity fault WITHOUT re-exporting either
// identifier.
//
// The gate that calls it is the first thing VerifyP3bRuleset does, ahead of the
// raw-byte ceiling and of every native gate, so on this path nothing bounds the
// work at all. Quoting the two supplied identifiers there cost 476 ms and 235 MB
// of allocation on a 64 MiB config id -- to report that two strings differ.
//
// The native producer found the same defect in its own gate and wrote the rule
// down: a 125 MiB identifier beside a wrong contract version "cost 1.23 seconds
// and allocated 251,662,352 bytes -- to compare two short strings and find them
// different. It is 3.8 microseconds and nothing now" (ordered_rules.go), which
// is why orderedRulesUnreadRefusal re-exports none of the supplied text. This
// reports the fault and the LENGTHS, which are O(1) on a string header, and
// keeps the three faults distinguishable.
func rulesetIdentityFault(r P3bRuleset) string {
	switch {
	case r.RulesetID == "":
		return "p4offline: ruleset id is empty"
	case r.Config.ConfigID == "":
		return "p4offline: config id is empty beside a ruleset id of " +
			strconv.Itoa(len(r.RulesetID)) + " bytes"
	default:
		return "p4offline: ruleset id and config id differ, at " +
			strconv.Itoa(len(r.RulesetID)) + " and " +
			strconv.Itoa(len(r.Config.ConfigID)) + " bytes"
	}
}

// VerifyP3bRuleset checks every binding of a supplied ruleset.
func VerifyP3bRuleset(r P3bRuleset) (VerifiedP3bRuleset, error) {
	if r.RulesetID == "" || r.Config.ConfigID != r.RulesetID {
		return VerifiedP3bRuleset{}, errors.Join(ErrRulesetIdentity, errors.New(rulesetIdentityFault(r)))
	}
	if len(r.RawBytes) == 0 {
		return VerifiedP3bRuleset{}, errors.Join(ErrRulesetRawHash, errors.New("p4offline: no raw ruleset bytes supplied"))
	}
	// Before the hash, deliberately: see rulesetRawCeiling. Nothing below bounds
	// the CARRIER, only the decoded config, so this is the one place a
	// supplier-controlled buffer stops being paid for.
	if ceiling := rulesetRawCeiling(r.Config); len(r.RawBytes) > ceiling {
		return VerifiedP3bRuleset{}, errors.Join(ErrRulesetRawSize,
			errors.New("p4offline: "+strconv.Itoa(len(r.RawBytes))+" raw bytes supplied; this ruleset's ceiling is "+
				strconv.Itoa(ceiling)))
	}
	if !isCanonicalHex(r.RawSHA256, 64) {
		return VerifiedP3bRuleset{}, errors.Join(ErrRulesetRawHash,
			errors.New("p4offline: declared raw hash is not 64 lower-case hex digits"))
	}
	if got := sha256Hex(r.RawBytes); got != r.RawSHA256 {
		return VerifiedP3bRuleset{}, errors.Join(ErrRulesetRawHash,
			errors.New("p4offline: raw bytes hash to "+got+", declared "+r.RawSHA256))
	}
	if err := checkRulesetKeys(r.RawBytes); err != nil {
		return VerifiedP3bRuleset{}, errors.Join(ErrRulesetRawDecode, err)
	}
	dec := json.NewDecoder(bytes.NewReader(r.RawBytes))
	dec.DisallowUnknownFields()
	var decoded predictioneval.OrderedRulesConfig
	if err := dec.Decode(&decoded); err != nil {
		return VerifiedP3bRuleset{}, errors.Join(ErrRulesetRawDecode, err)
	}
	if rest := bytes.TrimSpace(r.RawBytes[dec.InputOffset():]); len(rest) != 0 {
		return VerifiedP3bRuleset{}, errors.Join(ErrRulesetRawDecode,
			errors.New("p4offline: raw bytes continue after the first document"))
	}
	if !configsEqual(decoded, r.Config) {
		return VerifiedP3bRuleset{}, ErrRulesetConfigMismatch
	}
	if !isCanonicalHex(r.NativeConfigDigest, 64) {
		return VerifiedP3bRuleset{}, errors.Join(ErrRulesetNativeDigest,
			errors.New("p4offline: declared native digest is not 64 lower-case hex digits"))
	}
	// The core's own digest is observable only through an evaluation, so a
	// tiny fixed probe — two equal outcomes, a zero balance, no entropy — is
	// evaluated to obtain it. The probe's answer is discarded; only the
	// config digest it reports is used. A config the core refuses before it
	// reaches the traversal reports none and is unusable. The probe runs over
	// BOTH the typed config and the decoded one, so a field the hand-written
	// comparison does not know about cannot slip between them.
	digest, err := nativeConfigDigest(r.Config)
	if err != nil {
		return VerifiedP3bRuleset{}, err
	}
	if digest != r.NativeConfigDigest {
		return VerifiedP3bRuleset{}, errors.Join(ErrRulesetNativeDigest,
			errors.New("p4offline: core reports "+digest+", declared "+r.NativeConfigDigest))
	}
	if decodedDigest, err := nativeConfigDigest(decoded); err != nil || decodedDigest != digest {
		return VerifiedP3bRuleset{}, errors.Join(ErrRulesetConfigMismatch,
			errors.New("p4offline: the core digests the decoded document differently from the supplied config"))
	}
	return VerifiedP3bRuleset{
		RulesetID:          r.RulesetID,
		RawSHA256:          r.RawSHA256,
		NativeConfigDigest: r.NativeConfigDigest,
		Config:             detachConfig(r.Config),
		witness:            rulesetWitness(r.RulesetID, r.RawSHA256, r.NativeConfigDigest),
	}, nil
}

// checkRulesetKeys walks the raw document token by token and refuses a
// duplicated key, a key not spelled exactly as the contract spells it, a
// mandatory key the document omits, and a null anywhere but "detailed".
func checkRulesetKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return errors.New("p4offline: raw ruleset document is not a JSON object")
	}
	return walkRulesetObject(dec, "")
}

func walkRulesetObject(dec *json.Decoder, path string) error {
	allowed, known := rulesetKeySets[path]
	if !known {
		return errors.New("p4offline: an object at " + pathName(path) + " has no place in the contract")
	}
	seen := map[string]bool{}
	for {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			if d == '}' {
				for _, req := range allowed {
					if containsID(rulesetOptionalKeys[path], req) {
						continue
					}
					if !seen[req] {
						return errors.New("p4offline: mandatory key " + strconv.Quote(req) + " is absent at " + pathName(path))
					}
				}
				return nil
			}
			return errors.New("p4offline: unexpected " + d.String() + " in object at " + pathName(path))
		}
		key, ok := tok.(string)
		if !ok {
			return errors.New("p4offline: object key at " + pathName(path) + " is not a string")
		}
		if seen[key] {
			return errors.New("p4offline: key " + strconv.Quote(key) + " appears twice at " + pathName(path))
		}
		seen[key] = true
		if !containsID(allowed, key) {
			return errors.New("p4offline: key " + strconv.Quote(key) + " at " + pathName(path) + " is not spelled as the contract spells it")
		}
		child := key
		if path != "" {
			child = path + "." + key
		}
		if err := walkRulesetValue(dec, child, child == rulesetNullOK); err != nil {
			return err
		}
	}
}

func walkRulesetValue(dec *json.Decoder, path string, nullOK bool) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok == nil {
		if nullOK {
			return nil
		}
		return errors.New("p4offline: null at " + pathName(path) + " is not a value the contract admits there")
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch d {
	case '{':
		return walkRulesetObject(dec, path)
	case '[':
		for dec.More() {
			if err := walkRulesetValue(dec, path, false); err != nil {
				return err
			}
		}
		_, err := dec.Token()
		return err
	}
	return errors.New("p4offline: unexpected " + d.String() + " at " + pathName(path))
}

func pathName(path string) string {
	if path == "" {
		return "the document root"
	}
	return strconv.Quote(path)
}

// configsEqual compares two configs exactly: floats by bits, an absent
// detailed list equal to an empty one (the core treats them alike).
func configsEqual(a, b predictioneval.OrderedRulesConfig) bool {
	if a.ConfigID != b.ConfigID || a.HasDefault != b.HasDefault || len(a.Detailed) != len(b.Detailed) {
		return false
	}
	for i := range a.Detailed {
		x, y := a.Detailed[i], b.Detailed[i]
		if x.Comparator != y.Comparator ||
			math.Float64bits(x.RawThresholdPercent) != math.Float64bits(y.RawThresholdPercent) ||
			math.Float64bits(x.RawAttemptRatePercent) != math.Float64bits(y.RawAttemptRatePercent) ||
			x.Points.MaxValue != y.Points.MaxValue ||
			math.Float64bits(x.Points.RawPercent) != math.Float64bits(y.Points.RawPercent) {
			return false
		}
	}
	return math.Float64bits(a.Default.RawMinPercent) == math.Float64bits(b.Default.RawMinPercent) &&
		math.Float64bits(a.Default.RawMaxPercent) == math.Float64bits(b.Default.RawMaxPercent) &&
		a.Default.Points.MaxValue == b.Default.Points.MaxValue &&
		math.Float64bits(a.Default.Points.RawPercent) == math.Float64bits(b.Default.Points.RawPercent)
}

func detachConfig(c predictioneval.OrderedRulesConfig) predictioneval.OrderedRulesConfig {
	out := c
	if c.Detailed != nil {
		out.Detailed = append([]predictioneval.OrderedRule(nil), c.Detailed...)
	}
	return out
}

func knownAt(v int64, provenance string, position int64) predictioneval.SuppliedInt64 {
	return predictioneval.SuppliedInt64{
		Presence:               predictioneval.SuppliedKnown,
		Value:                  predictioneval.OrderedRulesInt64(v),
		Provenance:             provenance,
		AvailableAtPosition:    predictioneval.OrderedRulesInt64(position),
		HasAvailableAtPosition: true,
	}
}

// p3bProbeStream is the fixed probe used only to read a config's native
// digest. It is not evidence and its answer is never reported.
func p3bProbeStream() (predictioneval.OrderedRulesStream, error) {
	const p = "p4offline-ruleset-probe"
	stream, err := predictioneval.ProjectOrderedRulesStream(predictioneval.OrderedRulesSource{
		Scope: predictioneval.OrderedRulesScope{
			Namespace: p, EpisodeID: p, AccountContext: p, AssociationEvidence: p,
			SourceContractVersion: predictioneval.OrderedRulesStreamContractVersion,
			Coverage:              predictioneval.CoverageCompleteDeclared, HasInterval: true,
		},
		Candidates: []predictioneval.OrderedRulesCandidate{{
			Identity: p, HasPosition: true,
			SourceKind:        predictioneval.SourceKindCalculateSnapshot,
			EpisodeMembership: predictioneval.MembershipProven,
			OutcomesPresence:  predictioneval.SuppliedKnown,
			Outcomes: []predictioneval.OrderedRulesOutcome{
				{Identity: "a", Points: knownAt(1, p, 0)},
				{Identity: "b", Points: knownAt(1, p, 0)},
			},
			Balance: knownAt(0, p, 0),
		}},
	}, predictioneval.CommonAdmission{ManifestID: p, ViewKind: predictioneval.ViewCalculateOnly, Population: p, OrderBasis: p})
	if err != nil {
		return predictioneval.OrderedRulesStream{}, errors.Join(ErrRulesetConfigUnusable, err)
	}
	return stream, nil
}

// ProjectP3bSingleCandidate is seam 6: the one-candidate common-data
// projection of a COMPLETE factset.
func ProjectP3bSingleCandidate(fs CommonFactset) (P3bProjection, error) {
	if err := VerifyCommonFactset(fs); err != nil {
		return P3bProjection{}, err
	}
	if fs.Completeness != FactsetComplete || !fs.ReachedDecision {
		return P3bProjection{}, errors.Join(ErrFactsetNotEvaluable, errors.New("p4offline: factset is "+string(fs.Completeness)))
	}
	position := fs.CutoffPosition
	provenance := "p4-common-factset:" + fs.Digest
	identity := "attempt-" + strconv.FormatUint(fs.Attempt.AttemptID, 10)
	cand := predictioneval.OrderedRulesCandidate{
		Identity:          identity,
		Position:          predictioneval.OrderedRulesInt64(position),
		HasPosition:       true,
		SourceKind:        predictioneval.SourceKindCalculateSnapshot,
		EpisodeMembership: predictioneval.MembershipProven,
		OutcomesPresence:  predictioneval.SuppliedKnown,
		Provenance:        provenance,
	}
	for _, o := range fs.Outcomes {
		points := knownAt(int64(o.TotalPoints), provenance, position)
		if !o.Present {
			// A hole is not a zero-point outcome, and a vector with a hole
			// is not a whole vector.
			points = predictioneval.SuppliedInt64{Presence: predictioneval.SuppliedMissing, Reason: "P2_MODEL_VECTOR_HOLE"}
			cand.OutcomesPresence = predictioneval.SuppliedInvalid
			cand.OutcomesReason = "P2_MODEL_VECTOR_HOLE"
		}
		cand.Outcomes = append(cand.Outcomes, predictioneval.OrderedRulesOutcome{Identity: o.ID, Points: points})
	}
	if fs.BalancePresent {
		cand.Balance = knownAt(fs.Balance, provenance, position)
	} else {
		cand.Balance = predictioneval.SuppliedInt64{Presence: predictioneval.SuppliedMissing, Reason: predictioneval.IneligibleMissingBalance}
	}
	scope := predictioneval.OrderedRulesScope{
		Namespace: "p4offline",
		// The framed identity: a component containing a delimiter cannot
		// alias another episode.
		EpisodeID:             fs.Episode.String(),
		AccountContext:        "collector-session:" + fs.Episode.CollectorSessionID,
		AssociationEvidence:   "terminal-decision-envelope:" + fs.TerminalObservationID,
		SourceContractVersion: predictioneval.OrderedRulesStreamContractVersion,
		Coverage:              predictioneval.CoverageCompleteDeclared,
		CoverageDetail:        "COMMON_CUTOFF boundary C<F proven by p4offline.SelectEpisodes over a COMPLETE AS_FINALIZED session",
		IntervalFromPosition:  predictioneval.OrderedRulesInt64(position),
		IntervalToPosition:    predictioneval.OrderedRulesInt64(position),
		HasInterval:           true,
	}
	admission := predictioneval.CommonAdmission{
		ManifestID:       P3bProjectionManifestID,
		ViewKind:         predictioneval.ViewCalculateOnly,
		Population:       "the selected first automated opportunity's decision-time calculate input",
		OrderBasis:       "collector sequence of the terminal decision envelope",
		SourceReferences: []string{fs.TerminalObservationID},
	}
	stream, err := predictioneval.ProjectOrderedRulesStream(
		predictioneval.OrderedRulesSource{Scope: scope, Candidates: []predictioneval.OrderedRulesCandidate{cand}}, admission)
	if err != nil {
		return P3bProjection{}, errors.Join(ErrP3bProjectionRefused, err)
	}
	return P3bProjection{
		Mode:              P3bProjectionMode,
		FactsetDigest:     fs.Digest,
		EventID:           fs.Episode.EventID,
		CutoffPosition:    fs.CutoffPosition,
		CandidateIdentity: identity,
		OutcomeCount:      len(fs.Outcomes),
		Stream:            stream,
	}, nil
}

// traceWordCount is the number of words a one-candidate traversal can
// consume at most — one per rule per outcome — with the bounds checked
// BEFORE the product is formed.
func traceWordCount(rules, outcomes int) (int, error) {
	if rules < 0 || rules > predictioneval.MaxOrderedRulesRules {
		return 0, errors.Join(ErrEntropyCount, errors.New("p4offline: "+strconv.Itoa(rules)+" rules is outside the P3b bound"))
	}
	if outcomes < 0 || outcomes > predictioneval.MaxOrderedRulesOutcomes {
		return 0, errors.Join(ErrEntropyCount, errors.New("p4offline: "+strconv.Itoa(outcomes)+" outcomes is outside the P3b bound"))
	}
	return rules * outcomes, nil
}

// EvaluateP3bWithTrace evaluates one factset under a verified ruleset with a
// SUPPLIED trace: the factset is projected here, the coordinates must name
// the factset's own digest and round, the trace must be exactly what the
// algorithm produces for those coordinates, and every binding the core
// reports afterwards is checked.
func EvaluateP3bWithTrace(fs CommonFactset, rs VerifiedP3bRuleset,
	coords EntropyCoordinates, trace predictioneval.SuppliedDrawTrace) (P3bCaseResult, error) {
	if err := rs.check(); err != nil {
		return P3bCaseResult{}, err
	}
	// The protocol's domain is checked before anything is projected, so an
	// out-of-protocol coordinate is refused as exactly that on every path.
	if err := checkEntropyCoordinates(coords); err != nil {
		return P3bCaseResult{}, err
	}
	proj, err := ProjectP3bSingleCandidate(fs)
	if err != nil {
		return P3bCaseResult{}, err
	}
	if err := bindEntropyCoordinates(coords, fs); err != nil {
		return P3bCaseResult{}, err
	}
	return evaluateProjected(fs, proj, rs, coords, trace)
}

// bindEntropyCoordinates refuses coordinates outside the protocol and
// coordinates that do not name this factset: the digest reference must be
// the factset's own digest and the paired opportunity must be the factset's
// own round. The dataset identity and version are the caller's binding and
// are carried, not checked beyond the protocol's form. It runs on EVERY
// evaluation path, including the one that refuses the projection, so the
// closed domain does not depend on which factset was passed.
func bindEntropyCoordinates(coords EntropyCoordinates, fs CommonFactset) error {
	if err := checkEntropyCoordinates(coords); err != nil {
		return err
	}
	if want := DigestReference(fs.Digest); coords.CommonFactsetDigest != want {
		return errors.Join(ErrP3bBinding,
			errors.New("p4offline: entropy factset digest "+strconv.Quote(coords.CommonFactsetDigest)+" is not this factset's "+strconv.Quote(want)))
	}
	// The digest gate above quotes both sides deliberately: isDigestReference
	// holds each to 71 bytes, and naming them IS the diagnosis. This gate
	// quotes neither, for the opposite reason. The coordinate side is bounded
	// -- checkEntropyCoordinates ran first -- but the FACTSET's round is bounded
	// by nothing in this package: VerifyCommonFactset checks the two contract
	// strings, the digest and the label consistency, and no length. So every
	// factset whose round exceeds the coordinate bound necessarily fails this
	// equality, and quoting it made the refusal cost 2.47 GB of allocation on a
	// 64 MiB round, reached through the exported EvaluateP3bCase. Same rule as
	// rulesetIdentityFault and ValidateDrawTrace: the fault and the lengths.
	if coords.PairedOpportunityID != fs.Episode.EventID {
		return errors.Join(ErrP3bBinding,
			errors.New("p4offline: entropy paired opportunity of "+strconv.Itoa(len(coords.PairedOpportunityID))+
				" bytes is not the factset's round of "+strconv.Itoa(len(fs.Episode.EventID))+" bytes"))
	}
	return nil
}

func evaluateProjected(fs CommonFactset, proj P3bProjection, rs VerifiedP3bRuleset,
	coords EntropyCoordinates, trace predictioneval.SuppliedDrawTrace) (P3bCaseResult, error) {
	// The COUNT is judged before the copy, and the order is the whole point.
	// The core consumes the words it is handed without copying, so the words
	// must be detached from the caller's array before they are validated and
	// consumed -- otherwise the array that is checked is not the array that is
	// read. But detaching FIRST meant a supplied slice 64x past the declared
	// ceiling was duplicated in full before the gate whose only job is to
	// refuse it: 512 MB copied to refuse 512 MB. A slice header's length
	// cannot be changed under a value receiver, so reading it before the copy
	// costs the detachment property nothing.
	//
	// ONE CONSEQUENCE, a precedence shift rather than a behaviour change: a
	// trace that is BOTH past the ceiling AND carries a foreign semantics
	// version or run identity used to come back as ErrEntropyTrace, because
	// ValidateDrawTrace's first two gates ran before ValidateEntropyWords
	// reached the count. It now comes back as ErrEntropyCount. No input moves
	// between admitted and refused -- the same bound is applied to the same
	// length, one call earlier -- but a caller switching on the sentinel sees a
	// different one for that class.
	if err := checkEntropyCount(len(trace.Words)); err != nil {
		return P3bCaseResult{}, err
	}
	trace.Words = append([]predictioneval.OrderedRulesHex64(nil), trace.Words...)
	if err := ValidateDrawTrace(coords, trace); err != nil {
		return P3bCaseResult{}, err
	}
	ev := predictioneval.EvaluateOrderedRules(proj.Stream, rs.Config, trace)
	action := MapP3bAction(ev)
	admitted := action.Legal && determinate(action.Class)
	if admitted && (ev.StreamDigest == "" || ev.ConfigDigest == "") {
		return P3bCaseResult{}, errors.Join(ErrP3bBinding, errors.New("p4offline: core reported "+string(ev.Status)+" without its digests"))
	}
	if ev.StreamDigest != "" && ev.StreamDigest != proj.Stream.SelectionDigest {
		return P3bCaseResult{}, errors.Join(ErrP3bBinding, errors.New("p4offline: core stream digest differs from the projection"))
	}
	if ev.ConfigDigest != "" && ev.ConfigDigest != rs.NativeConfigDigest {
		return P3bCaseResult{}, errors.Join(ErrP3bBinding, errors.New("p4offline: core config digest differs from the verified ruleset"))
	}
	res := P3bCaseResult{
		Policy:             PolicyP3b,
		FactsetDigest:      fs.Digest,
		RulesetID:          rs.RulesetID,
		RulesetRawSHA256:   rs.RawSHA256,
		NativeConfigDigest: rs.NativeConfigDigest,
		Projection:         proj,
		Trace: EntropyTraceBinding{
			Coordinates:   coords,
			RunID:         trace.RunID,
			WordsSupplied: len(trace.Words),
			WordsConsumed: ev.RawWordsConsumed,
			EntropyDigest: ev.EntropyDigest,
		},
		Evaluation: ev,
		Action:     action,
	}
	if ev.Selected != nil && carriesChoice(action.Class) {
		res.Choice = PolicyChoice{Present: true, Index: ev.Selected.OutcomeIndex, OutcomeID: ev.Selected.OutcomeIdentity}
	}
	switch action.Class {
	case ActionWouldAttempt:
		// A known stake, INCLUDING zero: zero is an amount, not an abstention.
		res.Stake = KnownInt64(int64(ev.Stake.Value))
	case ActionParticipationAdmittedStakeUnknown:
		res.Stake = UnknownInt64(ev.Reason)
	case ActionNoAttemptInSuppliedPrefix:
		// A statement about the supplied prefix only: no financial zero.
		res.Stake = UnknownInt64(string(predictioneval.StatusNoAttemptInSuppliedPrefix))
	case ActionUnknownInput, ActionRefused:
		res.Stake = UnknownInt64(ev.Reason)
	default:
		res.Stake = UnknownInt64(StakeReasonUnsupportedShape)
	}
	res.witness = p3bResultWitness(res)
	return res, nil
}

// EvaluateP3bCase is the P3b plumbing for one factset under one set of
// entropy coordinates: project, generate exactly enough entropy for the
// candidate, evaluate. The coordinates carry the owner's dataset binding and
// the run index; their digest and opportunity must be this factset's.
//
// The trace supplies one word per rule per outcome — the most a traversal of
// one verified candidate can consume — so exhaustion cannot occur by
// construction and every word is reproducible from the coordinates alone.
func EvaluateP3bCase(fs CommonFactset, rs VerifiedP3bRuleset, coords EntropyCoordinates) (P3bCaseResult, error) {
	if err := rs.check(); err != nil {
		return P3bCaseResult{}, err
	}
	if err := checkEntropyCoordinates(coords); err != nil {
		return P3bCaseResult{}, err
	}
	proj, err := ProjectP3bSingleCandidate(fs)
	if errors.Is(err, ErrP3bProjectionRefused) {
		if berr := bindEntropyCoordinates(coords, fs); berr != nil {
			return P3bCaseResult{}, berr
		}
		refused := P3bCaseResult{
			Policy:             PolicyP3b,
			FactsetDigest:      fs.Digest,
			RulesetID:          rs.RulesetID,
			RulesetRawSHA256:   rs.RawSHA256,
			NativeConfigDigest: rs.NativeConfigDigest,
			ProjectionRefusal:  err.Error(),
			// No trace was drawn; the result still names the run it was
			// produced under, by its coordinates and by the identity of the
			// empty trace for those coordinates.
			Trace: EntropyTraceBinding{Coordinates: coords, RunID: entropyRunID(coords, 0)},
			Action: ActionMapping{
				MapVersion: NativeActionMapVersion, Policy: PolicyP3b,
				NativeAction: NativeActionP4ProjectionRefused, Class: ActionRefused, Legal: true,
			},
			Stake: UnknownInt64(NativeActionP4ProjectionRefused),
		}
		refused.witness = p3bResultWitness(refused)
		return refused, nil
	}
	if err != nil {
		return P3bCaseResult{}, err
	}
	if err := bindEntropyCoordinates(coords, fs); err != nil {
		return P3bCaseResult{}, err
	}
	count, err := traceWordCount(rs.Rules(), len(fs.Outcomes))
	if err != nil {
		return P3bCaseResult{}, err
	}
	trace, err := BuildDrawTrace(coords, count)
	if err != nil {
		return P3bCaseResult{}, err
	}
	return evaluateProjected(fs, proj, rs, coords, trace)
}

// Decision binds a P3b result to its case as a policy decision. The factset
// must be the one the result was evaluated over, and the result must be
// exactly what the evaluator produced (a binding contradiction is named
// first, an underived result second). The decision's derivation names the
// verified ruleset by id, raw-bytes hash and native digest, the entropy run
// identity and the core's entropy digest, so decisions of different
// rulesets or runs on one case are distinguishable.
func (r P3bCaseResult) Decision(fs CommonFactset) (PolicyDecision, error) {
	d, err := decisionOf(PolicyP3b, fs, r.FactsetDigest, r.Action, r.Choice, r.Stake,
		PolicyP3b+":ruleset="+strconv.Quote(r.RulesetID)+":raw="+r.RulesetRawSHA256+":native="+r.NativeConfigDigest+
			":run="+strconv.Quote(r.Trace.RunID)+":entropy="+r.Trace.EntropyDigest)
	if err != nil {
		return PolicyDecision{}, err
	}
	if !r.derived() {
		return PolicyDecision{}, ErrResultNotDerived
	}
	return d, nil
}

// carriesChoice reports whether a P3b class is one under which the core
// chose an outcome to bet on, so that the result carries its choice. A
// NO_ATTEMPT_IN_SUPPLIED_PREFIX is a determinate answer that bet on nothing.
func carriesChoice(c ActionClass) bool {
	return c == ActionWouldAttempt || c == ActionParticipationAdmittedStakeUnknown
}
