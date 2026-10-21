package syncdsp

// The band bank: a 9-band log-spaced analyser.
//
// Structure: a ladder of one-pole lowpass sections with geometrically spaced
// corners. Band i is the difference of two adjacent rungs (energy between two
// corners), and the last band is the lowest rung itself (everything below it).
// This is the cheap end of the filter-bank design space, and it is chosen for
// one reason above all: *every node is bounded by the input amplitude*, at
// every frequency, with no scaling decisions to make.
//
// Why not the Goertzel comb that this used to have (and which the
// datatrans/dsp assembly implements)? Because a Goertzel resonator's state
// gain is 1/(2·sin ω), which blows up at both ends of the spectrum: measured
// on this window (N=250, Fraction=13) the state gain is 2.0e7 at the 256 Hz
// band and 8.3e7 at the 7.9 kHz band, against int32's 2.1e9. Feeding it a
// full-scale signal is impossible; attenuating per band (which is what the
// first version did) starves the low bands of resolution to 5-6 bits, and
// then a 6 dB difference in mic gain produces a completely different
// quantization grid and a mean hash distance of 4 out of 15 bits — the exact
// failure the agreement test found. A resonator's magnitude-squared is also a
// difference of large nearly-equal numbers, so an empty band reads a
// cancellation residue instead of zero, which again differs per device.
//
// A one-pole ladder has none of these problems: |H| ≤ 1 for every section, so
// the products below peak at 5e8 against int32's 2.1e9 with no band-dependent
// scaling, an empty band accumulates a genuinely small number, and there is no
// 64-bit arithmetic anywhere. The cost is gentle 6 dB/oct skirts (adjacent
// bands overlap), which is fine here: the hash wants a coarse log spectral
// envelope, not a spectrum analyser.

// Bank geometry.
const (
	// NStages is the number of rungs in the ladder; each rung is two
	// cascaded identical one-pole sections (12 dB/oct skirts, and a
	// maximally-flat-ish magnitude that never exceeds the input, so no rung
	// can ring or overflow).
	NStages = 7
	// NBands is NStages+1: one band per gap between rungs, plus the lowest
	// rung (all content below the bottom corner). Eight bands gives the
	// 8 temporal + 7 tilt bits of the 15-bit hash.
	NBands = NStages + 1

	// poleShift is the fractional scale of BankK and HPK.
	poleShift = 14

	// inputShift drops the 16-bit ADC word by one bit before filtering,
	// keeping every intermediate inside int32 with 4x headroom. One bit is
	// irrelevant next to the ~40 dB of margin it buys.
	inputShift = 1
)

// BankK holds the one-pole coefficient k = 1 - exp(-2*pi*fc/fs) in Q14, for
// corners descending from 4 kHz to 250 Hz (fs = 15996.58 Hz: 2/3 octave
// steps). Band 0 is the top band (above 4 kHz, where "air" and cymbals live)
// and band NBands-1 the bottom one (below 250 Hz: kick and bass). Hardcoded,
// and checked against float math on the host by TestBankConstants, so no float
// math appears in device code.
//
// The top corner is 4 kHz rather than something nearer Nyquist on purpose: a
// one-pole section only reaches k/(2-k) = -3.6 dB at Nyquist (fs is just 4x
// the corner), so a rung placed near Nyquist would barely attenuate and the
// top three bands would end up measuring the same thing. With the top rung at
// 4 kHz the top band is a real band and the whole EDM range still fits.
var BankK = [NStages]int32{12979, 10295, 7600, 5322, 3592, 2366, 1532}

// HPK is the DC blocking highpass in front of the ladder: 30 Hz, Q14 (r =
// 16384-HPK). Blocking must happen inside the filter, not by subtracting the
// window mean: the ladder state persists across frames, and a per-window mean
// would make each frame's samples depend on where the frame boundaries fell.
const HPK = 192

