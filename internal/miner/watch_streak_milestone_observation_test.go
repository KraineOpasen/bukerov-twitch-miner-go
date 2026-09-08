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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/auth"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
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

	// rewardOrder is every RewardList channelID in dispatch order, so a test
	// can assert the roster ORDER the cursor produced, not only per-target
	// counts.
	rewardOrder []string

	// rewardListRespond, when set, decides the HTTP status and body of one
	// RewardList answer from its channelID and the running RewardList count
	// (1-based). A zero status keeps the default answer; an empty body keeps
	// the default body for that status.
	rewardListRespond func(channelID string, n int) (status int, body string)

	// contextBody, when set, replaces the default ChannelPointsContext response
	// body — the BUSINESS read, used to prove the diagnostic-only suppressions
	// do not leak onto it.
	contextBody string

	// contextDelay, when set, holds every ChannelPointsContext answer for that
	// long, so a test can make the BUSINESS pass consume the whole poll period.
	contextDelay time.Duration

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

// rewardSequence returns the RewardList channelIDs in dispatch order.
func (rt *milestoneRoundTripper) rewardSequence() []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return append([]string(nil), rt.rewardOrder...)
}

// rewardTotal returns how many RewardList requests reached the transport.
func (rt *milestoneRoundTripper) rewardTotal() int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return len(rt.rewardOrder)
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
	rewardChannelID := ""
	if operation.Name == "RewardList" {
		if rt.rewardBy == nil {
			rt.rewardBy = map[string]int{}
		}
		rewardChannelID, _ = operation.Variables["channelID"].(string)
		rt.rewardBy[rewardChannelID]++
		rt.rewardOrder = append(rt.rewardOrder, rewardChannelID)
		rewardCount = len(rt.rewardOrder)
	}
	hook := rt.onRewardList
	respond := rt.rewardListRespond
	rewardBody := rt.rewardListBody
	contextBody := rt.contextBody
	contextDelay := rt.contextDelay
	offerClaim := rt.offerClaim
	rt.mu.Unlock()

	status := http.StatusOK
	response := `{"data":{}}`
	switch operation.Name {
	case "ChannelPointsContext":
		if contextDelay > 0 {
			time.Sleep(contextDelay)
		}
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
		if respond != nil {
			if st, body := respond(rewardChannelID, rewardCount); st != 0 {
				status = st
				if body != "" {
					response = body
				}
			}
		}
	}
	return &http.Response{
		StatusCode: status,
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

// milestoneCycleNow builds the cycle bonusPollLoop would hand a stage that is
// serviced right now: a tick stamped now and the production period, so the
// next-tick bound leaves the whole stage budget as slack and the test's own
// budget/transport fixture is what decides the outcome. Tests that need a
// specific tick, period, overdue cycle or non-zero cursor build the
// milestoneCycle directly.
func milestoneCycleNow(cursor int) milestoneCycle {
	return milestoneCycle{Tick: time.Now(), Period: bonusPollInterval, Cursor: cursor}
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

	m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(0))

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
	logs := captureLogs(t)
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
	// Long enough that a cycle's business pass leaves the observation stage
	// real slack before the next tick (D1 yields on a consumed period), short
	// enough that two cycles complete well inside the bound below.
	bonusPollInterval = 500 * time.Millisecond
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
		t.Fatalf("the bonus poll loop did not reach a second RewardList observation within 30s; logs:\n%s", logs.String())
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
	m.observeWatchStreakMilestones(ctx, milestoneCycleNow(0))

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

	m.observeWatchStreakMilestones(ctx, milestoneCycleNow(0))

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

	m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(0))

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

	m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(0))

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

			m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(0))

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

		m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(0))

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

	m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(0))

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

	m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(0))
	m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(0))

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

	m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(0))

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
	m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(0))
	previousLogs := logbuf.String()
	previous := recordLines(previousLogs, milestoneObservationRecord)
	if len(previous) != 1 {
		t.Fatalf("previous observation records = %d, want 1", len(previous))
	}

	// EVENT (delivered later, with a milestone value that did not change)
	msg := watchStreakFrame(t, s, 450, 5450, "2026-09-05T12:00:00Z")
	deliverPointsEarned(t, m, s, msg)

	// FOLLOWING OBSERVATION
	m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(0))

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

	m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(0))

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
	m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(0))

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

	m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(0))

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

	// The stage's OWN half of the rule, not merely the client's refusal: an
	// ID-less online entry is not ELIGIBLE, so it never counts toward what the
	// cycle can examine. With exactly the allowance's worth of identifiable
	// targets ahead of it, a mask that counted it would reach it with the
	// allowance spent and write a spurious COUNT record.
	t.Run("an ID-less entry is not counted as eligible", func(t *testing.T) {
		logs := captureLogs(t)
		logins := milestoneLogins(t, milestoneCycleDispatchAllowance+1)
		rt := &milestoneRoundTripper{}
		m, streamers := newMilestoneMiner(t, rt, logins, logins)
		streamers[logins[len(logins)-1]].ChannelID = ""
		logs.Reset()

		next := m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(0))

		if got := rt.rewardTotal(); got != milestoneCycleDispatchAllowance {
			t.Fatalf("dispatches = %d, want %d", got, milestoneCycleDispatchAllowance)
		}
		if got := len(recordLines(logs.String(), milestoneBudgetRecord)); got != 0 {
			t.Fatalf("an ID-less online entry produced %d budget record(s); it is not eligible, so the roster "+
				"was finished:\n%s", got, logs.String())
		}
		if next != len(logins)-1 {
			t.Errorf("cursor = %d, want %d (after the last started target)", next, len(logins)-1)
		}
	})

	// And on a cycle that IS cut on COUNT, the ID-less entry is excluded from
	// the unexamined count, which is defined over eligible targets only.
	t.Run("an ID-less entry is excluded from the unexamined count", func(t *testing.T) {
		logs := captureLogs(t)
		logins := milestoneLogins(t, milestoneCycleDispatchAllowance+2)
		rt := &milestoneRoundTripper{}
		m, streamers := newMilestoneMiner(t, rt, logins, logins)
		streamers[logins[1]].ChannelID = "" // sits between started targets
		logs.Reset()

		m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(0))

		budget := recordLines(logs.String(), milestoneBudgetRecord)
		if len(budget) != 1 {
			t.Fatalf("budget records = %d, want 1 (one eligible target refused on COUNT)", len(budget))
		}
		if got := attrValue(budget[0], "unexaminedTargets"); got != "1" {
			t.Errorf("unexaminedTargets = %q, want 1: the ID-less entry is not eligible and must not be counted", got)
		}
	})
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
	m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(0))

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
	m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(0))
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
	m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(0))
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

// stallingTransport models a Twitch that accepts the connection and never
// answers: it releases only when the request context is done.
//
// It counts its requests and hands back one token per request it released that
// way, so a test can prove the fixture actually STALLED rather than assume it.
// An earlier version had no such seam, and the test below passed against a
// transport that failed instantly - it was asserting an outcome many paths
// reach, not the stall it named.
type stallingTransport struct {
	mu       sync.Mutex
	attempts int
	released chan struct{}
}

func newStallingTransport() *stallingTransport {
	return &stallingTransport{released: make(chan struct{}, 64)}
}

func (s *stallingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.attempts++
	s.mu.Unlock()

	<-req.Context().Done()

	select {
	case s.released <- struct{}{}:
	default:
	}
	return nil, req.Context().Err()
}

func (s *stallingTransport) requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
}

