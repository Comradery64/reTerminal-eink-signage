package config

import (
	"sync"
	"testing"
)

// TestLiveConcurrentLoadStore exercises the exact gap Live closes: concurrent readers must never
// observe anything other than a fully-formed *Config, even while writers are Store-ing new ones.
// Run with -race.
func TestLiveConcurrentLoadStore(t *testing.T) {
	base := validBase()
	live := NewLive(&base)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					c := live.Load()
					if c == nil || len(c.Rooms) == 0 {
						t.Errorf("Load returned an invalid config: %+v", c)
					}
				}
			}
		}()
	}

	for i := 0; i < 200; i++ {
		next := validBase()
		live.Store(&next)
	}
	close(stop)
	wg.Wait()
}

func TestLiveLoadReturnsWhatWasStored(t *testing.T) {
	base := validBase()
	live := NewLive(&base)
	if got := live.Load(); got != &base {
		t.Fatalf("Load() = %p, want the exact pointer passed to NewLive (%p)", got, &base)
	}

	updated := validBase()
	updated.Listen = ":9999"
	live.Store(&updated)
	if got := live.Load(); got.Listen != ":9999" {
		t.Fatalf("Load() after Store did not observe the update: got Listen=%q", got.Listen)
	}
}
