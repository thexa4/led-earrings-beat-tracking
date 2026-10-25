package syncdsp

// Scratch experiment (delete or promote): distribution of the per-frame loudness
// deviation, with and without the measured impulses, to see whether the depth
// feature's response to impulses is a few outlier frames (fixable with a
// frame-rate median) or a broad shift (not fixable that way).

import (
	"fmt"
	"math"
	"testing"
)

func TestScratchDistribution(t *testing.T) {
	const frames = 2 * synthSection / Window
	for _, cfg := range []struct {
		name   string
		db     int32
		mad    float64
		spAmp  float64
		spRate float64
	}{
		{"music-24 clean", 24, 1.5, 0, 0},
		{"music-24 +spikes", 24, 1.5, 40, 0.03},
		{"music-72 noise only", 72, 5.0, 0, 0},
		{"music-72 +spikes", 72, 5.0, 40, 0.03},
	} {
		dist := frontEnd2(cfg.db, cfg.mad, cfg.spAmp, cfg.spRate, 0x51ed270b)
		win := make([]uint16, Window)
		// Frame loudness straight off the distorted input - the same
		// sum-of-magnitudes the bank accumulates, without needing the pipeline's
		// internals - so the shape of the distribution is visible rather than
		// already smoothed away.
		var sums []float64
		for f := range frames {
			Synth(int64(f*Window), win)
			dist(win, nil)
			var acc float64
			for _, v := range win {
				acc += math.Abs(float64(int32(v) - 17120))
			}
			sums = append(sums, math.Log2(acc))
		}
		m := pct(sums, 0.5)
		dev := make([]float64, len(sums))
		for i, v := range sums {
			dev[i] = math.Abs(v - m)
		}
		t.Logf("%-22s level p50 %6.1f bits | |dev| p50 %5.2f p90 %5.2f p99 %5.2f max %5.2f",
			cfg.name, m, pct(dev, .5), pct(dev, .9), pct(dev, .99), pct(dev, 1.0))
	}
}

// frontEnd2 is frontEnd with the parameters spelled out, so the scratch test can
// turn the impulses off.
func frontEnd2(db int32, mad, spikeAmp, spikeRate float64, seed uint32) func([]uint16, *int32) {
	s := seed | 1
	next := func() float64 {
		s ^= s << 13
		s ^= s >> 17
		s ^= s << 5
		return (float64(s) + 0.5) / 4294967296
	}
	return func(w []uint16, _ *int32) {
		for i, v := range w {
			y := float64(int32(v)-32768) / math.Pow(2, float64(db)/6)
			if mad > 0 {
				y += mad * 1.65 * (next() + next() + next() - 1.5)
			}
			if next() < spikeRate {
				if next() < 0.5 {
					y -= spikeAmp
				} else {
					y += spikeAmp
				}
			}
			y += benchDC
			if y > 65535 {
				y = 65535
			} else if y < 0 {
				y = 0
			}
			w[i] = uint16(y)
		}
	}
}

// How often does the impulse rejector fire, and on what? Counts are cumulative
// over the run, so the rate per million samples is the readable number; the step
// is the tracker's scale, which must sit near the input's own step size for the
// limit to mean anything.
func TestScratchRejector(t *testing.T) {
	const frames = 2 * synthSection / Window
	for _, cfg := range []struct {
		name   string
		db     int32
		mad    float64
		spAmp  float64
		spRate float64
	}{
		{"music-24 clean", 24, 0, 0, 0},
		{"music-24 bench noise", 24, 1.5, 0, 0},
		{"music-24 +spikes", 24, 1.5, 40, 0.03},
		{"music-72 +spikes", 72, 5.0, 40, 0.03},
		{"music-72 noise only", 72, 5.0, 0, 0},
	} {
		dist := frontEnd2(cfg.db, cfg.mad, cfg.spAmp, cfg.spRate, 0x51ed270b)
		var p Pipeline
		p.Init()
		win := make([]uint16, Window)
		for f := range frames {
			Synth(int64(f*Window), win)
			dist(win, nil)
			p.Process(win)
		}
		c := p.SpikeStats()
		n := int64(frames * Window)
		t.Logf("%-22s changed %7d (%6.0f per million, %.2f%%)",
			cfg.name, c, 1e6*float64(c)/float64(n), 100*float64(c)/float64(n))
	}
}

