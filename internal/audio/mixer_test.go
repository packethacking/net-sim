package audio

import "testing"

func mkBlock(level int16) Block {
	b := make(Block, BlockBytes)
	for i := 0; i+1 < BlockBytes; i += 2 {
		b[i] = byte(uint16(level))
		b[i+1] = byte(uint16(level) >> 8)
	}
	return b
}

func TestMixSilence(t *testing.T) {
	m := NewMixer(6, false, "silence")
	out, dec := m.Mix(nil)
	if dec != MixSilence {
		t.Errorf("decision = %d, want MixSilence", dec)
	}
	if !out.IsSilence() {
		t.Error("output should be silence")
	}
}

func TestMixSingleAttenuated(t *testing.T) {
	m := NewMixer(6, false, "silence")
	src := mkBlock(10000)
	out, dec := m.Mix([]ActiveTX{{Block: src, LossDB: 6}})
	if dec != MixSingle {
		t.Errorf("decision = %d, want MixSingle", dec)
	}
	// 6 dB attenuation ≈ 0.5x, so peak should be ≈ 5000 (allow some rounding)
	peak := out.PeakAbs()
	if peak < 4900 || peak > 5100 {
		t.Errorf("peak = %d, want ~5000 (6 dB below 10000)", peak)
	}
}

func TestMixCaptureStrongerWins(t *testing.T) {
	m := NewMixer(6, false, "silence")
	strong := mkBlock(20000)
	weak := mkBlock(5000)
	// strong attenuated 0 dB, weak attenuated 12 dB → margin = 12 dB ≥ 6 dB
	out, dec := m.Mix([]ActiveTX{
		{Block: weak, LossDB: 12},
		{Block: strong, LossDB: 0},
	})
	if dec != MixCapture {
		t.Errorf("decision = %d, want MixCapture", dec)
	}
	// With LossDB=0, strong block should pass through unattenuated.
	if out.PeakAbs() < 19000 || out.PeakAbs() > 21000 {
		t.Errorf("peak = %d, want ~20000 (strong unattenuated)", out.PeakAbs())
	}
}

func TestMixCollisionSilenced(t *testing.T) {
	m := NewMixer(6, false, "silence")
	a := mkBlock(20000)
	b := mkBlock(20000)
	// Both at 0 dB → margin = 0 < 6 dB capture threshold → collision
	out, dec := m.Mix([]ActiveTX{
		{Block: a, LossDB: 0},
		{Block: b, LossDB: 0},
	})
	if dec != MixCollision {
		t.Errorf("decision = %d, want MixCollision", dec)
	}
	if !out.IsSilence() {
		t.Error("collision output should be silence in v1")
	}
}

func TestMixCollisionMarginAtThreshold(t *testing.T) {
	m := NewMixer(6, false, "silence")
	a := mkBlock(20000)
	b := mkBlock(20000)
	// margin = 6 dB exactly. Per PLAN: "if margin >= capture_db: capture".
	out, dec := m.Mix([]ActiveTX{
		{Block: a, LossDB: 0},
		{Block: b, LossDB: 6},
	})
	if dec != MixCapture {
		t.Errorf("decision = %d, want MixCapture (margin == captureDB)", dec)
	}
	if out.IsSilence() {
		t.Error("expected captured signal, got silence")
	}
}

func TestMixCollisionNoiseProducesGarble(t *testing.T) {
	m := NewMixer(6, false, "noise")
	a := mkBlock(20000)
	b := mkBlock(20000)
	// Both 6 dB down → margin = 0 < capture threshold → collision.
	// Strongest post-loss RMS = 20000 × 10^(-6/20) ≈ 10024; the garble
	// must come out at a comparable level, not silence and not full-scale.
	out, dec := m.Mix([]ActiveTX{
		{Block: a, LossDB: 6},
		{Block: b, LossDB: 6},
	})
	if dec != MixCollision {
		t.Fatalf("decision = %d, want MixCollision", dec)
	}
	if out.IsSilence() {
		t.Fatal("noise-mode collision must not be silent")
	}
	got := out.RMS()
	want := 20000 * AmplitudeFromDB(6)
	if got < 0.7*want || got > 1.3*want {
		t.Errorf("garble rms = %.0f, want within 30%% of strongest post-loss level %.0f", got, want)
	}
}

func TestMixCollisionNoiseLevelTracksStrongest(t *testing.T) {
	m := NewMixer(6, false, "noise")
	a := mkBlock(20000)
	b := mkBlock(20000)
	// A distant collision (both 30 dB down) is QUIET garble.
	out, dec := m.Mix([]ActiveTX{
		{Block: a, LossDB: 30},
		{Block: b, LossDB: 30},
	})
	if dec != MixCollision {
		t.Fatalf("decision = %d, want MixCollision", dec)
	}
	got := out.RMS()
	want := 20000 * AmplitudeFromDB(30) // ≈ 632
	if got < 0.7*want || got > 1.3*want {
		t.Errorf("weak-collision garble rms = %.0f, want within 30%% of %.0f", got, want)
	}
}

// Capture (margin >= capture_db) must be byte-identical between noise
// mode and the default — the noise model only changes what a *collision*
// sounds like.
func TestMixCaptureUnchangedInNoiseMode(t *testing.T) {
	strong := mkBlock(20000)
	weak := mkBlock(5000)
	active := []ActiveTX{
		{Block: weak, LossDB: 12},
		{Block: strong, LossDB: 0},
	}
	noiseOut, noiseDec := NewMixer(6, false, "noise").Mix(active)
	silenceOut, silenceDec := NewMixer(6, false, "silence").Mix(active)
	if noiseDec != MixCapture || silenceDec != MixCapture {
		t.Fatalf("decisions = %d / %d, want MixCapture for both", noiseDec, silenceDec)
	}
	if string(noiseOut) != string(silenceOut) {
		t.Error("capture output differs between noise and silence collision modes")
	}
}

// Single-source mixing must also be untouched by the collision mode.
func TestMixSingleUnchangedInNoiseMode(t *testing.T) {
	src := mkBlock(10000)
	active := []ActiveTX{{Block: src, LossDB: 6}}
	noiseOut, noiseDec := NewMixer(6, false, "noise").Mix(active)
	silenceOut, silenceDec := NewMixer(6, false, "silence").Mix(active)
	if noiseDec != MixSingle || silenceDec != MixSingle {
		t.Fatalf("decisions = %d / %d, want MixSingle for both", noiseDec, silenceDec)
	}
	if string(noiseOut) != string(silenceOut) {
		t.Error("single-source output differs between noise and silence collision modes")
	}
}

func TestAmplitudeFromDB(t *testing.T) {
	cases := []struct {
		lossDB float64
		want   float64
	}{
		{0, 1.0},
		{6, 0.501}, // ≈ 1/2
		{20, 0.1},  // 1/10
		{40, 0.01}, // 1/100
	}
	for _, c := range cases {
		got := AmplitudeFromDB(c.lossDB)
		diff := got - c.want
		if diff < -0.01 || diff > 0.01 {
			t.Errorf("AmplitudeFromDB(%g) = %g, want ~%g", c.lossDB, got, c.want)
		}
	}
}
