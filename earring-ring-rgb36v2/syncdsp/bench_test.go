package syncdsp

// What this file is for: the capability of the pair. Not "does the hash survive
// distortion" - the other tests cover that - but "at what signal level do two
// of these earrings start disagreeing, given the front end they actually have".
//
// The front-end model is measured, not invented. With the LEDs dark and nothing
// playing near the rig (tools/freeze.py, which stops TIM22 so the 250-sample
// window is read without the DMA writing under it):
//
//	device L   idle level 16990   MAD 1-2 codes   p-p 12-20   largest step 8-13
//	device R   idle level 17270   MAD 4-6 codes   p-p 112-168 largest step 53-89
//
// Both idle levels are the microphone bias at 0.26 of full scale (0.835 V on a
// 3.22 V supply), not the mid-rail the pipeline assumes, hence benchDC below.
// R's p-p is 25x its MAD with a 60-count step, so its input has isolated
// impulses on top of a smooth floor; L is quiet and clean. The impulses are the
// thing to be careful about: an earlier version of this file used 900-count
// spikes and concluded the pair was hopeless, and 900 counts was the *dump's*
// scale - the dump stores samples after the AGC's gain - while the pipeline
// input is pre-gain. Ten times too big, and every conclusion drawn from it was
// about a front end that does not exist.
//
// So: sweep the music level down through the measured noise floor, and watch
// where the modulation gate closes and where pattern agreement breaks. The gap
// between those two levels is the honest specification of "same colour".

import (
	"math"
	"testing"
)

// benchDC is the microphone's idle ADC code, measured on both devices.
const benchDC = 17120

// frontEnd models one device's input path: level, the real bias offset, the
// measured broadband floor, and the measured impulses. Per-device seeds, since
// the whole point is that the two devices do not share their noise.
func frontEnd(db int32, mad float64, spikeAmp float64, spikeRate float64, seed uint32) func([]uint16, *int32) {
	s := seed | 1
	next := func() float64 {
		s ^= s << 13
		s ^= s >> 17
		s ^= s << 5
		return (float64(s) + 0.5) / 4294967296
	}
	return func(w []uint16, _ *int32) {
		for i, v := range w {
			y := 32768 + (float64(int32(v)-32768) / float64(int32(1)<<(db/6)))
			if mad > 0 {
				// Sum of three uniforms: triangular, mean 0, scaled to the
				// measured MAD.
				y += mad * 1.65 * (next() + next() + next() - 1.5)
			}
			if next() < spikeRate {
				if next() < 0.5 {
					y -= spikeAmp
				} else {
					y += spikeAmp
				}
			}
			y += benchDC - 32768
			if y > 65535 {
				y = 65535
			} else if y < 0 {
				y = 0
			}
			w[i] = uint16(y)
		}
	}
}

// The two devices as measured. L is the clean one; R carries the impulses.
func benchL(db int32) func([]uint16, *int32) {
	return frontEnd(db, 1.5, 40, 0.03, 0x1d2c4a5b)
}
func benchR(db int32) func([]uint16, *int32) {
	return frontEnd(db, 5.0, 40, 0.03, 0x8f3b2c1d)
}

type capRow struct {
	db             int32
	openA, openB   float64
	agree          float64
	locked         float64
	swingA, swingB float64
}

func TestBenchCapability(t *testing.T) {
	const frames = 8 * synthSection / Window
	rows := []capRow(nil)
	// The sweep starts 12 dB below full scale on purpose. The microphone idles
	// at 17120, so a signal that really does span the converter spends half its
	// cycle against the bottom rail, and what gets measured then is the clipping,
	// not the pipeline. A real earring at that bias cannot produce a full-scale
	// excursion; -12 dB is about the loudest the front end has.
	for _, db := range []int32{12, 24, 36, 42, 48, 54, 60, 72} {
		// Built once per level, not once per frame: the model carries a seeded
		// generator, so rebuilding it would restart the sequence every frame and
		// turn the noise floor into a periodic tone.
		distA, distB := benchL(db), benchR(db)
		inA, inB := newInst(distA, false), newInst(distB, false)
		win := make([]uint16, Window)
		var openA, openB, bothLock, agree, locked int
		var swA, swB []float64
		for f := range frames {
			Synth(int64(f*Window), win)
			distA(win, &inA.carry)
			ea := inA.p.Process(win)
			Synth(int64(f*Window), win)
			distB(win, &inB.carry)
			eb := inB.p.Process(win)
			if ea.Modulating {
				openA++
			}
			if eb.Modulating {
				openB++
			}
			if ea.Locked && eb.Locked {
				bothLock++
				if ea.Pattern == eb.Pattern {
					agree++
				}
			}
			if ea.Locked {
				locked++
			}
			if f > 4*1<<ModPole {
				_, m1 := inA.p.Modulation()
				_, m2 := inB.p.Modulation()
				swA = append(swA, float64(m1)/256)
				swB = append(swB, float64(m2)/256)
			}
		}
		rows = append(rows, capRow{db,
			100 * float64(openA) / frames, 100 * float64(openB) / frames,
			100 * float64(agree) / math.Max(float64(bothLock), 1),
			100 * float64(locked) / frames,
			median(swA), median(swB)})
	}
	t.Logf("%5s %8s %8s %8s %7s %9s %9s", "dB", "gate A%", "gate B%", "lock%",
		"pattern", "swing A", "swing B")
	for _, r := range rows {
		t.Logf("%5d %8.1f %8.1f %8.1f %6.0f%% %8.2fb %8.2fb",
			r.db, r.openA, r.openB, r.locked, r.agree, r.swingA, r.swingB)
	}
	// The assertions are deliberately about the ends of the sweep, not the
	// middle: the middle is where the front end's noise crosses the music, and
	// where the answer is allowed to be "no". A loud source must agree, and a
	// source that does not exist must leave the devices idle rather than
	// inventing two different dances.
	loud := rows[0]
	if loud.agree < 90 {
		t.Errorf("at full level pattern agreement is %.0f%%", loud.agree)
	}

	// A room with nothing in it, which is not the same question as "the music 72
	// dB down" and must not be confused with it. After an AGC that is doing its job
	// a recording 72 dB down is a normal-looking signal - that level blindness is
	// what lets two earrings at different distances agree on a hash at all - so the
	// row above cannot come out idle and asking it to is asking the pipeline to be
	// level-sensitive again. What can be told apart is depth: a room has nothing
	// modulating it, and a recording does, however faint the recording was.
	for _, dev := range []struct {
		name string
		fe   func([]uint16, *int32)
	}{{"clean L", benchL(0)}, {"impulsive R", benchR(0)}} {
		in := newInst(nil, false)
		open := 0
		// Silence for the model is mid-rail: frontEnd maps mid-rail input onto the
		// measured bias itself, so pre-filling with benchDC would offset twice and
		// drive the samples against the bottom rail.
		idle := make([]uint16, Window)
		for range frames {
			for i := range idle {
				idle[i] = 32768
			}
			dev.fe(idle, &in.carry)
			if in.p.Process(idle).Modulating {
				open++
			}
		}
		pct := 100 * float64(open) / frames
		t.Logf("silent room, %-14s gate open %5.1f%%", dev.name, pct)
		if pct > 20 {
			t.Errorf("with nothing playing at all the gate is open %.0f%% of the time on the %s device",
				pct, dev.name)
		}
	}
}
