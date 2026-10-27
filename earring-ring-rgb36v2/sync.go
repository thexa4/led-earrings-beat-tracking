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
	"device/arm"
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

	// State for the pulse variant (sync variant 1), updated in syncPoll once
	// per analysis frame and read per LED by syncAnimate.
	syncNoiseT int // noise time: advances faster the louder the mids
	syncBright int // 0..256: the beat's decay or the bass level, whichever is up

	// State for the waves variant (sync variant 0): per band - bass, mid,
	// high, drawn red, green, blue - its average and its waves, updated in
	// syncPoll once per analysis frame, and the picture they make, drawn
	// there too (while the variant is shown) and read per LED by syncAnimate.
	syncBands    [3]syncBand
	syncWaves    [len(syncBands) * syncWaveSlots]syncWave // band i's from i*syncWaveSlots
	syncWaveRing [numLEDs]Color

	// Uploaded programs in sync mode (variants 3 and 4, see syncNextFrame).
	syncSlot        int // custom slot loaded into animationBuf, plus one (0: none)
	syncCustomFrame int // syncAnimFrame/2 at the program's last nextFrame

	syncMainFrame int // syncAnimFrame/2 at the main loop's last frame (see main)

	syncRunning bool  // capture is up (also: stopSync is idempotent)
	syncRead    int   // next sample to consume from adcData
	syncFed     int32 // samples fed into the current analysis frame
)

// The waves variant: a band rising above its own slow average spawns a wave
// at the bar position, which runs both ways around the ring and fades.
const (
	syncWaveSlots = 4  // waves per band; a new one replaces the oldest
	syncWaveLife  = 32 // frames a wave lasts (0.5 s), spreading half an LED per frame
	syncWaveGap   = 8  // minimum frames between two waves of one band
	syncWaveAvg   = 7  // the average's EMA shift: ~2 s at 64 frames/s
)

type syncWave struct {
	birth       int32 // syncAnimFrame it spawned at
	origin, amp uint8
}

type syncBand struct {
	avg  int32 // the band's level, averaged, Q8 (an integer EMA at this shift sticks)
	last int32 // syncAnimFrame of the band's last wave
	up   bool  // the level was above avg last frame
	next uint8 // slot of the band's next wave, 0..syncWaveSlots-1
}

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
	// Waves from an earlier run would have births ahead of the restarted
	// syncAnimFrame: ghosts once it caught up. (The bands' last spawns are
	// ahead too: syncSpawnWave's unsigned gap test lets them through.)
	syncWaves = [len(syncWaves)]syncWave{}
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
		// Emits nothing, but counts as a call: ARM's unroller leaves such
		// loops alone (unrolled 4x, this loop took 24 bytes more).
		arm.Asm("")
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
	avail := write - syncRead
	if avail < 0 {
		avail += syncWindowSize
	}
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
		// Patterns are 0..NumPatterns-1, all below numNoisePatterns (checked
		// below), so no modulo: a signed one pulled in a 60 B library call.
		syncVariant = int(ev.Pattern)
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
		sh.beatMs = uint16(uint(per) * 125 >> 3) // 64 frames/s: 15.625 ms per frame
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

	// Waves variant: each band rising above its average spawns a wave.
	syncSpawnWave(0)
	syncSpawnWave(1)
	syncSpawnWave(2)
	if modeVariants[modeSync] == 0 {
		// The waves move once per frame, so they are drawn once per frame,
		// and only while shown (the saved variant is the shown one in sync
		// mode): drawing them per LED, in the pass that lights it, made the
		// passes 20% longer than pulse's, and the LEDs as much dimmer.
		syncDrawWaves()
	}
}

// syncSpawnWave updates band i's average with its level, and when the level
// rises above it spawns a wave at the bar position: beat 1 at the top (LED 0),
// a quarter turn clockwise per beat. The wave is as bright as the level,
// which rises at once (LevelRelease), so the rise is the hit's peak or near
// it. Out of line: once for all three bands, and syncPoll has no registers to
// spare.
//
// The bar is the beat tracker's: sh.beats counts the same beats as its
// counter, both from 0 when sync starts, and it puts a bar on every fourth.
// Before lock the onsets standing in for beats turn it.
//
//go:noinline
func syncSpawnWave(i int) {
	sh := syncShare
	b := &syncBands[i]
	x := (*[3]uint8)(unsafe.Pointer(&sh.bass))[i] // bass, mid, high: adjacent in the block
	x8 := int32(x) << 8
	up := x8 > b.avg
	b.avg += (x8 - b.avg) >> syncWaveAvg
	if up && !b.up && uint(syncAnimFrame-int(b.last)) >= syncWaveGap {
		b.last = int32(syncAnimFrame)
		syncWaves[i*syncWaveSlots+int(b.next)] = syncWave{
			birth:  b.last,
			origin: uint8(((sh.beats&3)<<8 + uint32(sh.beatPhase)) * numLEDs >> 10),
			amp:    x,
		}
		b.next = (b.next + 1) % syncWaveSlots
	}
	b.up = up
}

