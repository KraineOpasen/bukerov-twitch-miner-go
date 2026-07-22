package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// Spade discovery hardening.
//
// The spade (minute-watched beacon) endpoint is scraped from Twitch's own pages:
// the channel page links a settings.js asset, and that asset embeds the spade_url.
// Both hops and the resulting URL are attacker-influenceable (a compromised or
// spoofed page could point the beacon anywhere), so discovery is split into
// three checked phases — network fetch, strict parse/validation, and an
// observation-guarded publish — instead of regex-scrape-then-store.
//
// What we can prove from this repository: the settings asset is served from a
// Twitch-owned origin (static.twitchcdn.net / assets.twitch.tv, per
// settingsURLPattern) and Usher lives on ttvnw.net. The EXACT spade hostname is
// NOT pinned here (it has changed over time and is not provable from a committed
// fixture), so — exactly as the task allows — the spade URL is required to be
// HTTPS on a Twitch-owned DNS suffix rather than a single speculative hostname.
// This is a deliberate, conservative limitation: a spade endpoint Twitch might
// serve from a non-Twitch CDN would be rejected, which is safer than trusting an
// arbitrary URL scraped from a page.

// spadeHTTPClient is the narrow HTTP surface Spade discovery needs, injectable for
// tests. Satisfied by *http.Client.
type spadeHTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

const (
	// maxChannelPageBytes / maxSettingsBytes bound the scraped responses so a
	// hostile or broken endpoint cannot exhaust memory. The real channel page and
	// settings.js are well under these.
	maxChannelPageBytes = 8 << 20  // 8 MiB
	maxSettingsBytes    = 16 << 20 // 16 MiB

	// spadeFetchUA is a plain browser UA for the page scrapes.
	spadeFetchUA = "Mozilla/5.0 (X11; Linux x86_64; rv:85.0) Gecko/20100101 Firefox/85.0"
)

// twitchOwnedSuffixes is the set of DNS suffixes proven or well-known to be
// Twitch-owned infrastructure. A host is accepted only if it equals one of these
// or is a subdomain of one. twitch.tv/twitchcdn.net back the pages and settings
// asset; ttvnw.net backs Usher/edge; jtvnw.net is Twitch's static/CDN domain.
var twitchOwnedSuffixes = []string{"twitch.tv", "twitchcdn.net", "ttvnw.net", "jtvnw.net"}

// errSpadeDiscovery classifies a redacted spade-discovery failure. It carries a
// stable stage + reason only — never a URL, response body, header, or token — so
// it is safe to log and to surface. It maps to models.ReasonSpadeUnavailable at
// the CheckStreamerOnline boundary.
type errSpadeDiscovery struct {
	stage  string
	reason string
}

func (e *errSpadeDiscovery) Error() string {
	return fmt.Sprintf("spade discovery failed at %s: %s", e.stage, e.reason)
}

func spadeErr(stage, reason string) error { return &errSpadeDiscovery{stage: stage, reason: reason} }

// discoverSpadeURL runs the hardened pipeline: begin a session observation BEFORE
// any I/O (newest-STARTED-wins), fetch + validate the channel page and settings
// asset, extract + validate the spade URL, then publish it only if our
// observation is still the latest. Any failure returns a redacted error and never
// clears the last-known spade URL.
func (c *TwitchClient) discoverSpadeURL(ctx context.Context, streamer *models.Streamer) error {
	obs := streamer.Stream.BeginSessionObservation()

	channelURL := fmt.Sprintf("%s/%s", c.twitchBaseURL, streamer.Username)
	channelBody, err := c.fetchSpadeAsset(ctx, "channel_page", channelURL, c.maxChannelPageBytes, false)
	if err != nil {
		return err
	}

	settingsRaw, err := extractFirstMatch(c.settingsURLPattern, channelBody)
	if err != nil {
		return spadeErr("settings_url", "settings asset URL not found on the channel page")
	}
	settingsURL, err := validateTwitchAssetURL(settingsRaw)
	if err != nil {
		return spadeErr("settings_url", redactURLErr(err))
	}

	// The settings asset must come from a confirmed Twitch-owned origin even after
	// redirects, so a page that points the settings link at a foreign host cannot
	// feed us a spoofed spade_url.
	settingsBody, err := c.fetchSpadeAsset(ctx, "settings_asset", settingsURL, c.maxSettingsBytes, true)
	if err != nil {
		return err
	}

	spadeRaw, err := extractSpadeURL(c.spadeURLPattern, settingsBody)
	if err != nil {
		return err
	}
	spadeURL, err := validateTwitchAssetURL(spadeRaw)
	if err != nil {
		return spadeErr("spade_url", redactURLErr(err))
	}

	// Publish only if our observation is still the latest-begun one; a newer
	// refresh that started after us wins and this (older) result is dropped.
	streamer.Stream.PublishSpadeURLIfCurrent(obs, spadeURL)
	return nil
}

