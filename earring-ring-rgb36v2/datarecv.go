package main

import (
	"device/stm32"
	"machine"
	"unsafe"

	"codeberg.org/maaike328p/datatrans"
	"codeberg.org/maaike328p/datatrans/dsp"
)

const (
	// possible values for 4.2MHz: 12000 14000 15000 25000 28000 30000 35000 42000 50000 etc
	// possible values for 4.0MHz: 10000 16000 20000 25000 40000 50000
	// possible values for 8.0MHz: 10000 16000 20000 25000 32000 40000 50000
	// but since we need calibration anyway, really any value would be fine
	sampleRate = 10000

	// number of samples per window we do frequency analysis on
	windowSize = sampleRate / datatrans.SymbolsPerSecond

	systemClock    = 4.2e6
	clockPrescaler = uint32(systemClock/sampleRate/2 - 1)
)

// Size of a program slot in flash memory.
const slotSize = 1536 // 1.5kB

// Maximum size of a program (smaller than slotSize because we also need to save
// the program size).
const maxProgramSize = slotSize - 4

// Size (in bytes) of the buffer used during reception.
// Should be bigger than the slot, so that the fountain code has some space to
// work with.
const animationBufSize = maxProgramSize + 1024

var animationBuf [animationBufSize / 4]uint32

//go:align 4
var adcData [windowSize * 2]uint16

// ADC data with DC offset removed.
// Two extra elements: the last one to serve as a special "stop" value and the
// one after that because the Goertzel pair code reads two values at once.
//
// To save RAM this is overlaid on the tail of animationBuf, which is free
// during a data receive: the fountain decoder reconstructs the program into
// animationBuf[1:] up to maxProgramSize bytes (byte 1536), while this overlay
// starts at byte 4*(animationBufSize/4 - (windowSize+2)) = 1748, so the two
// never overlap (checked by the array assertion below). The Goertzel and copy
// code receive the buffer as an ordinary slice argument, so unlike an overlay
// of the sync pipeline (which is fed from the same loop that reads the DMA
// buffer) the aliasing cast costs the hot loops nothing. receiveData sets
// both stop-sentinel words explicitly because, unlike the old bss array, the
// overlay does not come pre-zeroed.
const adcDataNormWords = windowSize + 2

var _ [len(animationBuf) - maxProgramSize/4 - adcDataNormWords]int8 // disjoint

var adcDataNormalized = unsafe.Slice((*int32)(unsafe.Pointer(&animationBuf[len(animationBuf)-adcDataNormWords])), adcDataNormWords)

func adcDataWriteIndex() uint32 {
	return uint32(len(adcData)) - stm32.DMA1.CH[0].NDTR.Get()
}

// The receive decoder lives *inside* the sync pipeline (see main.syncPipe):
// the two engines are never live at the same time - dataRecv runs the main
// loop with sync stopped (main.go stops sync on every mode switch, and sync
// mode has no path into dataRecv), and both re-init their state on entry
// (startSync -> Pipeline.Init, dataRecv -> Decoder.Initialize), so neither
// ever reads the other's leftovers. syncPipe is the larger of the two, and
// both are plain integer structs, so the cast is safe.
//
// This overlay saves 592 B of RAM. Unlike the rejected animationBuf overlay
// (which aliased the DMA buffer and cost the feed loop its register
// allocation), neither loop reads the other's storage here: the receive
// loop consumes adcDataNormalized, and sync never runs during a receive.
var decoder = (*datatrans.Decoder)(unsafe.Pointer(&syncPipe))

var _ [unsafe.Sizeof(syncPipe) - unsafe.Sizeof(datatrans.Decoder{})]byte

var adcWriteWindow uint8 = 0 // 0 or 1 to indicate current window

var dcOffset uint32

