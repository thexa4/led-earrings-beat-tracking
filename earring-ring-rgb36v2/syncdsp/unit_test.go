package syncdsp

import (
	"math"
	"math/bits"
	"testing"
)

// TestBankConstants verifies the hardcoded Q14 ladder coefficients against
// float math on the host, and that the corners are log spaced and in range.
// The device cannot do this check (no floats), so the golden selftest depends
// on these being right.
func TestBankConstants(t *testing.T) {
	prev := float64(SampleRate) / 2
	for i, k := range BankK {
		fc := -float64(SampleRate) * math.Log(1-float64(k)/16384) / (2 * math.Pi)
		// Quantization of k to Q14 moves the corner; 3% is invisible here.
		if fc > 10 && math.Abs(fc-targetCorner(i))/targetCorner(i) > 0.03 {
			t.Errorf("stage %d: corner %.0f Hz, want ~%.0f Hz", i, fc, targetCorner(i))
		}
		if fc >= prev*(1-0.03) {
			t.Errorf("stage %d: corner %.0f Hz is not below the previous one (%.0f)", i, fc, prev)
		}
		prev = fc
		if k <= 0 || k >= 16384 {
			t.Errorf("stage %d: coefficient %d out of Q14 range", i, k)
		}
	}
	// The bottom rung has to leave room for the lowest band to be useful, and
	// the top corner has to stay below Nyquist with margin for the ADC.
	if fc := corner(0); fc > 7500 {
		t.Errorf("top corner %.0f Hz too close to Nyquist", fc)
	}
	if fc := corner(NStages - 1); fc > 600 {
		t.Errorf("bottom corner %.0f Hz swallows the bass", fc)
	}
}

// targetCorner is the design corner frequency of stage i (descending).
func targetCorner(i int) float64 {
	return [NStages]float64{4000, 2520, 1587, 1000, 630, 397, 250}[i]
}

func corner(i int) float64 {
	return -float64(SampleRate) * math.Log(1-float64(BankK[i])/16384) / (2 * math.Pi)
}

// bandTonePeak runs a steady tone through the bank and returns the band with
// the largest energy.
func bandTonePeak(f, amp float64) (int, [NBands]uint32) {
	raw := make([]uint16, Window)
	var p Pipeline
	p.Init()
	// The DC blocker and the ladder start cold, and the step from silence to a
	// tone is a broadband click. Let that settle before measuring, or every
	// band reads loud.
	for w := 0; w < 24; w++ {
		for i := range raw {
			t := float64(w*Window + i)
			raw[i] = uint16(32768 + amp*math.Sin(2*math.Pi*f*t/SampleRate))
		}
		p.Process(raw)
	}
	best, bestV := 0, p.energies[0]
	for i, v := range p.energies[1:] {
		if v > bestV {
			best, bestV = i+1, v
		}
	}
	return best, p.energies
}

// TestBankResponse checks what the hash actually needs from the bank: that it
// separates frequency. Sweeping tone frequency must sweep the peak band
// monotonically (band 0 is the top of the range, NBands-1 the bottom), and no
// band may be dead.
//
// It deliberately does *not* assert that a tone lands in the band named by the
// geometric mean of two corners. Each band is the difference of two one-pole
// lowpasses, whose peak sits close to the upper corner of the gap, not in the
// middle - an earlier version of this test asserted the middle and failed on
// code that was verified byte-exact against a separate model of the same
// arithmetic.
func TestBankResponse(t *testing.T) {
	if corner(0) >= SampleRate/2*0.9 {
		t.Fatalf("top corner %.0f Hz aliases", corner(0))
	}
	tones := []float64{60, 120, 250, 400, 630, 1000, 1600, 2500, 4000, 6000}
	prevPeak := NBands
	for _, amp := range []float64{16000, 2000} {
		prevPeak = NBands
		for _, f := range tones {
			peak, e := bandTonePeak(f, amp)
			// Higher frequency must never move the peak to a *lower* index.
			if prevPeak < NBands && peak > prevPeak {
				t.Errorf("amp %.0f: peak band went %d -> %d as frequency rose to %.0f Hz (%v)",
					amp, prevPeak, peak, f, e)
			}
			prevPeak = peak
			// No band may be flat-zero at a reasonable level, and the loudest
			// band must dominate the quietest by a real margin, or the shape
			// bits carry no information.
			min, max := e[0], e[0]
			for _, v := range e[1:] {
				if v < min {
					min = v
				}
				if v > max {
					max = v
				}
			}
			if max < 100 {
				t.Errorf("tone at %.0f Hz amp %.0f: everything silent (%v)", f, amp, e)
			}
			// A bank with no frequency selectivity would read nearly the same in
			// every band; 6x (15 dB) of spread is a low bar that still catches
			// that. It is not a claim about resolution: bands here are 2/3
			// octave wide, so a pure tone legitimately lights up several.
			if min*6 > max {
				t.Errorf("tone at %.0f Hz amp %.0f: bands too flat to separate (%v)", f, amp, e)
			}
		}
	}
	// Each band must win somewhere, otherwise it contributes nothing.
	wins := map[int]bool{}
	for _, f := range []float64{60, 90, 130, 190, 280, 420, 630, 950, 1500, 2400, 3700, 5500, 7000} {
		peak, _ := bandTonePeak(f, 16000)
		wins[peak] = true
	}
	for b := 0; b < NBands; b++ {
		if !wins[b] {
			t.Errorf("band %d is never the loudest: it carries no information", b)
		}
	}
}

