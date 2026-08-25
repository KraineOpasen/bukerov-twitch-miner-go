package discovery

import (
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/policy"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/watcher"
)

// campaignPolicyPublisher is deliberately discovered dynamically so this
// behavior regression still compiles against the pre-fix Manager. The old
// implementation falls back to game-wide ranks and fails the exact-channel
// assertions below; the fixed production seam publishes both maps atomically.
type campaignPolicyPublisher interface {
	SetCampaignPolicy(map[string]int, map[string]policy.CampaignSemantic)
}

func publishCampaignPolicy(m *Manager, gameRanks map[string]int, campaignClasses map[string]policy.SemanticClass) {
	if publisher, ok := any(m).(campaignPolicyPublisher); ok {
		publisher.SetCampaignPolicy(gameRanks, testCampaignSemantics(campaignClasses))
		return
	}
	m.SetGameRanks(gameRanks)
}

type recordingCandidatePolicy struct {
	fakeSlotStatus
	login           string
	facts           watcher.CandidateCampaignPolicy
	gameRanks       map[string]int
	campaignClasses map[string]policy.SemanticClass
	campaignFacts   map[string]policy.CampaignSemantic
}

func testCampaignSemantics(classes map[string]policy.SemanticClass) map[string]policy.CampaignSemantic {
	if classes == nil {
		return nil
	}
	facts := make(map[string]policy.CampaignSemantic, len(classes))
	for id, class := range classes {
		facts[id] = policy.CampaignSemantic{SemanticClass: class, SecondaryEligible: true}
	}
	return facts
}

func (r *recordingCandidatePolicy) SetDiscoveryCandidatePolicy(login string, facts watcher.CandidateCampaignPolicy) {
	r.login = login
	r.facts = facts
}

func (r *recordingCandidatePolicy) DiscoveryCampaignPolicy() (map[string]int, map[string]policy.CampaignSemantic) {
	if r.campaignFacts != nil {
		return r.gameRanks, r.campaignFacts
	}
	return r.gameRanks, testCampaignSemantics(r.campaignClasses)
}

// TestOrderGamesByPolicy verifies the campaign-policy cross-game ordering:
// published ranks reorder the configured game list (lower rank first),
// unranked games keep their relative order at the end, and no published ranks
// leaves the configured order bit-identical.
func TestOrderGamesByPolicy(t *testing.T) {
	m := &Manager{}
	games := []string{"Alpha", "Bravo", "Charlie", "Delta"}

	// No ranks published → identical order.
	if got := m.orderGamesByPolicy(games); !equal(got, games) {
		t.Fatalf("no ranks: expected unchanged order, got %v", got)
	}

	// Ranks favor Charlie, then Alpha; Bravo/Delta unranked keep their order.
	m.SetGameRanks(map[string]int{"charlie": 0, "alpha": 1})
	got := m.orderGamesByPolicy(games)
	want := []string{"Charlie", "Alpha", "Bravo", "Delta"}
	if !equal(got, want) {
		t.Fatalf("ranked order = %v, want %v", got, want)
	}

	// Clearing ranks restores the configured order.
	m.SetGameRanks(nil)
	if got := m.orderGamesByPolicy(games); !equal(got, games) {
		t.Fatalf("after clearing ranks, expected configured order, got %v", got)
	}
}

func TestSetCampaignPolicyDistinguishesExactEmptyFromRankOnlySnapshot(t *testing.T) {
	m := &Manager{}
	ranks := map[string]int{"game one": 0}
	m.SetGameRanks(ranks)
	m.SetCampaignPolicy(ranks, map[string]policy.CampaignSemantic{})
	published := m.campaignPolicy.Load()
	if published == nil || published.campaignSemantics == nil {
		t.Fatal("exact empty campaign-class publication was collapsed into rank-only fallback")
	}
}