// bank is the filter state plus the per-frame energy accumulators.
//
// Error-feedback states for the noise shaper discussed in bankShape are
// deliberately not allocated: bankShape disables the shaper on every rung
// (it measured worse for cross-device agreement), and keeping zeroed
// errA/errB fields plus their dead branches would spend 56 B of the tiny
// RAM budget on code that never runs.
type bank struct {
	hpX, hpY int32          // DC blocker memory
	rungA    [NStages]int32 // first pole of each rung
	rungB    [NStages]int32 // second pole of each rung

	energy [NBands]uint32 // sum of |band| over the frame

	// gainShift scales the DC-blocked sample; see Pipeline.agcUpdate for what
	// picks it and bank.run for where it is applied.
	gainShift int32

	// Input swing of the frame being accumulated, in raw ADC counts around
	// whatever bias that word carries. This is the level the gain tracker is
	// chosen from, and it is deliberately measured before the blocker, before
	// the gain and before any limiter, for two reasons: peak-to-peak ignores
	// the microphone bias without needing to estimate it (a bench device idles
	// 15600 counts off mid-rail), and a post-gain detector would be blind
	// exactly when it matters - a loud input at startup is caught by the
	// limiter, so the clipped value is all it would see, and it would conclude
	// the signal is quiet and keep the gain up.
	minX, maxX int32
}

// inputPeakPP returns the input swing accumulated over the frame just finished
// and arms the trackers for the next one.
func (b *bank) inputPeakPP() int32 {
	pp := b.maxX - b.minX
	b.minX, b.maxX = 1<<30, -(1 << 30)
	return pp
}

// reset clears the filter state, including the gain. Called from Pipeline.Init.
func (b *bank) reset() {
	*b = bank{}
	b.minX, b.maxX = 1<<30, -(1 << 30)
}

// scalePow multiplies by 2^d for either sign of d, rounding on the way down so
// a reduction does not bias the signal low by half an LSB. (Go's shift operators
// take an unsigned count, so `v << uint(-1)` is not a down-shift - it is a
// shift by 4294967295, which is zero for every v the pipeline produces. This is
// the only place either direction is needed, hence the helper rather than a
// general routine.)
func scalePow(v, d int32) int32 {
	if d >= 0 {
		return v << uint(d)
	}
	return (v + 1<<(-d-1)) >> uint(-d)
}

// rescaleGain moves the filter state by the same factor as a change of
// gainShift, so the step is invisible at the band outputs. The DC blocker's y
// is part of that state now - the gain is applied inside its recursion - so it
// moves too; hpX is not, being a sample of the un-scaled input.
//
// Shifting the states right loses bits, so it rounds: an unrounded shift
// would bias the state low by half an LSB per step, which is a deterministic
// error but a device-specific one (it depends on how many steps that device
// took to reach its level).
func (b *bank) rescaleGain(d int32) {
	b.hpY = scalePow(b.hpY, d)
	for i := range b.rungA {
		b.rungA[i] = scalePow(b.rungA[i], d)
		b.rungB[i] = scalePow(b.rungB[i], d)
	}
}

// bankShape marks the rungs whose truncation error gets fed back.
//
// A leaky integrator with a Q0 state can only move in steps of one LSB, so with
// a truncating shift it stalls as soon as k*|x-s| < 1<<poleShift, i.e. for
// |x-s| < 16384/k. For the top rung (k=12979) that is 1.3 LSB and harmless; for
// the bottom one (k=1532) it is nearly 11 LSB, which is a hard nonlinearity in
// the middle of the bass band. Feeding the previous sample's truncation error
// back into the next one removes the stall: the state still moves in whole LSBs,
// but its average tracks the input exactly, and the leftover error becomes a
// small high-frequency dither instead of a fixed offset.
//
// That matters here because the whole design rests on two earrings producing the
// same hash: a stalled state is a deterministic function of the signal's
// absolute amplitude, so two devices 6 dB apart stall at different points and
// their band energies drift apart in a way that no amount of normalising undoes.
//
// This is switched off for every rung, and the honest reason is a measurement,
// not a theory: with the shaper active on the narrow rungs, the mean hash
// distance between the same signal at 0 dB and -6 dB went UP from 0.31 to 1.10
// bits (and from 0.06 to 1.64 under a high-frequency tilt). The stall it removes
// is real but harmless, because it is a *stable* offset that both devices agree
// on; the dither that replaces it moves with the signal's phase, so the two
// earrings' band energies jitter independently and the hash bits flip.
//
// Rounding, which is applied unconditionally below, is what actually helps: it
// removes the systematic half-LSB bias. The lesson is that for cross-device
// agreement a small deterministic error beats an unbiased noisy one.
//
// The shaper itself, its per-rung enable table, and the errA/errB states have
// been removed on that basis; this note is kept so they are not blindly
// reintroduced.

// run processes one sample and adds its band magnitudes to the accumulators.
//
// The recurrence form is s += (k·(x-s))>>poleShift, a leaky integrator towards
// x with DC gain exactly 1. Writing it that way rather than as y = k·x + r·y
// keeps the state's gain at 1 (so a bass-heavy mix cannot inflate it) and needs
// one multiply per pole instead of two.
//
// Every shift rounds (add half an LSB before shifting): truncation biases the
// filter's DC response by a constant, which is measurable as cross-device hash
// disagreement. See bankShape for why the next step - error feedback - is *not*
// taken.
func (b *bank) run(x int32) {
	// DC block with the input gain folded into the recursion:
	//
	//	y[n] = r·(y[n-1] + G·(x[n] - x[n-1]))
	//
	// which is exactly G·HP(x): the transposed form only ever sees the input
	// *difference*, and the microphone bias lives in x's absolute value, so it
	// cancels in x[n]-x[n-1] whether or not the difference is scaled. That is
	// what makes the gain safe to apply this early - putting it on the
	// pipeline input instead amplifies the 15600-count bias along with the
	// music (tried, and it needs a DC estimator that then tracks the kick
	// waveform and breaks beat lock).
	//
	// It also moves where the rounding happens, which is the point. Scaling
	// the blocker's *output* quantises the pre-gain signal, so a device 42 dB
	// down carries 64x more relative rounding noise than one at full level;
	// measured as 3.6 bits of steady-state hash disagreement between two
	// devices hearing the same music. Inside the recursion the error is
	// ±1 LSB of the post-gain signal, which is the same for every device.
	if x < b.minX {
		b.minX = x
	}
	if x > b.maxX {
		b.maxX = x
	}
	const half = 1 << (poleShift - 1)
	dx := scalePow(x-b.hpX, b.gainShift)
	b.hpX = x
	// |y| ≤ |d| for this structure, so |s| is bounded by the clamp below, and
	// 2^17·16192 = 2.12e9 is the largest product that stays inside int32. At
	// gain 1 the clamp can never engage (|d| ≤ 65535 ⇒ |s| ≤ 131070), so it is
	// purely a limiter for a blast that arrives before the gain has come down;
	// it lets through a flat-top for a sample or two, and the AGC drops gain
	// the frame it sees the peak.
	s := b.hpY + dx
	if s > 1<<17 {
		s = 1 << 17
	} else if s < -(1 << 17) {
		s = -(1 << 17)
	}
	y := ((16384-HPK)*s + half) >> poleShift
	b.hpY = y

	// Saturating: the gain is picked so this almost never engages, and a
	// transient that does hit it loses a peak, not the frame.
	if y > AgcFullScale {
		y = AgcFullScale
	} else if y < -AgcFullScale {
		y = -AgcFullScale
	}

	prev := y
	for i := 0; i < NStages; i++ {
		a := b.rungA[i]
		a += (BankK[i]*(prev-a) + half) >> poleShift
		b.rungA[i] = a

		c := b.rungB[i]
		c += (BankK[i]*(a-c) + half) >> poleShift
		b.rungB[i] = c

		d := prev - c
		if d < 0 {
			d = -d
		}
		b.energy[i] += uint32(d)
		prev = c
	}
	d := prev
	if d < 0 {
		d = -d
	}
	b.energy[NBands-1] += uint32(d)
}

// energies folds the accumulated band magnitudes into out (as raw sums) and
// clears the accumulators for the next frame.
//
//go:noinline
func (b *bank) fold(out *[NBands]uint32) uint32 {
	total := uint32(0)
	for i := 0; i < NBands; i++ {
		out[i] = b.energy[i]
		total += b.energy[i]
		b.energy[i] = 0
	}
	return total
}
