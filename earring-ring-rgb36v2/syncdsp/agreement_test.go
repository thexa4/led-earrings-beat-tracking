package syncdsp

import (
	"math"
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
// benchDC is the microphone's idle ADC code, measured on both bench devices.
const benchDC = 17120

func benchOffset(w []uint16, _ *int32) {
	for i, v := range w {
		y := benchDC + (int32(v)-32768)/64
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
