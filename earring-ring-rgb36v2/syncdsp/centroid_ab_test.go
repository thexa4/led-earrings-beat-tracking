package syncdsp

// Does the spectral centroid still pick the same colour on two different
// earrings?
//
// The claim under test is specific: a pattern read from a weighted sum of the
// spectrum, compared against that device's own running average and spread, is
// invariant to the things that differ between two pieces of the same hardware -
// a microphone a few dB quieter, a response tilted towards the bass, and above
// all noise that has nothing to do with what the other one is hearing. The
// bench measures this directly, but a bench pass is ~5 minutes of two radios and
// a tuned sink, and it can only be run against whatever firmware is flashed.
// This runs the same computation over real material in seconds, over device
// pairs that cannot be produced any other way, and it separates the three things
// that get conflated on hardware: the arithmetic, the AGC, and the room.
//
//	PODCAST=~/psync/edm280.wav go test -count=1 -v -run TestPodcastCentroid ./syncdsp/
//
// Read the numbers together, never on their own. Agreement alone has a degenerate
// answer - two devices both stuck on one colour score ~81% with a chance-
// corrected agreement of exactly zero - so every row carries kappa (agreement
// beyond what the two occupancy histograms predict), the change rate, and the
// per-pattern occupancy. A row is only good if agreement is high, kappa is most
// of the way towards it, all four patterns appear, and neither device is
// flickering.
//
// The distortions are applied to the samples, before the pipeline, exactly as a
// real front end would apply them, so everything downstream - the AGC, the floor
// pin, the contrast step, the centroid and its reference - is the shipped code.
//
// One caveat about the level rows, which matters for how much to trust them. They
// attenuate a signal that is already quantised to 16 bits, so each 6 dB of
// attenuation spends a bit of resolution and the quiet device ends up with a worse
// signal-to-noise ratio than the loud one. Whether a real earring hears that too
// depends on where its noise floor lives: if it is acoustic - the room, which both
// microphones pick up in the same proportion as the music - the AGC equalises the
// two and nothing is lost; if it is the ADC's own, the penalty is real. The bench
// says there is a penalty, which is itself the evidence that the floor is
// electronic, and the 20 dB row should be read as the pessimistic end.

import (
	"fmt"
	"math"
	"os"
	"sort"
	"testing"
)

// gain scales the signal, which is what a microphone a few dB less sensitive
// does before anything else notices. The AGC is supposed to erase this entirely;
// if it does not, the centroid is reading level rather than shape.
func gain(f float64) func([]uint16, *int32) {
	return func(w []uint16, _ *int32) {
		for i, v := range w {
			y := 32768 + (float64(int32(v)-32768) * f)
			if y > 65535 {
				y = 65535
			} else if y < 0 {
				y = 0
			}
			w[i] = uint16(y)
		}
	}
}

// bassTilt adds a standing bass emphasis, the stand-in for one earring's
// response sitting lower in the treble than its partner's. Unlike gain this is
// *not* uniform across the spectrum, so it survives the AGC and shows up in the
// centroid as a constant offset - which is precisely what centAvg is there to
// remove. Corner ~200 Hz, amount in doublings of the low part relative to the
// high part.
//
// Streaming rather than whole-slice like the tilt() in tilt_agreement_test.go,
// because this one has to sit in front of a pipeline that is fed frame by frame.
func bassTilt(doublings float64) func([]uint16, *int32) {
	const a = 0.925 // exp(-2*pi*200/16000)
	k := math.Pow(2, doublings) - 1
	return func(w []uint16, carry *int32) {
		lp := float64(*carry)
		for i, v := range w {
			x := float64(int32(v) - 32768)
			lp = a*lp + (1-a)*x
			y := x + k*lp
			if y > 32767 {
				y = 32767
			} else if y < -32768 {
				y = -32768
			}
			w[i] = uint16(int32(y) + 32768)
		}
		*carry = int32(lp)
	}
}

// runCentroid streams audio through one device and records, per frame, both the
// pattern the device would commit to (the one telemetry reports and the bench
// reads) and the instantaneous choice the selector makes before the commit gate.
// It also reports the extremes of the centroid and its spread, which is how the
// integer scale gets chosen rather than guessed.
type device struct {
	// committed is what the device shows: the pattern after the commit gate, which
	// is the number telemetry reports and the bench reads.
	committed []int32
	// instant is the selector's answer before the gate, so a drop in agreement can
	// be attributed to the feature rather than to the gate's phasing.
	instant []int32
	// cent, avg and dev are the three numbers the choice is made from, recorded so
	// the bucket positions can be re-derived afterwards without re-running the
	// pipeline.
	cent, avg, dev []int32
}

