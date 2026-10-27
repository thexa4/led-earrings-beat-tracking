package main

// Shared audio capture plumbing for the two microphone consumers: the IR/RF
// data receiver (datarecv.go) and party sync (sync.go). They never run at the
// same time, and their capture setup was near-identical register code twice
// over (~700 B duplicated); this file is the single copy.
//
// The parameters that actually differ between the two modes are the sample
// clock (TIM22 prescaler) and the ADC prescaler (CCR), both driven by the
// system clock the caller selects. The exact register order below is
// load-bearing in three places (ADRDY handshake before CHSELR, full Set() of
// CFGR2 to clear a mic-mode low-frequency config, and starting DMA before
// ADSTART); both modes must be re-verified on device after any change here.

import (
	"device/stm32"
	"machine"
	"unsafe"
)

// startCapture powers the microphone, brings up the ADC with 32x
// oversampling, and starts TIM22 + DMA circular capture of length samples
// into the 16-bit buffer at mar.
//
// The caller must have switched to the intended system clock already. TIM22
// runs from PCLK, so the sample rate is
//
//	fs = PCLK / (2 * (psc + 1))    (TIM22 ARR=1, TRGO on every update)
//
// CFGR2 below leaves CKMODE=0, which per RM0377 is the asynchronous clock
// (HSI16) divided by CCR.PRESC. ccr is written verbatim to ADC->CCR; the
// datasheet limit on the ADC clock is 16/8/4 MHz in voltage range 1/2/3. A
// 32x oversampled conversion takes 32 * (12.5 + 1.5) = 448 ADC cycles.
// Both callers run range 2 and pass PRESC=/2: 8 MHz, 56 us of dataRecv's
// 100 us and sync's 62.5 us period. Measured on sync's rate: /4 misses every
// other trigger, so the prescaler is HSI16's (the old 8.8 kSPS at MSI, once
// read as the ADC running from PCLK, was the CPU starving).
// The clock must be set before ADEN; see below.
//
// Both callers must share this one copy: -opt=2 re-inlines (and so
// re-duplicates) it into each of them otherwise.
//
//go:noinline
func startCapture(mar uint32, length int, psc uint32, ccr uint32) {
	// Power on microphone.
	micPin.Configure(machine.PinConfig{Mode: machine.PinOutput})
	micPin.High()

	// Provide clock to ADC.
	// Note that PCLK2 is divided when the mic is disabled to save power (~1µA),
	// but the ADC doesn't work with such a low frequency so we need to set the
	// division value back to 1 (no division).
	stm32.RCC.SetCFGR_PPRE2(stm32.RCC_CFGR_PPRE2_Div1)
	stm32.RCC.SetAPB2ENR_ADCEN(1)

	// The clock (CCR.PRESC, CFGR2.CKMODE) takes effect only if written while
	// the ADC is disabled: written after ADEN, PRESC reads back but the ADC
	// keeps its old clock (measured: sync at /256 still sampled 16 kSPS). A
	// reset first, as mic mode may have left the ADC on. Everything else in
	// CCR is zero, in particular LFMEN off.
	stm32.RCC.APB2RSTR.Set(stm32.RCC_APB2RSTR_ADCRST)
	stm32.RCC.APB2RSTR.Set(0)
	stm32.ADC.CCR.Set(ccr)

	// Power on ADC: clear ADRDY, enable, wait until ready.
	stm32.ADC.ISR.Set(stm32.ADC_ISR_ADRDY)
	stm32.ADC.CR.Set(stm32.ADC_CR_ADEN)
	for stm32.ADC.ISR.Get()&stm32.ADC_ISR_ADRDY == 0 {
	}

	// Microphone on channel 8 (PB0).
	stm32.ADC.CHSELR.Set(1 << 8)

	// Enable TIM22 as the sample trigger: prescale so it counts at twice the
	// sample rate, wraparound every other clock, TRGO on update.
	stm32.RCC.SetAPB2ENR_TIM22EN(1)
	stm32.TIM22.PSC.Set(psc)
	stm32.TIM22.ARR.Set(1)
	stm32.TIM22.CR1.Set(stm32.TIM_CR1_CEN)
	stm32.TIM22.CR2.Set(stm32.TIM_CR2_MMS_Update << stm32.TIM_CR2_MMS_Pos)

	// DMA: circular capture, 16-bit on both sides, into the caller's buffer.
	stm32.RCC.AHBENR.SetBits(stm32.RCC_AHBENR_DMAEN)
	stm32.DMA1.CH[0].PAR.Set(uint32(uintptr(unsafe.Pointer(&stm32.ADC.DR))))
	stm32.DMA1.CH[0].MAR.Set(mar)
	stm32.DMA1.CH[0].NDTR.Set(uint32(length))
	stm32.DMA1.CH[0].CR.Set(stm32.DMA_CH_CR_MINC |
		stm32.DMA_CH_CR_MSIZE_Bits16<<stm32.DMA_CH_CR_MSIZE_Pos |
		stm32.DMA_CH_CR_PSIZE_Bits16<<stm32.DMA_CH_CR_PSIZE_Pos |
		stm32.DMA_CH_CR_TEIE | stm32.DMA_CH_CR_CIRC)
	stm32.DMA1.CH[0].CR.SetBits(stm32.DMA_CH_CR_EN)

	// ADC: continuous DMA mode, one transfer per TIM22 trigger, 32x
	// oversampling with a 1-bit shift (resolution + noise shaping for free).
	stm32.ADC.CFGR1.Set(stm32.ADC_CFGR1_DMAEN | stm32.ADC_CFGR1_DMACFG |
		stm32.ADC_CFGR1_EXTSEL_TIM22_TRGO<<stm32.ADC_CFGR1_EXTSEL_Pos |
		stm32.ADC_CFGR1_EXTEN_RisingEdge<<stm32.ADC_CFGR1_EXTEN_Pos)
	// Full Set (not SetBits): a preceding mic mode configures the ADC on
	// PCLK with low-frequency mode, and that would both starve the 32x
	// oversampling and mis-clock the conversions.
	stm32.ADC.CFGR2.Set(0 |
		stm32.ADC_CFGR2_OVSE |
		1<<stm32.ADC_CFGR2_OVSS_Pos |
		stm32.ADC_CFGR2_OVSR_Mul32<<stm32.ADC_CFGR2_OVSR_Pos)

	// Start the ADC.
	stm32.ADC.CR.Set(stm32.ADC_CR_ADEN | stm32.ADC_CR_ADSTART)
}

