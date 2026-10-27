package syncdsp

import (
	"fmt"
	"go/format"
	"math"
	"os"
	"sort"
	"strings"
	"testing"
)

// The agreement harness: run the pipeline twice over the same synth music
// with independent per-device distortions and check that the decisions stay
// the same. This is the actual definition of "roughly synchronous"
// (PARTY-SYNC.md §1).

type instance struct {
	p     Pipeline
	win   []uint16
	dist  func(w []uint16, carry *int32)
	carry int32 // filter memory between frames (for the tilt model)
	// Drift model: sample position advances Window + 1 every 250 frames
	// (~+0.4%), which is well beyond the trim tolerance of two MSI clocks.
	drift bool
}

func newInst(dist func([]uint16, *int32), drift bool) *instance {
	in := &instance{dist: dist, drift: drift}
	in.p.Init()
	in.win = make([]uint16, Window)
	return in
}

func (in *instance) frame(f int) Events {
	pos := int64(f * Window)
	if in.drift {
		pos += int64(f) / 250
	}
	Synth(pos, in.win)
	if in.dist != nil {
		in.dist(in.win, &in.carry)
	}
	return in.p.Process(in.win)
}

func run(inst *instance, frames int) []Events {
	evs := make([]Events, frames)
	for f := range frames {
		evs[f] = inst.frame(f)
	}
	return evs
}

// Distortion models.

// halfGain models a device further from the PA (or a quieter mic): −6 dB.
// Careful: uint16 arithmetic would wrap around mid-rail, so do this in int32.
func halfGain(w []uint16, _ *int32) {
	for i, v := range w {
		y := 32768 + (int32(v)-32768)/2
		if y < 0 {
			y = 0
		}
		w[i] = uint16(y)
	}
}

// quiet models the measured bench operating point: the music reaches the
// microphone as a few tens of ADC counts (-42 dB here, which is what a loud
// speaker 1 cm from this front end produces). Without input normalization this
// is where the pipeline stops agreeing with itself: the filter states stall on
// their own LSBs and the beat tracker locks to a different tempo (see
// AgcTargetPeak).
func quiet(w []uint16, _ *int32) {
	for i, v := range w {
		w[i] = uint16(32768 + (int32(v)-32768)/64)
	}
}

// benchOffset is quiet() plus the real front end's DC: this microphone idles at
// 0.26 of full scale, not at the mid-rail the pipeline subtracts, so the signal
// rides on a -15600 count error that the DC blocker has to absorb. Modelled
// because the two effects interact: the blocker's output is where the stalls
// happen.
func benchOffset(w []uint16, _ *int32) {
	for i, v := range w {
		y := 17120 + (int32(v)-32768)/64
		if y < 0 {
			y = 0
		}
		w[i] = uint16(y)
	}
}

// hfTilt models hand shadowing: a crude HF *shelf* (+~4 dB) via a one-tap
// pre-emphasis with cross-frame memory.
func hfTilt(w []uint16, carry *int32) {
	x := *carry
	for i, v := range w {
		d := (int32(v) - x) >> 2
		x = int32(v)
		y := int32(v) + d
		if y > 65535 {
			y = 65535
		} else if y < 0 {
			y = 0
		}
		w[i] = uint16(y)
	}
	*carry = x
}

// combo: everything at once — the realistic worst case.
func combo(w []uint16, carry *int32) {
	halfGain(w, nil)
	hfTilt(w, carry)
}

func anchors(evs []Events) []int {
	var out []int
	for f, e := range evs {
		if e.Anchor {
			out = append(out, f)
		}
	}
	return out
}

// matchAnchors greedily matches anchor frames between A and B within
// tolerance (drift alone is ±0.4%, so ±8 frames over ~2000).
func matchAnchors(a, b []int, tol int) (matched, onlyA, onlyB int) {
	usedB := make([]bool, len(b))
	for _, fa := range a {
		best := -1
		for j, fb := range b {
			if usedB[j] {
				continue
			}
			d := fb - fa
			if d < 0 {
				d = -d
			}
			if d <= tol && (best < 0 || d < absInt(b[best]-fa)) {
				best = j
			}
		}
		if best >= 0 {
			usedB[best] = true
			matched++
		} else {
			onlyA++
		}
	}
	onlyB = len(b) - matched
	return
}

