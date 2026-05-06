// Package samoyed spawns and supervises one samoyed-direwolf child process
// per simulated port. It also owns the modem-mode → CLI-flags translation
// (the source of truth lives next to NOTES-audio-io.md).
//
// The router treats each child as a black box exposing:
//   - a stdin Writer where the router pushes RX audio (PCM samples)
//   - a UDP datagram source where the child pushes TX audio
//   - a TCP KISS port (managed by the child itself)
package samoyed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/packethacking/net-sim/internal/config"
)

// SampleRate is the audio sample rate samoyed defaults to (and we follow
// suit). See NOTES-audio-io.md.
const SampleRate = 44100

// BytesPerSample for mono signed-16-bit PCM.
const BytesPerSample = 2

// SamplesPerSecond reports the bytes/second of the canonical PCM stream.
func BytesPerSecond() int { return SampleRate * BytesPerSample }

// Spec is everything a Child needs to launch.
type Spec struct {
	NodeID, PortID string
	Modem          config.Modem
	KissPort       int
	RxAudioUDPPort int // local UDP port the child sends TX audio to (router listens here)

	BinaryPath  string // path to samoyed-direwolf
	PreloadPath string // path to libpa_stub.so (LD_PRELOAD)
	WorkDir     string // where the temporary direwolf.conf goes
}

// Child wraps one samoyed-direwolf process.
type Child struct {
	spec Spec

	cmd     *exec.Cmd
	stdin   io.WriteCloser // RX audio sink (router → samoyed)
	udpConn *net.UDPConn   // TX audio source (samoyed → router)

	mu    sync.Mutex
	exitC chan error
}

// SupportedMode reports whether the modem mode can run on this samoyed
// build. Anything that depends on samoyed features not yet available must
// fail at startup, not silently. See NOTES-audio-io.md for the catalogue.
func SupportedMode(m config.Modem) error {
	switch m.Mode {
	case config.ModeAFSK1200, config.ModeGFSK9600:
		return nil
	case config.ModeIL2P:
		// IL2P is a wrapper around an underlying modem.
		switch m.Inner {
		case config.ModeAFSK1200, config.ModeGFSK9600:
			return nil
		default:
			return fmt.Errorf("il2p inner=%q not supported by current samoyed", m.Inner)
		}
	case config.ModeBPSK:
		return errors.New("bpsk not yet supported by current samoyed")
	}
	return fmt.Errorf("unknown mode %q", m.Mode)
}

// Args translates a modem spec into samoyed-direwolf CLI flags. The
// translation table lives in NOTES-audio-io.md.
func Args(m config.Modem) []string {
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
		// strong (default-ish) → -I 1, weak → -I 0. samoyed's IL2P
		// codec doesn't implement the trailing-CRC variant; this is
		// FEC strength only, despite the original `crc:` field name.
		il2p := "0"
		if m.FEC == config.FECStrong {
			il2p = "1"
		}
		out = append(out, "-I", il2p)
		return out
	}
	return nil
}

// deriveCallsign turns a node id into a synthetic AX.25 callsign for
// MYCALL. samoyed wants something there but we never use it on the TX
// path (KISS frames carry their own source), so this is purely for
// log readability — `[a.vhf] N0A-1>APDW01:...` is easier to scan with
// multiple nodes than the same line saying `N0CALL`.
//
// Pads "N0" then takes the alphanumeric prefix of the node id, capping
// at 6 chars total per the AX.25 v2 callsign limit.
func deriveCallsign(nodeID string) string {
	var sb strings.Builder
	sb.WriteString("N0")
	for _, r := range strings.ToUpper(nodeID) {
		if sb.Len() >= 6 {
			break
		}
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
		}
	}
	if sb.Len() == 2 {
		// node id was empty / all-punctuation — fall back so MYCALL is valid
		return "N0SIM"
	}
	return sb.String()
}

// confLines builds the per-port samoyed config file body. The MODEM
// directive is included only for AFSK1200 — see NOTES-audio-io.md for why
// other modes are switched on via CLI flags instead.
func (s Spec) confLines() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "ADEVICE - udp:127.0.0.1:%d\n", s.RxAudioUDPPort)
	sb.WriteString("ACHANNELS 1\n")
	sb.WriteString("CHANNEL 0\n")
	fmt.Fprintf(&sb, "MYCALL %s\n", deriveCallsign(s.NodeID))
	if s.Modem.Mode == config.ModeAFSK1200 ||
		(s.Modem.Mode == config.ModeIL2P && s.Modem.Inner == config.ModeAFSK1200) {
		sb.WriteString("MODEM 1200\n")
	}
	sb.WriteString("KISSPORT 0\n")
	fmt.Fprintf(&sb, "KISSPORT %d\n", s.KissPort)
	sb.WriteString("AGWPORT 0\n")
	sb.WriteString("DNSSD 0\n")
	return sb.String()
}

