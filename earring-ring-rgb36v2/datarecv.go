package main

import (
	"device/stm32"
	"machine"
	"unsafe"

	"github.com/aykevl/things/earring-ring-rgb36v2/rolled"
	"github.com/aykevl/things/earring-ring-rgb36v2/screamrx"
)

const (
	sampleRate = 10000

	// number of samples per window we do frequency analysis on (one symbol)
	windowSize = sampleRate / 50

	// TIM22 prescaler for 10 kSPS from the 16 MHz HSI16 clock the receiver
	// runs at: 16 MHz / (2 * (799+1)). One step is 0.125%.
	clockPrescaler = 16e6/sampleRate/2 - 1

	// Largest PSC trim, in steps: HSI16 is factory trimmed to +-1% at 25 C
	// and drifts further with temperature, and the sender's clock adds its
	// own error. +-20 steps is +-2.5%.
	maxTrim = 20

	// Windows (one symbol, 20 ms) without a verified packet after which the
	// receiver drops its clock trim, 5 s: a working lock verifies a packet
	// at least every 15 windows (14 words and the hash symbol).
	staleWindows = 250
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
// To save RAM this is overlaid on the tail of animationBuf: the fountain
// decoder is given only animationBuf[1:fountainBufEnd], the words between the
// size/hash header word and this overlay (bytes 4..1748), checked below to
// hold a whole program. It must not get all of animationBuf[1:]: the fountain
// stores up to 48 packets, filling its highest slot first, so for programs
// over 1152 B its top slots would land on this window and be overwritten on
// every symbol (TestFountainWindowOverlap in scream/tools/screamcheck). With
// 436 words a 1532 B program gets 36 slots instead of 48, which slows a
// transfer by ~2-4% in screamcheck. The Goertzel and copy
// code receive the buffer as an ordinary slice argument, so unlike an overlay
// of the sync pipeline (which is fed from the same loop that reads the DMA
// buffer) the aliasing cast costs the hot loops nothing. receiveData sets
// both stop-sentinel words explicitly because, unlike the old bss array, the
// overlay does not come pre-zeroed.
const adcDataNormWords = windowSize + 2

const fountainBufEnd = len(animationBuf) - adcDataNormWords

var _ [fountainBufEnd - 1 - maxProgramSize/4]int8 // the fountain holds a program

var adcDataNormalized = unsafe.Slice((*int32)(unsafe.Pointer(&animationBuf[fountainBufEnd])), adcDataNormWords)

func adcDataWriteIndex() uint32 {
	return uint32(len(adcData)) - stm32.DMA1.CH[0].NDTR.Get()
}

// The receive decoder lives *inside* the sync pipeline (see main.syncPipe):
// the two engines are never live at the same time - dataRecv runs the main
// loop with sync stopped (main.go stops sync on every mode switch, and sync
// mode has no path into dataRecv), and both re-init their state on entry
// (startSync -> Pipeline.Init, dataRecv -> Decoder.Initialize), so neither
// ever reads the other's leftovers. syncPipe is the larger of the two, and
// both are plain integer structs (the decoder's one slice points into
// animationBuf), so the cast is safe.
//
// Unlike the rejected animationBuf overlay (which aliased the DMA buffer and
// cost the feed loop its register allocation), neither loop reads the
// other's storage here: the receive loop consumes adcDataNormalized, and
// sync never runs during a receive.
var decoder = (*screamrx.Decoder)(unsafe.Pointer(&syncPipe))

var _ [unsafe.Sizeof(syncPipe) - unsafe.Sizeof(screamrx.Decoder{})]byte

// The receiver reads the circular DMA buffer by software index, so a window
// can start at any sample (the decoder moves it by Adjust to stay on the
// symbol grid). adcWritten counts the samples the DMA has written since the
// capture started; adcRead is where the next window starts, on that count.
// (The old receiver moved the grid by stopping the DMA for a few sample
// periods, which BENCH.md forbids: the ADC overruns and the ring can freeze.)
var (
	adcBase, adcLast uint32 // adcWritten bookkeeping: whole laps, last index
	adcRead          uint32
	dcOffset         uint32
	// Overruns counts windows the DMA had already overwritten when they were
	// copied (the decoder fell more than a window behind).
	adcOverruns uint16
)

// adcWritten returns the monotonic DMA write count. It must be called at
// least once per lap of adcData (40 ms), which the receive loop does: a
// window takes 20 ms and the decoder keeps up with it.
//
//go:noinline
func adcWritten() uint32 {
	i := adcDataWriteIndex()
	if i >= uint32(len(adcData)) {
		i = 0 // NDTR reads 0 for an instant before it reloads
	}
	if i < adcLast {
		adcBase += uint32(len(adcData))
	}
	adcLast = i
	return adcBase + i
}

// adcNextWindow waits until the window at adcRead is complete and copies it,
// less the previous window's mean (the DC offset) and shifted right by the
// decoder's gain, into adcDataNormalized.
// It reports false if part of the window had been overwritten before the
// copy finished; the window is then moved to the newest samples.
//
//go:noinline
func adcNextWindow() bool {
	for adcWritten() < adcRead+windowSize {
	}
	// The window is at most two straight runs of the ring: up to its end,
	// then from its start.
	src := adcRead % uint32(len(adcData))
	n := min(windowSize, uint32(len(adcData))-src)
	dcOffset = (normalize(0, src, n) + normalize(n, 0, windowSize-n)) / windowSize
	ok := adcWritten() <= adcRead+uint32(len(adcData))
	adcRead += windowSize
	if !ok {
		adcOverruns++
		adcRead = adcWritten() - windowSize
	}
	return ok
}

// normalize copies n samples from adcData[src:] to adcDataNormalized[dst:],
// less the DC offset and shifted right by the decoder's gain, and returns
// their sum. One rolled loop for both runs of a window: one loop over the
// ring, with the wrap check in every step, was 456 bytes unrolled, and 30%
// more instructions per sample rolled.
//
//go:noinline
func normalize(dst, src, n uint32) uint32 {
	out := adcDataNormalized[dst : dst+n]
	dc := int32(dcOffset)
	gain := decoder.Gain()
	sum := uint32(0)
	for i, v := range adcData[src : src+n] {
		rolled.Loop()
		out[i] = (int32(v) - dc) >> gain
		sum += uint32(v)
	}
	return sum
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

	// System clock: HSI16 at voltage range 2 with one wait state, and the
	// ADC on its asynchronous HSI16 clock divided by 2 (8 MHz; CKMODE=0).
	// The old receiver ran the core from MSI at 4.2 MHz in range 3 with the
	// ADC at 16 MHz, four times the range 3 limit; this is in spec, and the
	// decoder needs the cycles. A 32x oversampled conversion takes 448 ADC
	// cycles, 56 us of the 100 us sample period. Recorded with screamdump
	// (configuration 2), the noise floor is that of the old setting (MAD
	// 16-20 codes in every configuration).
	hsi16Up(stm32.PWR_CR_VOS_V1_5)
	startCapture(uint32(uintptr(unsafe.Pointer(&adcData))), len(adcData),
		clockPrescaler, 1<<stm32.ADC_CCR_PRESC_Pos)

	// Special marker used by the Goertzel implementation to know this is the
	// end of the array.
	adcDataNormalized[windowSize] = 0x7fff_ffff
	adcDataNormalized[windowSize+1] = 0 // pair code reads two at once; overlay is not zeroed

	// Receive the data.
	decoder.Initialize(animationBuf[1:fountainBufEnd], maxProgramSize, sampleRate,
		windowSize*256/(clockPrescaler+1))
	receiveData(slot)

	// Disable capture, then restore the idle clock.
	stopCapture()
	hsi16Down()

	// Restore the LEDs.
	// Note: have to set the below two as input due to a bug (pull mode is not
	// reset when switching from input to output mode).
	setIndicatorPin(A10, machine.PinInput)
	setIndicatorPin(A11, machine.PinInput)
	setIndicatorPin(A12, machine.PinInput)
	configureLEDs()
}

func receiveData(slot int) {
	// No decoder.Reset(): dataRecv's Initialize, just before, ends with one.

	button2WasReleased := false // true once button 2 was released once (for exiting the receiver)
	whiteWasOn := false         // used for blinking LED
	trim := int32(0)
	stale := staleWindows // windows left until the trim is dropped

	adcBase, adcLast = 0, 0
	adcRead = adcWritten()
	adcNextWindow() // primes the DC offset
	for {
		if !adcNextWindow() {
			decoder.Reset()
		}
		result := decoder.Process(adcDataNormalized[:])
		adcRead += uint32(int32(decoder.Adjust()))
		telemWindow() // -tags screamtelem only

		// Follow the sender's clock, but go back to the nominal one after
		// staleWindows without a verified packet. The trim outlives the
		// decoder's resets so that the clock keeps following the sender, but
		// a lock can walk it away from the sender before it is lost, and
		// later locks that never verify give back only their own trims, so
		// nothing brings it back (bench: one receive sat at PSC 797 against
		// a sender at 801 and verified nothing for over 90 s; receives that
		// started from 799 took 5 to 18 s). The decoder resets first, so
		// that this RateTrim drains its give-back too.
		stalled := stale == 0
		if stalled {
			decoder.Reset()
		}
		t := int32(decoder.RateTrim())
		if stalled {
			t = -trim
		}
		if t != 0 {
			trim = max(-maxTrim, min(maxTrim, trim+t))
			stm32.TIM22.PSC.Set(uint32(clockPrescaler + trim))
		}

		// Decoder had to stop for some reason (error or fully received).
		if result < 0 {
			if result == screamrx.StatusReceived {
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
			// Some error, restart.
			decoder.Reset()
			continue
		}

		valid := decoder.ReceivingValid()
		if valid && !whiteWasOn {
			// Blink the LEDs white-ish.
			setIndicatorPin(A11, machine.PinInputPulldown)
			whiteWasOn = true
		} else {
			// Go back to the previous (purple) color.
			setIndicatorPin(A11, machine.PinInput)
			whiteWasOn = false
		}
		// Once per stall is enough: a trim that outlives a lock was made by
		// one that verified a packet, which starts the count again.
		stale--
		if valid {
			stale = staleWindows
		}

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
