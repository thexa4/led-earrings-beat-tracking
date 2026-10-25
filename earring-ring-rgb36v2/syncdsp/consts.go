// Package syncdsp derives a compact perceptual "hash" of music from its
// spectrum, so that multiple devices listening to the same music make the same
// pattern decisions without any radio link.
//
// This package is deliberately pure integer Go with no floats, no divides and
// no per-sample 64-bit arithmetic, so it compiles for Cortex-M0+ and runs
// bit-identical on the host for testing. The only 64-bit multiplies happen
// once per frame (log2 and the spectral line fit), where the emulated cost is
// irrelevant. See PARTY-SYNC.md for the design.
package syncdsp

// Capture/frame geometry. See PARTY-SYNC.md §D2.
const (
	// Window is the number of samples per analysis frame (~15.6 ms).
	Window = 250
	// SampleRate is nominal; the real device clock is 15996.58 Hz
	// (4.194304 MHz / 262). Only used in comments/tests: the DSP itself is
	// rate-agnostic, because everything downstream is indexed by frames and
	// beats, never by seconds.
	SampleRate = 15997
)

// Floor for weak bands.
//
// RelFloorQ8 pins bands that sit far below the loudest band to a common value,
// applied to the *smoothed* spectrum (see endFrame step 5b). The floor is
// relative to the loudest band on purpose: a threshold in dB below that band
// is invariant under the mic gain, distance and hand-shadow differences
// between devices, so all devices pin the same bands and their hash bits
// agree. An absolute threshold would be device-specific, which is the mistake
// this line replaces.
//
// Applying it after smoothing rather than per raw frame matters just as much:
// the raw maximum flickers with the beat, so a per-frame floor would move
// under the hash, and silence (all energies zero) would pin to 0 while quiet
// music pinned elsewhere - device-specific again. Floored once on the settled
// spectrum, silence and quiet music get the same word on every device.
const RelFloorQ8 = 10 << 8 // 30 dB below the loudest band

// Input normalization (see Pipeline.feed).
//
// The hash and the thresholds are all relative, so in principle the absolute
// input level does not matter. Measured, it does: replaying the same 60 s of
// music through this pipeline at successively lower level (pipeline-sees RMS,
// 2026-10 bench sweep)
//
//	RMS   43   86   172   430   860  1720  3440  4303
//	per   19   23    20    28    28    28    28    27   (beat period, frames)
//	pat  41%  62%   66%   96%   99%   99%   99%  100%   (pattern vs the loud run)
//	hsh  88%  89%   91%   92%   92%   93%   93%   98%   (exact hash vs loud)
//
// Below roughly 400 counts the one-pole ladder's integer stalls are no longer
// negligible against the signal, the spectrum distorts, and the beat tracker
// locks to a different tempo - two earrings 20 dB apart would then disagree on
// everything, which is the exact failure this whole design exists to prevent.
// A real earring on a bench hears ~40 counts RMS (a loud speaker at 1 cm), so
// this is not a corner case: it is the operating point.
//
// So the normalizer holds each device's post-gain peak inside a band with a
// power-of-two gain, which the relative hash cancels exactly
// (TestHashLevelInvariant), and which keeps every device in the flat part of
// the sweep above.
const (
	// AgcHighPeak: above this post-gain peak the gain comes down, immediately,
	// to the largest value that fits. It sits ~1.4 octaves below the rail so
	// the next transient lands inside the bank's headroom instead of clipping.
	// A device louder than this with zero gain stays there: the sweep shows
	// agreement is flat all the way up, and attenuation would only throw signal
	// away.
	AgcHighPeak = 12000
	// AgcLowPeak: below this the gain goes up, slowly. The 4:1 band is the
	// point. With one threshold, music whose peak wanders near it toggles the
	// gain every few bars, and each toggle is a discontinuity the two earrings
	// hit on different frames - measurably worse than no AGC at all. Inside the
	// band nothing moves, so two devices converge and then sit still.
	AgcLowPeak = 3000
	// AgcMaxShift caps the gain at 2^6 = 64x, which puts the ceiling at a
	// 187-count pre-gain peak. Higher gains would amplify the front end's own
	// noise into the pipeline as if it were music - the onset detector has no
	// other way to tell them apart (see the silence-gate note in PARTY-SYNC).
	AgcMaxShift = 6
	// AgcMinShift allows attenuation down to 1/4. Not for headroom - the
	// saturation in bank.run handles a transient - but because the internal
	// level is itself the thing two devices must agree on: the sweep shows
	// decisions drifting whenever the post-gain peak sits more than a factor of
	// two apart, and a device beside the PA would otherwise sit a factor of 4
	// above the band no matter what it did. The cost is 2 bits of resolution on
	// the loudest inputs, which is nothing next to the drift it removes.
	AgcMinShift = 0
	// AgcPeakDecay is the peak-hold leak: 2^9 frames, a ~8 s time constant.
	// Far longer than a bar or a phrase on purpose: within a track the level is
	// set once and only a genuinely different track moves it. A fast leak makes
	// every quiet break a gain change, and the pumping that follows is what the
	// wide band above exists to avoid.
	AgcPeakDecay = 9
	// AgcHoldFrames is the minimum wait between gain *increases*: 2 s.
	// Decreases stay instant - a gain too high clips on the next transient and
	// is corrected the same frame, while a gain too low is silent and
	// persistent.
	AgcHoldFrames = 128
	// AgcFullScale saturates the amplified sample, protecting the bank's
	// "every node is bounded by the input amplitude" property. Note the gain is
	// applied *after* the bank's DC block, so the microphone bias (which on the
	// bench device is 15600 counts) is not part of what gets amplified.
	AgcFullScale = 1<<15 - 1
)