// TestBankHeadroom runs the bank with a worst-case full-scale signal (loudest
// possible content at every frequency, plus full-scale steps) and asserts the
// internal products stay well inside int32, which is what makes this bank safe
// at every frequency without band-dependent input scaling.
func TestBankHeadroom(t *testing.T) {
	const int32Max = 2147483647
	worstProd := int64(0)
	worstState := int64(0)
	raw := make([]uint16, Window)
	var p Pipeline
	p.Init()
	for _, f := range []float64{30, 100, 395, 994, 2502, 6300, 7900} {
		for i := 0; i < Window; i++ {
			// Full swing square wave: the worst case for the DC blocker
			// bracket, which sees the whole step at once.
			v := int32(-32000)
			if math.Sin(2*math.Pi*f*float64(i)/SampleRate) >= 0 {
				v = 32000
			}
			raw[i] = uint16(32768 + v)
		}
		p.Process(raw)
	}
	// Re-run in exact arithmetic to measure the peaks.
	var s [NStages]int32
	hpX, hpY := int32(0), int32(0)
	for i := 0; i < Window; i++ {
		x := (int32(raw[i]) - 1<<15) >> inputShift
		if prod := int64(16384-HPK) * int64(hpY+x-hpX); abs64(prod) > worstProd {
			worstProd = abs64(prod)
		}
		y := ((16384 - HPK) * (hpY + x - hpX)) >> poleShift
		hpX, hpY = x, y
		prev := y
		for j := 0; j < NStages; j++ {
			if prod := int64(BankK[j]) * int64(prev-s[j]); abs64(prod) > worstProd {
				worstProd = abs64(prod)
			}
			s[j] += (BankK[j] * (prev - s[j])) >> poleShift
			if abs64(int64(s[j])) > worstState {
				worstState = abs64(int64(s[j]))
			}
			prev = s[j]
		}
	}
	if worstProd > int32Max/2 {
		t.Errorf("int32 headroom: worst product %d exceeds half of int32", worstProd)
	}
	t.Logf("worst product = %.1f%% of int32, worst state = %.1f%%",
		100*float64(worstProd)/int32Max, 100*float64(worstState)/int32Max)
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// TestLog2e checks the binary-search log2 against the math/bits reference.
func TestLog2e(t *testing.T) {
	ref := func(v uint32) int32 {
		if v == 0 {
			return 0
		}
		msb := 31 - bits.LeadingZeros32(v)
		// Fraction: the 8 bits *below* the MSB.
		frac := (v << (32 - uint(msb))) >> 24
		return int32(msb)<<8 | int32(frac)
	}
	vals := []uint32{0, 1, 2, 3, 4, 255, 256, 257, 65535, 65536, 1 << 20,
		1<<31 - 1, 1 << 31, 0xffffffff}
	for i := 0; i < 100000; i += 37 {
		vals = append(vals, uint32(i)*7919, uint32(i)*104729+1)
	}
	for _, v := range vals {
		if got, want := log2e(v), ref(v); got != want {
			t.Errorf("log2e(%d) = %d, want %d", v, got, want)
		}
	}
}

// TestMulQ16 checks the split 32-bit multiply against int64 arithmetic over
// the full reachable range of the slope product (|n| <= 2^18) plus a margin,
// and both signs.
func TestMulQ16(t *testing.T) {
	check := func(n int32, q uint32) {
		want := int32((int64(n) * int64(q)) >> 16)
		if got := mulQ16(n, q); got != want {
			t.Fatalf("mulQ16(%d, %d) = %d, want %d", n, q, got, want)
		}
	}
	for _, q := range []uint32{49939, 1, 0xffff, 32768} {
		check(0, q)
		for i := int32(-1 << 18); i <= 1<<18; i += 257 {
			check(i, q)
		}
		check(1<<18, q)
		check(-(1 << 18), q)
		check(1<<24-1, q)
		check(-(1<<24)+1, q)
	}
}

// TestHashDist checks the software Hamming distance against the standard
// library, over every reachable hash value (masked) and against a few partners.
func TestHashDist(t *testing.T) {
	for i := 0; i <= HashMask; i += 3 {
		u := uint32(i) & HashMask
		if got, want := hashDist(u, 0), int32(bits.OnesCount32(u)); got != want {
			t.Fatalf("hashDist(%#x, 0) = %d, want %d", u, got, want)
		}
		if got, want := hashDist(u, ^u), int32(bits.OnesCount32(HashMask)); got != want {
			t.Fatalf("hashDist(%#x, complement) = %d, want %d (all bits differ)", u, got, want)
		}
	}
	if hashDist(0, 1<<30) != 0 {
		t.Error("hashDist must ignore bits outside HashMask")
	}
}

// TestSynthDeterministic: two calls with the same index range must agree.
func TestSynthDeterministic(t *testing.T) {
	a := make([]uint16, Window)
	b := make([]uint16, Window)
	Synth(12345, a)
	Synth(12345, b)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("synth not deterministic at %d", i)
		}
	}
	// Sanity: signal is near mid-rail and actually varies.
	min, max := a[0], a[0]
	sum := 0
	for _, v := range a {
		sum += int(v)
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	if min < 500 || max > 65000 {
		t.Errorf("synth out of range: %d..%d", min, max)
	}
	if absInt(sum/len(a)-32768) > 8000 {
		t.Errorf("synth DC far from mid-rail: %d", sum/len(a))
	}
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// TestHashDeadband checks that tiny log-domain differences never flip temporal
// bits while clear ones do.
func TestHashDeadband(t *testing.T) {
	var cur, old [NBands]int32
	for b := range cur {
		cur[b] = 4000 // level does not matter, the hash is level invariant
		old[b] = 4000
	}
	dev := spectralDevs(&cur)
	// A flat spectrum has every band within the floor of the max, so the hot
	// field is all-ones here; the temporal field is what this test is about.
	if h := hashFrame(cur, old, &dev) & ((1 << NBands) - 1); h != 0 {
		t.Errorf("identical flat frames should have no temporal bits, got %015b", h)
	}
	// 1 dB up in band 3 (256 Q8 log2 == 6.02 dB): below the 3 dB deadband.
	old[3] = cur[3] - 256/6
	if h := hashFrame(cur, old, &dev); h&(1<<3) != 0 {
		t.Errorf("1 dB change flipped a temporal bit: %015b", h)
	}
	// 6 dB up: must flip.
	old[3] = cur[3] - 256
	if h := hashFrame(cur, old, &dev); h&(1<<3) == 0 {
		t.Errorf("6 dB change did not flip: %015b", h)
	}
}

// TestHashLevelInvariant is the core cross-device property: scaling every band
// by the same factor - a constant in the log domain, which is what a different
// mic gain, distance or hand shadow is - must leave the hash untouched.
func TestHashLevelInvariant(t *testing.T) {
	var cur, old [NBands]int32
	for b := range cur {
		cur[b] = int32(2600 + 37*b + (b*b*91)%700)
		old[b] = cur[b] - int32(b%3)*150
	}
	dev := spectralDevs(&cur)
	h0 := hashFrame(cur, old, &dev)
	if h0 == 0 {
		t.Fatalf("test input produces a zero hash, pick a livelier one")
	}
	for _, off := range []int32{-1500, -300, 700, 5000, 20000} {
		var c2, o2 [NBands]int32
		for b := range c2 {
			c2[b] = cur[b] + off
			o2[b] = old[b] + off
		}
		dev2 := spectralDevs(&c2)
		if h := hashFrame(c2, o2, &dev2); h != h0 {
			t.Errorf("offset %d changed hash %015b -> %015b", off, h0, h)
		}
	}
}

// TestHashEntropy guards against a degenerate hash. This pipeline has hit that
// twice, both times invisibly (the code ran, produced no errors, and agreed with
// itself perfectly): first a per-band AGC equalised every band so every
// comparison came out equal, then a raw per-frame hash that was simply the same
// word on every beat. Both made the hash constant, which makes the ring match
// everything at distance 0 and no transition ever look novel.
//
// The correct expectation is the opposite of "varies constantly": the hash must
// be *stable while the music is stable* (otherwise the periodicity check can
// never match) and *different between sections* (otherwise nothing is ever
// novel). So the test measures both halves:
//
//   - distinct words across the full 8-section structure, which varies;
//   - within-section stability: consecutive hashes far from a section boundary
//     must mostly be identical or one bit apart;
//   - section separation: hashes from two different sections must be far apart,
//     or the anchor can never fire.
func TestHashEntropy(t *testing.T) {
	const sections = 8
	const frames = sections * synthSection / Window
	var p Pipeline
	p.Init()
	raw := make([]uint16, Window)

	seen := map[uint32]bool{}
	bits := 0
	// Per-section hash sets, sampled mid-section (away from transitions,
	// where the hash is allowed - required - to be constant).
	sectionWords := make([]map[uint32]int, sections)
	for i := range sectionWords {
		sectionWords[i] = map[uint32]int{}
	}
	prev := uint32(0)
	framesSinceChange := 0
	steady, steadySame := 0, 0
	for f := 0; f < frames; f++ {
		Synth(int64(f)*Window, raw)
		ev := p.Process(raw)
		if ev.HashAge > 0 { // a fresh hash was computed this frame
			seen[ev.Hash] = true
			bits += int(hashDist(ev.Hash, 0))
		}
		// Stability and section identity are measured on the *identity*
		// fields (shape + hot), exactly like the pipeline's own settle
		// tracking: the temporal field's job is to move while energy rises,
		// so a full-hash stability test would call every drop "unstable" and
		// every anchor frame novel-by-temporal-flicker. The parts that must
		// be still are the parts that describe the section.
		id := ev.Hash & IdentityMask
		if id != prev {
			framesSinceChange = 0
		} else {
			framesSinceChange++
		}
		// "Steady" = not within 2 smoothing constants of any identity change,
		// and past warmup: the hash has settled into its section value.
		if f >= WarmupFrames && framesSinceChange > 2*1<<SmoothShift {
			steady++
			if id == prev {
				steadySame++
			}
			sec := (f * Window) / synthSection
			sectionWords[sec%sections][id]++
		}
		prev = id
	}
	avgBits := float64(bits) / frames
	if avgBits < 1.0 || avgBits > 12 {
		t.Errorf("hash popcount mean %.2f out of range: hash is flat or saturating", avgBits)
	}
	if len(seen) < 24 {
		t.Errorf("only %d distinct hashes across %d sections: hash is degenerate", len(seen), sections)
	}
	if steady > 0 && float64(steadySame)/float64(steady) < 0.85 {
		t.Errorf("hash unstable within sections: only %.0f%% of settled frames repeat the previous word",
			100*float64(steadySame)/float64(steady))
	}

	// Section separation: every pair of sections must differ. If two
	// sections share their settled word, an anchor at that transition can
	// never fire and the ring treats them as the same music.
	var rep [sections]uint32
	for s := 0; s < sections; s++ {
		if len(sectionWords[s]) == 0 {
			t.Errorf("section %d never settles: no steady hash sampled", s)
			continue
		}
		// Take the dominant word: transient identities that happened to
		// hold steady for 64 frames exist near the section edges, but the
		// section's own value dominates by frame count.
		best := 0
		for h, n := range sectionWords[s] {
			if n > best {
				rep[s], best = h, n
			}
		}
	}
	// Section separation, checked across *adjacent* sections only. This is
	// the semantically correct scope: an anchor compares a section against the
	// ring of previous sections and fires at a transition, so the pair that
	// must be far apart is a section and the one that actually follows it in
	// the timeline - not two arbitrary sections of different modes that the
	// synth never plays back to back. (Break and build, for instance, are both
	// broadband and hash 1 bit apart, but a break is always separated from a
	// build by a drop, so no anchor ever needs to tell them apart directly.)
	// Same-mode neighbours are the same audio, so distance 0 is correct there.
	for i := 0; i < sections; i++ {
		j := (i + 1) % sections
		mi, mj := sectionMode(i), sectionMode(j)
		if mi == mj {
			continue
		}
		if d := hashDist(rep[i], rep[j]); d < DMin {
			t.Errorf("sections %d (mode %d) and %d (mode %d) identity %.30b vs %.30b: distance %d < DMin, no anchor can fire at this transition",
				i, mi, j, mj, rep[i], rep[j], d)
		}
	}
	// The other half of the contract: the same mode heard twice must hash to
	// within DAccept, or the periodicity check can never recognise a section
	// returning after 2/4/8 bars. (In this synth same mode == same audio, so
	// this asserts the hash is a function of the music, not of the timeline
	// position or the filter's distant memory.)
	for i := 0; i < sections; i++ {
		for j := i + 1; j < sections; j++ {
			if sectionMode(i) != sectionMode(j) {
				continue
			}
			if len(sectionWords[i]) == 0 || len(sectionWords[j]) == 0 {
				continue
			}
			if d := hashDist(rep[i], rep[j]); d > DAccept {
				t.Errorf("sections %d and %d share mode %d but hash %.30b vs %.30b: distance %d > DAccept, periodicity cannot match",
					i, j, sectionMode(i), rep[i], rep[j], d)
			}
		}
	}
	t.Logf("entropy: %d distinct words, mean popcount %.2f, steady-repeat %.0f%%",
		len(seen), avgBits, 100*float64(steadySame)/math.Max(float64(steady), 1))
}
