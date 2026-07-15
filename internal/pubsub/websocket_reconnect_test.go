package pubsub

import (
	"testing"
	"time"
)

// keys renders a topic slice to its wire strings for order-sensitive comparison.
func keys(topics []Topic) []string {
	out := make([]string, len(topics))
	for i, t := range topics {
		out[i] = t.String()
	}
	return out
}

func TestMergeTopics(t *testing.T) {
	a1 := NewTopic(TopicVideoPlaybackByID, "1")
	a2 := NewTopic(TopicRaid, "1")
	b1 := NewTopic(TopicPredictionsChannel, "2")

	tests := []struct {
		name string
		a, b []Topic
		want []string
	}{
		{
			name: "empty snapshot keeps parked pending topics",
			// This is the core b.2 invariant: a failed Connect() leaves topics=nil
			// with the real set stranded in pendingTopics; the retry must not lose
			// it. mergeTopics(nil, pending) must return the full pending set.
			a:    nil,
			b:    []Topic{a1, a2, b1},
			want: []string{a1.String(), a2.String(), b1.String()},
		},
		{
			name: "union dedups overlap, a-order first then new b",
			a:    []Topic{a1, a2},
			b:    []Topic{a2, b1},
			want: []string{a1.String(), a2.String(), b1.String()},
		},
		{
			name: "both empty yields empty",
			a:    nil,
			b:    nil,
			want: []string{},
		},
		{
			name: "identical sets collapse to one copy each",
			a:    []Topic{a1, b1},
			b:    []Topic{a1, b1},
			want: []string{a1.String(), b1.String()},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := keys(mergeTopics(tc.a, tc.b))
			if len(got) != len(tc.want) {
				t.Fatalf("mergeTopics() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("mergeTopics()[%d] = %q, want %q (full: %v)", i, got[i], tc.want[i], got)
				}
			}
		})
	}
}

// TestConnSnapshot verifies the per-index view the health signal and debug
// snapshot consume: one ConnState per connection, carrying its topic count and
// lifecycle flags, so a single stuck connection is visible individually.
func TestConnSnapshot(t *testing.T) {
	now := time.Now()

	healthy := &WebSocketClient{index: 0, lastPong: now}
	healthy.topics = []Topic{
		NewTopic(TopicVideoPlaybackByID, "1"),
		NewTopic(TopicRaid, "1"),
	}

	zombie := &WebSocketClient{index: 1, lastPong: now} // reconnected but 0 topics

	reconnecting := &WebSocketClient{index: 2, isReconnecting: true, isClosed: true}

	pool := &WebSocketPool{clients: []*WebSocketClient{healthy, zombie, reconnecting}}

	snap := pool.ConnSnapshot()
	if len(snap) != 3 {
		t.Fatalf("ConnSnapshot() len = %d, want 3", len(snap))
	}

	if snap[0].Index != 0 || snap[0].Topics != 2 || snap[0].Reconnecting || snap[0].Closed {
		t.Errorf("index 0 = %+v, want {Index:0 Topics:2 flags:false}", snap[0])
	}
	if snap[1].Index != 1 || snap[1].Topics != 0 {
		t.Errorf("index 1 = %+v, want {Index:1 Topics:0}", snap[1])
	}
	if snap[2].Index != 2 || !snap[2].Reconnecting || !snap[2].Closed {
		t.Errorf("index 2 = %+v, want reconnecting+closed", snap[2])
	}
}
