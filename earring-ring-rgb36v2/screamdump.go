//go:build baremetal && screamdump

package main

import (
	"device/stm32"
	"machine"
	"unsafe"
)

// The scream recorder, compiled only with -tags screamdump: the earring boots
// straight into the data receiver's capture path and records what the
// microphone hears, so a pattern transfer can be replayed into the decoder on
// the host (tools/screamdump.py reads it). It never returns to the
// animations, never sleeps (stop mode kills SWD) and never writes flash.
//
//	tinygo build -tags screamdump -o screamdump.hex -target=./stm32l0x1.json -opt=2 -panic=trap
//
// Capture is startCapture exactly as dataRecv calls it, at 10 kSPS, in one of
// three clock configurations the host selects at run time by writing
// screamCfgReq (the firmware tears the capture down and brings it up again in
// the new configuration, then echoes the value in screamCfg):
//
//	0  dataRecv today: MSI 4.19 MHz, voltage range 3, ADC on async HSI16 /1
//	   (16 MHz, out of spec: range 3 allows 4 MHz), TIM22 PSC 209
//	1  HSI16 system clock, range 1, 1 WS, ADC async /1 (16 MHz), PSC 799
//	2  HSI16 system clock, range 2, 1 WS, ADC async /2 (8 MHz), PSC 799
//
// Samples are stored as 8-bit G.711 mu-law (10 kB/s, which both ST-Links
// sustain at once; 16-bit would not fit the ~19 kB/s each one reads) of the
// deviation from a slow IIR DC tracker, divided by 2^screamShift (host
// writable, default 1, so +-16k codes fit the 14-bit mu-law range).
//
// Layout in animationBuf, words: the sample ring, then the control words.
// The sample counter is the highest word, so a read of the ring followed by
// a read of the counter bounds what the ring could hold (see the reader).
const (
	screamRingBytes = 2400 // 240 ms at 10 kSPS
	screamRingWords = screamRingBytes / 4
)

var _ [len(animationBuf) - screamRingWords - 8]int8 // ring + control words fit

var (
	screamCfgReq  = &animationBuf[screamRingWords+0] // host: wanted clock config
	screamCfg     = &animationBuf[screamRingWords+1] // config in force
	screamShift   = &animationBuf[screamRingWords+2] // host: deviation >> this
	screamDC      = &animationBuf[screamRingWords+3] // DC estimate, Q4 codes
	screamClipped = &animationBuf[screamRingWords+4] // samples past mu-law range
	screamToggle  = &animationBuf[screamRingWords+5] // host: 1 toggles A11 per symbol
	screamCount   = &animationBuf[screamRingWords+6] // samples stored since boot
)

const (
	screamDCFrac  = 4
	screamDCShift = 10 // ~100 ms at 10 kSPS: well below the 400 Hz lowest tone
)

func screamRing() []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(&animationBuf[0])), screamRingBytes)
}

func screamDump() {
	disableLEDs()
	// The receiver's indicator lights, as dataRecv drives them for slot 0.
	setIndicatorPin(A10, machine.PinInputPulldown)
	setIndicatorPin(A12, machine.PinInputPulldown)
	setIndicatorPin(A9, machine.PinOutput)
	A9.High()

	*screamCfgReq = 0
	*screamShift = 1
	*screamToggle = 0
	*screamCount = 0
	*screamClipped = 0
	for {
		cfg := *screamCfgReq
		screamStart(cfg)
		*screamCfg = cfg
		screamRun(cfg)
		stopCapture()
	}
}

// screamStart switches to clock configuration cfg and starts the capture.
func screamStart(cfg uint32) {
	stm32.RCC.CR.SetBits(stm32.RCC_CR_HSI16ON_Msk)
	for stm32.RCC.CR.Get()&stm32.RCC_CR_HSI16RDYF_Msk == 0 {
	}
	// Always pass through MSI at range 3 / WS0 first, so every switch starts
	// from the same, legal state.
	stm32.RCC.SetICSCR_MSIRANGE(stm32.RCC_ICSCR_MSIRANGE_Range6)
	stm32.RCC.CFGR.ReplaceBits(stm32.RCC_CFGR_SW_MSI, stm32.RCC_CFGR_SW_Msk, 0)
	for stm32.RCC.CFGR.Get()&stm32.RCC_CFGR_SWS_Msk != stm32.RCC_CFGR_SWS_MSI {
	}
	vos := uint32(stm32.PWR_CR_VOS_V1_2)
	switch cfg {
	case 1:
		vos = stm32.PWR_CR_VOS_V1_8
	case 2:
		vos = stm32.PWR_CR_VOS_V1_5
	}
	stm32.RCC.APB1ENR.SetBits(stm32.RCC_APB1ENR_PWREN_Msk)
	stm32.PWR.CR.ReplaceBits(vos, stm32.PWR_CR_VOS_Msk, stm32.PWR_CR_VOS_Pos)
	waitForVOS()
	stm32.RCC.APB1ENR.ClearBits(stm32.RCC_APB1ENR_PWREN_Msk)

	psc := uint32(4.2e6/sampleRate/2 - 1) // the old receiver's MSI setting
	ccr := uint32(0)
	if cfg != 0 {
		stm32.FLASH.SetACR_LATENCY(stm32.Flash_ACR_LATENCY_WS1)
		stm32.RCC.CFGR.ReplaceBits(stm32.RCC_CFGR_SW_HSI16, stm32.RCC_CFGR_SW_Msk, 0)
		for stm32.RCC.CFGR.Get()&stm32.RCC_CFGR_SWS_Msk != (stm32.RCC_CFGR_SWS_HSI16 << 2) {
		}
		psc = clockPrescaler
		if cfg == 2 {
			ccr = 1 << stm32.ADC_CCR_PRESC_Pos // async clock /2
		}
	} else {
		stm32.FLASH.SetACR_LATENCY(stm32.Flash_ACR_LATENCY_WS0)
	}
	startCapture(uint32(uintptr(unsafe.Pointer(&adcData))), len(adcData), psc, ccr)
}

// screamRun records until the host asks for another configuration.
func screamRun(cfg uint32) {
	read := uint32(0)
	seeded := false
	dc := int32(0)
	phase := 0
	ring := screamRing()
	for *screamCfgReq == cfg {
		write := adcDataWriteIndex()
		if write >= uint32(len(adcData)) {
			write = 0
		}
		for read != write {
			s := int32(adcData[read]) << screamDCFrac
			read++
			if read == uint32(len(adcData)) {
				read = 0
			}
			if !seeded {
				dc = s
				seeded = true
			}
			dev := s - dc
			dc += (dev + 1<<(screamDCShift-1)) >> screamDCShift
			v := dev >> (screamDCFrac + *screamShift)
			n := *screamCount
			ring[n%screamRingBytes] = muLaw(v)
			*screamCount = n + 1

			phase++
			if phase == windowSize {
				phase = 0
				*screamDC = uint32(dc)
				if *screamToggle != 0 {
					A11.Set(!A11.Get())
				}
			}
		}
	}
}

// muLaw is G.711 mu-law of a 14-bit signed value (clipped, and counted).
//
//go:noinline
func muLaw(v int32) byte {
	sign := byte(0)
	if v < 0 {
		v = -v
		sign = 0x80
	}
	if v > 8158 {
		v = 8158
		*screamClipped++
	}
	v += 33
	exp := byte(0)
	for t := v >> 6; t != 0; t >>= 1 {
		exp++
	}
	mant := byte(v>>(exp+1)) & 0xf
	return ^(sign | exp<<4 | mant)
}