// TestWatchCandidatesReevaluatesValidCurrentAfterPolicyChange is the
// production-seam regression for a stale discovery proposal. Both channels
// remain fully eligible, so no ordinary invalidReason can mask the policy
// change: publishing a strictly stronger semantic game rank must change the
// next candidate handed to the Unified Slot Broker.
func TestWatchCandidatesReevaluatesValidCurrentAfterPolicyChange(t *testing.T) {
	provider := &fakeCampaigns{campaigns: []*models.Campaign{
		activeCampaign("g1", "Game One"),
		activeCampaign("g2", "Game Two"),
	}}
	client := &fakeClient{}
	m := newTestManager([]string{"Game One", "Game Two"}, provider, client)
	g1 := onlineCandidate("game_one_channel", "1", "Game One", "g1", 9000)
	g2 := onlineCandidate("game_two_channel", "2", "Game Two", "g2", 100)
	m.pool = []*Channel{g1, g2}

	m.SetGameRanks(map[string]int{"game one": 0, "game two": 1})
	initial := m.WatchCandidates()
	if len(initial) != 1 || initial[0].Streamer != g1.Streamer {
		t.Fatalf("initial policy proposal = %v, want Game One", candidateLogins(initial))
	}

	// A pure semantic change: both candidates are still online, carry their
	// active campaigns, remain configured, and have not changed game.
	m.SetGameRanks(map[string]int{"game one": 1, "game two": 0})
	got := m.WatchCandidates()
	if len(got) != 1 || got[0].Streamer != g2.Streamer {
		t.Fatalf("proposal after strictly stronger Game Two rank = %v, want [game_two_channel]", candidateLogins(got))
	}
	if len(client.checked) != 0 {
		t.Fatalf("policy-only rerank made online API checks despite sufficient local pool state: %v", client.checked)
	}
}

// TestWatchCandidatesUsesExactCarriedCampaignClassAcrossGames proves that a
// game's best campaign is only a discovery pre-order, never the semantic class
// assigned wholesale to every channel in that game. Game One has the global
// class-0 campaign, but its live channel carries only class 5; the Game Two
// channel carrying class 1 is the real policy winner and must be proposed.
func TestWatchCandidatesUsesExactCarriedCampaignClassAcrossGames(t *testing.T) {
	urgentOne := activeCampaign("g1", "Game One")
	urgentOne.ID = "game-one-urgent"
	weakOne := activeCampaign("g1", "Game One")
	weakOne.ID = "game-one-weak"
	middleTwo := activeCampaign("g2", "Game Two")
	middleTwo.ID = "game-two-middle"
	provider := &fakeCampaigns{campaigns: []*models.Campaign{urgentOne, weakOne, middleTwo}}
	m := newTestManager([]string{"Game One", "Game Two"}, provider, &fakeClient{})

	gameOne := onlineCandidate("game_one_weak", "1", "Game One", "g1", 9000)
	gameOne.Streamer.Stream.SetCampaignIDs([]string{weakOne.ID})
	gameTwo := onlineCandidate("game_two_middle", "2", "Game Two", "g2", 100)
	gameTwo.Streamer.Stream.SetCampaignIDs([]string{middleTwo.ID})
	m.pool = []*Channel{gameOne, gameTwo}
	published := &recordingCandidatePolicy{}
	m.SetSlotStatus(published)

	publishCampaignPolicy(m,
		map[string]int{"game one": 0, "game two": 1},
		map[string]policy.SemanticClass{urgentOne.ID: 0, weakOne.ID: 5, middleTwo.ID: 1},
	)
	got := m.WatchCandidates()
	if len(got) != 1 || got[0].Streamer != gameTwo.Streamer {
		t.Fatalf("exact carried-campaign proposal = %v, want [game_two_middle]", candidateLogins(got))
	}
	if published.login != gameTwo.Streamer.GetUsername() || !published.facts.Ranked || published.facts.Utility.SemanticClass != 1 {
		t.Fatalf("same-tick broker publication = login %q facts %+v, want game_two_middle class 1", published.login, published.facts)
	}
	m.mu.Lock()
	m.current = nil
	m.pool = nil
	m.mu.Unlock()
	if got := m.WatchCandidates(); len(got) != 0 || published.login != "" {
		t.Fatalf("empty proposal retained stale policy publication: candidates=%v login=%q", candidateLogins(got), published.login)
	}
}