// Is the pipeline's own output stationary when its input is stationary? The
// bench case that disagrees is a quiet room, which is a stationary input, and
// the frame-loudness modulation the gate reads should then be small. If it is
// not, the modulation is the pipeline's own - the DC blocker idling on the
// 15600-count bias error - and two devices idle differently.
//
// Compare the same noise at the mid-rail the pipeline subtracts against the same
// noise at the microphone's real idle code.
func TestScratchStationary(t *testing.T) {
	const frames = 2 * synthSection / Window
	for _, cfg := range []struct {
		name string
		dc   float64
		mad  float64
		sp   float64
	}{
		{"noise 30 at mid-rail", 32768, 30, 0},
		{"noise 30 at mic bias", 17120, 30, 0},
		{"noise 5 at mic bias", 17120, 5, 0},
		{"noise 5 at mic bias +spikes", 17120, 5, 40},
		{"noise 5 at mid-rail", 32768, 5, 0},
		{"silence at mic bias", 17120, 0, 0},
	} {
		s := uint32(0x51ed270b)
		next := func() float64 {
			s ^= s << 13
			s ^= s >> 17
			s ^= s << 5
			return (float64(s) + 0.5) / 4294967296
		}
		var p Pipeline
		p.Init()
		win := make([]uint16, Window)
		var mods, tots []float64
		for f := range frames {
			for i := range win {
				y := cfg.dc + cfg.mad*1.65*(next()+next()+next()-1.5)
				if cfg.sp > 0 && next() < 0.03 {
					if next() < 0.5 {
						y -= cfg.sp
					} else {
						y += cfg.sp
					}
				}
				win[i] = uint16(y)
			}
			p.Process(win)
			if f > 4*1<<ModPole {
				_, m := p.Modulation()
				mods = append(mods, float64(m)/256)
				tots = append(tots, float64(p.total)/256)
			}
		}
		m := pct(tots, 0.5)
		dev := make([]float64, len(tots))
		for i, v := range tots {
			dev[i] = math.Abs(v - m)
		}
		t.Logf("%-26s total p50 %6.0f bits, |dev| p50 %5.2f p90 %5.2f | logMod p50 %5.2f p90 %5.2f bits",
			cfg.name, m, pct(dev, .5), pct(dev, .9), pct(mods, .5), pct(mods, .9))
	}
}

// The depth tracker is a mean, so a handful of enormous frames move it. Pick the
// bound on each frame's deviation from data: replay the tracker over sequences
// recorded from the real pipeline and see what each bound does to the two
// populations that have to stay apart - a quiet room whose only "modulation" is
// the microphone's own impulses, and real music down at the noise floor.
func TestScratchDepthCap(t *testing.T) {
	const frames = 2 * synthSection / Window
	cases := []struct {
		name string
		wav  string
		db   float64
		mad  float64
		sp   float64
	}{
		{"room: noise 5 +40sp", "", 0, 5, 40},
		{"room: noise 5 +90sp", "", 0, 5, 90},
		{"room: noise 2 +90sp", "", 0, 2, 90},
		{"music -36", "/tmp/psync/music16.wav", 36, 0, 0},
		{"music -48", "/tmp/psync/music16.wav", 48, 0, 0},
		{"music -60", "/tmp/psync/music16.wav", 60, 0, 0},
		{"talk -36", "/tmp/psync/talk16.wav", 36, 0, 0},
		{"talk -54", "/tmp/psync/talk16.wav", 54, 0, 0},
	}
	caps := []int32{1 << 30, 1024, 768, 512, 384, 256}
	head := "case              "
	for _, c := range caps {
		if c > 1<<29 {
			head += "  none  "
		} else {
			head += fmt.Sprintf(" %5.1fb", float64(c)/256)
		}
	}
	t.Log(head)
	for _, tc := range cases {
		seq := depthSeq(t, tc.wav, tc.db, tc.mad, tc.sp, frames)
		if len(seq) == 0 {
			t.Logf("%-18s (no fixture)", tc.name)
			continue
		}
		line := fmt.Sprintf("%-18s", tc.name)
		for _, cap := range caps {
			line += fmt.Sprintf(" %6.2f", capDepth(seq, cap))
		}
		t.Log(line)
	}
}

// depthSeq records the pipeline's own per-frame loudness (log2 Q8) over a
// synthetic bench input: music optional, attenuated by db, on top of the
// microphone's real idle code, with a triangular noise floor of the given MAD
// and, at 3% of samples, the isolated impulses freeze.py measured on the right
// earring.
func depthSeq(t *testing.T, wav string, db, mad, spike float64, frames int) []int32 {
	seq, _ := depthSeqBias(t, wav, db, mad, spike, frames, 17120)
	return seq
}

// depthSeqSpike is depthSeq plus the fraction of samples the impulse limiter
// actually changed on the way through.
func depthSeqSpike(t *testing.T, wav string, db, mad, spike float64, frames int) ([]int32, float64) {
	return depthSeqBias(t, wav, db, mad, spike, frames, 17120)
}

