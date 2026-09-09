package analytics

// The envelope's closed vocabularies mirror constants that live in
// internal/models. Nothing structural keeps the two in step, so this file does.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestTheEnvelopeVocabulariesCoverEveryDomainConstant regresses silent
// vocabulary drift.
//
// The strategy, delay-mode, filter-key, comparison and gate-reason lists in
// prediction_decision_envelope.go are hand-copied from internal/models. Adding
// a fourteenth strategy there is routine, and nothing would have failed: every
// envelope for a round using it would store Strategy = "UNKNOWN", the record
// would silently stop being sufficient to re-derive the decision — the exact
// defect this trail exists to prevent — and no test would notice, because the
// two lists were never compared and a re-derivation just reports "unsupported".
//
// Reflection cannot enumerate Go constants, so this reads the domain source.
// That is the same technique this package already uses where an invariant
// cannot be observed at runtime, and it is what makes a NEW constant fail here
// rather than in production data months later.
func TestTheEnvelopeVocabulariesCoverEveryDomainConstant(t *testing.T) {
	// Every file of the domain package, not just bet.go: a constant of one of
	// these types added elsewhere in internal/models would otherwise be
	// invisible to this guard, which is exactly the drift it exists to catch.
	betSrc := readDomainPackage(t)

	for _, tc := range []struct {
		name    string
		goType  string
		allowed []string
		// exempt lists literals that are deliberately not vocabulary members.
		exempt map[string]string
	}{
		{
			name: "betting strategies", goType: "Strategy",
			allowed: observationBetStrategies,
		},
		{
			name: "delay modes", goType: "DelayMode",
			allowed: observationDelayModes,
		},
		{
			name: "filter outcome keys", goType: "OutcomeKey",
			allowed: observationFilterKeys,
		},
		{
			name: "filter comparisons", goType: "Condition",
			allowed: observationFilterComparisons,
		},
		{
			name: "stake and health gate reasons", goType: "GateReason",
			allowed: observationStakeGateReasons,
			// GateNone is the empty string: "no gate bound this stake". It is
			// carried by closedOptional, which preserves "" as itself, so it is
			// deliberately not a member of the closed list.
			exempt: map[string]string{"": "models.GateNone is the empty string, preserved by closedOptional"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			literals := domainConstLiterals(t, betSrc, tc.goType)
			if len(literals) == 0 {
				t.Fatalf("found no %s constants in internal/models/bet.go; the scan pattern has "+
					"drifted and this guard is no longer checking anything", tc.goType)
			}
			for _, want := range literals {
				if reason, ok := tc.exempt[want]; ok {
					t.Logf("exempt %q: %s", want, reason)
					continue
				}
				if !containsValue(tc.allowed, want) {
					t.Fatalf("internal/models declares %s %q, which the envelope's closed "+
						"vocabulary does not accept. Every decision using it would be stored "+
						"as %q, and the record would silently stop being sufficient to "+
						"re-derive the decision it claims to explain", tc.goType, want, ValueUnknown)
				}
			}
		})
	}
}

// readDomainPackage concatenates every non-test Go file of internal/models as
// source text.
func readDomainPackage(t *testing.T) string {
	t.Helper()
	dir := filepath.Join("..", "models")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read internal/models: %v", err)
	}
	var b strings.Builder
	var read int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		content, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read internal/models/%s: %v", name, err)
		}
		b.Write(content)
		b.WriteString("\n")
		read++
	}
	if read == 0 {
		t.Fatal("read no source from internal/models; this guard is checking nothing")
	}
	return b.String()
}

// domainConstLiterals extracts every string literal assigned to a constant of
// the given named type, e.g. `StrategySmart Strategy = "SMART"`.
func domainConstLiterals(t *testing.T, src, goType string) []string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^\s*\w+\s+` + regexp.QuoteMeta(goType) + `\s*=\s*"([^"]*)"`)
	var out []string
	for _, m := range re.FindAllStringSubmatch(src, -1) {
		out = append(out, m[1])
	}
	return out
}

func containsValue(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
