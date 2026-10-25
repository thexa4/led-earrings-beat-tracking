package syncdsp

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
	// Modulating is the modulation-depth gate: something with a musical
	// envelope is in the input. False means the frame's spectrum is the room's
	// own noise, amplified by the AGC into the same range as quiet music, and no
	// decision derived from it means anything. Folded into Silent; reported
	// separately so a trace can tell "quiet" from "nothing here".
	Modulating bool
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
	prevBass  int32
	prevBass2 int32
	bassFloor int32
	bassOnset bool // debug: this frame's bass onset, for the replay trace
	level     int32
	linPeak   int32
	// logAvg/logMod: the mean and mean absolute deviation of the frame's log2
	// band-energy sum. Their difference is how many bits the loudness swings by
	// over ModPole frames, which - being a difference of logarithms - is the one
	// measure of the input that a gain cannot imitate at all. See ModMinBits.
	logSum     int32 // this frame's loudness, log2 Q8, before the silence gate
	logAvg     int32
	logMod     int32
	modulating bool // hysteresis state of the depth gate, see ModEnterQ8
	// Impulse filter (see feed): the four samples before the one being fed, oldest
	// first, and a count of the samples the filter changed, for the host tests.
	spPrimed bool
	spFill   int32 // priming samples taken, see feed
	spX1     int32
	spX2     int32
	spX3     int32
	spX4     int32
	spX5     int32
	spCount  int32
	// actAvg is the smoothed absolute input level in ADC codes, with the AGC's own
	// gain taken back out. See Activity.
	actAvg         int32
	actInit        bool
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

	// framesSincePattern counts frames since the last pattern change, for the
	// no-lock commit cadence. See PatternNoLockFrames.
	framesSincePattern int32

	// beatsSincePattern counts beats since the last pattern change, enforcing
	// PatternRefractoryBeats. Counted in decide(), so it advances at the same
	// rate the ring does - including the virtual beats used before lock - which
	// keeps the two devices' counters aligned without a shared clock.
	beatsSincePattern int32

	// Pattern feature: the spectral centroid, and where this music's quartiles lie.
	//
	// cent is the running average of the band-weighted spectrum (Q8 log2 band
	// energies times PatternWeights, scaled up by PatternCentroidScale for integer
	// headroom). The pattern is which of cut[0..2] it sits between, so it answers
	// "is this moment unusually bassy or unusually bright for this music" rather
	// than "where does this microphone put its energy", which is the question no two
	// earrings can answer the same way.
	//
	// centAvg and centDev - that value's own average and mean absolute deviation -
	// are kept to size the cuts, not to place them. centDev starts at zero, so
	// PatternWarmupFrames suppresses the choice until it is worth reading.
	//
	// centGapQ8 is the low-passed form of (cent - centAvg) in Q8, and is what the colour
	// is actually read from: the choice is made on the filtered deviation rather than on
	// cent, so that averaging does not change what the decision means. centDev is the
	// spread of the filtered deviation, not of the raw one, or the cuts would silently
	// mean fewer deviations after the low-pass than before it. See PatternGapShift, and
	// see the note where it is updated for why this one accumulator carries fraction bits
	// when its neighbours do not.
	//
	cent      int32
	centGapQ8 int32
	centAvg   int32
	centDev   int32
	centInit  bool

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
// Two samples of delay live here: the filter emits the middle of a five-sample
// window, so x[n] is handed on when x[n+2] arrives. The delay is constant, so it
// costs nothing but latency, and the frame boundary moves with it identically on
// every device.
//
// Impulses are dealt with here rather than further down because they are the
// loudest thing this front end produces when nothing is playing, and the only
// thing about it that is device-specific. Measured on the bench with the LEDs
// dark and the capture frozen (tools/freeze.py): one device's 250-sample window
// spans 12-20 codes, the other's 112-168, and the difference is not a noisier
// floor - their median deviations are 1.5 and 4.5 codes - it is isolated samples
// that step 40-90 codes and come straight back, on about 3% of samples and
// uncorrelated between the two earrings.
//
// What they do downstream is exactly the damage the pipeline is built to avoid.
// The DC blocker turns each one into a decaying step, which is a bass transient:
// the bottom band, which is the beat tracker's input, gets a hit the other
// earring does not have. The AGC's level measure is max-min over the frame, so
// an impulse is the frame's level. And the frame's band energies jump, which is
// what the modulation gate reads as music - measured on the bench noise alone, a
// 2-count floor with 3% of samples carrying 90-count impulses reads 2.7 bits of
// modulation depth against 1.0 bit for the same noise with the impulses absent.
// The impulses are not a detail of the quiet room; they are what the quiet room
// looks like to this pipeline. Removing them at the sample fixes all three at
// once; every fix further down has to be made three times and has to guess
// afterwards.
//
// What separates the two populations is not a ratio but a conjunction, and both
// halves of it are absolute: the neighbourhood must be doing nothing, *and* this
// sample must be far out. A kick, a cymbal, a kicked table or a loud bass note all
// have neighbours moving with them, so the first test fails and the filter stays
// out of the way - at every level, because "moving" here means more than
// SpikeCodesQuiet, which a signal carrying real information always is. A
// microphone impulse has neighbours sitting in the noise floor, so the first test
// passes and the second decides.
//
// Two things make that test work, and both were learned the hard way.
//
// The window has to be *centred* on the sample being judged. With a causal window
// (the four samples before this one) a smooth waveform's deviation from its own
// median is a first-order quantity - the slope - so the turn of a slow bass note,
// where the slope passes through a few tens of codes, fires the same test as an
// impulse does: measured, a causal window replaces 8% of the samples of a clean
// recording. Centred, the deviation of a smooth sample is its *curvature*, which
// is smaller by an order of magnitude at these frequencies, and the neighbours stop
// arguing: the same recording loses 0.1% of its samples.
//
// The scale has to be a low order statistic of the neighbours, not their mean or
// maximum, because of the pairs: two impulses on consecutive samples are common at
// a 3% rate and each vouches for the other if two of four neighbours can be
// outliers. The second-smallest tolerates exactly that many.
//
// Five samples rather than three, for the same reason one step down: one surviving
// impulse is not one frame of error but a decaying step in the DC blocker lasting a
// second, which is why the measured depth of a 2-count floor carrying 3% of
// 90-count impulses is 2.68 bits raw, 1.99 with a three-point median and 1.06 with
// a five-point, against 1.02 with the impulses absent.
//
// When the filter does not fire it is the identity, which is what keeps the bank's
// frequency response intact: a median applied to every sample turns a 6 kHz tone -
// two and a third samples per period at this rate - into silence, and moved the
// peak band of a 6 kHz sweep down out of band 0. That is a third of what the hash
// can see.
func (p *Pipeline) feed(v uint16) {
	x := int32(v)
	if !p.spPrimed {
		// Two samples of latency, so that the sample being judged has neighbours
		// on both sides. Taken once at Init, so every frame afterwards carries the
		// same delay on every device.
		p.spX1, p.spX2, p.spX3, p.spX4, p.spX5 = x, x, x, x, x
		if p.spFill < 2 {
			p.spFill++
			return
		}
		p.spPrimed = true
	}
	p.spX1, p.spX2, p.spX3, p.spX4, p.spX5 = p.spX2, p.spX3, p.spX4, p.spX5, x
	// c = the median of the five-sample window, by a 9-comparator sorting network
	// (checked exhaustively in TestMedianNetwork). An isolated sample cannot be the
	// middle of five whatever its size, so the median is the estimate of what this
	// sample should have been had nothing landed on it, and it costs no threshold.
	a, b, c, d, e := p.spX1, p.spX2, p.spX3, p.spX4, p.spX5
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
		a, c = c, a
	}
	if b > e {
		b, e = e, b
	}
	if b > d {
		b, d = d, b
	}
	if b > c {
		b, c = c, b
	}
	// The local slope: the four adjacent differences inside the window, sorted, so
	// that d2 - the second smallest - says how much this neighbourhood moves by
	// itself. Two properties matter and neither holds for the obvious alternative,
	// the neighbours' distance from the median. A waveform faster than about four
	// samples per period has neighbours sitting at the same phase as each other, so
	// their distance from the median is *zero* and a quiet-field test built on it
	// calls a cymbal a flat field and eats it; the adjacent difference of the same
	// waveform is its full swing and says otherwise. And two impulses in a row are
	// tolerated, because the difference between them is one of the four and the
	// second smallest ignores two outliers.
	d1, d2, d3, d4 := p.spX1-p.spX2, p.spX2-p.spX3, p.spX3-p.spX4, p.spX4-p.spX5
	if d1 < 0 {
		d1 = -d1
	}
	if d2 < 0 {
		d2 = -d2
	}
	if d3 < 0 {
		d3 = -d3
	}
	if d4 < 0 {
		d4 = -d4
	}
	if d1 > d2 {
		d1, d2 = d2, d1
	}
	if d3 > d4 {
		d3, d4 = d4, d3
	}
	if d1 > d3 {
		d1, d3 = d3, d1
	}
	if d2 > d4 {
		d2, d4 = d4, d2
	}
	if d2 > d3 {
		d2, d3 = d3, d2
	}
	dv := p.spX3 - c
	if dv < 0 {
		dv = -dv
	}
	if (d2 <= SpikeCodesQuiet && dv > SpikeCodesOutlier) ||
		dv > SpikeCodesOutlier+SpikeRelativeMul*d2 {
		// Two ways to be an impulse, and the second is the case that matters in
		// use.
		//
		// d2 <= SpikeCodesQuiet is the original: a neighbourhood still to a few
		// codes, so whatever is far out cannot be part of a waveform. That is
		// the dark-LED bench case - a 1.5-code floor carrying 40-90 code
		// impulses.
		//
		// While an earring animates, which is the only state a working pair is
		// ever in, that test never passes: the music's own adjacent differences
		// are a hundred codes or more, so d2 exceeds SpikeCodesQuiet and the
		// filter stands down exactly when its own LED coupling is loudest - and
		// the coupling follows the beat, since LED brightness does. It measures
		// up to +16381 codes, one sample wide, strictly positive
		// (tools/haltab.py, tools/wavedump.py), against a room tone of 200-270
		// counts of coherent amplitude, so it is the largest thing in the window
		// and the sums of squares that make the band energies are all but made
		// of it.
		//
		// dv > SpikeCodesOutlier+SpikeRelativeMul*d2 is the level-free form of
		// the same question: how far out is this sample compared with how much
		// its neighbourhood moves by itself? A point on a waveform sits about one
		// slope step from its own median, so its ratio is near 1 however loud
		// the music is; an impulse's is in the hundreds. The absolute term stays
		// so a quiet field's noise is never edited, and the ratio is what makes
		// the rule hold at signal levels instead of only in their absence.
		//
		// Either way the sample is replaced by the median, which is a
		// noise-floor sample like its neighbours.
		p.spCount++
	} else {
		c = p.spX3
	}
	p.filters.run(c - 1<<15)
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
		// linPeak is a peak-hold of the *linear* sum, which scales with the
		// gain directly. It is only ever compared against a linear sum of the
		// same signal, so scaling it keeps the silence decision unchanged. The
		// modulation trackers need no such correction: they live in the log2
		// domain, where a gain step is an additive constant that the mean
		// absorbs and the deviation never sees (which is the whole reason they
		// are not linear - the same trick the hash's shape bits use).
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