func runCentroid(t *testing.T, data []uint16, dist func([]uint16, *int32), from, to int) device {
	t.Helper()
	var p Pipeline
	p.Init()
	win := make([]uint16, Window)
	var carry int32
	d := device{
		committed: make([]int32, to-from),
		instant:   make([]int32, to-from),
		cent:      make([]int32, to-from),
		avg:       make([]int32, to-from),
		dev:       make([]int32, to-from),
	}
	for f := from; f < to; f++ {
		copy(win, data[f*Window:(f+1)*Window])
		if dist != nil {
			dist(win, &carry)
		}
		ev := p.Process(win)
		d.committed[f-from] = ev.Pattern
		d.instant[f-from] = p.pattern()
		d.cent[f-from], d.avg[f-from], d.dev[f-from] = p.cent, p.centAvg, p.centDev
	}
	return d
}

// score compares two pattern traces the way the bench measures them: raw
// agreement, the chance-corrected version of it, the rate each side changes at,
// and how the time is shared out.
//
// chance is what two devices with these same occupancy histograms would agree on
// with no shared information whatsoever, and kappa discounts agreement down to
// that baseline. It is the difference between a real result and two devices that
// happen to favour the same colour.
func score(t *testing.T, name string, a, b []int32) (agree, kappa float64) {
	t.Helper()
	na := make([]float64, NumPatterns)
	nb := make([]float64, NumPatterns)
	same := 0
	for i := range a {
		na[a[i]]++
		nb[b[i]]++
		if a[i] == b[i] {
			same++
		}
	}
	n := float64(len(a))
	agree = float64(same) / n
	chance := 0.0
	for i := range na {
		chance += na[i] / n * nb[i] / n
	}
	if chance < 1 {
		kappa = (agree - chance) / (1 - chance)
	}
	minutes := n / 62.5 / 60
	t.Logf("%-22s agree %5.1f%%  kappa %+0.3f  (chance %4.1f%%)  changes/min L %4.1f R %4.1f  used %s|%s",
		name, 100*agree, kappa, 100*chance,
		float64(changes(a))/minutes, float64(changes(b))/minutes,
		occupancy(na), occupancy(nb))
	return agree, kappa
}

// agreeOf is the bare same-pattern fraction, for the sweep where the histograms
// are all reported next to it anyway.
func agreeOf(a, b []int32) float64 {
	same := 0
	for i := range a {
		if a[i] == b[i] {
			same++
		}
	}
	return float64(same) / float64(len(a))
}

// agreementOf is agree and kappa without the log line, for assertions.
func agreementOf(a, b []int32) (agree, kappa float64) {
	na := make([]float64, NumPatterns)
	nb := make([]float64, NumPatterns)
	same := 0
	for i := range a {
		na[a[i]]++
		nb[b[i]]++
		if a[i] == b[i] {
			same++
		}
	}
	n := float64(len(a))
	agree = float64(same) / n
	chance := 0.0
	for i := range na {
		chance += na[i] / n * nb[i] / n
	}
	return agree, (agree - chance) / (1 - chance)
}

// medianI32 returns the middle value of a slice, sorting it in place. The
// float64 version lives in quiet_test.go.
func medianI32(v []int32) int32 {
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	if len(v) == 0 {
		return 0
	}
	return v[len(v)/2]
}

// occupancyOf is occupancy from a pattern trace instead of a finished histogram.
func occupancyOf(pats []int32) string {
	n := make([]float64, NumPatterns)
	for _, p := range pats {
		n[p]++
	}
	return occupancy(n)
}

// changes counts pattern transitions, the flicker measure: a selector that is
// correct but changes twelve times a minute is not usable, and the commit gate is
// what is supposed to prevent it.
func changes(pats []int32) int {
	n := 0
	for i := 1; i < len(pats); i++ {
		if pats[i] != pats[i-1] {
			n++
		}
	}
	return n
}

// occupancy renders how the time splits across patterns, so a score riding on one
// colour being half the track is visible rather than hidden in a percentage.
func occupancy(n []float64) string {
	total := 0.0
	for _, v := range n {
		total += v
	}
	out := ""
	for i, v := range n {
		if i > 0 {
			out += " "
		}
		out += fmt.Sprintf("%d:%2.0f%%", i, 100*v/total)
	}
	return out
}

