// Package router orchestrates the simulator: spawns samoyed children, owns
// the topology, and routes audio between ports per the link table and
// receiver-side mixer.
//
// Audio flow per port P:
//
//	     ┌─ KISS TCP ───────────────────────┐
//	     │ (samoyed exposes; router doesn't │
//	     │  touch KISS frames)              │
//	     │                                  │
//	stdin: ◄── rxFeeder ── mixer ── all S where link S→P
//	     │                                  │
//	     │                  fresh blocks ──►│
//	     │                                  │
//	udp out: ── txReader ── for each link P→D, push to D's bus
//
// Each samoyed gets continuous PCM on stdin (silence-filled when no peer
// is transmitting) so its demodulator never starves.
//
// samoyed only writes to its UDP output when actually keying. Importantly,
// samoyed produces TX audio as fast as it can compute it (no pacing): a
// 500 ms frame can land on UDP in a few wall-clock ms. We therefore queue
// per-link blocks and the receiver-side rxFeeder drains them at exactly
// SampleRate/BlockSamples Hz, so the audio reaches each samoyed at the
// rate it expects to consume it.
package router

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/packethacking/net-sim/internal/audio"
	"github.com/packethacking/net-sim/internal/config"
	"github.com/packethacking/net-sim/internal/events"
	"github.com/packethacking/net-sim/internal/tnc"
)

// txActiveThreshold is the audio peak (0–32767) above which a port's
// transmitted block counts as "keyed." Tuned by eye against samoyed's
// preamble levels — comfortably above the residual non-zero noise the
// encoder leaves behind, well below modulator full-scale.
const txActiveThreshold = 1500

// txSilenceWindow is the wall-clock idle gap after which a tx_end event
// fires. We use a wall-clock watchdog rather than counting silent
// blocks because samoyed stops sending UDP datagrams entirely at burst
// end (rather than streaming silence), so block-count silence detection
// would never advance after the last audible block. 200 ms is long
// enough to bridge the inter-frame gaps inside a multi-frame burst
// without falsely closing keying, short enough to mark "key up" cleanly.
const txSilenceWindow = 200 * time.Millisecond

// Options configures a Router. All paths default to "look up on $PATH".
type Options struct {
	SamoyedBin          string // path to samoyed-direwolf
	DirewolfBin         string // path to direwolf
	WorkDir             string // where temporary config files / FIFOs go
	Verbose             bool   // log every routing decision
	Logger              *slog.Logger
	StartingRxAudioPort int // first ephemeral UDP port for samoyed-→-router audio

	// RecordDir, if non-empty, is the base directory under which WAV
	// recordings are written. Each call to StartRecording (or the
	// auto-start path triggered by RecordOnStart) creates a fresh
	// timestamped subdirectory holding two files per port — <node>.<port>.tx.wav
	// and <node>.<port>.rx.wav.
	RecordDir string

	// RecordOnStart causes Router.Start to begin recording immediately
	// after children come up. Ignored if RecordDir is empty.
	RecordOnStart bool

	// EventBus, if non-nil, receives per-port activity events from the
	// audio path (tx_start / tx_end / rx_decision). Publishing is
	// non-blocking, so leaving the bus attached but unsubscribed is
	// cheap. See the events package.
	EventBus *events.Bus

	// AudioTap, if non-nil, receives raw PCM blocks from the TX and RX
	// audio paths, keyed by "<node>.<port>|tx" or "<node>.<port>|rx".
	// Per-block Publish is a no-op when nobody's subscribed to a key,
	// so leaving the tap attached is cheap.
	AudioTap *audio.Tap
}