// BeatPeriod exposes the locked beat period (frames) for host tools; the
// device reads it through telemetry instead and the linker drops this.
func (p *Pipeline) BeatPeriod() int32 { return p.beat.Period }

// SpikeStats exposes the impulse rejector: the number of samples replaced since
// Init and the step size the limit is currently scaled from. Host tests use it
// to check the rejector fires on the measured bench impulses and stays quiet on
// music.
func (p *Pipeline) SpikeStats() (count int32) { return p.spCount }

// Activity returns the absolute level of the input in ADC codes, Q8: the mean
// rectified band magnitude per sample, with this frame's AGC gain divided back
// out, smoothed over the last few frames.
//
// This is the pipeline's only level measure that answers "how loud is the room"
// rather than "how loud does the AGC have to make it", and it is the one the
// silence gate has to read: every other quantity here is a difference between
// frames and is deliberately gain-free, which also makes it blind to a room that
// is simply empty. Three properties matter and the other candidates each lack
// one:
//
//   - Gain-free. Measuring the samples instead, or their spread, measures the
//     AGC's decision, and the AGC sits at maximum gain in a quiet room, so a
//     quiet room looks like a loud one.
//   - Impulse-proof. It is built from the samples after the median filter, and
//     it is a mean magnitude, so the 40-to-90 count spikes on the noisy device
//   - which dominate any peak-to-peak measure of its idle input, 112 counts
//     against a 5-count deviation - are already gone.
//   - Broadband. A slope- or movement-based statistic (the sample-to-sample
//     difference, say) is really a high-frequency level: measured on the test
//     signal it reads 0.9 codes at -24 dB against a 965-count peak, because a
//     bass-heavy signal barely moves from one sample to the next at 16 kHz. It
//     would gate on treble, not on sound.
func (p *Pipeline) Activity() int32 { return p.actAvg }

