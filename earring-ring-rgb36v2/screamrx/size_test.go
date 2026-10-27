package screamrx

import (
	"codeberg.org/maaike328p/datatrans/dsp"
	"testing"
	"unsafe"
)

func TestSize(t *testing.T) {
	var d Decoder
	t.Logf("Decoder %d B: fountain %d, lv %d, packet %d, tones %d", unsafe.Sizeof(d),
		unsafe.Sizeof(d.fountain), unsafe.Sizeof(d.lv), unsafe.Sizeof(d.packet), unsafe.Sizeof(d.tones))
}

func TestGoertzelTable(t *testing.T) {
	for k := range numFreqs {
		var c dsp.GoertzelFixedConsts
		c.Init(float32(400+50*k), fs, symLen)
		if c != goertzelConst(k) {
			t.Errorf("tone %d: table %08x, Init %08x", k, goertzelConsts[k], *(*uint32)(unsafe.Pointer(&c)))
		}
	}
}
