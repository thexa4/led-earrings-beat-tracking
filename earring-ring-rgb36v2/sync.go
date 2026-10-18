package main

// Party sync mode: multiple earrings listening to the same music derive the
// same pattern decisions from it (no radio). The DSP lives in the syncdsp
// package (host-tested); this file is the device plumbing: capture setup,
// non-blocking sample feeding, and the animation state.
//
// Clock/ADC setup is deliberately a near-copy of the capture half of
// dataRecv() (see startSyncCapture): that configuration is proven to sample
// accurately at 4.2 MHz with the ADC on HSI16 and 32x oversampling. The only
// differences: LEDs stay on (this mode is the LEDs), the DMA buffer is one
// 250-sample analysis window instead of two symbol windows, and the timer
// prescaler gives 15,997 Hz to match syncdsp.SampleRate.
//
// Running at 4.2 MHz has two consequences the loop structure must respect:
//
//   - The bit-bang multiplexer is cycle-counted, so a full refresh gets ~16x
//     faster. That is harmless for the LEDs (same duty, just higher rate),
//     but animations driven by the main loop's frame counter would run 16x
//     too fast too. So sync mode ignores that counter: syncAnimate reads
//     syncAnimFrame, which advances once per analysis frame (16 Hz) instead.
//
//   - A full pipeline frame takes ~20k cycles (~5 ms). Running that in one
//     burst would freeze the multiplexer that long, holding one LED group on
//     visibly. So samples are fed incrementally: each main-loop iteration
//     hands the pipeline whatever the DMA has written since the last
//     iteration (bounded burst), and only the frame *boundary* work
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

	// TIM22 prescaler: fs = 4.194304 MHz / (2 * (syncPSC+1)) = 15996.6 Hz,
	// matching syncdsp.SampleRate to within 0.02%.
	syncPSC = 130

	// Maximum samples fed to the pipeline per main-loop iteration. At the
	// ~4.2 MHz loop rate iterations arrive far more often than samples do
	// (~6 needed, ~4.2 MHz / 1.5k cycles per iteration), so this cap only
	// limits catch-up after a stall; 24 samples ≈ 1.6k cycles ≈ one refresh
	// period, which is invisible.
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

	// Enable HSI16: the ADC runs from it (CKMODE=0, the reset value, selects
	// HSI16 - the same configuration dataRecv() uses, with the same reason:
	// 32x oversampling needs a fast ADC clock independent of the system clock).
	stm32.RCC.CR.SetBits(stm32.RCC_CR_HSI16ON)

	// System clock: MSI range 6 (4.2 MHz), like dataRecv(). This is what
	// makes TIM22 (PCLK domain) count fast enough for 16 kSPS and gives the
	// DSP its cycle budget.
	stm32.RCC.SetICSCR_MSIRANGE(stm32.RCC_ICSCR_MSIRANGE_Range6)

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
	// every other PCLK: fs = 4.194 MHz / (2 * (PSC+1)).
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
	// free, and each trigger's 32 sub-conversions take ~28 µs of HSI16 -
	// well inside the 62.5 µs sample period.
	stm32.ADC.CFGR1.Set(stm32.ADC_CFGR1_DMAEN | stm32.ADC_CFGR1_DMACFG |
		stm32.ADC_CFGR1_EXTSEL_TIM22_TRGO<<stm32.ADC_CFGR1_EXTSEL_Pos |
		stm32.ADC_CFGR1_EXTEN_RisingEdge<<stm32.ADC_CFGR1_EXTEN_Pos)
	// Full Set (not SetBits) with CKMODE left at 0: a preceding mic mode
	// configures the ADC on PCLK with low-frequency mode, and that would
	// both starve the 32x oversampling and mis-clock the conversions.
	// Clear CCR the same way (LFMEN off at 4.2 MHz system clock).
	stm32.ADC.CFGR2.Set(0 |
		stm32.ADC_CFGR2_OVSE |
		1<<stm32.ADC_CFGR2_OVSS_Pos |
		stm32.ADC_CFGR2_OVSR_Mul32<<stm32.ADC_CFGR2_OVSR_Pos)
	stm32.ADC.CCR.Set(0)

	stm32.ADC.CR.Set(stm32.ADC_CR_ADEN | stm32.ADC_CR_ADSTART)
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

	// Disable HSI16 (nothing else uses it; dataRecv does the same on exit).
	stm32.RCC.CR.ClearBits(stm32.RCC_CR_HSI16ON)

	// Back to the animation clock.
	stm32.RCC.SetICSCR_MSIRANGE(defaultMSIRANGE)
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

	// Samples available without lapping the writer. The producer adds ~6 per
	// iteration and never runs ahead of the buffer, so this difference is
	// always the true backlog; syncFeedCap bounds the burst.
	avail := (write - syncRead + syncWindowSize) % syncWindowSize
	if avail > syncFeedCap {
		avail = syncFeedCap
	}

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
