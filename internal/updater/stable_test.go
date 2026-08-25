package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	buildversion "github.com/KraineOpasen/bukerov-twitch-miner-go/internal/version"
)

func completeStableRelease(tag string) release {
	public, _, ok := parseStableTag(tag)
	if !ok {
		public = "0.0.0"
	}
	amd64 := []byte(buildversion.StableArtifactIdentity(public, "linux", "amd64"))
	arm64 := []byte(buildversion.StableArtifactIdentity(public, "linux", "arm64"))
	amd64Sum := sha256.Sum256(amd64)
	arm64Sum := sha256.Sum256(arm64)
	checksums := []byte(fmt.Sprintf("%s  twitch-miner-go-linux-amd64\n%s  twitch-miner-go-linux-arm64\n",
		hex.EncodeToString(amd64Sum[:]), hex.EncodeToString(arm64Sum[:])))
	return release{
		TagName: tag,
		HTMLURL: "https://example.test/releases/" + tag,
		Assets: []asset{
			stableTestAsset("twitch-miner-go-linux-amd64", amd64, "https://example.test/amd64"),
			stableTestAsset("twitch-miner-go-linux-arm64", arm64, "https://example.test/arm64"),
			stableTestAsset("checksums.txt", checksums, "https://example.test/checksums"),
		},
	}
}

func stableTestAsset(name string, data []byte, url string) asset {
	sum := sha256.Sum256(data)
	return asset{
		Name: name, URL: url, State: "uploaded", Size: int64(len(data)),
		Digest: "sha256:" + hex.EncodeToString(sum[:]),
	}
}

func allowTestStableProvenance(context.Context, *release, string, string) (stableProvenanceEvidence, error) {
	return stableProvenanceEvidence{SourceCommit: strings.Repeat("a", 40)}, nil
}

func TestParseStableTagExactCanonicalMapping(t *testing.T) {
	valid := map[string]string{
		"stable-v0.1.3":    "0.1.3",
		"stable-v10.20.30": "10.20.30",
	}
	for tag, want := range valid {
		got, _, ok := parseStableTag(tag)
		if !ok || got != want {
			t.Errorf("parseStableTag(%q) = %q, %t; want %q, true", tag, got, ok, want)
		}
	}
	for _, tag := range []string{
		"v99.0.0",
		"stable-v0.1.3-rc.1",
		"stable-v0.1.3foo",
		"foo-stable-v0.1.3",
		"stable-v00.1.3",
		"stable-v0.01.3",
		"stable-v0.1.03",
		"stable-V0.1.3",
		"stable-v9223372036854775808.0.0",
	} {
		if _, _, ok := parseStableTag(tag); ok {
			t.Errorf("parseStableTag(%q) qualified a non-canonical tag", tag)
		}
	}
}

func TestStableSupportedAssetContract(t *testing.T) {
	want := []string{"twitch-miner-go-linux-amd64", "twitch-miner-go-linux-arm64"}
	if len(stableBinaryAssetNames) != len(want) {
		t.Fatalf("stable binary asset count = %d, want %d", len(stableBinaryAssetNames), len(want))
	}
	for i := range want {
		if stableBinaryAssetNames[i] != want[i] {
			t.Fatalf("stableBinaryAssetNames[%d] = %q, want %q", i, stableBinaryAssetNames[i], want[i])
		}
	}
}

func TestStrictStableChecksumsRequireExactCanonicalAssetSet(t *testing.T) {
	amd64 := strings.Repeat("a", 64)
	arm64 := strings.Repeat("b", 64)
	valid := fmt.Sprintf("%s  twitch-miner-go-linux-amd64\n%s  twitch-miner-go-linux-arm64\n", amd64, arm64)
	if got, ok := strictChecksumFor(valid, "twitch-miner-go-linux-amd64"); !ok || got != amd64 {
		t.Fatalf("valid strict checksum = %q, %t; want %q, true", got, ok, amd64)
	}
	for name, body := range map[string]string{
		"missing architecture": amd64 + "  twitch-miner-go-linux-amd64\n",
		"duplicate":            valid + amd64 + "  twitch-miner-go-linux-amd64\n",
		"unexpected asset":     valid + amd64 + "  twitch-miner-go-linux-riscv64\n",
		"uppercase digest":     strings.ToUpper(amd64) + "  twitch-miner-go-linux-amd64\n" + arm64 + "  twitch-miner-go-linux-arm64\n",
		"binary-mode alias":    amd64 + " *twitch-miner-go-linux-amd64\n" + arm64 + "  twitch-miner-go-linux-arm64\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := strictChecksumFor(body, "twitch-miner-go-linux-amd64"); ok {
				t.Fatal("non-canonical checksum set qualified")
			}
		})
	}
}

