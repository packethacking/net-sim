package router

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/packethacking/net-sim/internal/audio"
	"github.com/packethacking/net-sim/internal/config"
)

// TestScaledPeriods pins the arithmetic every paced loop relies on: the
// rxFeeder/composite block ticker, the txWatchdog tick, and the
// txSilenceWindow all divide by time_scale, and scale <= 1 (including
// the zero value of a hand-built config) is a no-op.
func TestScaledPeriods(t *testing.T) {
	cases := []struct {
		d     time.Duration
		scale float64
		want  time.Duration
	}{
		{blockPeriod, 1, 10 * time.Millisecond},
		{blockPeriod, 10, time.Millisecond},
		{blockPeriod, 0, 10 * time.Millisecond}, // unvalidated zero config = real time
		{txSilenceWindow, 1, 200 * time.Millisecond},
		{txSilenceWindow, 4, 50 * time.Millisecond},
		{txWatchdogTick, 5, 10 * time.Millisecond},
		{txWatchdogTick, 0.5, 50 * time.Millisecond}, // sub-1 scales are clamped to real time
	}
	for _, c := range cases {
		if got := scaled(c.d, c.scale); got != c.want {
			t.Errorf("scaled(%v, %g) = %v, want %v", c.d, c.scale, got, c.want)
		}
	}
}

// collectWriter accumulates rxFeeder output and closes done once target
// bytes have arrived, so the test can stop the feeder without sleeping.
type collectWriter struct {
	mu     sync.Mutex
	buf    []byte
	target int
	done   chan struct{}
	once   sync.Once
}

func (w *collectWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.buf = append(w.buf, p...)
	n := len(w.buf)
	w.mu.Unlock()
	if n >= w.target {
		w.once.Do(func() { close(w.done) })
	}
	return len(p), nil
}

// runRxFeederSequence pre-loads one link queue with nBlocks marked blocks,
// runs the real rxFeeder against it at the given time_scale, and returns
// the first nBlocks of delivered audio.
func runRxFeederSequence(t *testing.T, timeScale float64, nBlocks int) []byte {
	t.Helper()
	src := config.PortRef{NodeID: "a", PortID: "vhf"}
	dst := config.PortRef{NodeID: "b", PortID: "vhf"}
	q := newLinkQueue(src, dst, 0, 0)

	r := &Router{
		cfg:     &config.Config{TimeScale: timeScale},
		mixer:   audio.NewMixer(6, false, "silence"),
		logger:  quietLogger(),
		rxLinks: map[config.PortRef][]*linkQueue{dst: {q}},
	}

	for i := 0; i < nBlocks; i++ {
		blk := make(audio.Block, audio.BlockBytes)
		// Unique nonzero marker in the first sample (LE int16).
		blk[0] = byte(i + 1)
		blk[1] = byte((i + 1) >> 8)
		q.push(blk, r.logger)
	}

	w := &collectWriter{target: nBlocks * audio.BlockBytes, done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	feederDone := make(chan struct{})
	go func() {
		r.rxFeeder(ctx, dst, w)
		close(feederDone)
	}()
	select {
	case <-w.done:
	case <-time.After(10 * time.Second):
		t.Fatal("rxFeeder never delivered the expected blocks")
	}
	cancel()
	<-feederDone
	return w.buf[:nBlocks*audio.BlockBytes]
}

// TestRxFeederSequenceIdenticalAcrossTimeScales: time_scale changes only
// the pacing of delivery, never its content — the block SEQUENCE a
// receiver hears must be byte-identical at scale 1 and at a large scale.
func TestRxFeederSequenceIdenticalAcrossTimeScales(t *testing.T) {
	const nBlocks = 8
	realTime := runRxFeederSequence(t, 1, nBlocks)
	accelerated := runRxFeederSequence(t, 20, nBlocks)
	if !bytes.Equal(realTime, accelerated) {
		t.Fatal("block sequence differs between time_scale 1 and 20 — scaling must affect timing only")
	}
	// And the markers really came through in FIFO order.
	for i := 0; i < nBlocks; i++ {
		off := i * audio.BlockBytes
		got := int(realTime[off]) | int(realTime[off+1])<<8
		if got != i+1 {
			t.Fatalf("block %d carries marker %d, want %d", i, got, i+1)
		}
	}
}