// Silence detection. A silent frame pushes nothing to the ring and triggers no
// decision, so a quiet room leaves the pattern alone instead of matching
// "everything at the floor" against the ring at distance 0.
//
// There are two independent criteria because they answer different questions and
// only one of them can answer the important one.
//
// The RELATIVE one (SilenceDiv) says "this frame is far below the loudest thing
// nearby". It is measured against the same signal in both earrings, so it lands
// on the same frames in both, which is what makes it safe. It cannot say "the
// room is empty": in an empty room the recent peak is the room's own noise, so
// nothing is ever far below it, and it never fires.
//
// That is what the ABSOLUTE one (SilenceEnterQ8) is for, and it is the reason
// Pipeline.Activity exists: an empty room is the case where two earrings can
// never agree - there is nothing in common between two microphones hearing
// nothing - so the only correct behaviour is for both of them to stop deciding.
// Every gain-free statistic in the pipeline fails at this because it is gain-free:
// the AGC lifts the room floor to the same range as quiet music and from then on
// the two are indistinguishable. The absolute level, with the AGC's own gain
// divided back out, is the one thing that still knows.
//
// Measured through the shipped pipeline (Pipeline.Activity, mean rectified band
// magnitude per sample in ADC codes, smoothed), against the bench front-end model
// at the levels TestBenchCapability sweeps:
//
//	input          p10    p50    p90     [codes]
//	room, clean L  6.8    7.1    7.4
//	room, noisy R 10.1   10.4   10.7
//	signal -36 dB 17.1   27.6   38.8   (L; R 19.4/30.2/39.4)
//	signal -42 dB 11.7   16.7   21.3   (L; R 14.2/19.5/22.7)
//	signal -48 dB  9.1   11.5   13.2   (L; R 11.7/14.4/15.4)
//	signal -60 dB  7.2    7.8    8.3   (L; R 10.4/10.9/11.3)
//
// The two rooms sit 3 codes apart, which is the measured difference between the
// devices' own noise floors and is why the threshold has to clear the noisier
// one rather than the average. 13 codes to enter sits above both rooms with the
// better part of a bit to spare and below a signal 36 dB down; 17 to leave is
// just over the p10 of a signal at that same level, so real music at a normal
// level always gets out, and the pair is left alone only where the level matrix
// says the two of them could not agree anyway (agreement is 34% at 48 dB down).
// Hysteresis rather than one number because the smoothed value still wanders by
// a fifth near the floor, and every edge is a frame on which one earring decides
// and the other holds.
const (
	SilenceEnterQ8 = 13 * 256 // below this, nothing is playing
	SilenceExitQ8  = 17 * 256 // above this, something is

	SilenceDiv   = 64 // 36 dB below the recent peak
	LoudDecay    = 7  // peak-hold leak: 2^7 frames ≈ 2 s to decay 6 dB
	LoudMinFloor = 1 << 12

	// ModPole is the time constant of the two modulation-depth trackers
	// (logAvg, logMod): 2^5 frames ≈ half a second, which is one beat at the
	// fast end and half a bar at the slow end - the window over which music is
	// supposed to move up and down. Measured over the same span, stationary
	// noise averages out and does not.
	ModPole = 5

	// The swing, in Q8 log2 units (256 = one bit of energy = 3 dB), that counts
	// as "something is happening". Two numbers rather than one so the state has
	// hysteresis: the trackers are smoothed, but not so smoothly that a single
	// threshold could not chatter at a boundary, and every edge is a frame on
	// which one earring decides and the other does not.
	//
	// Set from TestScratchGateTable, which runs the shipped pipeline over each
	// candidate input at every level and replays this tracker - including the
	// hysteresis - over the result. The last column is the fraction of frames the
	// gate is open, which is the only number that describes what the earrings do:
	//
	//	input                     p10    p50    p90    open
	//	nothing at all           0.12   0.12   0.12     0%
	//	noise floor, 1 count     0.80   0.87   1.00     0%
	//	noise floor, 3 counts    0.80   0.88   0.97     0%
	//	noise floor, 30 counts   0.78   0.88   0.98     0%
	//	noise floor, 300 counts  0.78   0.87   0.98     0%
	//	floor 2 + 90-count spikes 1.05  1.31   1.54    14%
	//	floor 5 + 90-count spikes 0.95  1.11   1.30     0%
	//	music -24 dB             2.05   2.56   2.97   100%
	//	music -48 dB             2.06   2.57   2.96   100%
	//	music -60 dB             2.03   2.51   2.89    99%
	//	music -60 dB + spikes    2.00   2.45   2.82    99%
	//	music -48 dB + 300 noise 0.72   0.81   0.93     0%
	//	talk -36 dB              1.59   1.80   2.50    99%
	//	talk -60 dB              1.55   1.77   2.42    97%
	//
	// Two things to read off this. The first is that the feature is completely
	// level-blind, which is the property that makes it usable: a recording 60 dB
	// down scores the same as one at full scale, because the AGC has already made
	// them the same signal and a ratio of logarithms cannot see the difference.
	// That is also the whole reason a gate is needed: after the AGC the earrings'
	// own noise is a perfectly normal looking input, and the two of them hashing
	// their independent noise is the ~8% pattern agreement measured on the bench.
	//
	// The second is that the gate cannot be what removes the microphone's
	// impulses. The row "floor 2 + spikes" is the impulse case and it leaks 14%;
	// it is the limiter in feed that takes the same input from 2.7 bits down to
	// 1.3, and the gate only finishes the job. Anything that modulates - a tap on
	// the table, a coat rustling in the dark, an earring hearing itself - reads
	// like music here and will open it, which is the correct answer: something
	// *is* happening, and both earrings should see it.
	//
	// 1.6 bits to enter sits above every quiet-room row's p90 and below every
	// recording's p10; 1.45 to hold keeps the state through the dips each
	// recording has without letting a leak latch on.
	ModEnterQ8 = 410
	ModHoldQ8  = 371

	// The two absolute sizes the impulse filter in Pipeline.feed decides on, both
	// in raw ADC codes at the microphone, which is where the samples arrive.
	//
	// SpikeCodesQuiet: how still a neighbourhood has to be before the filter will
	// act at all - the second-smallest of the four differences around the sample,
	// so two of the five samples are allowed to be impulses. The oversampled noise
	// floor measures 1-6 codes of deviation on the two bench devices, so sixteen is
	// three times the noisier floor: any input carrying real information fails this
	// test and passes through the filter untouched. That is the whole point - the
	// filter is not supposed to have an opinion about a signal, only about a spike
	// in a flat field. Below this, at 4 codes, it starts editing waveforms and the
	// level matrix gets worse rather than better.
	SpikeCodesQuiet = 16

	// SpikeCodesOutlier: how far out the fifth sample has to be, in codes, to be
	// called an impulse once its neighbourhood is known to be still. The bench
	// devices' impulses measure 40-90 codes; the same devices' noise moves about ten
	// at the outside, so this sits between the two and well under the impulses. It
	// is deliberately not a fraction of anything: the previous design made it one,
	// and a fraction of a signal's own scatter is a different rule at every input
	// level, which is the one thing a front end must not be. See feed for the
	// measurements of what that cost.
	SpikeCodesOutlier = 25

	// SpikeRelativeMul is how much further out than its own neighbourhood a
	// sample has to be to count as an impulse when the neighbourhood is NOT
	// still, which is the case whenever the earring is playing music and
	// animating at the same time. See feed: the quiet-field test alone leaves the
	// LED coupling completely untouched at the levels where the earrings work.
	//
	// Set from the captured windows (/tmp/psync/wave, tools/wavedump.py), where
	// dv is a sample's distance from the median of its five and d2 is the
	// second-smallest of the four differences inside that window.
	//
	// Pure tones, at the room's level (270 counts) and four times it: dv/d2 is
	// 0.00 to 1.00 up to 1 kHz, 1.15 at 3 kHz and 1.41 at 6 kHz - the worst a
	// waveform can do, because a point on a smooth curve sits one slope step
	// from its own median whatever its amplitude. The ratio is scale-free, which
	// is the property the absolute-only rule lacked.
	//
	// The captured windows while animating: the excursions (dv > 500) measure
	// dv/d2 = 76 at the median, 510 at p90 on R and 12252 on L. The samples
	// below the 500 gate reach p99 = 6 and p99.9 = 27-55, and the largest of
	// those are 400-count excursions in a still neighbourhood - smaller versions
	// of the same coupling, not music.
	//
	// Eight is therefore five times clear of the worst real waveform and fires
	// on the great majority of the coupling: at the d2 ≈ 100 a busy passage
	// measures, the threshold is 25 + 8*100 = 825 codes. Going to 64 approaches
	// the old quiet-field-only behaviour, which is what this constant exists to
	// replace; going to 2 starts editing the 3-6 kHz band.
	SpikeRelativeMul = 8
)

