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
	// The column names the read selects, taken from the SELECT list itself
	// rather than restated: a restated list is one more thing that can drift.
	ident := regexp.MustCompile(`[a-z][a-z0-9_]*`)
	selected := map[string]bool{}
	for _, part := range strings.Split(observationSelectColumns, ",") {
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
	if len(selected) < 25 {
		t.Fatalf("parsed only %d columns out of the select list, so this check is not "+
			"actually inspecting it: %v", len(selected), selected)
	}

	var unmeasured []string
	for col := range selected {
		if !strings.Contains(observationRowWidthBytes, col) {
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
	var phantom []string
	for _, tok := range ident.FindAllString(observationRowWidthBytes, -1) {
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