// Modulation exposes the loudness-modulation trackers, in Q8 log2 units: the
// running mean of the frame band-energy sum and its mean absolute deviation.
// logMod/256 is the loudness swing in bits (3 dB per bit). Host tests use it to
// check that a modulation-depth gate separates music from noise; the device does
// not read it.
func (p *Pipeline) Modulation() (avg, mad int32) { return p.logAvg, p.logMod }

// InputPeak exposes the AGC's tracked input peak-to-peak, in raw ADC counts.
// The debug sample dump (sync.go) uses it to scale its 8-bit ring, so the
// recording and the DSP agree on how loud the room is; the host tests read it to
// check the AGC is behaving without replaying the file twice.
func (p *Pipeline) InputPeak() int32 { return p.agcPeak }

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
		// Used for the silence gate and for the loudness, neither of which cares
		// about the difference between 2^27 and larger.
		if m > 1<<27 {
			m = 1 << 27
		}
		linear += int32(m)
	}
	// Absolute level, in ADC codes: this frame's rectified band magnitude per
	// sample with the AGC's gain divided back out. See Activity for why this and
	// nothing else is the measure of an empty room.
	abs := linear >> p.agcShift / Window
	if !p.actInit {
		p.actInit, p.actAvg = true, abs<<8
	} else {
		p.actAvg += (abs<<8 - p.actAvg) / (1 << ModPole)
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
	// Modulation depth: how far this frame's loudness sits from the running
	// mean, in the log2 domain, smoothed over ModPole frames. Both one-poles
	// round: truncating a negative deviation biases the mean downward, and the
	// whole point is the difference of two small numbers.
	// (The mic dump hit exactly this - a truncating DC tracker froze a few
	// hundred counts from the signal and made every capture a constant.)
	//
	// The loudness is log2 of the *sum* of the band energies, not the sum of
	// their log2 values. That distinction is the difference between a feature
	// that measures the room and one that measures the arithmetic: the sum of
	// logs is a geometric mean over the eight bands, so each band contributes
	// its own relative error no matter how quiet it is, and at the levels this
	// front end actually works at - a couple of counts before the AGC - a band
	// energy of eleven codes that dithers to twelve is a tenth of a bit. Eight
	// of those add up. Measured over the pipeline's own input, a *constant*
	// input produces 2.8 bits of swing in the sum of logs, which is more than
	// real music at the same level and enough on its own to open a gate tuned
	// to music. The log of the sum cannot do that: one band moving by one code
	// out of eleven changes the total by a sixtieth.
	logSum := maxE
	for i := range e {
		logSum += e[i]
	}
	// Remove this frame's gain, which is the whole point of doing this in the log
	// domain. The AGC's shift is applied to the samples before the bands are
	// measured, so it adds exactly one bit to every band's log2 and NBands bits to
	// this sum - and the AGC moves on its own, on each device's own noise, several
	// times a minute. Without this the depth tracker measures the gain controller:
	// measured on a room with no source in it at all, 2.67 bits, which is more than
	// a real recording's 2.5, and the gate therefore open. With it, the same room
	// reads its actual modulation, which is around one bit, and the gate has
	// something to decide on.
	logSum -= p.agcShift * 256 * NBands
	p.logSum = logSum
	ldev := logSum - p.logAvg
	p.logAvg += (ldev + 1<<(ModPole-1)) >> ModPole
	if ldev < 0 {
		ldev = -ldev
	}
	// No bound on ldev. There was one, to stop the microphone's impulses pulling
	// the average up: it bought 0.5 bits on the impulse case and cost the same
	// 0.5 bits on music, which left the two populations exactly as far apart as
	// before (measured across seven caps: the room and the recording never came
	// within half a bit of each other at any bound). The median in feed removes the
	// impulses instead, which moves the room down without touching the music - see
	// the table on ModEnterQ8 for the numbers that followed.
	p.logMod += (ldev - p.logMod + 1<<(ModPole-1)) >> ModPole
	// Two thresholds, because the alternative was measured: a single one makes
	// the gate flicker on and off across a passage, and each edge is a frame on
	// which one earring decides and the other does not - which is the failure
	// this gate exists to remove, reintroduced as a side effect. Enter high,
	// hold low. See ModEnterQ8 for the data behind both.
	switch {
	case !p.modulating && p.logMod >= ModEnterQ8:
		p.modulating = true
	case p.modulating && p.logMod < ModHoldQ8:
		p.modulating = false
	}
	// The depth gate deliberately stops here: it is reported (Events.Modulating)
	// and it is not folded into p.silent. It was, and that is worth writing down,
	// because it looked like the obvious place for it and it is wrong.
	//
	// p.silent is not a status, it is an input: it gates the total, the onsets, the
	// anchors and the hash itself. Anything ANDed into it changes what the
	// earring decides, on the frames where it is on - and the depth gate's idea of
	// "the frames where it is on" is not the same in two devices, even the same
	// recording at two levels, because the tracker runs over each device's own
	// noise. Measured over the level matrix, with the gate inside p.silent, pattern
	// agreement between full scale and 36 dB down fell to 55-62%; with the gate out
	// it is back above the 70% the test demands. The gate is level-blind by
	// construction and still disagreed by five percent of frames, which is five
	// percent of frames on which one earring recomputed its pattern and the other
	// held its old one.
	//
	// The failure the depth gate was written for - two earrings hashing their own
	// independent noise in a quiet room - is not fixable there either. In a quiet
	// room the two depth measurements are as uncorrelated as the spectra they come
	// from, so the gate does not synchronise them, it only adds a second way to
	// disagree. What it is good for is the opposite direction, and it is kept for
	// that: a tap, a hand, a coat rustling in the dark modulates, and a room that
	// is merely silent does not, which is a fact worth having on the LED even
	// though it is not one the hash can act on.
	//
	// Silence is therefore two criteria, and the absolute one carries the case the
	// depth gate was supposed to. Measured (see SilenceEnterQ8 for the table), a
	// room with nothing in it reads 7 codes on the clean device and 10 on the
	// noisy one, where the test signal reads 28 at 36 dB down and 17 at 42 dB: a
	// real margin, on a quantity the AGC cannot move, on both devices at once. The
	// depth feature cannot offer that - the same sweep puts a silent room at 2.4
	// bits of swing and the test signal 60 dB down at 2.4 bits too, so at the quiet
	// end the room modulates as much as the music does and no threshold on that
	// axis separates them.
	switch {
	case p.silent && p.actAvg > SilenceExitQ8:
		p.silent = false
	case !p.silent && p.actAvg < SilenceEnterQ8:
		p.silent = true
	}
	if linear < p.linPeak/SilenceDiv {
		// Far below the recent peak: whatever was playing has stopped, even if
		// the AGC has not caught up with it yet.
		p.silent = true
	}

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

	// 5b'. The pattern feature: the spectral centroid, and its own slow reference.
	//
	// One weighted sum and three running statistics, and the device drops out of it
	// without being modelled. A gain - the AGC's, or one microphone simply being
	// quieter than the other - adds the same amount to every band, and because
	// PatternWeights sums to zero, that amount multiplies away here. A steady spectral
	// tilt, which is what a microphone's response and the room add over the long run,
	// enters the centroid as a constant, and subtracting the device's own centAvg
	// removes constants by construction. What is left is divided by the device's own
	// spread, so a device whose centroid swings twice as far is not read as twice as
	// opinionated.
	//
	// The sum runs over all eight bands rather than over a few candidates, taken from
	// the smoothed spectrum: the two earrings' smoothed shapes correlate +0.7 to +0.9,
	// so summing averages their independent noise down instead of amplifying it at
	// whichever band happens to be a hair ahead on one device. See PatternWeights.
	//
	// Computed from the floored smooth, so in silence every band sits at the pin, the
	// weighted sum is flat, the centroid is constant and centDev decays: a quiet room
	// produces no pattern changes, which is what it should do.
	//
	// The weights are deliberately constants. Scaling each one by how far its band sits
	// above RelFloorQ8 was tried, because on paper it is strictly better: a band pinned
	// by the floor carries no music, only whatever the AGC is amplifying, so weighting it
	// down should hand less of the decision to noise. Measured in isolation it does -
	// against a clean device, at the bench's quiet operating point, the plain ramp agrees
	// 80.4% of the time and the tapered one 99.0%. It is not shippable as written, and
	// the reason is the sentence above: the taper makes the weights depend on the frame's
	// loudest band, so they change every frame, and a set of weights that moves breaks
	// the zero-sum property that the whole design rests on. A flat spectrum no longer
	// sums to zero, and the spread no longer collapses on silence - the guard at
	// PatternMinDev stopped catching a still device, which is the one case that must
	// never invent a pattern. Tapering from a slow spectrum instead would restore it, at
	// the cost of eight more running states, and has not been tried.
	c := int32(0)
	for i, v := range p.smooth {
		c += v * PatternWeights[i]
	}
	c *= PatternCentroidScale
	if !p.centInit {
		// First frame: seed all of them so the centroid starts at rest rather than
		// reading as a huge deviation from zero.
		p.centInit = true
		p.cent, p.centAvg, p.centDev, p.centGapQ8 = c, c, 0, 0
	} else {
		p.cent += (c - p.cent + (1 << (PatternCentroidShift - 1))) >> PatternCentroidShift
		gap := p.cent - p.centAvg
		// The reference keeps following the raw deviation: low-passing it here instead
		// would make the reference chase the same variation it exists to measure out, and
		// the colour would stop meaning "unusual for this music". Only the quantity being
		// judged, and the spread that sizes it, see the filtered version.
		//
		// Kept in Q8 rather than as an integer of the same units as cent. An integer EMA
		// at shift 10 adds nothing at all while the difference to be caught is under 512,
		// and a smoothed deviation is routinely within 512 of itself, so the plain form
		// stops wherever it happened to stop moving and stays there. centDev then decays
		// towards that frozen value, and one device drops under PatternMinDev and reports
		// the middle pattern while its partner, frozen somewhere else, does not: measured
		// on the quiet-offset bench case, that took the case from 92% agreement to 0%,
		// deterministically, for the whole capture. Carrying eight fraction bits puts the
		// deadband at two units of the centroid instead of five hundred and twelve.
		p.centGapQ8 += ((gap << 8) - p.centGapQ8 + (1 << (PatternGapShift - 1))) >> PatternGapShift
		// The ruler stays the spread of the RAW deviation. Measuring the spread of the
		// smoothed one instead is self-consistent on paper and fails in a specific way: a
		// centroid that drifts steadily against a lagging reference has almost no
		// oscillation for the low-pass to remove, so its smoothed deviation keeps the full
		// drift while its smoothed spread loses everything the low-pass did remove, and the
		// ratio runs to the clamp. Both earrings then pin to whichever end colour their own
		// drift points at, which is the stuck-on-one-pattern failure this is meant to fix.
		// Observed on TestAgreement/bench-quiet-offset, where a slow drift put the pair on
		// opposite end buckets for the whole capture: 92% agreement became 0%.
		abs := gap
		if abs < 0 {
			abs = -abs
		}
		p.centDev += (abs - p.centDev + (1 << (PatternScaleShift - 1))) >> PatternScaleShift
		p.centAvg += (gap + (1 << (PatternScaleShift - 1))) >> PatternScaleShift
	}

	// The pattern's backstop, deliberately placed here rather than next to the musical
	// triggers in decide(): this runs every frame without condition, and the two things
	// it needs are the centroid and its spread, which are already computed above. The
	// triggers in decide() all depend on something going well - the beat tracker having
	// lock, the hash having settled - and when they do not, a device otherwise has no
	// path that ever calls setPattern, so it holds its warmup colour indefinitely while
	// its partner keeps changing. That asymmetry, not the choice itself, is what makes
	// two earrings sit on different colours: the underlying choice agrees while the
	// committed one does not.
	//
	// The window is long enough that the musical triggers still decide when a change
	// lands, and the choice is safe to re-read on an arbitrary frame because it is a
	// function of the music alone - two devices sampling it at unrelated moments get the
	// same answer, so this can cost a little latency but never agreement.
	if p.frame >= PatternWarmupFrames && p.framesSincePattern >= PatternMaxHoldFrames {
		p.setPattern(p.pattern())
	}
	p.framesSincePattern++

	// 5c. The hash is recomputed every frame from a snapshot of the smoothed
	// spectrum, but its *temporal* field reads p.prevSmooth, which only
	// advances at ring pushes (below). Between beats that makes the per-frame
	// hash the current smoothed spectrum against the last beat's: a live "what
	// is rising underfoot" view, and the value the golden table and the device
	// selftest compare against. At the push itself this is exactly the hash
	// the ring stores.
	p.smoothSnap = p.smooth
	h := hashFrame(p.smoothSnap, p.prevSmooth, &dev)
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

	// 6. Beat tracker: advance phase, fold in the onset, possibly emit a beat
	// boundary. Driven by the kick band, not the full-spectrum onset: see
	// onsetDetectBass. Everything else in this function - the anchor, the
	// energy gate, the pattern triggers - keeps reading the full-spectrum
	// onset, because a section change is a broadband event.
	beatBoundary, barBoundary := p.beat.Tick(p.bassOnset, p.frame)

	// The events struct is state across frames, so it is rebuilt from scratch
	// every frame: a stale Hash here once made every frame look identical.
	p.events = Events{
		Hash:       h,
		HashAge:    1,
		Onset:      onset,
		Beat:       beatBoundary,
		Bar:        barBoundary,
		Locked:     p.beat.Locked,
		Silent:     p.silent,
		Modulating: p.modulating,
		Pattern:    p.lastPattern,
	}

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
		// to realign on. setPattern resets the refractory itself when the
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
		p.setPattern(p.pattern())
	}

	// Debug branch: append this frame to the SWD-polled telemetry ring.
	p.recordTelemetry()

	// 9. Tempo: every TempoStep frames, correlate the recent envelope against
	// itself. This is what decides the period; the interval histogram above
	// only provides the early beats before the ring is full.
	p.pushEnv(total)
	if p.frame%TempoStep == TempoStep-1 {
		p.tempoScan()
	}

	p.frame++
	return p.events
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
	step := energy / TempoScale
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
		sm := (prev + 2*cur + next) / 4
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
	if !b.Locked {
		// Start the fresh grid at a beat; the phase-locked loop in Tick pulls it
		// onto the kicks from here within a few beats. The earlier version set
		// the phase by picking the loudest of the Period comb positions in the
		// envelope, but measured against the device's own loudness that landed
		// the beat on the loud frame no better than a fixed phase did (both
		// ~chance), and cost a nested loop over the ring - the PLL tracking the
		// onsets does the job for free.
		b.phase = 0
	}
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
		for _, lagBars := range [3]int32{LagBars1, LagBars2, LagBars3} {
			nominal := lagBars * BeatsPerBar
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
	// decide *when* it is re-read. Re-reading is a no-op when the dominant band
	// has not clearly changed, so nothing is gained by reading it rarely - and
	// something is lost, because a device that fell out of step is only corrected
	// at the next re-read.
	//
	// The bar boundary is one of the triggers, which is what keeps changes on a
	// musical beat rather than mid-figure. Two devices' bar boundaries land a few
	// frames apart at most, and both read essentially the same spectrum there, so
	// this trigger cannot by itself cause a divergence the way an event that only
	// one device sees would.
	pat := p.pattern()

	// These triggers are all musical, and every one of them can be absent: no beat
	// lock means no barBeat, and decide() is only reached on a settled hash, so a device
	// whose hash will not settle never gets here at all. When that happens the pattern
	// freezes on whatever the warmup handed it, which is the "stuck on one pattern"
	// complaint, and it is a commitment bug rather than a selection bug - at the bench's
	// quiet operating point a clean device against one 42 dB down agreed on its committed
	// pattern 28% of the time, and 81% once the backstop was in place, with the selection
	// code unchanged in between. That backstop lives in endFrame, next to the centroid it
	// reads, and is documented at PatternMaxHoldFrames.
	if (returned || barBeat) && p.beatsSincePattern >= PatternRefractoryBeats {
		p.setPattern(pat)
	}
	p.beatsSincePattern++
}