// depthSeqBias is depthSeq with the idle code the signal sits on chosen by the
// caller, so the only difference between two runs can be the DC error the
// pipeline's blocker has to absorb.
func depthSeqBias(t *testing.T, wav string, db, mad, spike float64, frames int, bias float64) ([]int32, float64) {
	t.Helper()
	var src []uint16
	if wav != "" {
		src = loadWav16(t, wav)
		if len(src) < frames*Window {
			return nil, 0
		}
	}
	atten := math.Pow(2, db/6) // 6 dB per bit, the convention frontEnd uses
	s := uint32(0x51ed270b)
	next := func() float64 {
		s ^= s << 13
		s ^= s >> 17
		s ^= s << 5
		return (float64(s) + 0.5) / 4294967296
	}
	var p Pipeline
	p.Init()
	win := make([]uint16, Window)
	var out []int32
	for f := range frames {
		for i := range win {
			y := 0.0
			if src != nil {
				y = float64(int32(src[f*Window+i])-32768) / atten
			}
			if mad > 0 {
				y += mad * 1.65 * (next() + next() + next() - 1.5)
			}
			if spike > 0 && next() < 0.03 {
				if next() < 0.5 {
					y -= spike
				} else {
					y += spike
				}
			}
			y += bias
			if y < 0 {
				y = 0
			} else if y > 65535 {
				y = 65535
			}
			win[i] = uint16(y)
		}
		p.Process(win)
		if f > 4*1<<ModPole {
			out = append(out, p.logSum)
		}
	}
	return out, float64(p.SpikeStats()) / float64(frames*Window)
}

// capDepth replays the depth tracker over a recorded loudness sequence with a
// given bound on each frame's deviation, and reports where the tracker settles,
// in bits.
func capDepth(seq []int32, cap int32) float64 {
	var avg, mod int32
	for i, v := range seq {
		ldev := v - avg
		avg += (ldev + 1<<(ModPole-1)) >> ModPole
		if ldev < 0 {
			ldev = -ldev
		}
		if ldev > cap {
			ldev = cap
		}
		mod += (ldev - mod + 1<<(ModPole-1)) >> ModPole
		if i == len(seq)/2 {
			break
		}
	}
	return float64(mod) / 256
}

// Does the microphone's idle code matter to the pipeline? The pipeline subtracts
// the mid-rail, but this microphone idles at 0.26 of full scale, so every sample
// carries a 15600-count error into the DC blocker. At a normal level the blocker
// swamps that with signal; the question is what it does at the levels the bench
// actually sees. The two earrings idle 270 counts apart, so anything the blocker
// does with the offset is device-specific by construction.
func TestScratchBias(t *testing.T) {
	const frames = 2 * synthSection / Window
	t.Logf("%-16s %7s %7s %7s %7s", "case", "32768", "24000", "17120", "17260")
	for _, tc := range []struct {
		name string
		wav  string
		db   float64
		mad  float64
	}{
		{"music -36", "/tmp/psync/music16.wav", 36, 0},
		{"music -48", "/tmp/psync/music16.wav", 48, 0},
		{"music -54", "/tmp/psync/music16.wav", 54, 0},
		{"music -60", "/tmp/psync/music16.wav", 60, 0},
		{"music -66", "/tmp/psync/music16.wav", 66, 0},
		{"talk -60", "/tmp/psync/talk16.wav", 60, 0},
		{"room noise 3", "", 0, 3},
	} {
		line := fmt.Sprintf("%-16s", tc.name)
		for _, bias := range []float64{32768, 24000, 17120, 17260} {
			seq, _ := depthSeqBias(t, tc.wav, tc.db, tc.mad, 0, frames, bias)
			if len(seq) == 0 {
				line += "     n/a"
				continue
			}
			mad := pct32(seq, 0.5)
			dev := make([]float64, len(seq))
			for i, v := range seq {
				dev[i] = math.Abs(float64(v)/256 - mad)
			}
			line += fmt.Sprintf(" %7.2f", pct(dev, 0.5))
		}
		t.Log(line)
	}
}

// pct32 is pct over an int32 slice.
func pct32(v []int32, q float64) float64 {
	s := make([]float64, len(v))
	for i, x := range v {
		s[i] = float64(x) / 256
	}
	return pct(s, q)
}

