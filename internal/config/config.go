// Package config loads and validates the simulator's YAML topology file.
//
// The router's correctness depends on this file being strict — any unknown
// key is an error, every link must reference real ports, and every link's
// endpoints must agree on modem configuration. Mistakes that look like
// runtime bugs ("why isn't this frame decoding?") are easier to debug as
// startup errors with line numbers.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

// Mode is the user-facing modem type from the catalogue in PLAN.md.
type Mode string

const (
	ModeAFSK1200 Mode = "afsk1200"
	ModeGFSK9600 Mode = "gfsk9600"
	ModeBPSK     Mode = "bpsk"
	ModeIL2P     Mode = "il2p"
)

// Modem is the per-port modem configuration. It is opaque to the router
// except for cross-link compatibility validation; the router translates it
// into samoyed CLI flags via the table in NOTES-audio-io.md.
type Modem struct {
	Mode Mode `yaml:"mode"`

	// IL2P only.
	Inner Mode  `yaml:"inner,omitempty"`
	CRC   *bool `yaml:"crc,omitempty"`

	// BPSK only.
	Baud      int `yaml:"baud,omitempty"`
	CarrierHz int `yaml:"carrier_hz,omitempty"`
}

// Equivalent reports whether two modem configs describe the same on-the-air
// signal. Both ends of a link must be Equivalent or the link is invalid.
func (m Modem) Equivalent(o Modem) bool {
	if m.Mode != o.Mode {
		return false
	}
	switch m.Mode {
	case ModeIL2P:
		if m.Inner != o.Inner {
			return false
		}
		mc, oc := false, false
		if m.CRC != nil {
			mc = *m.CRC
		}
		if o.CRC != nil {
			oc = *o.CRC
		}
		return mc == oc
	case ModeBPSK:
		return m.Baud == o.Baud && m.CarrierHz == o.CarrierHz
	}
	return true
}

// Port is one TNC+radio combination — one samoyed child process.
type Port struct {
	ID       string `yaml:"id"`
	Modem    Modem  `yaml:"modem"`
	KissPort int    `yaml:"kiss_port"`
	Callsign string `yaml:"callsign,omitempty"`
}

// Node is a logical station — one or more ports under one identity.
type Node struct {
	ID       string `yaml:"id"`
	Callsign string `yaml:"callsign,omitempty"`
	Ports    []Port `yaml:"ports"`
}

// Link is a one-directional audio path between two ports.
type Link struct {
	From   string  `yaml:"from"`    // "<node_id>.<port_id>"
	To     string  `yaml:"to"`      // "<node_id>.<port_id>"
	LossDB float64 `yaml:"loss_db"` // attenuation, positive number = quieter

	// NoiseDB is the white-noise floor on this link, expressed as dB
	// below full-scale (positive = quieter; 0 = no noise). Phase 4.
	NoiseDB float64 `yaml:"noise_db,omitempty"`
}

// MixerMode selects the receiver-side mixing model.
type MixerMode string

const (
	MixerFMCapture MixerMode = "fm_capture" // default — see PLAN Phase 3
	MixerLinearSum MixerMode = "linear_sum" // SSB future, stub only
)

// CollisionMode selects what happens when two TX signals arrive at one
// receiver and neither captures the other (margin < CaptureDB).
type CollisionMode string

const (
	CollisionSilence CollisionMode = "silence" // v1 default
	CollisionSum     CollisionMode = "sum"     // stub
	CollisionNoise   CollisionMode = "noise"   // stub
)

// Config is the whole topology file.
type Config struct {
	MixerMode     MixerMode     `yaml:"mixer_mode,omitempty"`
	CaptureDB     float64       `yaml:"capture_db,omitempty"`
	CollisionMode CollisionMode `yaml:"collision_mode,omitempty"`

	Nodes []Node `yaml:"nodes"`
	Links []Link `yaml:"links"`
}

// Load reads and validates a YAML config file.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Parse(f)
}

// ParseBytes is a convenience over Parse for in-memory config bytes
// (used by sim-web when validating an edited config before saving).
func ParseBytes(b []byte) (*Config, error) {
	return Parse(bytes.NewReader(b))
}

// Parse reads YAML from r with strict decoding (unknown fields are errors)
// and runs cross-link validation.
func Parse(r io.Reader) (*Config, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)

	cfg := &Config{}
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("yaml: %w", err)
	}

	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.MixerMode == "" {
		c.MixerMode = MixerFMCapture
	}
	if c.CaptureDB == 0 {
		c.CaptureDB = 6.0
	}
	if c.CollisionMode == "" {
		c.CollisionMode = CollisionSilence
	}
}

// PortRef is a (node, port) handle resolved from a "node.port" string.
type PortRef struct {
	NodeID string
	PortID string
}

func (p PortRef) String() string { return p.NodeID + "." + p.PortID }