// Router is the running simulator.
type Router struct {
	opts   Options
	cfg    *config.Config
	mixer  *audio.Mixer
	logger *slog.Logger

	mu       sync.RWMutex
	children map[config.PortRef]*tnc.Child

	// linkQueues hold per-link audio: when source S transmits, blocks are
	// pushed into linkQueues[S][index] for every link S→D. The destination
	// D's rxFeeder drains the corresponding queue at sample rate.
	//
	// A separate queue per link (rather than one per source) means each
	// destination gets its own back-pressure / drop behaviour and we don't
	// need to track per-destination read positions on a shared queue.
	linkQueues map[config.PortRef][]*linkQueue // keyed by source
	rxLinks    map[config.PortRef][]*linkQueue // keyed by destination

	// session is the active recording session, or nil. Read on the audio
	// hot path (txReader, rxFeeder) so it lives in an atomic pointer.
	// recordMu serialises Start/StopRecording against each other and
	// against shutdown.
	session  atomic.Pointer[recordSession]
	recordMu sync.Mutex

	// txTrackers — one per port — record the wall-clock time of each
	// port's last above-threshold TX block. The txWatchdog goroutine
	// fires tx_end when a tracker hasn't been bumped within
	// txSilenceWindow. Populated at Start; pointers are stable.
	txTrackers map[config.PortRef]*txTracker

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// txTracker carries the wall-clock TX-active state for one port,
// shared by the port's txReader (writes lastBusyNanos / sets active)
// and the txWatchdog (clears active on staleness).
type txTracker struct {
	active        atomic.Bool
	lastBusyNanos atomic.Int64
}

// linkQueue is one source→destination link's audio buffer. The capacity
// is sized for ~3 s of audio at SampleRate / BlockSamples blocks per
// second — large enough to absorb a full samoyed TX burst (preamble +
// frame + postamble for typical AX.25) without dropping.
type linkQueue struct {
	src, dst config.PortRef
	loss     float64
	noise    float64
	ch       chan audio.Block
}

func newLinkQueue(src, dst config.PortRef, loss, noise float64) *linkQueue {
	const capBlocks = 3 * audio.SampleRate / audio.BlockSamples
	return &linkQueue{
		src:   src,
		dst:   dst,
		loss:  loss,
		noise: noise,
		ch:    make(chan audio.Block, capBlocks),
	}
}

// pushNonBlocking enqueues a block; drops oldest if full. Logs the drop.
func (q *linkQueue) pushNonBlocking(blk audio.Block, logger *slog.Logger) {
	select {
	case q.ch <- blk:
		return
	default:
	}
	// Full: drop the oldest block to make room. This indicates the
	// downstream rxFeeder is falling behind — usually a sign something
	// has stalled rather than a normal-operations event, so log it.
	select {
	case <-q.ch:
	default:
	}
	select {
	case q.ch <- blk:
	default:
	}
	logger.Warn("audio queue overflow", "from", q.src, "to", q.dst)
}

// pop returns one block if immediately available.
func (q *linkQueue) pop() (audio.Block, bool) {
	select {
	case b := <-q.ch:
		return b, true
	default:
		return nil, false
	}
}

// Start spawns all samoyed children and begins routing audio.
func Start(ctx context.Context, cfg *config.Config, opts Options) (*Router, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.StartingRxAudioPort == 0 {
		opts.StartingRxAudioPort = 17000
	}

	for _, n := range cfg.Nodes {
		for _, p := range n.Ports {
			if err := tnc.SupportedMode(p.Modem); err != nil {
				return nil, fmt.Errorf("%s.%s: %w", n.ID, p.ID, err)
			}
		}
	}

	rctx, cancel := context.WithCancel(ctx)
	r := &Router{
		opts:       opts,
		cfg:        cfg,
		mixer:      audio.NewMixer(cfg.CaptureDB, cfg.MixerMode == config.MixerLinearSum, string(cfg.CollisionMode)),
		logger:     opts.Logger,
		children:   map[config.PortRef]*tnc.Child{},
		linkQueues: map[config.PortRef][]*linkQueue{},
		rxLinks:    map[config.PortRef][]*linkQueue{},
		txTrackers: map[config.PortRef]*txTracker{},
		cancel:     cancel,
	}

	for _, l := range cfg.Links {
		fr, _ := parsePortRef(l.From)
		to, _ := parsePortRef(l.To)
		q := newLinkQueue(fr, to, l.LossDB, l.NoiseDB)
		r.linkQueues[fr] = append(r.linkQueues[fr], q)
		r.rxLinks[to] = append(r.rxLinks[to], q)
	}

	udpPort := opts.StartingRxAudioPort
	for _, n := range cfg.Nodes {
		for _, p := range n.Ports {
			ref := config.PortRef{NodeID: n.ID, PortID: p.ID}
			backend := tnc.Backend(p.TNC)
			if backend == "" {
				backend = tnc.BackendSamoyed
			}
			spec := tnc.Spec{
				Backend:        backend,
				NodeID:         n.ID,
				PortID:         p.ID,
				Modem:          p.Modem,
				KissPort:       p.KissPort,
				RxAudioUDPPort: udpPort,
				SamoyedBin:     opts.SamoyedBin,
				DirewolfBin:    opts.DirewolfBin,
				WorkDir:        opts.WorkDir,
			}
			// direwolf doesn't use the per-port UDP port. Reserve it
			// anyway so subsequent samoyed ports get the next one
			// regardless of preceding direwolf ports — keeps the UDP
			// port assignment stable as the topology is edited.
			udpPort++

			child, err := tnc.Start(rctx, spec)
			if err != nil {
				_ = r.shutdown()
				return nil, fmt.Errorf("start %s: %w", ref, err)
			}
			r.children[ref] = child

			fields := []any{
				"node", n.ID, "port", p.ID, "tnc", string(backend),
				"mode", p.Modem.Mode, "kiss_tcp", p.KissPort,
			}
			if backend == tnc.BackendSamoyed {
				fields = append(fields, "rx_audio_udp", spec.RxAudioUDPPort)
			}
			r.logger.Info("port up", fields...)
		}
	}

	// One tx tracker per port — pointers are stable across the run.
	for ref := range r.children {
		r.txTrackers[ref] = &txTracker{}
	}

	for ref, child := range r.children {
		ref := ref
		child := child
		r.wg.Add(2)
		go func() {
			defer r.wg.Done()
			r.txReader(rctx, ref, child)
		}()
		go func() {
			defer r.wg.Done()
			r.rxFeeder(rctx, ref, child.Stdin())
		}()
		if opts.EventBus != nil {
			r.wg.Add(1)
			go func() {
				defer r.wg.Done()
				r.txWatchdog(rctx, ref)
			}()
		}
	}

	for ref, child := range r.children {
		ref := ref
		child := child
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			err := child.Wait()
			if rctx.Err() == nil {
				r.logger.Error("samoyed child exited unexpectedly", "port", ref, "err", err)
				cancel()
			}
		}()
	}

	if opts.RecordDir != "" && opts.RecordOnStart {
		if _, err := r.StartRecording(); err != nil {
			r.logger.Warn("auto-start recording failed", "err", err)
		}
	}

	return r, nil
}

