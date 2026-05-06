package tnc

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/packethacking/net-sim/internal/config"
)

// direwolfConf renders the per-port direwolf.conf body. The ADEVICE
// output is an ALSA pcm name that we define in a per-port .asoundrc:
// `pcm.dwout` is type=file pointing at a FIFO the router reads.
//
// Stock Dire Wolf 1.8 has no UDP audio output, so we tunnel TX audio
// through ALSA's `file` plugin into a named pipe. samoyed's UDP path
// is more direct; expect to rip this out once Dire Wolf gains UDP out.
func direwolfConf(s Spec) string {
	var b []byte
	b = appendf(b, "ADEVICE - %s\n", direwolfPCMName)
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
	return string(b)
}

const direwolfPCMName = "dwout"

func direwolfASoundRC(fifoPath string) string {
	return fmt.Sprintf(`pcm.%s {
  type file
  slave.pcm null
  file "%s"
  format raw
}
`, direwolfPCMName, fifoPath)
}

func startDirewolf(ctx context.Context, s Spec) (*Child, error) {
	if s.DirewolfBin == "" {
		s.DirewolfBin = "direwolf"
	}
	if s.WorkDir == "" {
		s.WorkDir = os.TempDir()
	}

	// Per-port scratch dir holds: .asoundrc, direwolf.conf, tx.fifo.
	// We point HOME at this dir so libasound picks up *our* asoundrc
	// without having to splat a global /etc/asound.conf.
	portDir := filepath.Join(s.WorkDir, fmt.Sprintf("dw-%s-%s", s.NodeID, s.PortID))
	if err := os.MkdirAll(portDir, 0o755); err != nil {
		return nil, fmt.Errorf("workdir: %w", err)
	}

	fifoPath := filepath.Join(portDir, "tx.fifo")
	_ = os.Remove(fifoPath) // idempotent
	if err := syscall.Mkfifo(fifoPath, 0o644); err != nil {
		return nil, fmt.Errorf("mkfifo %s: %w", fifoPath, err)
	}

	if err := os.WriteFile(filepath.Join(portDir, ".asoundrc"),
		[]byte(direwolfASoundRC(fifoPath)), 0o644); err != nil {
		_ = os.Remove(fifoPath)
		return nil, fmt.Errorf("write asoundrc: %w", err)
	}

	confPath := filepath.Join(portDir, "direwolf.conf")
	if err := os.WriteFile(confPath, []byte(direwolfConf(s)), 0o644); err != nil {
		_ = os.Remove(fifoPath)
		return nil, fmt.Errorf("write conf: %w", err)
	}

	// Open the FIFO RDWR so reading doesn't block waiting for a writer
	// (direwolf hasn't started yet) and a clean shutdown can close it
	// from our side regardless. POSIX leaves O_RDWR on FIFOs lightly
	// specified; on Linux it does what we want.
	fifo, err := os.OpenFile(fifoPath, os.O_RDWR, 0)
	if err != nil {
		_ = os.Remove(fifoPath)
		return nil, fmt.Errorf("open fifo: %w", err)
	}

	args := []string{"-c", confPath, "-t", "0", "-q", "dh"}
	args = append(args, modemArgs(s.Modem)...)

	cmd := exec.CommandContext(ctx, s.DirewolfBin, args...)
	cmd.Env = append(os.Environ(), "HOME="+portDir)
	cmd.Stdout = newPrefixWriter(os.Stderr, "["+s.NodeID+"."+s.PortID+"] ")
	cmd.Stderr = cmd.Stdout

	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = fifo.Close()
		_ = os.Remove(fifoPath)
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		_ = fifo.Close()
		_ = stdin.Close()
		_ = os.Remove(fifoPath)
		return nil, fmt.Errorf("start direwolf: %w", err)
	}

	c := &Child{
		spec:     s,
		cmd:      cmd,
		stdin:    stdin,
		txReader: fifo,
		exitC:    make(chan error, 1),
		cleanups: []func() error{
			func() error { return fifo.Close() },
			func() error { return os.Remove(fifoPath) },
		},
	}
	go func() { c.exitC <- cmd.Wait() }()

	if err := waitListening(ctx, s.KissPort, 5*time.Second); err != nil {
		_ = c.Stop()
		return nil, fmt.Errorf("wait for kiss tcp %d: %w", s.KissPort, err)
	}
	return c, nil
}
