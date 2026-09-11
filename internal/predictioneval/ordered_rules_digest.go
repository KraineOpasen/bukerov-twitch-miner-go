package predictioneval

import (
	"crypto/sha256"
	"hash"
	"math"
)

// THE ORDERED-RULES BINDINGS.
//
// Four SEPARATE, domain-separated digests, one per component of a run: the
// supplied stream, the raw config, the whole supplied entropy, and the prefix
// actually consumed. Keeping them apart is the point — a result that carried a
// single blended hash could be re-attached to a different config or a different
// entropy trace and still verify, which is precisely the recombination these
// exist to prevent. See
// TestOrderedRulesCandidatePolicyAndDrawBindingsCannotBeRecombined.
//
// None of these is [CommonInputDigestVersion]. That one witnesses a persisted
// attempt's row set through the store's own row witnesses; these witness values
// a caller supplied. Reusing it here would attach a claim about verified
// database rows to numbers that were passed in as arguments.
//
// The bound worth stating exactly: these are UNKEYED SHA-256 over supplied
// data. They detect accidental corruption and prevent recombination. They
// authenticate NOTHING — a caller who can choose the values can also compute
// the digest of the values they chose. A supplied number stays supplied.
//
// Note what the stream digest hashes: the supplied values THEMSELVES, in their
// own canonical form. A promised row witness would not do, because the values
// here need not have come from a store at all.

const (
	domainOrderedStream   = "pe-ors-stream/v1"
	domainOrderedConfig   = "pe-ors-config/v1"
	domainOrderedEntropy  = "pe-ors-entropy/v1"
	domainOrderedConsumed = "pe-ors-consumed/v1"
)

// hex64 renders a uint64 as fixed-width lower-case hex.
//
// Fixed width, and hex rather than decimal, so a 64-bit word survives every
// artifact it passes through intact. A raw word pushed through a
// double-precision JSON number loses its low bits silently, and a
// variable-width rendering lets two different words share a digest prefix.
func hex64(v uint64) string {
	const digits = "0123456789abcdef"
	var out [16]byte
	for i := 15; i >= 0; i-- {
		out[i] = digits[v&0x0f]
		v >>= 4
	}
	return string(out[:])
}

// digestFloat hashes a float64 by its BITS, not its rendering.
//
// Two floats one ulp apart can render identically at any fixed precision, and
// this model's whole numeric-fidelity argument rests on such pairs being
// distinguishable — the [9,1] pool share against a 90% threshold differs from
// the direct-share shortcut in exactly the last bit. A digest that could not
// tell them apart would let the falsified arithmetic pass as the faithful one.
func digestFloat(h hash.Hash, v float64) { digestPart(h, hex64(math.Float64bits(v))) }

func digestUint(h hash.Hash, v uint64) { digestPart(h, hex64(v)) }

func digestSupplied(h hash.Hash, v SuppliedInt64) {
	digestPart(h, string(v.Presence))
	// The value is hashed only when it is KNOWN. Hashing the zero that sits
	// behind a MISSING presence would make "absent" and "absent, with a
	// leftover zero" produce different digests for the same evidence.
	if v.Presence == SuppliedKnown {
		digestInt(h, v.Value)
		digestPart(h, v.Provenance)
		digestInt(h, v.AvailableAtPosition)
		// The DECLARATION as well as the position. Inside a projected stream
		// the flag is always set, since the projection refuses a KNOWN value
		// without it — which is exactly why leaving it unhashed looked like
		// decoration and was not. EvaluateOrderedRules takes the stream by
		// value, so the digest is what stands between it and a stream the
		// projection never produced; a flag the digest ignores can be stripped
		// from a projected stream, and the evaluator would then read a balance
		// whose availability was never declared while the digest still matched.
		digestBool(h, v.HasAvailableAtPosition)
	} else {
		// A non-KNOWN value's position and declaration are not hashed for the
		// same reason its value is not: there is no value whose timing could
		// matter, so a leftover zero must not make two absences differ.
		digestPart(h, v.Reason)
	}
}

func digestCandidate(h hash.Hash, c *OrderedRulesCandidate) {
	digestPart(h, c.Identity)
	digestInt(h, c.Position)
	digestBool(h, c.HasPosition)
	digestPart(h, string(c.SourceKind))
	digestPart(h, string(c.EpisodeMembership))
	digestPart(h, c.Provenance)
	digestPart(h, string(c.OutcomesPresence))
	digestPart(h, c.OutcomesReason)
	digestInt(h, int64(len(c.Outcomes)))
	for i := range c.Outcomes {
		digestPart(h, c.Outcomes[i].Identity)
		digestSupplied(h, c.Outcomes[i].Points)
	}
	digestSupplied(h, c.Balance)
}

