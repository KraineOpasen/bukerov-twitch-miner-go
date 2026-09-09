//go:build race

package pubsub_test

// raceDetectorEnabled reports whether this test binary was built with -race.
//
// It exists for ONE reason, and it is the same reason the analytics package
// carries this pair of files. The observation collector enforces a HARD 5 ms
// budget per fact (analytics.ObservationWriteDeadline) and never retries: a
// commit that misses it is a permanent, counted drop. This repository's SQLite
// driver (modernc.org/sqlite) is pure Go, so -race instruments every load and
// store inside the database engine, and a single insert measures 5.6-9.1 ms
// against that 5 ms budget — every fact is dropped, deterministically, before
// any assertion can be made. Without -race the same insert takes ~1.2 ms and
// commits.
//
// The analytics package solves this for its own tests by relaxing the
// collector's writeDeadline, which is an unexported field: unreachable from
// this external test package, and there is deliberately no exported knob for
// it.
//
// Be clear about what that costs. This repository's CI runs `go test -race`,
// so CI does NOT execute the fully integrated four-leg end-to-end proof. Under
// -race the legs are still covered, each in a build that can measure it:
// the producer by the in-package envelope tests, the miner adapter
// exhaustively by TestObservationAdapterIsFieldComplete's reflection over
// every field, and the collector/store/reader by the analytics package's own
// tests. The integrated run is verified WITHOUT -race, which is a deliberate
// manual step. Treat a green -race CI as evidence of the legs, never of the
// integrated chain.
const raceDetectorEnabled = true
