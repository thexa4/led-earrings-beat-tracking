//go:build baremetal && screamfeed

package main

import (
	"device/arm"
	"device/stm32"
	"runtime/volatile"

	"github.com/aykevl/things/earring-ring-rgb36v2/screamrx"
)

// The scream decoder on target, fed by the host: built with -tags screamfeed,
// the earring boots straight into the receive decoder, but its windows come
// over SWD from the host instead of from the ADC. The host test
// (screamrx TestFeedTarget) runs the simulator, sends each window, and
// compares every result with the same decoder on the host, bit for bit;
// SysTick times every Process call at the receiver's 16 MHz clock.
//
//	tinygo build -tags screamfeed -o screamfeed.hex -target=./stm32l0x1.json -opt=2 -panic=trap
//
// Protocol: the host writes the window (200 int16, after DC removal and the
// decoder's gain shift) into adcData, then bumps Seq; the firmware processes
// it and publishes the results with Ack = Seq. Never sleeps (stop mode kills
// SWD) and never writes flash.
var screamFeedCtl struct {
	Seq, Ack  uint32
	Status    int32
	Adjust    int32
	Trim      int32
	Gain      int32
	Cycles    uint32 // this window's Process call
	MaxCycles uint32
	Length    uint32 // received message: length and hash32 (StatusReceived)
	Hash      uint32
	Stats     screamrx.Stats
}

func screamFeed() {
	hsi16Up(stm32.PWR_CR_VOS_V1_5)
	arm.SYST.SYST_RVR.Set(0xffffff)
	arm.SYST.SYST_CVR.Set(0)
	arm.SYST.SYST_CSR.Set(arm.SYST_CSR_ENABLE | arm.SYST_CSR_CLKSOURCE)

	adcDataNormalized[windowSize] = 0x7fff_ffff
	adcDataNormalized[windowSize+1] = 0
	decoder.Initialize(animationBuf[1:fountainBufEnd], maxProgramSize, sampleRate,
		windowSize*256/(clockPrescaler+1))
	c := &screamFeedCtl
	for {
		// Volatile: the host writes Seq behind the compiler's back (a plain
		// load makes this loop provably infinite, and LLVM drops the rest).
		seq := volatile.LoadUint32(&c.Seq)
		for seq == volatile.LoadUint32(&c.Ack) {
			seq = volatile.LoadUint32(&c.Seq)
		}
		for i := range windowSize {
			adcDataNormalized[i] = int32(int16(volatile.LoadUint16(&adcData[i])))
		}
		t0 := arm.SYST.SYST_CVR.Get()
		status := decoder.Process(adcDataNormalized[:])
		cycles := (t0 - arm.SYST.SYST_CVR.Get()) & 0xffffff
		c.Status = int32(status)
		c.Adjust = int32(decoder.Adjust())
		c.Trim = int32(decoder.RateTrim())
		c.Gain = int32(decoder.Gain())
		c.Cycles = cycles
		c.MaxCycles = max(c.MaxCycles, cycles)
		if status == screamrx.StatusReceived {
			b := decoder.Bytes()
			c.Length = uint32(len(b))
			c.Hash = hash32(b)
		}
		c.Stats = decoder.Stats
		if status < 0 {
			decoder.Reset()
		}
		volatile.StoreUint32(&c.Ack, seq)
	}
}
