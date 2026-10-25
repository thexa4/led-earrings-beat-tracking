package syncdsp

// Which scalar to read the pattern from, chosen by measurement.
//
// The pipeline can produce a colour from any one number summarising the
// spectrum, and the choice is not obvious. This records the smoothed spectrum
// from the shipped front end and scores candidate summaries against each other,
// on the two things that matter: do two devices that hear the same music at
// different levels pick the same colour, and does the choice use all the colours
// it is given.
//
// Because the candidates are scored from the recorded spectrum rather than inside
// the pipeline, one pass over the audio evaluates every candidate, which is what
// makes the comparison wide enough to be worth anything.
//
//	PODCAST=~/psync/edm280.wav go test -count=1 -v -run TestCentroidFeature ./syncdsp/
//
// The candidates. All are computed from the same smoothed spectrum the hash reads.
//
//	ramp      sum(w_b * x_b), w = -7..+7. Linear in frequency, so a common-mode
//	          gain cancels exactly (the weights sum to zero) and the result is a
//	          weighted mean of where the energy sits.
//	centroid  the textbook spectral centroid: sum(b * e_b) / sum(e_b) with e_b the
//	          band energy above the frame's own floor. A ratio, so it needs no
//	          sum-to-zero trick to reject gain, and a band sitting at the floor
//	          contributes nothing, so empty bands cannot add noise.
//	reliable  the ramp with each band weighted by how far it stands above the
//	          floor, and the product re-centred so the weights still sum to zero.
//	          Keeps the ramp's linearity and drops the bands that are only
//	          reporting quantisation noise.
//	interior  the ramp over the four middle bands only, dropping both open-ended
//	          rungs, which is the band set the previous design used for a
//	          different reason.

import (
	"fmt"
	"math"
	"os"
	"sort"
	"testing"
)

// runTrace is a recorded run: the smoothed spectrum per frame, plus the pattern the
// device actually committed to.
type runTrace struct {
	smooth    [][NBands]int32
	committed []int32
}

// recordFeature runs audio through the shipped front end and records the
// smoothed spectrum, which is the input every candidate summary reads.
func recordFeature(t *testing.T, data []uint16, dist func([]uint16, *int32), from, to int) runTrace {
	t.Helper()
	var p Pipeline
	p.Init()
	win := make([]uint16, Window)
	var carry int32
	tr := runTrace{}
	for f := from; f < to; f++ {
		copy(win, data[f*Window:(f+1)*Window])
		if dist != nil {
			dist(win, &carry)
		}
		ev := p.Process(win)
		if f >= from {
			tr.smooth = append(tr.smooth, p.smoothSnap)
			tr.committed = append(tr.committed, ev.Pattern)
		}
	}
	return tr
}

// feature is a summary of one frame's spectrum.
type feature func(x *[NBands]int32) int32

// floorOf is the frame's own floor: the pin every band is floored to, which is
// what separates the bands carrying music from the bands reporting nothing.
func floorOf(x *[NBands]int32) int32 {
	max := x[0]
	for _, v := range x[1:] {
		if v > max {
			max = v
		}
	}
	return max - RelFloorQ8
}

func rampFeature(x *[NBands]int32) int32 {
	c := int32(0)
	for i, v := range x {
		c += v * PatternWeights[i]
	}
	return c * PatternCentroidScale
}

func centroidFeature(x *[NBands]int32) int32 {
	floor := floorOf(x)
	num, den := int64(0), int64(0)
	for i, v := range x {
		e := int64(v - floor)
		if e <= 0 {
			continue
		}
		num += e * int64(i)
		den += e
	}
	if den == 0 {
		return 0
	}
	// Q8 of a band index in 0..7, scaled to the same order of magnitude as the
	// ramp candidate so the guards and shifts below apply equally to all of them.
	return int32(num*256/den) * 64
}

func reliableFeature(x *[NBands]int32) int32 {
	floor := floorOf(x)
	var w [NBands]int64
	mean := int64(0)
	for i := range x {
		e := int64(x[i] - floor)
		if e < 0 {
			e = 0
		}
		w[i] = e * int64(PatternWeights[i])
		mean += w[i]
	}
	mean /= NBands
	c := int64(0)
	for i := range x {
		c += (w[i] - mean) * int64(x[i])
	}
	return int32(c / 256)
}

func interiorFeature(x *[NBands]int32) int32 {
	c := int32(0)
	for i, v := range x {
		if i == 0 || i == NBands-1 {
			continue
		}
		c += v * PatternWeights[i]
	}
	return c * PatternCentroidScale
}

// madCutQ8 is the retired ruler, kept here so the comparison stays reproducible:
// the shipped selector now tracks the quartiles (see quantile below and
// PatternCutStepShift), and what replaced it was cuts at a fixed multiple of the
// spread - 216/256 = 0.844 mean absolute deviations, a Gaussian's quartiles.
const madCutQ8 = 216