// TestAStalledTwitchIsRecordedAsOurDeadlineNotTheTransportsTimeout pins the
// MINER-LAYER consequence of the classification an earlier version of the
// budget's own comment got backwards.
//
// That comment claimed the budget is longer than the transport's 30s HTTP
// timeout so a genuine stall still surfaces as TRANSPORT_TIMEOUT rather than
// being masked by our deadline. It is false, and the reason is the retry
// contract: a Client.Timeout error carries status code 0, which the shared
// transport classifies as TRANSIENT, so the first timeout is never returned to
// this caller — it is retried. The budget expires during a later attempt and
// the stage sees its OWN context's error.
//
// What this test does and does not cover, stated exactly, because an earlier
// version of this comment overclaimed:
//
//   - It DOES prove that at this layer a stalled Twitch is ended by the stage's
//     own budget and recorded as our deadline, at roughly the budget rather
//     than at the transport's 30s timeout.
//   - It does NOT exercise the timeout-is-retried mechanism itself. The client
//     timeout is a hard-coded 30s that this package cannot shorten, so with a
//     test-sized budget the stage is cancelled during attempt 1 and no timeout
//     ever fires. Measured on the version that claimed otherwise: 1 attempt.
//     That mechanism is pinned where it lives, by
//     TestAClientTimeoutIsRetriedRatherThanReturned in internal/twitch.
//
// The fixture is asserted, not assumed: an earlier version of this test passed
// against a transport that failed INSTANTLY, because a fast failure is also
// retried and also ends at the budget. The released-token handshake below is
// what makes the word "stalled" in the test name mean something.
func TestAStalledTwitchIsRecordedAsOurDeadlineNotTheTransportsTimeout(t *testing.T) {
	const budget = 200 * time.Millisecond

	previousBudget := milestoneObservationCycleBudget
	milestoneObservationCycleBudget = budget
	t.Cleanup(func() { milestoneObservationCycleBudget = previousBudget })

	m, _ := newMilestoneMiner(t, &milestoneRoundTripper{}, []string{"alpha"}, []string{"alpha"})

	// A real stall, which is not the same as a slow answer: never respond, and
	// release only when the request context is done. A transport that merely
	// sleeps and then answers is NOT this - it returns a response the client
	// accepts, and the observation succeeds. That distinction is why this uses
	// its own transport rather than the shared fixture's timing hook.
	stall := newStallingTransport()
	previousTransport := http.DefaultTransport
	http.DefaultTransport = stall
	t.Cleanup(func() { http.DefaultTransport = previousTransport })

	buf := captureLogs(t)
	start := time.Now()
	m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(0))
	elapsed := time.Since(start)
	out := buf.String()

	// 1. The fixture stalled. Every request it took was released by context
	// cancellation, never by an answer of its own.
	issued := stall.requests()
	if issued == 0 {
		t.Fatalf("the stalling transport was never reached, so this test proves nothing. Log:\n%s", out)
	}
	for i := 0; i < issued; i++ {
		select {
		case <-stall.released:
		case <-time.After(5 * time.Second):
			t.Fatalf("request %d of %d was not released by context cancellation: the fixture did "+
				"not stall, so a passing assertion below would be about something else", i+1, issued)
		}
	}

	// 2. Our budget is what ended it, not the transport's 30s timeout.
	if elapsed >= 30*time.Second {
		t.Fatalf("the stage took %v: the transport's own timeout ended this, not the cycle budget", elapsed)
	}
	if elapsed < budget {
		t.Fatalf("the stage returned in %v, before its %v budget: it did not wait on the stall at all",
			elapsed, budget)
	}

	// 3. And the record names our deadline rather than the transport's.
	if strings.Contains(out, "TRANSPORT_TIMEOUT") {
		t.Fatalf("the stage recorded TRANSPORT_TIMEOUT for a stall stopped by its own budget; "+
			"if that is now genuinely reachable here, the budget comment and SPECIFICATIONS.md "+
			"need rewriting with it. Log:\n%s", out)
	}
	if !strings.Contains(out, "DEADLINE_EXCEEDED") && !strings.Contains(out, "CANCELLED") {
		t.Fatalf("expected the stage's own deadline to be what the record names. Log:\n%s", out)
	}
}

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

	m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(0))

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

	m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(0))

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
	m.observeWatchStreakMilestones(cancelled, milestoneCycleNow(0))
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
	m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(0))

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

// ---------------------------------------------------------------------------
// D1 (business-first deadline), D2 (roster cursor), D4 (cycle allowance).
// ---------------------------------------------------------------------------

// milestonePQNFBody is the structured APQ rejection the diagnostic read accepts
// as evidence; every candidate client ID answering it costs one dispatch each.
const milestonePQNFBody = `{"errors":[{"message":"PersistedQueryNotFound","extensions":{"code":"PERSISTED_QUERY_NOT_FOUND"}}]}`

// milestoneChannelIDs maps logins to the channel IDs the fixture resolves them
// to, in roster order.
func milestoneChannelIDs(streamers map[string]*models.Streamer, logins ...string) []string {
	out := make([]string, 0, len(logins))
	for _, login := range logins {
		out = append(out, streamers[login].ChannelID)
	}
	return out
}

func assertRewardSequence(t *testing.T, rt *milestoneRoundTripper, want []string) {
	t.Helper()
	got := rt.rewardSequence()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RewardList dispatch order = %v, want %v", got, want)
	}
}

// TestMilestoneStageDeadlineIsTheEarliestBound pins the D1 arithmetic with
// literals and no sleeping: the stage may run until the earliest of its own
// budget, the next business tick, and the owner's deadline; a cycle whose
// business pass consumed the period, or an overdue tick, has no slack.
func TestMilestoneStageDeadlineIsTheEarliestBound(t *testing.T) {
	if milestoneObservationCycleBudget != 40*time.Second || bonusPollInterval != 60*time.Second {
		t.Fatalf("production constants moved (budget %v, period %v); update the table", milestoneObservationCycleBudget, bonusPollInterval)
	}
	tick := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	const period = 60 * time.Second

	tests := []struct {
		name       string
		business   time.Duration // how long the business pass took after the tick
		tickLate   time.Duration // how late the serviced tick itself was read (coalesced)
		owner      time.Duration // owner deadline after the tick; 0 = none
		wantSource milestoneDeadlineSource
		wantSlack  time.Duration
	}{
		{"business 10s: own budget binds", 10 * time.Second, 0, 0, milestoneDeadlineStageBudget, 40 * time.Second},
		{"business 20s: tie keeps the earlier-listed source", 20 * time.Second, 0, 0, milestoneDeadlineStageBudget, 40 * time.Second},
		{"business 30s: next tick binds, 30s left", 30 * time.Second, 0, 0, milestoneDeadlineNextTick, 30 * time.Second},
		{"business 59s: 1s left", 59 * time.Second, 0, 0, milestoneDeadlineNextTick, 1 * time.Second},
		{"business 60s: period consumed, no slack", 60 * time.Second, 0, 0, milestoneDeadlineNextTick, 0},
		{"business 70s: overdue, negative slack, no fresh budget", 70 * time.Second, 0, 0, milestoneDeadlineNextTick, -10 * time.Second},
		{"coalesced tick read two periods late", 5 * time.Second, 2 * period, 0, milestoneDeadlineNextTick, -65 * time.Second},
		{"earlier owner deadline binds", 10 * time.Second, 0, 15 * time.Second, milestoneDeadlineOwner, 5 * time.Second},
		{"later owner deadline does not", 10 * time.Second, 0, 5 * time.Minute, milestoneDeadlineStageBudget, 40 * time.Second},
		// The owner comparison is strict too: an owner deadline exactly equal
		// to the binding bound does not take the name.
		{"owner tie with the stage budget keeps STAGE_BUDGET", 10 * time.Second, 0, 50 * time.Second, milestoneDeadlineStageBudget, 40 * time.Second},
		{"owner tie with the next tick keeps NEXT_TICK", 30 * time.Second, 0, 60 * time.Second, milestoneDeadlineNextTick, 30 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			owner := context.Context(context.Background())
			if tc.owner != 0 {
				var stop context.CancelFunc
				owner, stop = context.WithDeadline(context.Background(), tick.Add(tc.owner))
				defer stop()
			}
			stageStart := tick.Add(tc.tickLate).Add(tc.business)
			deadline, source := milestoneStageDeadline(owner, tick, period, stageStart)
			if source != tc.wantSource {
				t.Errorf("source = %q, want %q", source, tc.wantSource)
			}
			if got := deadline.Sub(stageStart); got != tc.wantSlack {
				t.Errorf("slack = %v, want %v", got, tc.wantSlack)
			}
		})
	}
}