// Hash layout. See hashFrame for what each field means and why the hash is
// built out of differences.
const (
	// ShapeShift and ContrastShift are the bit offsets of the second and third
	// hash fields; the temporal field occupies the bottom NBands bits. The
	// shape field is two bits per band (except the bottom band), so it is
	// 2*(NBands-1) wide and the hot field starts at NBands + 2*(NBands-1).
	ShapeShift    = NBands
	ContrastShift = NBands + 2*(NBands-1)

	// HashMask covers every bit the hash uses: NBands temporal + 2*(NBands-1)
	// shape + NBands hot bits, i.e. the low HashBits bits. The spec allows
	// 15-32 bits per frame; 30 is what three live fields need without any of
	// them saturating (measured), and it still fits one register.
	HashMask = (1 << (ContrastShift + NBands)) - 1

	// HashBits is the population of HashMask: the width of a full Hamming
	// distance, used to scale the matching thresholds.
	HashBits = NBands + 2*(NBands-1) + NBands

	// IdentityMask selects the fields that describe the *section* - shape and
	// hot - and excludes the temporal field, which tracks push phase and
	// flickers even in steady state. Settle tracking and the anchor's repeat
	// guard use it, so temporal flicker neither fakes a section change nor
	// masks a real one.
	IdentityMask = HashMask & ^uint32((1<<NBands)-1)
)