func TestPodcastCentroid(t *testing.T) {
	path := os.Getenv("PODCAST")
	if path == "" {
		t.Skip("PODCAST not set")
	}
	data := readWavPCM16(t, path)
	frames := len(data)/Window - 1
	// Skip the first 40 s: cent, centAvg and centDev all have a ~16 s time
	// constant, and scoring the settle would measure the seed, not the selector.
	from := 2500 // 40 s at 62.5 analysis frames/s
	if from >= frames {
		t.Skipf("fixture is only %.0f s", float64(frames)/62.5)
	}
	to := frames
	if e := os.Getenv("PODEND"); e != "" {
		var s float64
		fmt.Sscanf(e, "%g", &s)
		if end := int(40*62.5 + s*62.5); end > from && end < to {
			to = end
		}
	}
	minutes := float64(to-from) / 62.5 / 60
	t.Logf("fixture %d frames (%.0f s scored)", to-from, minutes*60)

	// The reference is not clean audio - it is one device among equals, with its
	// own noise, because two real earrings both have noise and neither is the
	// truth. Every row is therefore a realistic two-device mismatch.
	// The table is built by a function rather than written out once, because the noise
	// generators below carry their own PRNG state inside the closure they return. Held
	// in a literal, that state survives the first pass, so the self-check at the bottom
	// of this test - which replays the reference device and expects the same answer -
	// resumes the hiss stream several minutes in, gets different samples, and reports a
	// perfectly reproducible runner as broken. Building a fresh table hands every pass
	// its own generator at the same seed.
	type dev struct {
		name string
		dist func([]uint16, *int32)
	}
	newDevs := func() []dev {
		return []dev{
			{"ref: noise", noisy(3, 7)},
			// Independent hiss, the difference that cannot be calibrated out and was
			// measured to dominate on the bench.
			{"noise 3 vs 8", chain(noisy(3, 7), noisy(8, 99))},
			{"noise 8, other seed", noisy(8, 1234)},
			// The axis the real pair turned out to live on, kept here as a record of
			// what the harness cannot do. Read out of RAM (tools/patternstate.py) while
			// both earrings played the same track from one speaker: their centDev was
			// 512 on one and 2754 on the other, and their scaled deviations sat at
			// -1.59 and -0.97 - the same sign in 100% of samples, so the two agreed
			// about the music throughout and disagreed only about how far that put them
			// from their own reference. A steady 0.6 MAD offset is enough to straddle a
			// cut. It is not a level difference (they were 3.5 dB apart) and not a
			// response tilt: measured twice a few minutes apart the difference had
			// reversed sign, so it is where each earring sits in the room, not what it
			// is made of.
			//
			// These two rows were an attempt to produce that asymmetry by adding hiss at
			// constant level, on the theory that noise compresses the log-domain swing.
			// It does not, and the ruler printout below shows it: x1.01 and x1.01, with
			// agreement still 99.5%. White hiss spreads over every band, and the
			// zero-sum weights cancel a common addition, so hiss moves the level of the
			// spectrum and not the movement of its shape - which is the only thing
			// centDev measures. The real earrings' per-band excursions differed by 1.5x
			// while their centroid excursions differed by 5x: one of them was hearing a
			// spectrum that pumped in level while holding its shape, and no amount of
			// additive noise on a single channel can reproduce that, because holding
			// shape while pumping is exactly the common-mode that this design is built to
			// throw away.
			//
			// Left in as the negative control. If the ruler printout ever shows every
			// row near x1.00, as it does now, then no row in this table tests the
			// self-referencing divisor, and a high agreement number says nothing about
			// two physical devices.
			{"contrast mismatch 40", chain(noisy(3, 7), noisy(40, 1111))},
			{"contrast mismatch 90", chain(noisy(3, 7), noisy(90, 1212))},
			// Systematic differences: these are the ones a fixed threshold would get
			// wrong and a self-referenced one should not.
			{"6 dB quieter", chain(gain(0.5), noisy(3, 555))},
			{"12 dB quieter", chain(gain(0.25), noisy(3, 666))},
			// The condition the bench is actually in: one earring sits with the AGC
			// pinned at zero shift while the other rides two or three shifts up, which
			// is about twenty dB apart. Where this row lands decides whether the
			// remaining disagreement is the selector's or the rig's.
			{"20 dB quieter", chain(gain(0.1), noisy(3, 333))},
			{"24 dB quieter", chain(gain(0.0625), noisy(3, 444))},
			{"bass tilt +1.5x", chain(bassTilt(0.6), noisy(3, 777))},
			{"bass tilt +3x, quiet", chain(bassTilt(0.85), gain(0.5), noisy(3, 888))},
			// The laptop's own speaker, which removes the bottom: the worst case in
			// the model set, and the one the bench sees in practice.
			{"speaker shelf", chain(spk, noisy(3, 222))},
		}
	}
	devs := newDevs()

	// Every device is run once and its trace kept. The sweep below re-derives
	// buckets from those traces, and re-running the filter bank once per candidate
	// threshold would measure the same thing eight times slower.
	runs := make([]device, len(devs))
	for i, d := range devs {
		runs[i] = runCentroid(t, data, d.dist, from, to)
	}
	reference := runs[0]
	sorted := make([]int32, len(reference.cent))
	copy(sorted, reference.cent)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var spread int32
	for _, v := range reference.dev {
		if v > spread {
			spread = v
		}
	}
	// medianI32 sorts in place, so it gets a copy: the sweep below re-reads these
	// traces frame by frame and must find them in frame order.
	devCopy := make([]int32, len(reference.dev))
	copy(devCopy, reference.dev)
	t.Logf("centroid median %d, range %d .. %d; spread p50 %d, max %d, PatternMinDev %d",
		sorted[len(sorted)/2], sorted[0], sorted[len(sorted)-1],
		medianI32(devCopy), spread, PatternMinDev)

	// The check that should have been here all along. The selector divides each
	// device's deviation by that device's own spread, so a device whose spread is
	// unusual is read on a different ruler than its partner, and agreement between
	// them is no longer a test of the feature. On the real pair the two divisors came
	// out 5.4x apart over one window, and no row in this table does anything close:
	// every row measures its device between x0.53 and x1.01 of the reference, so
	// nothing here exercises the self-referencing divisor at all and a high agreement
	// number is not evidence about two physical earrings. What the pair actually
	// reports is 63% over 177 s (tools/pairscore.py, ~/psync/pair-long-nofloor) -
	// against 99.8% here for the closest analogue. Read the hardware number, not this
	// one, for how the feature behaves; read this one for whether the arithmetic
	// changed.
	//
	// A ratio far from 1 is the point of the contrast rows. A ratio near 1 across
	// every row means the table is testing arithmetic, not a pair of devices.
	t.Logf("per-device ruler (centDev is the divisor; median z is where the device sits on it):")
	refMed := medianI32(append([]int32(nil), reference.dev...))
	for i, d := range devs {
		cp := append([]int32(nil), runs[i].dev...)
		md := medianI32(cp)
		zs := make([]float64, len(runs[i].cent))
		for k := range zs {
			dv := runs[i].dev[k]
			if dv < 1 {
				dv = 1
			}
			zs[k] = float64(runs[i].cent[k]-runs[i].avg[k]) / float64(dv)
		}
		sort.Float64s(zs)
		ratio := 0.0
		if refMed > 0 {
			ratio = float64(md) / float64(refMed)
		}
		t.Logf("  %-22s centDev %7d  x%0.2f ref  median z %+0.2f",
			d.name, md, ratio, zs[len(zs)/2])
	}
	// If the spread sat below the guard for a meaningful part of the track the
	// device would be pinned to the middle pattern and the rest of this test would
	// be measuring nothing.
	if medianI32(devCopy) < 4*PatternMinDev {
		t.Errorf("median centroid spread %d is barely above the guard %d: the scale is too small to read",
			medianI32(devCopy), PatternMinDev)
	}
	for i, d := range devs[1:] {
		score(t, d.name, reference.committed, runs[i+1].committed)
		score(t, d.name+" [per-frame]", reference.instant, runs[i+1].instant)
	}

	// A regression floor on the pair that matters. On the bench the two earrings do
	// not hear the same level - one sits with the AGC pinned at zero shift while the
	// other rides two or three shifts up - which is this row. Independent noise, the
	// row above it, costs nothing at all (~100%: the ~16 s average erases it), so if
	// agreement is mediocre in the field it is the level mismatch doing it, and this
	// is the number to watch.
	for i, d := range devs[1:] {
		if d.name != "12 dB quieter" {
			continue
		}
		agree, kappa := agreementOf(reference.instant, runs[i+1].instant)
		if agree < 0.7 {
			t.Errorf("a 12 dB level difference loses agreement down to %.1f%%: the AGC is leaking into the shape",
				100*agree)
		}
		if kappa < 0.55 {
			t.Errorf("a 12 dB level difference leaves kappa at %+0.3f", kappa)
		}
	}

	// Self-check: the same device twice must agree exactly, or the trace is not a
	// function of the audio.
	again := runCentroid(t, data, newDevs()[0].dist, from, to)
	for i := range reference.committed {
		if reference.committed[i] != again.committed[i] {
			t.Fatalf("the same input gave a different pattern at frame %d: the runner is not reproducible", i)
		}
	}

	// Occupancy. The requirement from the user is that the patterns are spread
	// evenly over real material, and quartile cuts only achieve that if the
	// centroid is genuinely distributed rather than pinned: one pattern holding
	// more than half the time means the buckets are not where the music is.
	used := map[int32]int{}
	for _, p := range reference.committed {
		used[p]++
	}
	for pat := int32(0); pat < NumPatterns; pat++ {
		if float64(used[pat]) < 0.08*float64(to-from) {
			t.Errorf("pattern %d holds only %.1f%% of the time: bucket %d is nearly unreachable",
				pat, 100*float64(used[pat])/float64(to-from), pat)
		}
	}
	// Flicker. The commit gate is meant to keep the visible change rate low; above
	// about one change every five seconds the animation never gets to say anything.
	if c := float64(changes(reference.committed)) / minutes; c > 12 {
		t.Errorf("reference changes pattern %.1f times a minute", c)
	}
}
