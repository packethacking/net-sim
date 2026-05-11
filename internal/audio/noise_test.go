package audio

import (
	"math"
	"sync"
	"testing"
)

// TestAddNoiseConcurrent is a regression test: every receiver goroutine
// in the router calls Mixer.AddNoise on the same *Mixer. Pre-fix the
// shared *rand.Rand was unprotected and panicked under load
// ("index out of range [-1]" inside math/rand). Race-detector enabled
// at CI catches the data race even without a panic.
func TestAddNoiseConcurrent(t *testing.T) {
	m := NewMixer(6, false, "silence")
	const goroutines = 16
	const iterations = 200
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				blk := Silence()
				m.AddNoise(blk, 18)
			}
		}()
	}
	wg.Wait()
}

// rms computes root-mean-square sample value (0..32768).
func rms(b Block) float64 {
	var sum float64
	n := 0
	for i := 0; i+1 < len(b); i += 2 {
		s := int16(uint16(b[i]) | uint16(b[i+1])<<8)
		sum += float64(s) * float64(s)
		n++
	}
	if n == 0 {
		return 0
	}
	return math.Sqrt(sum / float64(n))
}

func TestAddNoiseLevels(t *testing.T) {
	m := NewMixer(6, false, "silence")
	cases := []struct {
		noiseDB    float64
		wantSigma  float64
		tolerance  float64
	}{
		{20, 3277, 500},   // -20 dBFS hiss
		{10, 10362, 1500}, // -10 dBFS
		{6, 16412, 2500},  // -6 dBFS, deafening
	}
	for _, c := range cases {
		b := Silence()
		m.AddNoise(b, c.noiseDB)
		got := rms(b)
		if math.Abs(got-c.wantSigma) > c.tolerance {
			t.Errorf("noise_db=%g: rms=%g, want ~%g (±%g)", c.noiseDB, got, c.wantSigma, c.tolerance)
		}
	}
}

func TestAddNoiseZeroIsNop(t *testing.T) {
	m := NewMixer(6, false, "silence")
	for _, nd := range []float64{0, -1, -100} {
		b := Silence()
		m.AddNoise(b, nd)
		if !b.IsSilence() {
			t.Errorf("noise_db=%g: expected silence to remain silent", nd)
		}
	}
}