func TestStableAssetMetadataRejectsOversizedControlledAssets(t *testing.T) {
	for _, a := range []asset{
		{Name: "checksums.txt", URL: "https://example.test/checksums", State: "uploaded", Size: maxStableChecksumsSize + 1, Digest: "sha256:" + strings.Repeat("a", 64)},
		{Name: "twitch-miner-go-linux-amd64", URL: "https://example.test/binary", State: "uploaded", Size: maxStableBinarySize + 1, Digest: "sha256:" + strings.Repeat("a", 64)},
	} {
		if err := validateStableAssetMetadata(&a); err == nil {
			t.Fatalf("oversized stable asset %q qualified", a.Name)
		}
	}
}

func TestLatestStableReleaseMainCannotWinOrSuppress(t *testing.T) {
	main99 := completeStableRelease("stable-v99.0.0")
	main99.TagName = "v99.0.0"
	main999999 := completeStableRelease("stable-v999999.0.0")
	main999999.TagName = "v999999.0.0"
	releases := []release{
		completeStableRelease("stable-v0.1.2"),
		main99,
		completeStableRelease("stable-v0.1.3"),
		main999999,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/repo/releases" || r.URL.Query().Get("per_page") != "100" || r.URL.Query().Get("page") != "1" {
			t.Errorf("unexpected stable discovery request: %s", r.URL.String())
		}
		_ = json.NewEncoder(w).Encode(releases)
	}))
	defer srv.Close()

	u := New(Options{Repo: "owner/repo", CurrentVersion: "0.1.2", ReleaseChannel: "stable",
		apiBaseURL: srv.URL, httpClient: srv.Client()})
	rel, err := u.latestRelease(context.Background())
	if err != nil {
		t.Fatalf("latestRelease: %v", err)
	}
	if rel.TagName != "stable-v0.1.3" || rel.PublicVersion != "0.1.3" {
		t.Fatalf("stable candidate = %q -> %q, want stable-v0.1.3 -> 0.1.3", rel.TagName, rel.PublicVersion)
	}
}

func TestLatestStableReleaseExhaustsMultiplePages(t *testing.T) {
	page1 := make([]release, stableReleasePageSize)
	page2 := make([]release, stableReleasePageSize)
	for i := range page1 {
		page1[i] = completeStableRelease(fmt.Sprintf("stable-v99.0.%d", i))
		page1[i].TagName = fmt.Sprintf("v99.0.%d", i)
		page2[i] = completeStableRelease(fmt.Sprintf("stable-v100.0.%d", i))
		page2[i].TagName = fmt.Sprintf("v100.0.%d", i)
	}
	page1[50] = completeStableRelease("stable-v0.1.2")
	page3 := []release{completeStableRelease("stable-v0.1.3")}
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Query().Get("page") {
		case "1":
			_ = json.NewEncoder(w).Encode(page1)
		case "2":
			_ = json.NewEncoder(w).Encode(page2)
		case "3":
			_ = json.NewEncoder(w).Encode(page3)
		default:
			t.Fatalf("unexpected page %q", r.URL.Query().Get("page"))
		}
	}))
	defer srv.Close()

	u := New(Options{Repo: "owner/repo", CurrentVersion: "0.1.1", ReleaseChannel: "stable",
		apiBaseURL: srv.URL, httpClient: srv.Client()})
	rel, err := u.latestRelease(context.Background())
	if err != nil {
		t.Fatalf("latestRelease: %v", err)
	}
	if rel.TagName != "stable-v0.1.3" || requests.Load() != 3 {
		t.Fatalf("candidate=%q requests=%d, want stable-v0.1.3 and 3 pages", rel.TagName, requests.Load())
	}
}

