package miner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/auth"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/pubsub"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/streamer"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/twitch"
)

var milestoneTestSequence atomic.Uint64

// milestoneRoundTripper records the ORDER of every GQL operation the miner
// issues, plus the RewardList channelID variables, so a test can assert both
// the per-cycle request bound and the business-pass-first ordering without any
// sleep-based synchronization.
type milestoneRoundTripper struct {
	mu           sync.Mutex
	ops          []string
	rewardBy     map[string]int
	contextBy    map[string]int
	contextOrder []string

	// onRewardList runs (outside the lock) after each RewardList request is
	// recorded. It is the deterministic seam a cancellation test uses to stop
	// the loop exactly mid-roster.
	onRewardList func(n int)

	// rewardListBody, when set, replaces the default RewardList response body.
	rewardListBody string

	// contextBody, when set, replaces the default ChannelPointsContext response
	// body — the BUSINESS read, used to prove the diagnostic-only suppressions
	// do not leak onto it.
	contextBody string
}

func (rt *milestoneRoundTripper) counts() (ops []string, reward map[string]int) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	ops = append([]string(nil), rt.ops...)
	reward = make(map[string]int, len(rt.rewardBy))
	for k, v := range rt.rewardBy {
		reward[k] = v
	}
	return ops, reward
}

// businessTargets returns the logins the business pass read, in wire order.
func (rt *milestoneRoundTripper) businessTargets() []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return append([]string(nil), rt.contextOrder...)
}

const defaultRewardListBody = `{"data":{"channel":{"id":"12345","self":{"watchStreakMilestone":{` +
	`"watchStreakThreshold":3,"watchStreakCopoBonus":450,"state":"ACTIVE","expiresAt":"2026-09-10T00:00:00Z",` +
	`"missedStreams":[{"broadcastIdentifiers":[{"id":"missed-b-1"}]}],` +
	`"watchStreakMilestone":{"id":"m-1","value":4,"achievementTimestamp":"2026-09-05T12:00:00Z","shareStatus":"UNSHARED"}}}}}}`

func (rt *milestoneRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	var operation struct {
		Name      string                 `json:"operationName"`
		Variables map[string]interface{} `json:"variables"`
	}
	if err := json.Unmarshal(body, &operation); err != nil {
		return nil, err
	}

	rt.mu.Lock()
	rt.ops = append(rt.ops, operation.Name)
	if operation.Name == "ChannelPointsContext" {
		if login, ok := operation.Variables["channelLogin"].(string); ok {
			if rt.contextBy == nil {
				rt.contextBy = map[string]int{}
			}
			rt.contextBy[login]++
			rt.contextOrder = append(rt.contextOrder, login)
		}
	}
	rewardCount := 0
	if operation.Name == "RewardList" {
		if rt.rewardBy == nil {
			rt.rewardBy = map[string]int{}
		}
		channelID, _ := operation.Variables["channelID"].(string)
		rt.rewardBy[channelID]++
		for _, n := range rt.rewardBy {
			rewardCount += n
		}
	}
	hook := rt.onRewardList
	rewardBody := rt.rewardListBody
	contextBody := rt.contextBody
	rt.mu.Unlock()

	response := `{"data":{}}`
	switch operation.Name {
	case "ChannelPointsContext":
		response = `{"data":{"community":{"channel":{"self":{"communityPoints":{"balance":777,"availableClaim":null}}}}}}`
		if contextBody != "" {
			response = contextBody
		}
	case "RewardList":
		response = defaultRewardListBody
		if rewardBody != "" {
			response = rewardBody
		}
		if hook != nil {
			hook(rewardCount)
		}
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(response)),
		Request:    req,
	}, nil
}

// newMilestoneMiner builds a Miner with a real TwitchClient over rt and one
// tracked streamer per login. Only the logins listed in online are confirmed
// online.
func newMilestoneMiner(t *testing.T, rt *milestoneRoundTripper, logins []string, online []string) (*Miner, map[string]*models.Streamer) {
	t.Helper()
	previousTransport := http.DefaultTransport
	http.DefaultTransport = rt
	t.Cleanup(func() { http.DefaultTransport = previousTransport })

	twitchAuth := auth.NewTwitchAuth("tester", "device-id")
	twitchAuth.ReplaceCredentials(auth.TokenResponse{AccessToken: "milestone-secret-token"})
	twitchAuth.SetUserID("100")
	client := twitch.NewTwitchClient(twitchAuth, "device-id")

	cfg := &config.Config{
		Username:         "tester",
		StreamerSettings: models.DefaultStreamerSettings(),
	}
	for _, login := range logins {
		cfg.Streamers = append(cfg.Streamers, config.StreamerConfig{Username: login})
	}
	manager := streamer.NewManager(fakeStreamerAPI{}, cfg.StreamerSettings)
	if err := manager.LoadFromConfig(cfg.Streamers, nil); err != nil {
		t.Fatalf("load streamers: %v", err)
	}

	streamers := make(map[string]*models.Streamer, len(logins))
	for _, login := range logins {
		s := manager.Get(login)
		if s == nil {
			t.Fatalf("streamer %q not tracked", login)
		}
		streamers[login] = s
	}
	for _, login := range online {
		streamers[login].SetConfirmedOnline()
		streamers[login].SetChannelPointsCapability(models.CapabilityEnabled, models.CapReasonConfirmedContext)
	}

	m := &Miner{
		config:             cfg,
		client:             client,
		streamers:          manager,
		streamCheckTrigger: make(chan struct{}, 1),
		autoRedeemState:    make(map[string]*autoRedeemRuntime),
	}
	return m, streamers
}

// captureLogs redirects slog to a buffer for the duration of the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	previous := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

// recordLines returns the log lines belonging to one diagnostic record kind.
func recordLines(logs string, record string) []string {
	var out []string
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "record="+record) {
			out = append(out, line)
		}
	}
	return out
}

// attrValue extracts a slog text-handler attribute value from one line.
func attrValue(line, key string) string {
	idx := strings.Index(line, " "+key+"=")
	if idx < 0 {
		return ""
	}
	rest := line[idx+len(key)+2:]
	if strings.HasPrefix(rest, `"`) {
		end := strings.Index(rest[1:], `"`)
		if end < 0 {
			return rest
		}
		return rest[1 : end+1]
	}
	if end := strings.Index(rest, " "); end >= 0 {
		return rest[:end]
	}
	return rest
}

func milestoneLogins(t *testing.T, n int) []string {
	t.Helper()
	seq := milestoneTestSequence.Add(1)
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("milestone-%d-%d", seq, i))
	}
	return out
}

// TestObservationRunsOncePerOnlineTargetAndSkipsIneligible proves the per-cycle
// request bound: exactly one RewardList per ONLINE target with a channel
// identity, and none at all for an offline one.
func TestObservationRunsOncePerOnlineTargetAndSkipsIneligible(t *testing.T) {
	captureLogs(t)
	logins := milestoneLogins(t, 3)
	rt := &milestoneRoundTripper{}
	m, streamers := newMilestoneMiner(t, rt, logins, logins[:2])

	m.observeWatchStreakMilestones(context.Background())

	_, reward := rt.counts()
	for _, login := range logins[:2] {
		if got := reward[streamers[login].ChannelID]; got != 1 {
			t.Errorf("online %s: RewardList requests = %d, want exactly 1", login, got)
		}
	}
	if got := reward[streamers[logins[2]].ChannelID]; got != 0 {
		t.Errorf("offline %s: RewardList requests = %d, want 0", logins[2], got)
	}
	if len(reward) != 2 {
		t.Errorf("distinct RewardList targets = %d, want 2 (%v)", len(reward), reward)
	}
}

