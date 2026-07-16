package constants

const (
	TwitchURL      = "https://www.twitch.tv"
	GQLURL         = "https://gql.twitch.tv/gql"
	PubSubURL      = "wss://pubsub-edge.twitch.tv/v1"
	OAuthDeviceURL = "https://id.twitch.tv/oauth2/device"
	OAuthTokenURL  = "https://id.twitch.tv/oauth2/token"
	IRCURL         = "irc.chat.twitch.tv"
	IRCPortTLS     = 6697
	UsherURL       = "https://usher.ttvnw.net"

	ClientIDTV      = "ue6666qo983tsx6so1t0vnawi233wa"
	ClientIDBrowser = "kimne78kx3ncx6brgo4mv6wki5h1ko"
	ClientIDMobile  = "r8s4dac0uhzifbpu9sjdiwzctle17ff"

	DefaultClientVersion = "ef928475-9403-42f2-8a34-55784bd08e16"

	TVUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/108.0.0.0 Safari/537.36"

	MaxTopicsPerConnection = 50
	MaxSimultaneousStreams = 2
)

var OAuthScopes = "channel_read chat:read user_blocks_edit user_blocks_read user_follows_edit user_read"

// GQLClientIDFallbacks is the ordered list of public Twitch client IDs the GQL
// client cycles through when a request fails with PersistedQueryNotFound. These
// are the well-known, non-secret client IDs shipped by Twitch's own web/TV/
// mobile clients (the same values every miner uses). Twitch periodically
// rotates or invalidates the persisted-query hashes tied to a given client ID,
// which breaks every GQL call for a hardcoded default; trying the alternates
// lets the miner recover without a code change. The default (ClientIDTV) is
// first so healthy requests keep using it.
var GQLClientIDFallbacks = []string{
	ClientIDTV,
	ClientIDBrowser,
	ClientIDMobile,
}
