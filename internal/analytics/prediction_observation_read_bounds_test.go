package analytics

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestTheWidthExpressionCoversEveryColumnTheReadSelects is the anti-drift check
// behind the replay reader's byte budget.
//
// The budget bounds memory only if it measures what the read materializes.
// Sampling a few columns behaviourally cannot establish that: it passes while
// any unnamed column stays unmeasured. So the two constants are compared
// STRUCTURALLY, and the comparison admits no exemptions.
//
// It used to allow a fixedWidth list — integer and REAL columns excused on the
// grounds that their declared type bounds them. It does not any more, because
// that reasoning was false: prediction_observations is not STRICT, so an
// INTEGER-affinity column holds arbitrarily large TEXT or BLOB, and a column
// excused from measurement is a place for an oversized value to hide. Removing
// the exemption is also what makes this test simple to keep honest — there is
// no judgement left in it.
func TestTheWidthExpressionCoversEveryColumnTheReadSelects(t *testing.T) {
	for _, pair := range []struct {
		what    string
		selects string
		width   string
		minCols int
		// subsetOnly relaxes the reverse direction for a read whose columns
		// are a proper subset of the ones the width expression measures.
		subsetOnly bool
	}{
		{what: "observation rows", selects: observationSelectColumns,
			width: observationRowWidthBytes, minCols: 25},
		// The session row needs the same treatment, and did not get it first
		// time: its width expression measured three columns while
		// ReadObservationSession scanned twelve. That row is read BEFORE any
		// observation-row bound applies, so it was the earliest unbounded
		// allocation in the load.
		{what: "the session row", selects: observationSessionSelectColumns,
			width: observationSessionRowWidthBytes, minCols: 12},
		// And the witness sweep, which is the read that hid behind both. It
		// runs INSIDE ReadObservationSession and materializes a row at a time
		// to recompute its digest, so the replay reader bounds it with the
		// fact-read's width expression before it learns the session id. That
		// only holds while this list stays a SUBSET of the columns that
		// expression measures — hence one direction here, not two: the width
		// expression legitimately measures four columns this sweep does not
		// select.
		{what: "the witness sweep", selects: observationWitnessSelectColumns,
			width: observationRowWidthBytes, minCols: 25, subsetOnly: true},
	} {
		t.Run(pair.what, func(t *testing.T) {
			assertWidthCoversSelect(t, pair.selects, pair.width, pair.minCols, pair.subsetOnly)
		})
	}
}

// assertWidthCoversSelect compares a SELECT list against the width expression
// meant to measure it, in both directions.
func assertWidthCoversSelect(t *testing.T, selectList, widthExpr string, minCols int, subsetOnly bool) {
	t.Helper()
	// The column names the read selects, taken from the SELECT list itself
	// rather than restated: a restated list is one more thing that can drift.
	ident := regexp.MustCompile(`[a-z][a-z0-9_]*`)
	selected := map[string]bool{}
	for _, part := range strings.Split(selectList, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// Each entry is a bare column or COALESCE(column, default); the first
		// identifier that is not a SQL keyword is the column.
		for _, tok := range ident.FindAllString(part, -1) {
			if strings.EqualFold(tok, "coalesce") {
				continue
			}
			selected[tok] = true
			break
		}
	}
	if len(selected) < minCols {
		t.Fatalf("parsed only %d columns out of the select list (want at least %d), so this "+
			"check is not actually inspecting it: %v", len(selected), minCols, selected)
	}

	var unmeasured []string
	for col := range selected {
		if !strings.Contains(widthExpr, col) {
			unmeasured = append(unmeasured, col)
		}
	}
	sort.Strings(unmeasured)
	if len(unmeasured) > 0 {
		t.Errorf("the read materializes %v, and the width expression does not measure them.\n"+
			"This table is not STRICT, so a column's declared type bounds nothing: an "+
			"INTEGER-affinity column holds arbitrarily large TEXT or BLOB. Every selected "+
			"column must be measured — there is no type that earns an exemption.", unmeasured)
	}

	// And the reverse: the width expression must not measure a column the read
	// does not select, or the budget refuses loads on bytes nobody pays for.
	// A subset read is exempt from this half by construction — it is bounded
	// BY a wider read's expression, so measuring more than it selects is the
	// conservative direction and the only one available.
	if subsetOnly {
		return
	}
	var phantom []string
	for _, tok := range ident.FindAllString(widthExpr, -1) {
		switch strings.ToLower(tok) {
		case "length", "cast", "as", "blob", "coalesce":
			continue
		}
		if !selected[tok] {
			phantom = append(phantom, tok)
		}
	}
	sort.Strings(phantom)
	if len(phantom) > 0 {
		t.Errorf("the width expression measures %v, which the read does not select. "+
			"A budget that charges for bytes nobody materializes refuses loads it should "+
			"allow.", phantom)
	}
}