// TestObservationRunsAfterTheBusinessPassOfTheSameCycle is the composition
// proof: on the EXISTING bonus cycle, the whole business pass (bonus claiming
// and auto-redeem for every online streamer) completes before the first
// observation request, and no observation request is interleaved into it.
func TestObservationRunsAfterTheBusinessPassOfTheSameCycle(t *testing.T) {
	captureLogs(t)
	logins := milestoneLogins(t, 2)
	rt := &milestoneRoundTripper{}
	m, _ := newMilestoneMiner(t, rt, logins, logins)

	// Give both streamers a live auto-redeem config so the business pass really
	// issues its own follow-up operations, not just the context read.
	m.config.AutoRedeem = map[string]config.AutoRedeemConfig{}
	for _, login := range logins {
		m.config.AutoRedeem[login] = config.AutoRedeemConfig{
			Enabled: true, Budget: 1000, RewardIDs: []string{"reward-x"},
		}
	}

	previousInterval := bonusPollInterval
	bonusPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { bonusPollInterval = previousInterval })

	seenTwo := make(chan struct{})
	var once sync.Once
	rt.mu.Lock()
	rt.onRewardList = func(n int) {
		if n >= 2 {
			once.Do(func() { close(seenTwo) })
		}
	}
	rt.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.bonusPollLoop(ctx)
	}()
	<-seenTwo
	cancel()
	<-done

	ops, reward := rt.counts()

	firstReward := -1
	for i, op := range ops {
		if op == "RewardList" {
			firstReward = i
			break
		}
	}
	if firstReward < 0 {
		t.Fatalf("no RewardList request was issued; ops=%v", ops)
	}
	// Everything before the first observation must be a business-pass read, and
	// the business pass must have covered EVERY online streamer — that is the
	// property ("the cycle's business pass completed first"), not any particular
	// number of calls per streamer, which is the bonus/auto-redeem path's own
	// business and may change without weakening this guarantee.
	prefix := ops[:firstReward]
	for i, op := range prefix {
		if op != "ChannelPointsContext" {
			t.Errorf("prefix op %d = %q, want a business-pass read; ops=%v", i, op, ops)
		}
	}
	covered := map[string]bool{}
	for i, login := range rt.businessTargets() {
		if i >= len(prefix) {
			break
		}
		covered[login] = true
	}
	for _, login := range logins {
		if !covered[login] {
			t.Errorf("the observation ran before the business pass reached %s; ops=%v", login, ops)
		}
	}
	// The observation block that follows is contiguous: no business operation
	// is pushed behind an observation inside the same cycle.
	for i := firstReward; i < firstReward+len(logins) && i < len(ops); i++ {
		if ops[i] != "RewardList" {
			t.Errorf("observation block op %d = %q, want RewardList; ops=%v", i, ops[i], ops)
		}
	}

	// Per-target bound across every cycle that ran: an observation can never
	// outnumber the business pass that precedes it.
	contexts, totalRewards := 0, 0
	for _, op := range ops {
		if op == "ChannelPointsContext" {
			contexts++
		}
	}
	for _, n := range reward {
		totalRewards += n
	}
	if totalRewards > contexts {
		t.Errorf("observation requests (%d) outnumbered business-pass reads (%d)", totalRewards, contexts)
	}
	if len(reward) != len(logins) {
		t.Errorf("distinct observation targets = %d, want %d (%v)", len(reward), len(logins), reward)
	}
}

// TestObservationHonoursAlreadyCancelledContext proves a cancelled owner issues
// no observation request at all.
func TestObservationHonoursAlreadyCancelledContext(t *testing.T) {
	logs := captureLogs(t)
	logins := milestoneLogins(t, 3)
	rt := &milestoneRoundTripper{}
	m, _ := newMilestoneMiner(t, rt, logins, logins)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.observeWatchStreakMilestones(ctx)

	if _, reward := rt.counts(); len(reward) != 0 {
		t.Fatalf("cancelled observation stage still issued requests: %v", reward)
	}
	// The stage RETURNS on cancellation; it does not walk the rest of the
	// roster emitting skipped records.
	if lines := recordLines(logs.String(), milestoneObservationRecord); len(lines) != 0 {
		t.Fatalf("cancelled stage emitted %d records; it must return instead of walking the roster", len(lines))
	}
}

// TestObservationStopsMidRosterOnCancellation proves cancellation releases the
// stage deterministically: the roster is abandoned at the cancellation point
// rather than run to completion.
func TestObservationStopsMidRosterOnCancellation(t *testing.T) {
	logs := captureLogs(t)
	logins := milestoneLogins(t, 4)
	rt := &milestoneRoundTripper{}
	m, _ := newMilestoneMiner(t, rt, logins, logins)

	ctx, cancel := context.WithCancel(context.Background())
	rt.mu.Lock()
	rt.onRewardList = func(n int) {
		if n == 1 {
			cancel() // cancel while the first observation is still in flight
		}
	}
	rt.mu.Unlock()

	m.observeWatchStreakMilestones(ctx)

	_, reward := rt.counts()
	total := 0
	for _, n := range reward {
		total += n
	}
	if total != 1 {
		t.Fatalf("observation requests after mid-roster cancellation = %d, want exactly 1 (%v)", total, reward)
	}
	// Releasing the loop means RETURNING, not walking the remaining roster to
	// emit three more skipped records.
	lines := recordLines(logs.String(), milestoneObservationRecord)
	if len(lines) != 1 {
		t.Fatalf("observation records after mid-roster cancellation = %d, want exactly 1; "+
			"the loop must abandon the rest of the roster", len(lines))
	}
}

// TestBonusPollLoopReturnsOnCancellation proves the composed stage did not
// change the loop's shutdown ownership: the loop still returns on cancellation.
func TestBonusPollLoopReturnsOnCancellation(t *testing.T) {
	captureLogs(t)
	logins := milestoneLogins(t, 1)
	rt := &milestoneRoundTripper{}
	m, _ := newMilestoneMiner(t, rt, logins, logins)

	previousInterval := bonusPollInterval
	bonusPollInterval = time.Hour // never ticks; only cancellation can end it
	t.Cleanup(func() { bonusPollInterval = previousInterval })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.bonusPollLoop(ctx)
	}()
	cancel()
	<-done

	if _, reward := rt.counts(); len(reward) != 0 {
		t.Fatalf("cancelled loop still observed: %v", reward)
	}
}

