package main

import (
	"math/rand/v2"
	"testing"
)

// TestFountainWindowOverlap reproduces the receive buffer layout of the
// firmware: for programs over 1152 B the fountain's top slots used to share
// memory with the 20 ms window overlaid on animationBuf's tail, and since the
// fountain fills its highest free slot first, every stored multi-symbol packet
// was overwritten by the next window copy.
func TestFountainWindowOverlap(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for _, size := range []int{698, 1152, 1156, maxProgramSize} {
		data := make([]byte, size)
		for i := range data {
			data[i] = byte(r.Uint32())
		}
		for _, tc := range []struct {
			name string
			l    layout
			want bool // all trials must succeed
		}{
			{"device", layoutDevice, true},
			{"separate", layoutSeparate, true},
			{"overlap", layoutOverlap, size <= 1152},
		} {
			ok, total := 0, 0
			for range 10 {
				symbols, success := transfer(data, 0, r, tc.l)
				if success {
					ok++
					total += symbols
				}
			}
			mean := 0.0
			if ok > 0 {
				mean = float64(total) / float64(ok) / 50
			}
			t.Logf("%4d B %-8s %2d/10 received, mean %.1f s", size, tc.name, ok, mean)
			if tc.want && ok != 10 {
				t.Errorf("%d B, %s layout: %d/10 received, want 10", size, tc.name, ok)
			}
			if !tc.want && ok != 0 {
				t.Errorf("%d B, %s layout: %d/10 received, expected the overlap to break it", size, tc.name, ok)
			}
		}
	}
}
