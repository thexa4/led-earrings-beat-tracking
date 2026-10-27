package main

// Party sync mode: multiple earrings listening to the same music derive the
// same pattern decisions from it (no radio). The DSP lives in the syncdsp
// package (host-tested); this file is the device plumbing: capture setup,
// non-blocking sample feeding, and the animation state.
//
// Clock/ADC setup follows the capture half of dataRecv() (that
// configuration is proven with 32x oversampling) but runs the CPU from HSI16
// at 16 MHz instead of MSI at 4.2 MHz: measured over SWD, the feed DSP
// saturates the core at 4.2 MHz (~475 cycles/sample incl. bit-bang and
// animation), so there the ADC free-runs at ~8.8 kSPS with every other
// trigger dropped and a "15.6 ms" analysis frame is really 28.4 ms. Sync
// still worked at that rate - every device made the same mistake, and the
// agreement is relative - but it put all frame-unit constants in syncdsp
// 1.82x off their documented wall time. At 16 MHz the 62.5 us sample period
// is real: TIM22 divides HSI16 down to 16 kHz (measured 15.92 kSPS on R:
// HSI16 is 0.5 % slow there) and the ADC converts from HSI16/2 in 56 us.
// (16 MHz needs voltage range 2 and one flash wait state, which cost battery
// current - accepted; the frame-unit constants in syncdsp are tuned for the
// true 64 fps frame rate, see PARTY-SYNC.md §11.1.)
//
// Running above the idle clock has two consequences the loop structure must
// respect:
//
//   - The bit-bang multiplexer is cycle-counted, so a full refresh runs 4x
//     faster. That is harmless for the LEDs (relative duties are cycle
//     ratios, and the refresh only gets less flickery), but animations
//     driven by the main loop's frame counter would speed up too. So sync
//     mode ignores that counter: syncAnimate reads syncAnimFrame, which
//     advances once per analysis frame instead.
//
//   - A full pipeline frame takes ~185k cycles (~11.5 ms at 16 MHz, three
//     quarters of the CPU: PC-sampled over SWD, with the flash prefetch;
//     ~10 % more without it). Running
//     that in one burst would freeze the multiplexer that long, holding one
//     LED group on visibly. So samples are fed incrementally: each main-loop
//     iteration hands the pipeline whatever the DMA has written since the
//     last iteration (bounded burst), and only the frame *boundary* work
//     (log2/hash/beat/decide, ~2k cycles) runs at once.

import (
	"device/stm32"
	"unsafe"

	"github.com/aykevl/things/earring-ring-rgb36v2/syncdsp"
)

const (
	// Analysis window in samples: one DMA buffer, one pipeline frame.
	syncWindowSize = syncdsp.Window

	// TIM22 prescaler: fs = 16 MHz / (2 * (syncPSC+1)) = exactly 16000 Hz
	// (PCLK = HSI16, see startSync; the old 4.194 MHz / 2*131 setup produced
	// 15996.6 Hz on paper but 8.8 kSPS in practice - see the file comment).
	syncPSC = 499

	// Maximum samples fed to the pipeline per main-loop iteration: it bounds
	// catch-up bursts, so the multiplexer is never held for long. Raising it
	// to 32 or 200 did not change what the pipeline consumed (measured with
	// -tags syncboot): at 16 MHz the loop keeps up. At 4.2 MHz it *was* the
	// steady state (24 × ~370 Hz ≈ the 8.8 kSPS the starved CPU managed).
	syncFeedCap = 24
)

// The pipeline state. Note: overlaying this on animationBuf to save RAM is
// tempting (the two are never live at once) but costs ~600 bytes of flash in
// the sample loop, because the unsafe.Pointer cast makes every pipeline
// store alias the DMA buffer and kills register allocation. Keep it a plain
// global; the Pipeline struct is kept small instead.
var syncPipe syncdsp.Pipeline

