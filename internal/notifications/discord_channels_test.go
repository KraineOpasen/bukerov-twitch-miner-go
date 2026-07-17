package notifications

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// guildTextChannel builds a text channel the provider will keep.
func guildTextChannel(id string) *discordgo.Channel {
	return &discordgo.Channel{ID: id, Name: id, Type: discordgo.ChannelTypeGuildText}
}

func channelIDs(chs []Channel) []string {
	out := make([]string, len(chs))
	for i, c := range chs {
		out[i] = c.ID
	}
	return out
}

func oneChannel(t *testing.T, chs []Channel, wantID string) {
	t.Helper()
	if got := channelIDs(chs); len(got) != 1 || got[0] != wantID {
		t.Fatalf("want channels [%q], got %v", wantID, got)
	}
}

type channelsResult struct {
	ch  []Channel
	err error
}

// attach a non-nil session so GetChannels proceeds to the fetch path.
func withSession(p *DiscordProvider, s *discordgo.Session) {
	p.mu.Lock()
	p.session = s
	p.mu.Unlock()
}

// --- T1: a stale response for the OLD guild is discarded, retry serves the new guild ---

func TestDiscordGetChannelsDiscardsStaleGuildResponse(t *testing.T) {
	p := NewDiscordProvider("token-old", "guild-old")
	withSession(p, &discordgo.Session{})

	started := make(chan struct{})
	release := make(chan struct{})
	var calls int32
	p.fetchGuildChannels = func(_ *discordgo.Session, guildID string) ([]*discordgo.Channel, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			close(started)
			<-release
			return []*discordgo.Channel{guildTextChannel("chan-" + guildID)}, nil // resolves for guild-old
		}
		return []*discordgo.Channel{guildTextChannel("chan-" + guildID)}, nil
	}

	done := make(chan channelsResult, 1)
	go func() {
		ch, err := p.GetChannels(context.Background(), false)
		done <- channelsResult{ch, err}
	}()

	<-started                                // old request is in flight against guild-old
	p.UpdateConfig("token-old", "guild-new") // generation bumps, cache cleared
	close(release)                           // old request completes -> stale

	r := <-done
	if r.err != nil {
		t.Fatalf("GetChannels: %v", r.err)
	}
	oneChannel(t, r.ch, "chan-guild-new") // never the old guild's channel
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("want exactly 2 fetches (stale + retry), got %d", n)
	}

	p.mu.RLock()
	cache := channelIDs(p.channelCache)
	sameGen := p.channelCacheGeneration == p.configGeneration
	p.mu.RUnlock()
	if len(cache) != 1 || cache[0] != "chan-guild-new" {
		t.Fatalf("cache must hold only the new guild's channels, got %v", cache)
	}
	if !sameGen {
		t.Fatalf("cached channels must belong to the current config generation")
	}
}

// --- T2: token change (same guild) is caught by the generation guard, not a guild compare ---

func TestDiscordGetChannelsDiscardsStaleTokenGeneration(t *testing.T) {
	p := NewDiscordProvider("token-old", "guild")
	withSession(p, &discordgo.Session{})

	started := make(chan struct{})
	release := make(chan struct{})
	var calls int32
	p.fetchGuildChannels = func(_ *discordgo.Session, _ string) ([]*discordgo.Channel, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			close(started)
			<-release
			return []*discordgo.Channel{guildTextChannel("from-old-token")}, nil
		}
		return []*discordgo.Channel{guildTextChannel("from-new-token")}, nil
	}

	done := make(chan channelsResult, 1)
	go func() {
		ch, err := p.GetChannels(context.Background(), false)
		done <- channelsResult{ch, err}
	}()

	<-started
	p.UpdateConfig("token-new", "guild") // guild unchanged; only generation guard can catch this
	close(release)

	r := <-done
	if r.err != nil {
		t.Fatalf("GetChannels: %v", r.err)
	}
	oneChannel(t, r.ch, "from-new-token")
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("want 2 fetches, got %d", n)
	}
}

// --- T3: a result from a replaced session is rejected even when token/guild are unchanged ---

func TestDiscordGetChannelsRejectsResultFromReplacedSession(t *testing.T) {
	p := NewDiscordProvider("token", "guild")
	sessA := &discordgo.Session{}
	sessB := &discordgo.Session{}
	withSession(p, sessA)

	started := make(chan struct{})
	release := make(chan struct{})
	var calls int32
	p.fetchGuildChannels = func(s *discordgo.Session, _ string) ([]*discordgo.Channel, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			close(started)
			<-release
			return []*discordgo.Channel{guildTextChannel("from-session-A")}, nil
		}
		return []*discordgo.Channel{guildTextChannel("from-session-B")}, nil
	}

	done := make(chan channelsResult, 1)
	go func() {
		ch, err := p.GetChannels(context.Background(), false)
		done <- channelsResult{ch, err}
	}()

	<-started
	withSession(p, sessB) // session replaced; generation unchanged
	close(release)

	r := <-done
	if r.err != nil {
		t.Fatalf("GetChannels: %v", r.err)
	}
	oneChannel(t, r.ch, "from-session-B")
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("want 2 fetches, got %d", n)
	}
}

