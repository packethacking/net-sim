package tnc

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/packethacking/net-sim/internal/config"
)

// samoyedConf renders the per-port samoyed direwolf.conf body.
//
// MODEM is included only for AFSK1200; non-1200 modes go on the CLI
// because samoyed's config-file MODEM directive doesn't activate
// non-1200 modes (upstream issue tracked separately).
func samoyedConf(s Spec) string {
	var b []byte
	b = appendf(b, "ADEVICE - udp:127.0.0.1:%d\n", s.RxAudioUDPPort)
	b = append(b, "ACHANNELS 1\n"...)
	b = append(b, "CHANNEL 0\n"...)
	b = appendf(b, "MYCALL %s\n", deriveCallsign(s.NodeID))
	if s.Modem.Mode == config.ModeAFSK1200 ||
		(s.Modem.Mode == config.ModeIL2P && s.Modem.Inner == config.ModeAFSK1200) {
		b = append(b, "MODEM 1200\n"...)
	}
	b = append(b, "KISSPORT 0\n"...)
	b = appendf(b, "KISSPORT %d\n", s.KissPort)
	b = append(b, "AGWPORT 0\n"...)
	b = append(b, "DNSSD 0\n"...)
	return string(b)
}

func startSamoyed(ctx context.Context, s Spec) (*Child, error) {
	if s.SamoyedBin == "" {
		s.SamoyedBin = "samoyed-direwolf"
	}
	if s.WorkDir == "" {
		s.WorkDir = os.TempDir()
	}
	if err := os.MkdirAll(s.WorkDir, 0o755); err != nil {
		return nil, fmt.Errorf("workdir: %w", err)
	}

	udpAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: s.RxAudioUDPPort}
	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, fmt.Errorf("listen udp %d: %w", s.RxAudioUDPPort, err)
	}

	confPath := filepath.Join(s.WorkDir, fmt.Sprintf("samoyed-%s-%s.conf", s.NodeID, s.PortID))
	if err := os.WriteFile(confPath, []byte(samoyedConf(s)), 0o644); err != nil {
		_ = udpConn.Close()
		return nil, fmt.Errorf("write conf: %w", err)
	}

	args := []string{"-c", confPath, "-t", "0", "-q", "dh"}
	args = append(args, modemArgs(s.Modem)...)

	cmd := exec.CommandContext(ctx, s.SamoyedBin, args...)
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
		spec:     s,
		cmd:      cmd,
		stdin:    stdin,
		txReader: &udpReader{conn: udpConn},
		exitC:    make(chan error, 1),
		cleanups: []func() error{
			func() error { return udpConn.Close() },
		},
	}
	go func() { c.exitC <- cmd.Wait() }()

	if err := waitListening(ctx, s.KissPort, 5*time.Second); err != nil {
		_ = c.Stop()
		return nil, fmt.Errorf("wait for kiss tcp %d: %w", s.KissPort, err)
	}
	return c, nil
}

// udpReader adapts a UDPConn to io.Reader by concatenating successive
// datagram payloads into a byte stream. The router slices that stream
// into BlockBytes blocks itself; samoyed's UDP_AUDIO_OUT_BUF_MAXLEN
// (1472) doesn't align with our 882-byte blocks, so we have to splice.
type udpReader struct {
	conn *net.UDPConn
	buf  []byte
	pos  int
}

func (u *udpReader) Read(p []byte) (int, error) {
	if u.pos >= len(u.buf) {
		// Need a fresh datagram.
		tmp := make([]byte, 4096)
		n, _, err := u.conn.ReadFromUDP(tmp)
		if err != nil {
			return 0, err
		}
		u.buf = tmp[:n]
		u.pos = 0
	}
	n := copy(p, u.buf[u.pos:])
	u.pos += n
	return n, nil
}

func (u *udpReader) Close() error { return u.conn.Close() }

// SetReadDeadline forwards the deadline to the underlying UDP socket so
// the router's txReader can apply read timeouts.
func (u *udpReader) SetReadDeadline(t time.Time) error {
	return u.conn.SetReadDeadline(t)
}

func appendf(b []byte, f string, a ...any) []byte {
	return append(b, []byte(fmt.Sprintf(f, a...))...)
}
