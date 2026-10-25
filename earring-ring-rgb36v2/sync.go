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
// trigger dropped and a "15.6 ms" analysis frame is really 28.4 ms - which
// is fine in the field (master, every device agrees), but it makes the frame
// constants in syncdsp 1.82x off their documented wall time. At 16 MHz the
// 62.5 us sample period is real: TIM22 divides down to exactly 16 kHz and
// the ADC runs synchronously from PCLK. (16 MHz needs voltage range 1, which
// costs battery current - accepted, and master is intended to move to this
// clock too; frame-unit constants in syncdsp are being retuned for the true
// 64 fps frame rate on this branch.)
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
//   - A full pipeline frame takes ~20k cycles (~1.3 ms at 16 MHz). Running
//     that in one burst would freeze the multiplexer that long, holding one
//     LED group on visibly. So samples are fed incrementally: each main-loop
//     iteration hands the pipeline whatever the DMA has written since the
//     last iteration (bounded burst), and only the frame *boundary* work
//     (log2/hash/beat/decide, ~2k cycles) runs at once.

import (
	"device/stm32"
	"machine"
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

	// Maximum samples fed to the pipeline per main-loop iteration. At 16 MHz
	// and 16 kSPS the loop needs ~16000/loop-rate ≈ 11 samples per iteration,
	// so this cap only limits catch-up bursts (24 samples ≈ 1.6k cycles ≈ one
	// refresh period, which is invisible). At 4.2 MHz it *was* the steady
	// state (24 × ~370 Hz ≈ the 8.8 kSPS the starved CPU managed).
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
	syncRead = 0
	syncFed = 0
	syncRunning = true

	// System clock: HSI16 (exactly 16 MHz), so PCLK is 16 MHz for TIM22 and
	// the ADC (which runs synchronously from PCLK via CKMODE=0; note that
	// 0 does *not* select HSI16 async as the old comment here believed - the
	// measured 8.8 kSPS at MSI Range6 proves it).
	//
	// 16 MHz needs voltage range 1 (the idle config runs range 3, whose spec
	// maximum of 2 MHz sync at 4.2 MHz was already quietly over). Order: raise
	// the scale and wait for the regulator (VOSF clears when ready), give the
	// flash its wait state, then switch over. Power is the bench supply here;
	// this trade is not for battery firmware.
	// TinyGo's device/stm32 masks for this part are already shifted into
	// place (HSI16ON_Msk=0x1 at Pos 0, HSI16RDYF_Msk=0x4 at Pos 2): use them
	// directly - shifting again by _Pos points at the wrong bit.
	stm32.RCC.CR.SetBits(stm32.RCC_CR_HSI16ON_Msk)
	for stm32.RCC.CR.Get()&stm32.RCC_CR_HSI16RDYF_Msk == 0 {
	}
	stm32.RCC.APB1ENR.SetBits(stm32.RCC_APB1ENR_PWREN_Msk)
	stm32.PWR.CR.ReplaceBits(stm32.PWR_CR_VOS_V1_8, stm32.PWR_CR_VOS_Msk,
		stm32.PWR_CR_VOS_Pos)
	waitForVOS()
	stm32.RCC.APB1ENR.ClearBits(stm32.RCC_APB1ENR_PWREN_Msk)
	stm32.FLASH.SetACR_LATENCY(stm32.Flash_ACR_LATENCY_WS1)
	stm32.RCC.CFGR.ReplaceBits(stm32.RCC_CFGR_SW_HSI16, stm32.RCC_CFGR_SW_Msk, 0)
	// SWS is the SW field at bits 3:2: shift the constant, or the comparison
	// against the masked register is never true - LLVM proves that and marks
	// everything after the loop unreachable, silently dropping the rest of
	// the program (measured: a 30 KB firmware links to 4 KB, no warning).
	// If this loop ever appears to hang, suspect the SWD read path first:
	// CFGR has been observed to read back 0 over the debug probe while the
	// core demonstrably ran from HSI16 at the full 16 MHz.
	for stm32.RCC.CFGR.Get()&stm32.RCC_CFGR_SWS_Msk != (stm32.RCC_CFGR_SWS_HSI16 << 2) {
	}

	// Power on microphone.
	micPin.Configure(machine.PinConfig{Mode: machine.PinOutput})
	micPin.High()

	// ADC clock: PCLK2 undivided (see dataRecv for why).
	stm32.RCC.SetCFGR_PPRE2(stm32.RCC_CFGR_PPRE2_Div1)
	stm32.RCC.SetAPB2ENR_ADCEN(1)

	// Power on ADC.
	stm32.ADC.ISR.Set(stm32.ADC_ISR_ADRDY)             // clear ADRDY bit
	stm32.ADC.CR.Set(stm32.ADC_CR_ADEN)                // enable ADC
	for stm32.ADC.ISR.Get()&stm32.ADC_ISR_ADRDY == 0 { // wait until ready
	}

	// Mic on channel 8 (PB0).
	stm32.ADC.CHSELR.Set(1 << 8)

	// TIM22 triggers the ADC at fs; ARR=1 so the update event (TRGO) happens
	// every other PCLK: fs = 16 MHz / (2 * (PSC+1)).
	stm32.RCC.SetAPB2ENR_TIM22EN(1)
	stm32.TIM22.PSC.Set(syncPSC)
	stm32.TIM22.ARR.Set(1)
	stm32.TIM22.CR1.Set(stm32.TIM_CR1_CEN)
	stm32.TIM22.CR2.Set(stm32.TIM_CR2_MMS_Update << stm32.TIM_CR2_MMS_Pos)

	// DMA: one circular 250-sample window. adcData is 400 samples long
	// (dataRecv's double symbol buffer); sync mode uses the first 250 of it,
	// reusing existing RAM rather than declaring a second buffer.
	stm32.RCC.AHBENR.SetBits(stm32.RCC_AHBENR_DMAEN)
	stm32.DMA1.CH[0].PAR.Set(uint32(uintptr(unsafe.Pointer(&stm32.ADC.DR))))
	stm32.DMA1.CH[0].MAR.Set(uint32(uintptr(unsafe.Pointer(&adcData[0]))))
	stm32.DMA1.CH[0].NDTR.Set(syncWindowSize)
	stm32.DMA1.CH[0].CR.Set(stm32.DMA_CH_CR_MINC |
		stm32.DMA_CH_CR_MSIZE_Bits16<<stm32.DMA_CH_CR_MSIZE_Pos |
		stm32.DMA_CH_CR_PSIZE_Bits16<<stm32.DMA_CH_CR_PSIZE_Pos |
		stm32.DMA_CH_CR_CIRC)
	stm32.DMA1.CH[0].CR.SetBits(stm32.DMA_CH_CR_EN)

	// ADC: continuous DMA, one transfer per TIM22 trigger, 32x oversampling
	// with a 1-bit shift (same as dataRecv): resolution + noise shaping for
	// free. Each trigger's sub-conversions take ~461 ADC cycles (measured),
	// which at the 8 MHz ADC clock below is ≈ 58 µs - inside the 62.5 µs
	// sample period, so no trigger is missed (at the old 4.19 MHz PCLK and
	// ADCPRE=1 they took ~110 µs, which is exactly where the 8.8 kSPS came
	// from).
	stm32.ADC.CFGR1.Set(stm32.ADC_CFGR1_DMAEN | stm32.ADC_CFGR1_DMACFG |
		stm32.ADC_CFGR1_EXTSEL_TIM22_TRGO<<stm32.ADC_CFGR1_EXTSEL_Pos |
		stm32.ADC_CFGR1_EXTEN_RisingEdge<<stm32.ADC_CFGR1_EXTEN_Pos)
	// Full Set (not SetBits) with CKMODE left at 0: a preceding mic mode
	// configures the ADC on PCLK with low-frequency mode, and that would
	// both starve the 32x oversampling and mis-clock the conversions.
	stm32.ADC.CFGR2.Set(0 |
		stm32.ADC_CFGR2_OVSE |
		1<<stm32.ADC_CFGR2_OVSS_Pos |
		stm32.ADC_CFGR2_OVSR_Mul32<<stm32.ADC_CFGR2_OVSR_Pos)
	// CCR is left alone, deliberately, and read back over SWD to confirm: on
	// this part the only writable bits are PRESC (21:18), VREFEN, TSEN, VLCDEN
	// and LFMEN (22..25). There is no ADCPRE - that field belongs to the F0/F3
	// family, and an earlier comment here believed bit 28 was it and wrote
	// 1<<28 "AHB/2". The write is silently dropped (CCR reads 0), which is also
	// why the sample rate never changed: the ADC clock comes from CFGR2.CKMODE,
	// and CKMODE=0 on the L0 is ADCLK = HSI16 asynchronous, 16 MHz here. So the
	// 32 sub-conversions take 32 x (1.5 + 10.5) = 384 ADC cycles = 24 us, well
	// inside the 62.5 us sample period, and HSI16 stays on anyway as the system
	// clock. LFMEN stays off: it is for an ADC clock below 3.5 MHz.
	stm32.ADC.CCR.Set(0)

	stm32.ADC.CR.Set(stm32.ADC_CR_ADEN | stm32.ADC_CR_ADSTART)
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
// forever (measured: CFGR never written, magic never written; LLVM also
// dropped the loop's iteration bound because the exit was data-dependent on
// a register it cannot prove changes). Bound the loop *and* wait for the
// settled level.
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

	// Stop DMA.
	stm32.DMA1.CH[0].CR.ClearBits(stm32.DMA_CH_CR_EN)
	stm32.RCC.AHBENR.ClearBits(stm32.RCC_AHBENR_DMAEN)

	// Shut down ADC (reset also clears its oversampling config for whoever
	// runs next).
	stm32.RCC.APB2RSTR.Set(stm32.RCC_APB2RSTR_ADCRST)
	stm32.RCC.APB2RSTR.Set(0)
	stm32.RCC.SetAPB2ENR_ADCEN(0)
	stm32.RCC.SetCFGR_PPRE2(stm32.RCC_CFGR_PPRE2_Div16)

	// Shut down timer.
	stm32.RCC.SetAPB2ENR_TIM22EN(0)

	// Power off microphone.
	micPin.Configure(machine.PinConfig{Mode: machine.PinInputAnalog})

	// Back to the animation clock: switch to MSI first (still Range2, the
	// idle speed - startSync no longer retunes MSI), then it is legal to drop
	// the voltage scale again; the flash latency can go either way but WS0
	// matches the idle config. Order mirrors the power-up half in reverse.
	stm32.RCC.CFGR.ReplaceBits(stm32.RCC_CFGR_SW_MSI, stm32.RCC_CFGR_SW_Msk, 0)
	// (MSI is field value 0, so the SWS mask/constant mismatch noted above
	// cannot bite here.)
	for stm32.RCC.CFGR.Get()&stm32.RCC_CFGR_SWS_Msk != stm32.RCC_CFGR_SWS_MSI {
	}
	stm32.FLASH.SetACR_LATENCY(stm32.Flash_ACR_LATENCY_WS0)
	stm32.RCC.APB1ENR.SetBits(stm32.RCC_APB1ENR_PWREN)
	stm32.PWR.CR.ReplaceBits(stm32.PWR_CR_VOS_V1_2, stm32.PWR_CR_VOS_Msk,
		stm32.PWR_CR_VOS_Pos)
	waitForVOS()
	stm32.RCC.APB1ENR.ClearBits(stm32.RCC_APB1ENR_PWREN)

	// Disable HSI16 (nothing else uses it; dataRecv does the same on exit).
	stm32.RCC.CR.ClearBits(stm32.RCC_CR_HSI16ON)
}

