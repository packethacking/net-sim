package events

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPublishReachesSubscriber(t *testing.T) {
	b := NewBus()
	ch, cancel := b.Subscribe()
	defer cancel()

	b.Publish(Event{Type: TXStart, Port: "a.vhf"})

	select {
	case e := <-ch:
		if e.Type != TXStart || e.Port != "a.vhf" {
			t.Errorf("got %+v, want TXStart/a.vhf", e)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestMultipleSubscribersAllReceive(t *testing.T) {
	b := NewBus()
	const n = 4
	channels := make([]<-chan Event, n)
	cancels := make([]func(), n)
	for i := 0; i < n; i++ {
		channels[i], cancels[i] = b.Subscribe()
	}
	defer func() {
		for _, c := range cancels {
			c()
		}
	}()

	b.Publish(Event{Type: TXStart, Port: "x"})

	for i, ch := range channels {
		select {
		case e := <-ch:
			if e.Port != "x" {
				t.Errorf("sub %d: got %+v", i, e)
			}
		case <-time.After(1 * time.Second):
			t.Errorf("sub %d: timed out", i)
		}
	}
}

func TestSlowSubscriberDoesNotBlockPublisher(t *testing.T) {
	b := NewBus()
	// One subscriber that never reads. Buffer is 1024 — flood it well
	// past that and verify Publish still returns promptly.
	_, cancel := b.Subscribe()
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 10_000; i++ {
			b.Publish(Event{Type: TXStart, Port: "flood"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publisher blocked on slow subscriber")
	}
}

func TestCancelUnsubscribes(t *testing.T) {
	b := NewBus()
	ch, cancel := b.Subscribe()
	cancel()
	// After cancel, the channel is closed; reading should observe close
	// immediately rather than hanging.
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("channel should be closed after cancel")
		}
	case <-time.After(500 * time.Millisecond):
		t.Error("cancel did not close channel")
	}
	// Further Publish on the bus must not panic (subscription is gone).
	b.Publish(Event{Type: TXStart, Port: "z"})
}

func TestCancelTwiceIsSafe(t *testing.T) {
	b := NewBus()
	_, cancel := b.Subscribe()
	cancel()
	cancel() // must not double-close / panic
}

func TestCloseDropsSubscribersAndStopsPublish(t *testing.T) {
	b := NewBus()
	ch, _ := b.Subscribe()
	b.Close()
	// All subscriber channels closed.
	if _, ok := <-ch; ok {
		t.Error("subscriber channel should be closed after bus Close()")
	}
	// Publish after Close is a no-op (also: no panic).
	b.Publish(Event{Type: TXStart, Port: "z"})
}

func TestCloseTwiceIsSafe(t *testing.T) {
	b := NewBus()
	b.Close()
	b.Close()
}

func TestNilBusIsSafe(t *testing.T) {
	var b *Bus
	b.Publish(Event{Type: TXStart, Port: "z"}) // must not panic
	b.Close()                                  // must not panic
}

func TestEventTypeConstantsAreStable(t *testing.T) {
	// These strings are part of the wire format consumed by the
	// visualiser — assert them so a casual rename gets caught.
	cases := []struct {
		got, want string
	}{
		{string(TXStart), "tx_start"},
		{string(TXEnd), "tx_end"},
		{string(RXDecision), "rx_decision"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("event type drift: got %q, want %q", c.got, c.want)
		}
	}
}

// TestConcurrentPublishSubscribe is a stress test: many publishers and
// subscribers running for a brief window, all events must reach all
// subscribers (or be dropped consistently) without races.
func TestConcurrentPublishSubscribe(t *testing.T) {
	b := NewBus()
	defer b.Close()

	const subs = 8
	const events = 200

	var wg sync.WaitGroup
	totalSeen := atomic.Int64{}

	for i := 0; i < subs; i++ {
		ch, cancel := b.Subscribe()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer cancel()
			deadline := time.After(2 * time.Second)
			for {
				select {
				case _, ok := <-ch:
					if !ok {
						return
					}
					totalSeen.Add(1)
				case <-deadline:
					return
				}
			}
		}()
	}

	// brief settle so subs are registered
	time.Sleep(20 * time.Millisecond)

	for i := 0; i < events; i++ {
		b.Publish(Event{Type: TXStart, Port: "x"})
	}

	// Let consumers drain.
	time.Sleep(300 * time.Millisecond)
	b.Close()
	wg.Wait()

	// With a 1024 buffer and 200 events, no drops expected.
	if got := totalSeen.Load(); got < int64(subs*events) {
		t.Errorf("totalSeen = %d, want >= %d (subs*events)", got, subs*events)
	}
}