// TestANoSlackCycleAdmitsNoDiagnosticRequest runs the real stage on a cycle
// whose period is already consumed or overdue: zero dispatches, the target
// that was due keeps its turn, and the record says the cycle was cut on TIME
// by the next tick with nothing started.
func TestANoSlackCycleAdmitsNoDiagnosticRequest(t *testing.T) {
	for name, late := range map[string]time.Duration{
		"period exactly consumed": 0,
		"overdue by a period":     bonusPollInterval,
		"overdue by many periods": 5 * bonusPollInterval,
	} {
		t.Run(name, func(t *testing.T) {
			logs := captureLogs(t)
			logins := milestoneLogins(t, 3)
			rt := &milestoneRoundTripper{}
			m, _ := newMilestoneMiner(t, rt, logins, logins)
			logs.Reset()

			// The serviced tick was due one full period (plus `late`) ago, as a
			// business pass that ran that long would leave it.
			cycle := milestoneCycle{Tick: time.Now().Add(-bonusPollInterval - late), Period: bonusPollInterval, Cursor: 1}
			next := m.observeWatchStreakMilestones(context.Background(), cycle)

			if got := rt.rewardTotal(); got != 0 {
				t.Fatalf("a no-slack cycle dispatched %d RewardList request(s); want 0", got)
			}
			if next != 1 {
				t.Errorf("cursor = %d, want 1: nothing started, so the due target keeps its turn", next)
			}
			if got := len(recordLines(logs.String(), milestoneObservationRecord)); got != 0 {
				t.Errorf("%d observation record(s) for a cycle that observed nothing", got)
			}
			lines := recordLines(logs.String(), milestoneBudgetRecord)
			if len(lines) != 1 {
				t.Fatalf("budget records = %d, want 1; logs:\n%s", len(lines), logs.String())
			}
			for key, want := range map[string]string{
				"cutoff": "TIME", "deadlineSource": "NEXT_TICK", "startedTargets": "0",
				"unexaminedTargets": "3", "dispatchesSpent": "0", "dispatchesRemaining": "3",
			} {
				if got := attrValue(lines[0], key); got != want {
					t.Errorf("%s = %q, want %q; line: %s", key, got, want, lines[0])
				}
			}
			slack, err := strconv.Atoi(attrValue(lines[0], "slackMs"))
			if err != nil || slack > 0 {
				t.Errorf("slackMs = %q, want a non-positive integer", attrValue(lines[0], "slackMs"))
			}
		})
	}

	t.Run("nothing eligible: no record either", func(t *testing.T) {
		logs := captureLogs(t)
		logins := milestoneLogins(t, 2)
		rt := &milestoneRoundTripper{}
		m, _ := newMilestoneMiner(t, rt, logins, nil) // all offline
		logs.Reset()
		next := m.observeWatchStreakMilestones(context.Background(),
			milestoneCycle{Tick: time.Now().Add(-2 * bonusPollInterval), Period: bonusPollInterval, Cursor: 1})
		if got := len(recordLines(logs.String(), milestoneBudgetRecord)); got != 0 || rt.rewardTotal() != 0 || next != 1 {
			t.Fatalf("budget records %d, dispatches %d, cursor %d; want 0/0/1", got, rt.rewardTotal(), next)
		}
	})
}

// TestACycleSpendsAtMostThreeDispatchesWhateverTheRosterSize is the D4 bound
// under the worst realistic shape: every target answers PersistedQueryNotFound,
// so a single target's complete client-ID traversal costs the whole allowance.
// N=1, N=10 and N=100 all cost exactly three dispatches per cycle.
func TestACycleSpendsAtMostThreeDispatchesWhateverTheRosterSize(t *testing.T) {
	for _, n := range []int{1, 10, 100} {
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			logs := captureLogs(t)
			logins := milestoneLogins(t, n)
			rt := &milestoneRoundTripper{rewardListBody: milestonePQNFBody}
			m, streamers := newMilestoneMiner(t, rt, logins, logins)
			logs.Reset()

			next := m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(0))

			if got := rt.rewardTotal(); got != milestoneCycleDispatchAllowance {
				t.Fatalf("N=%d: RewardList dispatches = %d, want exactly %d for the whole cycle", n, got, milestoneCycleDispatchAllowance)
			}
			first := streamers[logins[0]].ChannelID
			assertRewardSequence(t, rt, []string{first, first, first})

			obs := recordLines(logs.String(), milestoneObservationRecord)
			if len(obs) != 1 {
				t.Fatalf("observation records = %d, want 1 (only the first target was started)", len(obs))
			}
			if got := attrValue(obs[0], "outcome"); got != string(twitch.MilestoneUnsupported) {
				t.Errorf("outcome = %q, want UNSUPPORTED_QUERY: the traversal was COMPLETE, so the classification holds", got)
			}
			if got := attrValue(obs[0], "dispatches"); got != "3" {
				t.Errorf("dispatches = %q, want 3", got)
			}

			budget := recordLines(logs.String(), milestoneBudgetRecord)
			if n == 1 {
				if len(budget) != 0 || next != 0 {
					t.Fatalf("N=1: budget records %d, cursor %d; want none and a wrap to 0", len(budget), next)
				}
				return
			}
			if len(budget) != 1 {
				t.Fatalf("budget records = %d, want 1", len(budget))
			}
			for key, want := range map[string]string{
				"cutoff": "COUNT", "startedTargets": "1", "unexaminedTargets": strconv.Itoa(n - 1),
				"dispatchesSpent": "3", "dispatchesRemaining": "0",
			} {
				if got := attrValue(budget[0], key); got != want {
					t.Errorf("%s = %q, want %q", key, got, want)
				}
			}
			if next != 1 {
				t.Errorf("cursor = %d, want 1: the next cycle starts after the one target that was started", next)
			}
		})
	}
}