// fetchSpadeAsset GETs a discovery asset with strict status/size checks and an
// optional Twitch-owned final-origin check (after redirects). It closes the body
// and never includes the body in an error.
func (c *TwitchClient) fetchSpadeAsset(ctx context.Context, stage, rawURL string, maxBytes int64, requireTwitchFinal bool) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, spadeErr(stage, "could not build request")
	}
	req.Header.Set("User-Agent", spadeFetchUA)

	resp, err := c.spadeHTTP.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, spadeErr(stage, "request cancelled")
		}
		return nil, spadeErr(stage, "request failed")
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, spadeErr(stage, fmt.Sprintf("unexpected HTTP status %d", resp.StatusCode))
	}

	// Redirect guard: the FINAL URL (after any redirects the client followed) must
	// still be a Twitch-owned origin for assets that require it.
	if requireTwitchFinal && resp.Request != nil && resp.Request.URL != nil {
		if !isTwitchOwnedHost(resp.Request.URL.Hostname()) {
			return nil, spadeErr(stage, "redirected to a non-Twitch origin")
		}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, spadeErr(stage, "read cancelled")
		}
		return nil, spadeErr(stage, "read failed")
	}
	if int64(len(body)) > maxBytes {
		return nil, spadeErr(stage, "response body exceeded the size limit")
	}
	return body, nil
}

// extractFirstMatch returns the first capture group of pat in body.
func extractFirstMatch(pat *regexp.Regexp, body []byte) (string, error) {
	m := pat.FindSubmatch(body)
	if len(m) < 2 {
		return "", errors.New("not found")
	}
	return string(m[1]), nil
}

// extractSpadeURL pulls the spade_url out of the settings asset and JSON-unescapes
// it. The value is a JSON string literal, so escapes like `https:\/\/...` (and any
// \uXXXX) must be decoded — storing the literal backslashed form would be a
// corrupt, unusable URL.
func extractSpadeURL(pat *regexp.Regexp, body []byte) (string, error) {
	raw, err := extractFirstMatch(pat, body)
	if err != nil {
		return "", spadeErr("spade_url", "spade URL not found in the settings asset")
	}
	var decoded string
	if err := json.Unmarshal([]byte(`"`+raw+`"`), &decoded); err != nil {
		return "", spadeErr("spade_url", "malformed spade URL encoding")
	}
	if decoded == "" {
		return "", spadeErr("spade_url", "empty spade URL")
	}
	return decoded, nil
}

// validateTwitchAssetURL parses and strictly validates a discovered URL: it must
// be a well-formed, absolute, HTTPS URL on a Twitch-owned, publicly-routable host,
// with no embedded userinfo and no fragment. Returns the normalized URL string.
func validateTwitchAssetURL(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("empty URL")
	}
	if strings.Contains(raw, "#") {
		return "", errors.New("URL carries a fragment")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("malformed URL")
	}
	if u.Scheme != "https" {
		return "", errors.New("non-HTTPS URL")
	}
	if u.User != nil {
		return "", errors.New("URL carries userinfo")
	}
	if u.Fragment != "" {
		return "", errors.New("URL carries a fragment")
	}
	host := u.Hostname()
	if host == "" {
		return "", errors.New("URL has no host")
	}
	if isDisallowedHost(host) {
		return "", errors.New("URL host is not publicly routable")
	}
	if !isTwitchOwnedHost(host) {
		return "", errors.New("URL host is not Twitch-owned")
	}
	return u.String(), nil
}

// isTwitchOwnedHost reports whether host is (a subdomain of) a proven Twitch-owned
// suffix. A literal IP never matches.
func isTwitchOwnedHost(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	for _, s := range twitchOwnedSuffixes {
		if h == s || strings.HasSuffix(h, "."+s) {
			return true
		}
	}
	return false
}

// isDisallowedHost rejects localhost and any loopback/private/link-local/
// unspecified literal IP so a scraped URL can never point the beacon at an
// internal address (SSRF defense in depth; a literal IP also fails the
// Twitch-owned check).
func isDisallowedHost(host string) bool {
	h := strings.ToLower(host)
	if h == "localhost" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() || ip.IsUnspecified()
	}
	return false
}

// redactURLErr returns the validation error's message. The validation errors
// above are constructed to carry only a classification (never the URL itself), so
// this is safe to surface.
func redactURLErr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