var (
	// State read by syncAnimate (and written by syncPoll).
	syncAnimFrame int   // advances once per analysis frame (~16 Hz)
	syncVariant   int   // current pattern, from the pipeline
	syncBeatAge   int32 // analysis frames since the last beat
	syncAnchorAge int32 // analysis frames since the last anchor
	syncLocked    bool
	syncSilent    bool

	// State for the hash-view variant (sync variant 1), updated in syncPoll
	// once per analysis frame and read per LED by syncHashAnimate.
	syncHashCol   Color  // solid section color: hash at the last anchor
	syncBeatCount int32  // beats since startSync, for the marker position
	syncDotQ8     uint32 // marker position, 1/256 LED units, wraps at 36

	syncRunning bool  // capture is up (also: stopSync is idempotent)
	syncRead    int   // next sample to consume from adcData
	syncFed     int32 // samples fed into the current analysis frame
)

// startSync resets the pipeline and starts DMA capture. Safe to call from any
// mode: it configures clock, ADC, timer and DMA from scratch, like
// dataRecv() does (the ADC was reset by APB2RSTR on the way out of whatever
// ran before).
func startSync() {
	syncPipe.Init()
	syncAnimFrame = 0
	syncVariant = 0
	syncBeatAge = 100
	syncAnchorAge = 100
	syncLocked = false
	syncSilent = false
	syncHashCol = 0
	syncBeatCount = 0
	syncDotQ8 = 0
	syncRead = 0
	syncFed = 0
	syncRunning = true

	// System clock: HSI16 at voltage range 2 (see hsi16Up), as dataRecv.
	hsi16Up(stm32.PWR_CR_VOS_V1_5)

	// Capture: one circular 250-sample window at exactly 16 kSPS
	// (16 MHz / (2 * (499+1))). adcData is 400 samples long (dataRecv's
	// double symbol buffer); sync mode uses the first 250 of it, reusing
	// existing RAM rather than declaring a second buffer. The ADC clock is
	// HSI16/2, the 8 MHz range 2 allows: a 32x conversion takes 56 us of the
	// 62.5 us period (see capture.go; at /4 every other trigger is missed,
	// measured). Until capture.go wrote it before ADEN, this setting was
	// ignored and the ADC ran HSI16 undivided; the noise did not measurably
	// change with the fix.
	startCapture(uint32(uintptr(unsafe.Pointer(&adcData[0]))), syncWindowSize,
		syncPSC, 1<<stm32.ADC_CCR_PRESC_Pos)
}

// waitForVOS waits for the power regulator to finish the voltage-scale switch
// started by the write to PWR_CR.VOS. VOSF means "switching": the hardware
// sets it while the regulator moves and clears it when the new scale is
// reached - so the only correct wait is "until it reads 0", bounded (a switch
// takes tens of us; the bound is orders of magnitude above that even at the
// idle clock).
//
// An earlier version here waited for VOSF to *rise* first, on a misreading of
// the RM ("set while switching" taken as "will set on the write"). The
// regulator often finishes before software samples the bit - it never
// observes the rise then, and on a switch that sets nothing the loop hung
// forever (measured: CFGR never written). The iteration bound below is
// belt-and-braces: the exit is data-dependent on a register LLVM cannot
// prove changes, so an unbounded wait could also be optimised oddly.
//
//go:noinline
func waitForVOS() {
	for i := 0; i < 100000 && stm32.PWR.CSR.Get()&stm32.PWR_CSR_VOSF_Msk != 0; i++ {
	}
}

// stopSync tears down everything startSync turned on, restoring the low-power
// clock state (mirrors the teardown half of dataRecv, minus the LED handling
// - the LEDs were never stopped here).
func stopSync() {
	if !syncRunning {
		return
	}
	syncRunning = false

	stopCapture()

	hsi16Down()
}