func TestWatchCandidatesReevaluatesExactCampaignClassWithinOneGame(t *testing.T) {
	strong := activeCampaign("g1", "Game One")
	strong.ID = "same-game-strong"
	weak := activeCampaign("g1", "Game One")
	weak.ID = "same-game-weak"
	provider := &fakeCampaigns{campaigns: []*models.Campaign{strong, weak}}
	m := newTestManager([]string{"Game One"}, provider, &fakeClient{})

	weakChannel := onlineCandidate("weak_current", "1", "Game One", "g1", 9000)
	weakChannel.Streamer.Stream.SetCampaignIDs([]string{weak.ID})
	strongChannel := onlineCandidate("strong_candidate", "2", "Game One", "g1", 100)
	strongChannel.Streamer.Stream.SetCampaignIDs([]string{strong.ID})
	m.current = weakChannel
	m.pool = []*Channel{weakChannel, strongChannel}
	publishCampaignPolicy(m,
		map[string]int{"game one": 0},
		map[string]policy.SemanticClass{strong.ID: 0, weak.ID: 2},
	)

	got := m.WatchCandidates()
	if len(got) != 1 || got[0].Streamer != strongChannel.Streamer {
		t.Fatalf("same-game exact semantic reevaluation = %v, want [strong_candidate]", candidateLogins(got))
	}
}

func TestWatchCandidatesUsesBoundedSecondaryAfterEqualPrimary(t *testing.T) {
	currentPrimary := activeCampaign("g1", "Game One")
	currentPrimary.ID = "current-primary"
	overlapPrimary := activeCampaign("g2", "Game Two")
	overlapPrimary.ID = "overlap-primary"
	overlapSecondary := activeCampaign("g2", "Game Two")
	overlapSecondary.ID = "overlap-secondary"
	provider := &fakeCampaigns{campaigns: []*models.Campaign{currentPrimary, overlapPrimary, overlapSecondary}}
	m := newTestManager([]string{"Game One", "Game Two"}, provider, &fakeClient{})

	current := onlineCandidate("current_channel", "1", "Game One", "g1", 9000)
	current.Streamer.Stream.SetCampaignIDs([]string{currentPrimary.ID})
	overlap := onlineCandidate("overlap_channel", "2", "Game Two", "g2", 10)
	overlap.Streamer.Stream.SetCampaignIDs([]string{overlapPrimary.ID, overlapSecondary.ID})
	m.current = current
	m.pool = []*Channel{current, overlap}
	published := &recordingCandidatePolicy{}
	m.SetSlotStatus(published)

	ranks := map[string]int{"game one": 0, "game two": 0}
	m.SetCampaignPolicy(ranks, map[string]policy.CampaignSemantic{
		currentPrimary.ID:   {SemanticClass: 0, SecondaryEligible: true},
		overlapPrimary.ID:   {SemanticClass: 0, SecondaryEligible: true},
		overlapSecondary.ID: {SemanticClass: 4},
	})
	if got := m.WatchCandidates(); len(got) != 1 || got[0].Streamer != current.Streamer {
		t.Fatalf("ineligible secondary changed equal-primary continuity: %v", candidateLogins(got))
	}

	// The only changed fact is fail-closed secondary eligibility. The snapshot
	// equality check must observe it, and exact candidate comparison must then
	// prefer the overlap without a directory/API refresh.
	m.SetCampaignPolicy(ranks, map[string]policy.CampaignSemantic{
		currentPrimary.ID:   {SemanticClass: 0, SecondaryEligible: true},
		overlapPrimary.ID:   {SemanticClass: 0, SecondaryEligible: true},
		overlapSecondary.ID: {SemanticClass: 4, SecondaryEligible: true},
	})
	got := m.WatchCandidates()
	if len(got) != 1 || got[0].Streamer != overlap.Streamer {
		t.Fatalf("equal-primary overlap proposal = %v, want [overlap_channel]", candidateLogins(got))
	}
	if published.login != overlap.Streamer.GetUsername() || !published.facts.Ranked ||
		!published.facts.Utility.HasSecondary || published.facts.Utility.SecondarySemanticClass != 4 {
		t.Fatalf("bounded secondary publication = login %q facts %+v", published.login, published.facts)
	}
}

