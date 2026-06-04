package router

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/packethacking/net-sim/internal/audio"
	"github.com/packethacking/net-sim/internal/config"
)

func twoNodeCfg() *config.Config {
	return &config.Config{
		Nodes: []config.Node{
			{ID: "a", Ports: []config.Port{{ID: "vhf"}}},
			{ID: "b", Ports: []config.Port{{ID: "vhf"}}},
			{ID: "c", Ports: []config.Port{{ID: "vhf"}}},
		},
	}
}

func newTestRouter(t *testing.T) *Router {
	t.Helper()
	return &Router{
		opts:   Options{RecordDir: t.TempDir()},
		cfg:    twoNodeCfg(),
		logger: quietLogger(),
	}
}

// readWAVChannels returns the channel count from a WAV header.
func readWAVChannels(t *testing.T, path string) uint16 {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(b) < 44 {
		t.Fatalf("%s too short: %d bytes", path, len(b))
	}
	return binary.LittleEndian.Uint16(b[22:24])
}

func wavDataBytes(t *testing.T, path string) uint32 {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return binary.LittleEndian.Uint32(b[40:44])
}

func TestCompositeStartStopProducesStereoFile(t *testing.T) {
	r := newTestRouter(t)

	a := config.PortRef{NodeID: "a", PortID: "vhf"}
	b := config.PortRef{NodeID: "b", PortID: "vhf"}

	path, err := r.StartCompositeRecording([]config.PortRef{a, b})
	if err != nil {
		t.Fatalf("StartCompositeRecording: %v", err)
	}
	// Reliable teardown of the ticker goroutine even if an assertion below
	// fails before the explicit stop (StopCompositeRecording is idempotent).
	defer r.StopCompositeRecording()
	if !r.CompositeActive() {
		t.Fatal("CompositeActive should be true after start")
	}

	// Feed only channel A (left). After stop+drain, both channels must
	// still be present and equal length (B padded with silence).
	cr := r.composite.Load()
	const n = 8
	for i := 0; i < n; i++ {
		cr.feed(a, mkBlockN(int16(2000+i)))
	}

	res, err := r.StopCompositeRecording()
	if err != nil {
		t.Fatalf("StopCompositeRecording: %v", err)
	}
	if r.CompositeActive() {
		t.Fatal("CompositeActive should be false after stop")
	}
	if res.Path != path {
		t.Errorf("result path = %q, want %q", res.Path, path)
	}
	if got := readWAVChannels(t, path); got != 2 {
		t.Errorf("channels = %d, want 2 (stereo)", got)
	}

	// At least the 8 fed blocks must have been written (real-time ticks
	// may have added silence frames before stop; never fewer than 8).
	wantMin := uint32(n * audio.BlockBytes * 2) // 2 channels
	if got := wavDataBytes(t, path); got < wantMin {
		t.Errorf("data bytes = %d, want >= %d", got, wantMin)
	}
	// Data must be a whole number of stereo frames.
	if got := wavDataBytes(t, path); got%uint32(audio.BlockBytes*2) != 0 {
		t.Errorf("data bytes = %d not a multiple of stereo frame size", got)
	}
}

// mkBlockN builds a full BlockBytes mono block at a constant level.
func mkBlockN(level int16) audio.Block {
	b := make(audio.Block, audio.BlockBytes)
	for i := 0; i+1 < audio.BlockBytes; i += 2 {
		b[i] = byte(uint16(level))
		b[i+1] = byte(uint16(level) >> 8)
	}
	return b
}

func TestCompositeDefaultsToFirstTwoPorts(t *testing.T) {
	r := newTestRouter(t)
	path, err := r.StartCompositeRecording(nil)
	if err != nil {
		t.Fatalf("StartCompositeRecording(nil): %v", err)
	}
	defer r.StopCompositeRecording()

	cr := r.composite.Load()
	if len(cr.channels) != 2 {
		t.Fatalf("default channels = %d, want 2", len(cr.channels))
	}
	if cr.channels[0].String() != "a.vhf" || cr.channels[1].String() != "b.vhf" {
		t.Errorf("default channels = %v, want [a.vhf b.vhf]", cr.channels)
	}
	if got := readWAVChannels(t, path); got != 2 {
		t.Errorf("channels = %d, want 2", got)
	}
}