// Tunables. See PARTY-SYNC.md §11.
const (
	// TemporalDeadband is the deadband on the temporal delta bits, in Q8 log2
	// units (256 == 6.02 dB): a band's smoothed energy has to rise by more than
	// this over one beat for its bit to be set. Without a deadband, near-zero
	// differences flip bits randomly between devices, so this is the single
	// most important knob for cross-device agreement. Because it is a
	// difference threshold it needs no knowledge of the device's gain.
	//
	// 32 (0.75 dB), tuned on the whole 60-minute podcast (not a synth patch):
	// the smoothed-vs-previous-push difference rarely reaches the old 3 dB, so
	// the field sat stuck at zero - the podcast-wide hash had 0.78 usable bits
	// and one word covered 84% of all beats. Halving it again measured no
	// better; this sits on the plateau between dead and churning. See
	// tools/hashsweep.py and syncdsp/podcast_hash_test.go.
	TemporalDeadband = 32 // 0.75 dB

	// ShapeLoQ8 and ShapeHiQ8 are the two thresholds of the shape field's
	// two-bit code, in Q8 log2 units above the fitted spectral line: code 1
	// from ShapeLoQ8, code 2 from ShapeHiQ8. The exact values are not
	// critical (anything that separates "on the line" from "a peak" works);
	// they are a quantisation of the spectrum's shape, and the cross-device
	// robustness comes from the fitted line, not from the thresholds.
	//
	// 48 and 96 (1.5 and 3 dB), from the podcast-wide sweep. Real spectra hug
	// the fitted line within a couple of dB, so the old 3/12 dB pair left most
	// shape bits at code 0 and the top code unreachable everywhere - the tier
	// was decoration, not information. With both thresholds live the code-2
	// tier fires on genuine spectral peaks, and the synth's break-vs-drop
	// sections separate again (distance >= DMin) while the podcast's worst
	// device still sits inside DAccept. 96 beat 128 and 192 on anchor
	// agreement (F1 0.73 vs 0.70/0.50 on the tilt device).
	ShapeLoQ8 = 48 // 1.5 dB
	ShapeHiQ8 = 96 // 3 dB

	// HotFloorQ8 is the hot-band field's threshold: a band within this much
	// (Q8 log2) of the loudest band of the smoothed spectrum is "hot". Unlike
	// RelFloorQ8 (the noise pin, 30 dB) this is deliberately tight: it is the
	// field that separates a narrow arrangement (a pad: one or two hot bands)
	// from a wide one (a full drop: most bands hot).
	//
	// 192 (4.5 dB), tuned on the whole podcast: EDM spans less than even 18 dB
	// across the band set, so the old value left every band hot everywhere -
	// all eight bits stuck at 1, zero usable information. 4.5 dB separates hot
	// from not without churning between devices (worst-device steady distance
	// p95 stays within DAccept, and TestAgreement's synth guards stay clean).
	HotFloorQ8 = 192 // 4.5 dB

	// SmoothShift is the time constant of the spectrum the hash and the pattern
	// are read from, in analysis frames: 2^5 = 32 frames, about half a second.
	// Long enough to cover a whole beat at any tempo in the tracked range, so the
	// smoothed spectrum does not depend on where the beat boundaries fell, and
	// short enough that a section change is visible within a bar. See endFrame
	// step 5 for why hashing the raw frame instead does not work at all.
	SmoothShift = 5

	// RingSize is the beat-indexed hash ring. It is indexed by *beat counter*
	// (not frame counter, not wall clock) because two earrings have no
	// shared clock: see PARTY-SYNC.md §D3. Must be a power of two.
	//
	// 64 slots, not more: the deepest entry ever read is LagBars3*BeatsPerBar +
	// LagTolerance = 34 beats back (novelty only looks 24 back), so a larger
	// ring would be dead RAM. 256 cost 1 kB of the 876 B budget for entries no
	// code path could reach.
	RingSize = 64

	// NoveltyWindow is how many beats back the anchor novelty check scans
	// (~10 s at 128 BPM).
	NoveltyWindow = 24

	// DMin: minimum Hamming distance from everything in the novelty window for a
	// beat to count as a transition anchor. Two bits of the 30: a section change
	// moves the shape/hot identity by several bits at once, while ordinary
	// beat-to-beat variation moves none.
	DMin = 2

	// DAccept: maximum Hamming distance for a periodicity match.
	DAccept = 3

	// LagTolerance is how many beats either side of each nominal lag the
	// periodicity search covers, absorbing the beat-count offset between
	// independently locked devices. Must keep the search windows of LagBars1/2/3
	// disjoint.
	LagTolerance = 2

	// LagBars are the "2, 4, 8 bars ago" periodicity confirmations.
	LagBars1 = 2
	LagBars2 = 4
	LagBars3 = 8

	// BeatsPerBar is 4/4 time, which EDM reliably is.
	BeatsPerBar = 4

	// AnchorRefractoryFrames: no second anchor within ~1 s. The settling-edge
	// trigger normally fires once per transition anyway; this only absorbs
	// identity wobble at the start of a section.
	AnchorRefractoryFrames = 64

	// AnchorRepeatDist is how far (in identity-field bits) the hash must move
	// past the one the last anchor fired on before a new anchor may fire.
	// Without it the settling edge retriggers many times per section: the hash
	// identity wobbles, and for the first couple of seconds of a new section
	// the ring window still holds the old section, so novelty stays high. The
	// steady-state identity wobble is ~0-1 bits and a real section change is
	// >=4, so this separates "same section, jitter" from "new section".
	AnchorRepeatDist = 4

	// AnchorSettleFrames is how many consecutive frames the hash *identity*
	// (every field except the temporal one, which legitimately flickers with
	// push phase even in steady state) must be unchanged before the hash is
	// taken to describe a settled section. The same gate guards ring pushes:
	// the ring must only ever contain settled hashes, so both novelty and
	// the periodicity match compare settled descriptors against each other
	// and never against a device-specific sample of the transition
	// transient. ~0.13 s, well inside the ~30-frame smoothing settling.
	AnchorSettleFrames = 8

	// AnchorOnsetMax: how recent (in frames) the last onset must be for the
	// anchor to fire - the brief's "strong onset" condition, evaluated on
	// the settling edge rather than at a beat boundary (a beat-phase gate
	// made the anchor depend on where each device's PLL happened to land,
	// which no two devices agree on). ~220 ms: longer than one beat at
	// 128 BPM, so a steady kick keeps it satisfied once the section starts.
	AnchorOnsetMax = 20

	// PatternRefractoryBeats is the minimum spacing between pattern changes
	// (1 bar). Without it a steady section matches the ring on *every* beat -
	//
	// This is what keeps the animation readable now that the choice itself is
	// stateless (see PatternIndex): the rate at which the spectrum is consulted is
	// limited, but the answer stays a pure function of it, so a device out of step
	// re-reads and converges instead of defending a position nobody else holds.
	// the hash is by design nearly constant between beats in a stationary
	// passage - and the animation flickers constantly. This is a visual
	// requirement as much as a robustness one: a change per beat is unreadable.
	PatternRefractoryBeats = 4
)