// syncPoll consumes everything the DMA has written since the last call and
// runs the pipeline boundary work when a frame completes. Called once per
// main-loop iteration (in place of updateMic).
//
//go:noinline
func syncPoll() {
	syncProbe() // -tags syncboot only
	// DMA write position: NDTR counts *remaining* slots down, so the next
	// sample lands at 250-NDTR.
	write := int(syncWindowSize - stm32.DMA1.CH[0].NDTR.Get())
	if write >= syncWindowSize {
		write = 0 // NDTR==0: the last write was the final slot
	}

	// Samples available without lapping the writer. The producer adds ~11 per
	// iteration and never runs ahead of the buffer, so this difference is
	// always the true backlog; syncFeedCap bounds the burst. A frame takes
	// exactly Window samples: the rest wait for the next call. (Feeding the
	// whole backlog made every frame 250 plus up to the cap, ~1 % long on
	// average and different each frame; measured with -tags syncboot.)
	avail := (write - syncRead + syncWindowSize) % syncWindowSize
	avail = min(avail, syncFeedCap, syncdsp.Window-int(syncFed))

	for range avail {
		syncPipe.ProcessSample(adcData[syncRead])
		syncRead++
		if syncRead >= syncWindowSize {
			syncRead = 0
		}
		syncFed++
	}

	if syncFed < syncdsp.Window {
		return
	}
	syncFed = 0

	// Frame boundary: the cheap part (~2k cycles: log2, hash, beat tracker,
	// decisions). Runs at most ~16 times/s.
	ev := syncPipe.EndFrame()

	syncAnimFrame++
	syncBeatAge++
	syncAnchorAge++
	if ev.Beat {
		syncBeatAge = 0
	}
	if ev.Anchor {
		syncAnchorAge = 0
		// The hash-view variant shows the section's hash as a solid color,
		// changed here at the anchor - the moment the section (and therefore
		// the color) is decided. Held until the next anchor, so two earrings
		// hearing the same music display the same color for the same section.
		syncHashCol = hashColor(ev.Hash)
	}
	if ev.Beat {
		syncBeatCount++
	}
	if ev.PatternChanged {
		// Map pattern -> palette here rather than per LED in syncAnimate:
		// animate runs 36 times per frame, patterns change once per bar.
		syncVariant = int(ev.Pattern) % numNoisePatterns
	}
	syncLocked = ev.Locked
	syncSilent = ev.Silent

	// Beat marker position for the hash view: the dot travels a quarter of the
	// ring (9 of 36 LEDs) per beat, smooth between beats by interpolating the
	// tracker's own period. One division per frame on a machine that does 64
	// frames/s with ~2k cycles of boundary work - noise.
	per := syncPipe.BeatPeriod()
	if per < 1 {
		per = 26 // before the first lock: keep the position well-defined
	}
	age := syncBeatAge
	if age > per {
		age = per
	}
	syncDotQ8 = (uint32(syncBeatCount)*9<<8 +
		(9<<8)*uint32(age)/uint32(per)) % (36 << 8)
}

// syncAnimate draws one LED for party sync mode.
//
// It deliberately does not take the main loop's frame counter: at the sync
// clock rate that counter advances 4x faster than the animations were tuned
// for. syncAnimFrame advances once per analysis frame (64 Hz), which is both
// a good animation rate and the rate every other earring is animating at, so
// the same pattern on two earrings is frame-locked as well as music-locked.
//
// Variant 1 is not an animation but an instrument: see syncHashAnimate.
func syncAnimate(led, variant int) Color {
	if variant == 1 {
		return syncHashAnimate(led)
	}

	// The pipeline picks one of NumPatterns patterns from the spectrum (a
	// pure function, identical on every device); the noise palettes give
	// them visibly different looks.
	c := noise(led, syncAnimFrame, syncVariant)

	switch {
	case !syncLocked:
		// Still finding the tempo: dim, so a pair of earrings visibly
		// settles when both lock.
		c = c.Scale(80)
	case syncAnchorAge <= 1:
		// Transition anchor: a white flash. This is the moment the
		// earrings are meant to snap together, so make it obvious.
		c = NewColor(255, 255, 255)
	case syncBeatAge == 0:
		// Beat: flash toward white for one analysis frame (~60 ms).
		c = NewColor(
			uint8(int(c.R())+(255-int(c.R()))/2),
			uint8(int(c.G())+(255-int(c.G()))/2),
			uint8(int(c.B())+(255-int(c.B()))/2))
	}

	if syncSilent {
		// Nobody playing music: nearly dark, but not fully off so the
		// mode doesn't look broken.
		c = c.Scale(40)
	}

	return c
}