// TestMixedOutcomesShareOneAllowance: a success, a target that needs a second
// dispatch (client-ID fallback or a transient retry) and the target after them
// all draw on the SAME three permits, so the third target is refused before
// its first dispatch and keeps its turn.
func TestMixedOutcomesShareOneAllowance(t *testing.T) {
	cases := map[string]func(channelID string, n int) (int, string){
		"success then APQ fallback success": func(_ string, n int) (int, string) {
			if n == 2 {
				return http.StatusOK, milestonePQNFBody // first candidate rejected; the fallback (n=3) succeeds
			}
			return 0, ""
		},
		"success then transient retry success": func(_ string, n int) (int, string) {
			if n == 2 {
				return http.StatusServiceUnavailable, "" // retried after one backoff; n=3 succeeds
			}
			return 0, ""
		},
	}
	for name, respond := range cases {
		t.Run(name, func(t *testing.T) {
			logs := captureLogs(t)
			logins := milestoneLogins(t, 5)
			rt := &milestoneRoundTripper{rewardListRespond: respond}
			m, streamers := newMilestoneMiner(t, rt, logins, logins)
			logs.Reset()

			next := m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(0))

			ids := milestoneChannelIDs(streamers, logins...)
			assertRewardSequence(t, rt, []string{ids[0], ids[1], ids[1]})
			obs := recordLines(logs.String(), milestoneObservationRecord)
			if len(obs) != 2 {
				t.Fatalf("observation records = %d, want 2; logs:\n%s", len(obs), logs.String())
			}
			for i, want := range []string{"1", "2"} {
				if got := attrValue(obs[i], "outcome"); got != string(twitch.MilestoneObserved) {
					t.Errorf("record %d outcome = %q, want OBSERVED", i, got)
				}
				if got := attrValue(obs[i], "dispatches"); got != want {
					t.Errorf("record %d dispatches = %q, want %s", i, got, want)
				}
			}
			budget := recordLines(logs.String(), milestoneBudgetRecord)
			if len(budget) != 1 {
				t.Fatalf("budget records = %d, want 1", len(budget))
			}
			for key, want := range map[string]string{"cutoff": "COUNT", "startedTargets": "2", "unexaminedTargets": "3"} {
				if got := attrValue(budget[0], key); got != want {
					t.Errorf("%s = %q, want %q", key, got, want)
				}
			}
			if next != 2 {
				t.Errorf("cursor = %d, want 2: the refused third target keeps its turn", next)
			}
		})
	}
}

// TestTheLastPermitCannotCauseAFourthDispatchOrARetryWait: a target that keeps
// failing transiently spends the whole allowance on its retries, is reported
// as UNAVAILABLE/ALLOWANCE_EXHAUSTED after its third dispatch WITHOUT sitting
// out the third backoff, and the next target is refused on COUNT.
func TestTheLastPermitCannotCauseAFourthDispatchOrARetryWait(t *testing.T) {
	logs := captureLogs(t)
	logins := milestoneLogins(t, 2)
	rt := &milestoneRoundTripper{rewardListRespond: func(string, int) (int, string) {
		return http.StatusServiceUnavailable, ""
	}}
	m, streamers := newMilestoneMiner(t, rt, logins, logins)
	logs.Reset()

	start := time.Now()
	next := m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(0))
	elapsed := time.Since(start)

	first := streamers[logins[0]].ChannelID
	assertRewardSequence(t, rt, []string{first, first, first})
	// Two backoffs were paid (>= 500ms + 1000ms), the third (>= 2s) was not.
	if elapsed < 1500*time.Millisecond {
		t.Fatalf("stage took %v: the fixture did not retry, so the bound below is vacuous", elapsed)
	}
	if elapsed >= 3500*time.Millisecond {
		t.Fatalf("stage took %v: it waited out a retry backoff after the last permit was spent", elapsed)
	}
	obs := recordLines(logs.String(), milestoneObservationRecord)
	if len(obs) != 1 {
		t.Fatalf("observation records = %d, want 1", len(obs))
	}
	if got := attrValue(obs[0], "failureClass"); got != string(twitch.MilestoneFailureAllowanceExhausted) {
		t.Errorf("failureClass = %q, want ALLOWANCE_EXHAUSTED — not a transient transport failure", got)
	}
	if got := attrValue(obs[0], "dispatches"); got != "3" {
		t.Errorf("dispatches = %q, want 3", got)
	}
	budget := recordLines(logs.String(), milestoneBudgetRecord)
	if len(budget) != 1 || attrValue(budget[0], "cutoff") != "COUNT" || attrValue(budget[0], "unexaminedTargets") != "1" {
		t.Fatalf("want one COUNT budget record with 1 unexamined target; got %v", budget)
	}
	if next != 1 {
		t.Errorf("cursor = %d, want 1", next)
	}
}