// Stop terminates all children and waits for routing goroutines to exit.
func (r *Router) Stop() error {
	r.cancel()
	return r.shutdown()
}

func (r *Router) shutdown() error {
	for _, c := range r.children {
		_ = c.Stop()
	}
	r.wg.Wait()
	if s := r.session.Swap(nil); s != nil {
		s.Close()
	}
	return nil
}

// StartRecording opens a fresh timestamped subdirectory under
// Options.RecordDir, opens a TX + RX wav per port, and installs the
// session as the live recorder. It is an error to call this when a
// session is already active or when RecordDir is empty. Returns the
// absolute directory path being recorded into.
func (r *Router) StartRecording() (string, error) {
	r.recordMu.Lock()
	defer r.recordMu.Unlock()
	if r.opts.RecordDir == "" {
		return "", errors.New("recorder: no RecordDir configured")
	}
	if r.session.Load() != nil {
		return "", errors.New("recorder: already recording")
	}
	var refs []config.PortRef
	for _, n := range r.cfg.Nodes {
		for _, p := range n.Ports {
			refs = append(refs, config.PortRef{NodeID: n.ID, PortID: p.ID})
		}
	}
	s, err := newRecordSession(r.opts.RecordDir, refs, r.logger)
	if err != nil {
		return "", err
	}
	r.session.Store(s)
	return s.Dir(), nil
}

// StopRecording closes the current session (flushes WAV headers) and
// disarms the audio-path tap. No error if there is no active session.
func (r *Router) StopRecording() error {
	r.recordMu.Lock()
	defer r.recordMu.Unlock()
	s := r.session.Swap(nil)
	if s == nil {
		return nil
	}
	s.Close()
	return nil
}

// RecordingActive reports whether a session is currently capturing.
func (r *Router) RecordingActive() bool { return r.session.Load() != nil }

// RecordingDir returns the directory of the current session, or "" if
// none.
func (r *Router) RecordingDir() string {
	if s := r.session.Load(); s != nil {
		return s.Dir()
	}
	return ""
}

// RecordingBase returns the configured base directory (Options.RecordDir),
// or "" if the router was started without one.
func (r *Router) RecordingBase() string { return r.opts.RecordDir }

// Wait blocks until the router stops (e.g. ctx cancelled or child died).
func (r *Router) Wait() {
	r.wg.Wait()
}

