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

// Tunables. See PARTY-SYNC.md §11.
const (
	// TemporalDeadband is the deadband on the temporal delta bits, in Q8 log2
	// units (256 == 6.02 dB): a band's smoothed energy has to rise by more than
	// this over one beat for its bit to be set. Without a deadband, near-zero
	// differences flip bits randomly between devices, so this is the single
	// most important knob for cross-device agreement. Because it is a
	// difference threshold it needs no knowledge of the device's gain.
	TemporalDeadband = 128 // 3 dB

	// ShapeLoQ8 and ShapeHiQ8 are the two thresholds of the shape field's
	// two-bit code, in Q8 log2 units above the fitted spectral line: code 1
	// from ShapeLoQ8, code 2 from ShapeHiQ8. The exact values are not
	// critical (anything that separates "on the line" from "a peak" works);
	// they are a quantisation of the spectrum's shape, and the cross-device
	// robustness comes from the fitted line, not from the thresholds.
	ShapeLoQ8 = 128 // 3 dB
	ShapeHiQ8 = 512 // 12 dB

	// HotFloorQ8 is the hot-band field's threshold: a band within this much
	// (Q8 log2) of the loudest band of the smoothed spectrum is "hot". Unlike
	// RelFloorQ8 (the noise pin, 30 dB) this is deliberately tight: it is the
	// field that separates a narrow arrangement (a pad: one or two hot bands)
	// from a wide one (a full drop: most bands hot).
	HotFloorQ8 = 3 << 8 // 18 dB

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
	// the hash is by design nearly constant between beats in a stationary
	// passage - and the animation flickers constantly. This is a visual
	// requirement as much as a robustness one: a change per beat is unreadable.
	PatternRefractoryBeats = 4

	// NumPatterns is the number of animation patterns the sync mode cycles
	// through: one per band that can win the pattern choice, so the mapping from
	// spectrum to pattern is one-to-one and needs no modulo. Pattern selection
	// is a pure function of the spectrum (see PatternIndex): it must never use
	// the RNG, or devices diverge.
	//
	// Must equal NBands-1; TestPatternIndexStable asserts it.
	NumPatterns = 7

	// PatternMargin is the hysteresis a band needs in order to take the pattern
	// from the band on screen, in the deviation units of spectralDevs (Q8<<8, so
	// 256<<8 is 6 dB). Adjacent bands in real music are often a fraction of a dB
	// apart, and without a margin two devices agreeing to within a bit still end
	// up on different patterns.
	PatternMargin = 32 << 8

	// WarmupFrames suppresses decisions while the DC tracker and per-band
	// AGC converge (~1 s).
	WarmupFrames = 64

	// BeatMinPeriod and BeatMaxPeriod bound the beat period search, in
	// analysis frames (15.6 ms at the true 64 fps frame rate): 18 frames
	// ≈ 213 BPM, 49 frames ≈ 49 BPM. EDM is far inside this. The floor
	// used to be 10 (≈ 426 BPM at 64 fps) from the 8.8 kSPS era; at the
	// real frame rate it admitted hat-rate intervals, which voted and won:
	// confident double-time locks (274-349 BPM) on 143 BPM music.
	BeatMinPeriod = 18
	BeatMaxPeriod = 49

	// BeatRefractoryFrames: minimum spacing between onsets (~234 ms).
	// Was 8 (~125 ms), which let open hats 13-14 frames after a kick
	// register as separate onsets at 64 fps.
	BeatRefractoryFrames = 15

	// LockCount is how many interval-histogram votes are needed before the
	// beat tracker claims lock.
	LockCount = 12
)