// TestCursorRotatesTheRosterAcrossCycles is the D2 proof over several cycles:
// the cursor follows the last STARTED target, wraps, is normalized against the
// current snapshot, skips ineligible entries without letting them move it, and
// survives an empty, refilled, shrunk and remove/re-add roster.
func TestCursorRotatesTheRosterAcrossCycles(t *testing.T) {
	t.Run("wrap across partial cycles", func(t *testing.T) {
		captureLogs(t)
		logins := milestoneLogins(t, 5)
		rt := &milestoneRoundTripper{}
		m, streamers := newMilestoneMiner(t, rt, logins, logins)
		ids := milestoneChannelIDs(streamers, logins...)

		cursor := 0
		cursor = m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(cursor))
		if cursor != 3 {
			t.Fatalf("cycle 1 cursor = %d, want 3", cursor)
		}
		cursor = m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(cursor))
		if cursor != 1 {
			t.Fatalf("cycle 2 cursor = %d, want 1 (wrapped)", cursor)
		}
		cursor = m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(cursor))
		if cursor != 4 {
			t.Fatalf("cycle 3 cursor = %d, want 4", cursor)
		}
		assertRewardSequence(t, rt, []string{ids[0], ids[1], ids[2], ids[3], ids[4], ids[0], ids[1], ids[2], ids[3]})
		// Every target was visited at most once per cycle, and every eligible
		// target was reached within two cycles.
		_, per := rt.counts()
		for _, id := range ids {
			if per[id] == 0 {
				t.Errorf("target %s was never reached across three cycles", id)
			}
		}
	})

	t.Run("full cycle wraps to zero without a budget record", func(t *testing.T) {
		logs := captureLogs(t)
		logins := milestoneLogins(t, 2)
		rt := &milestoneRoundTripper{}
		m, streamers := newMilestoneMiner(t, rt, logins, logins)
		logs.Reset()
		if next := m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(0)); next != 0 {
			t.Fatalf("cursor = %d, want 0", next)
		}
		assertRewardSequence(t, rt, milestoneChannelIDs(streamers, logins...))
		if got := len(recordLines(logs.String(), milestoneBudgetRecord)); got != 0 {
			t.Errorf("a finished roster emitted %d budget record(s)", got)
		}
	})

	t.Run("offline and ID-less entries are skipped and do not move the cursor", func(t *testing.T) {
		captureLogs(t)
		logins := milestoneLogins(t, 5)
		rt := &milestoneRoundTripper{}
		online := []string{logins[0], logins[2], logins[3], logins[4]}
		m, streamers := newMilestoneMiner(t, rt, logins, online)
		streamers[logins[3]].ChannelID = "" // online but unscoped
		ids := milestoneChannelIDs(streamers, logins...)

		// From 1: index 1 is offline, so the cycle is 2, 4 (3 has no ID), then
		// wraps to 0 — three dispatches, cursor lands after 0.
		if next := m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(1)); next != 1 {
			t.Fatalf("cursor = %d, want 1", next)
		}
		assertRewardSequence(t, rt, []string{ids[2], ids[4], ids[0]})
		_, per := rt.counts()
		if per[ids[1]] != 0 {
			t.Errorf("offline target %s was dispatched", ids[1])
		}
	})

	t.Run("out-of-range and negative cursors are normalized", func(t *testing.T) {
		captureLogs(t)
		logins := milestoneLogins(t, 3)
		rt := &milestoneRoundTripper{}
		m, streamers := newMilestoneMiner(t, rt, logins, logins)
		ids := milestoneChannelIDs(streamers, logins...)
		if next := m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(7)); next != 1 {
			t.Fatalf("cursor 7 over 3 targets: next = %d, want 1", next)
		}
		if next := m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(-1)); next != 2 {
			t.Fatalf("cursor -1 over 3 targets: next = %d, want 2", next)
		}
		assertRewardSequence(t, rt, []string{ids[1], ids[2], ids[0], ids[2], ids[0], ids[1]})
	})

	t.Run("empty, refilled, shrunk, removed and re-added rosters", func(t *testing.T) {
		logs := captureLogs(t)
		rt := &milestoneRoundTripper{}
		// LoadFromConfig refuses an empty roster, so seed one streamer and
		// remove it through the same runtime path a settings change uses.
		seed := milestoneLogins(t, 1)
		m, _ := newMilestoneMiner(t, rt, seed, nil)
		if _, removed, _, _ := m.streamers.ApplySettings(nil, m.config.StreamerSettings); len(removed) != 1 || len(m.streamers.All()) != 0 {
			t.Fatalf("could not empty the roster: removed %d, remaining %d", len(removed), len(m.streamers.All()))
		}
		logs.Reset()

		// Empty: nothing to do, cursor resets to 0, no record.
		if next := m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(4)); next != 0 {
			t.Fatalf("empty roster: next = %d, want 0", next)
		}
		if rt.rewardTotal() != 0 || len(recordLines(logs.String(), milestoneBudgetRecord)) != 0 {
			t.Fatalf("empty roster dispatched or recorded something")
		}

		// Refill with five, all online.
		logins := milestoneLogins(t, 5)
		apply := func(keep ...string) {
			t.Helper()
			var configs []config.StreamerConfig
			for _, login := range keep {
				configs = append(configs, config.StreamerConfig{Username: login})
			}
			added, _, _, _ := m.streamers.ApplySettings(configs, m.config.StreamerSettings)
			for _, s := range added {
				s.SetConfirmedOnline()
			}
			for _, s := range m.streamers.All() {
				if s.ChannelID == "" {
					t.Fatalf("streamer %s has no channel ID after ApplySettings", s.GetUsername())
				}
			}
		}
		apply(logins...)
		roster := func() []string {
			var out []string
			for _, s := range m.streamers.All() {
				out = append(out, s.ChannelID)
			}
			return out
		}
		ids := roster()
		if len(ids) != 5 {
			t.Fatalf("roster = %d, want 5", len(ids))
		}
		cursor := m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(0))
		if cursor != 3 {
			t.Fatalf("refilled roster: cursor = %d, want 3", cursor)
		}
		assertRewardSequence(t, rt, []string{ids[0], ids[1], ids[2]})

		// Shrink to two while the cursor says 3: normalized to 3 % 2 = 1, so
		// index 1 is served first, then index 0, and the cursor lands after 0.
		apply(logins[0], logins[1])
		ids = roster()
		cursor = m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(cursor))
		if cursor != 1 {
			t.Fatalf("shrunk roster: cursor = %d, want 1", cursor)
		}
		assertRewardSequence(t, rt, append(rt.rewardSequence()[:3], ids[1], ids[0]))

		// Remove one entry BEFORE the cursor position and re-add another: the
		// cursor is a position in the snapshot, not a streamer, so the entry
		// that slides into the position is served and the one that slid past
		// it waits one cycle. That is the documented cost of owning no
		// per-streamer state.
		apply(logins[0], logins[1], logins[2], logins[3])
		cursor = 2                             // logins[2] is due next
		apply(logins[1], logins[2], logins[3]) // remove logins[0], before the cursor
		ids = roster()
		before := rt.rewardTotal()
		cursor = m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(cursor))
		seq := rt.rewardSequence()[before:]
		// Snapshot is [1, 2, 3]; position 2 is logins[3], then wrap to 1, 2.
		if !reflect.DeepEqual(seq, []string{ids[2], ids[0], ids[1]}) {
			t.Fatalf("after removal before the cursor: order = %v, want %v", seq, []string{ids[2], ids[0], ids[1]})
		}
		if cursor != 2 {
			t.Fatalf("cursor after a full wrap = %d, want 2", cursor)
		}

		// Re-add logins[0]: it lands at the end of the snapshot and is reached
		// in roster order like any other entry; no stale pointer is involved.
		apply(logins[1], logins[2], logins[3], logins[0])
		ids = roster()
		before = rt.rewardTotal()
		cursor = m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(cursor))
		seq = rt.rewardSequence()[before:]
		if !reflect.DeepEqual(seq, []string{ids[2], ids[3], ids[0]}) {
			t.Fatalf("after re-add: order = %v, want %v", seq, []string{ids[2], ids[3], ids[0]})
		}
		if cursor != 1 {
			t.Fatalf("cursor = %d, want 1", cursor)
		}
	})
}

// TestASlowFirstTargetCannotMonopolizeTheRoster: without a cursor, a first
// target that alone consumes the stage's time would be the ONLY target ever
// observed. With it, the next cycle starts after that target.
func TestASlowFirstTargetCannotMonopolizeTheRoster(t *testing.T) {
	previousBudget := milestoneObservationCycleBudget
	milestoneObservationCycleBudget = 50 * time.Millisecond
	t.Cleanup(func() { milestoneObservationCycleBudget = previousBudget })

	captureLogs(t)
	logins := milestoneLogins(t, 3)
	rt := &milestoneRoundTripper{}
	m, streamers := newMilestoneMiner(t, rt, logins, logins)
	ids := milestoneChannelIDs(streamers, logins...)
	rt.mu.Lock()
	rt.rewardListRespond = func(channelID string, _ int) (int, string) {
		if channelID == ids[0] {
			time.Sleep(120 * time.Millisecond) // longer than the whole stage budget
		}
		return 0, ""
	}
	rt.mu.Unlock()

	cursor := m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(0))
	if cursor != 1 {
		t.Fatalf("cycle 1 cursor = %d, want 1: the slow target was started, so the cursor moves past it", cursor)
	}
	assertRewardSequence(t, rt, []string{ids[0]})

	cursor = m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(cursor))
	seq := rt.rewardSequence()
	if len(seq) < 3 || seq[1] != ids[1] || seq[2] != ids[2] {
		t.Fatalf("cycle 2 did not start after the slow target: order = %v", seq)
	}
	if cursor != 1 {
		t.Fatalf("cycle 2 cursor = %d, want 1 (targets 1, 2 and the slow 0 were all started)", cursor)
	}
}

