package syncdsp

import "testing"

// TestPatternWeights checks the weights that collapse the spectrum into one
// number. Summing to zero is load-bearing: a flat spectrum and a uniform AGC
// gain must produce no deviation at all.
func TestPatternWeights(t *testing.T) {
	sum, pos, neg := int32(0), 0, 0
	for _, w := range PatternWeights {
		sum += w
		if w > 0 {
			pos++
		}
		if w < 0 {
			neg++
		}
	}
	if sum != 0 {
		t.Errorf("PatternWeights sums to %d, want 0: a flat spectrum must give no deviation", sum)
	}
	if pos < 1 || neg < 1 {
		t.Errorf("weights have %d positive and %d negative entries: with one sign the centroid tracks loudness", pos, neg)
	}
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

// TestPatternIndexBucket: always in range, monotone (two devices that disagree
// land on neighbours), every pattern reachable, no opinion without a spread, and
// saturation rather than wrap-around at extreme inputs.
func TestPatternIndexBucket(t *testing.T) {
	if !(PatternCut1 < PatternCut2 && PatternCut2 < PatternCut3) {
		t.Fatalf("cuts not increasing: %d %d %d", PatternCut1, PatternCut2, PatternCut3)
	}
	if PatternCut1 <= -512 || PatternCut3 >= 512 {
		t.Fatalf("a cut sits outside the clamp of +-2 spreads, so a pattern is unreachable")
	}
	for _, spread := range []int32{PatternMinDev, 500, 40000} {
		prev := int32(-1)
		seen := map[int32]bool{}
		for gap := -4 * spread; gap <= 4*spread; gap += max(spread/64, 1) {
			pat := PatternIndex(gap, spread)
			if pat < 0 || pat >= NumPatterns {
				t.Fatalf("pattern %d out of range at gap=%d spread=%d", pat, gap, spread)
			}
			if pat < prev {
				t.Fatalf("pattern went backwards: %d then %d at gap=%d spread=%d", prev, pat, gap, spread)
			}
			prev = pat
			seen[pat] = true
		}
		if len(seen) != NumPatterns {
			t.Errorf("spread %d: only %d of %d patterns reachable", spread, len(seen), NumPatterns)
		}
	}
	for _, spread := range []int32{0, 1, PatternMinDev - 1} {
		if got := PatternIndex(12000, spread); got != NumPatterns/2 {
			t.Errorf("spread %d chose %d, want the middle pattern %d", spread, got, NumPatterns/2)
		}
	}
	for _, big := range []int32{1 << 20, 1 << 28, 1 << 30} {
		if got := PatternIndex(big, 1<<10); got != NumPatterns-1 {
			t.Errorf("gap %d chose %d, want the top pattern", big, got)
		}
		if got := PatternIndex(-big, 1<<10); got != 0 {
			t.Errorf("gap %d chose %d, want the bottom pattern", -big, got)
		}
	}
}

// TestPatternDeviceInvariant: a sensitivity difference scales the deviation and
// the spread together, and floor(a*n/(b*n)) == floor(a/b), so it cannot move the
// choice. (A tilt is a constant in the centroid, removed by the reference before
// PatternIndex is reached; the end-to-end gain/tilt cases are in TestAgreement.)
func TestPatternDeviceInvariant(t *testing.T) {
	const spread = int32(900)
	for _, s := range []int32{1, 3, 17, 1024} {
		for gap := int32(-3000); gap <= 3000; gap += 37 {
			if want, got := PatternIndex(gap, spread), PatternIndex(gap*s, spread*s); got != want {
				t.Errorf("sensitivity x%d moved the pattern %d -> %d at gap=%d", s, want, got, gap)
			}
		}
	}
}

// TestPatternConvergesAfterDivergence: two pipelines driven onto different
// patterns show the same one again once they are handed the same input. This is
// the property the old incumbent/margin selector broke, so any future state in
// this path has to clear it.
func TestPatternConvergesAfterDivergence(t *testing.T) {
	const spread = 500
	var a, b Pipeline
	a.Init()
	b.Init()
	read := func(p *Pipeline, gap int32) {
		p.frame = PatternWarmupFrames
		p.centGapQ8, p.centDevQ8 = gap<<8, spread<<8
		p.repattern()
	}
	read(&a, 4*spread)
	read(&b, -4*spread)
	if a.lastPattern == b.lastPattern {
		t.Fatalf("the two pipelines did not diverge, so this test proves nothing")
	}
	read(&a, 0)
	read(&b, 0)
	if a.lastPattern != b.lastPattern {
		t.Errorf("after a shared input the devices still differ: %d vs %d", a.lastPattern, b.lastPattern)
	}
}

// TestPatternChangeReported: the display only follows Events.PatternChanged, so
// every frame on which Events.Pattern moves must report it. The backstop once ran
// before the events were rebuilt, and its changes reached the pipeline state
// without ever reaching the LEDs.
func TestPatternChangeReported(t *testing.T) {
	in := newInst(nil, false)
	prev, changes := int32(0), 0
	for f := range testFrames {
		e := in.frame(f)
		if e.Pattern != prev && !e.PatternChanged {
			t.Fatalf("frame %d: pattern %d -> %d without PatternChanged", f, prev, e.Pattern)
		}
		if e.PatternChanged {
			changes++
		}
		prev = e.Pattern
	}
	if changes == 0 {
		t.Errorf("no pattern changes in %d frames, so this test proves nothing", testFrames)
	}
}

// TestPatternSilenceHoldsMiddle: a device with nothing to hear has no opinion,
// and says so with the middle pattern rather than flickering on rounding noise.
//
// The input is exact silence. Microphone hiss alone is not classified as silent by
// this pipeline: the AGC lifts it to music level and the pattern follows it.
func TestPatternSilenceHoldsMiddle(t *testing.T) {
	var p Pipeline
	p.Init()
	win := make([]uint16, Window)
	for i := range win {
		win[i] = benchDC
	}
	for range 3000 {
		p.Process(win)
	}
	if dev := p.centDevQ8 >> 8; dev >= PatternMinDev {
		t.Errorf("on silence the spread grew to %d, above the guard %d", dev, PatternMinDev)
	}
	if got := p.pattern(); got != NumPatterns/2 {
		t.Errorf("after 48 s of silence the pattern is %d, want the middle %d", got, NumPatterns/2)
	}
}
