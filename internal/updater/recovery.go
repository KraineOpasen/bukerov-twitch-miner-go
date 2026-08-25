package updater

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/util"
	buildversion "github.com/KraineOpasen/bukerov-twitch-miner-go/internal/version"
)

const (
	stableCacheSchema   = 1
	stableManifestName  = "active.json"
	maxStableManifest   = 64 << 10
	maxStableBinarySize = 256 << 20
)

var errNoStableCache = errors.New("stable recovery cache is empty")

type stableCacheManifest struct {
	Schema           int    `json:"schema"`
	Slot             string `json:"slot"`
	Tag              string `json:"tag"`
	Version          string `json:"version"`
	Channel          string `json:"channel"`
	GOOS             string `json:"goos"`
	GOARCH           string `json:"goarch"`
	Asset            string `json:"asset"`
	SHA256           string `json:"sha256"`
	APIDigest        string `json:"api_digest"`
	ArtifactIdentity string `json:"artifact_identity"`
	SourceCommit     string `json:"source_commit"`
	Size             int64  `json:"size"`
}

func platformCacheDir(root, goos, goarch string) string {
	return filepath.Join(root, goos+"-"+goarch)
}

func stableSlotPath(dir, slot string) string {
	return filepath.Join(dir, "slot-"+slot)
}

func stageStableCandidate(
	root string,
	rel *release,
	assetName string,
	data []byte,
	digest, sourceCommit string,
) error {
	if strings.TrimSpace(root) == "" {
		return fmt.Errorf("stable recovery cache path is empty")
	}
	if !sha256DigestRE.MatchString(digest) {
		return fmt.Errorf("invalid candidate digest %q", digest)
	}
	if !gitCommitRE.MatchString(sourceCommit) {
		return fmt.Errorf("invalid verified source commit %q", sourceCommit)
	}
	if len(data) == 0 || int64(len(data)) > maxStableBinarySize {
		return fmt.Errorf("candidate binary size %d is invalid", len(data))
	}
	dataSum := sha256.Sum256(data)
	if hex.EncodeToString(dataSum[:]) != digest {
		return fmt.Errorf("candidate data does not match verified digest %s", digest)
	}
	public, _, ok := parseStableTag(rel.TagName)
	if !ok || rel.PublicVersion != public {
		return fmt.Errorf("invalid stable release tag/version binding %q -> %q", rel.TagName, rel.PublicVersion)
	}
	if err := verifyStableArtifactIdentity(data, public, runtime.GOOS, runtime.GOARCH); err != nil {
		return err
	}
	a, err := findUniqueAsset(rel, assetName)
	if err != nil {
		return err
	}
	if a == nil {
		return fmt.Errorf("release %s has no asset %q", rel.TagName, assetName)
	}
	if a.Digest != "sha256:"+digest {
		return fmt.Errorf("candidate digest %s does not match API digest %s", digest, a.Digest)
	}
	candidate := stableCacheManifest{
		Schema:           stableCacheSchema,
		Tag:              rel.TagName,
		Version:          rel.publicVersion(),
		Channel:          "stable",
		GOOS:             runtime.GOOS,
		GOARCH:           runtime.GOARCH,
		Asset:            assetName,
		SHA256:           digest,
		APIDigest:        a.Digest,
		ArtifactIdentity: buildversion.StableArtifactIdentity(rel.publicVersion(), runtime.GOOS, runtime.GOARCH),
		SourceCommit:     sourceCommit,
		Size:             int64(len(data)),
	}

	dir := platformCacheDir(root, runtime.GOOS, runtime.GOARCH)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create stable cache directory: %w", err)
	}
	activeSlot := ""
	manifest, err := readStableManifest(root, runtime.GOOS, runtime.GOARCH)
	if err == nil {
		activeSlot = manifest.Slot
		cmp, comparable := compareVersions(candidate.Version, manifest.Version)
		if !comparable {
			return fmt.Errorf("compare candidate stable version %q with accepted floor %q",
				candidate.Version, manifest.Version)
		}
		if cmp < 0 {
			return fmt.Errorf("candidate stable version %s is below accepted recovery floor %s",
				candidate.Version, manifest.Version)
		}
		if cmp == 0 && !sameStableCandidate(candidate, manifest) {
			return fmt.Errorf("stable version %s conflicts with the already accepted immutable candidate",
				candidate.Version)
		}
	} else if !errors.Is(err, errNoStableCache) {
		return fmt.Errorf("read active stable cache before staging: %w", err)
	}
	nextSlot := "a"
	if activeSlot == "a" {
		nextSlot = "b"
	}

	if err := util.WriteFileAtomic(stableSlotPath(dir, nextSlot), data, 0755); err != nil {
		return fmt.Errorf("write stable cache slot %s: %w", nextSlot, err)
	}
	candidate.Slot = nextSlot
	body, err := json.MarshalIndent(candidate, "", "  ")
	if err != nil {
		return fmt.Errorf("encode stable cache manifest: %w", err)
	}
	body = append(body, '\n')
	if err := util.WriteFileAtomic(filepath.Join(dir, stableManifestName), body, 0600); err != nil {
		return fmt.Errorf("activate stable cache slot %s: %w", nextSlot, err)
	}
	return nil
}

