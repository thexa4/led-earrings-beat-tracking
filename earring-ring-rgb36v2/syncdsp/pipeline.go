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
	Hash           uint32
	HashAge        int32 // frames since Hash was last recomputed (0 = this frame)
	Onset          bool  // spectral flux onset this frame
	Beat           bool  // beat boundary this frame
	Bar            bool  // bar boundary this frame
	Anchor         bool  // transition anchor: reset to base pattern
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

	// Frame-level loudness tracking, used for the onset detector, the anchor
	// energy gate and silence detection:
	//   total     sum of per-band log2 energies (Q8)
	//   level     slow average of total
	//   linPeak   peak-hold of the linear band-energy sum (SilenceDiv leak)
	total      int32
	prevTotal  int32
	prevTotal2 int32 // onset flux runs over two frames, see onsetDetect
	fluxFloor  int32
	level      int32
	linPeak    int32
	onsetAge   int32 // frames since the last onset (100 = long ago)
	silent     bool  // far quieter than the recent peak: leave things alone

	// Beat tracker.
	beat BeatTracker

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

	// patternBand is the band currently on screen, which usePattern defends
	// against close challenges (hysteresis). Invalid until the first decision:
	// a zero value would otherwise look like a legitimate band 0.
	patternBand      int32
	patternBandValid bool

	frame  int32
	events Events
}

// Init resets the pipeline, including the filter state.
func (p *Pipeline) Init() {
	*p = Pipeline{}
	p.filters.reset()
}

// Process runs one analysis frame of Window samples. It needs no scratch
// buffer: the filter state lives in the pipeline, so the caller can hand it
// the ADC capture buffer directly.
func (p *Pipeline) Process(raw []uint16) Events {
	for _, v := range raw[:Window] {
		p.filters.run((int32(v) - 1<<15) >> inputShift)
	}
	return p.endFrame()
}

// ProcessSample feeds a single sample, for a caller that is interrupted by the
// ADC at sample rate and wants to spread the filter work between captures.
// Call EndFrame every Window samples.
func (p *Pipeline) ProcessSample(v uint16) {
	p.filters.run((int32(v) - 1<<15) >> inputShift)
}

// EndFrame finishes a frame of Window samples fed via ProcessSample.
func (p *Pipeline) EndFrame() Events { return p.endFrame() }

// BeatPeriod exposes the locked beat period (frames) for host tools
// (tools/hostbeat); it is unreferenced on the device and the linker drops it.
func (p *Pipeline) BeatPeriod() int32 { return p.beat.Period }

//go:noinline
func (p *Pipeline) endFrame() Events {
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
	p.silent = linear < p.linPeak/SilenceDiv

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

	// 6. Beat tracker: advance phase, fold in the onset, possibly emit a
	// beat boundary.
	beatBoundary, barBoundary := p.beat.Tick(onset, p.frame)

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

	// 7. Beat-indexed ring pushes, gated on the hash having settled (see 5d):
	// the ring only ever holds settled section descriptors, so both novelty
	// and the periodicity match compare settled values against each other.
	// Before the tracker locks, treat onsets as beats so the ring still fills
	// (music with weak transients just fills slower).
	push := (beatBoundary || (!p.beat.Locked && onset)) && !p.silent &&
		p.stableFrames >= AnchorSettleFrames
	if push && p.frame >= WarmupFrames {
		p.decide(h, barBoundary, &dev)
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
		p.beatsSincePattern = 0
		// Snap the pattern to the new section immediately, ignoring the
		// pattern refractory: this is the moment all the earrings are meant
		// to realign on. Deliberately *not* resetting the beat counter: the
		// ring is indexed by it, so zeroing it would shift this device's
		// ring relative to the others' and "4 bars ago" would mean
		// different music on each earring. The tracker's PLL already keeps
		// the count on the music.
		p.setPattern(p.usePattern(&dev))
	}

	p.frame++
	return p.events
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
	if flux > p.fluxFloor+(p.fluxFloor>>1)+48 &&
		p.frame-p.beat.lastOnset >= BeatRefractoryFrames {
		onset = true
	}
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
func (p *Pipeline) decide(h uint32, barBeat bool, dev *[NBands]int32) {
	b := &p.beat

	// The temporal reference advances now, at a musical position: the next
	// hash (per-frame or at the next push) diffs against this spectrum.
	p.prevSmooth = p.smoothSnap

	// Periodicity confirmation - did the music come back around?
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
				if prev != 0 && hashDist(h, prev) <= DAccept {
					returned = true
					break
				}
			}
			if returned {
				break
			}
		}
	}

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
	pat := p.usePattern(dev)

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
	p.events.PatternChanged = true
	p.events.Pattern = pat
}

// noveltyMin returns the minimum Hamming distance between h and the ring,
// excluding the ring's leading run of matches: entries pushed since h's own
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
		d := hashDist(h, other)
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

// PatternIndex picks the band that stands furthest above the fitted spectral
// line and maps it to an animation pattern: the pattern says "this moment is
// bass" or "this moment is hats", which is readable and self-healing.
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
// The bottom band is excluded, matching shapeBits: it is a catch-all lowpass
// that wins nearly always and would freeze the animation. That leaves bands
// 0..NBands-2, which map one pattern per band - hence NumPatterns == NBands-1,
// asserted in TestPatternIndexStable. Pattern 0 is "air" (above 4 kHz, hats and
// cymbals) and pattern NumPatterns-1 is the low mids.
//
// Plain argmax is still too sharp on its own: two adjacent bands are often
// within a fraction of a dB of each other, so devices whose deviations differ by
// a hair picked different bands from an identical hash. usePattern adds the
// hysteresis that fixes this; this function is the stateless part.
//
//go:noinline
func PatternIndex(dev *[NBands]int32) int32 {
	best := int32(0)
	for b := int32(1); b < NBands-1; b++ {
		if dev[b] > dev[best] {
			best = b
		}
	}
	return best
}