// Start launches the child. Returns once the child has begun accepting
// stdin and the TX audio UDP socket is listening.
func Start(ctx context.Context, s Spec) (*Child, error) {
	if err := SupportedMode(s.Modem); err != nil {
		return nil, err
	}
	if s.BinaryPath == "" {
		s.BinaryPath = "samoyed-direwolf"
	}
	if s.WorkDir == "" {
		s.WorkDir = os.TempDir()
	}
	if err := os.MkdirAll(s.WorkDir, 0o755); err != nil {
		return nil, fmt.Errorf("workdir: %w", err)
	}

	// Bind the TX-audio UDP socket *before* spawning samoyed so it can't
	// race-write into the void.
	udpAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: s.RxAudioUDPPort}
	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, fmt.Errorf("listen udp %d: %w", s.RxAudioUDPPort, err)
	}

	confPath := filepath.Join(s.WorkDir, fmt.Sprintf("port-%s-%s.conf", s.NodeID, s.PortID))
	if err := os.WriteFile(confPath, []byte(s.confLines()), 0o644); err != nil {
		_ = udpConn.Close()
		return nil, fmt.Errorf("write conf: %w", err)
	}

	args := []string{"-c", confPath, "-t", "0", "-q", "dh"}
	args = append(args, Args(s.Modem)...)

	cmd := exec.CommandContext(ctx, s.BinaryPath, args...)
	if s.PreloadPath != "" {
		cmd.Env = append(os.Environ(), "LD_PRELOAD="+s.PreloadPath)
	}
	cmd.Stdout = newPrefixWriter(os.Stderr, "["+s.NodeID+"."+s.PortID+"] ")
	cmd.Stderr = cmd.Stdout

	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = udpConn.Close()
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		_ = udpConn.Close()
		_ = stdin.Close()
		return nil, fmt.Errorf("start samoyed: %w", err)
	}

	c := &Child{
		spec:    s,
		cmd:     cmd,
		stdin:   stdin,
		udpConn: udpConn,
		exitC:   make(chan error, 1),
	}

	go func() {
		c.exitC <- cmd.Wait()
	}()

	// Probe the KISS TCP port to know when samoyed is ready to accept
	// frames. Bound by ctx.
	if err := waitListening(ctx, s.KissPort, 5*time.Second); err != nil {
		_ = c.Stop()
		return nil, fmt.Errorf("wait for kiss tcp %d: %w", s.KissPort, err)
	}

	return c, nil
}

// Stdin returns the writer carrying RX audio into samoyed.
func (c *Child) Stdin() io.Writer { return c.stdin }

// UDPConn returns the listener receiving TX audio from samoyed.
func (c *Child) UDPConn() *net.UDPConn { return c.udpConn }

// Spec returns the launch spec.
func (c *Child) Spec() Spec { return c.spec }

// Wait blocks until the child exits.
func (c *Child) Wait() error { return <-c.exitC }

// Done returns a channel that is closed when the child exits.
func (c *Child) Done() <-chan error { return c.exitC }

// Stop kills the child. Idempotent.
func (c *Child) Stop() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd == nil || c.cmd.Process == nil {
		return nil
	}
	_ = c.stdin.Close()
	_ = c.udpConn.Close()
	if err := c.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

func waitListening(ctx context.Context, port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("port %d not listening after %s", port, timeout)
		}
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// prefixWriter writes each line with a "[label] " prefix. Useful so logs
// from N samoyed children are distinguishable on the joint stderr.
type prefixWriter struct {
	w      io.Writer
	prefix string
	buf    []byte
	mu     sync.Mutex
}

func newPrefixWriter(w io.Writer, prefix string) io.Writer {
	return &prefixWriter{w: w, prefix: prefix}
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf = append(p.buf, b...)
	for {
		i := indexByte(p.buf, '\n')
		if i < 0 {
			break
		}
		_, err := p.w.Write([]byte(p.prefix))
		if err != nil {
			return 0, err
		}
		_, err = p.w.Write(p.buf[:i+1])
		if err != nil {
			return 0, err
		}
		p.buf = p.buf[i+1:]
	}
	return len(b), nil
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// PortNumber is a tiny helper for the demo configs that use string ports.
func PortNumber(s string) (int, error) { return strconv.Atoi(s) }
