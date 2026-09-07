package miner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
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
	opLogins     []string

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

	// offerClaim makes the context read advertise an available bonus, so the
	// business pass performs a real ClaimCommunityPoints mutation — the
	// operation the contract says an observation must never be inserted
	// between.
	offerClaim bool
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

// opLoginsSnapshot returns the per-op login attribution, aligned with ops().
func (rt *milestoneRoundTripper) opLoginsSnapshot() []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return append([]string(nil), rt.opLogins...)
}

const defaultRewardListBody = `{"data":{"channel":{"id":"12345","self":{"watchStreakMilestone":{` +
	`"watchStreakThreshold":3,"watchStreakCopoBonus":450,"state":"ACTIVE","expiresAt":"2026-09-10T00:00:00Z",` +
	`"missedStreams":[{"broadcastIdentifiers":[{"id":"missed-b-1"}]}],` +
	// value is string-encoded because that is the form the donor evidence says
	// Twitch sends for this field; the number form is tolerance and is covered
	// explicitly by TestObservationReportsMilestoneValueWireKind.
	`"watchStreakMilestone":{"id":"m-1","value":"4","achievementTimestamp":"2026-09-05T12:00:00Z","shareStatus":"UNSHARED"}}}}}}`

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
	// The login this op targeted, positionally aligned with ops, so a prefix of
	// ops can be attributed exactly. Empty when the op carries no login.
	login, _ := operation.Variables["channelLogin"].(string)
	rt.opLogins = append(rt.opLogins, login)
	if operation.Name == "ChannelPointsContext" && login != "" {
		if rt.contextBy == nil {
			rt.contextBy = map[string]int{}
		}
		rt.contextBy[login]++
		rt.contextOrder = append(rt.contextOrder, login)
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
	offerClaim := rt.offerClaim
	rt.mu.Unlock()

	response := `{"data":{}}`
	switch operation.Name {
	case "ChannelPointsContext":
		response = `{"data":{"community":{"channel":{"self":{"communityPoints":{"balance":777,"availableClaim":null}}}}}}`
		if offerClaim {
			response = `{"data":{"community":{"channel":{"self":{"communityPoints":{"balance":777,` +
				`"availableClaim":{"id":"claim-1"}}}}}}}`
		}
		if contextBody != "" {
			response = contextBody
		}
	case "ClaimCommunityPoints":
		response = `{"data":{"claimCommunityPoints":{"claim":{"id":"claim-1"}}}}`
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

// hasAttr reports whether a line carries the attribute at all, which attrValue
// cannot distinguish from a present-but-empty value.
func hasAttr(line, key string) bool {
	return strings.Contains(line, " "+key+"=")
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
	// A real bonus is available, so the business pass performs an actual
	// ClaimCommunityPoints mutation. The contract does not merely require the
	// observation to run last — it requires it never to be inserted BETWEEN
	// bonus claiming and auto-redeem, which a context-read-only fixture could
	// not detect.
	rt := &milestoneRoundTripper{offerClaim: true}
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
	// Bounded: the handshake is a real synchronization seam, but without a
	// deadline a loop that never reaches a second observation would hang the
	// package binary instead of failing with a reason.
	select {
	case <-seenTwo:
	case <-time.After(30 * time.Second):
		cancel()
		<-done
		t.Fatal("the bonus poll loop did not reach a second RewardList observation within 30s")
	}
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
	businessOps := map[string]bool{"ChannelPointsContext": true, "ClaimCommunityPoints": true}
	for i, op := range prefix {
		if !businessOps[op] {
			t.Errorf("prefix op %d = %q, want a business-pass operation; ops=%v", i, op, ops)
		}
	}
	// The claim mutation itself must be inside that prefix: an observation
	// inserted between the context read and the claim would put a RewardList
	// ahead of it.
	claims := 0
	for _, op := range prefix {
		if op == "ClaimCommunityPoints" {
			claims++
		}
	}
	if claims == 0 {
		t.Fatalf("the business pass performed no claim, so the fixture cannot prove non-interleaving; ops=%v", ops)
	}
	// Attribute the prefix EXACTLY: only ops that actually precede the first
	// observation count. Walking a separate per-login list and taking its first
	// len(prefix) entries would count context reads from a LATER cycle and pass
	// on an implementation that interleaved mid-roster.
	opLogins := rt.opLoginsSnapshot()
	covered := map[string]bool{}
	for i := 0; i < firstReward && i < len(opLogins); i++ {
		if opLogins[i] != "" {
			covered[opLogins[i]] = true
		}
	}
	for _, login := range logins {
		if !covered[login] {
			t.Errorf("the first observation ran before the business pass reached %s; ops=%v logins=%v",
				login, ops, opLogins[:firstReward])
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

	// failureClass must be PRESENT and empty, not absent: attrValue cannot tell
	// those apart on its own.
	if !hasAttr(line, "failureClass") {
		t.Error("record does not emit failureClass at all")
	}
	for key, want := range map[string]string{
		"outcome":                      "OBSERVED",
		"failureClass":                 "",
		"observedChannelId":            "12345",
		"expiresAt":                    "2026-09-10T00:00:00Z",
		"streamer":                     logins[0],
		"channelId":                    streamers[logins[0]].ChannelID,
		"selfMilestonePresence":        "<VALID>",
		"milestoneNodePresence":        "<VALID>",
		"milestoneId":                  "m-1",
		"milestoneValue":               "4",
		"milestoneValueWireKind":       "STRING",
		"watchStreakThreshold":         "3",
		"watchStreakCopoBonus":         "450",
		"state":                        "ACTIVE",
		"shareStatus":                  "UNSHARED",
		"achievementTimestamp":         "2026-09-05T12:00:00Z",
		"missedStreamsPresence":        "<VALID>",
		"missedStreamsCount":           "1",
		"missedStreamsMalformed":       "0",
		"broadcastIdentifierValidIds":  "1",
		"broadcastIdentifierMalformed": "0",
		"broadcastIdentifierArrays":    "VALID:1",
		"broadcastIdentifierIds":       "VALID:1",
		"localBroadcastContextOnly":    "broadcast-local-1",
		"grantLink":                    "UNKNOWN",
		"streakCountField":             "UNKNOWN",
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
		"selfMilestonePresence":     "<NULL>",
		"milestoneNodePresence":     "<MISSING>",
		"milestoneValue":            "<MISSING>",
		"state":                     "<MISSING>",
		"achievementTimestamp":      "<MISSING>",
		"shareStatus":               "<MISSING>",
		"expiresAt":                 "<MISSING>",
		"watchStreakThreshold":      "<MISSING>",
		"missedStreamsPresence":     "<MISSING>",
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
				"missedStreamsPresence":       "<VALID>",
				"missedStreamsCount":          "1",
				"broadcastIdentifierValidIds": "0",
				"broadcastIdentifierArrays":   "MALFORMED:1",
				"broadcastIdentifierIds":      "none",
			},
		},
		{
			name: "nested array genuinely empty",
			body: `{"data":{"channel":{"id":"1","self":{"watchStreakMilestone":{` +
				`"missedStreams":[{"broadcastIdentifiers":[]}]}}}}}`,
			want: map[string]string{
				"broadcastIdentifierValidIds": "0",
				"broadcastIdentifierArrays":   "EMPTY:1",
				"broadcastIdentifierIds":      "none",
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
			// Six elements classified at TWO nodes. Four are well-formed
			// element objects whose id is VALID / NULL / MISSING / MALFORMED;
			// the remaining two are a non-object element and a null element,
			// neither of which has an id node to classify at all — so they
			// appear in the ELEMENT tally and not in the id tally.
			body: `{"data":{"channel":{"id":"1","self":{"watchStreakMilestone":{` +
				`"missedStreams":[{"broadcastIdentifiers":[{"id":"keep"},{"id":null},{},{"id":9},"nope",null]}]}}}}}`,
			want: map[string]string{
				"broadcastIdentifierValidIds":  "1",
				"broadcastIdentifierMalformed": "2",
				"broadcastIdentifierArrays":    "MALFORMED:1",
				"broadcastIdentifierElements":  "VALID:4,NULL:1,MALFORMED:1",
				"broadcastIdentifierIds":       "VALID:1,MISSING:1,NULL:1,MALFORMED:1",
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

	t.Run("non-printable runes are replaced, so rendered size tracks bounded size", func(t *testing.T) {
		// slog's TextHandler escapes control characters on render, so a raw
		// byte can become four rendered ones. Bounding the RAW length alone
		// would let a control-character response render several times larger
		// than the size this feature publishes.
		control := strings.Repeat("\x00\x07\x1b", milestoneLogStringCap)
		got := truncateForLog(control)
		if strings.ContainsAny(got, "\x00\x07\x1b") {
			t.Errorf("control characters survived sanitization: %q", got)
		}
		if !utf8.ValidString(got) {
			t.Error("sanitized value is not valid UTF-8")
		}
		// A newline or terminal escape must never reach an operator console
		// through a Twitch-controlled value.
		if strings.ContainsAny(truncateForLog("a\nb\rc\x1b[31m"), "\n\r\x1b") {
			t.Error("a newline or terminal escape survived sanitization")
		}
		// Ordinary text, including non-ASCII, is untouched.
		for _, ok := range []string{"m-1", "ACTIVE", "2026-09-10T00:00:00Z", "ünïcodé ✓"} {
			if truncateForLog(ok) != ok {
				t.Errorf("sanitization altered printable text %q -> %q", ok, truncateForLog(ok))
			}
		}
	})

	t.Run("a hostile response cannot make the record unbounded", func(t *testing.T) {
		logs := captureLogs(t)
		logins := milestoneLogins(t, 1)
		// Control characters, not benign letters: this is what would blow the
		// rendered size past the bound if only raw bytes were capped. They are
		// JSON-ESCAPED here because a raw control byte inside a JSON string is
		// invalid JSON — the body has to decode to control characters, not
		// contain them literally.
		// Sized to stay under the diagnostic body cap (1 MiB across all five
		// fields). This subtest is about the RECORD's size bound, which needs a
		// response that actually parses; an over-cap body is refused outright
		// and is covered separately by TestDiagnosticResponseBodyIsCapped. Each
		// field is still ~180 KB, three orders of magnitude past the 128-char
		// per-value truncation, so the path under test is fully exercised.
		hostile := strings.Repeat(`\u001b\u0000Z`, 10000)
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
		// NOTE: asserting on the RENDERED line cannot prove sanitization —
		// slog's TextHandler escapes control bytes itself, so a raw NUL or ESC
		// could never appear here even with the sanitizer removed. The real
		// check is at the pre-format boundary, in
		// TestObservationAttributesAreSanitizedBeforeFormatting below. What
		// this subtest still proves is the SIZE bound, which the sanitizer IS
		// load-bearing for: unsanitized control bytes expand about fourfold on
		// render and would breach it.
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
		"provenStreakCount":         "UNKNOWN",
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

// TestWatchStreakCorrelationExactAmountsPassThroughUnchanged is the amount
// invariant: every valid integer reaches the ledger, History and the record
// EXACTLY as received.
//
// Twitch's documented ladder (2 -> 300, 3 -> 350, 4 -> 400, 5+ -> 450) is a
// current official fact, and the record LABELS an amount against it — but the
// ladder is never a filter. 137 and 451 are included precisely because they
// match no documented rung: they must still pass through untouched and be
// reported as off-ladder rather than rejected, rewritten or promoted. And
// whatever the amount, the record must never claim a proven streak count.
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
			if got := attrValue(line, "provenStreakCount"); got != "UNKNOWN" {
				t.Errorf("a +%d grant produced provenStreakCount=%q; no RewardList field is proven "+
					"to carry the streak count, so it must stay UNKNOWN", amount, got)
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

// TestOfficialLadderConsistencyLabelsWithoutMeasuring pins the corrected
// evidence model at its seam.
//
// KNOWN (Twitch Viewer Channel Point Guide, recorded 2026-09-06): streak count
// 2 -> +300, 3 -> +350, 4 -> +400, 5 or more -> +450.
//
// STILL UNKNOWN: which RewardList field, if any, carries the authoritative
// streak count. So the ladder may only ever produce a CONSISTENCY label, and
// the 5-or-more rung must never collapse to "the streak count is 5".
func TestOfficialLadderConsistencyLabelsWithoutMeasuring(t *testing.T) {
	tests := []struct {
		name       string
		total      int
		totalExact bool
		base       int
		baseExact  bool
		wantTier   watchStreakLadderTier
		wantBasis  string
	}{
		{"300 is the documented second streak", 300, true, 300, true, ladderStreakCount2, "BASELINE_POINTS"},
		{"350 is the documented third", 350, true, 350, true, ladderStreakCount3, "BASELINE_POINTS"},
		{"400 is the documented fourth", 400, true, 400, true, ladderStreakCount4, "BASELINE_POINTS"},
		{"450 is the documented fifth OR LATER", 450, true, 450, true, ladderStreakCount5Plus, "BASELINE_POINTS"},
		{"an off-ladder amount is labelled, not rejected", 451, true, 451, true, ladderOffBase, "BASELINE_POINTS"},
		{"zero is off the documented ladder", 0, true, 0, true, ladderOffBase, "BASELINE_POINTS"},
		{"a negative amount is off the ladder", -450, true, -450, true, ladderOffBase, "BASELINE_POINTS"},
		{
			// A points multiplier scales the credited total above the base. The
			// ladder documents BASE amounts, so comparing the total would call
			// an ordinary 5-or-more streak "off-ladder".
			"a multiplied total is judged on its baseline",
			675, true, 450, true, ladderStreakCount5Plus, "BASELINE_POINTS",
		},
		{
			"without a baseline the total is used and the basis says so",
			450, true, 0, false, ladderStreakCount5Plus, "TOTAL_POINTS",
		},
		{
			"a multiplied total with no baseline is off-ladder on the total",
			675, true, 0, false, ladderOffBase, "TOTAL_POINTS",
		},
		{"an inexact amount cannot be compared", 0, false, 0, false, ladderAmountUnknown, "TOTAL_POINTS"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tier, basis := officialLadderConsistency(tc.total, tc.totalExact, tc.base, tc.baseExact)
			if tier != tc.wantTier {
				t.Errorf("tier = %q, want %q", tier, tc.wantTier)
			}
			if basis != tc.wantBasis {
				t.Errorf("basis = %q, want %q", basis, tc.wantBasis)
			}
		})
	}

	// Every label is a CONSISTENCY statement. None of them may read as a count.
	for _, tier := range []watchStreakLadderTier{
		ladderStreakCount2, ladderStreakCount3, ladderStreakCount4, ladderStreakCount5Plus,
	} {
		if !strings.HasPrefix(string(tier), "CONSISTENT_WITH_") {
			t.Errorf("tier %q does not read as a consistency statement", tier)
		}
	}
	// The top rung is a SET, not a number: the amount cannot tell a 5th streak
	// from a 50th, and the label must not pretend otherwise.
	if string(ladderStreakCount5Plus) == "CONSISTENT_WITH_STREAK_COUNT_5" {
		t.Error("the flat top rung was rendered as an exact count")
	}
}

// TestOfficialLadderMatchesTheDocumentedSource pins the ladder to the owner's
// authoritative evidence, so a silent edit to the table is a test failure
// rather than a quiet change of a documented Twitch fact.
func TestOfficialLadderMatchesTheDocumentedSource(t *testing.T) {
	want := []watchStreakLadderRung{
		{StreakCount: 2, RewardPoints: 300, Tier: ladderStreakCount2},
		{StreakCount: 3, RewardPoints: 350, Tier: ladderStreakCount3},
		{StreakCount: 4, RewardPoints: 400, Tier: ladderStreakCount4},
		{StreakCount: 5, RewardPoints: 450, Tier: ladderStreakCount5Plus, FlatFromHere: true},
	}
	if len(officialWatchStreakLadder) != len(want) {
		t.Fatalf("ladder has %d rungs, want %d (Twitch Viewer Channel Point Guide)",
			len(officialWatchStreakLadder), len(want))
	}
	for i, rung := range want {
		if officialWatchStreakLadder[i] != rung {
			t.Errorf("rung %d = %+v, want %+v (Twitch Viewer Channel Point Guide)",
				i, officialWatchStreakLadder[i], rung)
		}
	}
	// Exactly one rung may be flat, and it must be the last: "5 or more" is the
	// top of the documented ladder, and a rung above it would imply it keeps
	// climbing.
	for i, rung := range officialWatchStreakLadder {
		if rung.FlatFromHere && i != len(officialWatchStreakLadder)-1 {
			t.Errorf("rung %d is flat but is not the last rung", i)
		}
		if !rung.FlatFromHere && i == len(officialWatchStreakLadder)-1 {
			t.Error("the top rung is not marked flat; the documented ladder is flat at 5 or more")
		}
	}
	// Every rung must carry a DISTINCT reward, or a reward could not identify
	// a rung at all and the consistency label would be arbitrary.
	seen := map[int]bool{}
	for _, rung := range officialWatchStreakLadder {
		if seen[rung.RewardPoints] {
			t.Errorf("reward %d appears on more than one rung; the label would be ambiguous", rung.RewardPoints)
		}
		seen[rung.RewardPoints] = true
	}
}

// TestWatchStreakCorrelationLadderLabelInTheRecord proves the label reaches the
// record on the real delivery path, with the streak count still UNKNOWN.
func TestWatchStreakCorrelationLadderLabelInTheRecord(t *testing.T) {
	tests := []struct {
		name     string
		total    int
		baseline int
		wantTier string
		wantBase string
	}{
		{"documented second streak", 300, 300, "CONSISTENT_WITH_STREAK_COUNT_2", "BASELINE_POINTS"},
		{"documented fifth or later", 450, 450, "CONSISTENT_WITH_STREAK_COUNT_5_OR_MORE", "BASELINE_POINTS"},
		{"multiplied fifth or later", 675, 450, "CONSISTENT_WITH_STREAK_COUNT_5_OR_MORE", "BASELINE_POINTS"},
		{"off the documented ladder", 451, 451, "OFF_DOCUMENTED_BASE_LADDER", "BASELINE_POINTS"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logbuf := captureLogs(t)
			m, s, svc := newMilestoneCorrelationMiner(t, &milestoneRoundTripper{})

			msg := watchStreakFrame(t, s, tc.total, 9000, "2026-09-05T12:00:00Z")
			msg.Data["point_gain"].(map[string]interface{})["baseline_points"] = float64(tc.baseline)
			deliverPointsEarned(t, m, s, msg)

			line := singleCorrelationLine(t, logbuf.String())
			for key, want := range map[string]string{
				"officialLadderConsistency": tc.wantTier,
				"officialLadderBasis":       tc.wantBase,
				"baselinePoints":            fmt.Sprint(tc.baseline),
				"exactTotalPoints":          fmt.Sprint(tc.total),
				"provenStreakCount":         "UNKNOWN",
				"milestoneLink":             "UNKNOWN",
			} {
				if got := attrValue(line, key); got != want {
					t.Errorf("%s = %q, want %q\nline: %s", key, got, want, line)
				}
			}
			// The label changed nothing about the accounting.
			exact := mustExactEarnings(t, svc.Repository(), s.GetUsername(), time.Time{}, time.Time{})
			gained := 0
			for _, share := range exact.Breakdown {
				if share.Reason == "WATCH_STREAK" {
					gained = share.Gained
				}
			}
			if gained != tc.total {
				t.Errorf("ledger recorded %d, want the credited total %d", gained, tc.total)
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
	if strings.Index(full, observations[0]) >= strings.Index(full, correlation) ||
		strings.Index(full, correlation) >= strings.Index(full, observations[1]) {
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
		// The ladder is documented, but which observed field carries the streak
		// count is not — so no observation may read as a count measurement.
		if got := attrValue(line, "streakCountField"); got != "UNKNOWN" {
			t.Errorf("observation streakCountField = %q, want UNKNOWN", got)
		}
	}
	if got := attrValue(correlation, "milestoneLink"); got != "UNKNOWN" {
		t.Errorf("correlation milestoneLink = %q, want UNKNOWN", got)
	}
	// No record may assert causality, or turn the documented ladder into a
	// measured streak count.
	if got := attrValue(correlation, "provenStreakCount"); got != "UNKNOWN" {
		t.Errorf("correlation provenStreakCount = %q, want UNKNOWN", got)
	}
	for _, phrase := range []string{
		"expected at grant", "changed because of grant", "expectedAtGrant", "changedByGrant",
		"causedBy", "milestoneLink=OBSERVED",
		// The flat top rung must never be rendered as an exact count.
		"provenStreakCount=5", "streakCount=5", "CONSISTENT_WITH_STREAK_COUNT_5 ",
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
// every per-cycle diagnostic emitted by one observation cycle.
//
// Two different consumers, both of which must stay quiet. internal/logger's
// console handler filters on level ALONE (no message allowlist) and defaults to
// INFO, so anything at INFO or above prints to stdout/docker logs once per
// online streamer per cycle, forever. internal/web/logclass.go additionally
// never hides a WARN or ERROR from the dashboard log view. In this feature's
// own expected steady state (an unaccepted RewardList hash) those lines carry
// no observed data at all, so the whole cycle must stay at DEBUG — where the
// retained file log, this feature's only persistence, still keeps them.
func TestUnsupportedQueryRecordStaysOffTheDashboardLogView(t *testing.T) {
	logs := captureLogs(t)
	logins := milestoneLogins(t, 1)
	rt := &milestoneRoundTripper{
		rewardListBody: `{"errors":[{"message":"PersistedQueryNotFound","extensions":{"code":"PERSISTED_QUERY_NOT_FOUND"}}]}`,
	}
	m, _ := newMilestoneMiner(t, rt, logins, logins)

	// Miner construction logs its own setup lines; only the CYCLE is under test.
	logs.Reset()
	m.observeWatchStreakMilestones(context.Background())

	lines := recordLines(logs.String(), milestoneObservationRecord)
	if len(lines) != 1 {
		t.Fatalf("observation records = %d, want 1", len(lines))
	}
	// DEBUG, not INFO: the console handler filters on level alone and defaults
	// to INFO, so an INFO record here would print one line per online streamer
	// per bonus cycle to stdout forever. DEBUG still reaches the retained file
	// log, which is this feature's only persistence.
	if got := attrValue(lines[0], "level"); got != "DEBUG" {
		t.Fatalf("unsupported-hash record level = %q, want DEBUG — an INFO record floods the console "+
			"every cycle in this feature's own expected steady state\nline: %s", got, lines[0])
	}
	if got := attrValue(lines[0], "outcome"); got != "UNSUPPORTED_QUERY" {
		t.Fatalf("outcome = %q, want UNSUPPORTED_QUERY", got)
	}
	// A failed observation parsed nothing at all, so every observed field must
	// render as the never-reached token — not as an empty value, and not as a
	// presence classification that would imply the parser saw the node.
	for _, key := range []string{
		"milestoneId", "milestoneValue", "milestoneValueWireKind", "state", "shareStatus", "expiresAt",
		"achievementTimestamp", "watchStreakThreshold", "watchStreakCopoBonus", "observedChannelId",
	} {
		if got := attrValue(lines[0], key); got != "<UNSET>" {
			t.Errorf("%s = %q on a failed observation, want <UNSET>", key, got)
		}
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
		if level := attrValue(line, "level"); level == "WARN" || level == "ERROR" || level == "INFO" {
			t.Errorf("the observation cycle emitted a console-visible %s line; every per-cycle "+
				"diagnostic must stay at DEBUG:\n%s", level, line)
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
	// The returned values are asserted, not discarded: the log line alone would
	// still pass if the business call had swallowed the stale hash and reported
	// success, which is the failure this test exists to catch.
	claimed, err := m.client.ClaimAvailableBonus(streamers[logins[0]])
	if claimed {
		t.Error("a business read against a stale hash reported an owned claim")
	}
	if !errors.Is(err, twitch.ErrPersistedQueryNotFound) {
		t.Errorf("business read error = %v, want ErrPersistedQueryNotFound", err)
	}

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
	if got := grantBindingOf(models.WatchStreakGrantFact{}, false); got != "UNKNOWN" {
		t.Errorf("grantBindingOf fallback = %q, want UNKNOWN (never a bare empty string)", got)
	}
	// A found fact with an empty binding is still not UNKNOWN: it is an observed
	// empty binding, and collapsing the two would erase the distinction.
	if got := grantBindingOf(models.WatchStreakGrantFact{}, true); got != "" {
		t.Errorf("grantBindingOf of a found fact = %q, want the observed binding", got)
	}
}

// TestCorrelationProvenBroadcastDistinguishesNoneFromUnknown covers the
// defensive branch M36 exposed: "NONE" and "UNKNOWN" are different answers.
//
// NONE means the admitted grant fact was found and carries no proven broadcast
// — a positive observation. UNKNOWN means no fact was found at all, so nothing
// is known either way; printing NONE there would assert an absence this code
// cannot see. Production always supplies a fact (the record is only emitted for
// a newly accepted grant), so this is asserted directly at the seam.
func TestCorrelationProvenBroadcastDistinguishesNoneFromUnknown(t *testing.T) {
	logbuf := captureLogs(t)
	m, s, _ := newMilestoneCorrelationMiner(t, &milestoneRoundTripper{})
	msg := watchStreakFrame(t, s, 450, 5450, "2026-09-05T12:00:00Z")
	gain := msg.Data["point_gain"].(map[string]interface{})

	t.Run("no admitted fact is UNKNOWN, not NONE", func(t *testing.T) {
		logbuf.Reset()
		// An empty result: the fact for this event identity is not present.
		m.logWatchStreakGrantCorrelation(msg, s, gain, models.WatchStreakGrantResult{}, ledgerCommitted)

		line := singleCorrelationLine(t, logbuf.String())
		if got := attrValue(line, "provenBroadcastId"); got != "UNKNOWN" {
			t.Errorf("provenBroadcastId = %q with no admitted fact, want UNKNOWN — NONE would assert "+
				"an absence this code cannot see", got)
		}
		if got := attrValue(line, "grantBinding"); got != "UNKNOWN" {
			t.Errorf("grantBinding = %q with no admitted fact, want UNKNOWN", got)
		}
	})

	t.Run("an admitted unbound fact is NONE", func(t *testing.T) {
		logbuf.Reset()
		grant := models.WatchStreakGrantResult{
			Admission: models.WatchStreakGrantNewUnbound,
			Persistence: models.WatchStreakPersistence{Grants: []models.WatchStreakGrantFact{{
				EventID:    msg.EventFingerprint,
				Binding:    models.WatchStreakGrantUnbound,
				AcceptedAt: time.Now(),
			}}},
		}
		m.logWatchStreakGrantCorrelation(msg, s, gain, grant, ledgerCommitted)

		line := singleCorrelationLine(t, logbuf.String())
		if got := attrValue(line, "provenBroadcastId"); got != "NONE" {
			t.Errorf("provenBroadcastId = %q for an admitted unbound grant, want NONE", got)
		}
		if got := attrValue(line, "grantBinding"); got != "GRANTED_UNBOUND" {
			t.Errorf("grantBinding = %q, want GRANTED_UNBOUND", got)
		}
	})
}

// TestObservationSkipsTargetsWithoutAChannelIdentity covers the second
// eligibility guard: online is not enough, the request must be scopeable.
func TestObservationSkipsTargetsWithoutAChannelIdentity(t *testing.T) {
	logs := captureLogs(t)
	logins := milestoneLogins(t, 2)
	rt := &milestoneRoundTripper{}
	m, streamers := newMilestoneMiner(t, rt, logins, logins)

	// Online, but with no channel identity to scope a request to.
	streamers[logins[0]].ChannelID = ""
	logs.Reset()

	m.observeWatchStreakMilestones(context.Background())

	_, reward := rt.counts()
	if len(reward) != 1 {
		t.Fatalf("distinct observation targets = %d, want 1 (%v)", len(reward), reward)
	}
	if got := reward[streamers[logins[1]].ChannelID]; got != 1 {
		t.Errorf("the identifiable streamer was observed %d times, want 1", got)
	}
	// The unscopeable target is skipped silently rather than emitting a record
	// that would claim an observation was attempted.
	lines := recordLines(logs.String(), milestoneObservationRecord)
	if len(lines) != 1 {
		t.Fatalf("observation records = %d, want 1", len(lines))
	}
	if got := attrValue(lines[0], "streamer"); got != logins[1] {
		t.Errorf("record is for %q, want the identifiable streamer %q", got, logins[1])
	}
}

// TestObservationRecordBoundsTheIdentifierSample covers the record's last
// unbounded-growth vector: a response with many broadcast identifiers must
// print a bounded SAMPLE while still reporting the exact count.
func TestObservationRecordBoundsTheIdentifierSample(t *testing.T) {
	logs := captureLogs(t)
	logins := milestoneLogins(t, 1)

	const identifiers = milestoneLogIDSample * 5
	ids := make([]string, 0, identifiers)
	for i := 0; i < identifiers; i++ {
		ids = append(ids, fmt.Sprintf(`{"id":"b-%02d"}`, i))
	}
	body := `{"data":{"channel":{"id":"1","self":{"watchStreakMilestone":{"missedStreams":[` +
		`{"broadcastIdentifiers":[` + strings.Join(ids, ",") + `]}]}}}}}`

	m, _ := newMilestoneMiner(t, &milestoneRoundTripper{rewardListBody: body}, logins, logins)
	logs.Reset()
	m.observeWatchStreakMilestones(context.Background())

	lines := recordLines(logs.String(), milestoneObservationRecord)
	if len(lines) != 1 {
		t.Fatalf("observation records = %d, want 1", len(lines))
	}
	// The COUNT is exact — bounding the sample must not bound the truth.
	if got := attrValue(lines[0], "broadcastIdentifierValidIds"); got != fmt.Sprint(identifiers) {
		t.Errorf("broadcastIdentifierValidIds = %q, want the exact %d", got, identifiers)
	}
	// The printed sample is not.
	printed := strings.Count(lines[0], "b-")
	if printed > milestoneLogIDSample {
		t.Errorf("record printed %d identifiers, want at most the %d-entry sample",
			printed, milestoneLogIDSample)
	}
	if printed == 0 {
		t.Error("record printed no identifier sample at all")
	}
}

// TestCorrelationLadderAmountUnknownReachesTheRecord proves the AMOUNT_UNKNOWN
// tier is not only a pure-function outcome: a frame whose amount the ledger
// cannot represent exactly must carry it into the emitted record.
func TestCorrelationLadderAmountUnknownReachesTheRecord(t *testing.T) {
	logbuf := captureLogs(t)
	m, s, _ := newMilestoneCorrelationMiner(t, &milestoneRoundTripper{})

	msg := watchStreakFrame(t, s, 450, 5450, "2026-09-05T12:00:00Z")
	gain := msg.Data["point_gain"].(map[string]interface{})
	gain["total_points"] = 450.5
	gain["baseline_points"] = 450.5
	deliverPointsEarned(t, m, s, msg)

	line := singleCorrelationLine(t, logbuf.String())
	for key, want := range map[string]string{
		"officialLadderConsistency": "AMOUNT_UNKNOWN",
		"exactTotalPoints":          "UNKNOWN",
		"baselinePoints":            "UNKNOWN",
		"provenStreakCount":         "UNKNOWN",
	} {
		if got := attrValue(line, key); got != want {
			t.Errorf("%s = %q, want %q\nline: %s", key, got, want, line)
		}
	}
}

// TestCorrelationProvenBroadcastReportsAProvenBinding covers the remaining
// branch: when an independent source HAS proved a binding, the record reports
// that broadcast rather than NONE or UNKNOWN.
func TestCorrelationProvenBroadcastReportsAProvenBinding(t *testing.T) {
	logbuf := captureLogs(t)
	m, s, _ := newMilestoneCorrelationMiner(t, &milestoneRoundTripper{})
	msg := watchStreakFrame(t, s, 450, 5450, "2026-09-05T12:00:00Z")
	gain := msg.Data["point_gain"].(map[string]interface{})

	grant := models.WatchStreakGrantResult{
		Admission: models.WatchStreakGrantNewBound,
		Persistence: models.WatchStreakPersistence{Grants: []models.WatchStreakGrantFact{{
			EventID:     msg.EventFingerprint,
			Binding:     models.WatchStreakGrantBound,
			BroadcastID: "proven-broadcast-9",
			AcceptedAt:  time.Now(),
		}}},
	}
	m.logWatchStreakGrantCorrelation(msg, s, gain, grant, ledgerCommitted)

	line := singleCorrelationLine(t, logbuf.String())
	if got := attrValue(line, "provenBroadcastId"); got != "proven-broadcast-9" {
		t.Fatalf("provenBroadcastId = %q, want the proven binding", got)
	}
	if got := attrValue(line, "grantBinding"); got != "GRANTED" {
		t.Errorf("grantBinding = %q, want GRANTED", got)
	}
	// Even with a proven broadcast, the streak count remains unmeasured.
	if got := attrValue(line, "provenStreakCount"); got != "UNKNOWN" {
		t.Errorf("provenStreakCount = %q, want UNKNOWN", got)
	}
}

// milestoneNodeFixture builds a RewardList body whose only variable is the
// missedStreams payload, so two fixtures differ in nothing but the node under
// test.
func milestoneNodeFixture(missedStreams string) string {
	return `{"data":{"channel":{"id":"12345","self":{"watchStreakMilestone":{` +
		`"state":"ACTIVE","missedStreams":` + missedStreams + `}}}}}`
}

// nodeClassification extracts ONLY the node-presence attributes of one
// observation record — never sequence, timestamps or identity — so two records
// can be compared for the classification they actually assert. Acceptance C:
// a difference in sequence or time is not evidence of a preserved distinction.
func nodeClassification(line string) map[string]string {
	out := map[string]string{}
	for _, key := range []string{
		"missedStreamsPresence", "missedStreamsCount", "missedStreamsMalformed",
		"missedStreamElements",
		"broadcastIdentifierArrays", "broadcastIdentifierElements", "broadcastIdentifierIds",
		"broadcastIdentifierValidIds", "broadcastIdentifierMalformed",
	} {
		out[key] = attrValue(line, key)
	}
	return out
}

// observeOnceWithMissedStreams runs one observation cycle against a fixture and
// returns the single emitted record line.
func observeOnceWithMissedStreams(t *testing.T, missedStreams string) string {
	t.Helper()
	logs := captureLogs(t)
	logins := milestoneLogins(t, 1)
	m, _ := newMilestoneMiner(t, &milestoneRoundTripper{
		rewardListBody: milestoneNodeFixture(missedStreams),
	}, logins, logins)
	logs.Reset()
	m.observeWatchStreakMilestones(context.Background())
	lines := recordLines(logs.String(), milestoneObservationRecord)
	if len(lines) != 1 {
		t.Fatalf("observation records = %d, want 1; logs:\n%s", len(lines), logs.String())
	}
	return lines[0]
}

// observeOnceWithMilestoneValue runs one observation cycle against a fixture
// whose ONLY variable is the raw JSON of the milestone value node, and returns
// the single emitted record line. Two fixtures therefore differ in nothing but
// the wire encoding under test.
func observeOnceWithMilestoneValue(t *testing.T, rawValue string) string {
	t.Helper()
	return observeOnceWithMilestoneNode(t, `"id":"m-1","value":`+rawValue)
}

// observeOnceWithMilestoneNode runs one observation cycle against a fixture
// whose only variable is the BODY of the nested milestone node, so a test can
// leave a key out entirely rather than only change its value.
func observeOnceWithMilestoneNode(t *testing.T, nodeBody string) string {
	t.Helper()
	logs := captureLogs(t)
	logins := milestoneLogins(t, 1)
	body := `{"data":{"channel":{"id":"12345","self":{"watchStreakMilestone":{` +
		`"state":"ACTIVE","watchStreakMilestone":{` + nodeBody + `}}}}}}`
	m, _ := newMilestoneMiner(t, &milestoneRoundTripper{rewardListBody: body}, logins, logins)
	logs.Reset()
	m.observeWatchStreakMilestones(context.Background())
	lines := recordLines(logs.String(), milestoneObservationRecord)
	if len(lines) != 1 {
		t.Fatalf("observation records = %d, want 1; logs:\n%s", len(lines), logs.String())
	}
	return lines[0]
}

// TestObservationReportsMilestoneValueWireKind is the MAJOR-3 fidelity proof
// asserted where it matters: the emitted record.
//
// Two things must both hold, and they pull in opposite directions:
//
//   - "value":"4" and "value":4 are the SAME observed integer, so a reader
//     comparing records across cycles must see the same 4 in both. Dropping
//     the string form as MALFORMED loses the observation entirely.
//   - They are NOT the same wire fact. Normalising them into one attribute and
//     stopping there would erase which encoding Twitch actually sent, which is
//     precisely the protocol evidence this diagnostic exists to collect.
//
// So the record carries the normalised integer AND its wire kind, separately.
//
// Written against attribute names only, so it compiles unchanged on the
// pre-repair candidate and fails there behaviourally.
func TestObservationReportsMilestoneValueWireKind(t *testing.T) {
	stringWire := observeOnceWithMilestoneValue(t, `"4"`)
	numberWire := observeOnceWithMilestoneValue(t, `4`)

	// Both encodings must survive as the same observed integer.
	if got := attrValue(stringWire, "milestoneValue"); got != "4" {
		t.Fatalf("string-encoded value:\"4\" reported as milestoneValue=%q, want 4 "+
			"- the observed value was lost", got)
	}
	if got := attrValue(numberWire, "milestoneValue"); got != "4" {
		t.Fatalf("integer-encoded value:4 reported as milestoneValue=%q, want 4", got)
	}

	// And the two wire kinds must stay told apart.
	if got := attrValue(stringWire, "milestoneValueWireKind"); got != "STRING" {
		t.Fatalf("string-encoded value reported wire kind %q, want STRING", got)
	}
	if got := attrValue(numberWire, "milestoneValueWireKind"); got != "NUMBER" {
		t.Fatalf("integer-encoded value reported wire kind %q, want NUMBER", got)
	}
}

// TestObservationDoesNotFabricateAWireKind proves the wire kind is an
// observation, not a default: a value node that was never validly read has no
// encoding to report, and must not borrow one.
func TestObservationDoesNotFabricateAWireKind(t *testing.T) {
	for _, tc := range []struct {
		name         string
		rawValue     string
		wantValue    string
		wantWireKind string
	}{
		{"null", `null`, "<NULL>", "<UNSET>"},
		{"non-numeric string", `"soon"`, "<MALFORMED>", "<UNSET>"},
		{"fractional number", `4.5`, "<MALFORMED>", "<UNSET>"},
		{"bool", `true`, "<MALFORMED>", "<UNSET>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line := observeOnceWithMilestoneValue(t, tc.rawValue)
			if got := attrValue(line, "milestoneValue"); got != tc.wantValue {
				t.Fatalf("milestoneValue = %q, want %q", got, tc.wantValue)
			}
			if got := attrValue(line, "milestoneValueWireKind"); got != tc.wantWireKind {
				t.Fatalf("milestoneValueWireKind = %q, want %q", got, tc.wantWireKind)
			}
		})
	}

	// A value key Twitch never sent at all is MISSING, and stays distinct from
	// the NULL and MALFORMED cases above.
	line := observeOnceWithMilestoneNode(t, `"id":"m-1"`)
	if got := attrValue(line, "milestoneValue"); got != "<MISSING>" {
		t.Fatalf("absent value key reported milestoneValue=%q, want <MISSING>", got)
	}
	if got := attrValue(line, "milestoneValueWireKind"); got != "<UNSET>" {
		t.Fatalf("absent value key reported milestoneValueWireKind=%q, want <UNSET>", got)
	}
}

// TestMilestoneLogWireKindNeverRendersAnEmptyValue covers the renderer's own
// contract rather than the paths the parser happens to produce today.
//
// A VALID integer that carries no wire kind cannot come out of the current
// parser, but the renderer must not depend on that: printing an empty
// milestoneValueWireKind= would read as "observed, and it was nothing", which
// is the one thing this attribute exists to prevent.
func TestMilestoneLogWireKindNeverRendersAnEmptyValue(t *testing.T) {
	for _, f := range []twitch.MilestoneIntField{
		{},
		{Presence: twitch.MilestoneFieldValid},
		{Presence: twitch.MilestoneFieldValid, Value: 4},
		{Presence: twitch.MilestoneFieldMissing},
		{Presence: twitch.MilestoneFieldNull},
		{Presence: twitch.MilestoneFieldMalformed},
		{Presence: twitch.MilestoneFieldMalformed, WireKind: twitch.MilestoneWireKindString},
		// WireKind is an exported string field, so the renderer must be closed
		// by construction: an encoding it does not know is not passed through
		// into a log attribute.
		{Presence: twitch.MilestoneFieldValid, Value: 4, WireKind: "\x1b[31mINJECTED"},
		{Presence: twitch.MilestoneFieldValid, Value: 4, WireKind: "number"},
	} {
		if got := milestoneLogWireKind(f); got != "<UNSET>" {
			t.Errorf("milestoneLogWireKind(%+v) = %q, want <UNSET>", f, got)
		}
	}
	for _, tc := range []struct {
		field twitch.MilestoneIntField
		want  string
	}{
		{twitch.MilestoneIntField{Presence: twitch.MilestoneFieldValid, Value: 4,
			WireKind: twitch.MilestoneWireKindString}, "STRING"},
		{twitch.MilestoneIntField{Presence: twitch.MilestoneFieldValid, Value: 4,
			WireKind: twitch.MilestoneWireKindNumber}, "NUMBER"},
	} {
		if got := milestoneLogWireKind(tc.field); got != tc.want {
			t.Errorf("milestoneLogWireKind(%+v) = %q, want %q", tc.field, got, tc.want)
		}
	}
}

// TestObservationStageIsBoundedByACycleBudget proves the diagnostic stage
// cannot run the bonus poll loop's goroutine for an unbounded time.
//
// The stage is a plain call on bonusPollLoop, so the loop cannot service its
// next tick until the stage returns. Without a bound, a slow RewardList is paid
// once per online target, serially, with the shared transport's whole retry
// schedule behind it - a diagnostic deferring the chest-claim fallback it has
// no business delaying.
//
// The fixture makes every RewardList take a fixed, uninterruptible slice of
// wall clock, so the only way to honour the budget is to stop starting new
// targets. The assertion is therefore on WORK ISSUED, not on elapsed time: a
// bounded stage must leave part of the roster unexamined rather than walk it
// all.
func TestObservationStageIsBoundedByACycleBudget(t *testing.T) {
	previousBudget := milestoneObservationCycleBudget
	milestoneObservationCycleBudget = 50 * time.Millisecond
	t.Cleanup(func() { milestoneObservationCycleBudget = previousBudget })

	const targets = 4
	logins := milestoneLogins(t, targets)
	rt := &milestoneRoundTripper{}
	// Each observation costs more than the whole budget, and the cost is NOT
	// cancellable, so the budget can only be honoured between targets.
	rt.onRewardList = func(int) { time.Sleep(120 * time.Millisecond) }
	m, _ := newMilestoneMiner(t, rt, logins, logins)

	m.observeWatchStreakMilestones(context.Background())

	_, reward := rt.counts()
	issued := 0
	for _, n := range reward {
		issued += n
	}
	if issued == 0 {
		t.Fatalf("no RewardList request was issued at all; the fixture proves nothing")
	}
	if issued >= targets {
		t.Fatalf("the stage issued %d of %d RewardList requests: it walked the whole roster "+
			"with the cycle budget already spent, so the bonus poll loop stays blocked "+
			"for as long as Twitch is slow", issued, targets)
	}
}

// TestObservationBudgetExhaustionIsRecorded proves a truncated roster is
// REPORTED rather than silently shortened.
//
// Absence is not evidence in this feature. A cycle that stops early must say so
// and say how much it did not look at, or a reader comparing cycles would read
// the missing records as "those streamers had nothing".
func TestObservationBudgetExhaustionIsRecorded(t *testing.T) {
	previousBudget := milestoneObservationCycleBudget
	milestoneObservationCycleBudget = 50 * time.Millisecond
	t.Cleanup(func() { milestoneObservationCycleBudget = previousBudget })

	logs := captureLogs(t)
	logins := milestoneLogins(t, 4)
	rt := &milestoneRoundTripper{}
	rt.onRewardList = func(int) { time.Sleep(120 * time.Millisecond) }
	m, _ := newMilestoneMiner(t, rt, logins, logins)
	logs.Reset()

	m.observeWatchStreakMilestones(context.Background())

	lines := recordLines(logs.String(), milestoneBudgetRecord)
	if len(lines) != 1 {
		t.Fatalf("budget-exhaustion records = %d, want exactly 1; logs:\n%s", len(lines), logs.String())
	}
	if got := attrValue(lines[0], "level"); got != "DEBUG" {
		t.Errorf("budget record level = %q, want DEBUG; it recurs on a degraded cycle", got)
	}
	unexamined := attrValue(lines[0], "unexaminedTargets")
	if unexamined == "" || unexamined == "0" {
		t.Errorf("unexaminedTargets = %q, want a positive count naming what was skipped", unexamined)
	}
	// An owner shutdown is NOT a budget exhaustion and must stay silent here.
	logs.Reset()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	m.observeWatchStreakMilestones(cancelled)
	if got := len(recordLines(logs.String(), milestoneBudgetRecord)); got != 0 {
		t.Errorf("an already-cancelled owner produced %d budget record(s); shutdown is not "+
			"budget exhaustion", got)
	}
}

// TestObservationDistinguishesNullElementFromNullChild is the R1 fidelity
// proof, asserted where it actually matters: the emitted observation record.
//
// Two DIFFERENT wire facts:
//
//	[null]                              — Twitch sent a null ELEMENT; that
//	                                      element has no broadcastIdentifiers
//	                                      node at all, so nothing about the
//	                                      child was ever observed.
//	[{"broadcastIdentifiers":null}]     — Twitch sent a well-formed element
//	                                      whose broadcastIdentifiers CHILD is
//	                                      explicitly null.
//
// They must not render identically. Encoding the element's own NULL into its
// child's presence slot is exactly the MISSING != NULL != EMPTY != VALID !=
// MALFORMED collapse this parser exists to prevent, moved one node up.
//
// This test is written against the record only — it names no struct field — so
// it compiles on the pre-repair candidate and fails there behaviourally.
func TestObservationDistinguishesNullElementFromNullChild(t *testing.T) {
	nullElement := nodeClassification(observeOnceWithMissedStreams(t, `[null]`))
	nullChild := nodeClassification(observeOnceWithMissedStreams(t, `[{"broadcastIdentifiers":null}]`))

	if reflect.DeepEqual(nullElement, nullChild) {
		t.Fatalf("a null ELEMENT and an element whose broadcastIdentifiers CHILD is null "+
			"produced identical node classifications — the two wire facts collapsed:\n"+
			"  [null]                          -> %v\n"+
			"  [{\"broadcastIdentifiers\":null}] -> %v", nullElement, nullChild)
	}
}

// TestObservationRecordCarriesPerNodeMissedStreamPresence is the consumer-side
// half of R3: the element/child distinction must survive all the way into the
// emitted record, not merely exist in a struct field.
//
// Expectations are specified from the WIRE SHAPE, independently of how the
// summary is implemented.
func TestObservationRecordCarriesPerNodeMissedStreamPresence(t *testing.T) {
	tests := []struct {
		name          string
		missedStreams string
		wantElements  string // tally over the ELEMENTS themselves
		wantArrays    string // tally over the child arrays of elements that had one
		wantIDs       string
		wantCount     string
		wantMalformed string
	}{
		{"null element", `[null]`, "NULL:1", "none", "none", "1", "0"},
		{"null child", `[{"broadcastIdentifiers":null}]`, "VALID:1", "NULL:1", "none", "1", "0"},
		{"absent child", `[{}]`, "VALID:1", "MISSING:1", "none", "1", "0"},
		{"empty child", `[{"broadcastIdentifiers":[]}]`, "VALID:1", "EMPTY:1", "none", "1", "0"},
		{"malformed element", `["nope"]`, "MALFORMED:1", "none", "none", "1", "1"},
		{
			"valid entry", `[{"broadcastIdentifiers":[{"id":"b-1"}]}]`,
			"VALID:1", "VALID:1", "VALID:1", "1", "0",
		},
		{
			"mixed list",
			`[null,{"broadcastIdentifiers":null},{"broadcastIdentifiers":[{"id":"x"}]},"nope",{}]`,
			"VALID:3,NULL:1,MALFORMED:1", "VALID:1,MISSING:1,NULL:1", "VALID:1", "5", "1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := nodeClassification(observeOnceWithMissedStreams(t, tc.missedStreams))
			for key, want := range map[string]string{
				"missedStreamElements":      tc.wantElements,
				"broadcastIdentifierArrays": tc.wantArrays,
				"broadcastIdentifierIds":    tc.wantIDs,
				"missedStreamsCount":        tc.wantCount,
				"missedStreamsMalformed":    tc.wantMalformed,
			} {
				if got[key] != want {
					t.Errorf("%s = %q, want %q\nfull: %v", key, got[key], want, got)
				}
			}
		})
	}

	// Pairwise: no two of these distinct wire shapes may render alike.
	seen := map[string]string{}
	for _, tc := range tests {
		key := fmt.Sprint(nodeClassification(observeOnceWithMissedStreams(t, tc.missedStreams)))
		if prev, dup := seen[key]; dup {
			t.Errorf("%q and %q render identical node classifications: %s", prev, tc.missedStreams, key)
		}
		seen[key] = tc.missedStreams
	}
}

// attrCapture is a slog.Handler that keeps each record's attributes as VALUES,
// before any TextHandler formatting or escaping. It is the pre-format
// observation boundary named as a pre-agreed test seam: assertions made here
// see exactly what the code put into the record, not what the renderer chose to
// display.
type attrCapture struct {
	mu      sync.Mutex
	records []map[string]string
}

func (h *attrCapture) Enabled(context.Context, slog.Level) bool { return true }
func (h *attrCapture) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *attrCapture) WithGroup(string) slog.Handler            { return h }

func (h *attrCapture) Handle(_ context.Context, r slog.Record) error {
	rec := map[string]string{"msg": r.Message, "level": r.Level.String()}
	r.Attrs(func(a slog.Attr) bool {
		rec[a.Key] = a.Value.String()
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, rec)
	h.mu.Unlock()
	return nil
}

// recordsFor returns the captured records whose "record" attribute matches.
func (h *attrCapture) recordsFor(kind string) []map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []map[string]string
	for _, r := range h.records {
		if r["record"] == kind {
			out = append(out, r)
		}
	}
	return out
}

// captureAttrs redirects slog to an attrCapture for the duration of the test.
func captureAttrs(t *testing.T) *attrCapture {
	t.Helper()
	previous := slog.Default()
	h := &attrCapture{}
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return h
}

// TestObservationAttributesAreSanitizedBeforeFormatting is the R7 repair.
//
// The fixture is VALID JSON carrying \\u-escaped control characters, so the
// values become hostile bytes only AFTER decoding — the state the record
// builder actually sees. Assertions are made on the attribute VALUES before
// slog formats them, because the TextHandler escapes control bytes on render
// and would hide a removed sanitizer behind its own output encoding.
func TestObservationAttributesAreSanitizedBeforeFormatting(t *testing.T) {
	// Decodes to ESC, NUL, BEL, LF, CR, TAB followed by printable text.
	const hostile = `\u001b\u0000\u0007\u000a\u000d\u0009hostile`
	body := `{"data":{"channel":{"id":"` + hostile + `","self":{"watchStreakMilestone":{` +
		`"state":"` + hostile + `","expiresAt":"` + hostile + `",` +
		`"missedStreams":[{"broadcastIdentifiers":[{"id":"` + hostile + `"}]}],` +
		`"watchStreakMilestone":{"id":"` + hostile + `","achievementTimestamp":"` + hostile + `",` +
		// value is string-accepting now, so it is a wire-string attack surface
		// like any other and must be in this fixture. A hostile string fails
		// strconv.Atoi and renders as <MALFORMED>, which is itself the
		// assertion: the raw bytes must never reach the attribute.
		`"value":"` + hostile + `",` +
		`"shareStatus":"` + hostile + `"}}}}}}`

	// The fixture must be valid JSON AND must actually decode to control bytes,
	// or this test would prove nothing about sanitization.
	var probe map[string]interface{}
	if err := json.Unmarshal([]byte(body), &probe); err != nil {
		t.Fatalf("fixture is not valid JSON: %v", err)
	}
	decoded := probe["data"].(map[string]interface{})["channel"].(map[string]interface{})["id"].(string)
	if !strings.ContainsAny(decoded, "\x00\x1b\x07\n\r\t") {
		t.Fatalf("fixture does not decode to control characters: %q", decoded)
	}

	attrs := captureAttrs(t)
	logins := milestoneLogins(t, 1)
	m, _ := newMilestoneMiner(t, &milestoneRoundTripper{rewardListBody: body}, logins, logins)
	m.observeWatchStreakMilestones(context.Background())

	records := attrs.recordsFor(milestoneObservationRecord)
	if len(records) != 1 {
		t.Fatalf("observation records = %d, want 1", len(records))
	}
	rec := records[0]

	// The hostile values genuinely reached the record — otherwise the loop
	// below would pass vacuously over a record that observed nothing.
	if rec["outcome"] != "OBSERVED" || rec["observedChannelId"] == "" {
		t.Fatalf("fixture did not produce an observed record: %v", rec)
	}

	// Every attribute VALUE the code produced is already free of control
	// characters and valid UTF-8, proven pre-format so the renderer cannot mask
	// a bypass.
	const control = "\x00\x01\x02\x03\x04\x05\x06\x07\x08\t\n\v\f\r\x1b"
	for key, value := range rec {
		if strings.ContainsAny(value, control) {
			t.Errorf("attribute %q reached the record with raw control characters: %q", key, value)
		}
		if !utf8.ValidString(value) {
			t.Errorf("attribute %q reached the record as invalid UTF-8: %q", key, value)
		}
	}
	// The value node reaches the record through a DIFFERENT defence from every
	// other wire string here, and the difference is worth asserting rather than
	// leaving to the generic loop above. It is string-accepting now, but the
	// only strings it accepts are runs of decimal digits, so hostile bytes
	// cannot survive the parse at all: they are refused, not sanitized. Naming
	// the expected tokens keeps this from passing for the wrong reason - if the
	// field ever started echoing what it was given, <MALFORMED> would not be
	// what came back.
	if rec["milestoneValue"] != "<MALFORMED>" {
		t.Errorf("hostile value rendered as %q, want <MALFORMED>", rec["milestoneValue"])
	}
	if rec["milestoneValueWireKind"] != "<UNSET>" {
		t.Errorf("hostile value reported wire kind %q, want <UNSET>", rec["milestoneValueWireKind"])
	}

	// Sanitization REPLACES rather than deletes: printable content survives and
	// the substituted bytes are visibly marked.
	if !strings.Contains(rec["observedChannelId"], "hostile") {
		t.Errorf("sanitization dropped printable content: %q", rec["observedChannelId"])
	}
	if !strings.Contains(rec["observedChannelId"], string(utf8.RuneError)) {
		t.Errorf("sanitization did not mark the replaced bytes: %q", rec["observedChannelId"])
	}
	// The bound still holds on the decoded values, independently of render.
	for _, key := range []string{"observedChannelId", "state", "expiresAt", "milestoneId", "achievementTimestamp"} {
		if len(rec[key]) > milestoneLogStringCap+len("...(truncated)") {
			t.Errorf("attribute %q exceeded the bound: %d bytes", key, len(rec[key]))
		}
	}
}

// TestObservationDistinguishesNullIdentifierElementFromNullId is the same
// fidelity claim as TestObservationDistinguishesNullElementFromNullChild, one
// node further down.
//
//	[{"broadcastIdentifiers":[null]}]        — a null ELEMENT of the identifier
//	                                           array; it has no id node at all.
//	[{"broadcastIdentifiers":[{"id":null}]}] — a well-formed element whose id
//	                                           node is explicitly null.
//
// The standing contract requires the presence vocabulary to be preserved at
// EVERY node including array elements, so these two wire facts must not render
// identically. Writing the element's own NULL into the id's slot is the same
// collapse, just one level deeper.
func TestObservationDistinguishesNullIdentifierElementFromNullId(t *testing.T) {
	nullElement := nodeClassification(observeOnceWithMissedStreams(t, `[{"broadcastIdentifiers":[null]}]`))
	nullID := nodeClassification(observeOnceWithMissedStreams(t, `[{"broadcastIdentifiers":[{"id":null}]}]`))

	if reflect.DeepEqual(nullElement, nullID) {
		t.Fatalf("a null identifier ELEMENT and an element whose id is null produced identical node "+
			"classifications — the two wire facts collapsed:\n"+
			"  [{\"broadcastIdentifiers\":[null]}]        -> %v\n"+
			"  [{\"broadcastIdentifiers\":[{\"id\":null}]}] -> %v", nullElement, nullID)
	}
}

// TestCorrelationAttributesAreSanitizedBeforeFormatting closes the second half
// of the R7 gap: the sanitizer seam covered the observation record only, while
// the grant-correlation record carries wire-controlled values of its own
// (Twitch's data.timestamp, the event identity, and the broadcast strings) that
// no test bounded.
//
// Same seam and same reasoning: assertions are made on attribute VALUES before
// slog formats them, because the TextHandler escapes control bytes on render
// and would hide a bypass behind its own output encoding.
func TestCorrelationAttributesAreSanitizedBeforeFormatting(t *testing.T) {
	const hostile = `\u001b\u0000\u0007\u000a\u000d\u0009hostile`

	decoded := hostileDecoded(t, hostile)

	attrs := captureAttrs(t)
	m, s, _ := newMilestoneCorrelationMiner(t, &milestoneRoundTripper{})
	// Wire-controlled strings on the correlation path: the broadcast identity
	// the record carries as context, and Twitch's own event timestamp. The
	// frame is parsed through the real layer first and its timestamp is then
	// replaced with the decoded hostile value — the parse helper renders the
	// payload with %q, which cannot express a control byte as valid JSON, and
	// the record reads Data["timestamp"] rather than the raw text anyway.
	s.Stream.Update(decoded, "title", nil, nil, 1)
	msg := watchStreakFrame(t, s, 450, 5450, "2026-09-05T12:00:00Z")
	msg.Data["timestamp"] = decoded
	deliverPointsEarned(t, m, s, msg)

	records := attrs.recordsFor(milestoneCorrelationRecord)
	if len(records) != 1 {
		t.Fatalf("correlation records = %d, want 1", len(records))
	}
	rec := records[0]

	const control = "\x00\x01\x02\x03\x04\x05\x06\x07\x08\t\n\v\f\r\x1b"
	for key, value := range rec {
		if strings.ContainsAny(value, control) {
			t.Errorf("attribute %q reached the correlation record with raw control characters: %q", key, value)
		}
		if !utf8.ValidString(value) {
			t.Errorf("attribute %q reached the correlation record as invalid UTF-8: %q", key, value)
		}
	}
	// The wire-controlled attributes are bounded, not merely sanitized.
	for _, key := range []string{"wireTimestamp", "localBroadcastContextOnly", "eventId", "streamer", "channelId"} {
		if _, ok := rec[key]; !ok {
			t.Errorf("correlation record does not emit %q at all", key)
			continue
		}
		if len(rec[key]) > milestoneLogStringCap+len("...(truncated)") {
			t.Errorf("attribute %q exceeded the bound: %d bytes", key, len(rec[key]))
		}
	}
	// And the hostile content genuinely reached the record, so the loop above
	// is not passing over an empty one.
	if !strings.Contains(rec["wireTimestamp"], "hostile") {
		t.Errorf("fixture did not reach wireTimestamp: %q", rec["wireTimestamp"])
	}
	if !strings.Contains(rec["wireTimestamp"], string(utf8.RuneError)) {
		t.Errorf("wireTimestamp was not sanitized: %q", rec["wireTimestamp"])
	}
}

// hostileDecoded turns a JSON-escaped fixture into the decoded string a caller
// would actually hold, so the hostile bytes exist only after decoding.
func hostileDecoded(t *testing.T, escaped string) string {
	t.Helper()
	var out string
	if err := json.Unmarshal([]byte(`"`+escaped+`"`), &out); err != nil {
		t.Fatalf("hostile fixture is not valid JSON: %v", err)
	}
	if !strings.ContainsAny(out, "\x00\x1b\n") {
		t.Fatalf("hostile fixture did not decode to control characters: %q", out)
	}
	return out
}
