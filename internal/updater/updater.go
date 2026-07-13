// Package updater keeps the running binary up to date with the latest
// GitHub release. It periodically queries the repository's Releases API and,
// when a newer release than the currently running version is found and
// self-update is enabled, downloads the binary for the current platform,
// atomically swaps it over the running executable, and asks the process to
// shut down cleanly so the container/service supervisor restarts it on the
// new build.
//
// The whole subsystem is best-effort: any failure (network error, read-only
// filesystem, checksum mismatch, ...) is logged and the miner keeps running
// on its current version. When self-update is disabled it still checks and
// logs/notifies that an update is available, so operators who have opted out
// of automatic replacement are not left in the dark.
package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultCheckInterval is used when no valid AUTO_UPDATE_CHECK_INTERVAL is
	// configured. A container can run for weeks without a restart, so a single
	// startup check is not enough - but releases are infrequent, so there is
	// no point hammering the API either.
	DefaultCheckInterval = 8 * time.Hour

	// minCheckInterval clamps absurdly small intervals so a misconfiguration
	// can't turn the updater into an API-rate-limit magnet.
	minCheckInterval = 15 * time.Minute

	// binaryBaseName is the prefix of every release asset (see the Release
	// workflow), e.g. "twitch-miner-go-linux-amd64".
	binaryBaseName = "twitch-miner-go"

	defaultAPIBaseURL = "https://api.github.com"
	userAgent         = "bukerov-twitch-miner-go-updater"

	apiTimeout      = 30 * time.Second
	downloadTimeout = 5 * time.Minute
	maxAttempts     = 3
	defaultRetryDly = 3 * time.Second
)

// NotifyFunc is invoked (best-effort) whenever a newer release is detected,
// regardless of whether self-update is enabled. It is called at most once per
// distinct latest version so it does not spam every check interval.
type NotifyFunc func(current, latest, releaseURL string)

// Options configures an Updater.
type Options struct {
	// Repo is the "owner/name" GitHub repository to check for releases.
	Repo string
	// CurrentVersion is the version of the running binary (internal/version).
	CurrentVersion string
	// Enabled turns on automatic download + self-replacement. When false the
	// updater only checks and logs/notifies that an update is available.
	Enabled bool
	// CheckInterval is how often to re-check for a new release.
	CheckInterval time.Duration
	// Notify, if set, is called when a newer release is found.
	Notify NotifyFunc
	// OnUpdate, if set, is invoked after the binary has been successfully
	// replaced. It should trigger a clean shutdown so the process exits 0 and
	// the supervisor restarts it on the new binary.
	OnUpdate func()

	// httpClient/apiBaseURL/execPath/retryDelay are overridable for tests;
	// zero values fall back to sane production defaults in New.
	httpClient *http.Client
	apiBaseURL string
	execPath   string
	retryDelay time.Duration
}

// Updater checks for and applies binary updates.
type Updater struct {
	opts Options

	// notifiedVersion is the latest version already surfaced via Notify, so a
	// pending update isn't announced on every interval.
	notifiedVersion string
}

