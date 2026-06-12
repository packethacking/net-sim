package router

import (
	"testing"
	"time"

	"github.com/packethacking/net-sim/internal/audio"
	"github.com/packethacking/net-sim/internal/config"
)

// newSquelchQueue builds a linkQueue with a stubbed clock so transmission
// boundaries can be driven deterministically (no sleeps). The returned
// advance function moves the fake clock forward.
func newSquelchQueue(squelchBlocks int) (q *linkQueue, advance func(time.Duration)) {
	q = newLinkQueue(
		config.PortRef{NodeID: "a", PortID: "vhf"},
		config.PortRef{NodeID: "b", PortID: "vhf"},
		0, 0, squelchBlocks, txSilenceWindow,
	)
	now := time.Unix(1000, 0)
	q.now = func() time.Time { return now }
	return q, func(d time.Duration) { now = now.Add(d) }
}

// markedBlock returns a block whose first sample carries marker (nonzero).
func markedBlock(marker int) audio.Block {
	blk := make(audio.Block, audio.BlockBytes)
	blk[0] = byte(marker)
	blk[1] = byte(marker >> 8)
	return blk
}

// pushBurst pushes n marked blocks (markers base..base+n-1) with 1 ms
// between pushes — well inside the silence window, i.e. one transmission.
func pushBurst(t *testing.T, q *linkQueue, advance func(time.Duration), base, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		q.push(markedBlock(base+i), quietLogger())
		advance(time.Millisecond)
	}
}

// popAll drains the queue, returning each block's first-sample marker
// (0 = silence).
func popAll(t *testing.T, q *linkQueue) []int {
	t.Helper()
	var out []int
	for {
		blk, ok := q.pop()
		if !ok {
			return out
		}
		out = append(out, int(blk[0])|int(blk[1])<<8)
	}
}

// TestSquelchMutesStartOfTransmission: squelch_open_ms=50 (5 blocks at
// 10 ms/block) mutes the first 5 blocks of a burst; the remainder comes
// through intact.
func TestSquelchMutesStartOfTransmission(t *testing.T) {
	const squelchBlocks = 5 // squelchBlocksFor(50)
	q, advance := newSquelchQueue(squelchBlocks)
	pushBurst(t, q, advance, 100, 20)

	got := popAll(t, q)
	if len(got) != 20 {
		t.Fatalf("popped %d blocks, want 20 (squelch must mute, never drop)", len(got))
	}
	for i := 0; i < squelchBlocks; i++ {
		if got[i] != 0 {
			t.Errorf("block %d = marker %d, want silence while squelch opens", i, got[i])
		}
	}
	for i := squelchBlocks; i < 20; i++ {
		if got[i] != 100+i {
			t.Errorf("block %d = marker %d, want %d (audio after squelch opens must be intact)", i, got[i], 100+i)
		}
	}
}

// TestSquelchRearmsAfterIdleGap: a second transmission after a
// silence-window-sized idle gap is muted again from its first block.
func TestSquelchRearmsAfterIdleGap(t *testing.T) {
	q, advance := newSquelchQueue(5)
	pushBurst(t, q, advance, 100, 8)
	advance(txSilenceWindow + 50*time.Millisecond) // key-up gap → new transmission
	pushBurst(t, q, advance, 200, 8)

	got := popAll(t, q)
	if len(got) != 16 {
		t.Fatalf("popped %d blocks, want 16", len(got))
	}
	// First transmission: 5 muted, 3 intact.
	for i := 0; i < 5; i++ {
		if got[i] != 0 {
			t.Errorf("tx1 block %d = %d, want silence", i, got[i])
		}
	}
	for i := 5; i < 8; i++ {
		if got[i] != 100+i {
			t.Errorf("tx1 block %d = %d, want %d", i, got[i], 100+i)
		}
	}
	// Second transmission: muted again from its first block.
	for i := 0; i < 5; i++ {
		if got[8+i] != 0 {
			t.Errorf("tx2 block %d = %d, want silence (squelch must re-arm per transmission)", i, got[8+i])
		}
	}
	for i := 5; i < 8; i++ {
		if got[8+i] != 200+i {
			t.Errorf("tx2 block %d = %d, want %d", i, got[8+i], 200+i)
		}
	}
}

// TestSquelchNotRearmedWithinBurst: a short push gap (an inter-frame pause
// inside one keyup, below the silence window) must not re-mute — same rule
// the txWatchdog uses to keep a multi-frame burst as one keying.
func TestSquelchNotRearmedWithinBurst(t *testing.T) {
	q, advance := newSquelchQueue(3)
	pushBurst(t, q, advance, 100, 5)
	advance(txSilenceWindow / 2) // inter-frame gap, still the same keyup
	pushBurst(t, q, advance, 200, 5)

	got := popAll(t, q)
	if len(got) != 10 {
		t.Fatalf("popped %d blocks, want 10", len(got))
	}
	for i := 3; i < 5; i++ {
		if got[i] != 100+i {
			t.Errorf("block %d = %d, want %d", i, got[i], 100+i)
		}
	}
	for i := 0; i < 5; i++ {
		if got[5+i] != 200+i {
			t.Errorf("second-frame block %d = %d, want %d (must NOT be re-muted mid-keyup)", i, got[5+i], 200+i)
		}
	}
}

// TestSquelchZeroIsTransparent: the default (squelch_open_ms: 0) delivers
// every block byte-identically — today's behaviour exactly.
func TestSquelchZeroIsTransparent(t *testing.T) {
	q, advance := newSquelchQueue(0)
	pushBurst(t, q, advance, 1, 6)
	advance(txSilenceWindow * 2)
	pushBurst(t, q, advance, 100, 6)

	got := popAll(t, q)
	want := []int{1, 2, 3, 4, 5, 6, 100, 101, 102, 103, 104, 105}
	if len(got) != len(want) {
		t.Fatalf("popped %d blocks, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("block %d = %d, want %d (squelch 0 must be transparent)", i, got[i], want[i])
		}
	}
}

// TestSquelchBlocksFor pins the ms→blocks conversion (10 ms blocks,
// rounded up).
func TestSquelchBlocksFor(t *testing.T) {
	cases := []struct {
		ms   float64
		want int
	}{
		{0, 0},
		{-5, 0},
		{1, 1},
		{10, 1},
		{50, 5},
		{55, 6},
		{500, 50},
	}
	for _, c := range cases {
		if got := squelchBlocksFor(c.ms); got != c.want {
			t.Errorf("squelchBlocksFor(%g) = %d, want %d", c.ms, got, c.want)
		}
	}
}
