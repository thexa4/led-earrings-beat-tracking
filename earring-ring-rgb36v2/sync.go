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

	// State for the pulse variant (sync variant 0), updated in syncPoll once
	// per analysis frame and read per LED by syncAnimate.
	syncNoiseT int // noise time: advances faster the louder the mids
	syncBright int // 0..256: the beat's decay or the bass level, whichever is up

	// Uploaded programs in sync mode (variants 2 and 3, see syncNextFrame).
	syncSlot        int // custom slot loaded into animationBuf, plus one (0: none)
	syncCustomFrame int // syncAnimFrame/2 at the program's last nextFrame

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
	syncNoiseT = 0
	syncSlot = 0
	syncShareReset(syncShareActive)
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
	syncShare.flags = 0

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
	sh := syncShare
	if ev.Beat {
		syncBeatAge = 0
		sh.beats++
	}
	if ev.Anchor {
		syncAnchorAge = 0
		sh.sections++
		sh.hash = ev.Hash
	}
	if ev.PatternChanged {
		// Map pattern -> palette here rather than per LED in syncAnimate:
		// animate runs 36 times per frame, patterns change once per bar.
		syncVariant = int(ev.Pattern) % numNoisePatterns
	}
	syncLocked = ev.Locked
	syncSilent = ev.Silent

	// Beat phase, 0..256 through the current beat, interpolated with the
	// tracker's own period. One division per frame on a machine that does 64
	// frames/s with ~2k cycles of boundary work - noise.
	per := syncPipe.BeatPeriod()
	if per < 1 {
		per = 26 // before the first lock: keep the phase well-defined
	}
	age := min(syncBeatAge, per)
	phase := 256 * uint32(age) / uint32(per)

	// The sync data block for uploaded programs (syncshare.go). Levels are
	// stored band 0 = top in the pipeline, lowest first in the block.
	lv := &syncPipe.Level
	for i := range sh.bands {
		sh.bands[i] = lv[len(sh.bands)-1-i]
	}
	sh.level = lv[syncdsp.NBands]
	sh.bass = lv[7]
	sh.mid = uint8((uint32(lv[4]) + uint32(lv[5]) + uint32(lv[6])) * 85 >> 8)
	sh.high = uint8((uint32(lv[0]) + uint32(lv[1]) + uint32(lv[2])) * 85 >> 8)
	sh.frame = uint32(syncAnimFrame)
	sh.beatPhase = uint8(min(phase, 255))
	sh.beatAge = uint8(min(syncBeatAge, 255))
	sh.sectionAge = uint8(min(syncAnchorAge, 255))
	sh.pattern = uint8(syncVariant)
	sh.palette = uintptr(unsafe.Pointer(unsafe.StringData(noisePalettes))) + uintptr(syncVariant*48)
	flags := uint8(syncShareActive)
	sh.beatMs = 0
	if syncLocked {
		flags |= syncShareLocked
		sh.beatMs = uint16(per * 125 / 8) // 64 frames/s: 15.625 ms per frame
	}
	if syncSilent {
		flags |= syncShareSilent
	}
	sh.flags = flags

	// Pulse variant: the palette flows faster with the mids, and the ring's
	// brightness follows the beat (a squared decay: a snap on the beat, dark
	// well before the next) or the bass level, whichever is higher, so music
	// without a steady kick still breathes.
	syncNoiseT += 1 + int(sh.mid>>6)
	k := int(256 - phase)
	syncBright = 56 + max(k*k>>8, int(sh.bass))*200>>8
}

// syncAnimate draws one LED for party sync mode.
//
// It deliberately does not take the main loop's frame counter: at the sync
// clock rate that counter advances 4x faster than the animations were tuned
// for. syncAnimFrame advances once per analysis frame (64 Hz), which is both
// a good animation rate and the rate every other earring is animating at, so
// the same pattern on two earrings is frame-locked as well as music-locked.
//
// Variants:
//
//	0  pulse: the music-chosen palette, brightness on the beat and the bass,
//	   flow speed from the mids, glitter from the highs
//	1  classic: the music-chosen palette with a flash on each beat
//	2  the uploaded program in custom slot 0, with the sync data block live
//	3  the same for slot 1
//
// Until flash ran out there was also a hash readout, an instrument for
// checking earrings against each other (section hash as a color, a dot
// chasing the beat); it is an uploadable program now, in SYNC-PROGRAMS.md.
func syncAnimate(led, variant int) Color {
	if variant >= 2 {
		return showCustom(led, syncMillis(), variant-2)
	}

	// The pipeline picks one of NumPatterns patterns from the spectrum (a
	// pure function, identical on every device); the noise palettes give
	// them visibly different looks.
	var c Color
	if variant == 0 {
		c = noise(led, syncNoiseT, syncVariant).Scale(syncBright)

		// Glitter: up to 1 in 8 LEDs, more the louder the highs, each held
		// for two frames (one frame, 16 ms, reads as flicker rather than as a
		// spark). Not synchronized between earrings, and need not be.
		x := (uint32(led)*0x9e3779b1 ^ uint32(syncAnimFrame>>1)) * 0x85ebca6b
		if h := syncShare.high; uint8(x>>24) < h>>3 {
			c = NewColor(h, h, h)
		}
	} else {
		c = noise(led, syncAnimFrame, syncVariant)
		switch {
		case !syncLocked:
			// Still finding the tempo: dim, so a pair of earrings visibly
			// settles when both lock.
			c = c.Scale(80)
		case syncBeatAge == 0:
			// Beat: flash toward white for one analysis frame (~16 ms).
			c = NewColor(
				uint8(int(c.R())+(255-int(c.R()))/2),
				uint8(int(c.G())+(255-int(c.G()))/2),
				uint8(int(c.B())+(255-int(c.B()))/2))
		}
	}

	if syncAnchorAge <= 1 {
		// Transition anchor: a white flash. This is the moment the earrings
		// are meant to snap together, so make it obvious.
		c = NewColor(255, 255, 255)
	}

	if syncSilent {
		// Nobody playing music: nearly dark, but not fully off so the
		// mode doesn't look broken.
		c = c.Scale(40)
	}

	return c
}

// syncMillis is the time base handed to uploaded programs in sync mode:
// analysis frames (exactly 15.625 ms at 16 kSPS) since sync started.
func syncMillis() int {
	return syncAnimFrame * 125 >> 3
}

// syncNextFrame runs once per main-loop frame in sync mode. For variants 2
// and 3 it loads the uploaded program from that custom slot (on entering the
// variant, with the same one-second grace as the custom modes) and calls its
// nextFrame every other analysis frame: 32 times a second, near the ~27 of
// the custom modes, so programs that count frames run at about their usual
// speed. The program runs with the variant it last had in its custom mode.
func syncNextFrame(variant int) {
	slot := variant - 2
	if slot < 0 || slot > 1 {
		return
	}
	if syncSlot != slot+1 {
		syncSlot = slot + 1
		customLoadPattern(slot, true)
	}
	if f := syncAnimFrame >> 1; f != syncCustomFrame {
		syncCustomFrame = f
		customNextFrame(syncMillis(), int(modeVariants[modeCustom0+slot]))
	}
}

// syncShareReset clears the sync data block and sets its header; flags is
// syncShareActive from startSync, 0 at boot.
//
//go:noinline
func syncShareReset(flags uint8) {
	*syncShare = syncShareT{
		magic:   syncShareMagic,
		version: syncShareVersion,
		flags:   flags,
	}
}