// TestObservationRecordCarriesBoundedProvenance proves the emitted record
// carries the required allowlisted provenance — and that achievementTimestamp
// is kept SEPARATE from the request window.
func TestObservationRecordCarriesBoundedProvenance(t *testing.T) {
	logs := captureLogs(t)
	logins := milestoneLogins(t, 1)
	rt := &milestoneRoundTripper{}
	m, streamers := newMilestoneMiner(t, rt, logins, logins)
	streamers[logins[0]].Stream.Update("broadcast-local-1", "title", nil, nil, 1)

	m.observeWatchStreakMilestones(context.Background())

	lines := recordLines(logs.String(), milestoneObservationRecord)
	if len(lines) != 1 {
		t.Fatalf("observation records = %d, want 1; logs:\n%s", len(lines), logs.String())
	}
	line := lines[0]

	for key, want := range map[string]string{
		"outcome":                      "OBSERVED",
		"failureClass":                 "",
		"streamer":                     logins[0],
		"channelId":                    streamers[logins[0]].ChannelID,
		"selfMilestonePresence":        "VALID",
		"milestoneNodePresence":        "VALID",
		"milestoneId":                  "m-1",
		"milestoneValue":               "4",
		"watchStreakThreshold":         "3",
		"watchStreakCopoBonus":         "450",
		"state":                        "ACTIVE",
		"shareStatus":                  "UNSHARED",
		"achievementTimestamp":         "2026-09-05T12:00:00Z",
		"missedStreamsPresence":        "VALID",
		"missedStreamsCount":           "1",
		"missedStreamsMalformed":       "0",
		"broadcastIdentifierCount":     "1",
		"broadcastIdentifierMalformed": "0",
		"broadcastIdentifierArrays":    "VALID:1",
		"broadcastIdentifierIds":       "VALID:1",
		"localBroadcastContextOnly":    "broadcast-local-1",
		"grantLink":                    "UNKNOWN",
		"expectedMilestone":            "UNKNOWN",
	} {
		if got := attrValue(line, key); got != want {
			t.Errorf("%s = %q, want %q\nline: %s", key, got, want, line)
		}
	}

	// The achievement timestamp is the achievement's own field; the sampling
	// window is a different, later pair of local times.
	achievement, err := time.Parse(time.RFC3339, attrValue(line, "achievementTimestamp"))
	if err != nil {
		t.Fatalf("achievementTimestamp not parseable: %v", err)
	}
	start, err := time.Parse(time.RFC3339Nano, attrValue(line, "requestStart"))
	if err != nil {
		t.Fatalf("requestStart not parseable: %v", err)
	}
	end, err := time.Parse(time.RFC3339Nano, attrValue(line, "requestEnd"))
	if err != nil {
		t.Fatalf("requestEnd not parseable: %v", err)
	}
	if end.Before(start) {
		t.Errorf("request window is reversed: %v -> %v", start, end)
	}
	if !achievement.Before(start) {
		t.Errorf("achievementTimestamp %v was treated as the sampling time %v", achievement, start)
	}
}

// TestObservationRecordNeverFabricatesAbsentFields proves an S-less response
// prints presence classifications rather than zeros/empties that would read as
// observed facts.
func TestObservationRecordNeverFabricatesAbsentFields(t *testing.T) {
	logs := captureLogs(t)
	logins := milestoneLogins(t, 1)
	rt := &milestoneRoundTripper{
		rewardListBody: `{"data":{"channel":{"id":"12345","self":{"watchStreakMilestone":null}}}}`,
	}
	m, _ := newMilestoneMiner(t, rt, logins, logins)

	m.observeWatchStreakMilestones(context.Background())

	lines := recordLines(logs.String(), milestoneObservationRecord)
	if len(lines) != 1 {
		t.Fatalf("observation records = %d, want 1", len(lines))
	}
	line := lines[0]
	for key, want := range map[string]string{
		"selfMilestonePresence":     "NULL",
		"milestoneNodePresence":     "MISSING",
		"milestoneValue":            "<MISSING>",
		"state":                     "<MISSING>",
		"achievementTimestamp":      "<MISSING>",
		"shareStatus":               "<MISSING>",
		"expiresAt":                 "<MISSING>",
		"watchStreakThreshold":      "<MISSING>",
		"missedStreamsPresence":     "MISSING",
		"broadcastIdentifierArrays": "none",
		"broadcastIdentifierIds":    "none",
	} {
		if got := attrValue(line, key); got != want {
			t.Errorf("%s = %q, want %q\nline: %s", key, got, want, line)
		}
	}
	if strings.Contains(line, "milestoneValue=0") {
		t.Errorf("absent milestone value was fabricated as 0: %s", line)
	}
	if strings.Contains(line, `state=""`) {
		t.Errorf("absent state was fabricated as an observed empty string: %s", line)
	}
}

// TestObservationRecordDistinguishesNullMalformedAndEmpty proves the record
// itself — the only persistence this feature has — keeps MISSING, NULL, EMPTY,
// VALID and MALFORMED apart, including for the nested broadcastIdentifiers
// arrays where a single aggregate count would collapse them into "0/0".
func TestObservationRecordDistinguishesNullMalformedAndEmpty(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		want  map[string]string
		notEq map[string]string
	}{
		{
			name: "nested array malformed",
			body: `{"data":{"channel":{"id":"1","self":{"watchStreakMilestone":{` +
				`"missedStreams":[{"broadcastIdentifiers":"nope"}]}}}}}`,
			want: map[string]string{
				"missedStreamsPresence":     "VALID",
				"missedStreamsCount":        "1",
				"broadcastIdentifierCount":  "0",
				"broadcastIdentifierArrays": "MALFORMED:1",
				"broadcastIdentifierIds":    "none",
			},
		},
		{
			name: "nested array genuinely empty",
			body: `{"data":{"channel":{"id":"1","self":{"watchStreakMilestone":{` +
				`"missedStreams":[{"broadcastIdentifiers":[]}]}}}}}`,
			want: map[string]string{
				"broadcastIdentifierCount":  "0",
				"broadcastIdentifierArrays": "EMPTY:1",
				"broadcastIdentifierIds":    "none",
			},
		},
		{
			name: "nested array null",
			body: `{"data":{"channel":{"id":"1","self":{"watchStreakMilestone":{` +
				`"missedStreams":[{"broadcastIdentifiers":null}]}}}}}`,
			want: map[string]string{"broadcastIdentifierArrays": "NULL:1"},
		},
		{
			name: "nested array missing",
			body: `{"data":{"channel":{"id":"1","self":{"watchStreakMilestone":{` +
				`"missedStreams":[{}]}}}}}`,
			want: map[string]string{"broadcastIdentifierArrays": "MISSING:1"},
		},
		{
			name: "per-element id classes survive",
			body: `{"data":{"channel":{"id":"1","self":{"watchStreakMilestone":{` +
				`"missedStreams":[{"broadcastIdentifiers":[{"id":"keep"},{"id":null},{},{"id":9},"nope"]}]}}}}}`,
			want: map[string]string{
				"broadcastIdentifierCount":     "1",
				"broadcastIdentifierMalformed": "4",
				"broadcastIdentifierArrays":    "MALFORMED:1",
				"broadcastIdentifierIds":       "VALID:1,MISSING:1,NULL:1,MALFORMED:2",
			},
		},
		{
			name: "scalar null is not an observed empty string",
			body: `{"data":{"channel":{"id":"1","self":{"watchStreakMilestone":{` +
				`"state":null,"watchStreakThreshold":"three"}}}}}`,
			want: map[string]string{
				"state":                "<NULL>",
				"watchStreakThreshold": "<MALFORMED>",
			},
		},
		{
			name: "an observed value that reads like a classification is not one",
			body: `{"data":{"channel":{"id":"1","self":{"watchStreakMilestone":{` +
				`"state":"MISSING"}}}}}`,
			want: map[string]string{"state": "MISSING"},
			// The observed literal must NOT render as the absence token.
			notEq: map[string]string{"state": "<MISSING>"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			logins := milestoneLogins(t, 1)
			m, _ := newMilestoneMiner(t, &milestoneRoundTripper{rewardListBody: tc.body}, logins, logins)

			m.observeWatchStreakMilestones(context.Background())

			lines := recordLines(logs.String(), milestoneObservationRecord)
			if len(lines) != 1 {
				t.Fatalf("observation records = %d, want 1", len(lines))
			}
			for key, want := range tc.want {
				if got := attrValue(lines[0], key); got != want {
					t.Errorf("%s = %q, want %q\nline: %s", key, got, want, lines[0])
				}
			}
			for key, unwanted := range tc.notEq {
				if got := attrValue(lines[0], key); got == unwanted {
					t.Errorf("%s = %q, which must not be indistinguishable from the classification\nline: %s",
						key, got, lines[0])
				}
			}
		})
	}
}

