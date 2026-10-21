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

// TestPatternIndexStable checks the pattern choice: in range, one pattern per
// band, gain invariant, and unmoved by perturbations small enough that no band
// actually changes rank.
func TestPatternIndexStable(t *testing.T) {
	if NumPatterns != NBands-1 {
		t.Errorf("NumPatterns = %d, want NBands-1 = %d: the mapping is one pattern per band", NumPatterns, NBands-1)
	}
	vectors := [][NBands]int32{
		{4100, 4600, 4900, 4700, 4300, 3900, 3600, 5200}, // mids
		{5400, 4300, 4100, 4000, 3900, 3800, 3700, 5100}, // air
		{3900, 4000, 4200, 4400, 4700, 5000, 5300, 5100}, // low mids
		{4500, 4500, 4500, 4500, 4500, 4500, 4500, 4500}, // flat
		{4000, 4100, 4200, 5200, 4200, 4100, 4000, 5000}, // band 3 lead
		{4000, 5000, 4200, 4100, 4100, 4100, 4100, 5000}, // band 1 lead
	}
	seen := map[int32]bool{}
	for _, v := range vectors {
		dev := spectralDevs(&v)
		pat := PatternIndex(&dev)
		if pat < 0 || pat >= NumPatterns {
			t.Fatalf("pattern %d out of range for %v", pat, v)
		}
		seen[pat] = true
		// Gain invariance: the same spectrum at another level.
		for _, off := range []int32{-2000, -300, 900, 6000} {
			var w [NBands]int32
			copy(w[:], v[:])
			for b := range w {
				w[b] += off
			}
			d2 := spectralDevs(&w)
			if p2 := PatternIndex(&d2); p2 != pat {
				t.Errorf("offset %d changed pattern %d -> %d", off, pat, p2)
			}
		}
		// A perturbation must not move the choice unless it changes the ranking.
		for b := 0; b < NBands; b++ {
			for _, eps := range []int32{-64, 64} {
				var w [NBands]int32
				copy(w[:], v[:])
				w[b] += eps
				d2 := spectralDevs(&w)
				if p2 := PatternIndex(&d2); p2 != pat && dev[pat]-dev[p2] > 64 {
					t.Errorf("band %d by %d Q8 log2 flipped pattern %d (gap %.2f dB) -> %d",
						b, eps, pat, float64(dev[pat]-dev[p2])/256, p2)
				}
			}
		}
	}
	if len(seen) < 4 {
		t.Errorf("PatternIndex uses only %d of %d patterns", len(seen), NumPatterns)
	}
}

// TestPatternHysteresis checks that a band cannot take the pattern from the one
// on screen without beating it by PatternMargin, and does take it once it does.
//
// It feeds usePattern deviation arrays directly, which is the domain the
// function works in: the deviations themselves are what spectralDevs produces
// and what the margin compares.
func TestPatternHysteresis(t *testing.T) {
	// Band 2 dominant, band 3 a clear second but inside the margin.
	var p Pipeline
	p.Init()
	dev := [NBands]int32{-20000, -10000, 0, -4000, -12000, -18000, -22000, -24000}
	if got := p.usePattern(&dev); got != 2 {
		t.Fatalf("first choice should be band 2, got %d", got)
	}
	// Band 3 comes within a whisker: not enough to take over.
	dev[3] = dev[2] - 1000
	if got := p.usePattern(&dev); got != 2 {
		t.Errorf("a 1000-unit challenge took the pattern (%d), needs %d", 1000, PatternMargin)
	}
	// Just short of the margin: still holds.
	dev[3] = dev[2] + PatternMargin - 1
	if got := p.usePattern(&dev); got != 2 {
		t.Errorf("a challenge of %d took the pattern, needs %d", PatternMargin-1, PatternMargin)
	}
	// Over the margin: takes over.
	dev[3] = dev[2] + PatternMargin
	if got := p.usePattern(&dev); got != 3 {
		t.Errorf("a clear win of %d did not take the pattern, got %d", PatternMargin, got)
	}
	// Now band 2 has to clear the same margin to win it back, and does not.
	dev[2] = dev[3] - 1
	if got := p.usePattern(&dev); got != 3 {
		t.Errorf("incumbent should defend, got %d", got)
	}
}