// usePattern is PatternIndex with hysteresis: a band takes over only when it
// clearly beats the one on screen, otherwise the animation keeps showing the
// band that was already closest.
//
// Without this, adjacent bands that are a fraction of a dB apart flip the
// pattern between devices that otherwise agree on everything - measured as two
// earrings with byte-identical hashes sitting on different patterns. The margin
// is in the same Q8<<8 deviation units as spectralDevs.
//
//go:noinline
func (p *Pipeline) usePattern(dev *[NBands]int32) int32 {
	challenger := PatternIndex(dev)
	if p.patternBandValid && challenger != p.patternBand &&
		dev[challenger] < dev[p.patternBand]+PatternMargin {
		// Not a clear win: keep the incumbent, but still report it as the
		// current pattern so devices converge.
		return p.patternBand
	}
	p.patternBand = challenger
	p.patternBandValid = true
	return p.patternBand
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
	Lock   int32 // votes toward lock
	// Votes per interval bin. int8 because the range is provably 0..60:
	// addVote clamps at 60 and decay only decrements positive entries.
	ivis       [BeatMaxPeriod - BeatMinPeriod + 1]int8
	lastOnset  int32
	onsetCount int32
}

// Tick advances the phase by one frame and folds in the onset. It returns
// (beatBoundary, barBoundary) for this frame.
//
//go:noinline
func (b *BeatTracker) Tick(onset bool, frame int32) (bool, bool) {
	beatBoundary := false
	barBoundary := false

	if onset {
		interval := frame - b.lastOnset
		b.lastOnset = frame
		b.onsetCount++
		if interval >= BeatMinPeriod && interval <= BeatMaxPeriod {
			b.vote(interval)
		}
		if !b.Locked && b.onsetCount >= 8 {
			b.tryLock()
		}
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
		// Phase-locked loop: pull the phase toward the onset so drift
		// between the local clock and the music is corrected continuously.
		if onset {
			d := b.phase
			if d > b.Period/2 {
				d -= b.Period
			}
			// d in (-Period/2, Period/2]: how far we are past (or before)
			// the nearest beat. Correct a quarter of it, with a one-frame
			// minimum |step| so small errors do not get stuck.
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
				// Stepped back through the boundary: emit it here rather
				// than losing the beat.
				b.phase += b.Period
				b.beat++
				beatBoundary = true
			} else if b.phase >= b.Period {
				b.phase -= b.Period
				b.beat--
			}
		}
	} else if onset {
		// Before lock, onsets act as (irregular) beats so the hash ring
		// still fills and anchors can fire on songs with sparse kicks.
		b.beat++
		beatBoundary = true
	}
	return beatBoundary, barBoundary
}

// vote folds an onset interval into the interval histogram with octave
// reinforcement (Dixon-style) and a two-frame tolerance.
//
// The doubling fold carries weight 2, not 1: with hats exactly between
// kicks, most measured intervals are half-beat intervals, and the ×2 fold is
// the main source of beat-period evidence. With weight 1 it merely cancels
// the per-vote decay and the tracker never locks (found in testing).
//
// The neighbour folds exist because real onsets are not frame-exact: over a
// 12 s EDM segment at 16 kSPS the interval between consecutive kicks spreads
// over 23..28 frames for a single tempo, and exact-bin voting dilutes that
// evidence below LockCount - the tracker never locks and beats stay raw
// onsets. Two frames either way (half weight per step) bridge the spread
// while keeping the peak centred.
//
// Decay runs on every other vote for the same reason: one decrement per
// onset bounds the achievable bin height by roughly the number of onsets,
// and at 64 fps the onset rate (not the frame rate) sets the vote budget -
// 12 s of 137 BPM music offers only ~27 intervals to split across bins.
// Half-rate decay doubles the evidence horizon without touching the weights.
func (b *BeatTracker) vote(interval int32) {
	if b.onsetCount&1 == 0 {
		b.decay()
	}
	b.addVote(interval, 4)
	b.addVote(interval-1, 2)
	b.addVote(interval+1, 2)
	b.addVote(interval-2, 1)
	b.addVote(interval+2, 1)
	b.addVote(interval/2, 3)
	b.addVote(interval*2, 2)
}

func (b *BeatTracker) addVote(interval int32, amount int16) {
	if interval >= BeatMinPeriod && interval <= BeatMaxPeriod {
		i := int32(b.ivis[interval-BeatMinPeriod]) + int32(amount)
		if i > 60 {
			i = 60
		}
		b.ivis[interval-BeatMinPeriod] = int8(i)
	}
}

//go:noinline
func (b *BeatTracker) decay() {
	for i := range b.ivis {
		if b.ivis[i] > 0 {
			b.ivis[i]--
		}
	}
}

// (decay/vote bounds are what keep ivis inside 0..60, which is what makes the
// int8 representation above safe.)

// tryLock picks the best tempo candidate. Only exact interval evidence
// counts (octave reinforcement alone can lock onto a half-tempo, which is
// still musically acceptable, so reinforced bins are included; anchors reset
// everything anyway if it is wrong).
//
//go:noinline
func (b *BeatTracker) tryLock() {
	best := -1
	bestCount := int8(LockCount)
	for i, c := range b.ivis {
		if c >= bestCount {
			bestCount = c
			best = i
		}
	}
	if best >= 0 {
		b.Period = int32(best) + BeatMinPeriod
		b.Locked = true
		b.phase = 0
	}
}
