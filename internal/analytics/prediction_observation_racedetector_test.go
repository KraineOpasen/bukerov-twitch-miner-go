//go:build race

package analytics

// raceDetectorEnabled reports whether this test binary was built with -race.
//
// It exists for ONE reason: the identity-purge pilot asserts a wall-clock
// bound, and this repository's SQLite driver (modernc.org/sqlite) is pure Go,
// so the race detector instruments every load and store inside the database
// engine itself. That is roughly a 25x slowdown on the measured DELETE, which
// makes a production latency bound unmeasurable rather than merely slower.
// The pilot therefore still runs — and still proves the purge is correct,
// bounded in rows and complete — but asserts the 250ms budget only in a build
// where the measurement means something.
//
// Be clear about what that costs: this repository's CI runs `go test -race`,
// so CI does NOT gate the 250ms budget. The budget is verified by running the
// suite WITHOUT -race, which is a deliberate manual step, not an automated
// one. Treat a reported timing as evidence from that run, never as something
// a green CI has checked.
const raceDetectorEnabled = true