func TestCompositeRejectsUnknownPort(t *testing.T) {
	r := newTestRouter(t)
	_, err := r.StartCompositeRecording([]config.PortRef{{NodeID: "x", PortID: "y"}})
	if err == nil {
		t.Fatal("StartCompositeRecording with unknown port should error")
	}
}

func TestCompositeRejectsDoubleStart(t *testing.T) {
	r := newTestRouter(t)
	if _, err := r.StartCompositeRecording(nil); err != nil {
		t.Fatalf("first start: %v", err)
	}
	defer r.StopCompositeRecording()
	if _, err := r.StartCompositeRecording(nil); err == nil {
		t.Error("second StartCompositeRecording should fail (already recording)")
	}
}

func TestCompositeRejectsNoRecordDir(t *testing.T) {
	r := &Router{opts: Options{}, cfg: twoNodeCfg(), logger: quietLogger()}
	if _, err := r.StartCompositeRecording(nil); err == nil {
		t.Error("StartCompositeRecording with no RecordDir should error")
	}
}

func TestCompositeStopWhenIdleIsNoError(t *testing.T) {
	r := newTestRouter(t)
	res, err := r.StopCompositeRecording()
	if err != nil {
		t.Errorf("StopCompositeRecording with no session: %v", err)
	}
	if res.Path != "" {
		t.Errorf("idle stop result path = %q, want empty", res.Path)
	}
}

func TestCompositeRejectsDuplicatePort(t *testing.T) {
	r := newTestRouter(t)
	a := config.PortRef{NodeID: "a", PortID: "vhf"}
	if _, err := r.StartCompositeRecording([]config.PortRef{a, a}); err == nil {
		t.Error("StartCompositeRecording with a duplicate port should error")
	}
}

// TestCompositeRealTimePacing verifies the recorder advances the timeline
// in real time even when channels are idle: after running ~150 ms with no
// fed audio, the file should hold roughly that many silent frames.
func TestCompositeRealTimePacing(t *testing.T) {
	r := newTestRouter(t)
	path, err := r.StartCompositeRecording(nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer r.StopCompositeRecording()
	time.Sleep(150 * time.Millisecond)
	r.StopCompositeRecording()

	frames := wavDataBytes(t, path) / uint32(audio.BlockBytes*2)
	// 150 ms / 10 ms = ~15 frames. Allow generous slack for scheduling.
	if frames < 5 || frames > 60 {
		t.Errorf("frames after ~150ms = %d, want roughly 15 (5..60)", frames)
	}
}

// TestCompositeConcurrentFeed exercises the real producer/consumer path:
// two goroutines feed two channels while the ticker goroutine drains and
// writes them. Run under -race this catches any unsynchronised access to
// the queues / writer / counters.
func TestCompositeConcurrentFeed(t *testing.T) {
	r := newTestRouter(t)
	a := config.PortRef{NodeID: "a", PortID: "vhf"}
	b := config.PortRef{NodeID: "b", PortID: "vhf"}
	path, err := r.StartCompositeRecording([]config.PortRef{a, b})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer r.StopCompositeRecording()
	cr := r.composite.Load()

	done := make(chan struct{})
	feeder := func(ref config.PortRef) {
		defer func() { done <- struct{}{} }()
		for i := 0; i < 200; i++ {
			cr.feed(ref, mkBlockN(int16(i)))
			time.Sleep(time.Millisecond)
		}
	}
	go feeder(a)
	go feeder(b)
	<-done
	<-done

	res, err := r.StopCompositeRecording()
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if got := readWAVChannels(t, path); got != 2 {
		t.Errorf("channels = %d, want 2", got)
	}
	// 200 blocks per channel were fed; the file must contain at least that
	// many frames (drain flushes any backlog at stop).
	if frames := wavDataBytes(t, path) / uint32(audio.BlockBytes*2); frames < 200 {
		t.Errorf("frames = %d, want >= 200", frames)
	}
	if res.Bytes <= 0 {
		t.Errorf("result bytes = %d, want > 0", res.Bytes)
	}
}

func TestAllPortRefs(t *testing.T) {
	r := newTestRouter(t)
	refs := r.AllPortRefs()
	if len(refs) != 3 {
		t.Fatalf("AllPortRefs = %d, want 3", len(refs))
	}
	if filepath.Base(refs[0].String()) != "a.vhf" {
		t.Errorf("first ref = %s, want a.vhf", refs[0])
	}
}