// PatternBands are the bands the pattern choice competes between, and the
// pattern is that band's position in this list.
//
// Every other band, not all of them, and that is measured rather than styled.
// Competing over all six interior bands was the original design, and on two
// 90 s captures of real material played over the bench speaker it put the two
// earrings on the same pattern for 34-51% of the time and left them stuck on
// *different* patterns for 41-64% of it, which is the complaint this whole
// exercise exists to answer. The cause is a ratio, not a bug: with six bands
// the gap between the winning band and the runner-up is 0.04 (in units of
// log2 band energy, so doublings) at the median, while two earrings measuring
// the same air disagree by 0.08. The discriminator is half the disagreement,
// so the winner is a coin flip - measured agreement 16.7%, exactly 1/6.
//
// Skipping bands fixes the ratio because neighbouring rungs of the ladder
// overlap (6 dB/oct skirts, 2/3 octave apart): neighbours are not independent
// measurements of different music, only of the same music plus noise, so an
// adjacent pair splits the choice without adding information. Three
// well-separated bands - 1.6-2.5 kHz, 630 Hz-1 kHz and 250-400 Hz - score
// 72-76% same-pattern and 4.5-5.6% stuck-different on both captures, on the
// metric that counts only stretches a viewer could notice (>= 2 s).
//
// Band 0 (>4 kHz) and band 7 (<250 Hz) are excluded as always: they are the
// two open-ended rungs, and they measure the least (disagreement 0.12 and
// 0.18 against 0.05-0.10 for the interior ones).
//
// PatternWeights replaces the candidate-band set above, and the reasoning that
// produced it is kept because the failure it fixes was expensive to find. What
// changed is that argmax over bands was measured against a weighted sum of all
// of them on two independent 285 s captures of real material, both sides polled
// over SWD, both selectors fed the same frames and re-read at the same rate:
//
//	argmax over {2,4,6}, 3 patterns:  same pattern 30-40% of the time,
//	                                  chance-corrected agreement -0.12 .. +0.07
//	centroid bucketed into 4:         same pattern 43-77%,          +0.24 .. +0.58
//
// The problem was never that argmax picked the wrong band. It is that picking a
// band discards the spectrum: the winner's identity survives and its margin does
// not, so a landslide and a coin flip give the same answer, and a band a hair
// ahead on one earring and a hair behind on the other gives two answers. A
// weighted sum keeps every band's magnitude and averages the independent noise
// in each of them, which is the same reason averaging sensors helps. It is also
// monotone in frequency, so two devices that disagree land on neighbouring
// colours instead of opposite ones - visible in the field as a slightly
// different shade rather than the wrong end of the spectrum.
//
// The ramp is not a fit. Directions chosen to maximise cross-device agreement on
// one capture (a cross-covariance eigenvector, per-band agreement weighting) were
// tried and did not transfer: fitted on one capture and scored on the other they
// were no better than this ramp and sometimes worse, while the in-sample numbers
// looked much better. That gap is the reason to ship the ramp: it has nothing to
// overfit.
//
// Band 0 is the top band (>4 kHz) and band 7 the bottom one (<250 Hz), so the
// weights are signed with bass positive: a large centroid is a bass-heavy moment.
var PatternWeights = [NBands]int32{-7, -5, -3, -1, 1, 3, 5, 7}

