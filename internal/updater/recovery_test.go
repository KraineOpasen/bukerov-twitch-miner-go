package updater

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	buildversion "github.com/KraineOpasen/bukerov-twitch-miner-go/internal/version"
)

func stageRecoveryFixture(t *testing.T, root, public string) ([]byte, stableCacheManifest) {
	t.Helper()
	if !stablePlatform(runtime.GOOS, runtime.GOARCH) {
		t.Skip("stable native recovery supports linux/amd64 and linux/arm64")
	}
	data := []byte("cached\x00" + buildversion.StableArtifactIdentity(public, runtime.GOOS, runtime.GOARCH) + "\x00binary")
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	rel := completeStableRelease("stable-v" + public)
	rel.PublicVersion = public
	for i := range rel.Assets {
		if rel.Assets[i].Name == assetName() {
			rel.Assets[i] = stableTestAsset(assetName(), data, "https://example.test/binary")
		}
	}
	if err := stageStableCandidate(root, &rel, assetName(), data, digest, strings.Repeat("a", 40)); err != nil {
		t.Fatalf("stageStableCandidate: %v", err)
	}
	manifest, err := readStableManifest(root, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatalf("readStableManifest: %v", err)
	}
	return data, manifest
}

func TestRecoverStableRestoresNewerCachedBinaryBeforeReexec(t *testing.T) {
	root := filepath.Join(t.TempDir(), "database", ".updater", "stable")
	data, _ := stageRecoveryFixture(t, root, "0.1.3")
	execPath := filepath.Join(t.TempDir(), "twitch-miner-go")
	if err := os.WriteFile(execPath, []byte("pinned-old"), 0755); err != nil {
		t.Fatal(err)
	}
	execErr := errors.New("exec probe")
	called := false
	restored, err := recoverStable(recoveryOptions{
		currentVersion: "0.1.2",
		releaseChannel: "stable",
		cacheDir:       root,
		execPath:       execPath,
		goos:           runtime.GOOS,
		goarch:         runtime.GOARCH,
		args:           []string{execPath, "-healthcheck"},
		env:            []string{"TZ=UTC"},
		execFn: func(path string, args, env []string) error {
			called = true
			if path != execPath || len(args) != 2 || args[1] != "-healthcheck" || len(env) != 1 {
				t.Errorf("exec args path=%q args=%v env=%v", path, args, env)
			}
			return execErr
		},
	})
	if !restored || !errors.Is(err, execErr) || !called {
		t.Fatalf("restored=%t called=%t err=%v", restored, called, err)
	}
	if got, _ := os.ReadFile(execPath); string(got) != string(data) {
		t.Fatalf("restored executable does not match cached candidate")
	}
}

func TestRecoverStableNeverDowngradesOrReexecsSameVersion(t *testing.T) {
	for _, current := range []string{"0.1.3", "0.1.4", "9.0.0"} {
		t.Run(current, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "cache")
			stageRecoveryFixture(t, root, "0.1.3")
			execPath := filepath.Join(t.TempDir(), "twitch-miner-go")
			_ = os.WriteFile(execPath, []byte("current"), 0755)
			called := false
			restored, err := recoverStable(recoveryOptions{
				currentVersion: current, releaseChannel: "stable", cacheDir: root,
				execPath: execPath, goos: runtime.GOOS, goarch: runtime.GOARCH,
				execFn: func(string, []string, []string) error { called = true; return nil },
			})
			if err != nil || restored || called {
				t.Fatalf("current=%s restored=%t called=%t err=%v", current, restored, called, err)
			}
			if got, _ := os.ReadFile(execPath); string(got) != "current" {
				t.Fatalf("current %s was downgraded", current)
			}
		})
	}
}

func TestRecoverStableRejectsCorruptCandidateWithoutTouchingExecutable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache")
	_, manifest := stageRecoveryFixture(t, root, "0.1.3")
	dir := platformCacheDir(root, runtime.GOOS, runtime.GOARCH)
	if err := os.WriteFile(stableSlotPath(dir, manifest.Slot), []byte("corrupt"), 0755); err != nil {
		t.Fatal(err)
	}
	execPath := filepath.Join(t.TempDir(), "twitch-miner-go")
	_ = os.WriteFile(execPath, []byte("pinned-old"), 0755)
	restored, err := recoverStable(recoveryOptions{
		currentVersion: "0.1.2", releaseChannel: "stable", cacheDir: root,
		execPath: execPath, goos: runtime.GOOS, goarch: runtime.GOARCH,
		execFn: func(string, []string, []string) error { t.Fatal("exec called for corrupt candidate"); return nil },
	})
	if err == nil || restored {
		t.Fatalf("restored=%t err=%v, want fail-closed refusal", restored, err)
	}
	if got, _ := os.ReadFile(execPath); string(got) != "pinned-old" {
		t.Fatalf("corrupt recovery changed executable: %q", got)
	}
}