//go:noinline
func adcDelaySamples(samples uint32) {
	if samples != 0 {
		stm32.DMA1.CH[0].CR.ClearBits(stm32.DMA_CH_CR_EN)
		stm32.DMA1.CH[0].CR.Get()
		for range samples {
			waitSamplePeriod()
		}

		stm32.DMA1.CH[0].CR.SetBits(stm32.DMA_CH_CR_EN)

		// This apparently helps to avoid a stuck DMA??
		// Though it doesn't seem to be needed anymore.
		//if stm32.DMA1.CH[0].CR.Get()&stm32.DMA_CH_CR_EN == 0 {
		//	for {
		//	}
		//}
	}
}

// dmaInWindow reports 1 while the DMA is writing into window w (0 or 1) of
// adcData. Out of line for the same reason as waitSamplePeriod.
//
//go:noinline
func dmaInWindow(w uint32) uint32 {
	if adcDataWriteIndex() >= windowSize {
		return w
	}
	return w ^ 1
}

// waitSamplePeriod waits for the sample timer to wrap once. Kept out of line:
// inlined, LLVM unrolls both spin loops and the loop around them four ways each.
//
//go:noinline
func waitSamplePeriod() {
	for stm32.TIM22.CNT.Get() != 0 {
	}
	for stm32.TIM22.CNT.Get() == 0 {
	}
}

// Wait until the write pointer is in the next window, and update
// adcDataNormalized so it contains the data for the window that's ready.
//
//go:noinline
func adcDataNextWindow(delayBefore uint32) {
	// The DMA is filling window w; wait until it has moved on to the other one.
	w := uint32(adcWriteWindow)
	if dmaInWindow(w) == 0 {
		// Shouldn't be in the next window already!
		println("overflow!")
	}
	for dmaInWindow(w) != 0 {
	}
	adcDelaySamples(delayBefore)
	adcWriteWindow = uint8(w ^ 1) // next window
	rawSamples := adcData[w*windowSize : w*windowSize+windowSize]

	// Adjust samples by DC offset.
	// At the same time, also sum up all the samples it reads so we can
	// calculate the DC offset for the next window.
	dcOffset = dsp.CopyWithOffset(rawSamples, adcDataNormalized[:], dcOffset)
}

// setIndicatorPin is Pin.Configure for the A1-A12 pins (all on GPIOA, whose
// clock configureLEDs enables at boot) in the three modes the receiver uses for
// its indicator lights. Configure inlines the clock enable and its whole mode
// switch at every call, and the receiver has a dozen of them.
//
//go:noinline
func setIndicatorPin(p machine.Pin, mode machine.PinMode) {
	pos := uint8(p) % 16 * 2
	if mode == machine.PinOutput {
		stm32.GPIOA.MODER.ReplaceBits(1, 3, pos)
		stm32.GPIOA.OSPEEDR.ReplaceBits(2, 3, pos) // high speed
		return
	}
	pull := uint32(0) // machine.PinInput: floating
	if mode == machine.PinInputPulldown {
		pull = 2
	}
	stm32.GPIOA.MODER.ReplaceBits(0, 3, pos)
	stm32.GPIOA.PUPDR.ReplaceBits(pull, 3, pos)
}

