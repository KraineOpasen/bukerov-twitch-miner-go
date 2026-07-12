package constants

type GQLOperation struct {
	OperationName string                 `json:"operationName"`
	Variables     map[string]interface{} `json:"variables,omitempty"`
	Extensions    GQLExtensions          `json:"extensions"`
}

type GQLExtensions struct {
	PersistedQuery GQLPersistedQuery `json:"persistedQuery"`
}

type GQLPersistedQuery struct {
	Version    int    `json:"version"`
	SHA256Hash string `json:"sha256Hash"`
}

func NewGQLOperation(name, hash string) GQLOperation {
	return GQLOperation{
		OperationName: name,
		Extensions: GQLExtensions{
			PersistedQuery: GQLPersistedQuery{
				Version:    1,
				SHA256Hash: hash,
			},
		},
	}
}

func (g GQLOperation) WithVariables(vars map[string]interface{}) GQLOperation {
	g.Variables = vars
	return g
}

var (
	WithIsStreamLiveQuery = NewGQLOperation(
		"WithIsStreamLiveQuery",
		"04e46329a6786ff3a81c01c50bfa5d725902507a0deb83b0edbf7abe7a3716ea",
	)

	PlaybackAccessToken = NewGQLOperation(
		"PlaybackAccessToken",
		"3093517e37e4f4cb48906155bcd894150aef92617939236d2508f3375ab732ce",
	)

	VideoPlayerStreamInfoOverlayChannel = NewGQLOperation(
		"VideoPlayerStreamInfoOverlayChannel",
		"a5f2e34d626a9f4f5c0204f910bab2194948a9502089be558bb6e779a9e1b3d2",
	)

	ClaimCommunityPoints = NewGQLOperation(
		"ClaimCommunityPoints",
		"46aaeebe02c99afdf4fc97c7c0cba964124bf6b0af229395f1f6d1feed05b3d0",
	)

	CommunityMomentCalloutClaim = NewGQLOperation(
		"CommunityMomentCallout_Claim",
		"e2d67415aead910f7f9ceb45a77b750a1e1d9622c936d832328a0689e054db62",
	)

	DropsPageClaimDropRewards = NewGQLOperation(
		"DropsPage_ClaimDropRewards",
		"a455deea71bdc9015b78eb49f4acfbce8baa7ccbedd28e549bb025bd0f751930",
	)

	ChannelPointsContext = NewGQLOperation(
		"ChannelPointsContext",
		"1530a003a7d374b0380b79db0be0534f30ff46e61cffa2bc0e2468a909fbc024",
	)

	JoinRaid = NewGQLOperation(
		"JoinRaid",
		"c6a332a86d1087fbbb1a8623aa01bd1313d2386e7c63be60fdb2d1901f01a4ae",
	)

	Inventory = NewGQLOperation(
		"Inventory",
		"d86775d0ef16a63a33ad52e80eaff963b2d5b72fada7c991504a57496e1d8e4b",
	).WithVariables(map[string]interface{}{"fetchRewardCampaigns": true})

	MakePrediction = NewGQLOperation(
		"MakePrediction",
		"b44682ecc88358817009f20e69d75081b1e58825bb40aa53d5dbadcc17c881d8",
	)

	ViewerDropsDashboard = NewGQLOperation(
		"ViewerDropsDashboard",
		"5a4da2ab3d5b47c9f9ce864e727b2cb346af1e3ea8b897fe8f704a97ff017619",
	).WithVariables(map[string]interface{}{"fetchRewardCampaigns": true})

	DropCampaignDetails = NewGQLOperation(
		"DropCampaignDetails",
		"f6396f5ffdde867a8f6f6da18286e4baf02e5b98d14689a69b5af320a4c7b7b8",
	)

	DropsHighlightServiceAvailableDrops = NewGQLOperation(
		"DropsHighlightService_AvailableDrops",
		"9a62a09bce5b53e26e64a671e530bc599cb6aab1e5ba3cbd5d85966d3940716f",
	)

	GetIDFromLogin = NewGQLOperation(
		"GetIDFromLogin",
		"94e82a7b1e3c21e186daa73ee2afc4b8f23bade1fbbff6fe8ac133f50a2f58ca",
	)

	ChannelFollows = NewGQLOperation(
		"ChannelFollows",
		"eecf815273d3d949e5cf0085cc5084cd8a1b5b7b6f7990cf43cb0beadf546907",
	).WithVariables(map[string]interface{}{"limit": 100, "order": "ASC"})

	UserPointsContribution = NewGQLOperation(
		"UserPointsContribution",
		"23ff2c2d60708379131178742327ead913b93b1bd6f665517a6d9085b73f661f",
	)

	ContributeCommunityPointsCommunityGoal = NewGQLOperation(
		"ContributeCommunityPointsCommunityGoal",
		"5774f0ea5d89587d73021a2e03c3c44777d903840c608754a1be519f51e37bb6",
	)

	// RedeemCustomReward spends channel points on a streamer's custom
	// channel-points reward. Twitch renamed this operation server-side from
	// RedeemCommunityPointsCustomReward to RedeemCustomReward (and now requires
	// pricingType + prompt in the input); the mutation field in the response is
	// still redeemCommunityPointsCustomReward. Hash captured from the current
	// Twitch web client.
	RedeemCustomReward = NewGQLOperation(
		"RedeemCustomReward",
		"d56249a7adb4978898ea3412e196688d4ac3cea1c0c2dfd65561d229ea5dcc42",
	)

	// DirectoryPageGame lists live channels in a game's directory
	// (twitch.tv/directory/category/<slug>) — the operation reference drop
	// miners (DevilXD/TwitchDropsMiner and its forks) use to find
	// drops-enabled channels per game. Takes the game *slug* plus an options
	// object whose systemFilters: ["DROPS_ENABLED"] restricts results
	// server-side to streams with drops activated (there is no per-stream
	// drops flag in the response).
	//
	// Twitch rotates this operation's hash every few months (5 rotations
	// since 2024), sometimes changing the variable shape with it (the
	// current era sends includeCostreaming; the pre-2025 era sent
	// includeIsDJ). Hash below is in effect since 2026-05, cross-validated
	// against DevilXD master and a dozen independent miners. On rotation the
	// GQL client logs PersistedQueryNotFound loudly — update the hash (and
	// re-check the variables in internal/api/directory.go) from
	// DevilXD/TwitchDropsMiner's constants.py, the canonical tracker.
	DirectoryPageGame = NewGQLOperation(
		"DirectoryPage_Game",
		"cb5dc816e139dcb8a118f14b4b677d59abc224a4b016c4bc2bb00a47fe0ddec4",
	)

	// DirectoryGameRedirect resolves a game's display name to its directory
	// slug (query: game(name: $name) { id slug }). Used before
	// DirectoryPageGame since that operation only accepts slugs; a local
	// slugify fallback covers lookup failures. Hash unchanged since 2024.
	DirectoryGameRedirect = NewGQLOperation(
		"DirectoryGameRedirect",
		"1f0300090caceec51f33c5e20647aceff9017f740f223c3c532ba6fa59f6b6cc",
	)
)