func TestRecoverStableRejectsMalformedOrCrossChannelManifest(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache")
	stageRecoveryFixture(t, root, "0.1.3")
	dir := platformCacheDir(root, runtime.GOOS, runtime.GOARCH)
	manifestPath := filepath.Join(dir, stableManifestName)
	body, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	body = []byte(strings.Replace(string(body), `"channel": "stable"`, `"channel": "main"`, 1))
	if err := os.WriteFile(manifestPath, body, 0600); err != nil {
		t.Fatal(err)
	}
	execPath := filepath.Join(t.TempDir(), "twitch-miner-go")
	_ = os.WriteFile(execPath, []byte("pinned-old"), 0755)
	restored, err := recoverStable(recoveryOptions{
		currentVersion: "0.1.2", releaseChannel: "stable", cacheDir: root,
		execPath: execPath, goos: runtime.GOOS, goarch: runtime.GOARCH,
		execFn: func(string, []string, []string) error { return nil },
	})
	if err == nil || restored || !strings.Contains(err.Error(), "channel") {
		t.Fatalf("restored=%t err=%v, want cross-channel rejection", restored, err)
	}
}

func TestRecoverStableMainChannelNeverConsumesStableCache(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache")
	stageRecoveryFixture(t, root, "0.1.3")
	manifestPath := filepath.Join(platformCacheDir(root, runtime.GOOS, runtime.GOARCH), stableManifestName)
	_ = os.WriteFile(manifestPath, []byte("not json"), 0600)
	restored, err := recoverStable(recoveryOptions{
		currentVersion: "99.0.0", releaseChannel: "main", cacheDir: root,
		goos: runtime.GOOS, goarch: runtime.GOARCH,
		execFn: func(string, []string, []string) error { t.Fatal("exec called for main"); return nil },
	})
	if err != nil || restored {
		t.Fatalf("main recovery restored=%t err=%v", restored, err)
	}
}

func TestStableCacheUsesTwoAtomicSlots(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache")
	_, first := stageRecoveryFixture(t, root, "0.1.3")
	_, second := stageRecoveryFixture(t, root, "0.1.4")
	if first.Slot == second.Slot {
		t.Fatalf("cache did not flip slots: first=%s second=%s", first.Slot, second.Slot)
	}
	dir := platformCacheDir(root, runtime.GOOS, runtime.GOARCH)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("cache entries = %d, want active manifest plus exactly two slots: %v", len(entries), entries)
	}
}

func TestStableCacheNeverRegressesAcceptedFloorOrMutatesSameVersion(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache")
	_, accepted := stageRecoveryFixture(t, root, "0.1.4")

	olderData := []byte("older\x00" + buildversion.StableArtifactIdentity("0.1.3", runtime.GOOS, runtime.GOARCH))
	olderSum := sha256.Sum256(olderData)
	olderDigest := hex.EncodeToString(olderSum[:])
	olderRelease := completeStableRelease("stable-v0.1.3")
	olderRelease.PublicVersion = "0.1.3"
	for i := range olderRelease.Assets {
		if olderRelease.Assets[i].Name == assetName() {
			olderRelease.Assets[i] = stableTestAsset(assetName(), olderData, "https://example.test/older")
		}
	}
	if err := stageStableCandidate(
		root, &olderRelease, assetName(), olderData, olderDigest, strings.Repeat("b", 40),
	); err == nil || !strings.Contains(err.Error(), "below accepted recovery floor") {
		t.Fatalf("older candidate error = %v, want durable-floor refusal", err)
	}

	mutatedData := []byte("mutated\x00" + buildversion.StableArtifactIdentity("0.1.4", runtime.GOOS, runtime.GOARCH))
	mutatedSum := sha256.Sum256(mutatedData)
	mutatedDigest := hex.EncodeToString(mutatedSum[:])
	mutatedRelease := completeStableRelease("stable-v0.1.4")
	mutatedRelease.PublicVersion = "0.1.4"
	for i := range mutatedRelease.Assets {
		if mutatedRelease.Assets[i].Name == assetName() {
			mutatedRelease.Assets[i] = stableTestAsset(assetName(), mutatedData, "https://example.test/mutated")
		}
	}
	if err := stageStableCandidate(
		root, &mutatedRelease, assetName(), mutatedData, mutatedDigest, strings.Repeat("a", 40),
	); err == nil || !strings.Contains(err.Error(), "conflicts with the already accepted immutable candidate") {
		t.Fatalf("same-version mutation error = %v, want immutable-candidate refusal", err)
	}

	got, err := readStableManifest(root, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	if got != accepted {
		t.Fatalf("accepted recovery floor changed after refusals:\n got  %#v\n want %#v", got, accepted)
	}
}
