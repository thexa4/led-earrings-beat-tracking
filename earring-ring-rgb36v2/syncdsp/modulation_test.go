package syncdsp

// Measure the modulation-depth feature (linAvg, linMod) on real recordings, to
// pick the threshold of a "is there anything here at all" gate from data rather
// than from a guess.
//
// The feature has to satisfy two properties at once, and only measurement says
// whether it does:
//
//   - level-independent: the AGC puts a whisper and a blast in the same internal
//     range, so a gate that reads depth must give the same answer at both;
//   - able to separate a real recording from amplified noise, at the signal to
//     noise ratios the bench actually has (the rig hears a couple of ADC counts
//     with nothing playing, and the two devices hear different counts).
//
// Run over the fixtures with:
//
//	MODWAV=/tmp/psync/music16.wav go test -count=1 -v -run TestModulationReal ./syncdsp/
//
// It reports, per (level, noise) cell, the percentile spread of the depth over
// the recording and the fraction of frames that would fall under the candidate
// gate - which is what "the earrings go idle" means, so it is the number to
// watch when choosing ModMinShift.

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"sort"
	"testing"
)

func loadWav16(t *testing.T, path string) []uint16 {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("no fixture: %v", err)
	}
	var data []byte
	for i := 12; i+8 <= len(b); {
		id, n := string(b[i:i+4]), int(binary.LittleEndian.Uint32(b[i+4:i+8]))
		if id == "data" {
			data = b[i+8 : i+8+n]
		}
		i += 8 + n + n%2
	}
	out := make([]uint16, len(data)/2)
	for i := range out {
		out[i] = binary.LittleEndian.Uint16(data[2*i:])
	}
	return out
}

// depths runs the pipeline over an already-distorted copy of the source and
// returns the per-frame modulation depth in percent, after warmup.
func depths(samples []uint16) []float64 {
	var p Pipeline
	p.Init()
	var out []float64
	for f := 0; (f+1)*Window <= len(samples); f++ {
		for i := f * Window; i < (f+1)*Window; i++ {
			p.ProcessSample(samples[i])
		}
		p.EndFrame()
		if f < 4*1<<ModPole {
			continue
		}
		_, mad := p.Modulation()
		out = append(out, float64(mad)/256)
	}
	return out
}

func pct(v []float64, q float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	i := int(q * float64(len(s)-1))
	return s[i]
}

func TestModulationReal(t *testing.T) {
	path := os.Getenv("MODWAV")
	if path == "" {
		t.Skip("MODWAV not set")
	}
	src := loadWav16(t, path)
	// The fixtures are at a normal listening level. The bench sees a peak of a
	// few counts, so the sweep goes a long way down: gain 1.0 is "at the PA",
	// gain 1/4096 is beyond the rig.
	cells := []struct {
		name  string
		gain  float64
		noise float64
	}{
		{"full", 1, 0},
		{"-24dB", 1.0 / 16, 0},
		{"-48dB", 1.0 / 256, 0},
		{"-60dB", 1.0 / 1024, 0},
		{"-60dB + noise 30", 1.0 / 1024, 30},
		{"-24dB + noise 300", 1.0 / 16, 300},
		{"muted + noise 30", 0, 30},
		{"muted + noise 300", 0, 300},
	}
	t.Logf("%-20s %6s %6s %6s %6s   %s", "cell", "p10", "p50", "p90", "mean",
		"% frames under 0.4 bits")
	for _, c := range cells {
		s := make([]uint16, len(src))
		rng := uint32(0x9e3779b9)
		for i, v := range src {
			y := float64(int32(v)-32768) * c.gain
			if c.noise > 0 {
				// Cheap uniform-ish noise: sum of three draws, triangular in
				// shape, good enough to average against.
				rng ^= rng << 13
				rng ^= rng >> 17
				rng ^= rng << 5
				u := (float64(rng&0xFFFF) + float64(rng>>16) - 65535) / 65535
				y += c.noise * 1.8 * u
			}
			if y > 32767 {
				y = 32767
			} else if y < -32768 {
				y = -32768
			}
			s[i] = uint16(int32(y) + 32768)
		}
		d := depths(s)
		var sum float64
		under := 0
		for _, v := range d {
			sum += v
			if v < 0.4 {
				under++
			}
		}
		t.Logf("%-20s %6.1f %6.1f %6.1f %6.1f   %5.1f%%  (%d frames)",
			c.name, pct(d, 0.1), pct(d, 0.5), pct(d, 0.9),
			sum/math.Max(float64(len(d)), 1),
			100*float64(under)/math.Max(float64(len(d)), 1), len(d))
	}
	fmt.Println()
}