func TestLatestStableReleaseRejectsDraftPrereleaseLookalikeAndPartial(t *testing.T) {
	draft := completeStableRelease("stable-v9.0.0")
	draft.Draft = true
	prerelease := completeStableRelease("stable-v8.0.0")
	prerelease.Prerelease = true
	partial := completeStableRelease("stable-v7.0.0")
	partial.Assets = partial.Assets[:1]
	missingChecksums := completeStableRelease("stable-v6.0.0")
	missingChecksums.Assets = missingChecksums.Assets[:2]
	releases := []release{
		draft,
		prerelease,
		partial,
		missingChecksums,
		{TagName: "stable-v5.0.0-rc.1"},
		{TagName: "foo-stable-v5.0.0"},
		completeStableRelease("stable-v0.1.3"),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(releases)
	}))
	defer srv.Close()
	u := New(Options{Repo: "owner/repo", ReleaseChannel: "stable", apiBaseURL: srv.URL, httpClient: srv.Client()})
	rel, err := u.latestRelease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rel.TagName != "stable-v0.1.3" {
		t.Fatalf("candidate = %q, want stable-v0.1.3", rel.TagName)
	}
}

func TestStableComparisonUsesPublicVersionAndNeverDowngrades(t *testing.T) {
	for _, tc := range []struct {
		name       string
		current    string
		stableTag  string
		wantNotify bool
	}{
		{name: "newer", current: "0.1.2", stableTag: "stable-v0.1.3", wantNotify: true},
		{name: "same", current: "0.1.3", stableTag: "stable-v0.1.3"},
		{name: "older", current: "0.1.4", stableTag: "stable-v0.1.3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			releases := []release{{TagName: "v99.0.0"}, completeStableRelease(tc.stableTag)}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(releases)
			}))
			defer srv.Close()
			notified := false
			u := New(Options{Repo: "owner/repo", CurrentVersion: tc.current, ReleaseChannel: "stable",
				Enabled: false, apiBaseURL: srv.URL, httpClient: srv.Client(),
				Notify: func(_, latest, _ string) {
					notified = true
					if latest != "0.1.3" {
						t.Errorf("notified latest = %q, want public 0.1.3", latest)
					}
				}})
			u.checkAndMaybeUpdate(context.Background())
			if notified != tc.wantNotify {
				t.Fatalf("notified = %t, want %t", notified, tc.wantNotify)
			}
		})
	}
}

func stableApplyFixture(t *testing.T, public string) (*httptest.Server, []byte) {
	t.Helper()
	binary := []byte("binary-prefix\x00" + buildversion.StableArtifactIdentity(public, runtime.GOOS, runtime.GOARCH) + "\x00binary-suffix")
	otherArch := "arm64"
	if runtime.GOARCH == "arm64" {
		otherArch = "amd64"
	}
	otherBinary := []byte(buildversion.StableArtifactIdentity(public, "linux", otherArch))
	binSum := sha256.Sum256(binary)
	otherSum := sha256.Sum256(otherBinary)
	checksums := []byte(fmt.Sprintf("%s  %s\n%s  %s\n",
		hex.EncodeToString(binSum[:]), assetName(),
		hex.EncodeToString(otherSum[:]), assetNameFor("linux", otherArch)))

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/releases", func(w http.ResponseWriter, r *http.Request) {
		rel := release{
			TagName: "stable-v" + public,
			HTMLURL: "https://example.test/releases/stable-v" + public,
			Assets: []asset{
				stableTestAsset(assetName(), binary, srvURL(r)+"/binary"),
				stableTestAsset(assetNameFor("linux", otherArch), otherBinary, srvURL(r)+"/other"),
				stableTestAsset("checksums.txt", checksums, srvURL(r)+"/checksums"),
			},
		}
		_ = json.NewEncoder(w).Encode([]release{rel})
	})
	mux.HandleFunc("/binary", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(binary) })
	mux.HandleFunc("/other", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(otherBinary) })
	mux.HandleFunc("/checksums", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(checksums) })
	return httptest.NewServer(mux), binary
}

