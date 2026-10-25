package syncdsp

// The front end as it actually behaves while the earrings are animating.
//
// Everything else in this package models the impulses measured with the LEDs
// DARK (tools/freeze.py: 40-90 codes, symmetric, on a 1.5-code floor), which
// led to an impulse filter that only acts in a flat field. With the animation
// running - which is the only state a pair of earrings is ever in - the input
// looks completely different, and tools/haltab.py proves the difference is
// self-inflicted: the same window read with the CPU halted versus running,
// TIM22/ADC/DMA running in both cases,
//
//	L running   p-p med 1042  max 2093   >500 up 20  down 0   biggest +2062
//	L halted    p-p med   66  max   82   >500 up  0  down 0   biggest   +59
//	R running   p-p med  535  max 8705   >500 up 53  down 1   biggest +8193
//	R halted    p-p med   34  max   49   >500 up  0  down 0   biggest   +40
//
// and tools/wavedump.py shows what they look like: one sample wide, runs never
// wider than two, and strictly positive (51 up vs 1 down, 104 up vs 0 down).
// Sound is symmetric, so these are the LED bit-bang coupling into the
// converter, and they are 20-40 dB above the music: the 1 kHz tone in the room
// measures 200-270 counts of coherent amplitude on L while its excursions reach
// 16,381.
//
// Two consequences for the filter as written. It tolerates runs of two (a
// 5-point median needs three inliers), so the width is not the problem. The
// problem is SpikeCodesQuiet: it demands a neighbourhood still to 16 codes, and
// the music's own sample-to-sample slope at these levels is a hundred codes or
// more, so while anything is playing the filter declines to have an opinion -
// exactly when the coupling is loudest, since LED brightness follows the beat.
//
// This file measures what that costs, with spikes drawn from the distribution
// above rather than invented.

import (
	"math"
	"testing"
)

// ledSpikes models one device's input while it animates: the music, the
// measured idle bias, and one-sided positive excursions at the measured rate.
//
// amp is the largest excursion; they are drawn log-uniformly from 500 codes
// (the detection gate used on the device) up to amp, because the measured ones
// are spread over that whole range rather than clustered. rate is the fraction
// of samples involved, and run2 the fraction of those that arrive as a pair,
// which is what tools/wavedump.py's run-width histogram shows (43 singles + 4
// pairs on L, 92 + 6 on R).
func ledSpikes(rate, run2, amp float64, seed uint32) func([]uint16, *int32) {
	s := seed | 1
	next := func() float64 {
		s ^= s << 13
		s ^= s >> 17
		s ^= s << 5
		return (float64(s) + 0.5) / 4294967296
	}
	hold := 0
	return func(w []uint16, _ *int32) {
		for i, v := range w {
			y := float64(int32(v)-32768) + benchDC
			if hold > 0 {
				// Second sample of a pair: same excursion, slightly smaller,
				// which is what the captured pairs look like.
				y += 0.7 * amp * math.Log2(1+next()*31) / 5
				hold--
			} else if next() < rate {
				a := 500 * math.Exp2(next()*math.Log2(amp/500))
				y += a
				if next() < run2 {
					hold = 1
				}
			}
			if y > 65535 {
				y = 65535
			} else if y < 0 {
				y = 0
			}
			w[i] = uint16(y)
		}
	}
}

// TestLedNoiseCost is the A/B that decides whether the impulse filter has to
// change: the same music through two devices whose only difference is an
// independent LED-coupling spike train, once with the spikes absent (the
// ceiling agreement can reach) and once with them at the measured rate.
func TestLedNoiseCost(t *testing.T) {
	const frames = testFrames
	do := func(mk func([]uint16, *int32)) (tr trace, sp int32) {
		in := newInst(mk, false)
		tr = trace{evs: make([]Events, frames), period: make([]int32, frames)}
		win := make([]uint16, Window)
		for f := range frames {
			Synth(int64(f*Window), win)
			mk(win, nil)
			tr.evs[f] = in.p.Process(win)
		}
		sp = in.p.SpikeStats()
		return tr, sp
	}
	clean := func(w []uint16, _ *int32) {}
	// L measures 50 excursions over 30 windows of 250 = 0.7% of samples, R 104
	// = 1.4%; largest seen +16381 and +8212.
	rA, _ := do(clean)
	rB, _ := do(clean)
	sA, spA := do(ledSpikes(0.007, 0.09, 16000, 0x51ed2704))
	sB, spB := do(ledSpikes(0.014, 0.06, 8200, 0x2f3b8c1d))

	ag := func(a, b trace) float64 {
		same, tot := agree(a, b, func(x, y Events) bool { return x.Pattern == y.Pattern })
		return float64(same) / math.Max(float64(tot), 1)
	}
	t.Logf("filter replaced %.2f%% of samples (spiked A), %.2f%% (spiked B)",
		100*float64(spA)/float64(frames*Window), 100*float64(spB)/float64(frames*Window))
	t.Logf("pattern agreement  clean vs clean %.1f%%", 100*ag(rA, rB))
	t.Logf("pattern agreement  spiked vs spiked %.1f%%", 100*ag(sA, sB))
	t.Logf("pattern agreement  clean vs spiked %.1f%% / %.1f%%",
		100*ag(rA, sA), 100*ag(rB, sB))

	var pa, ps []int32
	for _, e := range rA.evs[WarmupFrames:] {
		pa = append(pa, e.Pattern)
	}
	for _, e := range sA.evs[WarmupFrames:] {
		ps = append(ps, e.Pattern)
	}
	t.Logf("patterns used: clean %d, spiked %d", uniq(pa), uniq(ps))
	if a := ag(rA, rB); a < 0.9 {
		t.Errorf("harness broken: clean vs clean only %.0f%%", 100*a)
	}
}

func uniq(v []int32) int {
	m := map[int32]bool{}
	for _, x := range v {
		m[x] = true
	}
	return len(m)
}
