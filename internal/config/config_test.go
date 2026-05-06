package config

import (
	"strings"
	"testing"
)

func TestStrictUnknownField(t *testing.T) {
	yaml := `
nodes:
  - id: a
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8001
        bogus: yes
links: []
`
	_, err := Parse(strings.NewReader(yaml))
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("expected unknown-field error, got %v", err)
	}
}

func TestModemMismatch(t *testing.T) {
	yaml := `
nodes:
  - id: a
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8001
  - id: b
    ports:
      - id: vhf
        modem: { mode: gfsk9600 }
        kiss_port: 8002
links:
  - from: a.vhf
    to:   b.vhf
`
	_, err := Parse(strings.NewReader(yaml))
	if err == nil || !strings.Contains(err.Error(), "modem mismatch") {
		t.Fatalf("expected modem mismatch error, got %v", err)
	}
}

func TestUnknownPort(t *testing.T) {
	yaml := `
nodes:
  - id: a
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8001
links:
  - from: a.vhf
    to:   ghost.vhf
`
	_, err := Parse(strings.NewReader(yaml))
	if err == nil || !strings.Contains(err.Error(), "unknown port") {
		t.Fatalf("expected unknown port error, got %v", err)
	}
}

func TestKissPortCollision(t *testing.T) {
	yaml := `
nodes:
  - id: a
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8001
  - id: b
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8001
`
	_, err := Parse(strings.NewReader(yaml))
	if err == nil || !strings.Contains(err.Error(), "kiss_port 8001 used by both") {
		t.Fatalf("expected kiss_port collision error, got %v", err)
	}
}

func TestIL2PRequiresInnerAndFEC(t *testing.T) {
	yaml := `
nodes:
  - id: a
    ports:
      - id: vhf
        modem: { mode: il2p, inner: afsk1200 }
        kiss_port: 8001
`
	_, err := Parse(strings.NewReader(yaml))
	if err == nil || !strings.Contains(err.Error(), "fec") {
		t.Fatalf("expected fec error, got %v", err)
	}
}

func TestIL2PRejectsBadFEC(t *testing.T) {
	yaml := `
nodes:
  - id: a
    ports:
      - id: vhf
        modem: { mode: il2p, inner: afsk1200, fec: medium }
        kiss_port: 8001
`
	_, err := Parse(strings.NewReader(yaml))
	if err == nil || !strings.Contains(err.Error(), "fec=") {
		t.Fatalf("expected fec value error, got %v", err)
	}
}

func TestIL2PRejectsLegacyCRC(t *testing.T) {
	yaml := `
nodes:
  - id: a
    ports:
      - id: vhf
        modem: { mode: il2p, inner: afsk1200, crc: true }
        kiss_port: 8001
`
	_, err := Parse(strings.NewReader(yaml))
	if err == nil || !strings.Contains(err.Error(), "crc") {
		t.Fatalf("expected unknown-field error mentioning crc, got %v", err)
	}
}

func TestValidConfig(t *testing.T) {
	yaml := `
mixer_mode: fm_capture
capture_db: 6.0
collision_mode: silence
nodes:
  - id: rdg
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8001
  - id: bsg
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8002
links:
  - from: rdg.vhf
    to:   bsg.vhf
    loss_db: 0
  - from: bsg.vhf
    to:   rdg.vhf
    loss_db: 0
`
	cfg, err := Parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	if cfg.CaptureDB != 6.0 {
		t.Errorf("capture_db = %g, want 6.0", cfg.CaptureDB)
	}
	if len(cfg.Links) != 2 {
		t.Errorf("links = %d, want 2", len(cfg.Links))
	}
}

func TestSelfLoopRejected(t *testing.T) {
	yaml := `
nodes:
  - id: a
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8001
links:
  - from: a.vhf
    to:   a.vhf
`
	_, err := Parse(strings.NewReader(yaml))
	if err == nil || !strings.Contains(err.Error(), "self-loop") {
		t.Fatalf("expected self-loop error, got %v", err)
	}
}

func TestMultiportNodeOK(t *testing.T) {
	yaml := `
nodes:
  - id: hub
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8001
      - id: uhf
        modem: { mode: afsk1200 }
        kiss_port: 8002
links: []
`
	if _, err := Parse(strings.NewReader(yaml)); err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
}