// setPattern switches the animation pattern and reports it in the events. The
// refractory is counted in beats whether or not the pattern actually changed, so
// a device that keeps re-reading the same pattern still waits the full window
// before the next look.
func (p *Pipeline) setPattern(pat int32) {
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

// PatternIndex maps the spectral centroid onto one of NumPatterns buckets, and
// the index increases with bassiness: 0 is a moment that is unusually bright for
// this track, NumPatterns-1 unusually bassy. Readable, and self-healing.
//
// The choice of driver is deliberate, and measured. Two properties matter more
// than musical sophistication:
//
//   - Self-healing. Because the pattern is a pure function of the audio and not
//     a counter of past events, a device that misses one event is back in step
//     at the next one instead of being one pattern behind for the rest of the
//     track.
//   - Insensitivity. Mappings tried before - which lag matched, a hash bit,
//     popcount modulo NumPatterns - each turned a single disagreeing bit into a
//     different pattern, or wrapped a neighbouring value into a distant one.
//
// What it compares is a weighted sum of every band's deviation from that band's own
// average, against that sum's own slow average and spread. This replaces the argmax
// over a few candidate bands, which is documented on PatternWeights together with
// the two-capture measurement that retired it.
//
// The difference that matters is what each does with a close call. argmax throws the
// margin away: a band one count ahead and a band three doublings ahead give the same
// answer, so two earrings whose measurements straddle a hair disagree completely,
// and no choice of which bands compete can fix a decision that discards the only
// information distinguishing them. A bucketed sum keeps every band's magnitude, and
// because it is monotone across the band set, two earrings that disagree at all
// disagree by one bucket - a slightly different shade - rather than by the whole
// spectrum. Deviation from a fitted line was also tried and is not used: a band that
// runs permanently hot on one earring drags the fit and bends its neighbours,
// whereas correcting each band against its own history leaves them independent.
//
// centDev is what makes the colour mean *unusually* rather than *bassy*. A device's
// steady spectral tilt enters the centroid as a constant, which subtracting centAvg
// removes; its sensitivity and AGC operating point enter as a scale factor, which
// dividing by the device's own spread removes. Neither has to be measured, stored or
// calibrated, and no radio is involved - see the file comment in sync.go.
//
// It has no memory, and that is a requirement rather than a simplification. An
// earlier version wrapped this in hysteresis - the pattern on screen keeps itself
// unless a challenger beats it by PatternMargin - which sounds exactly right and
// measured exactly wrong: two earrings running the same spectrum through it spent
// 57% of the time visibly matching and 36% of it visibly stuck on *different*
// colours, where the same spectra scored 81% with the memory removed. Re-tested on
// this feature as a margin on the bucket edges, and it did not help here either.
//
// The reason is that hysteresis is per-device state with no channel between the
// devices. Once the two are on different patterns, the incumbent only yields to a
// challenger that beats it by the margin - and the margin (0.19 doublings) is
// larger than the lead the true winner usually holds (0.04-0.10), so neither
// incumbent ever yields and the disagreement is permanent. A margin that is
// supposed to prevent flicker instead prevents agreement, which is the one thing
// the pattern has to do. Tie-breaks are not a problem without it: the bucket
// boundaries are fixed comparisons against the same three numbers, so an exact tie
// resolves to the same pattern on every device.
//
// What keeps the animation readable instead is the commit gate - a pattern is
// re-read on a bar boundary or a section return, and PatternRefractoryBeats
// enforces a minimum spacing - which limits the change *rate* without making the
// *choice* depend on what this device happened to be showing.
//
// centDev is what makes the colour mean *unusually* rather than *bassy*, and it is
// also the ruler, so a ruler that has stopped moving has to be noticed: on silence
// or a steady tone the centroid is constant, centDev decays toward zero through
// integer truncation, and every boundary collapses onto the reference - which would
// make the pattern flicker on rounding noise alone. A spread under PatternMinDev
// therefore reports the middle pattern, which is the right behaviour for a device
// sitting still in a quiet room and is also what keeps the division below from being
// a division by zero; on this target (-panic=trap) that would reset the device.
func PatternIndex(cent, centAvg, centDev int32) int32 {
	return PatternIndexGap(cent-centAvg, centDev)
}

// PatternIndexGap is PatternIndex with the deviation handed to it rather than formed
// from two arguments, because the deviation the colour is chosen on is not cent minus
// centAvg: it is centGap, that difference after PatternGapShift. Splitting it this way
// keeps the arithmetic below in one place and keeps the two-argument form above honest -
// the invariance tests are written against it, and they still exercise the same buckets.
//
//go:noinline
func PatternIndexGap(gap, centDev int32) int32 {
	if centDev < PatternMinDev {
		return NumPatterns / 2
	}
	d := gap
	// Clamped before the scaling rather than after it, which serves two ends. A
	// deviation far enough out that it times 256 would wrap an int32 cannot produce a
	// nonsense ratio: the bound is a multiple of the divisor, so the division is safe by
	// construction. And nothing is lost, since the mapping cannot express anything past
	// its end bucket anyway.
	if lim := centDev * 2; d > lim {
		d = lim
	} else if d < -lim {
		d = -lim
	}
	z := d * 256 / centDev // Q8, in units of the spread, bounded by +-2

	pat := int32(0)
	for i := int32(0); i < NumPatterns-1; i++ {
		// Cut i sits (i - (NumPatterns-2)/2) spreads from the reference; for the
		// shipped four that is -0.844, 0 and +0.844. See PatternCutQ8, which also
		// records why the cuts are *not* tracked empirically despite the buckets
		// coming out uneven.
		cut := (2*i - (NumPatterns - 2)) * PatternCutQ8 / 2
		if z > cut {
			pat = i + 1
		}
	}
	return pat
}

// pattern reads the pattern from this frame's centroid, suppressing it until the
// centroid's reference and spread are worth reading.
//
// The gate is not cosmetic. With a ~16 s time constant neither centAvg nor centDev
// means anything after one second, and while centDev is still near zero every
// boundary sits on top of the reference, so the choice is decided by which side of it
// the first few frames happened to land - and two earrings boot a few seconds apart
// would start on different colours and stay there until the spread grew enough to
// separate the buckets. WarmupFrames alone is not enough for that, because it is
// tuned for the AGC, which converges in about a second.
//
// Reporting the middle pattern while warming up is what makes this safe rather than
// merely delayed: it is the same answer a still device gets, so a device that is
// early is not wrong, just conservative, and every device reaches the same starting
// colour before any of them starts choosing.
func (p *Pipeline) pattern() int32 {
	if p.frame < PatternWarmupFrames {
		return NumPatterns / 2
	}
	// The smoothed deviation over the raw spread: the numerator is what gets averaged,
	// and the ruler is left alone so that a device whose centroid is drifting is measured
	// against the same yardstick as one whose centroid is not. See where centDev is
	// updated.
	return PatternIndexGap(p.centGapQ8>>8, p.centDev)
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
func hashFrame(cur, old [NBands]int32, dev *[NBands]int32) uint32 {
	var w uint32
	for b := 0; b < NBands; b++ {
		if cur[b]-old[b] > TemporalDeadband {
			w |= 1 << uint(b)
		}
	}
	return w | shapeCode(dev) | hotBits(&cur)
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

// BeatTracker keeps a beat phase locked to the music using onset intervals,
// so that the hash ring can be indexed by musical position (§D3).
type BeatTracker struct {
	Locked bool
	Period int32 // beat period in analysis frames, valid when Locked
	phase  int32 // 0..Period-1
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
}

// Tick advances the phase by one frame and folds in the onset. It returns
// (beatBoundary, barBoundary) for this frame.
//
//go:noinline
func (b *BeatTracker) Tick(onset bool, frame int32) (bool, bool) {
	beatBoundary := false
	barBoundary := false

	if onset {
		b.lastOnset = frame
	}

	if b.Locked {
		b.phase++
		if b.phase >= b.Period {
			b.phase = 0
			b.beat++
			beatBoundary = true
			if b.beat%BeatsPerBar == 0 {
				b.bar++
				barBoundary = true
			}
		}
		// Phase-locked loop: pull the phase toward the onset so drift between the
		// local clock and the music is corrected continuously. The period comes
		// from the envelope scan and can be a frame or two off the true tempo
		// (143 BPM is 26.9 frames), which over a minute is a whole beat of
		// drift; this is what keeps a locked grid on the kicks instead.
		if onset {
			d := b.phase
			if d > b.Period/2 {
				d -= b.Period
			}
			// d in (-Period/2, Period/2]: how far we are past (or before) the
			// nearest beat. Correct a quarter of it, with a one-frame minimum |step|
			// so small errors do not get stuck.
			step := d >> 2
			if step == 0 {
				if d > 0 {
					step = 1
				} else {
					step = -1
				}
			}
			b.phase -= step
			if b.phase < 0 {
				// Stepped back through the boundary: emit it here rather than losing
				// the beat.
				b.phase += b.Period
				b.beat++
				beatBoundary = true
			} else if b.phase >= b.Period {
				b.phase -= b.Period
				b.beat--
			}
		}
	} else if onset {
		// Before lock, onsets act as (irregular) beats so the hash ring still
		// fills and anchors can fire on songs with sparse kicks.
		b.beat++
		beatBoundary = true
	}
	return beatBoundary, barBoundary
}
