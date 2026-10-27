package main

import "device/stm32"

// hsi16Up switches the system clock from MSI to HSI16 (exactly 16 MHz), at
// voltage scale vos: PWR_CR_VOS_V1_8 (range 1) for sync, whose ADC runs from
// PCLK, or PWR_CR_VOS_V1_5 (range 2, which allows 16 MHz with one wait state)
// for the data receiver. Sync and the data receiver share this one copy.
//
// The idle config runs range 3, whose 4.2 MHz maximum the old receiver's
// 16 MHz ADC clock was already quietly over. Order: raise the scale and wait
// for the regulator (VOSF clears when ready), give the flash its wait state,
// then switch over. Power is the bench supply for sync; this trade is not for
// battery firmware.
//
// TinyGo's device/stm32 masks for this part are already shifted into place
// (HSI16ON_Msk=0x1 at Pos 0, HSI16RDYF_Msk=0x4 at Pos 2): use them directly -
// shifting again by _Pos points at the wrong bit.
//
//go:noinline
func hsi16Up(vos uint32) {
	stm32.RCC.CR.SetBits(stm32.RCC_CR_HSI16ON_Msk)
	for stm32.RCC.CR.Get()&stm32.RCC_CR_HSI16RDYF_Msk == 0 {
	}
	stm32.RCC.APB1ENR.SetBits(stm32.RCC_APB1ENR_PWREN_Msk)
	stm32.PWR.CR.ReplaceBits(vos, stm32.PWR_CR_VOS_Msk, stm32.PWR_CR_VOS_Pos)
	waitForVOS()
	stm32.RCC.APB1ENR.ClearBits(stm32.RCC_APB1ENR_PWREN_Msk)
	// One wait state, and the prefetch that makes it nearly free for
	// straight-line code (measured: the data receiver's Process 175k instead
	// of 231k cycles per window; sync's per-sample path ~10 % faster).
	stm32.FLASH.ACR.SetBits(stm32.Flash_ACR_LATENCY_WS1 | stm32.Flash_ACR_PRFTEN)
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
}

// hsi16Down returns to the animation clock: switch to MSI first (still at the
// idle range - nothing here retunes MSI), then it is legal to drop the voltage
// scale again; the flash latency can go either way but WS0 matches the idle
// config. Order mirrors hsi16Up in reverse. HSI16 is switched off (nothing
// else uses it).
//
//go:noinline
func hsi16Down() {
	stm32.RCC.CFGR.ReplaceBits(stm32.RCC_CFGR_SW_MSI, stm32.RCC_CFGR_SW_Msk, 0)
	// (MSI is field value 0, so the SWS mask/constant mismatch noted above
	// cannot bite here.)
	for stm32.RCC.CFGR.Get()&stm32.RCC_CFGR_SWS_Msk != stm32.RCC_CFGR_SWS_MSI {
	}
	// Back to WS0 and no prefetch.
	stm32.FLASH.ACR.ClearBits(stm32.Flash_ACR_LATENCY_Msk | stm32.Flash_ACR_PRFTEN)
	stm32.RCC.APB1ENR.SetBits(stm32.RCC_APB1ENR_PWREN)
	stm32.PWR.CR.ReplaceBits(stm32.PWR_CR_VOS_V1_2, stm32.PWR_CR_VOS_Msk,
		stm32.PWR_CR_VOS_Pos)
	waitForVOS()
	stm32.RCC.APB1ENR.ClearBits(stm32.RCC_APB1ENR_PWREN)
	stm32.RCC.CR.ClearBits(stm32.RCC_CR_HSI16ON)
}
