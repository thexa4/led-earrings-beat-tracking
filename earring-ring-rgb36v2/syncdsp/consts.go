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

// Silence detection, also relative for the same reason: a frame is silent
// when it is far quieter than the loudest frame seen recently. Silent frames
// push nothing to the ring and trigger no decision, so a quiet room leaves the
// pattern alone instead of matching "everything at the floor" against the ring
// at distance 0.
const (
	SilenceDiv   = 64 // 36 dB below the recent peak
	LoudDecay    = 7  // peak-hold leak: 2^7 frames ≈ 2 s to decay 6 dB
	LoudMinFloor = 1 << 12
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

// PatternWeights collapse the smoothed spectrum into the one number the pattern
// is read from (band 0 is >4 kHz, band 7 is <250 Hz). They must sum to zero, so
// that a gain - the AGC's, or one microphone being quieter - cancels out.
var PatternWeights = [NBands]int32{-7, 0, 0, 7, 0, 0, 0, 0}

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

	// SpikeCodesQuiet, SpikeCodesOutlier and SpikeRelativeMul are the impulse
	// filter's thresholds (see feed), in raw ADC codes.
	//
	// SpikeCodesQuiet is how still a neighbourhood must be (its second-smallest
	// adjacent difference) for SpikeCodesOutlier alone to decide. Bench devices
	// with the LEDs dark have a 1.5-4.5 code floor carrying isolated 40-90 code
	// impulses on ~3% of samples; at 4 codes the filter starts editing waveforms.
	//
	// SpikeRelativeMul is the test while music plays and the LEDs animate, where
	// the neighbourhood is never still: a sample counts as an impulse when it sits
	// more than SpikeCodesOutlier + SpikeRelativeMul*d2 from its five-sample median.
	// A point on a smooth waveform sits about one slope step from its median at any
	// amplitude: dv/d2 measured 0-1.0 up to 1 kHz, 1.41 at 6 kHz. The LED coupling,
	// up to +16381 codes one sample wide against 200-270 counts of room tone,
	// measured dv/d2 = 76 at the median and 510-12252 at p90. 2 starts editing the
	// 3-6 kHz band; 64 is back to the quiet-only rule, which leaves the coupling
	// untouched exactly while it is loudest.
	SpikeCodesQuiet   = 16
	SpikeCodesOutlier = 25
	SpikeRelativeMul  = 8

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
	// the hash is by design nearly constant between beats in a stationary
	// passage - and the animation flickers constantly. This is a visual
	// requirement as much as a robustness one: a change per beat is unreadable.
	PatternRefractoryBeats = 4

	// NumPatterns is the number of animation patterns the sync mode cycles
	// through. Pattern selection is a pure function of the spectrum (see
	// PatternIndex): it must never use the RNG, and must never depend on the
	// pattern this device happens to be showing, or devices diverge.
	//
	// Four, set by what two microphones can tell apart rather than by how many
	// animations exist: every extra cut is another place for two independent
	// measurements to land on opposite sides. PatternIndex has one cut per
	// boundary, so changing this means changing the cuts too.
	NumPatterns = 4

	// PatternCentroidShift, PatternScaleShift and PatternGapShift are the time
	// constants of the centroid, of its reference and spread, and of the low-pass
	// on its deviation, as shifts at 62.5 frames/s (10 = 16 s).
	//
	// Chosen together with PatternWeights and the cuts by replaying every
	// combination over two real two-earring bench captures of different stretches
	// of the podcast (both earrings' spectra and beat events, tools/paircapture.py
	// --state; TestBenchMulti in bench_replay_test.go), each from a cold start, and
	// ranking by the worse of the two:
	//
	//	                                  kappa 0-19 min / 20-40 min   least-used pattern
	//	b3-b0, 10/10/10, cuts +-216        0.75 / 0.74                  19% / 20%
	//	b4-b0, 11/12/9, cuts +-256         0.75 / 0.60                  21% / 19%
	//	b4-b0, 11/11/11, cuts +-150        0.67 / 0.65                  14% / 14%
	//	best multi-band weights            0.57 / 0.56                  17% / 16%
	//
	// b4-b0 at 11/12/9 was the winner on the first capture alone and lost 0.15 on
	// the second, so a single stretch of music is not enough to tune on. The
	// neighbours of the choice (b2/b3 splits, cuts +-180) all score 0.72-0.75. The
	// simulated two-device sweep (podcast_pattern_test.go) prefers multi-band
	// weights, which the real captures reject: the simulation is a parity check
	// here, not a tuner.
	PatternCentroidShift = 10
	PatternScaleShift    = 10
	PatternGapShift      = 10

	// PatternCentroidScale is headroom on the stored centroid so that the EMAs'
	// per-frame corrections stay in their linear range. It cancels in
	// PatternIndex, which divides by a spread in the same units.
	PatternCentroidScale = 4

	// PatternCut1..3 are the bucket boundaries, in Q8 units of the centroid's
	// mean absolute deviation from its reference.
	PatternCut1 = -216
	PatternCut2 = 0
	PatternCut3 = 216

	// PatternMaxHoldFrames is the longest the pattern goes without being re-read
	// (4 s), the backstop for a device whose musical triggers never fire. Above a
	// bar at any tracked tempo, so the musical triggers still decide when a change
	// lands whenever they work.
	PatternMaxHoldFrames = 250

	// PatternMinDev is the spread below which the pattern is not read: nothing
	// is moving, stay in the middle. Also PatternIndex's division guard.
	PatternMinDev = 8

	// PatternWarmupFrames holds the middle pattern for 8 s, until the centroid's
	// reference and spread mean something.
	PatternWarmupFrames = 500

	// PatternSeedFrames is how long the centroid's statistics are held at the
	// current value before they start averaging: 2 s, by which time the DC
	// blocker and the 0.5 s spectrum smoothing have absorbed the power-on step.
	PatternSeedFrames = 128

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