// TestTruncateForLogBoundsTwitchStrings proves the record's central privacy and
// robustness claim: no Twitch-controlled string reaches a log unbounded, and
// truncation never emits an invalid rune or an unmarked cut.
func TestTruncateForLogBoundsTwitchStrings(t *testing.T) {
	t.Run("short strings pass through", func(t *testing.T) {
		for _, v := range []string{"", "m-1", strings.Repeat("a", milestoneLogStringCap)} {
			if got := truncateForLog(v); got != v {
				t.Errorf("truncateForLog(%d bytes) altered a within-cap value", len(v))
			}
		}
	})

	t.Run("over-cap strings are cut and marked", func(t *testing.T) {
		long := strings.Repeat("a", milestoneLogStringCap*40)
		got := truncateForLog(long)
		if len(got) >= len(long) {
			t.Fatalf("value was not bounded: %d bytes", len(got))
		}
		if !strings.HasSuffix(got, "...(truncated)") {
			t.Errorf("truncation was not marked: %q", got[len(got)-20:])
		}
	})

	t.Run("cuts on a rune boundary", func(t *testing.T) {
		// Multi-byte runes straddling the cap must not be split.
		for pad := 0; pad < 4; pad++ {
			v := strings.Repeat("a", pad) + strings.Repeat("\u00e9", milestoneLogStringCap)
			got := truncateForLog(v)
			if !utf8.ValidString(got) {
				t.Fatalf("pad %d: truncation produced an invalid UTF-8 string", pad)
			}
		}
	})

	t.Run("a hostile response cannot make the record unbounded", func(t *testing.T) {
		logs := captureLogs(t)
		logins := milestoneLogins(t, 1)
		hostile := strings.Repeat("Z", 200000)
		body := `{"data":{"channel":{"id":"` + hostile + `","self":{"watchStreakMilestone":{` +
			`"state":"` + hostile + `","missedStreams":[{"broadcastIdentifiers":[{"id":"` + hostile + `"}]}],` +
			`"watchStreakMilestone":{"id":"` + hostile + `","achievementTimestamp":"` + hostile + `"}}}}}}`
		m, _ := newMilestoneMiner(t, &milestoneRoundTripper{rewardListBody: body}, logins, logins)

		m.observeWatchStreakMilestones(context.Background())

		lines := recordLines(logs.String(), milestoneObservationRecord)
		if len(lines) != 1 {
			t.Fatalf("observation records = %d, want 1", len(lines))
		}
		if len(lines[0]) > 4096 {
			t.Fatalf("a hostile response produced a %d-byte record; it must stay bounded", len(lines[0]))
		}
		if strings.Contains(lines[0], strings.Repeat("Z", milestoneLogStringCap+1)) {
			t.Error("an unbounded Twitch string reached the record")
		}
		if !strings.Contains(lines[0], "...(truncated)") {
			t.Error("the record does not mark the values it cut")
		}
	})
}

// TestObservationUnsupportedQueryStaysObservationalOnly proves a stale
// persisted-query hash produces UNKNOWN/UNAVAILABLE evidence and changes no
// Stream state, no capability and no streak state.
func TestObservationUnsupportedQueryStaysObservationalOnly(t *testing.T) {
	logs := captureLogs(t)
	logins := milestoneLogins(t, 1)
	rt := &milestoneRoundTripper{
		rewardListBody: `{"errors":[{"message":"PersistedQueryNotFound","extensions":{"code":"PERSISTED_QUERY_NOT_FOUND"}}]}`,
	}
	m, streamers := newMilestoneMiner(t, rt, logins, logins)
	s := streamers[logins[0]]
	s.Stream.Update("broadcast-local-1", "title", nil, nil, 1)

	beforeCapability := s.GetChannelPointsCapability()
	beforePoints := s.GetChannelPoints()
	beforeStreak := s.Stream.EvaluateWatchStreak(time.Now())
	beforeBroadcast := s.Stream.GetBroadcastID()
	beforeOnline := s.GetIsOnline()

	m.observeWatchStreakMilestones(context.Background())

	lines := recordLines(logs.String(), milestoneObservationRecord)
	if len(lines) != 1 {
		t.Fatalf("observation records = %d, want 1", len(lines))
	}
	if got := attrValue(lines[0], "outcome"); got != "UNSUPPORTED_QUERY" {
		t.Errorf("outcome = %q, want UNSUPPORTED_QUERY", got)
	}
	if got := attrValue(lines[0], "failureClass"); got != "PERSISTED_QUERY_NOT_FOUND" {
		t.Errorf("failureClass = %q, want PERSISTED_QUERY_NOT_FOUND", got)
	}

	if got := s.GetChannelPointsCapability(); got != beforeCapability {
		t.Errorf("capability changed: %v -> %v", beforeCapability, got)
	}
	if got := s.GetChannelPoints(); got != beforePoints {
		t.Errorf("points changed: %d -> %d", beforePoints, got)
	}
	afterStreak := s.Stream.EvaluateWatchStreak(time.Now())
	if afterStreak.State != beforeStreak.State {
		t.Errorf("streak state changed: %v -> %v", beforeStreak.State, afterStreak.State)
	}
	if got := s.Stream.GetBroadcastID(); got != beforeBroadcast {
		t.Errorf("broadcast changed: %q -> %q", beforeBroadcast, got)
	}
	if got := s.GetIsOnline(); got != beforeOnline {
		t.Errorf("online changed: %v -> %v", beforeOnline, got)
	}
}

// TestRepeatedIdenticalObservationsStayDistinctLogFacts proves temporal
// evidence is never deduped: two samples with identical content are two
// records with two sequences.
func TestRepeatedIdenticalObservationsStayDistinctLogFacts(t *testing.T) {
	logs := captureLogs(t)
	logins := milestoneLogins(t, 1)
	rt := &milestoneRoundTripper{}
	m, _ := newMilestoneMiner(t, rt, logins, logins)

	m.observeWatchStreakMilestones(context.Background())
	m.observeWatchStreakMilestones(context.Background())

	lines := recordLines(logs.String(), milestoneObservationRecord)
	if len(lines) != 2 {
		t.Fatalf("observation records = %d, want 2; logs:\n%s", len(lines), logs.String())
	}
	first, second := attrValue(lines[0], "sequence"), attrValue(lines[1], "sequence")
	if first == "" || first == second {
		t.Fatalf("identical observations collapsed onto sequence %q/%q", first, second)
	}
	for _, line := range lines {
		if got := attrValue(line, "milestoneValue"); got != "4" {
			t.Fatalf("milestone value changed between identical samples: %q", got)
		}
	}
}

