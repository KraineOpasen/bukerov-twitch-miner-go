//go:build !race

package analytics

// raceDetectorEnabled reports whether this test binary was built with -race.
// See the //go:build race counterpart for why the identity-purge pilot's
// wall-clock bound is only meaningful here.
const raceDetectorEnabled = false
