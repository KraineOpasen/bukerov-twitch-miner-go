package updater

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	buildversion "github.com/KraineOpasen/bukerov-twitch-miner-go/internal/version"
)

const (
	stableReleasePageSize = 100
	// This is a defensive API bound, not a first-page shortcut: 1,000 full
	// pages cover 100,000 releases. Reaching it fails closed instead of
	// silently claiming a possibly non-maximal stable candidate.
	maxStableReleasePages = 1000

	// DefaultStableCacheDir lives under the existing durable database mount.
	// In the scratch stable image WORKDIR=/, so this resolves to
	// /database/.updater/stable without adding a mount or host helper.
	DefaultStableCacheDir  = "database/.updater/stable"
	maxStableChecksumsSize = 64 << 10
)

var (
	stableTagRE            = regexp.MustCompile(`^stable-v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	sha256DigestRE         = regexp.MustCompile(`^[0-9a-f]{64}$`)
	stableArtifactMarkerRE = regexp.MustCompile(regexp.QuoteMeta(buildversion.StableArtifactPrefix) +
		`\|VERSION=(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)` +
		`\|CHANNEL=stable\|GOOS=[a-z0-9]+\|GOARCH=[a-z0-9]+`)
)

var stableBinaryAssetNames = []string{
	"twitch-miner-go-linux-amd64",
	"twitch-miner-go-linux-arm64",
}

func parseStableTag(tag string) (string, semver, bool) {
	m := stableTagRE.FindStringSubmatch(tag)
	if m == nil {
		return "", semver{}, false
	}
	parts := make([]int, 3)
	for i := range parts {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return "", semver{}, false
		}
		parts[i] = n
	}
	public := strings.Join(m[1:], ".")
	return public, semver{major: parts[0], minor: parts[1], patch: parts[2]}, true
}

func stablePlatform(goos, goarch string) bool {
	return goos == "linux" && (goarch == "amd64" || goarch == "arm64")
}

// latestStableRelease exhausts the Releases collection before choosing a
// candidate. Repository-wide /releases/latest is intentionally never used:
// main tags cannot qualify, influence ordering, or hide a stable release on a
// later page.
func (u *Updater) latestStableRelease(ctx context.Context) (*release, error) {
	var best *release
	var bestVersion semver
	seen := make(map[string]struct{})

	for page := 1; page <= maxStableReleasePages; page++ {
		url := fmt.Sprintf("%s/repos/%s/releases?per_page=%d&page=%d",
			strings.TrimRight(u.opts.apiBaseURL, "/"), u.opts.Repo, stableReleasePageSize, page)
		var releases []release
		if err := u.getJSON(ctx, url, &releases); err != nil {
			return nil, fmt.Errorf("list stable releases page %d: %w", page, err)
		}

		for i := range releases {
			rel := &releases[i]
			public, parsed, ok := parseStableTag(rel.TagName)
			if !ok || rel.Draft || rel.Prerelease {
				continue
			}
			if _, duplicate := seen[rel.TagName]; duplicate {
				return nil, fmt.Errorf("duplicate stable release identity %q", rel.TagName)
			}
			seen[rel.TagName] = struct{}{}
			if err := validateStableReleaseAssets(rel); err != nil {
				// A public release is visible before its workflow uploads every
				// controlled asset. It remains ineligible until the producer set
				// is complete and every server digest is present.
				continue
			}
			rel.PublicVersion = public
			if best == nil || compareSemver(parsed, bestVersion) > 0 {
				copyRel := *rel
				copyRel.Assets = append([]asset(nil), rel.Assets...)
				best = &copyRel
				bestVersion = parsed
			}
		}

		if len(releases) < stableReleasePageSize {
			if best == nil {
				return nil, fmt.Errorf("no complete canonical stable release found")
			}
			return best, nil
		}
	}
	return nil, fmt.Errorf("stable release pagination exceeded %d pages; refusing a non-global maximum",
		maxStableReleasePages)
}

func validateStableReleaseAssets(rel *release) error {
	want := map[string]struct{}{
		stableBinaryAssetNames[0]: {},
		stableBinaryAssetNames[1]: {},
		"checksums.txt":           {},
	}
	if len(rel.Assets) != len(want) {
		return fmt.Errorf("stable release %s has %d assets, want exactly %d", rel.TagName, len(rel.Assets), len(want))
	}
	for i := range rel.Assets {
		a := &rel.Assets[i]
		if _, ok := want[a.Name]; !ok {
			return fmt.Errorf("stable release %s has unexpected asset %q", rel.TagName, a.Name)
		}
		delete(want, a.Name)
		if err := validateStableAssetMetadata(a); err != nil {
			return err
		}
	}
	if len(want) != 0 {
		return fmt.Errorf("stable release %s is missing controlled assets", rel.TagName)
	}
	return nil
}

func validateStableAssetMetadata(a *asset) error {
	if a.State != "uploaded" {
		return fmt.Errorf("stable asset %q state is %q, want uploaded", a.Name, a.State)
	}
	if a.Size <= 0 {
		return fmt.Errorf("stable asset %q has invalid size %d", a.Name, a.Size)
	}
	maxSize := int64(maxStableBinarySize)
	if a.Name == "checksums.txt" {
		maxSize = maxStableChecksumsSize
	}
	if a.Size > maxSize {
		return fmt.Errorf("stable asset %q size %d exceeds limit %d", a.Name, a.Size, maxSize)
	}
	if a.URL == "" {
		return fmt.Errorf("stable asset %q has no download URL", a.Name)
	}
	if !strings.HasPrefix(a.Digest, "sha256:") || !sha256DigestRE.MatchString(strings.TrimPrefix(a.Digest, "sha256:")) {
		return fmt.Errorf("stable asset %q has invalid GitHub digest %q", a.Name, a.Digest)
	}
	return nil
}

func findUniqueAsset(rel *release, name string) (*asset, error) {
	var found *asset
	for i := range rel.Assets {
		if rel.Assets[i].Name != name {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("release %s has duplicate asset %q", rel.TagName, name)
		}
		found = &rel.Assets[i]
	}
	return found, nil
}

func verifyStableAssetDigest(a *asset, data []byte) (string, error) {
	if err := validateStableAssetMetadata(a); err != nil {
		return "", err
	}
	if int64(len(data)) != a.Size {
		return "", fmt.Errorf("stable asset %q size mismatch: got %d want %d", a.Name, len(data), a.Size)
	}
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	want := strings.TrimPrefix(a.Digest, "sha256:")
	if got != want {
		return "", fmt.Errorf("GitHub digest mismatch for %s: got %s want %s", a.Name, got, want)
	}
	return got, nil
}

func strictChecksumFor(body, file string) (string, bool) {
	wantNames := make(map[string]struct{}, len(stableBinaryAssetNames))
	for _, name := range stableBinaryAssetNames {
		wantNames[name] = struct{}{}
	}
	parsed := make(map[string]string, len(wantNames))
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || !sha256DigestRE.MatchString(fields[0]) {
			return "", false
		}
		if _, ok := wantNames[fields[1]]; !ok {
			return "", false
		}
		if _, duplicate := parsed[fields[1]]; duplicate {
			return "", false
		}
		parsed[fields[1]] = fields[0]
	}
	if len(parsed) != len(wantNames) {
		return "", false
	}
	sum, ok := parsed[file]
	return sum, ok
}

func verifyStableArtifactIdentity(data []byte, publicVersion, goos, goarch string) error {
	expected := buildversion.StableArtifactIdentity(publicVersion, goos, goarch)
	markers := stableArtifactMarkerRE.FindAll(data, 2)
	if len(markers) != 1 || !bytes.Equal(markers[0], []byte(expected)) || bytes.Count(data, []byte(expected)) != 1 {
		return fmt.Errorf("stable artifact identity mismatch: expected exactly one %q marker", expected)
	}
	return nil
}