// syncPoll consumes everything the DMA has written since the last call and
// runs the pipeline boundary work when a frame completes. Called once per
// main-loop iteration (in place of updateMic).
//
//go:noinline
func syncPoll() {
	// DMA write position: NDTR counts *remaining* slots down, so the next
	// sample lands at 250-NDTR.
	write := int(syncWindowSize - stm32.DMA1.CH[0].NDTR.Get())
	if write >= syncWindowSize {
		write = 0 // NDTR==0: the last write was the final slot
	}

	// Samples available without lapping the writer. The producer adds ~11 per
	// iteration and never runs ahead of the buffer, so this difference is
	// always the true backlog; syncFeedCap bounds the burst.
	avail := (write - syncRead + syncWindowSize) % syncWindowSize
	if avail > syncFeedCap {
		avail = syncFeedCap
	}

	for range avail {
		sample := adcData[syncRead]
		syncPipe.ProcessSample(sample)
		if micDumpEnabled {
			micDumpAdd(sample)
		}
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
	if micDumpEnabled {
		micDumpUpdateGain() // reads the AGC peak that EndFrame just updated
	}

	syncAnimFrame++
	syncBeatAge++
	syncAnchorAge++
	if ev.Beat {
		syncBeatAge = 0
	}
	if ev.Anchor {
		syncAnchorAge = 0
	}
	if ev.PatternChanged {
		// Map pattern -> palette here rather than per LED in syncAnimate:
		// animate runs 36 times per frame, patterns change once per bar.
		syncVariant = int(ev.Pattern) % numNoisePatterns
	}
	syncLocked = ev.Locked
	syncSilent = ev.Silent
}

// syncAnimate draws one LED for party sync mode.
//
// It deliberately does not take the main loop's frame counter: at the sync
// clock rate that counter advances ~16x faster than the animations were tuned
// for. syncAnimFrame advances once per analysis frame (~16 Hz), which is both
// a good animation rate and the rate every other earring is animating at, so
// the same pattern on two earrings is frame-locked as well as music-locked.
func syncAnimate(led int) Color {
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
