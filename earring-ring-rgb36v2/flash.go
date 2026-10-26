package main

// Minimal STM32L0 program-memory driver, replacing machine.Flash (the
// BlockDevice/io.ReaderAt/io.WriterAt abstraction costs about 1 kB of code
// for the three operations this firmware actually needs; the whole binary is
// the budget). The register sequences are copied verbatim from
// machine/machine_stm32l0_flash.go so the behavior is identical to what the
// pattern-save path has always used, including the L0 quirk that erased
// flash has zero bits.

import (
	"device/stm32"
	"runtime/interrupt"
	"runtime/volatile"
	"unsafe"
)

const (
	flashBase      = 0x08000000
	flashSize      = 32 * 1024
	flashPageSize  = 128 // smallest erasable unit on the L0
	flashDataStart = flashBase
	flashDataEnd   = flashBase + flashSize
)

// flashRead copies flash contents at an absolute address into dst. Flash is
// memory mapped, so this is just a copy.
func flashRead(dst []byte, addr uintptr) {
	src := unsafe.Slice((*byte)(unsafe.Pointer(addr)), len(dst))
	copy(dst, src)
}

// Not inlined: flashErasePages and flashWrite both start with it.
//
//go:noinline
func flashUnlock() {
	// Make sure the flash peripheral clock is enabled.
	stm32.RCC.AHBENR.SetBits(stm32.RCC_AHBENR_MIFEN)

	// Wait for the flash memory not to be busy.
	for stm32.FLASH.GetSR_BSY() != 0 {
	}

	// Disable interrupts while writing, since no memory operations may happen
	// while the unlock sequence is ongoing.
	mask := interrupt.Disable()

	// Remove PELOCK bit.
	stm32.FLASH.PEKEYR.Set(0x89ABCDEF)
	stm32.FLASH.PEKEYR.Set(0x02030405)

	// Remove PRGLOCK bit.
	stm32.FLASH.PRGKEYR.Set(0x8C9DAEBF)
	stm32.FLASH.PRGKEYR.Set(0x13141516)

	interrupt.Restore(mask)
}

func flashLock() {
	// Set PELOCK to 1, which also automatically sets PRGLOCK to 1.
	stm32.FLASH.PECR.Set(stm32.Flash_PECR_PELOCK)
}

// flashErasePages erases count pages of flash starting at page address addr.
//
// No defer for the lock in these two: on -panic=trap nothing between the unlock
// and the lock can return early, and a defer frame costs more than the call.
func flashErasePages(addr uintptr, count int) {
	flashUnlock()
	stm32.FLASH.PECR.Set(stm32.Flash_PECR_ERASE | stm32.Flash_PECR_PROG)

	for i := 0; i < count; i++ {
		a := addr + uintptr(i)*flashPageSize
		// To erase, write any value to somewhere in that page.
		(*volatile.Register32)(unsafe.Pointer(a)).Set(uint32(a))
	}
	flashLock()
}

// flashWrite writes len(p) bytes (len must be a multiple of 4, addr must be
// word aligned) to flash that has just been erased.
func flashWrite(addr uintptr, p []byte) {
	flashUnlock()

	// Callers pass a multiple of 4 bytes at a word-aligned address, so the
	// buffer can be reinterpreted as words directly (the byte-wise packing
	// plus its length checks cost more than they saved).
	words := unsafe.Slice((*uint32)(unsafe.Pointer(unsafe.SliceData(p))), len(p)/4)
	for i := 0; i < len(words); i++ {
		(*volatile.Register32)(unsafe.Pointer(addr + uintptr(i)*4)).Set(words[i])
	}
	flashLock()
}
