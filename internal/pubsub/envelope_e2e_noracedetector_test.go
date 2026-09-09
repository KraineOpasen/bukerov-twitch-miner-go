//go:build !race

package pubsub_test

// raceDetectorEnabled reports whether this test binary was built with -race.
// See the //go:build race counterpart for why the fully integrated end-to-end
// capture proof can only commit facts here.
const raceDetectorEnabled = false
