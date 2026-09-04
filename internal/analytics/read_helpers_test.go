package analytics

import (
	"testing"
	"time"
)

// The must* readers fail the test on a read error instead of letting a nil
// slice or a zero aggregate satisfy an "expected empty" assertion.

func mustPointSamples(t *testing.T, r Repository, streamer string, start, end time.Time, limit int) []PointSample {
	t.Helper()
	samples, err := r.GetPointSamples(streamer, start, end, limit)
	if err != nil {
		t.Fatalf("GetPointSamples(%s): %v", streamer, err)
	}
	return samples
}

func mustAnnotationRecords(t *testing.T, r Repository, streamer string, start, end time.Time) []AnnotationRecord {
	t.Helper()
	anns, err := r.GetAnnotationRecords(streamer, start, end)
	if err != nil {
		t.Fatalf("GetAnnotationRecords(%s): %v", streamer, err)
	}
	return anns
}

func mustExactEarnings(t *testing.T, r Repository, streamer string, start, end time.Time) ExactEarnings {
	t.Helper()
	exact, err := r.ExactEarningsBetween(streamer, start, end)
	if err != nil {
		t.Fatalf("ExactEarningsBetween(%s): %v", streamer, err)
	}
	return exact
}