func TestWatchCandidatesCompletedSecondaryProvidesNoUtility(t *testing.T) {
	currentPrimary := activeCampaign("g1", "Game One")
	currentPrimary.ID = "current-primary"
	overlapPrimary := activeCampaign("g2", "Game Two")
	overlapPrimary.ID = "overlap-primary"
	completedSecondary := activeCampaign("g2", "Game Two")
	completedSecondary.ID = "completed-secondary"
	completedSecondary.Drops[0].CurrentMinutesWatched = completedSecondary.Drops[0].MinutesRequired
	provider := &fakeCampaigns{campaigns: []*models.Campaign{currentPrimary, overlapPrimary, completedSecondary}}
	m := newTestManager([]string{"Game One", "Game Two"}, provider, &fakeClient{})

	current := onlineCandidate("current_channel", "1", "Game One", "g1", 9000)
	current.Streamer.Stream.SetCampaignIDs([]string{currentPrimary.ID})
	overlap := onlineCandidate("completed_overlap", "2", "Game Two", "g2", 10)
	overlap.Streamer.Stream.SetCampaignIDs([]string{overlapPrimary.ID, completedSecondary.ID})
	m.current = current
	m.pool = []*Channel{current, overlap}
	published := &recordingCandidatePolicy{}
	m.SetSlotStatus(published)

	m.SetCampaignPolicy(map[string]int{"game one": 0, "game two": 0}, map[string]policy.CampaignSemantic{
		currentPrimary.ID:     {SemanticClass: 0, SecondaryEligible: true},
		overlapPrimary.ID:     {SemanticClass: 0, SecondaryEligible: true},
		completedSecondary.ID: {SemanticClass: 4, SecondaryEligible: true},
	})
	got := m.WatchCandidates()
	if len(got) != 1 || got[0].Streamer != current.Streamer {
		t.Fatalf("completed secondary changed equal-primary continuity: %v", candidateLogins(got))
	}
	if published.login != current.Streamer.GetUsername() || published.facts.Utility.HasSecondary {
		t.Fatalf("completed secondary retained positive discovery utility: login=%q facts=%+v", published.login, published.facts)
	}
}

func TestWatchCandidatesExactEqualClassKeepsCurrentContinuity(t *testing.T) {
	one := activeCampaign("g1", "Game One")
	two := activeCampaign("g2", "Game Two")
	provider := &fakeCampaigns{campaigns: []*models.Campaign{one, two}}
	// Game Two is configured first and has more viewers, so a fresh tie would
	// choose it. Those tie-breaks must not churn a valid equal-class current.
	m := newTestManager([]string{"Game Two", "Game One"}, provider, &fakeClient{})
	current := onlineCandidate("game_one_current", "1", "Game One", "g1", 100)
	other := onlineCandidate("game_two_other", "2", "Game Two", "g2", 9000)
	m.current = current
	m.pool = []*Channel{other, current}
	publishCampaignPolicy(m,
		map[string]int{"game one": 0, "game two": 0},
		map[string]policy.SemanticClass{one.ID: 0, two.ID: 0},
	)

	for i := 0; i < 3; i++ {
		got := m.WatchCandidates()
		if len(got) != 1 || got[0].Streamer != current.Streamer {
			t.Fatalf("equal-class iteration %d proposal = %v, want current continuity", i, candidateLogins(got))
		}
	}
}

func TestWatchCandidatesCarriesRestrictedHardFact(t *testing.T) {
	restricted := activeCampaign("g1", "Game One")
	restricted.ID = "restricted-campaign"
	restricted.Channels = []string{"allowed-channel"}
	provider := &fakeCampaigns{campaigns: []*models.Campaign{restricted}}
	m := newTestManager([]string{"Game One"}, provider, &fakeClient{})
	allowed := onlineCandidate("allowed", "allowed-channel", "Game One", "g1", 100)
	allowed.Streamer.Stream.SetCampaignIDs([]string{restricted.ID})
	m.pool = []*Channel{allowed}
	published := &recordingCandidatePolicy{}
	m.SetSlotStatus(published)
	publishCampaignPolicy(m,
		map[string]int{"game one": 0},
		map[string]policy.SemanticClass{restricted.ID: 0},
	)

	got := m.WatchCandidates()
	if len(got) != 1 {
		t.Fatalf("restricted proposal = %v, want one candidate", candidateLogins(got))
	}
	if published.login != allowed.Streamer.GetUsername() || !published.facts.Restricted {
		t.Fatal("allowed restricted discovery campaign did not carry its hard restricted fact")
	}
	if !got[0].Streamer.HasChannelRestrictedCampaign() {
		t.Fatal("verified discovery streamer did not receive its exact eligible restricted campaign")
	}
}