// TestCancellationStopsAdmissionsMidBackoffAndMidRequest: an owner cancelled
// while the stage is inside a retry backoff, or inside an in-flight request,
// makes no further dispatch, records the started target as CANCELLED, emits no
// budget record (shutdown is silent) and leaves the cursor after the target
// that was started.
func TestCancellationStopsAdmissionsMidBackoffAndMidRequest(t *testing.T) {
	t.Run("during the retry backoff", func(t *testing.T) {
		logs := captureLogs(t)
		logins := milestoneLogins(t, 3)
		owner, cancel := context.WithCancel(context.Background())
		defer cancel()
		rt := &milestoneRoundTripper{rewardListRespond: func(string, int) (int, string) {
			return http.StatusServiceUnavailable, "" // transient: the client wants to back off
		}}
		rt.onRewardList = func(n int) {
			if n == 1 {
				cancel() // the owner goes away while the first answer is being returned
			}
		}
		m, streamers := newMilestoneMiner(t, rt, logins, logins)
		logs.Reset()

		start := time.Now()
		next := m.observeWatchStreakMilestones(owner, milestoneCycleNow(0))
		elapsed := time.Since(start)

		assertRewardSequence(t, rt, []string{streamers[logins[0]].ChannelID})
		if elapsed >= 500*time.Millisecond {
			t.Errorf("stage took %v after cancellation: it sat out a retry backoff", elapsed)
		}
		obs := recordLines(logs.String(), milestoneObservationRecord)
		if len(obs) != 1 || attrValue(obs[0], "outcome") != string(twitch.MilestoneCancelled) || attrValue(obs[0], "dispatches") != "1" {
			t.Fatalf("want one CANCELLED observation with dispatches=1; got %v", obs)
		}
		if got := len(recordLines(logs.String(), milestoneBudgetRecord)); got != 0 {
			t.Errorf("owner shutdown emitted %d budget record(s)", got)
		}
		if next != 1 {
			t.Errorf("cursor = %d, want 1", next)
		}
	})

	t.Run("during an in-flight request", func(t *testing.T) {
		logs := captureLogs(t)
		logins := milestoneLogins(t, 3)
		m, streamers := newMilestoneMiner(t, &milestoneRoundTripper{}, logins, logins)
		stall := newStallingTransport()
		previousTransport := http.DefaultTransport
		http.DefaultTransport = stall
		t.Cleanup(func() { http.DefaultTransport = previousTransport })
		owner, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			// Wait for the first request to be in flight, then pull the owner.
			deadline := time.Now().Add(5 * time.Second)
			for stall.requests() == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			cancel()
		}()
		logs.Reset()

		next := m.observeWatchStreakMilestones(owner, milestoneCycleNow(0))

		if got := stall.requests(); got != 1 {
			t.Fatalf("requests = %d, want exactly 1: cancellation must stop every further admission", got)
		}
		obs := recordLines(logs.String(), milestoneObservationRecord)
		if len(obs) != 1 || attrValue(obs[0], "outcome") != string(twitch.MilestoneCancelled) {
			t.Fatalf("want one CANCELLED observation; got %v", obs)
		}
		if got := attrValue(obs[0], "channelId"); got != streamers[logins[0]].ChannelID {
			t.Errorf("the cancelled observation names %q, want the first target", got)
		}
		if got := len(recordLines(logs.String(), milestoneBudgetRecord)); got != 0 {
			t.Errorf("owner shutdown emitted %d budget record(s)", got)
		}
		if next != 1 {
			t.Errorf("cursor = %d, want 1", next)
		}
	})
}

// TestBudgetRecordDistinguishesTimeFromCountAndExaminedFromUnexamined pins the
// record's fields for each way a cycle can stop short.
func TestBudgetRecordDistinguishesTimeFromCountAndExaminedFromUnexamined(t *testing.T) {
	t.Run("COUNT under the stage budget", func(t *testing.T) {
		logs := captureLogs(t)
		logins := milestoneLogins(t, 5)
		rt := &milestoneRoundTripper{}
		m, _ := newMilestoneMiner(t, rt, logins, logins)
		logs.Reset()
		m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(0))

		for _, line := range recordLines(logs.String(), milestoneObservationRecord) {
			if got := attrValue(line, "dispatches"); got != "1" {
				t.Errorf("observation dispatches = %q, want 1", got)
			}
		}
		lines := recordLines(logs.String(), milestoneBudgetRecord)
		if len(lines) != 1 {
			t.Fatalf("budget records = %d, want 1", len(lines))
		}
		for key, want := range map[string]string{
			"cutoff": "COUNT", "deadlineSource": "STAGE_BUDGET", "startedTargets": "3", "unexaminedTargets": "2",
			"dispatchesSpent": "3", "dispatchesRemaining": "0", "dispatchAllowance": "3", "cycleBudgetSeconds": "40",
		} {
			if got := attrValue(lines[0], key); got != want {
				t.Errorf("%s = %q, want %q; line: %s", key, got, want, lines[0])
			}
		}
		if slack, err := strconv.Atoi(attrValue(lines[0], "slackMs")); err != nil || slack <= 0 || slack > 40_000 {
			t.Errorf("slackMs = %q, want a positive value up to the 40s budget", attrValue(lines[0], "slackMs"))
		}
		if got := attrValue(lines[0], "level"); got != "DEBUG" {
			t.Errorf("level = %q, want DEBUG", got)
		}
		for _, forbidden := range logins {
			if strings.Contains(lines[0], forbidden) {
				t.Errorf("budget record names a streamer: %s", lines[0])
			}
		}
	})

	t.Run("TIME under the stage budget", func(t *testing.T) {
		previousBudget := milestoneObservationCycleBudget
		milestoneObservationCycleBudget = 50 * time.Millisecond
		t.Cleanup(func() { milestoneObservationCycleBudget = previousBudget })
		logs := captureLogs(t)
		logins := milestoneLogins(t, 4)
		rt := &milestoneRoundTripper{}
		rt.onRewardList = func(int) { time.Sleep(120 * time.Millisecond) }
		m, _ := newMilestoneMiner(t, rt, logins, logins)
		logs.Reset()
		m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(0))

		lines := recordLines(logs.String(), milestoneBudgetRecord)
		if len(lines) != 1 {
			t.Fatalf("budget records = %d, want 1", len(lines))
		}
		for key, want := range map[string]string{
			"cutoff": "TIME", "deadlineSource": "STAGE_BUDGET", "startedTargets": "1", "unexaminedTargets": "3",
			"dispatchesSpent": "1", "dispatchesRemaining": "2",
		} {
			if got := attrValue(lines[0], key); got != want {
				t.Errorf("%s = %q, want %q; line: %s", key, got, want, lines[0])
			}
		}
	})

	t.Run("COUNT under an earlier owner deadline names the owner", func(t *testing.T) {
		logs := captureLogs(t)
		logins := milestoneLogins(t, 5)
		rt := &milestoneRoundTripper{}
		m, _ := newMilestoneMiner(t, rt, logins, logins)
		owner, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		logs.Reset()
		m.observeWatchStreakMilestones(owner, milestoneCycleNow(0))

		lines := recordLines(logs.String(), milestoneBudgetRecord)
		if len(lines) != 1 {
			t.Fatalf("budget records = %d, want 1", len(lines))
		}
		if got := attrValue(lines[0], "deadlineSource"); got != "OWNER" {
			t.Errorf("deadlineSource = %q, want OWNER", got)
		}
		if slack, err := strconv.Atoi(attrValue(lines[0], "slackMs")); err != nil || slack <= 0 || slack > 10_000 {
			t.Errorf("slackMs = %q, want a positive value up to the owner's 10s", attrValue(lines[0], "slackMs"))
		}
	})

	t.Run("TIME under the next tick", func(t *testing.T) {
		logs := captureLogs(t)
		logins := milestoneLogins(t, 4)
		rt := &milestoneRoundTripper{}
		rt.onRewardList = func(int) { time.Sleep(120 * time.Millisecond) }
		m, _ := newMilestoneMiner(t, rt, logins, logins)
		logs.Reset()
		// The business pass left 50ms of the period.
		cycle := milestoneCycle{Tick: time.Now().Add(-bonusPollInterval + 50*time.Millisecond), Period: bonusPollInterval}
		m.observeWatchStreakMilestones(context.Background(), cycle)

		lines := recordLines(logs.String(), milestoneBudgetRecord)
		if len(lines) != 1 {
			t.Fatalf("budget records = %d, want 1", len(lines))
		}
		for key, want := range map[string]string{"cutoff": "TIME", "deadlineSource": "NEXT_TICK", "startedTargets": "1", "unexaminedTargets": "3"} {
			if got := attrValue(lines[0], key); got != want {
				t.Errorf("%s = %q, want %q; line: %s", key, got, want, lines[0])
			}
		}
	})
}

