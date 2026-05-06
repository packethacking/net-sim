package audio

import (
	"math"
	"math/rand"
	"sort"
)

// MixDecision describes what the mixer chose to emit, for verbose logging.
type MixDecision int

const (
	MixSilence MixDecision = iota
	MixSingle
	MixCapture   // strongest signal captured demodulator
	MixCollision // two or more signals within capture margin → garbage
	MixSum       // linear-sum mode (SSB stub)
)

// ActiveTX is one transmitter currently audible at a receiver port.
type ActiveTX struct {
	Block   Block
	LossDB  float64 // attenuation 0..N, larger = quieter
	NoiseDB float64 // optional noise level for this link (Phase 4)
}

// Level returns the receiver-side dB level of the TX. The mixer compares
// these to decide capture: stronger = larger Level (less negative).
//
// We use −LossDB rather than measuring peak amplitude on the block, because
// real FM capture cares about RF power on the channel, not modulated audio
// peaks (which are roughly constant for a digital signal anyway).
func (a ActiveTX) Level() float64 { return -a.LossDB }

// Mixer is the receive-side capture-effect mixer (PLAN Phase 3).
//
// Pure linear-sum mixing is wrong for FM. Real receivers exhibit the
// capture effect: when two signals arrive simultaneously the stronger one
// takes over the demodulator and the weaker is suppressed, *provided* the
// level difference exceeds the capture ratio (≈6 dB on narrow-band 2m FM).
// The default config sets CaptureDB = 6.0.
//
// Mixer is per-port (one node's two ports have independent paths).
type Mixer struct {
	CaptureDB     float64
	CollisionMode string // "silence" | "sum" | "noise"
	LinearSum     bool   // true = SSB stub mode

	rng *rand.Rand
}

// NewMixer constructs a mixer. captureDB defaults to 6 if zero.
func NewMixer(captureDB float64, linearSum bool, collisionMode string) *Mixer {
	if captureDB == 0 {
		captureDB = 6
	}
	return &Mixer{
		CaptureDB:     captureDB,
		CollisionMode: collisionMode,
		LinearSum:     linearSum,
		rng:           rand.New(rand.NewSource(1)),
	}
}

// Mix produces one block of receive audio from the list of active TX
// streams reaching this port. Returns the audio plus a tag describing the
// decision for verbose logging.
//
//	0 active   → silence
//	1 active   → that stream attenuated
//	2+ active, margin >= captureDB → strongest, attenuated (capture)
//	2+ active, margin <  captureDB → "collision garbage" per CollisionMode
func (m *Mixer) Mix(active []ActiveTX) (Block, MixDecision) {
	if m.LinearSum {
		// SSB stub. v1 only needs FM path working.
		return m.linearSum(active), MixSum
	}
	switch len(active) {
	case 0:
		return Silence(), MixSilence
	case 1:
		amp := AmplitudeFromDB(active[0].LossDB)
		return active[0].Block.Scale(amp), MixSingle
	}
	// >= 2: sort by RX level (strongest first), check capture margin.
	sorted := make([]ActiveTX, len(active))
	copy(sorted, active)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Level() > sorted[j].Level()
	})
	margin := sorted[0].Level() - sorted[1].Level()
	if margin >= m.CaptureDB {
		amp := AmplitudeFromDB(sorted[0].LossDB)
		return sorted[0].Block.Scale(amp), MixCapture
	}
	return m.collision(sorted), MixCollision
}

func (m *Mixer) collision(_ []ActiveTX) Block {
	switch m.CollisionMode {
	case "sum", "noise":
		// stubs only — silence is the simplest defensible model and the
		// only branch v1 needs working
		return Silence()
	}
	return Silence()
}

func (m *Mixer) linearSum(active []ActiveTX) Block {
	if len(active) == 0 {
		return Silence()
	}
	out := make(Block, BlockBytes)
	// accumulate as int32 to avoid clipping mid-sum, then clamp to int16
	acc := make([]int32, BlockSamples)
	for _, tx := range active {
		amp := AmplitudeFromDB(tx.LossDB)
		for i := 0; i+1 < len(tx.Block) && i/2 < BlockSamples; i += 2 {
			s := int16(uint16(tx.Block[i]) | uint16(tx.Block[i+1])<<8)
			acc[i/2] += int32(float64(s) * amp)
		}
	}
	for i, v := range acc {
		if v > math.MaxInt16 {
			v = math.MaxInt16
		}
		if v < math.MinInt16 {
			v = math.MinInt16
		}
		s := int16(v)
		out[i*2] = byte(uint16(s))
		out[i*2+1] = byte(uint16(s) >> 8)
	}
	return out
}

// AddNoise mixes white noise at the given dB-level into the block in place.
//
// noiseDB is the noise level expressed as positive dB below full-scale,
// applied to the gaussian noise's standard deviation (RMS amplitude):
//
//	noiseDB=20 → sigma ≈ 3277  (gentle hiss, well below 1200-baud signal)
//	noiseDB=10 → sigma ≈ 10362 (audible hiss, frame errors start to creep in)
//	noiseDB=6  → sigma ≈ 16412 (deafening — channel barely usable)
//
// 0 (or negative) is treated as "no noise". v1 generates flat gaussian
// noise — see PLAN Phase 4 ("flat white noise is the right model for
// channel noise floor at this layer").
func (m *Mixer) AddNoise(b Block, noiseDB float64) {
	if noiseDB <= 0 {
		return
	}
	sigma := AmplitudeFromDB(noiseDB) * float64(math.MaxInt16)
	for i := 0; i+1 < len(b); i += 2 {
		s := int16(uint16(b[i]) | uint16(b[i+1])<<8)
		n := m.rng.NormFloat64() * sigma
		v := float64(s) + n
		if v > math.MaxInt16 {
			v = math.MaxInt16
		}
		if v < math.MinInt16 {
			v = math.MinInt16
		}
		s2 := int16(v)
		b[i] = byte(uint16(s2))
		b[i+1] = byte(uint16(s2) >> 8)
	}
}