// TestObservationRecordsLeakNoSecrets is the privacy proof: the diagnostic
// records carry no token, no header, no raw GQL payload and no raw error text.
func TestObservationRecordsLeakNoSecrets(t *testing.T) {
	logs := captureLogs(t)
	logins := milestoneLogins(t, 1)
	rt := &milestoneRoundTripper{}
	m, _ := newMilestoneMiner(t, rt, logins, logins)

	m.observeWatchStreakMilestones(context.Background())

	lines := recordLines(logs.String(), milestoneObservationRecord)
	if len(lines) != 1 {
		t.Fatalf("observation records = %d, want 1", len(lines))
	}
	forbidden := []string{
		"milestone-secret-token", // the OAuth access token
		"OAuth ",
		"Authorization",
		"Client-Id",
		"client-id",
		"sha256Hash",
		"persistedQuery",
		"operationName",
		`{"data"`,   // any raw payload fragment
		"signature", // playback token/signature vocabulary
		"cookie",
		"Cookie",
	}
	for _, line := range lines {
		for _, bad := range forbidden {
			if strings.Contains(line, bad) {
				t.Errorf("observation record leaked %q:\n%s", bad, line)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Grant correlation
// ---------------------------------------------------------------------------

// newMilestoneCorrelationMiner builds a miner that can BOTH observe (a real
// TwitchClient over rt) and account (a live analytics service on a real
// database), so the PREVIOUS OBSERVATION -> EVENT -> FOLLOWING OBSERVATION
// sequence can be exercised end to end.
func newMilestoneCorrelationMiner(t *testing.T, rt *milestoneRoundTripper) (*Miner, *models.Streamer, *analytics.Service) {
	t.Helper()
	logins := milestoneLogins(t, 1)
	m, streamers := newMilestoneMiner(t, rt, logins, logins)

	// database.Open is a process-global singleton (see TestMain in
	// database_singleton_test.go): this resolves to the package-wide handle and
	// must never be closed by an individual test.
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	svc, err := analytics.NewService(db, t.TempDir(), 0)
	if err != nil {
		t.Fatalf("analytics service: %v", err)
	}
	m.analyticsSvc = svc
	return m, streamers[logins[0]], svc
}

// watchStreakFrame builds one WATCH_STREAK points-earned frame through the real
// parse layer, so it carries the production event identity.
func watchStreakFrame(t *testing.T, s *models.Streamer, total, balance int, ts string) *pubsub.PubSubMessage {
	t.Helper()
	return parsedPointsEarned(t, s.ChannelID, "WATCH_STREAK", total, balance, ts)
}

// singleCorrelationLine returns the one correlation record the logs must hold.
func singleCorrelationLine(t *testing.T, logs string) string {
	t.Helper()
	lines := recordLines(logs, milestoneCorrelationRecord)
	if len(lines) != 1 {
		t.Fatalf("correlation records = %d, want 1; logs:\n%s", len(lines), logs)
	}
	return lines[0]
}

// TestWatchStreakCorrelationReportsWhatTheLedgerActuallyDid proves a newly
// accepted grant produces one correlation record whose ledger outcome is the
// ledger's real answer, with the domain admission reported separately.
func TestWatchStreakCorrelationReportsWhatTheLedgerActuallyDid(t *testing.T) {
	logbuf := captureLogs(t)
	m, s, svc := newMilestoneCorrelationMiner(t, &milestoneRoundTripper{})
	s.Stream.Update("broadcast-live-1", "title", nil, nil, 1)

	msg := watchStreakFrame(t, s, 450, 5450, "2026-09-05T12:00:00Z")
	outcome := deliverPointsEarned(t, m, s, msg)
	if !outcome.WatchStreak.NewlyAccepted() {
		t.Fatal("fixture grant was not newly accepted")
	}

	line := singleCorrelationLine(t, logbuf.String())
	for key, want := range map[string]string{
		"ledgerOutcome":             "LEDGER_COMMITTED",
		"domainAdmission":           "NEW_UNBOUND",
		"domainAccepted":            "true",
		"grantBinding":              "GRANTED_UNBOUND",
		"exactTotalPoints":          "450",
		"exactAmount":               "true",
		"wireTimestamp":             "2026-09-05T12:00:00Z",
		"provenBroadcastId":         "NONE",
		"localBroadcastContextOnly": "broadcast-live-1",
		"milestoneLink":             "UNKNOWN",
		"expectedMilestone":         "UNKNOWN",
	} {
		if got := attrValue(line, key); got != want {
			t.Errorf("%s = %q, want %q\nline: %s", key, got, want, line)
		}
	}
	// Provenance, not merely non-emptiness: the record must carry the
	// ALREADY-EXISTING event identity, not a fresh one of its own.
	if got := attrValue(line, "eventId"); got != msg.EventFingerprint {
		t.Errorf("eventId = %q, want the frame's own identity %q", got, msg.EventFingerprint)
	}
	// Local acceptance time and the local accounting-outcome time are two
	// different clocks and are recorded separately.
	accepted, err := time.Parse(time.RFC3339Nano, attrValue(line, "localAcceptedAt"))
	if err != nil {
		t.Fatalf("localAcceptedAt not parseable: %v", err)
	}
	outcomeAt, err := time.Parse(time.RFC3339Nano, attrValue(line, "accountingOutcomeAt"))
	if err != nil {
		t.Fatalf("accountingOutcomeAt not parseable: %v", err)
	}
	if outcomeAt.Before(accepted) {
		t.Errorf("accounting outcome %v precedes acceptance %v", outcomeAt, accepted)
	}
	// localAcceptedAt must come from the ADMITTED GRANT FACT, not from a clock
	// read at log time — otherwise it silently becomes a third copy of the
	// accounting time and stops being acceptance provenance at all.
	var admitted time.Time
	for _, fact := range outcome.WatchStreak.Persistence.Grants {
		if fact.EventID == msg.EventFingerprint {
			admitted = fact.AcceptedAt
		}
	}
	if admitted.IsZero() {
		t.Fatal("fixture produced no admitted grant fact")
	}
	if !accepted.Equal(admitted.UTC()) {
		t.Errorf("localAcceptedAt = %v, want the admitted grant fact's own time %v", accepted, admitted.UTC())
	}

	exact := mustExactEarnings(t, svc.Repository(), s.GetUsername(), time.Time{}, time.Time{})
	if exact.Events != 1 {
		t.Fatalf("ledger rows = %d, want 1", exact.Events)
	}
}

// TestWatchStreakCorrelationExactAmountsPassThroughUnchanged is the 300/350/
// 400/450 invariant: every valid integer amount reaches the ledger, History and
// the record EXACTLY as received. Nothing is filtered, rewritten, promoted or
// rejected for failing to match a presumed ladder, and no amount implies an
// expected milestone.
//
// The amounts below are FIXTURE DATA, not an assertion that Twitch uses this
// ladder. 137 and 451 are included precisely so no ladder can be inferred.
func TestWatchStreakCorrelationExactAmountsPassThroughUnchanged(t *testing.T) {
	for _, amount := range []int{300, 350, 400, 450, 137, 451} {
		t.Run(fmt.Sprintf("plus-%d", amount), func(t *testing.T) {
			logbuf := captureLogs(t)
			m, s, svc := newMilestoneCorrelationMiner(t, &milestoneRoundTripper{})

			msg := watchStreakFrame(t, s, amount, 1000+amount, "2026-09-05T12:00:00Z")
			deliverPointsEarned(t, m, s, msg)

			line := singleCorrelationLine(t, logbuf.String())
			if got := attrValue(line, "exactTotalPoints"); got != fmt.Sprint(amount) {
				t.Errorf("exactTotalPoints = %q, want %d", got, amount)
			}
			if got := attrValue(line, "expectedMilestone"); got != "UNKNOWN" {
				t.Errorf("a +%d grant produced expectedMilestone=%q; it must stay UNKNOWN", amount, got)
			}

			exact := mustExactEarnings(t, svc.Repository(), s.GetUsername(), time.Time{}, time.Time{})
			gained := 0
			for _, share := range exact.Breakdown {
				if share.Reason == "WATCH_STREAK" {
					gained = share.Gained
				}
			}
			if gained != amount {
				t.Errorf("ledger recorded %d for a +%d grant", gained, amount)
			}
		})
	}
}

// TestWatchStreakCorrelationDuplicateGrantProducesNoSecondAccounting proves an
// exact domain replay is linearized away before any accounting or correlation
// record: one grant, one ledger row, one record.
func TestWatchStreakCorrelationDuplicateGrantProducesNoSecondAccounting(t *testing.T) {
	logbuf := captureLogs(t)
	m, s, svc := newMilestoneCorrelationMiner(t, &milestoneRoundTripper{})

	msg := watchStreakFrame(t, s, 450, 5450, "2026-09-05T12:00:00Z")
	deliverPointsEarned(t, m, s, msg)
	replay := deliverPointsEarned(t, m, s, msg)

	if replay.WatchStreak.Admission != models.WatchStreakGrantDuplicate {
		t.Fatalf("replay admission = %q, want DUPLICATE", replay.WatchStreak.Admission)
	}
	if lines := recordLines(logbuf.String(), milestoneCorrelationRecord); len(lines) != 1 {
		t.Fatalf("correlation records = %d, want 1 (a replay must not produce a second)", len(lines))
	}
	exact := mustExactEarnings(t, svc.Repository(), s.GetUsername(), time.Time{}, time.Time{})
	if exact.Events != 1 {
		t.Fatalf("ledger rows = %d, want 1", exact.Events)
	}
	if entry := s.History["WATCH_STREAK"]; entry == nil || entry.Counter != 1 || entry.Amount != 450 {
		t.Fatalf("history = %+v, want one 450-point grant", entry)
	}
}

// TestWatchStreakCorrelationLedgerDuplicateIsNotCommitted is the restart
// falsifier: after a restart the domain's in-memory grant ledger is empty, so
// the SAME event is admitted anew — but the exact ledger already holds it. The
// record must say DUPLICATE, never COMMITTED, and no second row may appear.
func TestWatchStreakCorrelationLedgerDuplicateIsNotCommitted(t *testing.T) {
	logbuf := captureLogs(t)
	m, s, svc := newMilestoneCorrelationMiner(t, &milestoneRoundTripper{})

	msg := watchStreakFrame(t, s, 450, 5450, "2026-09-05T12:00:00Z")
	deliverPointsEarned(t, m, s, msg)

	// Restart: a fresh Stream has no grant ledger and no broadcast binding.
	s.Stream = models.NewStream()
	replay := deliverPointsEarned(t, m, s, msg)
	if !replay.WatchStreak.NewlyAccepted() {
		t.Fatalf("post-restart admission = %q, want a fresh acceptance", replay.WatchStreak.Admission)
	}

	lines := recordLines(logbuf.String(), milestoneCorrelationRecord)
	if len(lines) != 2 {
		t.Fatalf("correlation records = %d, want 2", len(lines))
	}
	if got := attrValue(lines[0], "ledgerOutcome"); got != "LEDGER_COMMITTED" {
		t.Errorf("first ledgerOutcome = %q, want LEDGER_COMMITTED", got)
	}
	if got := attrValue(lines[1], "ledgerOutcome"); got != "LEDGER_DUPLICATE" {
		t.Errorf("post-restart ledgerOutcome = %q, want LEDGER_DUPLICATE", got)
	}
	if got := attrValue(lines[1], "domainAccepted"); got != "true" {
		t.Errorf("post-restart domainAccepted = %q; domain acceptance and ledger commitment are separate facts", got)
	}
	exact := mustExactEarnings(t, svc.Repository(), s.GetUsername(), time.Time{}, time.Time{})
	if exact.Events != 1 {
		t.Fatalf("ledger rows = %d, want 1 (the exact ledger must reject the duplicate)", exact.Events)
	}
	// The relationship to any milestone stays UNKNOWN across a restart.
	if got := attrValue(lines[1], "milestoneLink"); got != "UNKNOWN" {
		t.Errorf("post-restart milestoneLink = %q, want UNKNOWN", got)
	}
}

// TestWatchStreakCorrelationLedgerFailureIsNeverCommitted proves a refused
// ledger write is reported as FAILED — never as COMMITTED just because the
// domain accepted the grant.
func TestWatchStreakCorrelationLedgerFailureIsNeverCommitted(t *testing.T) {
	logbuf := captureLogs(t)
	m, s, svc := newMilestoneCorrelationMiner(t, &milestoneRoundTripper{})

	// Arm the repository's resurrection fence: every write path now returns
	// ErrStreamerDeleted. This is a real production failure mode (a streamer
	// removed while an authoritative event is still in flight) and, unlike
	// closing the process-global database handle, it is confined to this test.
	svc.Repository().Tombstone(s.GetUsername())

	msg := watchStreakFrame(t, s, 450, 5450, "2026-09-05T12:00:00Z")
	outcome := deliverPointsEarned(t, m, s, msg)
	if !outcome.WatchStreak.NewlyAccepted() {
		t.Fatal("fixture grant was not newly accepted")
	}

	line := singleCorrelationLine(t, logbuf.String())
	if got := attrValue(line, "ledgerOutcome"); got != "LEDGER_FAILED" {
		t.Fatalf("ledgerOutcome = %q, want LEDGER_FAILED\nline: %s", got, line)
	}
	if got := attrValue(line, "domainAccepted"); got != "true" {
		t.Errorf("domainAccepted = %q, want true (the domain DID accept)", got)
	}
	if got := attrValue(line, "exactTotalPoints"); got != "450" {
		t.Errorf("a failed ledger write altered the reported amount: %q", got)
	}
}

// TestWatchStreakCorrelationAnalyticsUnavailableIsExplicit proves a generation
// with no analytics service reports ANALYTICS_UNAVAILABLE, not FAILED and not
// a silent omission.
func TestWatchStreakCorrelationAnalyticsUnavailableIsExplicit(t *testing.T) {
	logbuf := captureLogs(t)
	m, s, _ := newMilestoneCorrelationMiner(t, &milestoneRoundTripper{})
	m.analyticsSvc = nil

	msg := watchStreakFrame(t, s, 450, 5450, "2026-09-05T12:00:00Z")
	deliverPointsEarned(t, m, s, msg)

	line := singleCorrelationLine(t, logbuf.String())
	if got := attrValue(line, "ledgerOutcome"); got != "ANALYTICS_UNAVAILABLE" {
		t.Fatalf("ledgerOutcome = %q, want ANALYTICS_UNAVAILABLE\nline: %s", got, line)
	}
}

// TestWatchStreakCorrelationTimelineOnlyFallbackIsExplicit proves a frame the
// exact ledger cannot admit is reported as TIMELINE_ONLY, distinct from both
// COMMITTED and FAILED.
func TestWatchStreakCorrelationTimelineOnlyFallbackIsExplicit(t *testing.T) {
	logbuf := captureLogs(t)
	m, s, svc := newMilestoneCorrelationMiner(t, &milestoneRoundTripper{})

	// No RFC 3339 wire timestamp: the ledger's event identity is not
	// establishable, so the frame is timeline-only.
	msg := watchStreakFrame(t, s, 450, 5450, "not-a-timestamp")
	deliverPointsEarned(t, m, s, msg)

	line := singleCorrelationLine(t, logbuf.String())
	if got := attrValue(line, "ledgerOutcome"); got != "TIMELINE_ONLY" {
		t.Fatalf("ledgerOutcome = %q, want TIMELINE_ONLY\nline: %s", got, line)
	}
	if got := attrValue(line, "exactTotalPoints"); got != "450" {
		t.Errorf("exactTotalPoints = %q, want 450 (the amount is still exact)", got)
	}
	exact := mustExactEarnings(t, svc.Repository(), s.GetUsername(), time.Time{}, time.Time{})
	if exact.Events != 0 {
		t.Fatalf("ledger rows = %d, want 0 for a timeline-only frame", exact.Events)
	}
}

// TestWatchStreakCorrelationNeverFabricatesABroadcastBinding proves the current
// Stream.BroadcastID is never promoted to ProvenBroadcastID — not in the record
// and not in the persisted grant fact.
func TestWatchStreakCorrelationNeverFabricatesABroadcastBinding(t *testing.T) {
	logbuf := captureLogs(t)
	m, s, _ := newMilestoneCorrelationMiner(t, &milestoneRoundTripper{})
	s.Stream.Update("broadcast-in-view", "title", nil, nil, 1)

	msg := watchStreakFrame(t, s, 450, 5450, "2026-09-05T12:00:00Z")
	outcome := deliverPointsEarned(t, m, s, msg)

	line := singleCorrelationLine(t, logbuf.String())
	if got := attrValue(line, "provenBroadcastId"); got != "NONE" {
		t.Fatalf("provenBroadcastId = %q, want NONE", got)
	}
	if got := attrValue(line, "localBroadcastContextOnly"); got != "broadcast-in-view" {
		t.Errorf("localBroadcastContextOnly = %q, want the observed broadcast as CONTEXT", got)
	}
	for _, fact := range outcome.WatchStreak.Persistence.Grants {
		if fact.Binding != models.WatchStreakGrantUnbound || fact.BroadcastID != "" {
			t.Fatalf("persisted grant fabricated a binding: %+v", fact)
		}
	}
}

// TestPreviousEventFollowingObservationLinksStayUnknown is the required
// diagnostic sequence: PREVIOUS OBSERVATION -> EVENT -> FOLLOWING OBSERVATION,
// with every link between them explicitly UNKNOWN. No record may claim a
// snapshot was expected at grant or changed because of the grant, and the
// event must not rewrite the observation that preceded it.
func TestPreviousEventFollowingObservationLinksStayUnknown(t *testing.T) {
	logbuf := captureLogs(t)
	rt := &milestoneRoundTripper{}
	m, s, _ := newMilestoneCorrelationMiner(t, rt)
	s.Stream.Update("broadcast-live-1", "title", nil, nil, 1)

	// PREVIOUS OBSERVATION
	m.observeWatchStreakMilestones(context.Background())
	previousLogs := logbuf.String()
	previous := recordLines(previousLogs, milestoneObservationRecord)
	if len(previous) != 1 {
		t.Fatalf("previous observation records = %d, want 1", len(previous))
	}

	// EVENT (delivered later, with a milestone value that did not change)
	msg := watchStreakFrame(t, s, 450, 5450, "2026-09-05T12:00:00Z")
	deliverPointsEarned(t, m, s, msg)

	// FOLLOWING OBSERVATION
	m.observeWatchStreakMilestones(context.Background())

	full := logbuf.String()
	observations := recordLines(full, milestoneObservationRecord)
	if len(observations) != 2 {
		t.Fatalf("observation records = %d, want 2 (previous + following)", len(observations))
	}
	correlation := singleCorrelationLine(t, full)

	// The event did not rewrite history: the previous record is byte-identical.
	if observations[0] != previous[0] {
		t.Fatalf("the grant rewrote the previous observation:\nbefore: %s\nafter:  %s", previous[0], observations[0])
	}
	// Ordering in the retained log: previous, then event, then following.
	if !(strings.Index(full, observations[0]) < strings.Index(full, correlation) &&
		strings.Index(full, correlation) < strings.Index(full, observations[1])) {
		t.Fatal("records are not in PREVIOUS -> EVENT -> FOLLOWING order")
	}
	// Distinct observations, not a deduped one.
	if attrValue(observations[0], "sequence") == attrValue(observations[1], "sequence") {
		t.Fatal("the two observations share a sequence")
	}
	// Every link stays UNKNOWN.
	for _, line := range observations {
		if got := attrValue(line, "grantLink"); got != "UNKNOWN" {
			t.Errorf("observation grantLink = %q, want UNKNOWN", got)
		}
		if got := attrValue(line, "expectedMilestone"); got != "UNKNOWN" {
			t.Errorf("observation expectedMilestone = %q, want UNKNOWN", got)
		}
	}
	if got := attrValue(correlation, "milestoneLink"); got != "UNKNOWN" {
		t.Errorf("correlation milestoneLink = %q, want UNKNOWN", got)
	}
	// No record may assert causality or expectation.
	for _, phrase := range []string{
		"expected at grant", "changed because of grant", "expectedAtGrant", "changedByGrant",
		"causedBy", "milestoneLink=OBSERVED", "expectedMilestone=450",
	} {
		if strings.Contains(full, phrase) {
			t.Errorf("a record asserted an unproven relationship: %q", phrase)
		}
	}
}

// TestBroadcastChangeDuringObservationCreatesNoGrantBinding proves a broadcast
// that changes while a RewardList request is in flight cannot manufacture a
// binding: the observation stays context-only and the grant ledger is untouched.
func TestBroadcastChangeDuringObservationCreatesNoGrantBinding(t *testing.T) {
	logbuf := captureLogs(t)
	rt := &milestoneRoundTripper{}
	m, s, _ := newMilestoneCorrelationMiner(t, rt)
	s.Stream.Update("broadcast-before", "title", nil, nil, 1)

	rt.mu.Lock()
	rt.onRewardList = func(int) {
		// The broadcast rolls over while the observation is in flight.
		s.Stream.Update("broadcast-after", "title", nil, nil, 1)
	}
	rt.mu.Unlock()

	m.observeWatchStreakMilestones(context.Background())

	lines := recordLines(logbuf.String(), milestoneObservationRecord)
	if len(lines) != 1 {
		t.Fatalf("observation records = %d, want 1", len(lines))
	}
	// Whichever broadcast the record captured, it is context only and it is not
	// a binding.
	if got := attrValue(lines[0], "grantLink"); got != "UNKNOWN" {
		t.Errorf("grantLink = %q, want UNKNOWN", got)
	}
	if !strings.Contains(lines[0], "localBroadcastContextOnly=") {
		t.Errorf("record did not label its broadcast as context only: %s", lines[0])
	}
	decision := s.Stream.EvaluateWatchStreak(time.Now())
	if decision.State == models.WatchStreakGranted {
		t.Fatal("an observation granted a watch streak")
	}
	if len(s.Stream.WatchStreakPersistence().Grants) != 0 {
		t.Fatal("an observation manufactured a grant fact")
	}
}

// TestWatchStreakCorrelationRecordLeaksNoSecrets is the privacy proof for the
// correlation record.
func TestWatchStreakCorrelationRecordLeaksNoSecrets(t *testing.T) {
	logbuf := captureLogs(t)
	m, s, _ := newMilestoneCorrelationMiner(t, &milestoneRoundTripper{})

	msg := watchStreakFrame(t, s, 450, 5450, "2026-09-05T12:00:00Z")
	deliverPointsEarned(t, m, s, msg)

	line := singleCorrelationLine(t, logbuf.String())
	for _, bad := range []string{
		"milestone-secret-token", "OAuth ", "Authorization", "Client-Id",
		"point_gain", "balance\":", `{"data"`, "cookie", "Cookie", "signature",
	} {
		if strings.Contains(line, bad) {
			t.Errorf("correlation record leaked %q:\n%s", bad, line)
		}
	}
}

// TestCorrelationRecordIsWatchStreakOnly proves the correlation record is
// scoped to WATCH_STREAK: every other reason code goes through the unchanged
// accounting path and emits nothing here.
func TestCorrelationRecordIsWatchStreakOnly(t *testing.T) {
	for _, reason := range []string{"CLAIM", "RAID", "WATCH", "SUB_GIFT"} {
		t.Run(reason, func(t *testing.T) {
			logbuf := captureLogs(t)
			m, s, svc := newMilestoneCorrelationMiner(t, &milestoneRoundTripper{})

			msg := parsedPointsEarned(t, s.ChannelID, reason, 450, 5450, "2026-09-05T12:00:00Z")
			deliverPointsEarned(t, m, s, msg)

			if lines := recordLines(logbuf.String(), milestoneCorrelationRecord); len(lines) != 0 {
				t.Fatalf("%s emitted %d correlation records, want 0:\n%s", reason, len(lines), lines[0])
			}
			// The unchanged accounting path still recorded the event exactly.
			exact := mustExactEarnings(t, svc.Repository(), s.GetUsername(), time.Time{}, time.Time{})
			if exact.Events != 1 {
				t.Fatalf("%s: ledger rows = %d, want 1", reason, exact.Events)
			}
		})
	}
}

// TestUnsupportedQueryRecordStaysOffTheDashboardLogView pins the log LEVEL of
// the unsupported-hash record.
//
// internal/web/logclass.go hides unmatched INFO/DEBUG lines from the dashboard
// log view and never hides a WARN or ERROR. The shared GQL transport already
// raises its own ERROR for an exhausted persisted query, so this diagnostic
// record must stay at INFO: raising it would add a second dashboard-visible
// line per online streamer per bonus cycle without adding information.
func TestUnsupportedQueryRecordStaysOffTheDashboardLogView(t *testing.T) {
	logs := captureLogs(t)
	logins := milestoneLogins(t, 1)
	rt := &milestoneRoundTripper{
		rewardListBody: `{"errors":[{"message":"PersistedQueryNotFound","extensions":{"code":"PERSISTED_QUERY_NOT_FOUND"}}]}`,
	}
	m, _ := newMilestoneMiner(t, rt, logins, logins)

	m.observeWatchStreakMilestones(context.Background())

	lines := recordLines(logs.String(), milestoneObservationRecord)
	if len(lines) != 1 {
		t.Fatalf("observation records = %d, want 1", len(lines))
	}
	if got := attrValue(lines[0], "level"); got != "INFO" {
		t.Fatalf("unsupported-hash record level = %q, want INFO\nline: %s", got, lines[0])
	}
	if got := attrValue(lines[0], "outcome"); got != "UNSUPPORTED_QUERY" {
		t.Fatalf("outcome = %q, want UNSUPPORTED_QUERY", got)
	}

	// And nothing else the cycle emitted may be dashboard-visible either. The
	// shared transport's own stale-hash WARN/ERROR lines are the operator's
	// signal for a BUSINESS operation; for a diagnostic read whose live
	// acceptance is not established they would fire for every online streamer
	// on every cycle, which is volume, not information.
	for _, line := range strings.Split(logs.String(), "\n") {
		if line == "" {
			continue
		}
		if level := attrValue(line, "level"); level == "WARN" || level == "ERROR" {
			t.Errorf("the observation cycle emitted a dashboard-visible %s line:\n%s", level, line)
		}
	}
}

// TestBusinessStaleHashStillRaisesTheOperatorError is the other half: silencing
// the stale-hash alert must apply ONLY to the diagnostic read. A business
// operation hitting the same stale hash must still raise it.
func TestBusinessStaleHashStillRaisesTheOperatorError(t *testing.T) {
	logs := captureLogs(t)
	logins := milestoneLogins(t, 1)
	rt := &milestoneRoundTripper{}
	m, streamers := newMilestoneMiner(t, rt, logins, logins)

	// Drive the BUSINESS path against a stale hash.
	rt.mu.Lock()
	rt.contextBody = `{"errors":[{"message":"PersistedQueryNotFound","extensions":{"code":"PERSISTED_QUERY_NOT_FOUND"}}]}`
	rt.mu.Unlock()
	_, _ = m.client.ClaimAvailableBonus(streamers[logins[0]])

	if !strings.Contains(logs.String(), "PersistedQueryNotFound on all known client IDs") {
		t.Error("a business read no longer raises the stale-hash ERROR; the diagnostic silence leaked")
	}
}

// TestWatchStreakCorrelationInexactAmountStaysUnknown covers the not-exact
// branch: an amount the ledger cannot represent exactly is reported as UNKNOWN,
// never rounded, truncated or defaulted to zero — and the frame still reaches
// the timeline-only path rather than the exact ledger.
func TestWatchStreakCorrelationInexactAmountStaysUnknown(t *testing.T) {
	for _, tc := range []struct {
		name  string
		total interface{}
	}{
		{"fractional", 450.5},
		{"string", "450"},
		{"null", nil},
		{"boolean", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logbuf := captureLogs(t)
			m, s, svc := newMilestoneCorrelationMiner(t, &milestoneRoundTripper{})

			// Build the frame through the real parse layer, then replace the
			// amount with a value the exact ledger must refuse.
			msg := watchStreakFrame(t, s, 450, 5450, "2026-09-05T12:00:00Z")
			gain := msg.Data["point_gain"].(map[string]interface{})
			gain["total_points"] = tc.total
			deliverPointsEarned(t, m, s, msg)

			line := singleCorrelationLine(t, logbuf.String())
			if got := attrValue(line, "exactAmount"); got != "false" {
				t.Errorf("exactAmount = %q, want false", got)
			}
			if got := attrValue(line, "exactTotalPoints"); got != "UNKNOWN" {
				t.Errorf("exactTotalPoints = %q, want UNKNOWN — an inexact amount must never be rounded or zeroed", got)
			}
			if got := attrValue(line, "ledgerOutcome"); got != "TIMELINE_ONLY" {
				t.Errorf("ledgerOutcome = %q, want TIMELINE_ONLY", got)
			}
			if exact := mustExactEarnings(t, svc.Repository(), s.GetUsername(), time.Time{}, time.Time{}); exact.Events != 0 {
				t.Errorf("ledger rows = %d, want 0 for an inexact amount", exact.Events)
			}
		})
	}
}

// TestWatchStreakCorrelationBindingIsExplicitWhenUnknown proves grantBinding
// never renders as a bare empty string that could read as an observed unbound
// binding.
func TestWatchStreakCorrelationBindingIsExplicitWhenUnknown(t *testing.T) {
	logbuf := captureLogs(t)
	m, s, _ := newMilestoneCorrelationMiner(t, &milestoneRoundTripper{})

	msg := watchStreakFrame(t, s, 450, 5450, "2026-09-05T12:00:00Z")
	deliverPointsEarned(t, m, s, msg)

	line := singleCorrelationLine(t, logbuf.String())
	if got := attrValue(line, "grantBinding"); got != "GRANTED_UNBOUND" {
		t.Fatalf("grantBinding = %q, want GRANTED_UNBOUND", got)
	}
	// And the fallback for an unmatchable fact is the explicit vocabulary.
	if got := grantBinding(models.WatchStreakGrantResult{}, "no-such-event"); got != "UNKNOWN" {
		t.Errorf("grantBinding fallback = %q, want UNKNOWN (never a bare empty string)", got)
	}
}
