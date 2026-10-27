//go:build baremetal && passcount

package main

// Bench build, -tags passcount: counts main-loop passes in every mode, for
// tools/passrate.py. The LEDs are lit only while a pass refreshes them, a
// fixed time per pass, so passes/s is the LEDs' brightness (and, outside
// sync mode, the animation speed: a frame is 12 passes). Everything else is
// the production firmware.
//
//	tinygo build -tags passcount -o passcount.elf -target=./stm32l0x1.json -opt=2 -panic=trap
var benchPasses uint32

func benchPass() {
	benchPasses++
}