const (
	// NumPatterns is the number of animation patterns the sync mode cycles
	// through. Pattern selection is a pure function of the spectrum (see
	// PatternIndex): it must never use the RNG, and must never depend on the
	// pattern this device happens to be showing, or devices diverge.
	//
	// Four. The count is set by what two microphones can tell apart, not by the
	// size of the filter bank nor by how many animations exist. Cutting the
	// centroid into five buckets instead of four cost 8-9 points of agreement on
	// both captures (39% to 31%, and 37% to 28%), because every extra cut is
	// another place for two independent measurements to land on opposite sides.
	// numNoisePatterns is 5, so one animation goes unused in sync mode; the
	// requirement is that the earrings agree, not that every animation is used.
	NumPatterns = 4

	// PatternCentroidShift is the time constant of the spectral centroid the
	// pattern is read from, as a shift on the per-frame correction: 1/1024 of the
	// difference per analysis frame, so ~16 s at 62.5 Hz.
	//
	// This is far slower than the ~1 s the band-argmax version used, and the
	// measurement says so: with the centroid feature, cross-device agreement rose
	// monotonically with the window - 8 s gave 63% and 41% on the two captures, 16
	// s gave 75% and 43%. Longer is better here because the disagreement between
	// two earrings is *noise*, not a fixed offset: the spectral shape they each
	// measure correlates +0.7 to +0.9 once smoothed, so averaging removes exactly
	// the thing that was different. That diagnosis is the whole reason averaging
	// is the fix rather than a band re-pick, and it is the opposite of the earlier
	// conclusion, which was drawn from captures taken while no audio was reaching
	// the bench at all (see BENCH.md: a device that appears to hear nothing is a
	// claim about two things, and the capture has to be falsified first).
	//
	// It is worth one more sentence, because this constant is the obvious thing to
	// reach for and it is nearly spent. Smoothing the deviation the choice is made
	// on for a further 0.2 s to 1.2 s - measured on a paired RAM dump, renormalised
	// so the cuts keep meaning the same number of deviations - moves the correlation
	// in the two earrings' choices from +0.78 to +0.82 and their agreement from 55%
	// to 58%. That is what a curve flattened out at its plateau looks like: whatever
	// the two earrings still disagree about is not high-frequency, and buying more of
	// it costs latency on every colour change. Do not raise this constant hoping for
	// the old gains.
	PatternCentroidShift = 10

	// PatternScaleShift is the time constant of the centroid's own reference level
	// and spread. The spread sizes the steps that place the bucket boundaries, so it
	// has to be a real measurement of how far this music swings rather than a transient
	// reading; same shift as the centroid itself, so ~16 s.
	//
	// Both must be slow. They are what makes the colour mean "unusually bassy for
	// this music", and a fast reference would chase the very variation it is
	// supposed to measure, so that a long bass passage would redefine normal and
	// stop being bass. It is also what cancels each device's steady spectral tilt:
	// that tilt enters the centroid as a constant, and a slow reference absorbs any
	// constant.
	//
	// "Any constant" is exactly the claim, and the boundary of it is worth knowing.
	// What cancels is a difference the two devices hold still. What does not is a
	// difference that changes what each one's signal-to-noise ratio is, because that
	// changes the shape of the spectrum rather than adding to it. Measured on real
	// material through the shipped pipeline: independent hiss on the two microphones
	// costs nothing at all (99.8% agreement - the ~16 s average simply erases it), a
	// 6 dB level difference costs 17 points, and a 20 dB one costs 40. That is why
	// the bench's remaining disagreement is worth attacking at the rig first: one
	// earring was found sitting with the AGC pinned at zero shift while the other rode
	// two or three shifts up, which is that twenty dB.
	PatternScaleShift = 10

	// PatternCentroidScale is extra headroom on the stored centroid. An EMA updated
	// at 1/1024 per frame loses anything it is asked to track below ~1024 units, so
	// the quantity has to be big enough that its per-frame correction stays in the
	// linear range. Measured over 250 s of real material: the centroid spans
	// -17848 .. +37560 and its spread sits near 3000, which puts the smallest
	// meaningful correction at a few units rather than at zero. It cancels in the
	// bucket comparison, which divides by a spread measured in the same units, so it
	// costs nothing but headroom.
	PatternCentroidScale = 4

	// PatternCutQ8 is the distance from the reference, in units of the mean
	// absolute deviation, at which a bucket boundary sits: 216/256 = 0.844.
	//
	// That number is the first and third quartiles of a Gaussian expressed in MADs
	// (0.6745 sigma, and sigma = 1.2533 MAD). The colours sharing the time is a stated
	// requirement - one colour owning the evening is the "stuck on one pattern"
	// complaint - and placing the cuts by distribution aims at it without per-device
	// calibration and without storing a histogram.
	//
	// It aims; it does not hit, and the miss is measured rather than assumed. Over
	// 250 s of EDM the four buckets take 16/15/42/27 per cent, not 25 each: the
	// centroid's distribution is crowded near its middle and long-tailed, so a
	// Gaussian's quartiles sit too far out. Two fixes were tried and both are wrong,
	// which is why the crude ruler stays.
	//
	// Scaling each side of the reference by its own deviation is the obvious one, and
	// it makes the imbalance worse: the middle bucket grows to 46% at the same cut and
	// to 66% as the cuts widen. That is the answer to the question - the two sides
	// differ in *shape*, not scale, so no multiple of any spread places the cuts
	// correctly.
	//
	// Tracking the quartiles empirically does fix the occupancy, and it is the
	// interesting failure. Estimating each cut by the standard online update (step up
	// by p, down by 1-p, so the equilibrium sits at F(q) = p) and sizing the step as a
	// fraction of the spread gives 25/25/25/25 at every step from 1/16 to 1/64 of the
	// spread. It costs 40 points of agreement to get it: the same material scores
	// 59.6% between two devices against 99.8% for the fixed cuts, and changes pattern
	// 1450 times a minute instead of 5.4. The reason is that the fixed cuts are a
	// *derived* quantity - the same smoothed mean and spread on both devices, so both
	// earrings' boundaries move together and a difference in the music cannot put them
	// on opposite sides. Three independently converged estimates are three independent
	// random walks: each device's boundaries sit slightly differently, and the
	// centroid sits near a boundary a quarter of the time. The estimator's own noise,
	// which is unshared, becomes the devices' disagreement.
	//
	// So the unevenness is the price of the cuts being a function of shared
	// statistics, and it is worth paying. Sweeping the step found no value that was
	// both even and stable: below 1/512 the estimates stop converging within a track
	// and 92% of the time lands in one bucket. See TestCentroidFeature, which
	// compares the two rulers over the same recorded spectra.
	PatternCutQ8 = 216

	// PatternMaxHoldFrames is the longest the pattern is ever allowed to go without
	// being re-read, in analysis frames: 4 s at 62.5 Hz.
	//
	// Every other trigger for re-reading is conditional on something going well. They
	// all sit behind decide(), which is only reached on a beat boundary or an onset, and
	// only while the hash has settled - and on a quiet input neither is guaranteed. A
	// device that meets none of them has no code path that ever calls setPattern, so it
	// holds whatever the warmup gave it, which is the middle colour, for as long as it
	// stays deaf. Its partner, hearing the same music a little better, keeps changing. To
	// anyone watching, the two earrings are stuck on different colours - the complaint
	// this feature exists to settle - and no amount of tuning the choice fixes it, because
	// the choice is rarely the problem. Measured at the bench's quiet operating point,
	// comparing a clean device with one 42 dB down: 28% agreement on the committed pattern
	// before this existed, 81% after, with the selection code unchanged in between.
	//
	// Re-reading on a clock is safe here in a way it would not be for a choice with
	// memory: the pattern is a pure function of the centroid and its running statistics,
	// so two devices sampling on unrelated frames land on the same answer and stay there.
	// The cost is up to one window of latency on a change, never a disagreement.
	//
	// 4 s is set above a bar of music at any tempo the tracker locks to, so when the
	// musical triggers do fire they still decide when a change lands and this only catches
	// the cases they miss.
	//
	// Measured on the real pair, this is necessary and not sufficient. Both earrings
	// flashed with it, hearing one track from one speaker, were locked and live - all
	// four colours seen, changes arriving every fifteen seconds or so - and agreed on
	// 63% of frames, with 35% of the run spent on two different colours. The freeze
	// this exists to fix is gone; the disagreement is not, and it is not this constant's
	// to fix.
	//
	// What the two devices disagree about is the size of a deviation they agree is
	// happening. Read out of RAM over 165 s (tools/patternstate.py, and the z
	// histogram in tools/cutfit.py), the correlation between their scaled deviations is
	// r = +0.78 and they carry the same sign in 79% of samples: they hear the same
	// rising and falling passage, and differ about how far it puts them from their own
	// reference. The measured z distribution is close to symmetric and close to the
	// same shape on both - quartiles about -0.8 / 0 / +0.7 - which is why the cuts sit
	// where they do and why moving them cannot help: sweeping the one free parameter k
	// across 0.4 to 1.5 on that data makes the least-used colour's share worse at every
	// value except the shipped 0.844.
	//
	// Two fixes were tried on hardware and both are gone. Subtracting a slow per-band
	// reference made the level matrix uniformly worse, because it high-passes the
	// centroid that centAvg already high-passes. Flooring the divisor at 2048 - so a
	// device whose centroid barely moves cannot amplify its own drift into a full-scale
	// choice - looked like a 9.6% to 96% fix on 57 s captures, and was wrong: over 177 s
	// it measured 52% against 63% without it, and emptied one colour entirely. The
	// short captures were one quiet passage and the honest number is the long one. Any
	// future change here has to be measured over three minutes or more, because a
	// minute is not long enough to contain the material that distinguishes the two.
	PatternMaxHoldFrames = 250

	// PatternMinDev is the centroid spread below which the pattern is not read, in
	// the stored centroid's own units. See PatternIndex: it is both the
	// division guard and the "nothing is moving, stay in the middle" rule.
	PatternMinDev = 8

	// PatternWarmupFrames suppresses pattern decisions until the centroid's
	// reference and spread are worth reading: with a ~16 s time constant neither is
	// meaningful after one second, and with a spread near zero every bucket boundary
	// collapses onto the reference, which flickers between two colours on noise.
	// 8 s is a compromise: long enough that the spread is real, short enough that
	// the earrings do not sit on one colour through the start of a track.
	PatternWarmupFrames = 500

	// WarmupFrames suppresses decisions while the DC tracker and per-band
	// AGC converge (~1 s).
	WarmupFrames = 64

	// BeatMinPeriod and BeatMaxPeriod bound the beat period search, in
	// analysis frames (15.6 ms each): 10 frames ≈ 240 BPM, 49 frames ≈ 49
	// BPM. EDM is far inside this.
	BeatMinPeriod = 18
	BeatMaxPeriod = 49

	// TempoWindow is the envelope history the beat tracker correlates: 256
	// frames = 4 s, a power of two so the ring wraps with a mask instead of a
	// compare. Windows from 3 to 6 s score the same on real material (within a
	// couple of percent of correctly-picked lag), so the shortest one that
	// still holds 5 beats at the slowest tracked tempo wins on RAM.
	//
	// 512 (8 s) was tried, twice, and is measurably worse on the device. The
	// motivation was real - 26-vs-27 is a genuine near-tie of this estimator
	// (device run pairs split it 5 times in 15, and the host margin is only
	// 13%, stable across capture phase and noise) - but doubling the window did
	// not buy resolution, it bought trend: the host margin *shrank* to 10% on
	// the bench-channel clips, and on the earring - which hears bass rumble the
	// host model does not - the scan locked onto period 18, BeatMinPeriod, and
	// stayed there. Autocorrelation of low-frequency wander rises toward short
	// lags, and a 2x longer window with the ref kept at ring-length/2 (>>10) is
	// 2x more wander. Slowing the ref cannot fix that (same ratio, same wander
	// relative to the ring); high-passing it harder is the documented failure
	// mode in pushEnv. 4 s it stays.
	TempoWindow = 256
	EnvMask     = TempoWindow - 1
	// TempoStep is how often the scan runs: every 2 s. A scan is 32 candidate
	// lags x 256 products, so the cost is bounded by one 8k-MAC burst per 2 s -
	// under 1% of a frame on average on a 16 MHz M0+ next to the filter bank.
	TempoStep = 128
	// TempoScale is the Q10 fixed point the per-lag evidence is kept in: a
	// candidate's correlation divided by the envelope's own variance, times
	// 1024. Normalising matters because raw correlation scales with how loud
	// the modulation is, and the same song moves between pianissimo and a drop;
	// devices (and the same device over time) have to compare like with like.
	TempoScale = 1024
	// TempoMinEvidence is the peak normalised correlation required before the
	// tracker claims a tempo at all, in TempoScale units: 0.1875. Measured over
	// the five real samples, the peak sits at 0.42-0.67 of the envelope's
	// variance where the kick is present and under 0.1 where it is not - a
	// breakdown, a stretch of talk. Locking onto those is worse than not
	// locking: the period is then wrong for as long as it takes to notice.
	TempoMinEvidence = 192
	// TempoEvidenceShift is the time constant of the accumulator that averages
	// each candidate's evidence over successive scans, as a shift: 1/8 per scan
	// over a 2 s scan is ~16 s of evidence. Averaging is what makes the choice
	// stable: single 4 s windows pick the right lag only 57-61% of the time,
	// while the accumulated evidence settles on the right lag on all five
	// samples and stays there.
	//
	// 1/4 -> 1/8 was made for the run-to-run tie, not for correctness, and won
	// on the device. The 26-vs-27 choice on the test clip is a stable near-tie
	// on the host (13% margin, invariant to capture phase, noise seed and
	// channel model - the estimator's resolution, not noise), but at 1/4 the
	// earring split it run to run. Matched 8-run batches, same clip, same seat,
	// LEDs dark (tools/runbatch.sh, scored by tools/batchscore.py):
	//
	//	                   period agreement   changes of mind   anchor F1
	//   1/4 (shift 2)         21/28              22             0.33
	//   1/8 (shift 3)         28/28               4             0.38
	//
	// The identity fields are untouched by this constant and measured the same
	// in both batches (1.28 vs 1.35) - the agreement gain is entirely the tempo
	// no longer wandering: at 1/4 individual runs made up to 6 excursions
	// (26->20->26->27 in one) while at 1/8 the worst run made one 10 s detour.
	// Excursions are the anti-sync event: two earrings hearing identical music
	// take them at different times, because their cause - which 4 s of noise
	// the EMA happened to be holding - is per-device. What 1/8 gives up is
	// reconvergence after a genuine tempo change (~16 s of evidence instead of
	// ~8 s, ~40 s instead of ~26 s to fully flip the accumulator on the test
	// case); the party use case tolerates that, and no host test measures it -
	// guards and the full-podcast F1 harness pass identically at both values.
	TempoEvidenceShift = 3
	// BeatRefractoryFrames: minimum spacing between onsets (~125 ms).
	BeatRefractoryFrames = 15
)
