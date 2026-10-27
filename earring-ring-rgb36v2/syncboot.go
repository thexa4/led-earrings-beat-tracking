//go:build baremetal && syncboot

package main

import "device/stm32"

// Bench build, -tags syncboot: boots into sync mode whatever mode the flash
// holds (the RAM copy only; nothing is saved), so a freshly flashed earring
// can be profiled in sync without pressing its buttons. Everything else is
// the production firmware.
//
//	tinygo build -tags syncboot -o syncboot.hex -target=./stm32l0x1.json -opt=2 -panic=trap
func syncBoot() {
	storedState[stateOffsetMode] = modeSync
}

// syncWraps counts the DMA ring's wraps (its transfer-complete flag, polled at
// each syncPoll; nothing else reads it in sync mode), so the host can compare
// what the ADC wrote, syncWraps*Window, with what the pipeline consumed,
// syncAnimFrame*Window+syncFed. A difference is samples lost to a lapped
// reader; agreement at under 16000/s is the HSI16 clock.
var syncWraps uint32

// syncPolls counts main-loop passes, one syncPoll each, three LEDs drawn per
// pass: the loop keeps up with the ADC while it makes 16000/syncFeedCap = 667
// passes/s, so the pass rate is what an uploaded program's getPixel costs.
// (The DMA backlog, what a long nextFrame costs, the host samples itself:
// NDTR against syncRead. A max kept here did not fit in flash.)
var syncPolls uint32

func syncProbe() {
	// Channel 1's TCIF is bit 1: TinyGo's DMA_ISR_TCIF1 is 0x20, channel 2's.
	if stm32.DMA1.ISR.Get()&0x2 != 0 {
		stm32.DMA1.IFCR.Set(0x2)
		syncWraps++
	}
	syncPolls++
}
