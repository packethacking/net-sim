// Package tnc spawns and supervises one TNC child process per simulated
// port. Two backends are supported:
//
//   - "samoyed" (default): https://github.com/doismellburning/samoyed,
//     a Go port of Dire Wolf. Audio in via stdin, audio out via UDP
//     datagrams.
//   - "direwolf": stock Dire Wolf (apt install direwolf). Audio in via
//     stdin, audio out via an ALSA "file" plugin writing to a FIFO
//     because stock Dire Wolf 1.8 still doesn't support UDP audio out.
//
// The router treats each child as a black box exposing:
//   - a stdin Writer where the router pushes RX audio (PCM samples)
//   - a TXAudio io.Reader where the child's TX PCM bytes appear
//   - a TCP KISS port (managed by the child itself)
package tnc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"

	"github.com/packethacking/net-sim/internal/config"
)

// SampleRate is the audio sample rate both backends default to (and we
// follow suit). See NOTES-audio-io.md.
const SampleRate = 44100

// BytesPerSample for mono signed-16-bit PCM.
const BytesPerSample = 2

// Backend names a TNC implementation.
type Backend string

const (
	BackendSamoyed  Backend = "samoyed"
	BackendDirewolf Backend = "direwolf"
)

// Spec is everything a Child needs to launch.
type Spec struct {
	Backend        Backend // "samoyed" (default) or "direwolf"
	NodeID, PortID string
	Modem          config.Modem
	KissPort       int

	// RxAudioUDPPort is the local UDP port the *samoyed* backend sends
	// TX audio to (router listens here). Ignored by direwolf.
	RxAudioUDPPort int

	SamoyedBin  string // path to samoyed-direwolf
	DirewolfBin string // path to direwolf
	PreloadPath string // libpa_stub.so for LD_PRELOAD (samoyed only)
	WorkDir     string // scratch dir for per-port config files (and FIFOs, for direwolf)
}

// Child is one running TNC process exposing stdin/TX-audio/KISS to the
// router. The internal layout differs per backend; the interface is
// uniform.
type Child struct {
	spec     Spec
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	txReader io.ReadCloser

	// cleanups run in Stop, in reverse order. Backends register their
	// own (close UDP listener, close + remove FIFO, etc.).
	cleanups []func() error

	mu    sync.Mutex
	exitC chan error
}

// Stdin returns the writer carrying RX audio into the TNC.
func (c *Child) Stdin() io.Writer { return c.stdin }

// TXAudio returns a reader producing the TX-side PCM stream from the
// TNC. samoyed datagrams are concatenated into a byte stream; direwolf
// bytes flow directly from the FIFO.
func (c *Child) TXAudio() io.Reader { return c.txReader }

// Spec returns the launch spec.
func (c *Child) Spec() Spec { return c.spec }

// Wait blocks until the child exits.
func (c *Child) Wait() error { return <-c.exitC }

// Done returns a channel that is closed when the child exits.
func (c *Child) Done() <-chan error { return c.exitC }

// Stop kills the child and runs registered cleanups. Idempotent.
func (c *Child) Stop() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.stdin.Close()
		_ = c.txReader.Close()
		if err := c.cmd.Process.Kill(); err != nil && !errors.Is(err, exec.ErrNotFound) {
			// keep going — cleanups still need to run
		}
	}
	for i := len(c.cleanups) - 1; i >= 0; i-- {
		_ = c.cleanups[i]()
	}
	c.cleanups = nil
	return nil
}

// Start launches a child using the configured backend. Defaults to
// samoyed if Backend is empty.
func Start(ctx context.Context, s Spec) (*Child, error) {
	if err := SupportedMode(s.Modem); err != nil {
		return nil, err
	}
	switch s.Backend {
	case "", BackendSamoyed:
		return startSamoyed(ctx, s)
	case BackendDirewolf:
		return startDirewolf(ctx, s)
	default:
		return nil, fmt.Errorf("unknown tnc backend %q", s.Backend)
	}
}

// SupportedMode reports whether the modem mode can run on the TNC.
// Both backends share the same modem flags, so the answer is the same
// for samoyed and direwolf.
func SupportedMode(m config.Modem) error {
	switch m.Mode {
	case config.ModeAFSK1200, config.ModeGFSK9600:
		return nil
	case config.ModeIL2P:
		switch m.Inner {
		case config.ModeAFSK1200, config.ModeGFSK9600:
			return nil
		default:
			return fmt.Errorf("il2p inner=%q not supported", m.Inner)
		}
	case config.ModeBPSK:
		return errors.New("bpsk not yet supported by either samoyed or direwolf")
	}
	return fmt.Errorf("unknown mode %q", m.Mode)
}

// modemArgs translates a modem spec into TNC CLI flags. Both samoyed
// and direwolf accept the same -B/-g/-I flags, so this is shared.
// Translation table also lives in NOTES-audio-io.md.
func modemArgs(m config.Modem) []string {
	switch m.Mode {
	case config.ModeAFSK1200:
		return nil
	case config.ModeGFSK9600:
		return []string{"-B", "9600", "-g"}
	case config.ModeIL2P:
		var out []string
		if m.Inner == config.ModeGFSK9600 {
			out = append(out, "-B", "9600", "-g")
		}
		il2p := "0"
		if m.FEC == config.FECStrong {
			il2p = "1"
		}
		out = append(out, "-I", il2p)
		return out
	}
	return nil
}