// syncDrawWaves draws the waves variant into syncWaveRing. The waves of a
// band add up, to at most 255.
//
//go:noinline
func syncDrawWaves() {
	syncWaveRing = [numLEDs]Color{}
	ring := (*[numLEDs][4]uint8)(unsafe.Pointer(&syncWaveRing)) // red, green, blue
	for i := range syncWaves {
		for led := range numLEDs {
			// A call in the loop: ARM's unroller leaves such loops alone.
			p := &ring[led][i/syncWaveSlots]
			*p = uint8(min(int(*p)+syncWaveLight(&syncWaves[i], led), 255))
		}
	}
}

// syncWaveLight is how much wave w lights LED led: its fronts are two LEDs
// wide, at half an LED per frame from the origin both ways around the ring,
// fading out over syncWaveLife.
//
//go:noinline
func syncWaveLight(w *syncWave, led int) int {
	age := syncAnimFrame - int(w.birth)
	if uint(age) >= syncWaveLife {
		return 0
	}
	d := led - int(w.origin)
	if d < 0 {
		d = -d
	}
	if d > numLEDs/2 {
		d = numLEDs - d
	}
	e := 2*d - age // distance from the front, in half LEDs
	if e < 0 {
		e = -e
	}
	if e >= 4 {
		return 0
	}
	return int(w.amp) * (syncWaveLife - age) * (4 - e) >> 7
}

var _ [numNoisePatterns - syncdsp.NumPatterns]struct{} // every pattern has a palette

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
//	0  waves: red, green and blue waves for the bass, mids and highs, each
//	   spawned by its band rising above its average, at a point going
//	   around the ring once per bar (syncDrawWaves)
//	1  pulse: the music-chosen palette, brightness on the beat and the bass,
//	   flow speed from the mids, glitter from the highs
//	2  classic: the music-chosen palette with a flash on each beat
//	3  the uploaded program in custom slot 0, with the sync data block live
//	4  the same for slot 1
//	5  interference pulse (patterns.go)
//	6  synthwave (patterns.go)
//
// A long press on the variant button swaps red and blue in all of them
// (see animate).
//
// Until flash ran out there was also a hash readout, an instrument for
// checking earrings against each other (section hash as a color, a dot
// chasing the beat); it is an uploadable program now, in SYNC-PROGRAMS.md.
func syncAnimate(led, variant int) Color {
	if uint(variant-3) < 2 {
		return showCustom(led, syncMillis(), variant-3)
	}

	// The pipeline picks one of NumPatterns patterns from the spectrum (a
	// pure function, identical on every device); the noise palettes give
	// them visibly different looks. Waves and pulse are tested first and the
	// patterns last: these tests run for every LED, in the pass that lights
	// them.
	var c Color
	if variant == 0 {
		c = syncWaveRing[led]
	} else if variant == 1 {
		c = noise(led, syncNoiseT, syncVariant).Scale(syncBright)

		// Glitter: up to 1 in 8 LEDs, more the louder the highs, each held
		// for two frames (one frame, 16 ms, reads as flicker rather than as a
		// spark). Not synchronized between earrings, and need not be.
		x := (uint32(led)*0x9e3779b1 ^ uint32(syncAnimFrame>>1)) * 0x85ebca6b
		if h := syncShare.high; uint8(x>>24) < h>>3 {
			c = NewColor(h, h, h)
		}
	} else if variant == 2 {
		c = noise(led, syncAnimFrame, syncVariant)
		switch {
		case !syncLocked:
			// Still finding the tempo: dim, so a pair of earrings visibly
			// settles when both lock.
			c = c.Scale(80)
		case syncBeatAge == 0:
			// Beat: flash toward white for one analysis frame (~16 ms).
			c = NewColor(
				uint8((uint(c.R())+255)>>1),
				uint8((uint(c.G())+255)>>1),
				uint8((uint(c.B())+255)>>1))
		}
	} else {
		c = syncPattern(led, variant)
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

// syncNextFrame runs once per main-loop frame in sync mode. For variants 3
// and 4 it loads the uploaded program from that custom slot (on entering the
// variant, with the same one-second grace as the custom modes) and calls its
// nextFrame every other analysis frame: 32 times a second, near the ~27 of
// the custom modes, so programs that count frames run at about their usual
// speed. The program runs with the variant it last had in its custom mode.
func syncNextFrame(variant int) {
	slot := variant - 3
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