// Is the fake modulation the impulses at all, or the AGC running out of signal?
// The same room with no impulses at all, at each noise floor, plus the absolute
// peak the microphone actually produces - which is the one number that separates
// "nothing is playing" from "something is" without knowing anything about the
// music, and which the AGC deliberately throws away.
func TestScratchFloor(t *testing.T) {
	const frames = 2 * synthSection / Window
	t.Logf("%-22s %7s %7s %7s %7s", "case", "depth", "gain", "raw p-p", "post pk")
	for _, tc := range []struct {
		mad, sp float64
	}{
		{0, 0}, {1, 0}, {2, 0}, {3, 0}, {5, 0}, {10, 0}, {30, 0},
		{2, 90}, {5, 40}, {5, 90}, {3, 90},
	} {
		name := fmt.Sprintf("noise %g sp %g", tc.mad, tc.sp)
		s := uint32(0x51ed270b)
		next := func() float64 {
			s ^= s << 13
			s ^= s >> 17
			s ^= s << 5
			return (float64(s) + 0.5) / 4294967296
		}
		var p Pipeline
		p.Init()
		win := make([]uint16, Window)
		var depths, rawPk, postPk []float64
		var gains []float64
		for f := range frames {
			mn, mx := 65535.0, 0.0
			for i := range win {
				y := 17120.0
				if tc.mad > 0 {
					y += tc.mad * 1.65 * (next() + next() + next() - 1.5)
				}
				if tc.sp > 0 && next() < 0.03 {
					if next() < 0.5 {
						y -= tc.sp
					} else {
						y += tc.sp
					}
				}
				if y < 0 {
					y = 0
				} else if y > 65535 {
					y = 65535
				}
				if y < mn {
					mn = y
				}
				if y > mx {
					mx = y
				}
				win[i] = uint16(y)
			}
			p.Process(win)
			if f > 4*1<<ModPole {
				_, m := p.Modulation()
				depths = append(depths, float64(m)/256)
				rawPk = append(rawPk, mx-mn)
				postPk = append(postPk, float64(p.agcPeak))
				gains = append(gains, float64(p.agcShift))
			}
		}
		t.Logf("%-22s %7.2f %7.1f %7.0f %7.0f", name, pct(depths, .5), pct(gains, .5),
			pct(rawPk, .5), pct(postPk, .5))
	}
}

// The threshold table, measured end to end on the pipeline as it now stands:
// every quiet-room model against every real recording, at every level, reporting
// the depth tracker's own distribution. The gate has to sit above the top of the
// first group and below the bottom of the second, and this is the table that
// says whether it does.
func TestScratchGateTable(t *testing.T) {
	const frames = 2 * synthSection / Window
	t.Logf("%-26s %6s %6s %6s   %s", "input", "p10", "p50", "p90", "% gated on")
	rows := []struct {
		name string
		wav  string
		db   float64
		mad  float64
		sp   float64
	}{
		{"room: dead", "", 0, 0, 0},
		{"room: noise 1", "", 0, 1, 0},
		{"room: noise 3", "", 0, 3, 0},
		{"room: noise 30", "", 0, 30, 0},
		{"room: noise 300", "", 0, 300, 0},
		{"room: noise 2 +90sp", "", 0, 2, 90},
		{"room: noise 5 +90sp", "", 0, 5, 90},
		{"room: noise 5 +40sp", "", 0, 5, 40},
		{"music -24", "/tmp/psync/music16.wav", 24, 0, 0},
		{"music -48", "/tmp/psync/music16.wav", 48, 0, 0},
		{"music -60", "/tmp/psync/music16.wav", 60, 0, 0},
		{"music -48 +300 noise", "/tmp/psync/music16.wav", 48, 300, 0},
		{"music -60 +90sp", "/tmp/psync/music16.wav", 60, 0, 90},
		{"talk -36", "/tmp/psync/talk16.wav", 36, 0, 0},
		{"talk -60", "/tmp/psync/talk16.wav", 60, 0, 0},
	}
	for _, tc := range rows {
		seq, act := depthSeqSpike(t, tc.wav, tc.db, tc.mad, tc.sp, frames)
		if len(seq) == 0 {
			t.Logf("%-26s (no fixture)", tc.name)
			continue
		}
		// Replay the shipped tracker over the recorded loudness sequence.
		var avg, mod int32
		var vals []float64
		// The replayed tracker starts at zero, so its first frames carry the
		// distance from silence rather than the modulation; 2^ModPole frames
		// halves it, and a few hundred put it under a tenth of a bit.
		for i, v := range seq {
			ldev := v - avg
			avg += (ldev + 1<<(ModPole-1)) >> ModPole
			if ldev < 0 {
				ldev = -ldev
			}
			mod += (ldev - mod + 1<<(ModPole-1)) >> ModPole
			if i > 400 {
				vals = append(vals, float64(mod)/256)
			}
		}
		// The number that matters is the hysteresis state, not the raw
		// threshold: a gate that flickers costs a frame of disagreement at every
		// edge, which is what it exists to prevent.
		on, onFrames := false, 0
		for _, v := range vals {
			q8 := int32(v * 256)
			switch {
			case !on && q8 >= ModEnterQ8:
				on = true
			case on && q8 < ModHoldQ8:
				on = false
			}
			if on {
				onFrames++
			}
		}
		t.Logf("%-26s %6.2f %6.2f %6.2f   %5.1f%% act=%.2f%%", tc.name, pct(vals, .1), pct(vals, .5),
			pct(vals, .9), 100*float64(onFrames)/float64(len(vals)),
			act)
	}
}