func TestStableApplyVerifiesIdentityPersistsCacheThenSwaps(t *testing.T) {
	if !stablePlatform(runtime.GOOS, runtime.GOARCH) {
		t.Skip("stable native producer supports linux/amd64 and linux/arm64")
	}
	srv, binary := stableApplyFixture(t, "0.1.3")
	defer srv.Close()
	dir := t.TempDir()
	execPath := filepath.Join(dir, "twitch-miner-go")
	if err := os.WriteFile(execPath, []byte("old"), 0755); err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(dir, "database", ".updater", "stable")
	restarted := false
	u := New(Options{Repo: "owner/repo", CurrentVersion: "0.1.2", ReleaseChannel: "stable", Enabled: true,
		StableCacheDir: cacheDir, apiBaseURL: srv.URL, httpClient: srv.Client(), execPath: execPath,
		provenanceVerifier: allowTestStableProvenance, OnUpdate: func() { restarted = true }})
	u.checkAndMaybeUpdate(context.Background())
	if !restarted {
		t.Fatal("OnUpdate was not called")
	}
	if got, _ := os.ReadFile(execPath); string(got) != string(binary) {
		t.Fatal("running executable was not replaced")
	}
	manifest, err := readStableManifest(cacheDir, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatalf("readStableManifest: %v", err)
	}
	if manifest.Tag != "stable-v0.1.3" || manifest.Version != "0.1.3" || manifest.Channel != "stable" {
		t.Fatalf("manifest identity = %#v", manifest)
	}
	cached, err := os.ReadFile(stableSlotPath(platformCacheDir(cacheDir, runtime.GOOS, runtime.GOARCH), manifest.Slot))
	if err != nil || string(cached) != string(binary) {
		t.Fatalf("cached binary mismatch: err=%v", err)
	}
}

func TestStableArtifactWithoutStableIdentityIsRejected(t *testing.T) {
	if !stablePlatform(runtime.GOOS, runtime.GOARCH) {
		t.Skip("stable native producer supports linux/amd64 and linux/arm64")
	}
	srv, _ := stableApplyFixture(t, "0.1.3")
	defer srv.Close()
	// Replace the server with a fixture whose binary/checksums/API digest all
	// agree, but whose payload is not a stable build. Identity must remain an
	// independent hard gate after checksum integrity succeeds.
	bad := []byte("main-channel-binary")
	other := []byte(buildversion.StableArtifactIdentity("0.1.3", "linux", "arm64"))
	if runtime.GOARCH == "arm64" {
		other = []byte(buildversion.StableArtifactIdentity("0.1.3", "linux", "amd64"))
	}
	badSum := sha256.Sum256(bad)
	otherSum := sha256.Sum256(other)
	otherName := "twitch-miner-go-linux-arm64"
	if runtime.GOARCH == "arm64" {
		otherName = "twitch-miner-go-linux-amd64"
	}
	checksums := []byte(fmt.Sprintf("%s  %s\n%s  %s\n", hex.EncodeToString(badSum[:]), assetName(),
		hex.EncodeToString(otherSum[:]), otherName))
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/releases", func(w http.ResponseWriter, r *http.Request) {
		rel := completeStableRelease("stable-v0.1.3")
		rel.Assets = []asset{
			stableTestAsset(assetName(), bad, srvURL(r)+"/bad"),
			stableTestAsset(otherName, other, srvURL(r)+"/other"),
			stableTestAsset("checksums.txt", checksums, srvURL(r)+"/checksums"),
		}
		_ = json.NewEncoder(w).Encode([]release{rel})
	})
	mux.HandleFunc("/bad", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(bad) })
	mux.HandleFunc("/other", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(other) })
	mux.HandleFunc("/checksums", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(checksums) })
	badSrv := httptest.NewServer(mux)
	defer badSrv.Close()

	dir := t.TempDir()
	execPath := filepath.Join(dir, "twitch-miner-go")
	_ = os.WriteFile(execPath, []byte("old"), 0755)
	u := New(Options{Repo: "owner/repo", CurrentVersion: "0.1.2", ReleaseChannel: "stable", Enabled: true,
		StableCacheDir: filepath.Join(dir, "cache"), apiBaseURL: badSrv.URL, httpClient: badSrv.Client(), execPath: execPath})
	u.checkAndMaybeUpdate(context.Background())
	if got, _ := os.ReadFile(execPath); string(got) != "old" {
		t.Fatalf("non-stable artifact replaced executable: %q", got)
	}
	if !strings.Contains(u.Snapshot().LastError, "stable artifact identity mismatch") {
		t.Fatalf("LastError = %q", u.Snapshot().LastError)
	}
}

