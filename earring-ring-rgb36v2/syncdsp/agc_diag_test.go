package syncdsp

import (
	"fmt"
	"testing"
)

// Temporary diagnostic: AGC behaviour and decision agreement vs level.
func TestAGCDiag(t *testing.T) {
	cases := []struct {
		name string
		dist func([]uint16, *int32)
	}{
		{"none", nil},
		{"gain-6dB", halfGain},
		{"hf-tilt", hfTilt},
		{"quiet-42dB", quiet},
		{"bench-quiet", benchOffset},
	}
	for _, tc := range cases {
		in := newInst(tc.dist, false)
		changes := 0
		prev := in.p.agcShift
		minS, maxS := int32(99), int32(-1)
		peakMin, peakMax := int32(1<<30), int32(0)
		locks := 0
		for f := 0; f < testFrames; f++ {
			in.frame(f)
			if in.p.beat.Locked {
				locks++
			}
			if f > 64 { // ignore the initial settle
				if in.p.agcShift != prev {
					changes++
					prev = in.p.agcShift
				}
				if in.p.agcShift < minS {
					minS = in.p.agcShift
				}
				if in.p.agcShift > maxS {
					maxS = in.p.agcShift
				}
				post := in.p.agcPeak << uint(in.p.agcShift)
				if post < peakMin {
					peakMin = post
				}
				if post > peakMax {
					peakMax = post
				}
			}
		}
		fmt.Printf("%-12s locks=%5.1f%% gain-changes=%3d shift=[%d..%d] post-peak=[%d..%d]\n",
			tc.name, 100*float64(locks)/float64(testFrames), changes, minS, maxS,
			peakMin, peakMax)
	}
}