// bucket is the retired selector: a 16 s average of the feature, its own reference
// level and spread, and cuts at a fixed multiple of that spread. Shared by every
// candidate, so what is compared is the feature and nothing else.
type bucket struct {
	shift int32
	scale int32
	init  bool
	ema   int32
	avg   int32
	dev   int32
}

func (b *bucket) step(v int32) int32 {
	half := int32(1 << (b.shift - 1))
	if !b.init {
		b.init = true
		b.ema, b.avg, b.dev = v, v, 0
		return NumPatterns / 2
	}
	b.ema += (v - b.ema + half) >> b.shift
	gap := b.ema - b.avg
	abs := gap
	if abs < 0 {
		abs = -gap
	}
	b.dev += (abs - b.dev + half) >> b.shift
	b.avg += (gap + half) >> b.shift
	if b.dev < PatternMinDev*b.scale {
		return NumPatterns / 2
	}
	d := gap
	if lim := 2 * b.dev; d > lim {
		d = lim
	} else if d < -lim {
		d = -lim
	}
	z := d * 256 / b.dev
	pat := int32(0)
	for i := int32(0); i < NumPatterns-1; i++ {
		if z > (2*i-(NumPatterns-2))*madCutQ8/2 {
			pat = int32(i) + 1
		}
	}
	return pat
}

// patternAt turns a recorded spectrum into a pattern trace.
func patternAt(tr runTrace, f feature) []int32 {
	b := bucket{shift: PatternCentroidShift, scale: 1}
	out := make([]int32, len(tr.smooth))
	for i := range tr.smooth {
		out[i] = b.step(f(&tr.smooth[i]))
	}
	return out
}

// quantile buckets by running percentile estimates instead of a multiple of the
// spread. The shipped ruler is one mean absolute deviation, which puts the cuts at
// a Gaussian's quartiles; measured over real material that hands the middle two
// buckets 71% of the time, because the centroid's distribution is not Gaussian and
// no rescaling fixes it - the cuts have to be *placed*, not spaced.
//
// Each estimate converges to the fraction it targets: moved up by (1-p) units when
// the sample is above it and down by p when below. The unit is a fraction of the
// spread, so convergence takes about the same time whatever the signal's scale,
// and the spread is already being tracked.
//
// The state is per-device, which is the thing that broke the hysteresis margin -
// so the distinction matters: this converges to a property of the *music* (where
// its quartiles lie), which both earrings are listening to, rather than to what
// this device happened to be showing last. It is slower to settle, and Pattern
// WarmupFrames is what keeps that settling out of the comparison.
type quantile struct {
	shift int32
	init  bool
	q     [NumPatterns - 1]int32
}

func (q *quantile) step(v int32, dev int32) int32 {
	if !q.init {
		q.init = true
		for i := range q.q {
			q.q[i] = v
		}
		return NumPatterns / 2
	}
	unit := dev >> q.shift
	if unit < 1 {
		unit = 1
	}
	for i := range q.q {
		// Targets 1/4, 1/2, 3/4 for the three cuts: up by p, down by 1-p, so that the
		// equilibrium sits at F(q) = p. See cutQuartiles - the inverse is a silent
		// failure that collapses the three estimates onto each other.
		up, down := [][2]int32{{1, 3}, {1, 1}, {3, 1}}[i][0], [][2]int32{{1, 3}, {1, 1}, {3, 1}}[i][1]
		if v > q.q[i] {
			q.q[i] += up * unit
		} else {
			q.q[i] -= down * unit
		}
	}
	// The three estimates are independent, so they can cross on a lumpy
	// distribution; crossed cuts would make the pattern non-monotone in the
	// feature, which is worse than slightly uneven buckets.
	for i := 1; i < len(q.q); i++ {
		if q.q[i] < q.q[i-1] {
			q.q[i] = q.q[i-1]
		}
	}
	pat := int32(0)
	for i := range q.q {
		if v > q.q[i] {
			pat = int32(i) + 1
		}
	}
	return pat
}

// quantileAt runs the same feature through percentile cuts, matching the shipped
// selector exactly: the cuts and the comparison both read the smoothed feature, not
// the frame's own value, and the spread sizes the step. The bucket above is reused
// for that average and that spread - only its fixed-spacing answer is discarded.
func quantileAt(tr runTrace, f feature, shift int32) []int32 {
	q := quantile{shift: shift}
	dev := bucket{shift: PatternCentroidShift, scale: 1}
	out := make([]int32, len(tr.smooth))
	for i := range tr.smooth {
		v := f(&tr.smooth[i])
		dev.step(v)
		out[i] = q.step(dev.ema, dev.dev)
	}
	return out
}

