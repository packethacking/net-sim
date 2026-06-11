package tnc

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/packethacking/net-sim/internal/config"
)

// KISS framing bytes.
const (
	kissFEND  = 0xC0
	kissFESC  = 0xDB
	kissTFEND = 0xDC
	kissTFESC = 0xDD
)

// xkissCmdData is the KISS ACKMODE command nibble (G8BPQ extended KISS).
const xkissCmdData = 0x0C

// TestSamoyedAckmodeRoundTrip drives a real samoyed child end to end over its
// KISS TCP port: it sends an ACKMODE data frame (command nibble 0x0C with two
// opaque id bytes) and asserts that, once the frame has actually been
// transmitted, samoyed echoes the command byte and the same two id bytes back.
//
// This is the feature net-sim depends on. It needs an ackmode-capable samoyed
// binary; set SAMOYED_BIN, or install it on PATH / at /opt/samoyed. The test
// skips (rather than fails) when no binary is available so the rest of the
// suite stays green in environments without samoyed built.
func TestSamoyedAckmodeRoundTrip(t *testing.T) {
	bin := findSamoyedBinary(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	spec := Spec{
		Backend:        BackendSamoyed,
		NodeID:         "ack",
		PortID:         "p0",
		Modem:          config.Modem{Mode: config.ModeAFSK1200},
		KissPort:       freeTCPPort(t),
		RxAudioUDPPort: freeUDPPort(t),
		SamoyedBin:     bin,
		WorkDir:        t.TempDir(),
	}

	child, err := Start(ctx, spec)
	if err != nil {
		t.Fatalf("start samoyed: %v", err)
	}
	defer child.Stop()

	// Drain the TX audio stream so nothing backs up while samoyed transmits.
	go func() { _, _ = io.Copy(io.Discard, child.TXAudio()) }()

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", spec.KissPort), 2*time.Second)
	if err != nil {
		t.Fatalf("dial kiss: %v", err)
	}
	defer conn.Close()

	wantID := [2]byte{0xAB, 0xCD}

	// ACKMODE data frame: command nibble 0x0C (channel 0), two id bytes, then
	// a raw AX.25 UI frame.
	payload := []byte{xkissCmdData, wantID[0], wantID[1]}
	payload = append(payload, ax25UIFrame("Q2TEST", "Q1TEST", "ackmode round-trip")...)

	if _, err := conn.Write(kissEncode(payload)); err != nil {
		t.Fatalf("write ackmode frame: %v", err)
	}

	// The acknowledgement must come back once the frame is on the air. Give it
	// plenty of time (TXDELAY + CSMA persist/slottime + render).
	ack := readKISSFrame(t, conn, 20*time.Second, func(d []byte) bool {
		return len(d) == 3 && d[0]&0x0F == xkissCmdData
	})

	if got := ack[0] & 0x0F; got != xkissCmdData {
		t.Errorf("ack command nibble = %#x, want %#x (0x0E would mean the buggy poll opcode)", got, xkissCmdData)
	}
	if ack[1] != wantID[0] || ack[2] != wantID[1] {
		t.Errorf("ack id bytes = % x, want % x", ack[1:3], wantID[:])
	}
}

// --- helpers ---------------------------------------------------------------

func findSamoyedBinary(t *testing.T) string {
	t.Helper()
	if b := os.Getenv("SAMOYED_BIN"); b != "" {
		return b
	}
	if p, err := exec.LookPath("samoyed-direwolf"); err == nil {
		return p
	}
	for _, p := range []string{
		"/opt/samoyed/dist/samoyed-direwolf",
		"/usr/local/bin/samoyed-direwolf",
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Skip("samoyed-direwolf binary not found (set SAMOYED_BIN); skipping ACKMODE integration test")
	return ""
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free tcp port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func freeUDPPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("free udp port: %v", err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

// kissEncode wraps payload in a KISS frame, applying SLIP-style escaping.
func kissEncode(payload []byte) []byte {
	out := []byte{kissFEND}
	for _, b := range payload {
		switch b {
		case kissFEND:
			out = append(out, kissFESC, kissTFEND)
		case kissFESC:
			out = append(out, kissFESC, kissTFESC)
		default:
			out = append(out, b)
		}
	}
	return append(out, kissFEND)
}

// readKISSFrame reads from conn until a decoded KISS frame satisfies match, or
// the timeout elapses (a fatal failure). Non-matching frames are logged and
// skipped.
func readKISSFrame(t *testing.T, conn net.Conn, timeout time.Duration, match func([]byte) bool) []byte {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))

	var acc []byte
	tmp := make([]byte, 4096)
	for {
		n, err := conn.Read(tmp)
		if n > 0 {
			acc = append(acc, tmp[:n]...)
			for {
				frame, rest, ok := nextKISSFrame(acc)
				if !ok {
					break
				}
				acc = rest
				if match(frame) {
					return frame
				}
				t.Logf("ignoring non-matching KISS frame from TNC: % x", frame)
			}
		}
		if err != nil {
			t.Fatalf("waiting for ACKMODE ack: %v (buffered: % x)", err, acc)
		}
	}
}

// nextKISSFrame extracts the first complete, de-escaped KISS frame from buf,
// returning it along with the unconsumed remainder. Empty frames (back-to-back
// FENDs) are skipped.
func nextKISSFrame(buf []byte) (frame, rest []byte, ok bool) {
	start := bytes.IndexByte(buf, kissFEND)
	if start < 0 {
		return nil, buf, false
	}
	end := bytes.IndexByte(buf[start+1:], kissFEND)
	if end < 0 {
		return nil, buf, false
	}
	end += start + 1

	raw := buf[start+1 : end]
	rest = buf[end:] // leave the closing FEND to open the next frame

	var dec []byte
	for k := 0; k < len(raw); k++ {
		if raw[k] == kissFESC && k+1 < len(raw) {
			k++
			switch raw[k] {
			case kissTFEND:
				dec = append(dec, kissFEND)
			case kissTFESC:
				dec = append(dec, kissFESC)
			default:
				dec = append(dec, raw[k])
			}
		} else {
			dec = append(dec, raw[k])
		}
	}
	if len(dec) == 0 {
		return nextKISSFrame(rest)
	}
	return dec, rest, true
}

// ax25UIFrame builds a raw (FCS-less) AX.25 UI frame: dest>src, control 0x03
// (UI), pid 0xF0 (no layer 3), then the info text.
func ax25UIFrame(dest, src, info string) []byte {
	f := encodeAX25Addr(dest, 0, true, false)             // destination: command bit set, not last
	f = append(f, encodeAX25Addr(src, 0, false, true)...) // source: response bit, last address
	f = append(f, 0x03, 0xF0)
	return append(f, []byte(info)...)
}

// encodeAX25Addr encodes one 7-byte AX.25 address field. cbit sets the
// command/response bit (bit 7); last sets the HDLC end-of-address bit (bit 0).
func encodeAX25Addr(call string, ssid byte, cbit, last bool) []byte {
	b := make([]byte, 7)
	for i := 0; i < 6; i++ {
		c := byte(' ')
		if i < len(call) {
			c = call[i]
		}
		b[i] = c << 1
	}
	ss := byte(0x60) | (ssid&0x0F)<<1 // reserved bits 6,5 set, SSID in bits 4-1
	if cbit {
		ss |= 0x80
	}
	if last {
		ss |= 0x01
	}
	b[6] = ss
	return b
}