// orderedRulesStreamDigest binds the projected selection.
//
// It covers the boundary as well as the candidates: two selections holding the
// same surviving candidates but established under different cutoffs are
// different evidence, because one of them is a prefix of a stream that was cut
// and the other is a complete one.
func orderedRulesStreamDigest(s OrderedRulesStream) string {
	h := sha256.New()
	digestPart(h, domainOrderedStream)
	digestPart(h, OrderedRulesModelVersion)
	digestPart(h, s.ContractVersion)

	digestPart(h, s.Scope.Namespace)
	digestPart(h, s.Scope.EpisodeID)
	digestPart(h, s.Scope.AccountContext)
	digestPart(h, s.Scope.AssociationEvidence)
	digestPart(h, s.Scope.SourceContractVersion)
	digestPart(h, string(s.Scope.Coverage))
	digestPart(h, s.Scope.CoverageDetail)
	digestInt(h, s.Scope.IntervalFromPosition)
	digestInt(h, s.Scope.IntervalToPosition)
	digestBool(h, s.Scope.HasInterval)

	digestPart(h, s.Admission.ManifestID)
	digestPart(h, string(s.Admission.ViewKind))
	digestPart(h, s.Admission.Population)
	digestPart(h, s.Admission.OrderBasis)
	digestInt(h, int64(len(s.Admission.SourceReferences)))
	for _, ref := range s.Admission.SourceReferences {
		digestPart(h, ref)
	}

	digestBool(h, s.Cutoff.Established)
	digestInt(h, s.Cutoff.Position)
	digestPart(h, string(s.Cutoff.Kind))
	digestPart(h, s.Cutoff.Identity)
	digestPart(h, string(s.Cutoff.Basis))
	digestInt(h, int64(s.Cutoff.DroppedAtOrAfter))

	digestInt(h, int64(len(s.Candidates)))
	for i := range s.Candidates {
		digestCandidate(h, &s.Candidates[i])
	}

	digestInt(h, int64(len(s.Qualifications)))
	for _, q := range s.Qualifications {
		digestPart(h, q)
	}
	return hexEncode(h.Sum(nil))
}

// orderedRulesConfigDigest binds the RAW config.
//
// Raw, deliberately: the exported contract accepts raw percentages and
// normalizes privately exactly once, so the digest witnesses what the caller
// actually supplied rather than a derived form two different raw configs could
// share.
func orderedRulesConfigDigest(c OrderedRulesConfig) string {
	h := sha256.New()
	digestPart(h, domainOrderedConfig)
	digestPart(h, OrderedRulesConfigBasisVersion)
	digestPart(h, c.ConfigID)
	digestInt(h, int64(len(c.Detailed)))
	for i := range c.Detailed {
		r := &c.Detailed[i]
		digestPart(h, string(r.Comparator))
		digestFloat(h, r.RawThresholdPercent)
		digestFloat(h, r.RawAttemptRatePercent)
		digestUint(h, uint64(r.Points.MaxValue))
		digestFloat(h, r.Points.RawPercent)
	}
	digestBool(h, c.HasDefault)
	digestFloat(h, c.Default.RawMinPercent)
	digestFloat(h, c.Default.RawMaxPercent)
	digestUint(h, uint64(c.Default.Points.MaxValue))
	digestFloat(h, c.Default.Points.RawPercent)
	return hexEncode(h.Sum(nil))
}

// orderedRulesEntropyDigest binds the WHOLE supplied trace.
//
// It is separate from the consumed-prefix digest on purpose. The two differing
// is exactly what shows that an unused suffix could not have influenced the
// evaluated prefix — a property that cannot be demonstrated if one hash covers
// both. See TestOrderedRulesAppendingPostCutoffFactsCannotChangeEvaluation.
func orderedRulesEntropyDigest(d SuppliedDrawTrace) string {
	h := sha256.New()
	digestPart(h, domainOrderedEntropy)
	digestPart(h, d.EntropySemanticsVersion)
	digestPart(h, d.RunID)
	digestInt(h, int64(len(d.Words)))
	for _, w := range d.Words {
		digestUint(h, w)
	}
	return hexEncode(h.Sum(nil))
}