// Validate runs the cross-cutting checks that aren't expressible in the
// type system. It is called by Parse but exposed for tests.
func (c *Config) Validate() error {
	if len(c.Nodes) == 0 {
		return errors.New("config: no nodes defined")
	}
	switch c.MixerMode {
	case MixerFMCapture, MixerLinearSum:
	default:
		return fmt.Errorf("config: unknown mixer_mode %q", c.MixerMode)
	}
	switch c.CollisionMode {
	case CollisionSilence, CollisionSum, CollisionNoise:
	default:
		return fmt.Errorf("config: unknown collision_mode %q", c.CollisionMode)
	}
	if c.CaptureDB < 0 {
		return fmt.Errorf("config: capture_db must be >= 0, got %g", c.CaptureDB)
	}

	// Build a lookup table and check ID uniqueness.
	type slot struct {
		nodeIdx, portIdx int
	}
	portMap := map[PortRef]slot{}
	nodeIDs := map[string]bool{}
	usedKissPorts := map[int]string{}

	for ni, n := range c.Nodes {
		if n.ID == "" {
			return fmt.Errorf("config: node #%d has empty id", ni)
		}
		if nodeIDs[n.ID] {
			return fmt.Errorf("config: duplicate node id %q", n.ID)
		}
		nodeIDs[n.ID] = true
		if len(n.Ports) == 0 {
			return fmt.Errorf("config: node %q has no ports", n.ID)
		}
		seenPort := map[string]bool{}
		for pi, p := range n.Ports {
			if p.ID == "" {
				return fmt.Errorf("config: node %q port #%d has empty id", n.ID, pi)
			}
			if seenPort[p.ID] {
				return fmt.Errorf("config: node %q has duplicate port id %q", n.ID, p.ID)
			}
			seenPort[p.ID] = true
			if p.KissPort <= 0 || p.KissPort > 65535 {
				return fmt.Errorf("config: node %q port %q: kiss_port %d out of range", n.ID, p.ID, p.KissPort)
			}
			if existing, ok := usedKissPorts[p.KissPort]; ok {
				return fmt.Errorf("config: kiss_port %d used by both %s and %s.%s", p.KissPort, existing, n.ID, p.ID)
			}
			usedKissPorts[p.KissPort] = n.ID + "." + p.ID

			if err := validateModem(p.Modem); err != nil {
				return fmt.Errorf("config: %s.%s modem: %w", n.ID, p.ID, err)
			}
			portMap[PortRef{n.ID, p.ID}] = slot{ni, pi}
		}
	}

	// Validate links and their cross-modem compatibility.
	seenLink := map[string]bool{}
	for li, l := range c.Links {
		fromRef, err := parsePortRef(l.From)
		if err != nil {
			return fmt.Errorf("config: link #%d from %q: %w", li, l.From, err)
		}
		toRef, err := parsePortRef(l.To)
		if err != nil {
			return fmt.Errorf("config: link #%d to %q: %w", li, l.To, err)
		}
		if fromRef == toRef {
			return fmt.Errorf("config: link #%d is a self-loop on %s", li, fromRef)
		}
		fs, ok := portMap[fromRef]
		if !ok {
			return fmt.Errorf("config: link #%d from: unknown port %s", li, fromRef)
		}
		ts, ok := portMap[toRef]
		if !ok {
			return fmt.Errorf("config: link #%d to: unknown port %s", li, toRef)
		}
		fromModem := c.Nodes[fs.nodeIdx].Ports[fs.portIdx].Modem
		toModem := c.Nodes[ts.nodeIdx].Ports[ts.portIdx].Modem
		if !fromModem.Equivalent(toModem) {
			return fmt.Errorf("config: link %s -> %s: modem mismatch (%s vs %s)", fromRef, toRef, fromModem.Mode, toModem.Mode)
		}
		if l.LossDB < 0 {
			return fmt.Errorf("config: link %s -> %s: loss_db must be >= 0", fromRef, toRef)
		}
		key := fromRef.String() + "->" + toRef.String()
		if seenLink[key] {
			return fmt.Errorf("config: duplicate link %s", key)
		}
		seenLink[key] = true
	}

	return nil
}

func parsePortRef(s string) (PortRef, error) {
	for i, c := range s {
		if c == '.' {
			if i == 0 || i == len(s)-1 {
				return PortRef{}, errors.New("expected node.port")
			}
			return PortRef{NodeID: s[:i], PortID: s[i+1:]}, nil
		}
	}
	return PortRef{}, errors.New("expected node.port")
}

func validateModem(m Modem) error {
	switch m.Mode {
	case ModeAFSK1200:
		if m.Inner != "" || m.CRC != nil || m.Baud != 0 || m.CarrierHz != 0 {
			return errors.New("afsk1200 takes no extra params")
		}
	case ModeGFSK9600:
		if m.Inner != "" || m.CRC != nil || m.Baud != 0 || m.CarrierHz != 0 {
			return errors.New("gfsk9600 takes no extra params")
		}
	case ModeIL2P:
		if m.Inner == "" {
			return errors.New("il2p requires inner")
		}
		if m.Inner != ModeAFSK1200 && m.Inner != ModeGFSK9600 {
			return fmt.Errorf("il2p inner=%q not supported", m.Inner)
		}
		if m.CRC == nil {
			return errors.New("il2p requires crc (true|false)")
		}
		if m.Baud != 0 || m.CarrierHz != 0 {
			return errors.New("il2p takes only inner and crc")
		}
	case ModeBPSK:
		if m.Baud == 0 {
			return errors.New("bpsk requires baud")
		}
		if m.Inner != "" || m.CRC != nil {
			return errors.New("bpsk takes baud and optional carrier_hz")
		}
	case "":
		return errors.New("missing mode")
	default:
		return fmt.Errorf("unknown mode %q", m.Mode)
	}
	return nil
}