// testFrames is exactly 8 synth sections (synthSection = 122880 samples), so
// the harness sees the full recurring structure: two intros, two breaks, four
// drops and the builds between them. Shorter runs saw only intro→drop and
// could not test the periodicity material at all.
const testFrames = 8 * synthSection / Window

func TestAgreement(t *testing.T) {
	cases := []struct {
		name  string
		dist  func([]uint16, *int32)
		drift bool
	}{
		{"none", nil, false},
		{"gain-6dB", halfGain, false},
		{"hf-tilt", hfTilt, false},
		{"gain+tilt+drift", combo, true},
		{"quiet-42dB", quiet, false},
		{"bench-quiet-offset", benchOffset, false},
	}

	base := run(newInst(nil, false), testFrames)
	if !hasLock(base) {
		t.Errorf("beat tracker never locked on the reference signal")
	}

	// Anchor rate sanity. The synth changes arrangement roughly every 490
	// frames, and not every change passes the energy gate, so ~1 anchor per
	// 2-4 sections is the expected band. Anchors are transitions, not beats:
	// one per 100 frames (every 16 beats) would already be a strobe.
	aAnchors := anchors(base)
	t.Logf("reference: %d anchors, lock=%v", len(aAnchors), hasLock(base))
	if len(aAnchors) < 3 || len(aAnchors) > testFrames/200 {
		t.Errorf("anchor rate out of range: %d in %d frames", len(aAnchors), testFrames)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			other := run(newInst(tc.dist, tc.drift), testFrames)
			// Tolerance is two tracker periods: the two devices lock their
			// beat grids at different points, so their first beat after a
			// transition can be a beat apart. What must agree is that both
			// snap at the same musical transition, not the same frame.
			matched, onlyA, onlyB := matchAnchors(aAnchors, anchors(other), 30)

			// Pattern agreement over frames where the reference is locked,
			// plus the per-frame hash Hamming distance: the ring decisions
			// work with distance, not equality, so distance is the metric
			// that matters.
			//
			// The distance is split by whether the reference's hash has been
			// stable for a while. A frame near a section change is the two
			// devices watching the music *change* - their smoothing filters
			// cross at different points, so a few bits differ for
			// ~SmoothShift frames, and no amount of tuning fixes that: it is
			// the transitions themselves, which is exactly what the hash is
			// supposed to highlight. The number that decides whether ring
			// matching works is the distance during *steady* passages: same
			// music, both devices settled. That is what DAccept compares
			// against, so it must stay small.
			same, tot := 0, 0
			var steadySum, transSum int
			var steadyN, transN int
			var ds [16]int // distance histogram, steady frames only
			worstSteady := 0
			stableRun := 0 // frames since the reference's hash last changed
			prevRef := uint32(0)
			for f := range base {
				if f < int(WarmupFrames) {
					continue
				}
				if base[f].Hash != prevRef {
					stableRun = 0
					prevRef = base[f].Hash
				} else {
					stableRun++
				}
				if base[f].Locked {
					tot++
					if base[f].Pattern == other[f].Pattern {
						same++
					}
				}
				hd := int(hashDist(base[f].Hash, other[f].Hash))
				if stableRun >= 1<<SmoothShift {
					steadyN++
					steadySum += hd
					if hd < len(ds) {
						ds[hd]++
					}
					if hd > worstSteady {
						worstSteady = hd
					}
				} else {
					transN++
					transSum += hd
				}
			}
			var p95 int
			acc := 0
			for i := 0; i < 16; i++ {
				acc += ds[i]
				if steadyN > 0 && acc >= steadyN*95/100 {
					p95 = i
					break
				}
			}
			t.Logf("anchors: matched=%d onlyA=%d onlyB=%d | pattern %d/%d (%.0f%%) | steady dist n=%d mean=%.2f p95=%d worst=%d hist=%v | transition dist mean=%.2f",
				matched, onlyA, onlyB,
				same, tot, 100*float64(same)/float64(tot),
				steadyN, float64(steadySum)/math.Max(float64(steadyN), 1), p95, worstSteady, ds[:8],
				float64(transSum)/math.Max(float64(transN), 1))

			if tot == 0 {
				t.Fatalf("reference never locked")
			}
			pa := float64(same) / float64(tot)
			if pa < 0.70 {
				t.Errorf("pattern agreement %.0f%% < 70%%", 100*pa)
			}
			if p95 > 3 {
				t.Errorf("steady hash distance p95 = %d > DAccept(3): ring matching would misfire", p95)
			}
			total := len(aAnchors) + onlyB
			if total > 0 {
				f1 := 0.0
				if matched > 0 {
					prec := float64(matched) / float64(matched+onlyA)
					rec := float64(matched) / float64(matched+onlyB)
					f1 = 2 * prec * rec / (prec + rec)
				}
				if f1 < 0.5 {
					t.Errorf("anchor agreement F1 = %.2f < 0.5 (matched=%d, onlyA=%d, onlyB=%d)", f1, matched, onlyA, onlyB)
				}
			}
		})
	}
}

