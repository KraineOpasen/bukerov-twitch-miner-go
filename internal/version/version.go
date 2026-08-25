package version

import (
	"fmt"
	"regexp"
	"runtime"
)

// Version is set at build time via -ldflags "-X github.com/KraineOpasen/bukerov-twitch-miner-go/internal/version.Version=..."
var Version = "dev"

// Channel identifies the release stream independently of Version. Stable
// images override this at build time; ordinary builds retain main behavior.
var Channel = "main"

// ArtifactIdentity is the single authoritative build-time identity for stable
// artifacts. Stable producers set it with -ldflags; init derives Version and
// Channel from the validated value so those fields cannot drift apart.
// Ordinary main/dev builds leave it empty and retain the legacy Version and
// Channel variables above.
var ArtifactIdentity string

// StableArtifactPrefix identifies the one accepted stable binary marker.
const StableArtifactPrefix = "BTM_STABLE_ARTIFACT_V1"

var canonicalVersionRE = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
var stableArtifactRE = regexp.MustCompile(`^` + regexp.QuoteMeta(StableArtifactPrefix) +
	`\|VERSION=([^|]+)\|CHANNEL=stable\|GOOS=([^|]+)\|GOARCH=([^|]+)$`)

// BuildIdentity is the parsed, platform-bound identity embedded in a stable
// executable.
type BuildIdentity struct {
	Version string
	Channel string
	GOOS    string
	GOARCH  string
}

// StableArtifactIdentity returns the exact marker both the stable producer
// embeds and the stable updater requires in a downloaded candidate.
func StableArtifactIdentity(publicVersion, goos, goarch string) string {
	return fmt.Sprintf("%s|VERSION=%s|CHANNEL=stable|GOOS=%s|GOARCH=%s",
		StableArtifactPrefix, publicVersion, goos, goarch)
}

// ParseArtifactIdentity validates the exact stable build marker. There are no
// optional fields or aliases: a main-channel or platform-mismatched artifact
// can never be normalized into a stable identity.
func ParseArtifactIdentity(value string) (BuildIdentity, error) {
	m := stableArtifactRE.FindStringSubmatch(value)
	if m == nil {
		return BuildIdentity{}, fmt.Errorf("invalid stable artifact identity")
	}
	out := BuildIdentity{Version: m[1], Channel: "stable", GOOS: m[2], GOARCH: m[3]}
	if !canonicalVersionRE.MatchString(out.Version) {
		return BuildIdentity{}, fmt.Errorf("invalid canonical stable version %q", out.Version)
	}
	if out.GOOS == "" || out.GOARCH == "" {
		return BuildIdentity{}, fmt.Errorf("stable artifact platform is empty")
	}
	if value != StableArtifactIdentity(out.Version, out.GOOS, out.GOARCH) {
		return BuildIdentity{}, fmt.Errorf("non-canonical stable artifact identity")
	}
	return out, nil
}

func init() {
	if ArtifactIdentity == "" {
		return
	}
	identity, err := ParseArtifactIdentity(ArtifactIdentity)
	if err != nil {
		panic(err)
	}
	if identity.GOOS != runtime.GOOS || identity.GOARCH != runtime.GOARCH {
		panic(fmt.Sprintf("stable artifact identity platform %s/%s does not match runtime %s/%s",
			identity.GOOS, identity.GOARCH, runtime.GOOS, runtime.GOARCH))
	}
	Version = identity.Version
	Channel = identity.Channel
}

// RepoURL is the GitHub repository URL
const RepoURL = "https://github.com/KraineOpasen/bukerov-twitch-miner-go"

// Repo is the "owner/name" GitHub repository slug, used for the Releases API
// when checking for auto-updates.
const Repo = "KraineOpasen/bukerov-twitch-miner-go"