// TestWatchCandidatesFailsClosedOnUnknownCampaignACL proves that an
// advertised campaign whose allowlist could not be authoritatively loaded is
// not a discovery eligibility source. ACLUnknown is deliberately not
// "restricted" (so it cannot gain restricted priority), but AllowsChannel is
// still the crediting gate and must fail closed.
func TestWatchCandidatesFailsClosedOnUnknownCampaignACL(t *testing.T) {
	unknown := activeCampaign("g1", "Game One")
	unknown.ID = "unknown-acl"
	unknown.ACL = models.CampaignACL{
		State:  models.ACLUnknown,
		Source: models.ACLSourceCampaignDetails,
	}
	allowed := activeCampaign("g2", "Game Two")
	allowed.ID = "allowed-unrestricted"
	provider := &fakeCampaigns{campaigns: []*models.Campaign{unknown, allowed}}
	m := newTestManager([]string{"Game One", "Game Two"}, provider, &fakeClient{})

	unknownChannel := onlineCandidate("unknown_acl", "1", "Game One", "g1", 9000)
	unknownChannel.Streamer.Stream.SetCampaignIDs([]string{unknown.ID})
	allowedChannel := onlineCandidate("allowed", "2", "Game Two", "g2", 100)
	allowedChannel.Streamer.Stream.SetCampaignIDs([]string{allowed.ID})
	m.pool = []*Channel{unknownChannel, allowedChannel}
	publishCampaignPolicy(m,
		map[string]int{"game one": 0, "game two": 1},
		map[string]policy.SemanticClass{unknown.ID: 0, allowed.ID: 1},
	)

	got := m.WatchCandidates()
	if len(got) != 1 || got[0].Streamer != allowedChannel.Streamer {
		t.Fatalf("ACLUnknown discovery proposal = %v, want fail-closed [allowed]", candidateLogins(got))
	}
	if len(unknownChannel.Streamer.Stream.GetCampaigns()) != 0 {
		t.Fatal("ACLUnknown campaign was assigned to the ephemeral discovery streamer")
	}
}

func TestWatchCandidatesKeepsCurrentForEqualOrWeakerSemanticRank(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ranks map[string]int
	}{
		{name: "equal", ranks: map[string]int{"game one": 0, "game two": 0}},
		{name: "weaker", ranks: map[string]int{"game one": 0, "game two": 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &fakeCampaigns{campaigns: []*models.Campaign{
				activeCampaign("g1", "Game One"),
				activeCampaign("g2", "Game Two"),
			}}
			// Game Two is configured first and has more viewers. Once Game One
			// is current, neither tie-break is allowed to manufacture a switch
			// unless Game Two's semantic class is strictly stronger.
			client := &fakeClient{}
			m := newTestManager([]string{"Game Two", "Game One"}, provider, client)
			g1 := onlineCandidate("game_one_channel", "1", "Game One", "g1", 100)
			g2 := onlineCandidate("game_two_channel", "2", "Game Two", "g2", 9000)
			m.pool = []*Channel{g2, g1}
			m.SetGameRanks(map[string]int{"game one": 0, "game two": 1})
			if got := m.WatchCandidates(); len(got) != 1 || got[0].Streamer != g1.Streamer {
				t.Fatalf("initial proposal = %v, want Game One", candidateLogins(got))
			}

			m.SetGameRanks(tc.ranks)
			for i := 0; i < 3; i++ {
				got := m.WatchCandidates()
				if len(got) != 1 || got[0].Streamer != g1.Streamer {
					t.Fatalf("iteration %d proposal = %v, want continuity on Game One", i, candidateLogins(got))
				}
			}
			if len(client.checked) != 0 {
				t.Fatalf("equal/weaker policy refresh performed online checks: %v", client.checked)
			}
		})
	}
}

func TestWatchCandidatesEquivalentRankPublicationsAreIdempotent(t *testing.T) {
	provider := &fakeCampaigns{campaigns: []*models.Campaign{
		activeCampaign("g1", "Game One"),
		activeCampaign("g2", "Game Two"),
	}}
	client := &fakeClient{}
	m := newTestManager([]string{"Game One", "Game Two"}, provider, client)
	g1 := onlineCandidate("game_one_channel", "1", "Game One", "g1", 100)
	g2 := onlineCandidate("game_two_channel", "2", "Game Two", "g2", 9000)
	m.pool = []*Channel{g1, g2}
	m.SetGameRanks(map[string]int{"game one": 0, "game two": 1})
	if got := m.WatchCandidates(); len(got) != 1 || got[0].Streamer != g1.Streamer {
		t.Fatalf("initial proposal = %v, want Game One", candidateLogins(got))
	}

	for i := 0; i < 5; i++ {
		m.SetGameRanks(map[string]int{"game one": 0, "game two": 1})
		if got := m.WatchCandidates(); len(got) != 1 || got[0].Streamer != g1.Streamer {
			t.Fatalf("identical publication %d changed current to %v", i, candidateLogins(got))
		}
	}
	// The ordinal values changed, but their ordering relation did not.
	m.SetGameRanks(map[string]int{"game one": 10, "game two": 11})
	if got := m.WatchCandidates(); len(got) != 1 || got[0].Streamer != g1.Streamer {
		t.Fatalf("order-equivalent publication changed current to %v", candidateLogins(got))
	}
	if len(client.checked) != 0 {
		t.Fatalf("idempotent policy publications performed online checks: %v", client.checked)
	}
}