// stopCapture shuts down everything startCapture turned on and restores the
// low-power state around the ADC (PCLK2 divided again). The caller's own
// system-clock teardown (MSI switch-back, voltage scale, HSI16) is not part
// of this - sync and the receiver tear their clocks down differently.
//
// Deliberately NOT //go:noinline, unlike startCapture: this sequence is
// short and all-constant, and measuring the two forms showed LLVM inlining
// it into both callers is ~80 B *cheaper* than one shared copy (an outlined
// version cannot fold the bit patterns into immediates on M0+). Source
// dedup is kept, the compiler picks the cheaper codegen.
func stopCapture() {
	// Stop DMA.
	stm32.DMA1.CH[0].CR.ClearBits(stm32.DMA_CH_CR_EN)
	stm32.RCC.AHBENR.ClearBits(stm32.RCC_AHBENR_DMAEN)

	// Shut down ADC. The reset also clears its oversampling config for
	// whoever runs next.
	stm32.RCC.APB2RSTR.Set(stm32.RCC_APB2RSTR_ADCRST)
	stm32.RCC.APB2RSTR.Set(0)
	stm32.RCC.SetAPB2ENR_ADCEN(0)
	stm32.RCC.SetCFGR_PPRE2(stm32.RCC_CFGR_PPRE2_Div16)

	// Shut down timer.
	stm32.RCC.SetAPB2ENR_TIM22EN(0)

	// Power off the microphone.
	micPin.Configure(machine.PinConfig{Mode: machine.PinInputAnalog})
}