// release/asset mirror the subset of the GitHub Releases API the updater uses.
type release struct {
	TagName    string  `json:"tag_name"`
	HTMLURL    string  `json:"html_url"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	Assets     []asset `json:"assets"`
}

type asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

// New builds an Updater, applying defaults for any unset option.
func New(opts Options) *Updater {
	if opts.CheckInterval <= 0 {
		opts.CheckInterval = DefaultCheckInterval
	}
	if opts.CheckInterval < minCheckInterval {
		opts.CheckInterval = minCheckInterval
	}
	if opts.apiBaseURL == "" {
		opts.apiBaseURL = defaultAPIBaseURL
	}
	if opts.retryDelay == 0 {
		opts.retryDelay = defaultRetryDly
	}
	if opts.httpClient == nil {
		opts.httpClient = &http.Client{Timeout: downloadTimeout}
	}
	return &Updater{opts: opts}
}

// Run performs an initial check and then re-checks every CheckInterval until
// the context is cancelled. It never returns an error: everything is
// best-effort and logged.
func (u *Updater) Run(ctx context.Context) {
	if !isReleaseVersion(u.opts.CurrentVersion) {
		// Dev/dirty builds ("dev", "v1.2.3-4-gabcdef", ...) have no meaningful
		// release to compare against and must never be silently rolled back to
		// the latest published release, so the updater stays dormant.
		slog.Info("Auto-update disabled: running a non-release build",
			"version", u.opts.CurrentVersion)
		return
	}

	slog.Info("Auto-update watcher started",
		"enabled", u.opts.Enabled,
		"current", u.opts.CurrentVersion,
		"checkInterval", u.opts.CheckInterval.String())

	u.checkAndMaybeUpdate(ctx)

	ticker := time.NewTicker(u.opts.CheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			u.checkAndMaybeUpdate(ctx)
		}
	}
}

// checkAndMaybeUpdate runs one cycle: fetch the latest release, and if it is
// newer than the running version either apply it (when enabled) or just
// log/notify. Errors are logged and swallowed so the loop keeps running.
func (u *Updater) checkAndMaybeUpdate(ctx context.Context) {
	rel, err := u.latestRelease(ctx)
	if err != nil {
		slog.Warn("Auto-update check failed", "error", err)
		return
	}

	cmp, ok := compareVersions(u.opts.CurrentVersion, rel.TagName)
	if !ok {
		slog.Debug("Auto-update: could not compare versions",
			"current", u.opts.CurrentVersion, "latest", rel.TagName)
		return
	}
	if cmp >= 0 {
		slog.Debug("Auto-update: already up to date",
			"current", u.opts.CurrentVersion, "latest", rel.TagName)
		return
	}

	slog.Info("Auto-update: newer release available",
		"current", u.opts.CurrentVersion, "latest", rel.TagName, "url", rel.HTMLURL)

	// Notify once per distinct latest version, whether or not self-update is on.
	if u.opts.Notify != nil && u.notifiedVersion != rel.TagName {
		u.notifiedVersion = rel.TagName
		u.opts.Notify(u.opts.CurrentVersion, rel.TagName, rel.HTMLURL)
	}

	if !u.opts.Enabled {
		slog.Info("Auto-update is disabled; not replacing the binary. Enable it with -auto-update or AUTO_UPDATE=true.",
			"latest", rel.TagName)
		return
	}

	if err := u.applyUpdate(ctx, rel); err != nil {
		// A read-only filesystem (common in hardened Docker setups) or any
		// other write failure must not take the miner down - log and carry on
		// with the current version.
		slog.Error("Auto-update: failed to apply update, continuing on current version",
			"current", u.opts.CurrentVersion, "latest", rel.TagName, "error", err)
		return
	}

	slog.Info("Auto-update: binary replaced successfully, restarting to load the new version",
		"from", u.opts.CurrentVersion, "to", rel.TagName)
	if u.opts.OnUpdate != nil {
		u.opts.OnUpdate()
	}
}

// latestRelease fetches the newest non-draft, non-prerelease release.
func (u *Updater) latestRelease(ctx context.Context) (*release, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/latest", strings.TrimRight(u.opts.apiBaseURL, "/"), u.opts.Repo)

	var rel release
	if err := u.getJSON(ctx, url, &rel); err != nil {
		return nil, err
	}
	if rel.Draft || rel.Prerelease {
		return nil, fmt.Errorf("latest release %q is a draft/prerelease", rel.TagName)
	}
	if rel.TagName == "" {
		return nil, fmt.Errorf("latest release has no tag name")
	}
	return &rel, nil
}

// getJSON GETs url and decodes the JSON body into v, retrying transient
// failures with a fixed backoff. Context cancellation aborts immediately.
func (u *Updater) getJSON(ctx context.Context, url string, v any) error {
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(u.opts.retryDelay):
			}
		}

		body, err := u.get(ctx, url, apiTimeout)
		if err != nil {
			lastErr = err
			slog.Debug("Auto-update: request failed, will retry", "attempt", attempt, "url", url, "error", err)
			continue
		}

		if err := json.Unmarshal(body, v); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
		return nil
	}
	return fmt.Errorf("after %d attempts: %w", maxAttempts, lastErr)
}

// get performs a single GET and returns the body for a 2xx response.
func (u *Updater) get(ctx context.Context, url string, timeout time.Duration) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := u.opts.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("unexpected status %d for %s", resp.StatusCode, url)
	}
	return body, nil
}

// applyUpdate downloads the release asset for the current platform, verifies
// it against the release checksums when available, and atomically replaces the
// running executable.
func (u *Updater) applyUpdate(ctx context.Context, rel *release) error {
	name := assetName()
	a := findAsset(rel, name)
	if a == nil {
		return fmt.Errorf("release %s has no asset %q for this platform", rel.TagName, name)
	}

	execPath, err := u.executablePath()
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}

	slog.Info("Auto-update: downloading new binary", "asset", name, "version", rel.TagName)
	data, err := u.get(ctx, a.URL, downloadTimeout)
	if err != nil {
		return fmt.Errorf("download %s: %w", name, err)
	}

	if err := u.verifyChecksum(ctx, rel, name, data); err != nil {
		return err
	}

	return replaceExecutable(execPath, data)
}

// verifyChecksum checks data against the sha256 listed for name in the
// release's checksums.txt asset. A missing checksums asset is tolerated (the
// download simply goes unverified); a present-but-mismatching checksum is a
// hard failure so a corrupt/tampered binary is never installed.
func (u *Updater) verifyChecksum(ctx context.Context, rel *release, name string, data []byte) error {
	sums := findAsset(rel, "checksums.txt")
	if sums == nil {
		slog.Debug("Auto-update: no checksums.txt in release, skipping verification", "version", rel.TagName)
		return nil
	}

	body, err := u.get(ctx, sums.URL, apiTimeout)
	if err != nil {
		slog.Warn("Auto-update: could not fetch checksums, skipping verification", "error", err)
		return nil
	}

	want, ok := checksumFor(string(body), name)
	if !ok {
		slog.Warn("Auto-update: checksums.txt has no entry for asset, skipping verification", "asset", name)
		return nil
	}

	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("checksum mismatch for %s: got %s want %s", name, got, want)
	}
	slog.Debug("Auto-update: checksum verified", "asset", name)
	return nil
}

// executablePath returns the path of the running binary (or the test override).
func (u *Updater) executablePath() (string, error) {
	if u.opts.execPath != "" {
		return u.opts.execPath, nil
	}
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	// Resolve symlinks so the rename targets the real file, not a link.
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return p, nil
}

// replaceExecutable atomically swaps the file at execPath for data. It writes
// a temp file in the same directory (so the final rename is atomic and stays
// on one filesystem) and renames it into place. On Linux the running process
// keeps executing the old, now-unlinked inode, so replacing itself is safe.
func replaceExecutable(execPath string, data []byte) error {
	dir := filepath.Dir(execPath)

	tmp, err := os.CreateTemp(dir, filepath.Base(execPath)+".new-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()

	// Clean up the temp file on any failure before the rename succeeds.
	success := false
	defer func() {
		if !success {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	// Match a normal executable's permissions before swapping it in.
	if err := os.Chmod(tmpName, 0755); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}

	if err := os.Rename(tmpName, execPath); err != nil {
		return fmt.Errorf("replace executable: %w", err)
	}

	success = true
	return nil
}

// assetName returns the release asset name for the running platform, e.g.
// "twitch-miner-go-linux-amd64" or "twitch-miner-go-windows-amd64.exe",
// matching the Release workflow's naming.
func assetName() string {
	name := fmt.Sprintf("%s-%s-%s", binaryBaseName, runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

func findAsset(rel *release, name string) *asset {
	for i := range rel.Assets {
		if rel.Assets[i].Name == name {
			return &rel.Assets[i]
		}
	}
	return nil
}

// checksumFor finds the sha256 hex digest for file within the body of a
// `sha256sum`-style checksums file (each line "<hex>  <filename>").
func checksumFor(body, file string) (string, bool) {
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		// The filename may be prefixed with '*' in binary-mode sums.
		if strings.TrimPrefix(fields[1], "*") == file {
			return fields[0], true
		}
	}
	return "", false
}

// ParseCheckInterval interprets the AUTO_UPDATE_CHECK_INTERVAL value. It
// accepts a Go duration string ("8h", "6h30m") or a bare number of hours
// ("12"). An empty or unparseable value yields DefaultCheckInterval, and any
// interval below minCheckInterval is clamped up to it.
func ParseCheckInterval(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultCheckInterval
	}

	if d, err := time.ParseDuration(raw); err == nil {
		return clampInterval(d)
	}
	if hours, err := strconv.Atoi(raw); err == nil {
		return clampInterval(time.Duration(hours) * time.Hour)
	}

	slog.Warn("Invalid AUTO_UPDATE_CHECK_INTERVAL, using default",
		"value", raw, "default", DefaultCheckInterval.String())
	return DefaultCheckInterval
}

func clampInterval(d time.Duration) time.Duration {
	if d < minCheckInterval {
		return minCheckInterval
	}
	return d
}