func TestWatchCandidatesPolicyModeTransitionsIncludeConfiguredGameOrder(t *testing.T) {
	provider := &fakeCampaigns{campaigns: []*models.Campaign{
		activeCampaign("g1", "Game One"),
		activeCampaign("g2", "Game Two"),
	}}
	m := newTestManager([]string{"Game One", "Game Two"}, provider, &fakeClient{})
	g1 := onlineCandidate("game_one_channel", "1", "Game One", "g1", 100)
	g2 := onlineCandidate("game_two_channel", "2", "Game Two", "g2", 9000)
	m.pool = []*Channel{g2, g1} // deliberately not configured order

	// nil is GAME_ORDER fallback: configured Game One wins even though the
	// cached pool order and viewer count favor Game Two.
	if got := m.WatchCandidates(); len(got) != 1 || got[0].Streamer != g1.Streamer {
		t.Fatalf("GAME_ORDER proposal = %v, want Game One", candidateLogins(got))
	}

	// GAME_ORDER -> ranked policy, then ranked -> another ranked mode.
	m.SetGameRanks(map[string]int{"game one": 1, "game two": 0})
	if got := m.WatchCandidates(); len(got) != 1 || got[0].Streamer != g2.Streamer {
		t.Fatalf("ranked transition proposal = %v, want Game Two", candidateLogins(got))
	}
	m.SetGameRanks(map[string]int{"game one": 0, "game two": 1})
	if got := m.WatchCandidates(); len(got) != 1 || got[0].Streamer != g1.Streamer {
		t.Fatalf("ranked-to-ranked proposal = %v, want Game One", candidateLogins(got))
	}

	// Ranked policy -> GAME_ORDER restores the configured semantic order. G1
	// is already that winner, so continuity is preserved.
	m.SetGameRanks(nil)
	if got := m.WatchCandidates(); len(got) != 1 || got[0].Streamer != g1.Streamer {
		t.Fatalf("return to GAME_ORDER proposal = %v, want Game One", candidateLogins(got))
	}
}

func TestWatchCandidatesSeesStrongerCandidateAddedAfterRankPublication(t *testing.T) {
	provider := &fakeCampaigns{campaigns: []*models.Campaign{
		activeCampaign("g1", "Game One"),
		activeCampaign("g2", "Game Two"),
	}}
	m := newTestManager([]string{"Game One", "Game Two"}, provider, &fakeClient{})
	g1 := onlineCandidate("game_one_channel", "1", "Game One", "g1", 100)
	g2 := onlineCandidate("game_two_channel", "2", "Game Two", "g2", 9000)
	m.pool = []*Channel{g1}
	m.SetGameRanks(map[string]int{"game one": 1, "game two": 0})
	if got := m.WatchCandidates(); len(got) != 1 || got[0].Streamer != g1.Streamer {
		t.Fatalf("fallback proposal = %v, want available Game One", candidateLogins(got))
	}

	// A later sync can add an already-known eligible candidate without another
	// policy publication. The current proposal must still converge.
	m.mu.Lock()
	m.pool = []*Channel{g1, g2}
	m.mu.Unlock()
	if got := m.WatchCandidates(); len(got) != 1 || got[0].Streamer != g2.Streamer {
		t.Fatalf("proposal after stronger pool arrival = %v, want Game Two", candidateLogins(got))
	}
}

type exactPolicyVerificationClient struct {
	fakeClient
	campaignIDs map[string][]string
}

func (c *exactPolicyVerificationClient) CheckStreamerOnline(streamer *models.Streamer) models.StatusTransition {
	c.checked = append(c.checked, streamer.GetUsername())
	streamer.Stream.SetCampaignIDs(c.campaignIDs[streamer.GetUsername()])
	return streamer.SetConfirmedOnline()
}