// txReader pulls TX-side PCM bytes from this port's TNC and fans
// each BlockBytes-sized chunk out to every linked destination's queue.
//
// TNCs burst TX audio (no real-time pacing on their end), so we must
// preserve the order and quantity of blocks rather than collapsing to
// "latest block" — otherwise the receiver hears only a fragment of the
// transmission. Source is io.Reader regardless of backend (samoyed
// UDP datagrams stitched into a byte stream, or direwolf FIFO bytes).
//
// The loop exits when src.Read returns an error — typically because
// Stop() closed the underlying socket / file at shutdown.
func (r *Router) txReader(ctx context.Context, ref config.PortRef, c *tnc.Child) {
	src := c.TXAudio()
	buf := make([]byte, 4096)
	pending := make([]byte, 0, audio.BlockBytes*4)
	outgoing := r.linkQueues[ref]   // empty slice if this port has no outgoing links
	tt := r.txTrackers[ref]         // may be nil only if Start partially failed

	for {
		n, err := src.Read(buf)
		if err != nil {
			// EOF / closed-during-shutdown is the normal exit path.
			if ctx.Err() != nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return
			}
			r.logger.Debug("tx audio read error", "port", ref, "err", err)
			return
		}
		if n == 0 {
			continue
		}
		pending = append(pending, buf[:n]...)
		for len(pending) >= audio.BlockBytes {
			blk := make(audio.Block, audio.BlockBytes)
			copy(blk, pending[:audio.BlockBytes])
			pending = pending[audio.BlockBytes:]
			peak := blk.PeakAbs()
			if r.opts.Verbose {
				r.logger.Debug("tx block", "port", ref, "peak", peak, "outgoing", len(outgoing))
			}
			// Bump the TX tracker on every active block; the watchdog
			// goroutine emits tx_start (first bump) and tx_end (after
			// txSilenceWindow with no further bumps).
			if r.opts.EventBus != nil && tt != nil && peak >= txActiveThreshold {
				tt.lastBusyNanos.Store(time.Now().UnixNano())
				if !tt.active.Swap(true) {
					r.opts.EventBus.Publish(events.Event{
						T:    time.Now(),
						Type: events.TXStart,
						Port: ref.String(),
						Peak: peak,
					})
				}
			}
			if s := r.session.Load(); s != nil {
				s.WriteTX(ref, blk)
			}
			if r.opts.AudioTap != nil {
				key := ref.String() + "|tx"
				if r.opts.AudioTap.HasSubscribers(key) {
					r.opts.AudioTap.Publish(key, blk)
				}
			}
			for _, q := range outgoing {
				q.pushNonBlocking(blk, r.logger)
			}
		}
	}
}

// txWatchdog fires tx_end after a port has been idle for txSilenceWindow.
// One goroutine per port; cheap because it sleeps almost all the time.
// The wall-clock timer is required because samoyed's TX UDP stream
// stops at end of burst — the txReader's Read blocks indefinitely with
// no further blocks to evaluate, so we can't drive silence detection
// from the audio path alone.
func (r *Router) txWatchdog(ctx context.Context, ref config.PortRef) {
	tt := r.txTrackers[ref]
	if tt == nil {
		return
	}
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	emitEnd := func() {
		// Also fires once during shutdown for any port that was keyed at
		// the moment ctx cancelled — visualiser then doesn't end with
		// stale "TX active" state.
		if tt.active.CompareAndSwap(true, false) && r.opts.EventBus != nil {
			r.opts.EventBus.Publish(events.Event{
				T:    time.Now(),
				Type: events.TXEnd,
				Port: ref.String(),
			})
		}
	}
	for {
		select {
		case <-ctx.Done():
			emitEnd()
			return
		case <-tick.C:
		}
		if !tt.active.Load() {
			continue
		}
		last := tt.lastBusyNanos.Load()
		if time.Since(time.Unix(0, last)) >= txSilenceWindow {
			emitEnd()
		}
	}
}