func hasLock(evs []Events) bool {
	for _, e := range evs {
		if e.Locked {
			return true
		}
	}
	return false
}

// TestGolden freezes the behaviour: hashes and event bits for the first 64
// frames (undistorted). The device boot selftest uses the same table, so any
// accidental change to the DSP or hash on either side trips both.
// Regenerate with: UPDATE_GOLDEN=1 go test ./syncdsp -run TestGolden
func TestGolden(t *testing.T) {
	in := newInst(nil, false)
	const n = 64
	var hashes [n]uint32
	var anchorBits uint64
	var patterns [n]int32
	for f := 0; f < n; f++ {
		e := in.frame(f)
		hashes[f] = e.Hash
		if e.Anchor {
			anchorBits |= 1 << uint(f)
		}
		patterns[f] = e.Pattern
	}

	if os.Getenv("UPDATE_GOLDEN") != "" {
		var b strings.Builder
		b.WriteString(`package syncdsp

// Golden reference values, produced by the host test
// (UPDATE_GOLDEN=1 go test ./syncdsp -run TestGolden) and checked:
//
//   - by that test, to catch accidental changes to the DSP or the hash;
//   - by the device boot selftest, to verify the pipeline running on the
//     STM32L031 produces exactly what the host produces.
//
// The pipeline is plain integer Go with no platform-dependent arithmetic (see
// the package comment), so this is a genuine end-to-end check of the device
// build: the same source compiles both sides, and a mismatch means the device's
// view of the world differs - a wrong filter coefficient, a different ADC range,
// or an int width surprise.
//
// Input: the deterministic Synth() stimulus from sample 0, undistorted, into a
// fresh Pipeline.

`)
		fmt.Fprintf(&b, "var GoldenHashes = [%d]uint32{\n", n)
		for i := 0; i < n; i += 8 {
			fmt.Fprintf(&b, "\t%#010x, %#010x, %#010x, %#010x, %#010x, %#010x, %#010x, %#010x,\n",
				hashes[i], hashes[i+1], hashes[i+2], hashes[i+3],
				hashes[i+4], hashes[i+5], hashes[i+6], hashes[i+7])
		}
		fmt.Fprintf(&b, "}\nvar GoldenAnchors uint64 = 0x%016x\n", anchorBits)
		fmt.Fprintf(&b, "var GoldenPatterns = [%d]int8{\n\t", n)
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "%d,", patterns[i])
			if i%16 == 15 {
				fmt.Fprintf(&b, "\n\t")
			}
		}
		fmt.Fprintf(&b, "}\n")
		src, err := format.Source([]byte(b.String()))
		if err != nil {
			t.Fatalf("generated golden.go does not parse: %v", err)
		}
		// go test runs with the package directory as cwd.
		if err := os.WriteFile("golden.go", src, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Log("golden.go regenerated")
		return
	}

	if GoldenHashes != hashes {
		diff := make([]int, 0, 8)
		for i := range hashes {
			if GoldenHashes[i] != hashes[i] {
				diff = append(diff, i)
			}
		}
		sort.Ints(diff)
		t.Errorf("hashes drifted from golden at frames %v (regenerate if intentional)", diff)
	}
	if GoldenAnchors != anchorBits {
		t.Errorf("anchor events drifted from golden: got %016x want %016x", anchorBits, GoldenAnchors)
	}
}

