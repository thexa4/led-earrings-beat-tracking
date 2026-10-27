//go:build baremetal && screamtelem

package main

import (
	"device/stm32"
	"runtime/volatile"
	"unsafe"

	"github.com/aykevl/things/earring-ring-rgb36v2/screamrx"
)

// The live receiver with counters, built with -tags screamtelem: the earring
// boots straight into dataRecv(-1) (receive into RAM, never write flash) and
// starts it again after every message, while the host reads the counters
// below with read_memory - unattended A/B runs of the real ADC path.
//
//	tinygo build -tags screamtelem -o screamtelem.hex -target=./stm32l0x1.json -opt=2 -panic=trap
//
// Never sleeps (stop mode kills SWD).
var screamTelemetry struct {
	Magic    uint32 // telemMagic once running
	Windows  uint32 // windows processed since boot
	Received uint32 // messages received
	LastHash uint32 // hash32 and length of the last one
	LastLen  uint32
	LastWin  uint32 // windows the last message took
	// MaxBacklog is the most samples the DMA was past the next window's
	// start right after a Process call: above windowSize the decoder is
	// falling behind, and at len(adcData) windows are lost (Overruns).
	// Signed: a timing move can put the next window's start ahead of it.
	MaxBacklog int32
	Overruns   uint32
	Trim       int32 // current TIM22 PSC offset
	Gain       int32
	Stats      screamrx.Stats
}

// The layout tools/screamtelem.py reads: 64 bytes.
var _ [unsafe.Sizeof(screamTelemetry) - 64]int8
var _ [64 - unsafe.Sizeof(screamTelemetry)]int8

const telemMagic = 0x5343524d // "SCRM"

var telemStart uint32

func screamTelem() {
	t := &screamTelemetry
	volatile.StoreUint32(&t.Magic, telemMagic)
	for {
		telemStart = t.Windows
		dataRecv(-1)
		b := decoder.Bytes()
		t.LastLen = uint32(len(b))
		t.LastHash = hash32(b)
		t.LastWin = t.Windows - telemStart
		volatile.StoreUint32(&t.Received, t.Received+1)
	}
}

// telemWindow runs after every decoded window.
func telemWindow() {
	t := &screamTelemetry
	t.MaxBacklog = max(t.MaxBacklog, int32(adcWritten()-adcRead))
	t.Overruns = uint32(adcOverruns)
	t.Trim = int32(stm32.TIM22.PSC.Get()) - clockPrescaler
	t.Gain = int32(decoder.Gain())
	t.Stats = decoder.Stats
	volatile.StoreUint32(&t.Windows, t.Windows+1)
}