// rxFeeder writes one audio block per BlockSamples / SampleRate seconds to
// this port's samoyed stdin. The block is the mixer's verdict on every
// active TX reaching this port through the topology.
//
// "Active" simply means a block is available on that link's queue. Per
// PLAN Phase 3 self-mute, a port never hears its own TX — that's enforced
// by config validation rejecting self-loops, so it's a no-op here.
func (r *Router) rxFeeder(ctx context.Context, dst config.PortRef, stdin io.Writer) {
	period := time.Duration(audio.BlockSamples) * time.Second / audio.SampleRate
	ticker := time.NewTicker(period)
	defer ticker.Stop()

	links := r.rxLinks[dst] // links *into* this destination

	// Per-destination RX event state. We emit rx_decision only when the
	// (decision, source-set) tuple changes since the last block — busy
	// channels would otherwise generate ~100 events/s/port. lastSources is
	// kept sorted so equality comparison is straightforward.
	var (
		lastDecision audio.MixDecision = -1
		lastSources  string
		sourcePorts  []*linkQueue // scratch, reused
	)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		var active []audio.ActiveTX
		sourcePorts = sourcePorts[:0]
		for _, q := range links {
			b, ok := q.pop()
			if !ok {
				continue
			}
			active = append(active, audio.ActiveTX{
				Block:   b,
				LossDB:  q.loss,
				NoiseDB: q.noise,
			})
			sourcePorts = append(sourcePorts, q)
		}

		blk, dec := r.mixer.Mix(active)

		if r.opts.EventBus != nil {
			srcKey, srcList := summariseSources(sourcePorts)
			if dec != lastDecision || srcKey != lastSources {
				// Only emit when something interesting is happening
				// or when the channel just went quiet — both inform
				// the visualiser, but silent-→-silent transitions
				// (the common case) are skipped.
				if dec != audio.MixSilence || lastDecision != -1 {
					r.opts.EventBus.Publish(events.Event{
						T:        time.Now(),
						Type:     events.RXDecision,
						Port:     dst.String(),
						Decision: decisionName(dec),
						Sources:  srcList,
					})
				}
				lastDecision = dec
				lastSources = srcKey
			}
		}

		// Per-link noise summed in *after* the capture decision so noise
		// on a quieter link doesn't spuriously change which signal wins
		// capture. PLAN Phase 4.
		var maxNoise float64
		for _, tx := range active {
			if tx.NoiseDB > maxNoise {
				maxNoise = tx.NoiseDB
			}
		}
		if len(active) == 0 {
			for _, q := range links {
				if q.noise > maxNoise {
					maxNoise = q.noise
				}
			}
		}
		if maxNoise > 0 {
			r.mixer.AddNoise(blk, maxNoise)
		}

		if r.opts.Verbose && dec != audio.MixSilence {
			r.logger.Debug("rx mix",
				"port", dst, "decision", decisionName(dec),
				"sources", len(active),
				"peak", blk.PeakAbs())
		}

		if s := r.session.Load(); s != nil {
			s.WriteRX(dst, blk)
		}
		if r.opts.AudioTap != nil {
			key := dst.String() + "|rx"
			if r.opts.AudioTap.HasSubscribers(key) {
				r.opts.AudioTap.Publish(key, blk)
			}
		}

		if _, err := stdin.Write(blk); err != nil {
			if ctx.Err() == nil {
				r.logger.Debug("stdin write failed", "port", dst, "err", err)
			}
			return
		}
	}
}

func parsePortRef(s string) (config.PortRef, error) {
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			return config.PortRef{NodeID: s[:i], PortID: s[i+1:]}, nil
		}
	}
	return config.PortRef{}, fmt.Errorf("invalid port ref %q", s)
}

// summariseSources returns a stable key for the set of source ports
// feeding a destination this tick, plus the sorted list of "node.port"
// strings. The key is the join of the sorted list; cheap to compare for
// equality across ticks.
func summariseSources(qs []*linkQueue) (string, []string) {
	if len(qs) == 0 {
		return "", nil
	}
	list := make([]string, len(qs))
	for i, q := range qs {
		list[i] = q.src.String()
	}
	// Bubble sort is fine — typically 0–5 entries.
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j-1] > list[j]; j-- {
			list[j-1], list[j] = list[j], list[j-1]
		}
	}
	// Join without allocating a separator string per call.
	key := list[0]
	for i := 1; i < len(list); i++ {
		key += "|" + list[i]
	}
	return key, list
}

func decisionName(d audio.MixDecision) string {
	switch d {
	case audio.MixSilence:
		return "silence"
	case audio.MixSingle:
		return "single"
	case audio.MixCapture:
		return "capture"
	case audio.MixCollision:
		return "collision"
	case audio.MixSum:
		return "sum"
	}
	return "?"
}
