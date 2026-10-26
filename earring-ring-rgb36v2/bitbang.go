//go:build baremetal

package main

// #include "bitbang.h"
import "C"
import (
	"device/stm32"
	"unsafe"
)

func configureLEDs() {
	// Note: enabling a pullup would save a bit of current (because we'd avoid
	// floating inputs) but sadly also means some LEDs get turned on slightly
	// unintentionally.
	// It might be possible to fix this by controlling the MODER register
	// directly (instead of the ODR register), setting the pin to either output
	// mode (high/low depending on anode/cathode) or analog mode which disables
	// the output entirely.

	// Set A1-A12 as open drain (and importantly, skip SWDIO/SWCLK)
	stm32.GPIOA.OTYPER.Set(ledPins)

	// All high (off), then all to output at high speed: what A1..A12.High()
	// and Configure(PinOutput) do pin by pin, as one write per register.
	stm32.RCC.IOPENR.SetBits(stm32.RCC_IOPENR_IOPAEN)
	stm32.GPIOA.BSRR.Set(ledPins)
	stm32.GPIOA.MODER.Set(stm32.GPIOA.MODER.Get()&^ledPins2 | ledPins2&0x5555_5555)
	stm32.GPIOA.OSPEEDR.Set(stm32.GPIOA.OSPEEDR.Get()&^ledPins2 | ledPins2&0xAAAA_AAAA)
}

// ledPins are A1-A12 (PA15, PA10..PA0) as a one-bit-per-pin mask; ledPins2 is
// the same pins in the two-bit-per-pin layout of MODER, OSPEEDR and PUPDR.
const (
	ledPins  = 0b1000_0111_1111_1111
	ledPins2 = 0b11<<30 | 1<<22 - 1
)

func disableLEDs() {
	// Configure(PinInputAnalog) on A1..A12: analog mode, no pull.
	stm32.GPIOA.MODER.SetBits(ledPins2)
	stm32.GPIOA.PUPDR.ClearBits(ledPins2)

	// Restore OTYPER to default value.
	stm32.GPIOA.OTYPER.Set(0)
}

// This function really shouldn't be inlined, since it is called in a few places
// and if it's inlined all of the long assembly for generating bitplanes is also
// inlined there.
//
// Size could be further reduced by not inlining the individual
// bitbang_update_bitplane_x functions but this seems fine for now.
//
//go:noinline
func setLEDs(index int, led0, led1, led2 uint32) {
	// Every third bitplane uses the same timing group; the C helpers only
	// touch memory (the timing-critical part is bitbang_show_leds), so the
	// group is picked from the index instead of repeating twelve inlined
	// copies here.
	switch index / 3 {
	case 0:
		C.bitbang_update_bitplane_1(led0, led1, led2, &bitplanes[index][0])
	case 1:
		C.bitbang_update_bitplane_2(led0, led1, led2, &bitplanes[index][0])
	case 2:
		C.bitbang_update_bitplane_3(led0, led1, led2, &bitplanes[index][0])
	case 3:
		C.bitbang_update_bitplane_4(led0, led1, led2, &bitplanes[index][0])
	}
}

var bitplanes [12][3]uint32

// Putting updateLEDs in RAM saves a bit of current consumption.
// TODO: this goes through a thunk, which adds a few cycles. GCC has
// __attribute__((long_call)) for ARM, perhaps we can also add this to Clang?
// (It's supported in Clang, but not in the ARM backend).
//
//go:section .ramfuncs.updateLEDs
func updateLEDs() {
	C.bitbang_show_leds(&bitplanes[0][0], unsafe.Pointer(stm32.GPIOA))
}