// sameStableCandidate prevents a mutable Release from replacing a previously
// accepted artifact under the same public version. Slot is deliberately
// excluded: it is the local atomic-storage coordinate, not producer identity.
func sameStableCandidate(a, b stableCacheManifest) bool {
	return a.Schema == b.Schema &&
		a.Tag == b.Tag &&
		a.Version == b.Version &&
		a.Channel == b.Channel &&
		a.GOOS == b.GOOS &&
		a.GOARCH == b.GOARCH &&
		a.Asset == b.Asset &&
		a.SHA256 == b.SHA256 &&
		a.APIDigest == b.APIDigest &&
		a.ArtifactIdentity == b.ArtifactIdentity &&
		a.SourceCommit == b.SourceCommit &&
		a.Size == b.Size
}

func readStableManifest(root, goos, goarch string) (stableCacheManifest, error) {
	dir := platformCacheDir(root, goos, goarch)
	body, err := readBoundedRegularFile(filepath.Join(dir, stableManifestName), maxStableManifest)
	if errors.Is(err, os.ErrNotExist) {
		return stableCacheManifest{}, errNoStableCache
	}
	if err != nil {
		return stableCacheManifest{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var manifest stableCacheManifest
	if err := dec.Decode(&manifest); err != nil {
		return stableCacheManifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	if err := ensureJSONEOF(dec); err != nil {
		return stableCacheManifest{}, err
	}
	if err := validateStableManifest(manifest, goos, goarch); err != nil {
		return stableCacheManifest{}, err
	}
	return manifest, nil
}

func ensureJSONEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("manifest contains trailing JSON")
		}
		return fmt.Errorf("decode manifest trailing data: %w", err)
	}
	return nil
}

func validateStableManifest(m stableCacheManifest, goos, goarch string) error {
	if m.Schema != stableCacheSchema {
		return fmt.Errorf("stable cache schema %d is unsupported", m.Schema)
	}
	if m.Slot != "a" && m.Slot != "b" {
		return fmt.Errorf("stable cache slot %q is invalid", m.Slot)
	}
	public, _, ok := parseStableTag(m.Tag)
	if !ok || public != m.Version {
		return fmt.Errorf("stable cache tag/version binding %q -> %q is invalid", m.Tag, m.Version)
	}
	if m.Channel != "stable" {
		return fmt.Errorf("stable cache channel is %q", m.Channel)
	}
	if !stablePlatform(goos, goarch) || m.GOOS != goos || m.GOARCH != goarch {
		return fmt.Errorf("stable cache platform %s/%s does not match runtime %s/%s", m.GOOS, m.GOARCH, goos, goarch)
	}
	if m.Asset != assetNameFor(goos, goarch) {
		return fmt.Errorf("stable cache asset is %q, want %q", m.Asset, assetNameFor(goos, goarch))
	}
	if !sha256DigestRE.MatchString(m.SHA256) || m.APIDigest != "sha256:"+m.SHA256 {
		return fmt.Errorf("stable cache digest binding is invalid")
	}
	wantIdentity := buildversion.StableArtifactIdentity(m.Version, goos, goarch)
	if m.ArtifactIdentity != wantIdentity {
		return fmt.Errorf("stable cache artifact identity is invalid")
	}
	if !gitCommitRE.MatchString(m.SourceCommit) {
		return fmt.Errorf("stable cache verified source commit is invalid")
	}
	if m.Size <= 0 || m.Size > maxStableBinarySize {
		return fmt.Errorf("stable cache binary size %d is invalid", m.Size)
	}
	return nil
}

