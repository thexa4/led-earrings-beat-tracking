package syncdsp

// Host proxy for the dominant device-side disagreement mechanism.
//
// Measured on the earring (tools/fieldwise.py over run pairs): identical music
// at aligned frames gives identity distance ~4.6 (hot field 3.0 - bits 24..26
// flip 47-80%), and the two runs' beat grids sit a median of 5 frames apart.
// The host's "rerun" model scores 0.00 because it replays the file with an
// identical clock - the one perturbation every host model lacks is the
// sample-phase offset between the audio stream and the analysis frame grid,
// which on the device differs per run (the player starts when it starts) and
// per beat (the grid locks where it locks).
//
// This test supplies exactly that perturbation: replay the same file twice,
// the second starting `off` samples late, so every frame's boundaries cut the
// music at a different phase while the content is unchanged. The score is the
// identity distance between temporally-nearest frames - if it reaches the
// device's ~4.6 at a few-frame offset, phase sensitivity is the mechanism, and
// any hash change can be screened on this curve before burning device time.
//
//	HOSTBEAT=/tmp/psync/h900.wav go test -count=1 -v -run TestPhaseAgreement ./syncdsp/
//
// SKIPs without the env var, like the other replay tests.

import (
	"fmt"
	"os"
	"testing"
)

func TestPhaseAgreement(t *testing.T) {
	path := os.Getenv("HOSTBEAT")
	if path == "" {
		t.Skip("HOSTBEAT not set")
	}
	data := readWavPCM16(t, path)

	replay := func(off int) ([]uint32, []bool) {
		var p Pipeline
		p.Init()
		n := (len(data) - off) / Window
		hashes := make([]uint32, n)
		locked := make([]bool, n)
		for f := 0; f < n; f++ {
			s := off + f*Window
			for i := 0; i < Window; i++ {
				p.ProcessSample(data[s+i])
			}
			ev := p.EndFrame()
			hashes[f] = ev.Hash
			locked[f] = p.beat.Locked
		}
		return hashes, locked
	}

	refH, refL := replay(0)
	fmt.Printf("offset  bestShift  identMean  ident<=3  fullMean  n\n")
	for _, off := range []int{0, 24, 48, 80, 120, 160, 240, 320} {
		oh, ol := replay(off)
		base := off / Window // nearest reference frame, whole part
		best := -1
		var bestID, bestFull float64
		var bestN, bestLe3 int
		for sh := -1; sh <= 1; sh++ {
			var sumID, sumFull int64
			n, le3 := 0, 0
			for g := range oh {
				f := g + base + sh
				if f < 0 || f >= len(refH) || !ol[g] || !refL[f] {
					continue
				}
				if g < 960 { // steady state only: skip 15 s of AGC/envRef/lock transient
					continue
				}
				sumID += int64(hashDist(oh[g]&IdentityMask, refH[f]&IdentityMask))
				sumFull += int64(hashDist(oh[g], refH[f]))
				if hashDist(oh[g]&IdentityMask, refH[f]&IdentityMask) <= 3 {
					le3++
				}
				n++
			}
			if n == 0 {
				continue
			}
			id := float64(sumID) / float64(n)
			if best == -1 || id < bestID {
				best, bestID = sh, id
				bestFull = float64(sumFull) / float64(n)
				bestN, bestLe3 = n, le3
			}
		}
		if best == -1 {
			fmt.Printf("%4d smp  -\n", off)
			continue
		}
		fmt.Printf("%4d smp  %+d        %6.2f    %5.1f%%   %7.2f    %d\n",
			off, best, bestID, 100*float64(bestLe3)/float64(bestN), bestFull, bestN)
	}
}