// TestBonusPollLoopCarriesTheCursorAcrossCycles proves the ONE cursor lives on
// the loop and is threaded from one serviced tick to the next: over two ticks
// a five-target roster is walked 0,1,2 then 3,4,0.
func TestBonusPollLoopCarriesTheCursorAcrossCycles(t *testing.T) {
	logs := captureLogs(t)
	logins := milestoneLogins(t, 5)
	rt := &milestoneRoundTripper{}
	m, streamers := newMilestoneMiner(t, rt, logins, logins)
	ids := milestoneChannelIDs(streamers, logins...)

	previousInterval := bonusPollInterval
	bonusPollInterval = 500 * time.Millisecond
	t.Cleanup(func() { bonusPollInterval = previousInterval })

	seenSix := make(chan struct{})
	var once sync.Once
	rt.mu.Lock()
	rt.onRewardList = func(n int) {
		if n >= 6 {
			once.Do(func() { close(seenSix) })
		}
	}
	rt.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.bonusPollLoop(ctx)
	}()
	select {
	case <-seenSix:
	case <-time.After(30 * time.Second):
		cancel()
		<-done
		t.Fatalf("the loop did not reach six RewardList dispatches within 30s; logs:\n%s", logs.String())
	}
	cancel()
	<-done

	seq := rt.rewardSequence()[:6]
	if want := []string{ids[0], ids[1], ids[2], ids[3], ids[4], ids[0]}; !reflect.DeepEqual(seq, want) {
		t.Fatalf("dispatch order across two ticks = %v, want %v", seq, want)
	}
}

// TestCursorFollowsTheLastStartedTargetWhateverItsOutcome: the cursor moves
// past a started target on a FAILED outcome exactly as on a success, including
// when that target is the last one the cycle touched and nothing was refused
// after it. Otherwise a roster whose last entry keeps failing would be
// re-examined first every cycle.
func TestCursorFollowsTheLastStartedTargetWhateverItsOutcome(t *testing.T) {
	cases := map[string]func(string, int) (int, string){
		"HTTP status refusal": func(_ string, n int) (int, string) {
			if n == 3 {
				return http.StatusNotFound, `{"error":"Not Found"}`
			}
			return 0, ""
		},
		"APQ traversal cut by the allowance": func(_ string, n int) (int, string) {
			if n == 3 {
				return http.StatusOK, milestonePQNFBody // the last permit; the traversal cannot complete
			}
			return 0, ""
		},
	}
	for name, respond := range cases {
		t.Run(name, func(t *testing.T) {
			logs := captureLogs(t)
			logins := milestoneLogins(t, 3)
			rt := &milestoneRoundTripper{rewardListRespond: respond}
			m, streamers := newMilestoneMiner(t, rt, logins, logins)
			ids := milestoneChannelIDs(streamers, logins...)
			logs.Reset()

			next := m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(0))
			assertRewardSequence(t, rt, []string{ids[0], ids[1], ids[2]})
			obs := recordLines(logs.String(), milestoneObservationRecord)
			if len(obs) != 3 || attrValue(obs[2], "outcome") == string(twitch.MilestoneObserved) {
				t.Fatalf("want three observations with the last one failed; got %d: %v", len(obs), obs)
			}
			if next != 0 {
				t.Fatalf("cursor = %d, want 0: the failed last target was STARTED, so the cursor wraps past it", next)
			}
			// And the next cycle really starts at the beginning again.
			m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(next))
			assertRewardSequence(t, rt, []string{ids[0], ids[1], ids[2], ids[0], ids[1], ids[2]})
		})
	}
}

// TestBonusPollLoopHandsTheStageTheServicedTickNotNow is the loop-level half of
// D1, and the falsifier for it: a business pass that consumes the whole poll
// period must leave the stage NO slack. If the loop handed the stage a fresh
// timestamp (or a padded period) instead of the serviced tick's, every such
// cycle would be granted a full 40s of diagnostics on top of the late business
// pass - exactly what D1 forbids.
func TestBonusPollLoopHandsTheStageTheServicedTickNotNow(t *testing.T) {
	// The loop writes records from its own goroutine while this test polls
	// for them, so the capture must be synchronized; captureLogs's plain
	// buffer is only safe to read after the loop has stopped.
	logs := &lockedLogBuffer{}
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	logins := milestoneLogins(t, 1)
	rt := &milestoneRoundTripper{contextDelay: 250 * time.Millisecond}
	m, _ := newMilestoneMiner(t, rt, logins, logins)

	previousInterval := bonusPollInterval
	bonusPollInterval = 200 * time.Millisecond // shorter than the business pass
	t.Cleanup(func() { bonusPollInterval = previousInterval })
	logs.Reset()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.bonusPollLoop(ctx)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(recordLines(logs.String(), milestoneBudgetRecord)) >= 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	ops, _ := rt.counts()
	business := 0
	for _, op := range ops {
		if op == "ChannelPointsContext" {
			business++
		}
	}
	if business < 3 {
		t.Fatalf("only %d business passes ran; the fixture did not drive enough cycles. Logs:\n%s", business, logs.String())
	}
	if got := rt.rewardTotal(); got != 0 {
		t.Fatalf("%d RewardList dispatch(es) on cycles whose business pass consumed the period: the loop granted "+
			"the stage slack it did not have (D1 violated at the loop)", got)
	}
	budget := recordLines(logs.String(), milestoneBudgetRecord)
	if len(budget) < 3 {
		t.Fatalf("budget records = %d, want >= 3 (one per consumed cycle)", len(budget))
	}
	for _, line := range budget {
		if attrValue(line, "cutoff") != "TIME" || attrValue(line, "deadlineSource") != "NEXT_TICK" || attrValue(line, "startedTargets") != "0" {
			t.Errorf("budget record is not a next-tick TIME cutoff with nothing started: %s", line)
		}
	}
}

// TestAZeroTickOrPeriodAdmitsNothing pins the documented fail-closed reading of
// a zero milestoneCycle: a zero Tick is maximally overdue and a zero Period
// leaves no slack, so neither admits a dispatch and neither is silently given
// the stage budget instead.
func TestAZeroTickOrPeriodAdmitsNothing(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	if deadline, source := milestoneStageDeadline(context.Background(), time.Time{}, bonusPollInterval, now); source != milestoneDeadlineNextTick || !deadline.Before(now) {
		t.Errorf("zero tick: source %q, deadline %v; want NEXT_TICK in the past", source, deadline)
	}
	if deadline, source := milestoneStageDeadline(context.Background(), now, 0, now); source != milestoneDeadlineNextTick || deadline.After(now) {
		t.Errorf("zero period: source %q, deadline %v; want NEXT_TICK with no slack", source, deadline)
	}

	logs := captureLogs(t)
	logins := milestoneLogins(t, 2)
	rt := &milestoneRoundTripper{}
	m, _ := newMilestoneMiner(t, rt, logins, logins)
	logs.Reset()
	if next := m.observeWatchStreakMilestones(context.Background(), milestoneCycle{Cursor: 1}); next != 1 || rt.rewardTotal() != 0 {
		t.Fatalf("zero cycle: next %d, dispatches %d; want 1/0", next, rt.rewardTotal())
	}
	if next := m.observeWatchStreakMilestones(context.Background(), milestoneCycle{Tick: time.Now(), Period: 0, Cursor: 1}); next != 1 || rt.rewardTotal() != 0 {
		t.Fatalf("zero period: next %d, dispatches %d; want 1/0", next, rt.rewardTotal())
	}
	if got := len(recordLines(logs.String(), milestoneBudgetRecord)); got != 2 {
		t.Errorf("budget records = %d, want 2 (one per refused cycle)", got)
	}
}

