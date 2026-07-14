package discovery

import "testing"

// TestOrderGamesByPolicy verifies the campaign-policy cross-game ordering:
// published ranks reorder the configured game list (lower rank first),
// unranked games keep their relative order at the end, and no published ranks
// leaves the configured order bit-identical.
func TestOrderGamesByPolicy(t *testing.T) {
	m := &Manager{}
	games := []string{"Alpha", "Bravo", "Charlie", "Delta"}

	// No ranks published → identical order.
	if got := m.orderGamesByPolicy(games); !equal(got, games) {
		t.Fatalf("no ranks: expected unchanged order, got %v", got)
	}

	// Ranks favor Charlie, then Alpha; Bravo/Delta unranked keep their order.
	m.SetGameRanks(map[string]int{"charlie": 0, "alpha": 1})
	got := m.orderGamesByPolicy(games)
	want := []string{"Charlie", "Alpha", "Bravo", "Delta"}
	if !equal(got, want) {
		t.Fatalf("ranked order = %v, want %v", got, want)
	}

	// Clearing ranks restores the configured order.
	m.SetGameRanks(nil)
	if got := m.orderGamesByPolicy(games); !equal(got, games) {
		t.Fatalf("after clearing ranks, expected configured order, got %v", got)
	}
}

func equal(a, b []string) bool {
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