func TestStableCacheFailurePreventsLiveSwap(t *testing.T) {
	if !stablePlatform(runtime.GOOS, runtime.GOARCH) {
		t.Skip("stable native producer supports linux/amd64 and linux/arm64")
	}
	srv, _ := stableApplyFixture(t, "0.1.3")
	defer srv.Close()
	dir := t.TempDir()
	execPath := filepath.Join(dir, "twitch-miner-go")
	_ = os.WriteFile(execPath, []byte("old"), 0755)
	cacheBlocker := filepath.Join(dir, "cache-blocker")
	_ = os.WriteFile(cacheBlocker, []byte("not a directory"), 0600)
	restarted := false
	u := New(Options{Repo: "owner/repo", CurrentVersion: "0.1.2", ReleaseChannel: "stable", Enabled: true,
		StableCacheDir: filepath.Join(cacheBlocker, "stable"), apiBaseURL: srv.URL, httpClient: srv.Client(),
		execPath: execPath, provenanceVerifier: allowTestStableProvenance,
		OnUpdate: func() { restarted = true }})
	u.checkAndMaybeUpdate(context.Background())
	if restarted {
		t.Fatal("OnUpdate called after cache persistence failed")
	}
	if got, _ := os.ReadFile(execPath); string(got) != "old" {
		t.Fatalf("live executable changed after cache failure: %q", got)
	}
}

func TestStableProvenanceFailurePreventsCacheAndLiveSwap(t *testing.T) {
	if !stablePlatform(runtime.GOOS, runtime.GOARCH) {
		t.Skip("stable native producer supports linux/amd64 and linux/arm64")
	}
	srv, _ := stableApplyFixture(t, "0.1.3")
	defer srv.Close()
	dir := t.TempDir()
	execPath := filepath.Join(dir, "twitch-miner-go")
	if err := os.WriteFile(execPath, []byte("old"), 0755); err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(dir, "cache")
	called := false
	u := New(Options{
		Repo: "owner/repo", CurrentVersion: "0.1.2", ReleaseChannel: "stable", Enabled: true,
		StableCacheDir: cacheDir, apiBaseURL: srv.URL, httpClient: srv.Client(), execPath: execPath,
		provenanceVerifier: func(_ context.Context, rel *release, name, digest string) (stableProvenanceEvidence, error) {
			called = true
			if rel.TagName != "stable-v0.1.3" || name != assetName() || !sha256DigestRE.MatchString(digest) {
				t.Errorf("provenance inputs tag=%q name=%q digest=%q", rel.TagName, name, digest)
			}
			return stableProvenanceEvidence{}, errors.New("signature probe failed")
		},
	})
	u.checkAndMaybeUpdate(context.Background())
	if !called {
		t.Fatal("stable provenance verifier was not called")
	}
	if got, _ := os.ReadFile(execPath); string(got) != "old" {
		t.Fatalf("unprovenanced candidate replaced executable: %q", got)
	}
	if _, err := os.Stat(platformCacheDir(cacheDir, runtime.GOOS, runtime.GOARCH)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unprovenanced candidate created recovery cache: %v", err)
	}
	if !strings.Contains(u.Snapshot().LastError, "verify stable build provenance") {
		t.Fatalf("LastError = %q", u.Snapshot().LastError)
	}
}

func TestLatestStableReleaseCancellation(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()
	u := New(Options{Repo: "owner/repo", ReleaseChannel: "stable", apiBaseURL: srv.URL,
		httpClient: srv.Client(), retryDelay: time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := u.latestRelease(ctx)
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stable discovery did not stop on cancellation")
	}
}

func TestStableDownloadCancellationRetainsCurrentExecutable(t *testing.T) {
	if !stablePlatform(runtime.GOOS, runtime.GOARCH) {
		t.Skip("stable native producer supports linux/amd64 and linux/arm64")
	}
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()
	rel := completeStableRelease("stable-v0.1.3")
	rel.PublicVersion = "0.1.3"
	for i := range rel.Assets {
		if rel.Assets[i].Name == assetName() {
			rel.Assets[i].URL = srv.URL
		}
	}
	execPath := filepath.Join(t.TempDir(), "twitch-miner-go")
	_ = os.WriteFile(execPath, []byte("old"), 0755)
	u := New(Options{ReleaseChannel: "stable", StableCacheDir: filepath.Join(t.TempDir(), "cache"),
		execPath: execPath, httpClient: srv.Client(), retryDelay: time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- u.applyUpdate(ctx, &rel) }()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("download cancellation error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stable download did not stop on cancellation")
	}
	if got, _ := os.ReadFile(execPath); string(got) != "old" {
		t.Fatalf("cancelled download changed executable: %q", got)
	}
}