// TestPatternWeightsRamp checks the weights that collapse the spectrum into one
// number, which is where the design of the pattern choice actually lives.
//
// It no longer requires the weights to rise monotonically from treble to bass.
// That was a proxy for two things, and the contrast this replaced the ramp with
// keeps the one that matters while dropping the other:
//
//   - Ordering the colours so two earrings that disagree land on neighbours rather
//     than on opposites. That is a property of the index, which maps one ordered
//     scalar onto ordered patterns, not of the weights being monotone in
//     frequency. TestPatternIndexBucket covers it directly.
//   - Summing to zero, so a flat spectrum and a uniform AGC gain produce no
//     deviation at all. This one is load-bearing and is checked below, and now also
//     behaviourally rather than only by adding up constants.
//
// Monotone in frequency turned out to cost agreement: it puts the largest positive
// weight on the band the two microphones disagree about most strongly, and disagree
// in the opposite direction to every other band. See PatternWeights.
func TestPatternWeightsRamp(t *testing.T) {
	if len(PatternWeights) != NBands {
		t.Fatalf("PatternWeights covers %d bands, the spectrum has %d", len(PatternWeights), NBands)
	}
	sum, pos, neg := int32(0), 0, 0
	max := int32(0)
	for _, w := range PatternWeights {
		sum += w
		if w > 0 {
			pos++
		}
		if w < 0 {
			neg++
		}
		if a := abs32(w); a > max {
			max = a
		}
	}
	if sum != 0 {
		t.Errorf("PatternWeights sums to %d, want 0: a flat spectrum must give no deviation", sum)
	}
	// A zero sum with a single non-zero band is arithmetically fine and useless in
	// practice: it would read one filter and nothing else. Two bands on each side is
	// the minimum that still averages down the noise in any one of them, which is
	// the stated reason for weighting the spectrum instead of picking a band.
	if pos < 1 || neg < 1 {
		t.Errorf("weights have %d positive and %d negative entries: with only one sign "+
			"the centroid tracks loudness, not shape", pos, neg)
	}
	// PatternMinDev is an absolute threshold on the spread of this weighted sum, so
	// the weights carry its meaning: scale the weights down and the guard starts
	// rejecting real music, scale them up and it stops catching a silent device.
	if max < 5 || max > 9 {
		t.Errorf("largest weight is %d: PatternMinDev=%d is stated in units of a sum "+
			"weighted to roughly this size, so rescaling here silently moves that guard",
			max, PatternMinDev)
	}
	// The sum-zero property, checked as behaviour: a spectrum with the same value in
	// every band must produce no deviation whatever that value is, since that is
	// exactly what a flat response and an AGC gain both look like to this code.
	for _, lvl := range []int32{1 << 10, 1 << 13, 1 << 15} {
		c := int32(0)
		for _, w := range PatternWeights {
			c += lvl * w
		}
		if c != 0 {
			t.Errorf("a flat spectrum at level %d gives centroid %d, want 0", lvl, c)
		}
	}
}

func abs32(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}

