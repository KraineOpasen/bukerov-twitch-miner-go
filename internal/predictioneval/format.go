package predictioneval

import "strconv"

// Rendering helpers for the comparison fields.
//
// Values are rendered as text rather than compared as text: the comparison
// itself is always done on the typed value, and these produce the human- and
// machine-readable echo that goes into a [Comparison]. Rendering never
// normalizes — a float is emitted with full precision so a scorecard cannot
// make two different numbers look identical.

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// ftoa renders a float64 with -1 precision: the shortest representation that
// parses back to exactly the same value. A fixed precision would round two
// distinguishable inputs into one printed string, which is the one thing a
// disagreement report must not do.
func ftoa(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }
