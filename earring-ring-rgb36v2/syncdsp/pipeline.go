package syncdsp

import "github.com/aykevl/things/earring-ring-rgb36v2/rolled"

// The per-frame processing pipeline:
//
//	raw uint16 samples (one analysis window)
//	  -> one-pole ladder band bank (bank.go), persistent filter state
//	  -> per-band energy = sum of |band| over the window
//	  -> log2 energy (integer, via binary search), relative floor
//	  -> 15-bit hash: 8 temporal delta bits + 7 spectral shape bits
//	  -> beat tracker (onset flux + interval histogram + phase)
//	  -> at each beat: ring buffer lookup, anchor / periodicity decision
//
// Every threshold in the hash is a difference (between frames, or against a
// fitted spectral line), so a device's gain cancels out entirely and no AGC is
// needed. Everything is deterministic and integer-only, so two devices hearing
// the same music make the same decisions.

// Events reports what happened in the most recent processed frame.
type Events struct {
	Hash     uint32
	HashAge  int32 // frames since Hash was last recomputed (0 = this frame)
	Onset    bool  // spectral flux onset this frame
	Beat     bool  // beat boundary this frame
	Bar      bool  // bar boundary this frame
	Anchor   bool  // transition anchor: reset to base pattern
	Returned bool  // periodicity confirmation: this section is one the
	// ring already holds at a bar-multiple lag (see decide). Reported because it
	// is the event that re-aligns two devices that drifted, and - unlike the
	// anchor - it fires often enough to measure.
	Pattern        int32
	PatternChanged bool
	Locked         bool // beat tracker has lock
	Silent         bool // too quiet to decide anything
}

// Pipeline holds all state for one device.
type Pipeline struct {
	// Filter bank: poles, DC blocker and the per-frame energy accumulators.
	filters bank

	// smooth is the time-smoothed log2 band spectrum that the hash and the
	// pattern are derived from (see endFrame step 5); prevSmooth is the same at
	// the previous beat, which is what the temporal half of the hash compares
	// against. Two arrays rather than a history ring because a beat is far longer
	// than a frame: indexing a short frame history by beats would need dozens of
	// frames' worth of spectra.
	//
	// smooth carries the relative floor applied after smoothing (see
	// RelFloorQ8), not before: flooring the raw frame would make the floor
	// depend on each frame's instantaneous loudest band - which flickers with
	// the beat - and would decouple it from everything derived from smooth.
	// Floored after smoothing, the floor lands identically on every device
	// (a gain shifts the max and the floor together) and lands once per
	// settling, not once per frame.
	smooth     [NBands]int32
	prevSmooth [NBands]int32
	// smoothSnap is the snapshot of smooth the per-frame hash was computed
	// from, so prevSmooth and the hash's temporal field advance together.
	smoothSnap [NBands]int32

	// Raw per-band energies for the frame being processed.
	energies [NBands]uint32

	// Input normalization (see feed and agcUpdate; AgcTargetPeak carries the
	// measurement that motivated it). Deliberately *outside* the hash's
	// relative design: the gain is a power of two, so it moves every band's
	// log2 energy by the same constant, which the shape comparisons cancel.
	//   agcPeak   peak-hold of the bank's per-frame DC-blocked peak
	//   agcShift  applied gain in bits, mirrored into filters.gainShift
	//   agcHold   frames until the next gain increase is allowed
	agcPeak  int32
	agcShift int32
	agcHold  int32

	// Frame-level loudness tracking, used for the onset detector, the anchor
	// energy gate and silence detection:
	//   total     sum of per-band log2 energies (Q8)
	//   level     slow average of total
	//   linPeak   peak-hold of the linear band-energy sum (SilenceDiv leak)
	total      int32
	prevTotal  int32
	prevTotal2 int32 // onset flux runs over two frames, see onsetDetect
	fluxFloor  int32

	// Bass-only flux detector, the beat tracker's input. Same shape as the
	// one above, run on the bottom band (below 250 Hz) instead of the full
	// spectrum. Measured against the played file on the host replay: the
	// full-spectrum onset lands within 2 frames of the true beat grid 28% of
	// the time, which is barely better than chance (18% for +-2 of 27) - it
	// fires on vocal consonants, guitar picks and hats, and the beat tracker
	// then locks to whatever periodicity that soup happens to have (period 19
	// on a 142 BPM track). The bottom band is the kick, which is the beat.
	prevBass       int32
	prevBass2      int32
	bassFloor      int32
	bassOnset      bool // debug: this frame's bass onset, for the replay trace
	level          int32
	linPeak        int32
	onsetAge       int32 // frames since the last onset (100 = long ago)
	lastOnsetFrame int32 // frame of the last full-spectrum onset (refractory)
	silent         bool  // far quieter than the recent peak: leave things alone

	// Beat tracker.
	beat BeatTracker

	// Tempo scan: a ring of the recent frame envelope, correlated against
	// itself to find the tempo. See tempoScan.
	env     [TempoWindow]int8
	envHead int32 // frames pushed so far; slot = envHead & EnvMask
	envRef  int32 // slow baseline subtracted before quantising
	// envSum and envSq are the ring's running sum and sum of squares, kept
	// current one element at a time by pushEnv. |value| <= 128 so both fit
	// comfortably in int32 (256 * 128 * 128 = 4.2M).
	envSum int32
	envSq  int32

	// Beat-indexed hash ring. Indexed by beat counter, never by wall clock:
	// see PARTY-SYNC.md §D3.
	ring        [RingSize]uint32
	ringCount   int32 // number of beats stored so far (capped at RingSize)
	lastHash    uint32
	lastPattern int32

	// Anchor stability: how many consecutive frames the hash *identity*
	// (every field except the temporal one) has been unchanged. The anchor
	// fires only once the hash has settled, so that it compares a settled
	// section descriptor against the ring instead of a device-specific
	// sample of the transition transient. The temporal field is excluded
	// because it legitimately flickers with push phase even in steady
	// state. See PARTY-SYNC.md §5.
	stableID        uint32
	stableFrames    int32
	lastAnchorFrame int32
	// lastAnchorHash is the hash the most recent anchor fired on (0 = none
	// ever: 0 is not a reachable hash, the hot field always has at least the
	// maximum band set). A repeat anchor needs the hash to have moved
	// AnchorRepeatDist from it, which turns the settling-edge trigger from
	// "novel right now" (true for seconds after a transition, while the ring
	// window still holds the previous section) into "novel section", once.
	lastAnchorHash uint32

	// beatsSincePattern counts beats since the last pattern change, enforcing
	// PatternRefractoryBeats. Counted in decide(), so it advances at the same
	// rate the ring does - including the virtual beats used before lock - which
	// keeps the two devices' counters aligned without a shared clock.
	beatsSincePattern int32
	// framesSincePattern counts frames since the last pattern change, for the
	// backstop at PatternMaxHoldFrames.
	framesSincePattern int32

	// Pattern feature: the spectral centroid (the PatternWeights sum over the
	// smoothed spectrum), its own slow reference and mean absolute deviation, and
	// the low-passed deviation the pattern is read from. All four carry eight
	// fraction bits: an integer EMA at these shifts has a deadband of hundreds of
	// counts and stops wherever it happened to stop moving. See endFrame step 5b'.
	centQ8    int32
	centGapQ8 int32
	centAvgQ8 int32
	centDevQ8 int32

	// Impulse filter (see feed): the five-sample window, oldest first, the
	// priming count, and how many samples the filter replaced (for host tests).
	spX1, spX2, spX3, spX4, spX5 int32
	spFill                       int32
	spCount                      int32

	// Display levels (see LevelRangeQ8): the reference peak per band plus
	// one for the whole spectrum, and the levels themselves - Level[i] for
	// band i (0 = top band, above 4 kHz; NBands-1 = below 250 Hz) and
	// Level[NBands] for the overall loudness. Written every frame; 0 while
	// silent.
	lvlPeak [NBands + 1]int32
	lvlTop  int32 // the highest of lvlPeak, last frame
	lvlN    int8  // NBands+1, see levels
	Level   [NBands + 1]uint8

	// The beat comb's input (BeatTracker.Tick): the sum over the mid bands
	// (BeatFluxBand) of the positive two-frame log2 flux. levels computes it
	// because it already walks the bands; lvlHist holds the last two frames.
	lvlHist  [2][NBands + 1]int16
	beatFlux int32
	lvlOff   int32 // gain step (Q8 log2) not yet applied to lvlPeak and lvlHist

	frame  int32
	events Events
}

// Init resets the pipeline, including the filter state.
func (p *Pipeline) Init() {
	*p = Pipeline{}
	p.filters.reset()
	// Start at maximum gain: the tracker gives gain back slowly but takes it
	// away instantly, so a device that boots in a quiet room is ready for the
	// first drop without pumping. See agcUpdate.
	p.agcShift = AgcMaxShift
	p.filters.gainShift = p.agcShift
	p.lvlN = NBands + 1
}

// Process runs one analysis frame of Window samples. It needs no scratch
// buffer: the filter state lives in the pipeline, so the caller can hand it
// the ADC capture buffer directly.
func (p *Pipeline) Process(raw []uint16) Events {
	for _, v := range raw[:Window] {
		p.feed(v)
	}
	return p.endFrame()
}

// feed converts one raw ADC word and hands it to the filter bank, which does
// the DC blocking and applies the frame's gain (bank.run). See AgcTargetPeak
// for why that gain exists.
//
// There is deliberately no DC estimator here. An early version tracked the
// microphone bias with a 0.5 s IIR and it broke the beat lock: at that time
// constant the loop follows the *asymmetry of a kick waveform* - a bass-heavy
// bar has a real local mean - so the estimate wanders a few hundred counts in
// step with the music, and the subtraction modulates the bass band by an amount
// comparable to a quiet passage's whole signal. The bank's 30 Hz blocker
// removes the bias in ~30 ms and never looks at the music's envelope.
//
// Before the bank, isolated impulses are replaced by the median of the
// five-sample window centred on them. The LEDs couple into the microphone as
// single samples of up to +16381 codes, uncorrelated between the two earrings
// and following the animation's brightness, so left in they are most of every
// band's energy: the DC blocker turns each into a bass transient, the AGC's
// peak-to-peak level is the impulse, and the spectrum the pattern is read from
// is the LEDs'. Measured on bench noise, 3% of 90-code impulses read 2.68 bits
// of modulation depth raw, 1.99 after a three-point median and 1.06 after this
// five-point one, against 1.02 with no impulses (a pair of impulses on adjacent
// samples is common, and five is the smallest window that outvotes it).
//
// The window is centred - two samples of latency - because against a causal one
// a smooth waveform's deviation is its slope, and the turn of a bass note fires
// the test (8% of a clean recording replaced); centred, it is the curvature
// (0.1%). The neighbourhood's own motion is d2, the second-smallest of the four
// adjacent differences: unlike the neighbours' distance from the median it is
// not zero for a waveform near Nyquist, and it ignores up to two outliers.
// When the filter does not fire the sample passes unchanged: a median on every
// sample would erase a 6 kHz tone, a third of what the hash can see.
func (p *Pipeline) feed(v uint16) {
	x := int32(v)
	if p.spFill < 3 {
		p.spX1, p.spX2, p.spX3, p.spX4, p.spX5 = x, x, x, x, x
		p.spFill++
		if p.spFill < 3 {
			return
		}
	}
	p.spX1, p.spX2, p.spX3, p.spX4, p.spX5 = p.spX2, p.spX3, p.spX4, p.spX5, x
	c := median5(p.spX1, p.spX2, p.spX3, p.spX4, p.spX5)

	d1, d2, d3, d4 := abs32(p.spX1-p.spX2), abs32(p.spX2-p.spX3),
		abs32(p.spX3-p.spX4), abs32(p.spX4-p.spX5)
	if d1 > d2 {
		d1, d2 = d2, d1
	}
	if d3 > d4 {
		d3, d4 = d4, d3
	}
	if d1 > d3 {
		d3 = d1
	}
	if d2 > d4 {
		d2 = d4
	}
	if d2 > d3 {
		d2 = d3
	}
	dv := abs32(p.spX3 - c)
	if (d2 <= SpikeCodesQuiet && dv > SpikeCodesOutlier) ||
		dv > SpikeCodesOutlier+SpikeRelativeMul*d2 {
		p.spCount++
	} else {
		c = p.spX3
	}
	p.filters.run(c - 1<<15)
}

// median5 is the middle of five by a nine-comparator network (checked over
// every ordering in TestMedian5).
func median5(a, b, c, d, e int32) int32 {
	if a > b {
		a, b = b, a
	}
	if d > e {
		d, e = e, d
	}
	if c > e {
		c, e = e, c
	}
	if c > d {
		c, d = d, c
	}
	if a > d {
		a, d = d, a
	}
	if a > c {
		c = a
	}
	if b > e {
		b = e
	}
	if b > d {
		b = d
	}
	if b > c {
		c = b
	}
	return c
}

func abs32(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}

// agcUpdate runs once per frame: fold this frame's input peak into a slow
// peak-hold, then keep that hold inside [AgcLowPeak, AgcHighPeak] with a
// power-of-two gain. The constants carry the measurements and the shape of the
// band; the asymmetry is the other half of the design.
//
// Fall immediately, climb slowly, and stay put in between. A gain that is too
// high clips on the next transient and is corrected the same frame; a gain that
// is too low is silent, persistent, and changes the tempo the beat tracker
// locks to. But a change is *also* a discontinuity that the two earrings hit on
// different frames, so the band has to be wide enough that ordinary musical
// dynamics never reach either edge - otherwise the cure is worse than the
// disease, which an earlier single-threshold version demonstrated by scoring
// worse on the agreement test than no AGC at all.
//
// Net effect: a quiet device is lifted into the range where the pipeline makes the
// same decisions as a loud one, and once there it stops moving.
func (p *Pipeline) agcUpdate() {
	// Level measure: the frame's input swing in raw ADC counts. Peak-to-peak needs
	// no DC estimate (the bias cancels in max-min), and taking it before the gain and
	// the limiter means a loud input cannot hide behind a clipped sample - see
	// bank.minX/maxX.
	pk := p.filters.inputPeakPP()
	if pk > p.agcPeak {
		p.agcPeak = pk
	} else {
		p.agcPeak -= p.agcPeak >> AgcPeakDecay
	}
	prev := p.agcShift
	post := scalePow(p.agcPeak, p.agcShift)
	switch {
	case post > AgcHighPeak:
		// Straight to the largest gain that fits, not one bit a frame at a
		// time: the loud passage is already here. The scan runs below zero
		// because a device next to the PA needs real attenuation - see AgcMin-
		// Shift for why that is worth the bits it costs.
		want := int32(AgcMinShift)
		for s := int32(AgcMaxShift); s >= int32(AgcMinShift); s-- {
			if scalePow(p.agcPeak, s) <= AgcHighPeak {
				want = s
				break
			}
		}
		if want != p.agcShift {
			p.agcShift = want
			p.agcHold = AgcHoldFrames
		}
	case post < AgcLowPeak && p.agcShift < AgcMaxShift:
		if p.agcHold > 0 {
			p.agcHold--
		} else {
			p.agcShift++
			p.agcHold = AgcHoldFrames
		}
	}
	p.filters.gainShift = p.agcShift

	// Hide the step from everything downstream. The gain is a power of two, so
	// in the log2 domain it is an exact constant: band energy m (a sum of
	// magnitudes) scales by 2^delta, hence log2e(m) by exactly delta, i.e. 256
	// per bit in Q8. Adding that to the log-domain state makes the step
	// invisible to the hash (whose shape bits are already shift-invariant - see
	// TestHashLevelInvariant), and to the onset detector, which otherwise sees
	// the whole jump as flux: one bit of gain is 2304 counts of total against a
	// threshold in the tens, i.e. a guaranteed false onset on every gain change,
	// and a gain change is exactly the kind of moment the beat grid cannot
	// afford.
	if d := p.agcShift - prev; d != 0 {
		// The ladder's own state is stored at the old gain, so a step leaves
		// every rung one gain-factor away from its input and the poles chase
		// that error over their own time constants (~10 samples for the bottom
		// rung) as a transient the other earring does not have. Move the state
		// with the gain and there is nothing to chase.
		p.filters.rescaleGain(d)
		off := d << 8
		for i := range p.smooth {
			rolled.Loop()
			p.smooth[i] += off
			p.prevSmooth[i] += off
			p.smoothSnap[i] += off
		}
		// total = maxE + sum(e): NBands+1 terms, each moved by off.
		toff := off * (NBands + 1)
		p.prevTotal += toff
		p.prevTotal2 += toff
		p.level += toff
		// The bass detector runs on one band, so one offset.
		p.prevBass += off
		p.prevBass2 += off
		// The display levels' peaks and flux history: levels applies it.
		p.lvlOff = off
		// linPeak is a peak-hold of the *linear* sum, which scales with the
		// gain directly. It is only ever compared against a linear sum of the
		// same signal, so scaling it keeps the silence decision unchanged.
		if d > 0 {
			p.linPeak <<= uint(d)
		} else {
			p.linPeak >>= uint(-d)
		}
	}
}

// ProcessSample feeds a single sample, for a caller that is interrupted by the
// ADC at sample rate and wants to spread the filter work between captures.
// Call EndFrame every Window samples.
func (p *Pipeline) ProcessSample(v uint16) {
	p.feed(v)
}

// EndFrame finishes a frame of Window samples fed via ProcessSample.
func (p *Pipeline) EndFrame() Events { return p.endFrame() }

// BeatPeriod exposes the locked beat period (frames): the sync-mode
// visualization uses it to place the beat marker between beats, and host tools
// to print the tracker state.
func (p *Pipeline) BeatPeriod() int32 { return p.beat.Period }

//go:noinline
func (p *Pipeline) endFrame() Events {
	// 0. Fold this frame's input peak into the gain tracker.
	p.agcUpdate()

	// 1. Fold this frame's band energies.
	p.filters.fold(&p.energies)

	// 2. log2 energies (Q8).
	//
	// The relative floor is NOT applied here: it belongs on the smoothed
	// spectrum (step 5b), where it is stable and device-invariant. The raw
	// values here feed only the loudness trackers, which want true levels.
	var e [NBands]int32
	maxE := int32(-1 << 30)
	linear := int32(0)
	for i, m := range p.energies {
		e[i] = log2e(m)
		if e[i] > maxE {
			maxE = e[i]
		}
		// Saturating sum: 8 bands x up to 2^28 would not fit int32 otherwise.
		// Only used for silence detection, so saturating is harmless.
		if m > 1<<28 {
			m = 1 << 28
		}
		linear += int32(m)
	}

	// Silence: far quieter than the loudest recent frame. Relative, for the
	// same reason as the band floor.
	if linear > p.linPeak {
		p.linPeak = linear
	} else {
		p.linPeak -= p.linPeak >> LoudDecay
		if p.linPeak < LoudMinFloor {
			p.linPeak = LoudMinFloor
		}
	}
	p.silent = linear < int32(uint32(p.linPeak)/SilenceDiv)

	p.levels(&e, log2e(uint32(linear)))

	total := maxE
	if !p.silent {
		for i := range e {
			total += e[i]
		}
	}
	p.total = total

	// 4. Loudness tracking + flux onset detection on the raw total (the hash
	// deliberately carries no absolute level information, so it cannot be
	// used for this).
	onset := p.onsetDetect(total)

	// 4b. The beat tracker's own onset: the same detector on the bottom band
	// only. See the bassFloor field comment for the measurement.
	p.bassOnset = p.onsetDetectBass(e[NBands-1])
	if onset {
		p.onsetAge = 0
	} else if p.onsetAge < 100 {
		p.onsetAge++
	}
	p.prevTotal2 = p.prevTotal
	p.prevTotal = total
	p.level += (total - p.level) >> 6

	// 5. Slowly smoothed spectrum. The hash and the pattern both read this, not
	// the instantaneous frame.
	//
	// This turned out to be the crux of the whole design. Hashing the frame as
	// it lands gives a word that is nearly constant everywhere: the hash is
	// sampled on the beat, and on the beat there is always a kick, so every beat
	// in every section produced the same three shape bits and no temporal bits.
	// Measured over 2400 frames, novelty against the ring was 0 for 114 of the
	// 117 beats *on* a section change and 0 for 114 of the beats elsewhere - the
	// hash distinguished nothing at all, which also made the periodicity check
	// match at distance 0 on every beat.
	//
	// Averaging over about half a second removes the beat cycle and leaves the
	// arrangement: which instruments are actually playing. That is what differs
	// between a breakdown and a drop, and it is exactly what two devices can
	// agree on. The time constant is long enough to cover a beat at any tempo in
	// the tracked range, so the smoothing itself does not depend on where the
	// beat boundaries fell.
	if p.frame == 0 {
		p.smooth = e
	} else {
		for i := range e {
			rolled.Loop()
			// Rounded: an unrounded shift stalls upward forever once the
			// difference is below 2^SmoothShift (0.75 dB), while downward
			// always moves at least one LSB. That asymmetry biases the
			// smoothed spectrum low and makes it depend on signal polarity.
			p.smooth[i] += (e[i] - p.smooth[i] + (1 << (SmoothShift - 1))) >> SmoothShift
		}
	}
	// 5b. Apply the relative floor to the smoothed spectrum (see RelFloorQ8):
	// bands far below the smoothed maximum hold only noise, and the pin lands
	// identically for every device because a gain shifts max and floor
	// together. Silence floors every band to the same value, which is exactly
	// right: a silent spectrum must not look like an arrangement.
	{
		sm := p.smooth[0]
		for _, v := range p.smooth[1:] {
			rolled.Loop()
			if v > sm {
				sm = v
			}
		}
		pin := sm - RelFloorQ8
		for i := range p.smooth {
			if p.smooth[i] < pin {
				p.smooth[i] = pin
			}
		}
	}
	dev := spectralDevs(&p.smooth)

	// 5b'. The pattern feature: the spectral centroid and its own slow reference.
	//
	// A gain adds the same amount to every band and PatternWeights sums to zero, so
	// gain multiplies out here. A steady spectral tilt (microphone response, hand,
	// room) enters as a constant, which subtracting the device's own reference
	// removes. What is left is divided by the device's own spread in PatternIndex,
	// so a device whose centroid swings twice as far is not read as twice as
	// opinionated. Computed from the floored smooth, so silence gives a constant
	// centroid, the spread decays and PatternMinDev holds the middle pattern.
	c := int32(0)
	for i, v := range p.smooth {
		rolled.Loop()
		c += v * PatternWeights[i]
	}
	c *= PatternCentroidScale
	if p.frame < PatternSeedFrames {
		// Seeded on every frame until the front end has settled. Seeded once on
		// frame 0 instead, the bias step the DC blocker absorbs at power-on lands
		// in the centroid's slow statistics and takes minutes to leave them.
		p.centQ8, p.centAvgQ8, p.centDevQ8, p.centGapQ8 = c<<8, c<<8, 0, 0
	} else {
		p.centQ8 += ((c << 8) - p.centQ8 + (1 << (PatternCentroidShift - 1))) >> PatternCentroidShift
		gap := (p.centQ8 >> 8) - (p.centAvgQ8 >> 8)
		// Only the quantity being judged is low-passed. The ruler stays the spread
		// of the raw deviation: the spread of the smoothed one loses what the
		// low-pass removed while a steady drift keeps all of its deviation, and the
		// ratio then runs to the clamp and pins both devices to an end pattern.
		p.centGapQ8 += ((gap << 8) - p.centGapQ8 + (1 << (PatternGapShift - 1))) >> PatternGapShift
		abs := gap
		if abs < 0 {
			abs = -abs
		}
		p.centDevQ8 += ((abs << 8) - p.centDevQ8 + (1 << (PatternScaleShift - 1))) >> PatternScaleShift
		// The reference is an accumulation of the gap, not an average moving
		// toward it: written as ((gap<<8) - centAvgQ8) >> shift it chases the gap
		// instead of the centroid, runs to zero, and pins the ratio.
		p.centAvgQ8 += ((gap << 8) + (1 << (PatternScaleShift - 1))) >> PatternScaleShift
	}

	// 5c. The hash is recomputed every frame from a snapshot of the smoothed
	// spectrum, but its *temporal* field reads p.prevSmooth, which only
	// advances at ring pushes (below). Between beats that makes the per-frame
	// hash the current smoothed spectrum against the last beat's: a live "what
	// is rising underfoot" view. At the push itself this is exactly the hash
	// the ring stores.
	p.smoothSnap = p.smooth
	h := hashFrame(&p.smoothSnap, &p.prevSmooth, &dev)
	p.lastHash = h

	// 5d. Settle tracking. The hash *identity* - shape and hot fields, which
	// describe the section - must be unchanged for AnchorSettleFrames before
	// the hash is trusted; the temporal field is excluded because it follows
	// push phase and flickers even in steady state. The settling edge (the
	// frame the count reaches the threshold) is the one shot the anchor gets
	// per transition; see step 8.
	id := h & IdentityMask
	if id == p.stableID {
		p.stableFrames++
	} else {
		p.stableID = id
		p.stableFrames = 0
	}

	// 6. Beat tracker: advance phase, fold in the flux, possibly emit a beat
	// boundary. Locked, the phase comes from the all-band flux comb (see
	// Tick); before lock, kick-band onsets stand in for beats (see
	// onsetDetectBass - the full-spectrum onset there costs two TestAgreement
	// cases). Everything else in this function - the anchor, the energy gate,
	// the pattern triggers - keeps reading the full-spectrum onset, because a
	// section change is a broadband event.
	beatBoundary, barBoundary := p.beat.Tick(p.bassOnset, p.beatFlux, p.frame)

	// The events struct is state across frames, so it is rebuilt from scratch
	// every frame: a stale Hash here once made every frame look identical.
	p.events = Events{
		Hash:    h,
		HashAge: 1,
		Onset:   onset,
		Beat:    beatBoundary,
		Bar:     barBoundary,
		Locked:  p.beat.Locked,
		Silent:  p.silent,
		Pattern: p.lastPattern,
	}

	// 6b. Pattern backstop: every musical trigger in decide() needs beat lock and
	// a settled hash, and a device without them would otherwise hold its warmup
	// pattern while its partner changes. Safe on an arbitrary frame because the
	// choice is a function of the music alone. It must run after the events are
	// rebuilt above, or the change is made without PatternChanged being reported
	// and the display never follows it.
	if p.frame >= PatternWarmupFrames && p.framesSincePattern >= PatternMaxHoldFrames {
		p.repattern()
	}
	p.framesSincePattern++

	// 7. Beat-indexed ring pushes, gated on the hash having settled (see 5d):
	// the ring only ever holds settled section descriptors, so both novelty
	// and the periodicity match compare settled values against each other.
	// Before the tracker locks, treat onsets as beats so the ring still fills
	// (music with weak transients just fills slower).
	push := (beatBoundary || (!p.beat.Locked && onset)) && !p.silent &&
		p.stableFrames >= AnchorSettleFrames
	if push && p.frame >= WarmupFrames {
		p.decide(h, barBoundary)
		p.events.HashAge = 0
	}

	// 8. Transition anchor: a strong onset, above-average energy, and a hash
	// that has settled onto something far from the ring. Deliberately NOT
	// tied to a beat boundary - which frame the PLL emits a beat on is a
	// per-device detail, and an onset/beat phase gate made the anchor fire on
	// one device and not the other at the same musical transition. Onsets
	// themselves are gain-invariant and shared, and so is settling (both are
	// functions of the audio, not of the count).
	//
	// This is a level trigger, not an edge: it is evaluated on every frame
	// the hash is settled, and the AnchorRepeatDist guard (the hash must move
	// >=AnchorRepeatDist identity bits from the one that last anchored) is
	// what makes it fire once per section rather than once per frame. An edge
	// trigger (stableFrames == settle) was measurably worse: it required every
	// other gate to align on the single settling frame, so a device that
	// happened to be between onsets that frame skipped the whole transition.
	if p.stableFrames >= AnchorSettleFrames &&
		p.frame >= WarmupFrames && p.ringCount >= 4 && !p.silent &&
		p.onsetAge <= AnchorOnsetMax &&
		p.frame-p.lastAnchorFrame >= AnchorRefractoryFrames &&
		total > p.level+(1<<7) &&
		p.noveltyMin(h) >= DMin &&
		hashDist(h&IdentityMask, p.lastAnchorHash&IdentityMask) >= AnchorRepeatDist {
		p.lastAnchorFrame = p.frame
		p.lastAnchorHash = h
		p.events.Anchor = true
		// Snap the pattern to the new section immediately, ignoring the
		// pattern refractory: this is the moment all the earrings are meant
		// to realign on. repattern resets the refractory itself when the
		// snap actually changes something; deliberately NOT zeroing it here
		// unconditionally - measured, that starved the bar-driven re-read
		// whenever anchors fired closer together than a bar (the tuned hash
		// fires them at transitions *and* jitters across sections), so the
		// pattern stopped tracking the music and cross-device agreement
		// dropped from 82% to the 40s. Deliberately *not* resetting the beat counter: the
		// ring is indexed by it, so zeroing it would shift this device's
		// ring relative to the others' and "4 bars ago" would mean
		// different music on each earring. The tracker's PLL already keeps
		// the count on the music.
		p.repattern()
	}

	// 9. Tempo: every TempoStep frames, correlate the recent envelope against
	// itself. This is what decides the period; the interval histogram above
	// only provides the early beats before the ring is full.
	p.pushEnv(total)
	if uint32(p.frame)%TempoStep == TempoStep-1 {
		p.tempoScan()
	}

	p.frame++
	return p.events
}

// levels updates the display levels from this frame's log2 band energies e
// and the log2 of their sum. See LevelRangeQ8; nothing else reads them.
//
// One loop, and its trip count is a field: with the constant NBands+1 the
// firmware build (-opt=2) unrolls it, and the two loops this used to be cost
// 744 B of flash. The noise floor comes from the previous frame's top peak for
// the same reason, which a frame late (16 ms) does not change visibly.
//
//go:noinline
func (p *Pipeline) levels(e *[NBands]int32, all int32) {
	off := p.lvlOff // a gain step since the last frame (see feed)
	p.lvlOff = 0
	floor := p.lvlTop + off - RelFloorQ8
	top := int32(-1 << 30)
	flux := int32(0)
	for i := range p.lvlN {
		v := all
		if i < NBands {
			v = e[i]
		}
		if uint(i-BeatFluxBand) < BeatFluxBands {
			flux += max(v-int32(p.lvlHist[1][i])-off, 0)
		}
		p.lvlHist[1][i] = p.lvlHist[0][i] + int16(off)
		p.lvlHist[0][i] = int16(v)
		pk := max(p.lvlPeak[i]+off-LevelPeakDecayQ8, v)
		p.lvlPeak[i] = pk
		top = max(top, pk)
		x := (v - max(pk, floor) + LevelRangeQ8) >> LevelShift
		x = min(max(x, int32(p.Level[i])-LevelRelease), 255)
		if x < 0 || p.silent {
			x = 0
		}
		p.Level[i] = uint8(x)
	}
	p.lvlTop = top
	p.beatFlux = flux
}

// pushEnv stores this frame's loudness in the tempo ring, relative to the
// slow level so the stored value is a small deviation rather than a large
// absolute level (and so a gain step does not show up as a modulation).
//
//go:noinline
func (p *Pipeline) pushEnv(total int32) {
	// envRef is deliberately slow (8 s, twice the ring itself). Subtracting a
	// faster baseline - the level tracker's 1.6 s average, for the first try -
	// high-passes the ring at a period close to the beat, which both eats the
	// modulation and adds a strong short-lag correlation of its own: the scan
	// then picks BeatMinPeriod on every sample. At 8 s the reference barely
	// moves over one ring trip, and the residual DC is removed per window
	// inside tempoScan.
	if p.silent {
		// Store a flat value rather than the silence cliff: a step from music to
		// silence correlates at every lag and outranks any tempo.
		total = p.envRef
	}
	if p.envHead == 0 {
		// Start the baseline at the first sample rather than at zero, or the
		// whole first trip around the ring saturates at +127 and the first scan
		// - the one that decides the initial tempo - correlates a clipped square.
		p.envRef = total
	}
	p.envRef += (total - p.envRef) >> 9
	v := (total - p.envRef) >> 4
	if v > 127 {
		v = 127
	} else if v < -128 {
		v = -128
	}
	// The ring's sum and sum of squares are kept current here, two adds and two
	// subtracts, so tempoScan needs no pass of its own to find the mean or the
	// variance: expanding (v-mean)*(v'-mean) over the full ring leaves only the
	// raw dot product in the inner loop.
	old := int32(p.env[p.envHead&EnvMask])
	p.envSum += v - old
	p.envSq += v*v - old*old
	p.env[p.envHead&EnvMask] = int8(v)
	p.envHead++
}

// tempoScan picks the beat period from the loudness envelope and the beat phase
// from the comb position that sits on the kicks. It is the reason the grid
// matches the music at all.
//
// What it replaced cannot: the beat tracker used to vote on the intervals
// between detected onsets, and those onsets are barely related to the kicks.
// Measured against the played file (tools/onset_quality.py), pipeline onsets
// land within two frames of the true beat grid 28% of the time, where a random
// spike train scores 18%; a flux detector restricted to the bass band scores
// 27%. Voting on such a series locks onto a periodicity of the detector, not of
// the music - and it did so reproducibly: period 19-20 frames (200 BPM) on a
// 142 BPM piece, on every device, at every level, while the earrings agreed
// with each other and with nothing else in the room.
//
// The envelope does carry the beat. Correlated over TempoWindow it peaks at the
// true lag; the per-window pick is right 57-61% of the time on the five real
// samples, which is not good enough on its own, so each candidate's evidence is
// accumulated across scans (TempoEvidenceShift) and the accumulated maximum is
// what decides. That settles on the correct period on all five and stays there.
//
// Cost: 32 lags x TempoWindow products every TempoStep frames - an 8k-MAC burst
// every 2 s - plus one division per candidate. Everything is int8 x int8 into an
// int32 accumulator: 128*128*256 = 4.2M, no saturation, no float. The ring is
// read in place; materialising a TempoWindow-entry int32 copy would cost 1 KB of
// stack in a build whose whole stack is 1 KB.
//
// ringDot correlates the envelope ring against a rotated copy of itself:
// sum(v[i]*v[i+lag]) around the whole ring. Circular rather than the one-sided
// autocorrelation on purpose - it does not depend on where in the ring the
// window starts, so the loops need neither a base offset nor a length that
// varies with the lag, and the wrapped terms are 256 frames apart, far outside
// any tempo. The two loops below are the two sides of that wrap; a single loop
// with a mask inside measures the same thing and costs a compare in the hottest
// place on a machine with no spare flash.
//
// Called three times per candidate lag - 96 calls every 2 s, so the call
// overhead is noise, and one copy of the body is what keeps the scan affordable.
//
//go:noinline
func (p *Pipeline) ringDot(lag int32) int32 {
	sum := int32(0)
	for i, v := range p.env[:TempoWindow-lag] {
		sum += int32(v) * int32(p.env[i+int(lag)])
	}
	for i, v := range p.env[TempoWindow-lag:] {
		sum += int32(v) * int32(p.env[i])
	}
	return sum
}

//go:noinline
func (p *Pipeline) tempoScan() {
	if p.envHead < TempoWindow {
		// The first few seconds are a partial window: correlating a ring that is
		// half zeros measures how fast the music started, not its tempo.
		return
	}
	b := &p.beat
	const nLag = BeatMaxPeriod - BeatMinPeriod + 1
	mean0 := p.envSum / TempoWindow
	// Expanding the mean-removed correlation over a full cycle of the ring
	// cancels the two cross terms against the sum, and leaves a constant that is
	// the same for every candidate lag:
	//
	//	sum((v[i]-m)*(v[i+l]-m)) = sum(v[i]*v[i+l]) - W*m*m
	//
	// so the inner loop below is a raw dot product of int8 values, and the
	// variance is one multiply. On a machine with no hardware divide and a
	// 1 KB stack this is what makes the scan cheap: no scratch array, no
	// per-element subtraction, and the two halves of the wrapped ring are each a
	// straight-line loop.
	corr0 := mean0 * mean0 * TempoWindow
	energy := p.envSq - corr0
	if energy < TempoScale {
		// A flat ring: nothing correlates with anything.
		return
	}
	// Both score and energy are totals over the ring, so their ratio needs no
	// per-lag normalisation - and the division is written as sm/(energy/scale)
	// rather than sm*scale/energy because either total can reach 4.2M, and
	// 4.2M*1024 does not fit in an int32.
	// Correlation against a shifted copy of itself, over the full ring. Circular
	// rather than the textbook one-sided autocorrelation on purpose: it is
	// invariant to where in the ring the window happens to start, so the loops
	// need neither a base offset nor a length that varies with the lag. The
	// wrapped terms are 256 frames apart, far outside any tempo, and cost a
	// little noise.
	// The three candidates that go into each smoothed score roll through three
	// variables rather than an array: filling one would be a second pass over
	// every lag, and 128 bytes of stack in a build whose whole stack is 1 KB.
	score0 := p.ringDot(BeatMinPeriod) - corr0
	prev, cur := score0, score0

	// Smooth over neighbouring lags - lag-1 and lag+1 are the same tempo to
	// within a frame, and a knife-edge win on one bin over its neighbour is
	// exactly the tie two devices would split - normalise by the envelope's own
	// variance, and fold into the running evidence.
	step := int32(uint32(energy) / TempoScale) // energy >= TempoScale here
	best := 0
	bestEv := int32(-1 << 30)
	for k := 0; k < nLag; k++ {
		// Three candidates roll through three variables rather than living in an
		// array: 128 bytes of stack in a build that has 1 KB, for a pass that a
		// rolling window does for free.
		next := cur
		if k+1 < nLag {
			next = p.ringDot(BeatMinPeriod+1+int32(k)) - corr0
		}
		// Low-pass across neighbouring lags: lag-1 and lag+1 are the same tempo
		// to within a frame, and a knife-edge win on one bin over its neighbour is
		// exactly the tie two devices would split. (Tried replacing this with a
		// second-difference comb 2cur-prev-next to cancel the weak-signal
		// short-lag trend bias: on clean audio it collapsed the evidence (~18x)
		// and mislocated the peak, 1800 lost lock and 2700 jumped to lag 46. The
		// envelope is already a smooth loudness curve; differencing it into a
		// spike train throws away the periodicity the scan depends on. REVERTED.)
		sm := (prev + 2*cur + next) >> 2
		prev, cur = cur, next
		q := sm / step
		// |q| <= TempoScale by Cauchy-Schwarz - a correlation of a signal with a
		// shifted copy of itself cannot exceed its own energy - so the evidence
		// fits in an int16 with no clamp, and so does any average of it.
		k1 := int32(b.ev[k])
		if b.evInit {
			k1 += (q - k1) >> TempoEvidenceShift
		} else {
			k1 = q
		}
		b.ev[k] = int16(k1)
		if k1 > bestEv {
			bestEv = k1
			best = k
		}
	}
	b.evInit = true
	if bestEv < TempoMinEvidence {
		// No tempo to be found: the music has no steady kick in this stretch.
		// Say nothing rather than commit to a period that will be wrong for as
		// long as it takes the accumulator to notice.
		return
	}
	period := BeatMinPeriod + int32(best)
	b.Period = period
	// The fractional period: the vertex of the parabola through the evidence
	// at best and its neighbours. 143 BPM is 26.85 frames; a whole-frame period
	// walks the grid off the kicks by a frame every seven beats.
	perQ8 := period << 8
	if best > 0 && best < nLag-1 {
		l, c, r := int32(b.ev[best-1]), bestEv, int32(b.ev[best+1])
		if den := 2*c - l - r; den > 0 {
			perQ8 += (r - l) * 128 / den
		}
	}
	// pos may now be past the end of a shorter cycle; Tick's subtraction brings
	// it back within a few frames, and the comb has room for the longest period.
	b.perQ8 = perQ8
	// The comb starts empty at lock and fills within a few beats. (Locking used
	// to set a starting phase - first the loudest comb position in the
	// envelope, then a fixed one - and both were ~chance: the comb in Tick does
	// that job from the kick flux.)
	b.Locked = true
}

// onsetDetect is a cheap flux detector on the log-domain frame total, with a
// leaky noise floor and a refractory period.
//
// The flux spans two frames (total now vs total two frames ago). A kick
// attack rises over two or three frames, and a one-frame delta crosses the
// threshold on whichever frame its *increment* first exceeds it - which one
// that is varies by a frame between kicks, and at 64 fps one frame is 16 ms
// of interval jitter. Measured at 16 kSPS with a one-frame delta, consecutive
// kick intervals spread over 18..36 frames for a constant 143 BPM tempo, and
// the beat histogram never accumulated LockCount votes. The two-frame delta
// captures the whole attack in one number whichever frame the rise starts
// on, which is exactly how it behaved at the old 8.8 kSPS frame rate, where
// one frame already covered the attack (intervals then clustered in two
// bins).
func (p *Pipeline) onsetDetect(total int32) bool {
	flux := total - p.prevTotal2
	absFlux := flux
	if absFlux < 0 {
		absFlux = -absFlux
	}
	p.fluxFloor += (absFlux - p.fluxFloor) >> 5

	onset := false
	if flux > p.fluxFloor+(p.fluxFloor>>1) &&
		p.frame-p.lastOnsetFrame >= BeatRefractoryFrames {
		onset = true
		p.lastOnsetFrame = p.frame
	}
	return onset
}

// onsetDetectBass is onsetDetect for a single band: the kick band.
//
// Kept separate from onsetDetect rather than parameterised because the two
// need different refractory references (the full-spectrum detector must not be
// quieted by a kick, and vice versa) and the bass one never needs the silence
// special case: a silent frame has no kick.
//
//go:noinline
func (p *Pipeline) onsetDetectBass(bass int32) bool {
	flux := bass - p.prevBass2
	absFlux := flux
	if absFlux < 0 {
		absFlux = -absFlux
	}
	p.bassFloor += (absFlux - p.bassFloor) >> 5

	onset := false
	if flux > p.bassFloor+(p.bassFloor>>1) &&
		p.frame-p.beat.lastOnset >= BeatRefractoryFrames {
		onset = true
	}
	p.prevBass2 = p.prevBass
	p.prevBass = bass
	return onset
}

// decide runs the periodicity role of the hash at a (possibly virtual) beat
// boundary where the hash has settled: store it in the ring, and switch the
// pattern when the music came back around or a bar went by. h is the per-frame
// hash from endFrame; at this point it is exactly the hash of the current
// smoothed spectrum against the previous push's.
//
// The transition anchor is *not* here: it fires per-frame on the hash settling
// edge (endFrame step 8), because a beat-phase gate was measurably device-
// specific while onsets and settling are not.
//
//go:noinline
func (p *Pipeline) decide(h uint32, barBeat bool) {
	b := &p.beat

	// The temporal reference advances now, at a musical position: the next
	// hash (per-frame or at the next push) diffs against this spectrum.
	p.prevSmooth = p.smoothSnap

	// Periodicity confirmation - did the music come back around?
	//
	// Compared with sectionDist, so "the same music" means the same *section*
	// (shape and hot fields); see that function for why the temporal field is
	// excluded.
	//
	// Each nominal lag (2, 4, 8 bars) is searched over a window of
	// ±LagTolerance beats rather than read at one exact index: two earrings
	// count beats independently from their own lock points, so their ring
	// indices are routinely off by a beat and an exact lookup would compare
	// different material.
	//
	// Note the result is a yes/no, *not* which lag matched. Which one wins is a
	// knife-edge decision (two candidate lags are often within one bit of each
	// other, and the winner then differs per device), and measured against the
	// agreement test it was worth more as a robustness problem than as musical
	// information. The spec only asks that a periodicity confirmation switches
	// the pattern, not that the lag selects it.
	returned := false
	if b.Locked {
		// LagBars1, LagBars2, LagBars3, by doubling: 68 B less than a range
		// over the three, which -opt=2 unrolled.
		for nominal := int32(LagBars1 * BeatsPerBar); nominal <= LagBars3*BeatsPerBar; nominal *= 2 {
			for off := -LagTolerance; off <= LagTolerance; off++ {
				lag := nominal + int32(off)
				if lag < 1 || b.beat < lag || p.ringCount <= lag {
					continue
				}
				prev := p.ring[(b.beat-lag)&(RingSize-1)]
				if prev != 0 && ringDist(h, prev) <= DAccept {
					returned = true
					break
				}
			}
			if returned {
				break
			}
		}
	}

	p.events.Returned = returned

	// Store the current hash at the current beat.
	p.ring[b.beat&(RingSize-1)] = h
	if p.ringCount < RingSize {
		p.ringCount++
	}

	// The pattern follows the music (see PatternIndex); the events below only
	// decide *when* it is re-read. Re-reading is a no-op when the centroid has
	// not crossed a cut, so nothing is gained by reading it rarely - and
	// something is lost, because a device that fell out of step is only corrected
	// at the next re-read.
	//
	// The bar boundary is one of the triggers, which is what keeps changes on a
	// musical beat rather than mid-figure. Two devices' bar boundaries land a few
	// frames apart at most, and both read essentially the same spectrum there, so
	// this trigger cannot by itself cause a divergence the way an event that only
	// one device sees would.
	if (returned || barBeat) && p.beatsSincePattern >= PatternRefractoryBeats {
		p.repattern()
	}
	p.beatsSincePattern++
}

// repattern re-reads the pattern and, if it moved, switches to it and reports it
// in the events. The refractory is counted in beats whether or not the pattern
// actually changed, so a device that keeps re-reading the same pattern still
// waits the full window before the next look. Not inlined: it has three callers
// and the flash has no room for three copies.
//
//go:noinline
func (p *Pipeline) repattern() {
	pat := p.pattern()
	if pat == p.lastPattern {
		return
	}
	p.lastPattern = pat
	p.beatsSincePattern = 0
	p.framesSincePattern = 0
	p.events.PatternChanged = true
	p.events.Pattern = pat
}

// noveltyMin returns the minimum section distance (see sectionDist: identity
// fields only) between h and the ring, excluding the ring's leading run of
// matches: entries pushed since h's own
// section settled are within DAccept of h by definition, and counting them
// would drive novelty to zero in exactly the moment the anchor is meant to
// fire. A ring that is *entirely* within tolerance is not novel either - it
// means the current section has been playing for the whole window (or the
// previous section was the same one, drop into drop): that returns 0.
//
//go:noinline
func (p *Pipeline) noveltyMin(h uint32) int32 {
	minD := int32(HashBits)
	seenOther := false
	n := p.ringCount
	if n > NoveltyWindow {
		n = NoveltyWindow
	}
	for i := int32(1); i <= n; i++ {
		other := p.ring[(p.beat.beat-i)&(RingSize-1)]
		d := ringDist(h, other)
		if !seenOther && d <= DAccept {
			continue // still inside the section h describes
		}
		seenOther = true
		if d < minD {
			minD = d
		}
	}
	if !seenOther {
		return 0
	}
	return minD
}

// spectralDevs fits a straight line through the eight log-band energies (x =
// band index) and returns each band's deviation from it, in Q8<<8 units.
//
// Deviation from the fitted line is the invariant view of the spectrum: a
// device's gain adds a constant to every band, which moves the intercept and
// leaves every deviation unchanged, and the steady downward slope that
// microphone response and EDM's low end impose on all music is absorbed into
// the fitted line. What remains is where the energy actually sits: kick, mids,
// or air.
//
// Comparing neighbours instead (sign(E_b - E_b+1)) does not work: the log
// spectrum of real music is monotone-ish, so all seven bits get the same value
// and the hash collapses to a handful of words. That was measured, not guessed.
//
// The fit is exact least squares over NBands = 8 points with x = 0..7, so
// Sum(x) = 28, Sum(x^2) = 140 and the denominator
//
//	n*Sum(x^2) - Sum(x)^2 = 8*140 - 784 = 336
//
//	slope     = (8*Sum(xy) - 28*Sum(y)) / 336
//	intercept = (Sum(y) - 28*slope) / 8
//
// The division by 336 becomes a multiply by 256/336 = 0.7619 in Q16, because
// Cortex-M0+ has no divide. The slope product is done in two 32-bit halves
// (mulQ16) so no 64-bit helper code is needed.
//
//go:noinline
func spectralDevs(e *[NBands]int32) [NBands]int32 {
	sy := int32(0)
	sxy := int32(0)
	for b := 0; b < NBands; b++ {
		sy += e[b]
		sxy += e[b] * int32(b)
	}
	// num fits int32 with room to spare: energies are Q8 log values below
	// 2^13, so |num| = |8*Sum(xy) - 28*Sum(y)| <= 2^5 * 8192 = 2^18. Only its
	// product with the reciprocal needs more than 32 bits, and mulQ16 does
	// that in two half-width multiplies: a uint64 here would pull the soft
	// 64-bit helpers into the binary for one multiply per frame.
	num := 8*sxy - 28*sy
	slope := mulQ16(num, 49939)
	intercept := (sy << 5) - (slope*7)>>1
	var dev [NBands]int32
	for b := 0; b < NBands; b++ {
		dev[b] = (e[b] << 8) - intercept - slope*int32(b)
	}
	return dev
}

// shapeCode returns 16 bits: a two-bit code per band (except the bottom one)
// of how far the band stands above the fitted spectral line.
//
// The bottom band is left out on purpose. It is a plain lowpass with no upper
// edge, so in EDM it holds the kick and bass and sits far above the fitted
// line whatever the music does: measured, its bit was set in nearly every
// frame. A constant field is worse than no field, because it dilutes the
// Hamming distances the ring matching relies on.
//
// Two bits per band rather than one: one bit ("above a threshold") collapses
// whole classes of arrangement to the same value, because the part of the
// spectrum that stands above the line moves by more than one band between,
// say, a pad-and-hats intro and a kick-and-bass drop. The 0/1/2 codes put
// that structure into the word: which bands stand out, and by how much.
//
// This field replaced both a one-bit shape field and a "hot band" field
// (within N dB of the loudest), both of which measured, on the full synth
// structure, into saturation: the music spans less than the floor distance
// end to end, so every bit was set in every section and the hash collapsed
// again. The quantised deviation has no global threshold to saturate against:
// the fitted line is computed from the spectrum itself.
//
//go:noinline
func shapeCode(dev *[NBands]int32) uint32 {
	var w uint32
	for b := 0; b < NBands-1; b++ {
		var code uint32
		switch {
		case dev[b] > ShapeHiQ8<<8:
			code = 2
		case dev[b] > ShapeLoQ8<<8:
			code = 1
		}
		w |= code << (2 * uint(b))
	}
	return w << ShapeShift
}

// mulQ16 returns (n * q) >> 16 (arithmetic shift, so floor division toward
// -infinity) using only 32-bit multiplies, for |n| < 2^24 and q < 2^16. The
// magnitude is split into halves and the result re-signed at the end; doing
// it on the two's-complement bits directly would leak q*2^16 into the low
// word for negative n. The unit test checks it against int64 arithmetic.
//
//go:noinline
func mulQ16(n int32, q uint32) int32 {
	neg := n < 0
	u := uint32(n)
	if neg {
		u = 0 - u
	}
	lo := (u & 0xffff) * q
	p := (u>>16)*q + lo>>16
	if neg {
		p = -p
		if lo&0xffff != 0 {
			p-- // floor of a negative product rounds away from zero
		}
	}
	return int32(p)
}

// hotBits returns the eight "hot band" bits: band b sits within HotFloorQ8 of
// the loudest band of the (smoothed) spectrum.
//
// This is the field that gives each arrangement its identity, and it replaced
// an earlier "contrast against a 5 s average" field that measured how long
// since the music changed rather than what the music is: a slow average never
// settles inside an 8 s section, so the hash drifted continuously, two plays
// of the same section hashed differently (periodicity could not match), and
// late in a track every section converged to the same word (anchors could not
// fire). Both failures measured, both fixed by this field.
//
// Hotness is the identity-like view that survives: it says which parts of the
// spectrum are actually carrying sound - pad-and-hats lights different bands
// than kick-and-bass - and it is invariant to gain (max and floor shift
// together) and to a fixed EQ tilt (both sides of the comparison move). It
// settles as fast as the smoothing settles, so the hash means the same thing
// at the start and the end of a section, which is what the ring needs.
//
// The threshold is HotFloorQ8, deliberately *not* the pin's RelFloorQ8: the
// first version here reused that 30 dB floor and measured straight into
// saturation - EDM spans less than 30 dB end to end across the band set, so
// every band was hot in every section and the field carried nothing. A few dB
// below the peak is where the arrangement actually differentiates.
//
//go:noinline
func hotBits(cur *[NBands]int32) uint32 {
	var w uint32
	max := cur[0]
	for _, v := range cur[1:] {
		if v > max {
			max = v
		}
	}
	floor := max - HotFloorQ8
	for b := 0; b < NBands; b++ {
		if cur[b] >= floor {
			w |= 1 << uint(ContrastShift+b)
		}
	}
	return w
}

// PatternIndex maps the centroid's deviation from its reference, in units of its
// own spread, onto one of NumPatterns ordered buckets.
//
// It has no memory, and that is a requirement. The previous selector (argmax over
// bands, with an incumbent that a challenger had to beat by a margin) measured on
// the bench at 57% visibly matching and 36% visibly stuck on different patterns:
// the incumbent is per-device state with no channel between the earrings, and the
// margin exceeded the lead the true winner usually held, so once divided neither
// device ever yielded. The same spectra scored 81% with the memory removed. The
// change rate is limited by the commit gates instead (bar boundaries,
// PatternRefractoryBeats), which do not make the choice depend on what this device
// happened to be showing.
//
// The buckets are ordered, so two devices that disagree land on neighbouring
// patterns. A spread under PatternMinDev reports the middle pattern: that is a
// still device in a quiet room, and also the division guard (on -panic=trap a
// division by zero resets the device).
func PatternIndex(gap, spread int32) int32 {
	if spread < PatternMinDev {
		return NumPatterns / 2
	}
	// Clamped before the scaling: the bound is a multiple of the divisor, so the
	// product cannot wrap and the quotient stays within +-512.
	if lim := spread * 2; gap > lim {
		gap = lim
	} else if gap < -lim {
		gap = -lim
	}
	z := gap * 256 / spread
	pat := int32(0)
	if z > PatternCut1 {
		pat++
	}
	if z > PatternCut2 {
		pat++
	}
	if z > PatternCut3 {
		pat++
	}
	return pat
}

// pattern reads the pattern from the low-passed centroid deviation, reporting the
// middle pattern until the reference and spread are worth reading: with a spread
// near zero every cut sits on the reference, and two earrings that booted a few
// seconds apart would start on different patterns.
func (p *Pipeline) pattern() int32 {
	if p.frame < PatternWarmupFrames {
		return NumPatterns / 2
	}
	return PatternIndex(p.centGapQ8>>8, p.centDevQ8>>8)
}

// hashFrame computes the 24-bit perceptual hash of a smoothed band spectrum
// (log2 energies, Q8, floored):
//
//	bits  0..7:  temporal   band b rose since the previous push
//	bits  8..21: shape      two-bit code per band (b = 0..NBands-2): how far
//	                         the band stands above the fitted spectral line
//	bits 22..29: hot        band b within HotFloorQ8 of the loudest band
//
// The width is a deliberate choice, not an accident: the spec allows a 15-32
// bit word. Twenty-nine bits carry three fields without one of them going
// dead - narrower layouts measured into either saturation or collapse - and
// the whole word still fits one register, so Hamming distance stays a few
// instructions.
//
// The three fields answer complementary questions, and every one of them is a
// comparison that a device's gain cancels - which is what makes two earrings
// agree with no AGC anywhere: shape says *where* the energy sits and how
// strongly, hot says *how wide* the active part of the spectrum is
// (arrangement identity), and temporal says what is rising underfoot (motion,
// transients).
//
//go:noinline
func hashFrame(cur, old, dev *[NBands]int32) uint32 {
	var w uint32
	for b := 0; b < NBands; b++ {
		if cur[b]-old[b] > TemporalDeadband {
			w |= 1 << uint(b)
		}
	}
	return w | shapeCode(dev) | hotBits(cur)
}

// hashDist is the Hamming distance between two hashes over the bits the hash
// actually uses. Written as the classic bit trick rather than
// math/bits.OnesCount32 because Cortex-M0+ has no POPCNT instruction and
// math/bits carries an emulation cost there.
//
//go:noinline
func hashDist(a, b uint32) int32 {
	x := (a ^ b) & HashMask
	x -= (x >> 1) & 0x55555555
	x = (x & 0x33333333) + ((x >> 2) & 0x33333333)
	x = (x + (x >> 4)) & 0x0f0f0f0f
	return int32((x * 0x01010101) >> 24)
}

// ringDist is the distance used for every ring comparison: the periodicity
// confirmation (decide) and the anchor's novelty gate (noveltyMin). It is the
// full 29-bit word.
//
// Measured, and deliberately not identity-only. Masking the temporal field out
// of these comparisons was tried on the device (tools/ringab.sh, 6 runs each
// way, same clip, same earring, LEDs dark, 15 run pairs per mode): anchor F1
// fell from 0.29 to 0.00 median and anchors per run from 5-9 to 1-7, and the
// periodicity confirmation agreed no better (returned F1 0.53 -> 0.41). The
// reason is the anchor's novelty gate: it asks for the current section to be at
// least DMin away from everything in the ring, and the temporal field's churn
// is worth ~2 bits, so comparing identity only makes every section look less
// novel and the anchor stops firing. Firing is what matters - the anchor is the
// one event that realigns two earrings that drifted, and a realignment signal
// that never fires is worth nothing however precise it is.
//
// The intuition that the temporal field should be excluded is not crazy (two
// runs of one clip disagree on ~27% of those bits at the same musical instant,
// and step 5d does exclude it from settle tracking), but the churn is not
// independent noise: it is driven by onsets, which both devices hear. So it
// carries real "something is happening now" evidence, and the host harness -
// whose device models all share an identical clock and so have no phase churn at
// all - agreed with the intuition for exactly that reason. Settle it on the
// device.
func ringDist(a, b uint32) int32 {
	return hashDist(a, b)
}

// log2e returns log2(v) in Q8 fixed point, for v up to 2^32-1 (band energies
// are uint32). The MSB search is a binary search written with plain shifts so
// this file needs no imports (and specifically no math/bits emulation code on
// the device); on Cortex-M0+ this is ~5 single-cycle shifts plus ~5 compares.
// Keeping this 32-bit matters: a uint64 parameter pulls in the soft 64-bit
// shift helpers, both in flash and in cycles.
//
//go:noinline
func log2e(v uint32) int32 {
	if v == 0 {
		return 0
	}
	// Binary search for the MSB position; afterwards n = index of the MSB
	// (0..31) and v has that MSB at bit 31.
	n := uint(0)
	if v>>16 == 0 {
		v <<= 16
	} else {
		n += 16
	}
	if v>>24 == 0 {
		v <<= 8
	} else {
		n += 8
	}
	if v>>28 == 0 {
		v <<= 4
	} else {
		n += 4
	}
	if v>>30 == 0 {
		v <<= 2
	} else {
		n += 2
	}
	if v>>31 == 0 {
		v <<= 1
	} else {
		n += 1
	}
	// The 8 bits below the MSB form the fraction.
	frac := (v >> 23) & 0xff
	return int32(n)<<8 | int32(frac)
}

// BeatTracker keeps a beat phase locked to the kicks (see Tick),
// so that the hash ring can be indexed by musical position (§D3).
type BeatTracker struct {
	Locked bool
	Period int32 // beat period in analysis frames, valid when Locked
	beat   int32 // monotonic beat count since lock; the ring index
	bar    int32
	// lastOnset is the frame of the previous onset, which is what the
	// refractory period is measured against. The interval histogram that used
	// to live here is gone: it voted on onset intervals, and onset intervals
	// turned out to carry no tempo information at all (see tempoScan).
	lastOnset int32
	// ev is the running evidence per candidate period, Q10 normalised
	// correlation of the loudness envelope (tempoScan). int16 is enough: the
	// values are clamped to that range by the accumulator. This is the memory
	// the interval histogram used to occupy, and the reason it is 64 bytes
	// rather than 32: onset intervals turned out to say nothing about tempo.
	ev     [BeatMaxPeriod - BeatMinPeriod + 1]int16
	evInit bool
	// comb is the leaky average onset flux at each position in the beat cycle;
	// pos is the position in Q8 frames (perQ8 the fractional period), best the
	// bin with the most flux, where the beat goes. since counts frames since
	// the last beat.
	comb  [BeatMaxPeriod + 1]int16
	perQ8 int32
	pos   int32
	best  int32
	since int32
}

// Tick advances the beat cycle by one frame and folds in this frame's onset
// flux x (>= 0, Q8 log2; Pipeline.beatFlux) and onset. It returns
// (beatBoundary, barBoundary) for this frame.
//
// Locked, the beat goes where the comb holds the most flux. The comb replaced
// a phase-locked loop that pulled the grid toward each bass onset: those
// onsets are as often a bass note or a fill as a kick, and every one of them
// moved the grid, so two earrings rarely agreed on where the beat was.
// Averaged over eight beats the kick wins. Measured against a kick-based
// reference (librosa's tracker on the mel bands below 200 Hz) on three
// 10-minute stretches of an EDM set, through the four device scenarios: beats
// within 70 ms of a kick 31% -> 60%, both earrings' beats within 32 ms of each
// other 39% -> 82% (beat_eval_test.go; bass-band flux alone: 56%, 73%).
//
// The flux comes from the mid bands only (BeatFluxBand). Summed over all
// bands, the bench earrings settled on opposite beats: on this music the hats
// and the bassline both rise on the off-beat, the kick wins or loses against
// them by a small margin, and which way it goes depends on how loud each
// microphone hears the top and the bottom (room, placement). Replayed from
// both earrings' recorded band energies on three stretches, mid bands against
// all bands: beats within 32 ms 33% -> 53%, half a beat apart 48% -> 23%,
// beats on the kick unchanged (56% -> 57%); of all 3^9 band weightings in
// {0,1,2} none did better on agreement. The host scenarios hear one spectrum
// on both earrings and cannot show this (pair 0.82 either way).
//
//go:noinline
func (b *BeatTracker) Tick(onset bool, x int32, frame int32) (bool, bool) {
	barBoundary := false
	if onset {
		b.lastOnset = frame
	}
	// Before lock, onsets act as (irregular) beats so the hash ring still
	// fills and anchors can fire on songs with sparse kicks.
	beatBoundary := onset
	if b.Locked {
		b.pos += 256
		if b.pos >= b.perQ8 {
			b.pos -= b.perQ8
		}
		bin := b.pos >> 8
		// x is a log2 ratio in Q8, at most 32<<8, so the average fits int16.
		c := int32(b.comb[bin])
		c += (x - c) >> BeatCombShift
		b.comb[bin] = int16(c)
		if c > int32(b.comb[b.best]) {
			b.best = bin
		}
		// The refractory keeps a move of the best bin from putting two beats in
		// one cycle.
		b.since++
		beatBoundary = bin == b.best && b.since*2 >= b.Period
	}
	if beatBoundary {
		b.since = 0
		b.beat++
		if b.Locked && uint32(b.beat)%BeatsPerBar == 0 {
			b.bar++
			barBoundary = true
		}
	}
	return beatBoundary, barBoundary
}
