package pubsub

import (
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// TestFindStreamerRaceFreeWithUpdateStreamers is the regression for the data
// race on p.streamers: findStreamer ranges the slice on every incoming PubSub
// message (the connection read-loop goroutine), while UpdateStreamers replaces
// it from the settings goroutine. Without a read lock in findStreamer this is a
// concurrent read/write of the slice header, which `go test -race` reports.
func TestFindStreamerRaceFreeWithUpdateStreamers(t *testing.T) {
	p := &WebSocketPool{streamers: []*models.Streamer{{ChannelID: "1"}}}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)

	// Writer: replace the streamer slice under the write lock.
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				p.UpdateStreamers([]*models.Streamer{{ChannelID: "1"}, {ChannelID: "2"}})
			}
		}
	}()

	// Reader: the handleMessage lookup path, ranging p.streamers.
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = p.findStreamer("2")
			}
		}
	}()

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
}