// TestWatchCandidatesCarriesClassFromSameTickVerification closes the late-pool
// seam: the candidate had no campaign IDs when policy was published, receives
// them during this WatchCandidates call, and must return its exact class in the
// same Candidate snapshot rather than waiting for the next miner refresh.
func TestWatchCandidatesCarriesClassFromSameTickVerification(t *testing.T) {
	weak := activeCampaign("g1", "Game One")
	weak.ID = "weak-current"
	strong := activeCampaign("g2", "Game Two")
	strong.ID = "strong-late"
	provider := &fakeCampaigns{campaigns: []*models.Campaign{weak, strong}}
	client := &exactPolicyVerificationClient{campaignIDs: map[string][]string{
		"strong_late": {strong.ID},
	}}
	m := newTestManager([]string{"Game One", "Game Two"}, provider, &fakeClient{})
	m.client = client

	current := onlineCandidate("weak_current", "1", "Game One", "g1", 9000)
	current.Streamer.Stream.SetCampaignIDs([]string{weak.ID})
	late := onlineCandidate("strong_late", "2", "Game Two", "g2", 100)
	late.Streamer.Stream.SetCampaignIDs(nil)
	late.Streamer.SetConfirmedOffline()
	m.current = current
	m.pool = []*Channel{current, late}
	published := &recordingCandidatePolicy{}
	m.SetSlotStatus(published)
	publishCampaignPolicy(m,
		map[string]int{"game one": 1, "game two": 0},
		map[string]policy.SemanticClass{weak.ID: 2, strong.ID: 0},
	)

	got := m.WatchCandidates()
	if len(got) != 1 || got[0].Streamer != late.Streamer {
		t.Fatalf("same-tick verified proposal = %v, want [strong_late]", candidateLogins(got))
	}
	if published.login != late.Streamer.GetUsername() || !published.facts.Ranked || published.facts.Utility.SemanticClass != 0 {
		t.Fatalf("same-tick verified publication = login %q facts %+v, want strong_late class 0", published.login, published.facts)
	}
	if len(client.checked) != 1 || client.checked[0] != late.Streamer.GetUsername() {
		t.Fatalf("verification calls = %v, want one bounded check for strong_late", client.checked)
	}
}

func TestWatchCandidatesUsesBrokerSemanticSnapshotAsProductionOwner(t *testing.T) {
	weak := activeCampaign("g1", "Game One")
	weak.ID = "weak-current"
	strong := activeCampaign("g2", "Game Two")
	strong.ID = "strong-broker"
	provider := &fakeCampaigns{campaigns: []*models.Campaign{weak, strong}}
	m := newTestManager([]string{"Game One", "Game Two"}, provider, &fakeClient{})
	current := onlineCandidate("weak_current", "1", "Game One", "g1", 9000)
	current.Streamer.Stream.SetCampaignIDs([]string{weak.ID})
	better := onlineCandidate("strong_broker", "2", "Game Two", "g2", 10)
	better.Streamer.Stream.SetCampaignIDs([]string{strong.ID})
	m.current = current
	m.pool = []*Channel{current, better}

	// Deliberately stale/opposite local publication. Production must use the
	// broker-owned snapshot exposed through SetSlotStatus, not this mirror.
	m.SetCampaignPolicy(
		map[string]int{"game one": 0, "game two": 1},
		testCampaignSemantics(map[string]policy.SemanticClass{weak.ID: 0, strong.ID: 2}),
	)
	published := &recordingCandidatePolicy{
		gameRanks:       map[string]int{"game one": 1, "game two": 0},
		campaignClasses: map[string]policy.SemanticClass{weak.ID: 2, strong.ID: 0},
	}
	m.SetSlotStatus(published)

	got := m.WatchCandidates()
	if len(got) != 1 || got[0].Streamer != better.Streamer {
		t.Fatalf("broker-owned semantic proposal = %v, want [strong_broker]", candidateLogins(got))
	}
	if published.login != better.Streamer.GetUsername() || !published.facts.Ranked || published.facts.Utility.SemanticClass != 0 {
		t.Fatalf("broker-owned semantic publication = login %q facts %+v, want strong_broker class 0", published.login, published.facts)
	}
	if len(published.facts.CampaignIDs) != 1 || published.facts.CampaignIDs[0] != strong.ID {
		t.Fatalf("exact carried campaign IDs = %v, want [%s]", published.facts.CampaignIDs, strong.ID)
	}
}