// orderedRulesConsumedDigest binds ONLY what the traversal actually read.
//
// It covers the source's IDENTITY, the boundary that bounded the traversal, the
// config, the entropy run, and then the candidates and words actually consumed
// — and nothing else. What it deliberately EXCLUDES is metadata about the full
// supplied set: the surviving candidate list beyond the consumed prefix, the
// count of candidates the boundary dropped, and the unconsumed entropy suffix.
//
// That exclusion is the point. Evidence about the whole source and evidence
// about the consumed prefix are different things, and if this digest moved when
// a post-boundary fact was appended, then "later facts cannot change an earlier
// result" would be unprovable — the digest itself would be the counterexample.
//
// The declared interval's UPPER endpoint is excluded for that reason, and is
// the only mandatory declaration that is: appending a fact past the boundary
// can legitimately widen IntervalToPosition, so binding it would break the
// stability above for precisely the appends the stability is about. Its LOWER
// endpoint is not symmetric with it and is bound — see below. The scope's
// coverage detail and the admission's source references are optional
// elaborations rather than declarations, and stay out with the optional text.
// Compare [OrderedRulesStream.SelectionDigest] and
// [OrderedRulesEvaluation.EntropyDigest], which DO cover the whole of their
// subjects and are reported separately for exactly that contrast. See
// TestOrderedRulesAppendingPostCutoffFactsCannotChangeEvaluation.
func orderedRulesConsumedDigest(s OrderedRulesStream, cfg OrderedRulesConfig, d SuppliedDrawTrace,
	candidatesConsumed, wordsConsumed int) string {
	h := sha256.New()
	digestPart(h, domainOrderedConsumed)
	digestPart(h, OrderedRulesModelVersion)

	// WHICH source, not how much of it there was.
	digestPart(h, s.ContractVersion)
	digestPart(h, s.Scope.Namespace)
	digestPart(h, s.Scope.EpisodeID)
	digestPart(h, s.Scope.AccountContext)
	// The WARRANT as well as the claim it supports. AccountContext is the
	// caller's assertion that the candidates and the calls came from one
	// account; AssociationEvidence is what validateScope demands before that
	// assertion is admitted at all — "a pool or session id does not prove an
	// account". Binding the claim and not its warrant let two prefixes
	// established under different evidence, one solid and one barely
	// admissible, carry the SAME consumed-prefix binding, which is exactly the
	// recombination these four digests exist to prevent.
	digestPart(h, s.Scope.AssociationEvidence)
	digestPart(h, s.Scope.SourceContractVersion)
	// The declared coverage binds too. A prefix read from a source that admits
	// it is missing its beginning is different evidence from the same prefix
	// read from a source that claims to be complete — the second says no
	// earlier intervention occurred, the first cannot.
	digestPart(h, string(s.Scope.Coverage))
	// And how far back that coverage claim REACHES. The two interval endpoints
	// look like one value and are not: appending a fact past the boundary can
	// widen IntervalToPosition, which is why binding it would break the
	// stability above — but no append lowers IntervalFromPosition, so binding
	// it costs that stability nothing. It carries evidence the upper endpoint
	// does not. Two sources holding the same candidate at position 10 and no
	// intervention, one declaring complete coverage over [0,100] and the other
	// over [-100,100], read the same prefix; only the second also asserts that
	// nothing intervened in the hundred positions before it. That is a stronger
	// claim about the absence of an earlier intervention, which is the claim
	// this whole model is careful about, and it was invisible here.
	//
	// HasInterval is NOT hashed beside it, and the difference from the
	// availability declaration — where calling the flag decoration was wrong —
	// is worth stating rather than assuming a second time. That flag was
	// unbound by the WHOLE-STREAM digest, so a projected stream could be
	// stripped of it and still verify. This one is bound there already, and
	// validateScope refuses a stream that omits it, so no ADMITTED prefix can
	// differ by it; hashing it here would separate two refusals from each
	// other and nothing else.
	digestInt(h, s.Scope.IntervalFromPosition)
	digestPart(h, s.Admission.ManifestID)
	digestPart(h, string(s.Admission.ViewKind))
	// Population and order basis by the same rule as the warrant above: both
	// are mandatory — validateAdmission refuses an admission that names
	// neither — and both say what the prefix IS, not how much of it there was.
	// The same rows drawn from a different population, or ordered on a
	// different basis, are a different prefix wearing the same contents.
	digestPart(h, s.Admission.Population)
	digestPart(h, s.Admission.OrderBasis)

	// The boundary bounded what could be read, so it is an input to the result.
	// Its DroppedAtOrAfter count is not: that is a fact about candidates the
	// traversal never reached.
	digestBool(h, s.Cutoff.Established)
	digestInt(h, s.Cutoff.Position)
	digestPart(h, string(s.Cutoff.Kind))
	digestPart(h, s.Cutoff.Identity)
	digestPart(h, string(s.Cutoff.Basis))

	digestPart(h, orderedRulesConfigDigest(cfg))
	digestPart(h, d.RunID)
	digestPart(h, d.EntropySemanticsVersion)

	digestInt(h, int64(candidatesConsumed))
	for i := 0; i < candidatesConsumed && i < len(s.Candidates); i++ {
		digestCandidate(h, &s.Candidates[i])
	}
	digestInt(h, int64(wordsConsumed))
	for i := 0; i < wordsConsumed && i < len(d.Words); i++ {
		digestUint(h, d.Words[i])
	}
	return hexEncode(h.Sum(nil))
}
