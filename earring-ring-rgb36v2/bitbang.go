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

// refreshLEDs puts led0..led2 (the LEDs index, index+12 and index+24) in the
// bitplanes, then shows all twelve groups once. The LEDs are lit only while
// it shows them, so every cycle a main-loop pass spends elsewhere makes them
// dimmer: one call through a register (bitbang_refresh_leds) to one function
// in RAM, which converts and shows.
func refreshLEDs(index int, led0, led1, led2 uint32) {
	C.bitbang_refresh_leds(C.int(index), led0, led1, led2)
}

// updateLEDs shows the bitplanes as they are.
func updateLEDs() {
	refreshLEDs(-1, 0, 0, 0)
}

// refreshLEDsRAM is refreshLEDs; index < 0 only shows. In RAM: that saves a
// bit of current, and at 16 MHz (sync mode) flash has a wait state.
//
//export refreshLEDsRAM
//go:section .ramfuncs.refreshLEDs
func refreshLEDsRAM(index int, led0, led1, led2 uint32) {
	if index >= 0 {
		// Each group of three indices has its common line on one of the
		// four pin slots (see bitbang_update_bitplane), which gets 0; the
		// other three LEDs go to the remaining slots in board order.
		// tools/bitbang_equiv.py checks this against the per-group builders
		// it replaced.
		a, b, c, d := led0, led2, led1, uint32(0)
		switch index / 3 {
		case 0:
			b, c, d = 0, led2, led1
		case 1:
			c, d = 0, led1
		case 3:
			a, d = 0, led0
		}
		// bitbang_show_leds takes the groups in the order of their common
		// pins' bits: index 0..8 have PA8..PA0, 9..11 PA15, PA10, PA9.
		pos := 8 - index
		if pos < 0 {
			pos += 12
		}
		// index is a group (0..11), so pos is in range: no bounds check.
		group := unsafe.Add(unsafe.Pointer(&bitplanes), pos*int(unsafe.Sizeof(bitplanes[0])))
		C.bitbang_update_bitplane(a, b, c, d, (*uint32)(group))
	}
	C.bitbang_show_leds(&bitplanes[0][0], unsafe.Pointer(stm32.GPIOA))
}

var bitplanes [12][4]uint32 // in bitbang_show_leds order, see refreshLEDsRAM

// ledsOff is the bitplane word that lights nothing: every LED pin high (what
// bitbang_update_bitplane makes of black).
const ledsOff = ledPins<<16 | ledPins

// clearLEDs turns every LED off in the bitplanes; the next refresh shows it.
func clearLEDs() {
	C.bitbang_fill(&bitplanes[0][0], ledsOff, C.int(len(bitplanes)*len(bitplanes[0])))
}