type transientPolicyClient struct {
	fakeClient
	attempts int
}

func (c *transientPolicyClient) CheckStreamerOnline(streamer *models.Streamer) models.StatusTransition {
	c.attempts++
	if c.attempts == 1 {
		return streamer.SetUnknown(models.ReasonTransportError)
	}
	return streamer.SetConfirmedOnline()
}

func TestPolicyPreemptionRetriesTransientlyUnknownStrongerCandidate(t *testing.T) {
	provider := &fakeCampaigns{campaigns: []*models.Campaign{
		activeCampaign("g1", "Game One"),
		activeCampaign("g2", "Game Two"),
	}}
	client := &transientPolicyClient{}
	m := newTestManager([]string{"Game One", "Game Two"}, provider, &fakeClient{})
	m.client = client
	g1 := onlineCandidate("game_one_channel", "1", "Game One", "g1", 100)
	g2 := onlineCandidate("game_two_channel", "2", "Game Two", "g2", 9000)
	g2.Streamer.SetConfirmedOffline()
	m.current = g1
	m.pool = []*Channel{g1, g2}
	m.SetGameRanks(map[string]int{"game one": 1, "game two": 0})

	first := m.WatchCandidates()
	if len(first) != 1 || first[0].Streamer != g1.Streamer {
		t.Fatalf("transient UNKNOWN displaced current without confirmation: %v", candidateLogins(first))
	}
	second := m.WatchCandidates()
	if len(second) != 1 || second[0].Streamer != g2.Streamer {
		t.Fatalf("unchanged ranks did not retry and converge stronger candidate: %v", candidateLogins(second))
	}
	if client.attempts != 2 {
		t.Fatalf("online verification attempts = %d, want bounded retry count 2", client.attempts)
	}
}

func TestPolicyPreemptionHonorsDiscoveryEligibility(t *testing.T) {
	restricted := activeCampaign("g2", "Game Two")
	restricted.ID = "restricted-g2"
	restricted.Channels = []string{"different-channel-id"}
	provider := &fakeCampaigns{campaigns: []*models.Campaign{
		activeCampaign("g1", "Game One"),
		activeCampaign("g2", "Game Two"),
		restricted,
	}}
	m := newTestManager([]string{"Game One", "Game Two"}, provider, &fakeClient{})
	current := onlineCandidate("current_game_one", "1", "Game One", "g1", 100)
	offline := onlineCandidate("offline_game_two", "2", "Game Two", "g2", 9000)
	offline.offline = true
	missingCampaign := onlineCandidate("missing_campaign", "3", "Game Two", "g2", 8000)
	missingCampaign.Streamer.Stream.SetCampaignIDs(nil)
	wrongGame := onlineCandidate("wrong_game", "4", "Game Two", "g2", 7000)
	wrongGame.Streamer.Stream.Update("broadcast-wrong", "title", &models.Game{ID: "other", Name: "Other"}, nil, 7000)
	avoided := onlineCandidate("avoided_game_two", "5", "Game Two", "g2", 6000)
	tracked := onlineCandidate("tracked_game_two", "6", "Game Two", "g2", 5000)
	disallowed := onlineCandidate("disallowed_restricted", "7", "Game Two", "g2", 4000)
	disallowed.Streamer.Stream.SetCampaignIDs([]string{"restricted-g2"})
	eligible := onlineCandidate("eligible_game_two", "8", "Game Two", "g2", 10)

	m.current = current
	m.pool = []*Channel{current, offline, missingCampaign, wrongGame, avoided, tracked, disallowed, eligible}
	m.tracked = &fakeTracked{names: []string{tracked.Streamer.GetUsername()}}
	m.SetAvoidChecker(&staticAvoidChecker{avoided: map[string]bool{avoided.Streamer.GetUsername(): true}})
	m.SetGameRanks(map[string]int{"game one": 1, "game two": 0})

	got := m.WatchCandidates()
	if len(got) != 1 || got[0].Streamer != eligible.Streamer {
		t.Fatalf("strict semantic preemption bypassed discovery eligibility: got %v, want [eligible_game_two]", candidateLogins(got))
	}
}

func candidateLogins(candidates []watcher.Candidate) []string {
	logins := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Streamer != nil {
			logins = append(logins, candidate.Streamer.GetUsername())
		}
	}
	return logins
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
