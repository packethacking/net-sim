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

func TestDuplicateProfileName(t *testing.T) {
	yaml := `
radio_profiles:
  - name: slow
    tx_to_rx_ms: 100
  - name: slow
    tx_to_rx_ms: 200
nodes:
  - id: a
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8001
links: []
`
	_, err := Parse(strings.NewReader(yaml))
	if err == nil || !strings.Contains(err.Error(), "duplicate radio_profile") {
		t.Fatalf("expected duplicate profile error, got %v", err)
	}
}

func TestUnknownProfileRef(t *testing.T) {
	yaml := `
nodes:
  - id: a
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8001
        profile: nonexistent
links: []
`
	_, err := Parse(strings.NewReader(yaml))
	if err == nil || !strings.Contains(err.Error(), "unknown profile") {
		t.Fatalf("expected unknown profile error, got %v", err)
	}
}

func TestNegativeTurnaround(t *testing.T) {
	yaml := `
nodes:
  - id: a
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8001
        tx_to_rx_ms: -10
links: []
`
	_, err := Parse(strings.NewReader(yaml))
	if err == nil || !strings.Contains(err.Error(), "tx_to_rx_ms must be >= 0") {
		t.Fatalf("expected negative turnaround error, got %v", err)
	}
}

func TestNegativeRxToTxOnPort(t *testing.T) {
	yaml := `
nodes:
  - id: a
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8001
        rx_to_tx_ms: -5
links: []
`
	_, err := Parse(strings.NewReader(yaml))
	if err == nil || !strings.Contains(err.Error(), "rx_to_tx_ms must be >= 0") {
		t.Fatalf("expected negative turnaround error, got %v", err)
	}
}

func TestProfileOverride(t *testing.T) {
	yaml := `
radio_profiles:
  - name: slow
    tx_to_rx_ms: 100
    rx_to_tx_ms: 50
nodes:
  - id: a
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8001
        profile: slow
        tx_to_rx_ms: 200
links: []
`
	cfg, err := Parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	ta := cfg.ResolvedTurnaround(PortRef{NodeID: "a", PortID: "vhf"})
	if ta.TxToRxMs != 200 {
		t.Errorf("TxToRxMs = %d, want 200 (port override)", ta.TxToRxMs)
	}
	if ta.RxToTxMs != 50 {
		t.Errorf("RxToTxMs = %d, want 50 (from profile)", ta.RxToTxMs)
	}
}

func TestBuiltinProfileRef(t *testing.T) {
	yaml := `
nodes:
  - id: a
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8001
        profile: baofeng-uv5r
links: []
`
	cfg, err := Parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	ta := cfg.ResolvedTurnaround(PortRef{NodeID: "a", PortID: "vhf"})
	if ta.TxToRxMs != 300 {
		t.Errorf("TxToRxMs = %d, want 300 (baofeng-uv5r)", ta.TxToRxMs)
	}
	if ta.RxToTxMs != 150 {
		t.Errorf("RxToTxMs = %d, want 150 (baofeng-uv5r)", ta.RxToTxMs)
	}
}

func TestNoProfileNoTurnaround(t *testing.T) {
	yaml := `
nodes:
  - id: a
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8001
links: []
`
	cfg, err := Parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	ta := cfg.ResolvedTurnaround(PortRef{NodeID: "a", PortID: "vhf"})
	if ta.TxToRxMs != 0 || ta.RxToTxMs != 0 {
		t.Errorf("expected zero turnaround, got %+v", ta)
	}
}

func TestNegativeProfileTurnaround(t *testing.T) {
	yaml := `
radio_profiles:
  - name: bad
    tx_to_rx_ms: -5
nodes:
  - id: a
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8001
links: []
`
	_, err := Parse(strings.NewReader(yaml))
	if err == nil || !strings.Contains(err.Error(), "tx_to_rx_ms must be >= 0") {
		t.Fatalf("expected negative turnaround error, got %v", err)
	}
}

func TestNegativeProfileRxToTx(t *testing.T) {
	yaml := `
radio_profiles:
  - name: bad
    rx_to_tx_ms: -3
nodes:
  - id: a
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8001
links: []
`
	_, err := Parse(strings.NewReader(yaml))
	if err == nil || !strings.Contains(err.Error(), "rx_to_tx_ms must be >= 0") {
		t.Fatalf("expected negative turnaround error, got %v", err)
	}
}

func TestEmptyProfileName(t *testing.T) {
	yaml := `
radio_profiles:
  - tx_to_rx_ms: 100
nodes:
  - id: a
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8001
links: []
`
	_, err := Parse(strings.NewReader(yaml))
	if err == nil || !strings.Contains(err.Error(), "empty name") {
		t.Fatalf("expected empty name error, got %v", err)
	}
}