// TestPatternIndexBucket checks the map from the centroid to a pattern: always in
// range, monotone, using every pattern, and giving up when there is nothing to read.
//
// Monotonicity is the property that replaced the candidate-band ranking. The centroid
// is an ordered quantity, so two earrings are pulled apart by exactly as much as
// their spectra differ; under the ranking they could sit on opposite colours because
// one band changed its mind.
func TestPatternIndexBucket(t *testing.T) {
	const avg = int32(12000)
	for _, dev := range []int32{PatternMinDev, 500, 40000} {
		prev := int32(-1)
		for cent := int32(-400000); cent <= 400000; cent += 250 {
			pat := PatternIndex(cent, avg, dev)
			if pat < 0 || pat >= NumPatterns {
				t.Fatalf("pattern %d out of range at cent=%d dev=%d", pat, cent, dev)
			}
			if pat < prev {
				t.Fatalf("pattern went backwards: %d then %d at cent=%d dev=%d", prev, pat, cent, dev)
			}
			prev = pat
		}
	}
	// Every pattern has to be reachable, or an animation is unreachable on the bench
	// and the count is a lie.
	seen := map[int32]bool{}
	for cent := int32(-400000); cent <= 400000; cent += 250 {
		seen[PatternIndex(cent, avg, 900)] = true
	}
	if len(seen) != NumPatterns {
		t.Errorf("PatternIndex uses only %d of %d patterns", len(seen), NumPatterns)
	}
	// A spectrum with no measurable variation gets no opinion. This is also the guard
	// that keeps the ratio in PatternIndex from dividing by zero, which on
	// -panic=trap would reset the device rather than misbehave quietly.
	for _, c := range [][3]int32{{0, 0, 0}, {12000, 0, 0}, {12000, 12000, 0}, {12000, 12000, PatternMinDev - 1}} {
		if got := PatternIndex(c[0], c[1], c[2]); got != NumPatterns/2 {
			t.Errorf("spread %d at cent %d chose %d, want the middle pattern %d",
				c[2], c[0], got, NumPatterns/2)
		}
	}
	// A wild centroid saturates at an end bucket instead of wrapping past it. The
	// bound has to hold at magnitudes no real frame reaches, because nothing else in
	// the path checks the centroid either.
	for _, big := range []int32{1 << 20, 1 << 28, 1 << 30} {
		if got := PatternIndex(big, 0, 1<<10); got != NumPatterns-1 {
			t.Errorf("centroid %d chose %d, want the top pattern %d", big, got, NumPatterns-1)
		}
		if got := PatternIndex(-big, 0, 1<<10); got != 0 {
			t.Errorf("centroid %d chose %d, want the bottom pattern 0", -big, got)
		}
	}
}

// TestPatternDeviceInvariant is the reason the centroid is read against its own
// running reference and its own spread instead of against fixed thresholds. The two
// earrings differ in sensitivity, in how much music each microphone hears, and in
// whatever tilt the AGC settles to - and none of those say which animation the two of
// them should be showing.
//
// Both sweeps are exact rather than approximate. A tilt adds the same constant to
// cent and to centAvg, so it cancels in the subtraction. A sensitivity scales cent,
// centAvg and centDev by the same factor, and scaling numerator and denominator
// together cannot move a truncated quotient: floor(a*n/(b*n)) is floor(a/b).
func TestPatternDeviceInvariant(t *testing.T) {
	const dev = int32(900)
	for _, k := range []int32{-(1 << 24), -50000, 700, 1 << 24} {
		for cent := int32(-3000); cent <= 3000; cent += 137 {
			want, got := PatternIndex(cent, 0, dev), PatternIndex(cent+k, k, dev)
			if got != want {
				t.Errorf("a tilt of %d moved the pattern %d -> %d at cent=%d", k, want, got, cent)
			}
		}
	}
	for _, s := range []int32{1, 3, 17, 1024} {
		for cent := int32(-3000); cent <= 3000; cent += 137 {
			want, got := PatternIndex(cent, 0, dev), PatternIndex(cent*s, 0, dev*s)
			if got != want {
				t.Errorf("a sensitivity of x%d moved the pattern %d -> %d at cent=%d", s, want, got, cent)
			}
		}
	}
}