type recoveryOptions struct {
	currentVersion string
	releaseChannel string
	cacheDir       string
	execPath       string
	goos           string
	goarch         string
	args           []string
	env            []string
	execFn         func(string, []string, []string) error
}

// RecoverStable replays an already verified stable artifact from the durable
// cache before any application service starts. AUTO_UPDATE controls future
// discovery; it intentionally does not turn a previously accepted version
// floor into a silent downgrade after container recreation.
func RecoverStable(currentVersion, releaseChannel, cacheDir string) (bool, error) {
	if releaseChannel != "stable" {
		return false, nil
	}
	execPath, err := os.Executable()
	if err != nil {
		return false, fmt.Errorf("resolve executable path for stable recovery: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(execPath); err == nil {
		execPath = resolved
	}
	args := append([]string{execPath}, os.Args[1:]...)
	return recoverStable(recoveryOptions{
		currentVersion: currentVersion,
		releaseChannel: releaseChannel,
		cacheDir:       cacheDir,
		execPath:       execPath,
		goos:           runtime.GOOS,
		goarch:         runtime.GOARCH,
		args:           args,
		env:            os.Environ(),
		execFn:         execReplacement,
	})
}

func recoverStable(opts recoveryOptions) (bool, error) {
	if opts.releaseChannel != "stable" {
		return false, nil
	}
	if strings.TrimSpace(opts.cacheDir) == "" {
		return false, fmt.Errorf("stable recovery cache path is empty")
	}
	manifest, err := readStableManifest(opts.cacheDir, opts.goos, opts.goarch)
	if errors.Is(err, errNoStableCache) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("validate stable recovery manifest: %w", err)
	}
	cmp, ok := compareVersions(opts.currentVersion, manifest.Version)
	if !ok {
		return false, fmt.Errorf("compare running stable version %q with cached %q", opts.currentVersion, manifest.Version)
	}
	if cmp >= 0 {
		return false, nil
	}

	dir := platformCacheDir(opts.cacheDir, opts.goos, opts.goarch)
	data, err := readBoundedRegularFile(stableSlotPath(dir, manifest.Slot), maxStableBinarySize)
	if err != nil {
		return false, fmt.Errorf("read stable recovery slot %s: %w", manifest.Slot, err)
	}
	if int64(len(data)) != manifest.Size {
		return false, fmt.Errorf("stable recovery size mismatch: got %d want %d", len(data), manifest.Size)
	}
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if got != manifest.SHA256 {
		return false, fmt.Errorf("stable recovery checksum mismatch: got %s want %s", got, manifest.SHA256)
	}
	if err := verifyStableArtifactIdentity(data, manifest.Version, opts.goos, opts.goarch); err != nil {
		return false, err
	}
	if err := replaceExecutable(opts.execPath, data); err != nil {
		return false, fmt.Errorf("restore stable executable: %w", err)
	}
	if opts.execFn == nil {
		return true, fmt.Errorf("stable recovery exec function is nil")
	}
	if err := opts.execFn(opts.execPath, opts.args, opts.env); err != nil {
		return true, fmt.Errorf("re-exec restored stable version %s: %w", manifest.Version, err)
	}
	return true, fmt.Errorf("re-exec restored stable version %s returned unexpectedly", manifest.Version)
}

func readBoundedRegularFile(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() < 0 || info.Size() > max {
		return nil, fmt.Errorf("%s size %d exceeds limit %d", path, info.Size(), max)
	}
	body, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > max {
		return nil, fmt.Errorf("%s exceeds limit %d", path, max)
	}
	return body, nil
}
