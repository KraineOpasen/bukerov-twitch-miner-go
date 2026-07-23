package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeOAuth is an httptest-backed stand-in for the three Twitch OAuth
// endpoints (device, token, validate). Handlers are swappable per test; hit
// counters and the refresh tokens presented are recorded for assertions. All
// token values are obvious test strings — no real credentials anywhere.
type fakeOAuth struct {
	t   *testing.T
	srv *httptest.Server

	mu                sync.Mutex
	deviceHits        int
	deviceGrantHits   int
	refreshGrantHits  int
	validateHits      int
	refreshTokensSeen []string

	deviceHandler   http.HandlerFunc
	tokenHandler    http.HandlerFunc // device_code grant polling
	refreshHandler  http.HandlerFunc // refresh_token grant
	validateHandler http.HandlerFunc
}

func newFakeOAuth(t *testing.T) *fakeOAuth {
	t.Helper()
	f := &fakeOAuth{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/device":
			f.mu.Lock()
			f.deviceHits++
			h := f.deviceHandler
			f.mu.Unlock()
			if h == nil {
				f.writeJSON(w, 200, DeviceCodeResponse{
					DeviceCode: "test-device-code", ExpiresIn: 1800, Interval: 5,
					UserCode: "TESTCODE", VerificationURI: "https://example.invalid/activate",
				})
				return
			}
			h(w, r)
		case "/token":
			if err := r.ParseForm(); err != nil {
				t.Errorf("token endpoint: bad form: %v", err)
			}
			switch r.PostForm.Get("grant_type") {
			case "refresh_token":
				f.mu.Lock()
				f.refreshGrantHits++
				f.refreshTokensSeen = append(f.refreshTokensSeen, r.PostForm.Get("refresh_token"))
				h := f.refreshHandler
				f.mu.Unlock()
				if h == nil {
					f.writeJSON(w, 200, TokenResponse{
						AccessToken: "test-access-2", RefreshToken: "test-refresh-2",
						ExpiresIn: 14000, Scope: requiredScopes(), TokenType: "bearer",
					})
					return
				}
				h(w, r)
			default:
				f.mu.Lock()
				f.deviceGrantHits++
				h := f.tokenHandler
				f.mu.Unlock()
				if h == nil {
					f.writeJSON(w, 200, TokenResponse{
						AccessToken: "test-access-df", RefreshToken: "test-refresh-df",
						ExpiresIn: 14000, Scope: requiredScopes(), TokenType: "bearer",
					})
					return
				}
				h(w, r)
			}
		case "/validate":
			f.mu.Lock()
			f.validateHits++
			h := f.validateHandler
			f.mu.Unlock()
			if h == nil {
				f.writeJSON(w, 200, validateResponse{
					ClientID: "ue6666qo983tsx6so1t0vnawi233wa", Login: "tester",
					Scopes: requiredScopes(), UserID: "uid-1", ExpiresIn: 5000,
				})
				return
			}
			h(w, r)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOAuth) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeOAuth) counts() (device, deviceGrant, refreshGrant, validate int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deviceHits, f.deviceGrantHits, f.refreshGrantHits, f.validateHits
}

// oauthError writes the documented Twitch OAuth error body.
func (f *fakeOAuth) oauthError(w http.ResponseWriter, status int, message string) {
	f.writeJSON(w, status, map[string]any{"status": status, "message": message})
}

// newLifecycleAuth returns a TwitchAuth wired to the fake OAuth server, in an
// isolated temp cwd, with an immediate-firing poll timer so no test waits on
// wall-clock intervals.
func newLifecycleAuth(t *testing.T, f *fakeOAuth) *TwitchAuth {
	t.Helper()
	t.Chdir(t.TempDir())
	a := NewTwitchAuth("tester", "device-xyz")
	a.deviceURL = f.srv.URL + "/device"
	a.tokenURL = f.srv.URL + "/token"
	a.validateURL = f.srv.URL + "/validate"
	a.timerAfter = immediateTimer
	return a
}

func immediateTimer(time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	ch <- time.Time{}
	return ch
}