// syncHashAnimate is sync mode's second variant: a readout of what the
// pipeline knows, for verifying several earrings in a room against each other
// (and against the music) at a glance.
//
//   - The whole ring shows one solid color, the current section's hash mapped
//     to hue. It changes at anchors - the moments an earring decides the music
//     moved to a new section - so two earrings in the room snap to the same
//     color at the same instant if they agree, and a color that never changes
//     (or changes at different times) reads immediately as no anchor / wrong
//     anchor.
//   - A single dot travels a quarter of the ring per beat - one lap per bar -
//     and blinks black/white at ~11 Hz (every 3 of the 64 analysis frames:
//     fast enough to read as a strobe, slow enough to stay distinct on video).
//     Two earrings whose dots chase at the same tempo are locked to the same
//     beat period; the dot position within the lap shows the bar phase.
//   - Unlocked: the ring stays dark apart from a dim base, so lock status is
//     readable from the color appearing at all.
//
// All the state (syncHashCol, syncDotQ8) is computed once per frame in
// syncPoll; this runs per LED.
func syncHashAnimate(led int) Color {
	if syncSilent {
		return Color(0)
	}
	if !syncLocked {
		// Finding the tempo: dim uniform glow, no color, no dot.
		return NewColor(24, 24, 24)
	}
	c := syncHashCol
	if syncAnchorAge <= 1 {
		// The anchor frame itself: one white frame so the snap is obvious.
		return NewColor(255, 255, 255)
	}

	// Dot at syncDotQ8>>8, blended into its neighbour by the fraction, so it
	// slides instead of jumping (9 LEDs per beat at 64 fps is a third of an
	// LED per frame - unblended that would look like a strobe, not a sweep).
	p0 := int(syncDotQ8 >> 8)
	frac := uint32(syncDotQ8 & 0xff)
	w := uint32(0)
	switch (led - p0 + 36) % 36 {
	case 0:
		w = 256 - frac
	case 1:
		w = frac
	}
	if w > 0 {
		// Blink ~11 Hz: dot alternates white and off (black), so its
		// position reads even against a light-colored background.
		var tr, tg, tb uint32
		if (syncAnimFrame/3)%2 == 0 {
			tr, tg, tb = 255, 255, 255
		}
		return NewColor(
			uint8((uint32(c.R())*(256-w)+tr*w)>>8),
			uint8((uint32(c.G())*(256-w)+tg*w)>>8),
			uint8((uint32(c.B())*(256-w)+tb*w)>>8))
	}
	return c
}

// hashColor maps a section hash to a solid color: hue from the hash bits,
// full saturation and value so every section is a distinct primary. The 29
// hash bits are folded so all three fields (temporal, shape, hot) influence
// the hue - the point is to *see* the hash, and two hashes differing in any
// field should not look the same.
func hashColor(h uint32) Color {
	x := h ^ (h >> 9) ^ (h >> 18)
	return hsvToRGB(uint8(x), 255, 255)
}

// hsvToRGB: 8-bit hue wheel (0=red, 85=green, 170=blue), classic 6-sector
// conversion, integer only (this runs on a Cortex-M0+ with no FPU).
func hsvToRGB(h, s, v uint8) Color {
	region := uint16(h) * 6 >> 8
	f := uint16(h) * 6 & 0xff
	p := uint16(v) * (255 - uint16(s)) >> 8
	q := uint16(v) * (255 - uint16(s)*f>>8) >> 8
	t := uint16(v) * (255 - uint16(s)*(255-f)>>8) >> 8
	switch region {
	case 0:
		return NewColor(v, uint8(t), uint8(p))
	case 1:
		return NewColor(uint8(q), v, uint8(p))
	case 2:
		return NewColor(uint8(p), v, uint8(t))
	case 3:
		return NewColor(uint8(p), uint8(q), v)
	case 4:
		return NewColor(uint8(t), uint8(p), v)
	default:
		return NewColor(v, uint8(p), uint8(q))
	}
}