// --- T4: when every attempt is stale, retry is bounded and returns a retryable error ---

func TestDiscordGetChannelsStaleRetryIsBounded(t *testing.T) {
	p := NewDiscordProvider("token", "guild-0")
	withSession(p, &discordgo.Session{})

	var calls int32
	// Each fetch invalidates its own snapshot by bumping the generation before it
	// returns, so every attempt is stale. No goroutine/barrier needed and no hang.
	p.fetchGuildChannels = func(_ *discordgo.Session, _ string) ([]*discordgo.Channel, error) {
		n := atomic.AddInt32(&calls, 1)
		p.UpdateConfig("token", "guild-"+strconv.Itoa(int(n)))
		return []*discordgo.Channel{guildTextChannel("stale")}, nil
	}

	_, err := p.GetChannels(context.Background(), false)
	if !errors.Is(err, errChannelConfigChanged) {
		t.Fatalf("want errChannelConfigChanged, got %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("retry must be bounded to 2 fetches, got %d", n)
	}
	p.mu.RLock()
	emptyCache := p.channelCache == nil
	p.mu.RUnlock()
	if !emptyCache {
		t.Fatalf("a never-current result must not populate the cache")
	}
}

// --- T5: a current result is cached; a second call is served from cache (no network) ---

func TestDiscordGetChannelsCachesCurrentGeneration(t *testing.T) {
	p := NewDiscordProvider("token", "guild")
	withSession(p, &discordgo.Session{})

	var calls int32
	p.fetchGuildChannels = func(_ *discordgo.Session, _ string) ([]*discordgo.Channel, error) {
		atomic.AddInt32(&calls, 1)
		return []*discordgo.Channel{guildTextChannel("c1")}, nil
	}

	ch1, err := p.GetChannels(context.Background(), false)
	if err != nil {
		t.Fatalf("GetChannels: %v", err)
	}
	oneChannel(t, ch1, "c1")
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("first call must fetch once, got %d", n)
	}

	p.mu.RLock()
	sameGen := p.channelCacheGeneration == p.configGeneration
	p.mu.RUnlock()
	if !sameGen {
		t.Fatalf("cache generation must match config generation")
	}

	ch2, err := p.GetChannels(context.Background(), false)
	if err != nil {
		t.Fatalf("GetChannels(cache): %v", err)
	}
	oneChannel(t, ch2, "c1")
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("second call must hit cache (no fetch), got %d fetches", n)
	}
}

// --- T6: forceRefresh bypasses a valid cache and refetches the current generation ---

func TestDiscordGetChannelsForceRefreshCurrentGeneration(t *testing.T) {
	p := NewDiscordProvider("token", "guild")
	withSession(p, &discordgo.Session{})

	var calls int32
	p.fetchGuildChannels = func(_ *discordgo.Session, _ string) ([]*discordgo.Channel, error) {
		atomic.AddInt32(&calls, 1)
		return []*discordgo.Channel{guildTextChannel("c1")}, nil
	}

	if _, err := p.GetChannels(context.Background(), false); err != nil { // caches
		t.Fatalf("GetChannels: %v", err)
	}
	if _, err := p.GetChannels(context.Background(), true); err != nil { // forced
		t.Fatalf("GetChannels(force): %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("forceRefresh must refetch despite a valid cache, got %d fetches", n)
	}
}

// --- T7: ValidateConfig snapshots credentials under the lock; the network op runs unlocked ---

func TestDiscordValidateConfigSnapshotsCredentialsUnderLock(t *testing.T) {
	p := NewDiscordProvider("token-abc", "guild-xyz")

	muFree := false
	tokenOK := false
	var gotGuild string
	p.validateConfig = func(_ context.Context, botToken, guildID string) error {
		if p.mu.TryLock() { // must be free: network never runs under d.mu
			muFree = true
			p.mu.Unlock()
		}
		tokenOK = botToken == "token-abc" // consistent snapshot; do not print the token
		gotGuild = guildID
		return nil
	}

	if err := p.ValidateConfig(context.Background()); err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
	if !muFree {
		t.Fatalf("d.mu must not be held while the validation network op runs")
	}
	if !tokenOK {
		t.Fatalf("validateConfig received a wrong or torn bot token")
	}
	if gotGuild != "guild-xyz" {
		t.Fatalf("validateConfig guild = %q, want guild-xyz", gotGuild)
	}
}

func TestDiscordValidateConfigNoRaceWithUpdateConfig(t *testing.T) {
	p := NewDiscordProvider("t0", "g0")
	p.validateConfig = func(_ context.Context, _, _ string) error { return nil }

	done := make(chan struct{})
	go func() {
		for i := 0; i < 200; i++ {
			p.UpdateConfig("t"+strconv.Itoa(i), "g"+strconv.Itoa(i))
		}
		close(done)
	}()
	for i := 0; i < 200; i++ {
		_ = p.ValidateConfig(context.Background())
	}
	<-done
}
