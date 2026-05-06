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
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/packethacking/net-sim/internal/audio"
	"github.com/packethacking/net-sim/internal/config"
	"github.com/packethacking/net-sim/internal/samoyed"
)

// Options configures a Router. All paths default to "look up on $PATH".
type Options struct {
	BinaryPath          string // path to samoyed-direwolf
	PreloadPath         string // path to libpa_stub.so
	WorkDir             string // where temporary direwolf.conf files go
	Verbose             bool   // log every routing decision
	Logger              *slog.Logger
	StartingRxAudioPort int // first ephemeral UDP port for samoyed-→-router audio
}

// Router is the running simulator.
type Router struct {
	opts   Options
	cfg    *config.Config
	mixer  *audio.Mixer
	logger *slog.Logger

	mu       sync.RWMutex
	children map[config.PortRef]*samoyed.Child

	// linkQueues hold per-link audio: when source S transmits, blocks are
	// pushed into linkQueues[S][index] for every link S→D. The destination
	// D's rxFeeder drains the corresponding queue at sample rate.
	//
	// A separate queue per link (rather than one per source) means each
	// destination gets its own back-pressure / drop behaviour and we don't
	// need to track per-destination read positions on a shared queue.
	linkQueues map[config.PortRef][]*linkQueue // keyed by source
	rxLinks    map[config.PortRef][]*linkQueue // keyed by destination

	cancel context.CancelFunc
	wg     sync.WaitGroup
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
			if err := samoyed.SupportedMode(p.Modem); err != nil {
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
		children:   map[config.PortRef]*samoyed.Child{},
		linkQueues: map[config.PortRef][]*linkQueue{},
		rxLinks:    map[config.PortRef][]*linkQueue{},
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
			callsign := p.Callsign
			if callsign == "" {
				callsign = n.Callsign
			}
			if callsign == "" {
				callsign = "N0CALL"
			}
			spec := samoyed.Spec{
				NodeID:         n.ID,
				PortID:         p.ID,
				Callsign:       callsign,
				Modem:          p.Modem,
				KissPort:       p.KissPort,
				RxAudioUDPPort: udpPort,
				BinaryPath:     opts.BinaryPath,
				PreloadPath:    opts.PreloadPath,
				WorkDir:        opts.WorkDir,
			}
			udpPort++

			child, err := samoyed.Start(rctx, spec)
			if err != nil {
				_ = r.shutdown()
				return nil, fmt.Errorf("start %s: %w", ref, err)
			}
			r.children[ref] = child

			r.logger.Info("port up",
				"node", n.ID, "port", p.ID,
				"mode", p.Modem.Mode, "kiss_tcp", p.KissPort,
				"rx_audio_udp", spec.RxAudioUDPPort)
		}
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
	return nil
}

// Wait blocks until the router stops (e.g. ctx cancelled or child died).
func (r *Router) Wait() {
	r.wg.Wait()
}

// txReader pulls TX-side audio from this port's samoyed via UDP and fans
// each block out to every linked destination's per-link queue.
//
// samoyed bursts TX audio (no real-time pacing on its end), so we must
// preserve the order and quantity of blocks rather than collapsing to
// "latest block" — otherwise the receiver hears just a fragment of the
// transmission.
func (r *Router) txReader(ctx context.Context, ref config.PortRef, c *samoyed.Child) {
	udp := c.UDPConn()
	buf := make([]byte, 4096)
	pending := make([]byte, 0, audio.BlockBytes*4)
	outgoing := r.linkQueues[ref] // empty slice if this port has no outgoing links

	for {
		_ = udp.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
		n, _, err := udp.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				if ctx.Err() != nil {
					return
				}
				continue
			}
			if ctx.Err() != nil {
				return
			}
			r.logger.Debug("udp read error", "port", ref, "err", err)
			return
		}
		pending = append(pending, buf[:n]...)
		for len(pending) >= audio.BlockBytes {
			blk := make(audio.Block, audio.BlockBytes)
			copy(blk, pending[:audio.BlockBytes])
			pending = pending[audio.BlockBytes:]
			if r.opts.Verbose {
				r.logger.Debug("tx block", "port", ref, "peak", blk.PeakAbs(), "outgoing", len(outgoing))
			}
			for _, q := range outgoing {
				q.pushNonBlocking(blk, r.logger)
			}
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

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		var active []audio.ActiveTX
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
		}

		blk, dec := r.mixer.Mix(active)

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