// TestIneligibleEntriesAndMidCycleChangesLeaveTheCursorAndMaskAlone pins three
// D2 rules that the happy paths cannot observe: an ineligible tail entry does
// not move the cursor on its own; eligibility is decided ONCE, up front, so a
// target that goes offline while an earlier request is in flight is still
// dispatched and still counted; and a target that loses its channel identity
// mid-cycle is skipped without ending the roster, without a record and without
// moving the cursor on its behalf.
func TestIneligibleEntriesAndMidCycleChangesLeaveTheCursorAndMaskAlone(t *testing.T) {
	t.Run("an ineligible tail entry does not move the cursor", func(t *testing.T) {
		captureLogs(t)
		logins := milestoneLogins(t, 2)
		rt := &milestoneRoundTripper{}
		m, streamers := newMilestoneMiner(t, rt, logins, logins[:1])
		next := m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(0))
		assertRewardSequence(t, rt, []string{streamers[logins[0]].ChannelID})
		if next != 1 {
			t.Fatalf("cursor = %d, want 1: the cursor sits after the started target, on the ineligible entry's "+
				"position, which the skip must not advance past", next)
		}
	})

	t.Run("the eligibility mask is taken once", func(t *testing.T) {
		logs := captureLogs(t)
		logins := milestoneLogins(t, 3)
		rt := &milestoneRoundTripper{}
		m, streamers := newMilestoneMiner(t, rt, logins, logins)
		rt.onRewardList = func(n int) {
			if n == 1 {
				streamers[logins[1]].SetConfirmedOffline() // after the mask, before its turn
			}
		}
		logs.Reset()
		next := m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(0))
		assertRewardSequence(t, rt, milestoneChannelIDs(streamers, logins...))
		if next != 0 {
			t.Errorf("cursor = %d, want 0", next)
		}
		if got := len(recordLines(logs.String(), milestoneBudgetRecord)); got != 0 {
			t.Errorf("a finished roster emitted %d budget record(s)", got)
		}
	})

	t.Run("a target that loses its channel identity mid-cycle is skipped", func(t *testing.T) {
		logs := captureLogs(t)
		logins := milestoneLogins(t, 3)
		rt := &milestoneRoundTripper{}
		m, streamers := newMilestoneMiner(t, rt, logins, logins)
		ids := milestoneChannelIDs(streamers, logins...)
		rt.onRewardList = func(n int) {
			if n == 1 {
				streamers[logins[1]].ChannelID = ""
			}
		}
		logs.Reset()
		next := m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(0))
		assertRewardSequence(t, rt, []string{ids[0], ids[2]})
		if next != 0 {
			t.Errorf("cursor = %d, want 0: the roster was finished (the skipped target did not end it)", next)
		}
		obs := recordLines(logs.String(), milestoneObservationRecord)
		if len(obs) != 2 {
			t.Errorf("observation records = %d, want 2: a target refused for no channel identity is not observed", len(obs))
		}
		if got := len(recordLines(logs.String(), milestoneBudgetRecord)); got != 0 {
			t.Errorf("a skipped target produced %d budget record(s); nothing was cut off", got)
		}
	})
}

// TestTheShippedClientIDSetFitsTheCycleAllowance is the miner-side half of the
// premise that a complete client-ID traversal fits one cycle: the shipped
// candidate set is bounded by the allowance THIS package chooses, so lowering
// the allowance below the set (or growing the set) fails here first.
func TestTheShippedClientIDSetFitsTheCycleAllowance(t *testing.T) {
	if got := len(constants.GQLClientIDFallbacks); got > milestoneCycleDispatchAllowance {
		t.Fatalf("%d shipped client IDs exceed the %d-dispatch cycle allowance: a complete APQ traversal no "+
			"longer fits one cycle and UNSUPPORTED_QUERY becomes unreachable through the stage",
			got, milestoneCycleDispatchAllowance)
	}
}

// TestRecordFootprintIsMeasuredAndBounded pins the per-record sizes the
// documentation quotes, with the method stated: one slog TextHandler at DEBUG
// (the shape internal/logger uses for the file), one OBSERVED record from the
// default fixture, one unaccepted-hash cycle, and one budget record. The bounds
// are generous so wording changes do not fail the build; the logged sizes are
// what SPECIFICATIONS.md derives its footprint from.
func TestRecordFootprintIsMeasuredAndBounded(t *testing.T) {
	// One cap per documented figure, plus one for the WHOLE unaccepted-hash
	// cycle and its line count, so that a new diagnostic line or a grown
	// summary cannot exceed the documented cycle measurement unnoticed and
	// silently invalidate the derived retention estimate.
	const (
		observationCap = 1536
		budgetCap      = 512
		cycleCap       = 2048
		wantCycleLines = 3 // the observation record, the all-candidates summary, the budget record
	)

	logs := captureLogs(t)
	logins := milestoneLogins(t, 1)
	m, _ := newMilestoneMiner(t, &milestoneRoundTripper{}, logins, logins)
	logs.Reset()
	m.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(0))
	observed := recordLines(logs.String(), milestoneObservationRecord)
	if len(observed) != 1 {
		t.Fatalf("observation records = %d, want 1", len(observed))
	}
	t.Logf("OBSERVED record: %d bytes", len(observed[0]))
	if len(observed[0]) > observationCap {
		t.Errorf("an OBSERVED record is %d bytes, over the %d-byte documented bound", len(observed[0]), observationCap)
	}

	logs.Reset()
	rt := &milestoneRoundTripper{rewardListBody: milestonePQNFBody}
	m2, _ := newMilestoneMiner(t, rt, milestoneLogins(t, 2), nil)
	for _, s := range m2.streamers.All() {
		s.SetConfirmedOnline()
	}
	logs.Reset()
	m2.observeWatchStreakMilestones(context.Background(), milestoneCycleNow(0))
	cycle := 0
	for _, line := range strings.Split(logs.String(), "\n") {
		if line != "" {
			cycle += len(line) + 1
		}
	}
	budget := recordLines(logs.String(), milestoneBudgetRecord)
	if len(budget) != 1 {
		t.Fatalf("budget records = %d, want 1 (two targets, one complete traversal)", len(budget))
	}
	cycleLines := len(strings.Split(strings.TrimSpace(logs.String()), "\n"))
	t.Logf("unaccepted-hash cycle: %d bytes across %d lines; budget record: %d bytes", cycle, cycleLines, len(budget[0]))
	if len(budget[0]) > budgetCap {
		t.Errorf("a budget record is %d bytes, over the %d-byte documented bound", len(budget[0]), budgetCap)
	}
	if cycleLines != wantCycleLines {
		t.Errorf("an unaccepted-hash cycle wrote %d lines, want %d; a new line per cycle changes the documented "+
			"footprint and must be re-measured:\n%s", cycleLines, wantCycleLines, logs.String())
	}
	if cycle > cycleCap {
		t.Errorf("an unaccepted-hash cycle is %d bytes, over the %d-byte documented bound", cycle, cycleCap)
	}
}

// lockedLogBuffer is a bytes.Buffer safe to read while another goroutine
// logs into it.
type lockedLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *lockedLogBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}
