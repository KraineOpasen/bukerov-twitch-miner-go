package analytics

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestTheWidthExpressionCoversEveryVariableWidthColumnTheReadSelects is the
// anti-drift check behind the replay reader's byte budget.
//
// The budget is only a bound on memory if it measures what the read actually
// materializes. Sampling a few columns in a behavioural test cannot establish
// that: it passes while any unnamed column stays unmeasured, and an unmeasured
// TEXT column is exactly where an oversized value hides. So this compares the
// two constants STRUCTURALLY — every column observationSelectColumns reads must
// appear in observationRowWidthBytes, or be listed here as fixed-width with a
// reason.
//
// It also fails when a column is added to the SELECT and to neither list, which
// is the drift that matters: the schema constrains the length of none of these
// TEXT columns, so a new one is a new hiding place by default.
func TestTheWidthExpressionCoversEveryVariableWidthColumnTheReadSelects(t *testing.T) {
	// Columns whose width is bounded by their TYPE, not by any constraint:
	// integers and the REAL columns. Listing them explicitly is what makes the
	// absence of every other column an error rather than an oversight.
	fixedWidth := map[string]bool{
		"id":                                true,
		"collector_epoch":                   true,
		"collector_sequence":                true,
		"routed_streamer_id":                true,
		"round_owner_streamer_id":           true,
		"retention_group_owner_streamer_id": true,
		"producer_at_ms":                    true,
		"received_at_ms":                    true,
		"connection_index":                  true,
		"connection_generation":             true,
		"connection_sequence":               true,
		"payload_version":                   true,
	}

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
			if tok == "coalesce" || tok == "COALESCE" {
				continue
			}
			selected[tok] = true
			break
		}
	}
	if len(selected) < 20 {
		t.Fatalf("parsed only %d columns out of the select list, so this check is not "+
			"actually inspecting it: %v", len(selected), selected)
	}

	var unmeasured []string
	for col := range selected {
		if fixedWidth[col] {
			continue
		}
		if !strings.Contains(observationRowWidthBytes, col) {
			unmeasured = append(unmeasured, col)
		}
	}
	sort.Strings(unmeasured)
	if len(unmeasured) > 0 {
		t.Errorf("the read materializes %v, and the width expression does not measure them.\n"+
			"The schema constrains the length of none of these columns, so an unmeasured one "+
			"lets an oversized value pass a byte budget that reports a small number. Add each "+
			"to observationRowWidthBytes, or to fixedWidth here if its width is bounded by its "+
			"type.", unmeasured)
	}

	// And the reverse: the width expression must not measure a column the read
	// does not select, or the budget refuses loads on bytes nobody pays for.
	var phantom []string
	for _, tok := range ident.FindAllString(observationRowWidthBytes, -1) {
		switch tok {
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
