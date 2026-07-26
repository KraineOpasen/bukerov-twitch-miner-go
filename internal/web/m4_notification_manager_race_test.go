package web

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// TestSetNotificationManagerConcurrentWithHandlerRace is the M4/MUT-M4-09
// killer: SetNotificationManager (server.go) and the read inside
// handleAPITestNotification both go through s.mu today — this test proves
// that under -race, by driving one goroutine that repeatedly calls
// SetNotificationManager (alternating a real manager and nil) concurrently
// with another goroutine repeatedly invoking the handler (which reads
// s.notificationManager under s.mu.RLock). If SetNotificationManager's
// s.mu.Lock()/Unlock() were ever removed, this reproduces an unsynchronized
// write racing the handler's RLock read. Synchronized purely by a stop
// channel + WaitGroup, no sleep.
func TestSetNotificationManagerConcurrentWithHandlerRace(t *testing.T) {
	s := newRenderServer(t)
	mgr := newNotificationsTestManager(t)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		useReal := false
		for {
			select {
			case <-stop:
				return
			default:
				if useReal {
					s.SetNotificationManager(mgr)
				} else {
					s.SetNotificationManager(nil)
				}
				useReal = !useReal
			}
		}
	}()

	for i := 0; i < 200; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/test-notification", nil)
		s.handleAPITestNotification(rec, req)
	}

	close(stop)
	wg.Wait()
}