func TestUserProfileOverridesBuiltin(t *testing.T) {
	yaml := `
radio_profiles:
  - name: baofeng-uv5r
    tx_to_rx_ms: 999
    rx_to_tx_ms: 888
nodes:
  - id: a
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8001
        profile: baofeng-uv5r
links: []
`
	cfg, err := Parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	ta := cfg.ResolvedTurnaround(PortRef{NodeID: "a", PortID: "vhf"})
	if ta.TxToRxMs != 999 {
		t.Errorf("TxToRxMs = %d, want 999 (user override of builtin)", ta.TxToRxMs)
	}
	if ta.RxToTxMs != 888 {
		t.Errorf("RxToTxMs = %d, want 888 (user override of builtin)", ta.RxToTxMs)
	}
}

func TestPortDirectTurnaroundNoProfile(t *testing.T) {
	yaml := `
nodes:
  - id: a
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8001
        tx_to_rx_ms: 42
        rx_to_tx_ms: 17
links: []
`
	cfg, err := Parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	ta := cfg.ResolvedTurnaround(PortRef{NodeID: "a", PortID: "vhf"})
	if ta.TxToRxMs != 42 {
		t.Errorf("TxToRxMs = %d, want 42", ta.TxToRxMs)
	}
	if ta.RxToTxMs != 17 {
		t.Errorf("RxToTxMs = %d, want 17", ta.RxToTxMs)
	}
}

func TestResolvedTurnaroundUnknownPort(t *testing.T) {
	cfg := &Config{
		Nodes: []Node{{ID: "a", Ports: []Port{{ID: "vhf"}}}},
	}
	ta := cfg.ResolvedTurnaround(PortRef{NodeID: "z", PortID: "uhf"})
	if ta.TxToRxMs != 0 || ta.RxToTxMs != 0 {
		t.Errorf("unknown port should give zero turnaround, got %+v", ta)
	}
}

func TestAllBuiltinProfilesExist(t *testing.T) {
	expected := []string{"ideal", "kenwood-th-d74", "yaesu-ftm-400", "baofeng-uv5r", "generic-ht"}
	profiles := BuiltinProfiles()
	for _, name := range expected {
		p, ok := profiles[name]
		if !ok {
			t.Errorf("built-in profile %q missing", name)
			continue
		}
		if p.Name != name {
			t.Errorf("profile %q has Name=%q", name, p.Name)
		}
		if p.TxToRxMs < 0 || p.RxToTxMs < 0 {
			t.Errorf("profile %q has negative turnaround: tx=%d rx=%d", name, p.TxToRxMs, p.RxToTxMs)
		}
	}
}

func TestValidConfigWithProfilesAndLinks(t *testing.T) {
	yaml := `
radio_profiles:
  - name: custom
    tx_to_rx_ms: 60
    rx_to_tx_ms: 25
    noise_db: 18
nodes:
  - id: a
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8001
        profile: custom
  - id: b
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8002
        profile: generic-ht
links:
  - { from: a.vhf, to: b.vhf, loss_db: 6 }
  - { from: b.vhf, to: a.vhf, loss_db: 6 }
`
	cfg, err := Parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	if len(cfg.RadioProfiles) != 1 {
		t.Errorf("radio_profiles = %d, want 1", len(cfg.RadioProfiles))
	}
	taA := cfg.ResolvedTurnaround(PortRef{NodeID: "a", PortID: "vhf"})
	if taA.TxToRxMs != 60 || taA.RxToTxMs != 25 {
		t.Errorf("node a turnaround = %+v, want {60 25}", taA)
	}
	taB := cfg.ResolvedTurnaround(PortRef{NodeID: "b", PortID: "vhf"})
	if taB.TxToRxMs != 150 || taB.RxToTxMs != 80 {
		t.Errorf("node b turnaround = %+v, want {150 80} (generic-ht)", taB)
	}
}

func TestProfileNoiseDBResolution(t *testing.T) {
	cfg := &Config{
		RadioProfiles: []RadioProfile{
			{Name: "noisy", NoiseDB: 25},
		},
		Nodes: []Node{
			{ID: "a", Ports: []Port{
				{ID: "vhf", Profile: "noisy"},
				{ID: "uhf", Profile: "noisy", NoiseDB: 30},
				{ID: "hf"},
			}},
		},
		DefaultNoiseDB: 10,
	}
	p := cfg.ResolveProfile("noisy")
	if p.NoiseDB != 25 {
		t.Errorf("profile noise = %g, want 25", p.NoiseDB)
	}
	// Port-level NoiseDB should still be inspectable by the router;
	// the profile just provides the value when port NoiseDB is unset.
	// (The actual portNoiseFloor resolution lives in the router.)
	if cfg.Nodes[0].Ports[0].NoiseDB != 0 {
		t.Errorf("port vhf NoiseDB = %g, want 0 (inherits from profile)", cfg.Nodes[0].Ports[0].NoiseDB)
	}
	if cfg.Nodes[0].Ports[1].NoiseDB != 30 {
		t.Errorf("port uhf NoiseDB = %g, want 30 (port override)", cfg.Nodes[0].Ports[1].NoiseDB)
	}
}