func TestCentroidFeature(t *testing.T) {
	cands := []struct {
		name string
		f    feature
	}{
		{"ramp", rampFeature},
		{"centroid", centroidFeature},
		{"reliable", reliableFeature},
		{"interior", interiorFeature},
	}

	// Two independent ways of making the two devices differ. The synthetic patch
	// has a known spectrum and no noise, so a level change there is a clean
	// measurement of the arithmetic; real material decides whether anything found
	// there is worth having.
	t.Run("synth", func(t *testing.T) {
		const frames = 2600
		trs := make([]runTrace, len(levelDB))
		for i, db := range levelDB {
			in := newInst(attenuate(db), false)
			win := make([]uint16, Window)
			trs[i] = runTrace{}
			for f := WarmupFrames + PatternWarmupFrames; f < frames; f++ {
				Synth(int64(f*Window), win)
				attenuate(db)(win, nil)
				in.p.Process(win)
				trs[i].smooth = append(trs[i].smooth, in.p.smoothSnap)
			}
		}
		// The pairs the bench is actually in, plus the extremes as a floor.
		pairs := [][2]int{{0, 1}, {0, 2}, {0, 3}, {1, 3}, {0, 5}, {2, 5}, {4, 5}}
		for _, c := range cands {
			pats := make([][]int32, len(trs))
			for i := range trs {
				pats[i] = patternAt(trs[i], c.f)
			}
			line := fmt.Sprintf("%-9s", c.name)
			for _, p := range pairs {
				line += fmt.Sprintf(" %d/%ddB %4.1f%%", levelDB[p[0]], levelDB[p[1]], 100*agreeOf(pats[p[0]], pats[p[1]]))
			}
			_, k := agreementOf(pats[0], pats[5])
			line += fmt.Sprintf("  kappa(0/36) %+0.3f", k)
			t.Log(line)
		}
	})

	if os.Getenv("PODCAST") == "" {
		t.Skip("PODCAST not set: synthetic only")
	}
	data := readWavPCM16(t, os.Getenv("PODCAST"))
	from, to := 2500, len(data)/Window-1
	t.Run("podcast", func(t *testing.T) {
		devs := []struct {
			name string
			dist func([]uint16, *int32)
		}{
			{"ref", noisy(3, 7)},
			{"20dB", chain(gain(0.1), noisy(3, 333))},
			{"shelf", chain(spk, noisy(3, 222))},
			{"tilt", chain(bassTilt(0.85), gain(0.5), noisy(3, 888))},
		}
		trs := make([]runTrace, len(devs))
		for i, d := range devs {
			trs[i] = recordFeature(t, data, d.dist, from, to)
		}
		for _, c := range cands {
			ref := patternAt(trs[0], c.f)
			line := fmt.Sprintf("  mad %-9s", c.name)
			for i := 1; i < len(trs); i++ {
				pat := patternAt(trs[i], c.f)
				_, k := agreementOf(ref, pat)
				line += fmt.Sprintf(" %s %4.1f%%/%+0.2f", devs[i].name, 100*agreeOf(ref, pat), k)
			}
			line += fmt.Sprintf("  %s chg/min %4.1f", occupancyOf(ref), float64(changes(ref))/float64(to-from)*62.5*60)
			t.Log(line)
			qref := quantileAt(trs[0], c.f, 9)
			line = fmt.Sprintf("  pct %-9s", c.name)
			for i := 1; i < len(trs); i++ {
				pat := quantileAt(trs[i], c.f, 9)
				_, k := agreementOf(qref, pat)
				line += fmt.Sprintf(" %s %4.1f%%/%+0.2f", devs[i].name, 100*agreeOf(qref, pat), k)
			}
			line += fmt.Sprintf("  %s chg/min %4.1f", occupancyOf(qref), float64(changes(qref))/float64(to-from)*62.5*60)
			t.Log(line)
		}

		// How long the cuts take to settle, which is the cost of measuring them
		// rather than assuming them. The step is a fraction of the spread, so it is
		// the fraction that decides both how quickly the buckets find the music and how
		// much they jitter afterwards: too small and they are still bunched near the
		// middle of the track and the pattern flips on nothing, too large and they chase
		// the centroid instead of its distribution.
		//
		// Scored on the last 120 s only, so what is measured is the settled behaviour
		// and not the approach to it, and paired with the change rate, which is how a
		// cut that jitters across the centroid shows up.
		const tail = 120 * 62
		if to-from > tail {
			t.Logf("step sweep (%d s tail, ramp feature)", tail/62)
			for _, sh := range []int32{4, 5, 6, 7, 8, 9, 10, 11, 12} {
				full := quantileAt(trs[0], rampFeature, sh)
				ref := full[len(full)-tail:]
				q1 := agreeOf(ref, quantileAt(trs[1], rampFeature, sh)[len(full)-tail:])
				q2 := agreeOf(ref, quantileAt(trs[2], rampFeature, sh)[len(full)-tail:])
				t.Logf("  step 1/%d of spread: %s chg/min %5.1f  agree 20dB %4.1f%%  shelf %4.1f%%",
					1<<sh, occupancyOf(ref), float64(changes(ref))/float64(tail)*62.5*60, 100*q1, 100*q2)
			}
		}
	})
}

// medianFeature is unused guard: keeps sort imported if the sweep above changes.
var _ = sort.Ints
var _ = math.Abs
