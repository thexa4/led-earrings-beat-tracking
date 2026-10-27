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

func TestISqrt32(t *testing.T) {
	check := func(x uint32) {
		if got, want := isqrt32(x), dsp.ISqrt32(x); got != want {
			t.Errorf("isqrt32(%d) = %d, dsp.ISqrt32 %d", x, got, want)
		}
	}
	for r := range uint32(1 << 16) {
		// Every root's square and its neighbours, then the top of the range.
		check(r*r - 1)
		check(r * r)
		check(r*r + 1)
	}
	for x := uint32(0xffffffff); x > 0xfff00000; x -= 0x123 {
		check(x)
	}
}
