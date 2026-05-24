package router

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/packethacking/net-sim/internal/audio"
	"github.com/packethacking/net-sim/internal/config"
	"github.com/packethacking/net-sim/internal/tnc"
)

// makeBlock builds a mono int16-LE block where every sample has the
// given amplitude. A value >= txActiveThreshold (1500) looks like
// active TX audio; 0 looks like silence.
func makeBlock(amplitude int16) audio.Block {
	blk := make(audio.Block, audio.BlockBytes)
	for i := 0; i+1 < len(blk); i += 2 {
		binary.LittleEndian.PutUint16(blk[i:], uint16(amplitude))
	}
	return blk
}

// feedBlocks writes n copies of blk into w and then closes it.
func feedBlocks(w io.WriteCloser, blocks []audio.Block) {
	for _, blk := range blocks {
		_, _ = w.Write(blk)
	}
	w.Close()
}

// drainQueue pops all available blocks from a linkQueue within a
// reasonable timeout, returning the total count and the count of
// non-silent (audible) blocks.
func drainQueue(q *linkQueue, timeout time.Duration) (total, audible int) {
	deadline := time.After(timeout)
	for {
		select {
		case blk := <-q.ch:
			total++
			if !blk.IsSilence() {
				audible++
			}
		case <-deadline:
			return
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

// TestRxToTxRampSuppressesEarlyBlocks verifies that the first N blocks
// of a new transmission are NOT routed to the link queue when the port
// has an RX-to-TX turnaround configured.
func TestRxToTxRampSuppressesEarlyBlocks(t *testing.T) {
	const rampMs = 30 // 30 ms → 3 blocks suppressed
	const totalActive = 10

	ref := config.PortRef{NodeID: "a", PortID: "vhf"}
	dst := config.PortRef{NodeID: "b", PortID: "vhf"}

	cfg := &config.Config{
		Nodes: []config.Node{
			{ID: "a", Ports: []config.Port{
				{ID: "vhf", RxToTxMs: rampMs},
			}},
			{ID: "b", Ports: []config.Port{
				{ID: "vhf"},
			}},
		},
	}

	q := newLinkQueue(ref, dst, 0, 0)
	r := &Router{
		cfg:        cfg,
		logger:     quietLogger(),
		linkQueues: map[config.PortRef][]*linkQueue{ref: {q}},
		rxLinks:    map[config.PortRef][]*linkQueue{},
		txTrackers: map[config.PortRef]*txTracker{ref: {}},
		opts:       Options{},
	}

	pr, pw := io.Pipe()
	child := tnc.NewTestChild(nopWriteCloser{io.Discard}, pr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.txReader(ctx, ref, child)
	}()

	// Feed totalActive loud blocks (a single TX burst).
	var blocks []audio.Block
	for i := 0; i < totalActive; i++ {
		blocks = append(blocks, makeBlock(5000))
	}
	go feedBlocks(pw, blocks)

	_, audible := drainQueue(q, 500*time.Millisecond)
	cancel()
	wg.Wait()

	// rampMs=30 → 3 blocks suppressed, so 10-3=7 should arrive.
	want := totalActive - (rampMs / 10)
	if audible != want {
		t.Errorf("audible blocks routed = %d, want %d (ramp suppressed %d)", audible, want, rampMs/10)
	}
}

// TestRxToTxZeroRampRoutesAll verifies that with no turnaround
// configured, every TX block is routed.
func TestRxToTxZeroRampRoutesAll(t *testing.T) {
	const totalActive = 8

	ref := config.PortRef{NodeID: "a", PortID: "vhf"}
	dst := config.PortRef{NodeID: "b", PortID: "vhf"}

	cfg := &config.Config{
		Nodes: []config.Node{
			{ID: "a", Ports: []config.Port{{ID: "vhf"}}},
			{ID: "b", Ports: []config.Port{{ID: "vhf"}}},
		},
	}

	q := newLinkQueue(ref, dst, 0, 0)
	r := &Router{
		cfg:        cfg,
		logger:     quietLogger(),
		linkQueues: map[config.PortRef][]*linkQueue{ref: {q}},
		rxLinks:    map[config.PortRef][]*linkQueue{},
		txTrackers: map[config.PortRef]*txTracker{ref: {}},
		opts:       Options{},
	}

	pr, pw := io.Pipe()
	child := tnc.NewTestChild(nopWriteCloser{io.Discard}, pr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.txReader(ctx, ref, child)
	}()

	var blocks []audio.Block
	for i := 0; i < totalActive; i++ {
		blocks = append(blocks, makeBlock(5000))
	}
	go feedBlocks(pw, blocks)

	_, audible := drainQueue(q, 500*time.Millisecond)
	cancel()
	wg.Wait()

	if audible != totalActive {
		t.Errorf("audible blocks routed = %d, want %d (no ramp)", audible, totalActive)
	}
}

// TestRxToTxRampResetsOnNewBurst verifies that the ramp-up countdown
// resets on each new TX burst (silence gap between bursts).
func TestRxToTxRampResetsOnNewBurst(t *testing.T) {
	const rampMs = 20 // 2 blocks suppressed per burst
	const burstLen = 6
	const silenceGap = 3

	ref := config.PortRef{NodeID: "a", PortID: "vhf"}
	dst := config.PortRef{NodeID: "b", PortID: "vhf"}

	cfg := &config.Config{
		Nodes: []config.Node{
			{ID: "a", Ports: []config.Port{
				{ID: "vhf", RxToTxMs: rampMs},
			}},
			{ID: "b", Ports: []config.Port{{ID: "vhf"}}},
		},
	}

	q := newLinkQueue(ref, dst, 0, 0)
	r := &Router{
		cfg:        cfg,
		logger:     quietLogger(),
		linkQueues: map[config.PortRef][]*linkQueue{ref: {q}},
		rxLinks:    map[config.PortRef][]*linkQueue{},
		txTrackers: map[config.PortRef]*txTracker{ref: {}},
		opts:       Options{},
	}

	pr, pw := io.Pipe()
	child := tnc.NewTestChild(nopWriteCloser{io.Discard}, pr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.txReader(ctx, ref, child)
	}()

	// Burst 1: 6 loud blocks, then 3 silent, then Burst 2: 6 loud.
	var blocks []audio.Block
	for i := 0; i < burstLen; i++ {
		blocks = append(blocks, makeBlock(5000))
	}
	for i := 0; i < silenceGap; i++ {
		blocks = append(blocks, makeBlock(0))
	}
	for i := 0; i < burstLen; i++ {
		blocks = append(blocks, makeBlock(5000))
	}
	go feedBlocks(pw, blocks)

	rampBlocks := rampMs / 10
	// Each burst loses rampBlocks audible blocks.
	wantAudible := 2 * (burstLen - rampBlocks)
	_, audible := drainQueue(q, 500*time.Millisecond)
	cancel()
	wg.Wait()

	if audible != wantAudible {
		t.Errorf("audible blocks routed = %d, want %d (two bursts, each losing %d to ramp)", audible, wantAudible, rampBlocks)
	}
}

// TestRxToTxRampFromProfile verifies that the ramp value is resolved
// from a radio profile when the port doesn't set it directly.
func TestRxToTxRampFromProfile(t *testing.T) {
	const totalActive = 10

	ref := config.PortRef{NodeID: "a", PortID: "vhf"}
	dst := config.PortRef{NodeID: "b", PortID: "vhf"}

	cfg := &config.Config{
		RadioProfiles: []config.RadioProfile{
			{Name: "slow", RxToTxMs: 40},
		},
		Nodes: []config.Node{
			{ID: "a", Ports: []config.Port{
				{ID: "vhf", Profile: "slow"},
			}},
			{ID: "b", Ports: []config.Port{{ID: "vhf"}}},
		},
	}

	q := newLinkQueue(ref, dst, 0, 0)
	r := &Router{
		cfg:        cfg,
		logger:     quietLogger(),
		linkQueues: map[config.PortRef][]*linkQueue{ref: {q}},
		rxLinks:    map[config.PortRef][]*linkQueue{},
		txTrackers: map[config.PortRef]*txTracker{ref: {}},
		opts:       Options{},
	}

	pr, pw := io.Pipe()
	child := tnc.NewTestChild(nopWriteCloser{io.Discard}, pr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.txReader(ctx, ref, child)
	}()

	var blocks []audio.Block
	for i := 0; i < totalActive; i++ {
		blocks = append(blocks, makeBlock(5000))
	}
	go feedBlocks(pw, blocks)

	_, audible := drainQueue(q, 500*time.Millisecond)
	cancel()
	wg.Wait()

	want := totalActive - 4 // 40ms / 10ms = 4 blocks suppressed
	if audible != want {
		t.Errorf("audible blocks routed = %d, want %d (profile ramp = 40ms)", audible, want)
	}
}

// TestTxToRxDeafWindow verifies that after a port's TX ends, the
// rxFeeder outputs silence for the configured deaf window even when
// audio is available on the link queues.
func TestTxToRxDeafWindow(t *testing.T) {
	const deafMs = 30 // 3 blocks of deafness

	dst := config.PortRef{NodeID: "a", PortID: "vhf"}
	src := config.PortRef{NodeID: "b", PortID: "vhf"}

	cfg := &config.Config{
		Nodes: []config.Node{
			{ID: "a", Ports: []config.Port{
				{ID: "vhf", TxToRxMs: deafMs},
			}},
			{ID: "b", Ports: []config.Port{{ID: "vhf"}}},
		},
	}

	q := newLinkQueue(src, dst, 0, 0)
	tt := &txTracker{}
	r := &Router{
		cfg:        cfg,
		logger:     quietLogger(),
		mixer:      audio.NewMixer(6.0, false, "silence"),
		linkQueues: map[config.PortRef][]*linkQueue{},
		rxLinks:    map[config.PortRef][]*linkQueue{dst: {q}},
		txTrackers: map[config.PortRef]*txTracker{dst: tt},
		opts:       Options{},
	}

	// Pre-fill the queue with loud blocks. The rxFeeder will drain
	// one per tick (~10ms).
	const totalBlocks = 10
	for i := 0; i < totalBlocks; i++ {
		q.pushNonBlocking(makeBlock(5000), r.logger)
	}

	// Simulate: port was transmitting, then TX ends after 2 ticks.
	tt.active.Store(true)

	var rxBuf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.rxFeeder(ctx, dst, &rxBuf)
	}()

	// Let rxFeeder run 2 ticks while TX is active (it should still
	// output mixed audio — or silence because the port is
	// self-transmitting, but the mixer will return the queued blocks
	// regardless since self-mute is enforced by config, not mixer).
	time.Sleep(25 * time.Millisecond)

	// TX ends → deaf window starts.
	tt.active.Store(false)

	// Let it run long enough for all blocks to be consumed.
	time.Sleep(150 * time.Millisecond)
	cancel()
	wg.Wait()

	// Count how many blocks were output in total.
	outputBlocks := rxBuf.Len() / audio.BlockBytes
	if outputBlocks == 0 {
		t.Fatal("rxFeeder produced no output")
	}

	// Count silent blocks: blocks where every byte is 0.
	silentCount := 0
	data := rxBuf.Bytes()
	for i := 0; i+audio.BlockBytes <= len(data); i += audio.BlockBytes {
		blk := audio.Block(data[i : i+audio.BlockBytes])
		if blk.IsSilence() {
			silentCount++
		}
	}

	// We expect at least deafMs/10 = 3 blocks of forced silence from
	// the deaf window. There may also be silence blocks from before
	// any audio arrived or after the queue drained.
	deafBlocks := deafMs / 10
	if silentCount < deafBlocks {
		t.Errorf("silent blocks = %d, want >= %d (deaf window)", silentCount, deafBlocks)
	}
}

// TestTxToRxNoDeafWhenZero verifies that with no turnaround, the
// rxFeeder immediately passes through audio after TX ends.
func TestTxToRxNoDeafWhenZero(t *testing.T) {
	dst := config.PortRef{NodeID: "a", PortID: "vhf"}
	src := config.PortRef{NodeID: "b", PortID: "vhf"}

	cfg := &config.Config{
		Nodes: []config.Node{
			{ID: "a", Ports: []config.Port{{ID: "vhf"}}},
			{ID: "b", Ports: []config.Port{{ID: "vhf"}}},
		},
	}

	q := newLinkQueue(src, dst, 0, 0)
	tt := &txTracker{}
	r := &Router{
		cfg:        cfg,
		logger:     quietLogger(),
		mixer:      audio.NewMixer(6.0, false, "silence"),
		linkQueues: map[config.PortRef][]*linkQueue{},
		rxLinks:    map[config.PortRef][]*linkQueue{dst: {q}},
		txTrackers: map[config.PortRef]*txTracker{dst: tt},
		opts:       Options{},
	}

	// Pre-fill the queue with loud blocks.
	const totalBlocks = 5
	for i := 0; i < totalBlocks; i++ {
		q.pushNonBlocking(makeBlock(5000), r.logger)
	}

	// Simulate TX ending immediately.
	tt.active.Store(true)

	var rxBuf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.rxFeeder(ctx, dst, &rxBuf)
	}()

	time.Sleep(15 * time.Millisecond)
	tt.active.Store(false)

	// Let all blocks drain.
	time.Sleep(100 * time.Millisecond)
	cancel()
	wg.Wait()

	// Count non-silent blocks: with zero turnaround, audio from the
	// queue should pass through immediately after TX ends.
	nonSilent := 0
	data := rxBuf.Bytes()
	for i := 0; i+audio.BlockBytes <= len(data); i += audio.BlockBytes {
		blk := audio.Block(data[i : i+audio.BlockBytes])
		if !blk.IsSilence() {
			nonSilent++
		}
	}

	if nonSilent == 0 {
		t.Error("expected non-silent blocks with zero turnaround, got all silence")
	}
}

// TestPortNoiseFloorFromProfile verifies that the portNoiseFloor
// resolver picks up NoiseDB from the radio profile.
func TestPortNoiseFloorFromProfile(t *testing.T) {
	cfg := &config.Config{
		RadioProfiles: []config.RadioProfile{
			{Name: "noisy", NoiseDB: 25},
		},
		Nodes: []config.Node{
			{ID: "a", Ports: []config.Port{
				{ID: "vhf", Profile: "noisy"},
			}},
		},
		DefaultNoiseDB: 10,
	}
	r := &Router{cfg: cfg, logger: quietLogger()}
	got := r.portNoiseFloor(config.PortRef{NodeID: "a", PortID: "vhf"})
	if got != 25 {
		t.Errorf("portNoiseFloor = %g, want 25 (from profile)", got)
	}
}

// TestPortNoiseFloorPortOverridesProfile verifies that a port-level
// NoiseDB takes precedence over the profile's.
func TestPortNoiseFloorPortOverridesProfile(t *testing.T) {
	cfg := &config.Config{
		RadioProfiles: []config.RadioProfile{
			{Name: "noisy", NoiseDB: 25},
		},
		Nodes: []config.Node{
			{ID: "a", Ports: []config.Port{
				{ID: "vhf", Profile: "noisy", NoiseDB: 30},
			}},
		},
		DefaultNoiseDB: 10,
	}
	r := &Router{cfg: cfg, logger: quietLogger()}
	got := r.portNoiseFloor(config.PortRef{NodeID: "a", PortID: "vhf"})
	if got != 30 {
		t.Errorf("portNoiseFloor = %g, want 30 (port override)", got)
	}
}

// TestPortNoiseFloorFallsToDefault verifies that when neither port nor
// profile sets NoiseDB, the global default is used.
func TestPortNoiseFloorFallsToDefault(t *testing.T) {
	cfg := &config.Config{
		Nodes: []config.Node{
			{ID: "a", Ports: []config.Port{{ID: "vhf"}}},
		},
		DefaultNoiseDB: 12,
	}
	r := &Router{cfg: cfg, logger: quietLogger()}
	got := r.portNoiseFloor(config.PortRef{NodeID: "a", PortID: "vhf"})
	if got != 12 {
		t.Errorf("portNoiseFloor = %g, want 12 (global default)", got)
	}
}

// nopWriteCloser wraps an io.Writer with a no-op Close.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