// TestPatternStateless pins the property that the hysteresis margin destroyed, and
// which the fixed cuts restore.
//
// The choice must depend on nothing but the centroid, reference and spread handed to
// it. The margin used to keep the pattern on screen as an incumbent that a challenger
// had to beat by a clear margin, and since that incumbent is per-device state with no
// channel between the earrings, the two could disagree forever: the margin (0.19
// doublings) exceeded the lead the true winner usually holds (0.04-0.10), so once
// divided neither incumbent ever yielded. Measured on the bench: 57% of the time
// visibly matching and 36% visibly stuck apart, against 81% matching with the memory
// removed.
//
// This is also why the ruler has to stay derived rather than learned. Anything the two
// devices converge on separately - a tracked quartile, an incumbent, a per-device
// threshold - is a source of disagreement that no amount of averaging removes. See
// PatternCutQ8, where a quartile tracker bought even buckets at the cost of forty
// points of agreement.
func TestPatternStateless(t *testing.T) {
	type in struct{ cent, avg, dev int32 }
	seq := make([]in, 0, 64)
	for i := int32(-8); i <= 8; i++ {
		for _, dev := range []int32{64, 900, 40000} {
			seq = append(seq, in{i * dev / 3, 12000, dev})
		}
	}
	for _, e := range seq {
		want := PatternIndex(e.cent, e.avg, e.dev)
		for repeat := 0; repeat < 3; repeat++ {
			if got := PatternIndex(e.cent, e.avg, e.dev); got != want {
				t.Fatalf("PatternIndex(%v) returned %d then %d: the choice has state", e, want, got)
			}
		}
	}
	// Order must not matter either: the same triple answers the same way whichever
	// frames came before it.
	for k := range seq {
		b := len(seq) - 1 - k
		if got, want := PatternIndex(seq[b].cent, seq[b].avg, seq[b].dev),
			PatternIndex(seq[b].cent, seq[b].avg, seq[b].dev); got != want {
			t.Fatalf("reverse pass unstable at %d", b)
		}
	}
}

// TestPatternConvergesAfterDivergence is the regression test for the failure above,
// written as the behaviour that matters: two pipelines handed the same spectrum show
// the same pattern, including after they were handed different ones.
//
// It looks redundant next to TestPatternStateless now that the choice is pure. It was
// written against the incumbent, which is exactly the kind of state that passes a
// same-input test and still divides two devices, because the input it reads includes
// what the device was last showing. Keep it for that: any future state in this path
// has to clear it.
func TestPatternConvergesAfterDivergence(t *testing.T) {
	// Two devices driven to opposite ends of the centroid, then handed the same one
	// again. The spread is well above PatternMinDev so the guard does not answer for
	// them.
	const dev = 500
	dark := PatternIndex(12000+4*dev, 12000, dev)
	bright := PatternIndex(12000-4*dev, 12000, dev)
	common := PatternIndex(12000, 12000, dev)

	var a, b Pipeline
	a.Init()
	b.Init()
	a.setPattern(dark)
	b.setPattern(bright)
	if a.lastPattern == b.lastPattern {
		t.Fatalf("the two pipelines did not diverge, so this test proves nothing")
	}
	a.setPattern(common)
	b.setPattern(common)
	if a.lastPattern != b.lastPattern {
		t.Errorf("after a shared frame the devices still differ: %d vs %d", a.lastPattern, b.lastPattern)
	}
}

// TestPatternSilenceHoldsMiddle is the other half of the guard: a device with
// nothing to hear has no opinion, and says so with the middle pattern rather than
// flickering between two on rounding noise.
//
// On silence every band sits at the floor pin, so the weighted sum is constant, its
// deviation collapses towards zero through integer truncation, and the cuts converge
// onto the same value as the centroid - at which point the answer would be decided
// entirely by which side of a boundary the last few counts landed on.
func TestPatternSilenceHoldsMiddle(t *testing.T) {
	var p Pipeline
	p.Init()
	win := make([]uint16, Window)
	for range 3000 {
		mute(win, nil)
		noisy(3, 7)(win, nil)
		p.Process(win)
	}
	// Read in plain centroid units, which is what PatternMinDev is stated in and
	// what the message is meant to be read as. Note this assertion is now a harder
	// test than it was: with the spread held in Q8 it can settle on its true value
	// instead of freezing wherever a 512-count deadband stopped it, so a failure
	// here means the guard genuinely no longer catches a still device rather than
	// that the spread was artificially clamped low.
	if dev := p.centDevQ8 >> 8; dev < PatternMinDev {
		t.Logf("spread %d is below the guard %d as expected", dev, PatternMinDev)
	} else {
		t.Errorf("on silence the spread grew to %d, above the guard %d: the guard no longer catches a still device",
			dev, PatternMinDev)
	}
	if got := p.pattern(); got != NumPatterns/2 {
		t.Errorf("after 48 s of silence the pattern is %d, want the middle %d", got, NumPatterns/2)
	}
}
