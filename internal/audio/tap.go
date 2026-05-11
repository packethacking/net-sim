package audio

// Tap is a pub/sub broadcast point for raw PCM audio blocks, used by
// the visualiser's "listen to a port" feature. Publishers (router's
// txReader / rxFeeder) call Publish on every block; subscribers (one
// per WebSocket client) receive a copy via the channel returned from
// Subscribe.
//
// Hot-path note: Publish acquires a read lock and is a no-op when no
// subscribers exist for the key. Slow subscribers drop blocks rather
// than stall the audio path.

import "sync"

type Tap struct {
	mu   sync.RWMutex
	subs map[string]map[chan Block]struct{}
}

// NewTap returns a ready-to-use, empty tap.
func NewTap() *Tap {
	return &Tap{subs: map[string]map[chan Block]struct{}{}}
}

// HasSubscribers reports whether anyone is currently listening on key.
// Audio paths can skip the per-block Publish overhead when this is
// false. Cheap (RLock + map lookup).
func (t *Tap) HasSubscribers(key string) bool {
	if t == nil {
		return false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.subs[key]) > 0
}

// Publish fans the block out to every subscriber on key. Each channel
// uses non-blocking send; full buffers drop the oldest block (so a
// stuck WebSocket doesn't gum up the audio path).
func (t *Tap) Publish(key string, b Block) {
	if t == nil {
		return
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	subs := t.subs[key]
	if len(subs) == 0 {
		return
	}
	for ch := range subs {
		select {
		case ch <- b:
		default:
			// Buffer full — drop oldest, push newest.
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- b:
			default:
			}
		}
	}
}

// Subscribe returns a buffered channel of blocks and a cancel function.
// Buffer is ~1 second of audio so a brief consumer pause doesn't
// immediately start dropping.
func (t *Tap) Subscribe(key string) (<-chan Block, func()) {
	ch := make(chan Block, SampleRate/BlockSamples) // ~1 s of blocks
	t.mu.Lock()
	if t.subs[key] == nil {
		t.subs[key] = make(map[chan Block]struct{})
	}
	t.subs[key][ch] = struct{}{}
	t.mu.Unlock()
	cancel := func() {
		t.mu.Lock()
		if set, ok := t.subs[key]; ok {
			if _, in := set[ch]; in {
				delete(set, ch)
				close(ch)
			}
			if len(set) == 0 {
				delete(t.subs, key)
			}
		}
		t.mu.Unlock()
	}
	return ch, cancel
}