//go:noinline
func dataRecv(slot int) {
	// Reset LEDs.
	disableLEDs()

	// Enable indicator lights.
	setIndicatorPin(A10, machine.PinInputPulldown)
	setIndicatorPin(A12, machine.PinInputPulldown)

	setIndicatorPin(A9, machine.PinOutput)
	A9.High()
	if slot >= 1 {
		setIndicatorPin(A8, machine.PinOutput)
		A8.High()
	}

	// Enable HSI16.
	stm32.RCC.CR.SetBits(stm32.RCC_CR_HSI16ON)

	// Switch system clock to 4.2MHz.
	stm32.RCC.SetICSCR_MSIRANGE(stm32.RCC_ICSCR_MSIRANGE_Range6)

	// Capture: the double symbol buffer (windowSize*2 samples) at 10 kSPS,
	// ADCPRE=AHB/1 (ccr=0): at this clock the ADC is within spec and the 32x
	// oversampled conversion (~95 µs) just fits the 100 µs sample period.
	// See capture.go for the shared setup this replaced.
	//
	// Debug: slow down the ADC by raising the prescaler, e.g. 0xffff.
	startCapture(uint32(uintptr(unsafe.Pointer(&adcData))), len(adcData),
		clockPrescaler, 0)

	// Special marker used by the Goertzel implementation to know this is the
	// end of the array.
	adcDataNormalized[windowSize] = 0x7fff_ffff
	adcDataNormalized[windowSize+1] = 0 // pair code reads two at once; overlay is not zeroed

	// Receive the data.
	decoder.Initialize(animationBuf[1:], maxProgramSize, sampleRate)
	receiveData(slot)

	// Disable capture, then restore the idle clock.
	stopCapture()

	// Switch back to MSI.
	stm32.RCC.CFGR.ReplaceBits(stm32.RCC_CFGR_SWS_MSI, stm32.RCC_CFGR_SW_Msk, 0)
	for stm32.RCC.CFGR.Get()&stm32.RCC_CFGR_SW_Msk != stm32.RCC_CFGR_SWS_MSI {
	}
	stm32.RCC.SetCFGR_HPRE(stm32.RCC_CFGR_HPRE_Div1)

	// Disable HSI16
	stm32.RCC.CR.ClearBits(stm32.RCC_CR_HSI16ON)

	// Switch system clock back to default.
	stm32.RCC.SetICSCR_MSIRANGE(defaultMSIRANGE)

	// Restore the LEDs.
	// Note: have to set the below two as input due to a bug (pull mode is not
	// reset when switching from input to output mode).
	setIndicatorPin(A10, machine.PinInput)
	setIndicatorPin(A11, machine.PinInput)
	setIndicatorPin(A12, machine.PinInput)
	configureLEDs()
}

func receiveData(slot int) {
	decoder.Reset()

	button2WasReleased := false // true once button 2 was released once (for exiting the receiver)
	whiteWasOn := false         // used for blinking LED

	// Wait for the first full symbol.
	adcDataNextWindow(0)
	for {
		result := decoder.ProcessSamples(adcDataNormalized[:])

		// Adjust timer prescaler, to calibrate to the audio source.
		// This is also used to continuously adjust the frequency
		trim := stm32.TIM22.PSC.Get()
		if adj := decoder.SpeedAdjust(); adj < 0 {
			trim = min(clockPrescaler+4, trim+1)
			stm32.TIM22.PSC.Set(trim)
		} else if adj > 0 {
			trim = max(clockPrescaler-4, trim-1)
			stm32.TIM22.PSC.Set(trim)
		}

		// Decoder had to stop for some reason (error or fully received).
		if result < 0 {
			if result == datatrans.ErrCodeReceived {
				// Fully received! Show green LEDs (while saving the binary):
				setIndicatorPin(A10, machine.PinInput)
				setIndicatorPin(A11, machine.PinInputPulldown)
				setIndicatorPin(A12, machine.PinInput)

				// Save the binary.
				binary := decoder.Bytes()
				animationBuf[0] = uint32(len(binary)) | hash32(binary)<<16
				if slot >= 0 { // slot < 0 means keep in RAM
					storePattern(slot)
				}
				return
			}
			decoder.Reset()
			// Some error, restart.
			adcDataNextWindow(0)
			continue
		}

		if decoder.ReceivingValid() && !whiteWasOn {
			// Blink the LEDs white-ish.
			setIndicatorPin(A11, machine.PinInputPulldown)
			whiteWasOn = true
		} else {
			// Go back to the previous (purple) color.
			setIndicatorPin(A11, machine.PinInput)
			whiteWasOn = false
		}

		// Wait until the next symbol arrives.
		adcDataNextWindow(uint32(result))

		// Exit on button2 press (but don't exit immediately when button2 is
		// still pressed due to the long press to enter receiving mode).
		pressed := button2Pressed()
		if !pressed {
			button2WasReleased = true
		}
		if pressed && button2WasReleased {
			// stop receiving
			return
		}
	}
}
