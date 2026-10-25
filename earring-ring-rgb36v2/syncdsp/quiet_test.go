package syncdsp

// The condition the earrings are actually in on the bench, which every other
// test here avoids: an input with no music in it.
//
// Measured at the rig (tools/freeze.py, tools/micdump.py on the two devices,
// LEDs dark, nothing playing near them): the whole input swing is a few ADC
// counts, one device about 20 dB noisier than the other, and the loudest things
// in the window are single-sample impulses that are uncorrelated between the
// two. The AGC normalises all of that into its 3000..12000 band, so downstream
// - after a gain that was chosen to make level invisible - amplified room noise
// is *indistinguishable from quiet music*. Two devices hashing their own
// independent noise have no reason to agree, and the measured on-device pattern
// agreement, ~8%, is about what a random pick among the patterns would give.
//
// So the question this file answers is not "can the hash survive noise" (it
// can, TestLevelMatrix shows 85-99% pattern agreement down to 9 counts RMS with
// a clean source) but "does the pipeline know the difference between a quiet
// passage and nothing at all", because the answer right now is no: the only
// level gate is `linear < linPeak/SilenceDiv`, which is relative to the loudest
// recent frame, and a frame of noise compared against other frames of noise is
// never quiet.
//
// Baseline numbers, printed by TestQuietMatrix. They are the "before" for the
// idle-state work, so they are printed rather than asserted: nothing here fails
// if the pipeline is made to sit still in a quiet room, and it should not pass
// while it cannot.

import (
	"math"
	"sort"
	"testing"
)

// mute throws the source away: the input is then whatever the noise model adds
// on top of the mid-rail, which is the bench with nothing playing.
func mute(w []uint16, _ *int32) {
	for i := range w {
		w[i] = 32768
	}
}

// chain applies several distortions in order, left to right.
func chain(fs ...func([]uint16, *int32)) func([]uint16, *int32) {
	return func(w []uint16, carry *int32) {
		for _, f := range fs {
			f(w, carry)
		}
	}
}

// spiky adds the impulses the bench actually has: single samples tens to
// hundreds of counts above the local level, at a given rate per sample, with a
// per-device seed. They are the residue of the ADC input node being high
// impedance next to a switching LED driver, and the reason they matter is the
// AGC peak and the onset detector are both single-sample measures.
func spiky(rate, amp float64, seed uint32) func([]uint16, *int32) {
	s := seed | 1
	next := func() float64 {
		s ^= s << 13
		s ^= s >> 17
		s ^= s << 5
		return (float64(s) + 0.5) / 4294967296
	}
	return func(w []uint16, _ *int32) {
		for i, v := range w {
			if next() < rate {
				d := int32(v) - 32768
				if next() < 0.5 {
					d -= int32(amp)
				} else {
					d += int32(amp)
				}
				if d > 32767 {
					d = 32767
				} else if d < -32768 {
					d = -32768
				}
				w[i] = uint16(d + 32768)
			}
		}
	}
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[len(s)/2]
}

type quietCase struct {
	name string
	a, b func([]uint16, *int32)
}

// runQuiet runs the same source through two devices with the given distortions
// and reports what the two agree on, along with how often each was locked.
func runQuiet(t *testing.T, c quietCase, frames int) {
	t.Helper()
	inA, inB := newInst(c.a, false), newInst(c.b, false)
	win := make([]uint16, Window)
	var agree, both, lockedA, lockedB int
	pats := map[int32]int{}
	var modA, modB []float64
	for f := range frames {
		Synth(int64(f*Window), win)
		if c.a != nil {
			c.a(win, &inA.carry)
		}
		ea := inA.p.Process(win)
		Synth(int64(f*Window), win)
		if c.b != nil {
			c.b(win, &inB.carry)
		}
		eb := inB.p.Process(win)
		if ea.Locked {
			lockedA++
		}
		if eb.Locked {
			lockedB++
		}
		if ea.Locked && eb.Locked {
			both++
			if ea.Pattern == eb.Pattern {
				agree++
			}
		}
		pats[eb.Pattern]++
		if f > 2*1<<ModPole { // let the trackers fill before sampling them
			_, m1 := inA.p.Modulation()
			_, m2 := inB.p.Modulation()
			modA = append(modA, float64(m1)/256)
			modB = append(modB, float64(m2)/256)
		}
	}
	distinct := len(pats)
	t.Logf("%-26s lock A/B %3.0f%%/%3.0f%%  pattern agree %3.0f%%  patterns %d  swing A/B %.2f/%.2f bits",
		c.name, 100*float64(lockedA)/float64(frames),
		100*float64(lockedB)/float64(frames),
		100*float64(agree)/math.Max(float64(both), 1), distinct,
		median(modA), median(modB))
}

func TestQuietMatrix(t *testing.T) {
	const frames = 8 * synthSection / Window
	// The bench input is a couple of counts; the synth is written at a healthy
	// level, so -60 dB puts it in the same place as the rig while the noise
	// amplitudes are the ones micdump measured (2 counts on the quiet device,
	// 25 on the loud one) plus a deliberately loud case to show the gate is not
	// just about small numbers.
	music := func(db int32) func([]uint16, *int32) { return attenuate(db) }
	cases := []quietCase{
		{"clean music", nil, nil},
		{"music -36dB", music(36), music(36)},
		{"music -60dB", music(60), music(60)},
		{"noise 3 only", chain(mute, noisy(3, 11)), chain(mute, noisy(3, 202))},
		{"noise 30 only", chain(mute, noisy(30, 11)), chain(mute, noisy(30, 202))},
		{"noise 300 only", chain(mute, noisy(300, 11)),
			chain(mute, noisy(300, 202))},
		{"music -60 + noise 30", chain(music(60), noisy(30, 11)),
			chain(music(60), noisy(30, 202))},
		{"music -36 + noise 300", chain(music(36), noisy(300, 11)),
			chain(music(36), noisy(300, 202))},
		{"bench: music -60 noise 3/30 +spikes",
			chain(music(60), noisy(3, 11), spiky(0.01, 900, 7)),
			chain(music(60), noisy(30, 202), spiky(0.01, 900, 99))},
	}
	for _, c := range cases {
		runQuiet(t, c, frames)
	}
}
