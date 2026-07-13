package version

// Version is set at build time via -ldflags "-X github.com/KraineOpasen/bukerov-twitch-miner-go/internal/version.Version=..."
var Version = "dev"

// RepoURL is the GitHub repository URL
const RepoURL = "https://github.com/KraineOpasen/bukerov-twitch-miner-go"

// Repo is the "owner/name" GitHub repository slug, used for the Releases API
// when checking for auto-updates.
const Repo = "KraineOpasen/bukerov-twitch-miner-go"
