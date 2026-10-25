package syncdsp

// One-off experiment (2026-10-23): why does the hot field disagree, and which
// threshold form is robust?
//
// Measured on the pair rig (/tmp/psync/p2, p3):
//   cross-device:  hot mean 4.6, <=3: 25%   (shape <=3: 90%)
//   same device, two plays of the same file: hot mean 2.31, <=3: 74%, pattern 39%
// A +-6 dB EQ tilt moves hot by only 0.8 bits (TestTiltAgreement), so tilt is
// NOT the mechanism. Two candidates remain, both cheap to screen here:
//
//   noise - the AGC raises a quiet device by up to 64x, amplifying its own
//           noise floor; hot's threshold sits at max-4.5 dB, i.e. inside a
//           dense cluster of band energies, so a little noise flips bits.
//   notch - a room mode or the ring shadowing a mic is not a tilt but a
//           single-band peak/null of up to ~10 dB. It moves `max` itself, and
//           max sets the floor for ALL eight bits at once - which is what the
//           uniform 60-80% per-bit flip rate looks like.
//
// Screened variants (recomputed per frame from each side's own smoothed
// snapshot; hysteresis keeps per-side sequential state):
//   hot(cur)   current field: band within HotFloorQ8 of the max
//   hot(dev)   compare against the fitted spectral line instead of the max
//   hot(med)   floor from the MEDIAN band (a single outlier cannot move it)
//   hot(hyst)  hot(cur) with per-bit hysteresis (margin + dwell frames)
//
//	PODCAST=/tmp/psync/h900.wav go test -count=1 -v -run TestHotRobust ./syncdsp/

import (
	"fmt"
	"math"
	"math/rand"
	"os"
	"testing"
)

// perturb returns a device-like variant: white noise of the given RMS plus an
// optional resonant peak/null at f0 (relative bandwidth bw, gain g applied to
// the bandpassed component).
func perturb(data []uint16, noise, f0, g, bw float64, seed int64) []uint16 {
	rng := rand.New(rand.NewSource(seed))
	a1, a2 := 0.0, 0.0
	if f0 > 0 {
		w := 2 * math.Pi * f0 / 16000
		a1 = 2 * (1 - bw) * math.Cos(w)
		a2 = -(1 - bw) * (1 - bw)
	}
	var y1, y2 float64
	out := make([]uint16, len(data))
	for i, v := range data {
		x := float64(int16(v))
		bp := x - a1*y1 - a2*y2
		y2, y1 = y1, bp
		y := x + g*bp
		if noise > 0 {
			y += noise * rng.NormFloat64()
		}
		if y > 32700 {
			y = 32700
		} else if y < -32700 {
			y = -32700
		}
		out[i] = uint16(int16(y))
	}
	return out
}

// hot variants -------------------------------------------------------------

const (
	hystMargin = 43 // 0.5 dB either side of the threshold
	hystFrames = 3  // held for 3 frames (47 ms)
)

type hotState struct {
	word   uint32
	since  [NBands]int8
	inited bool
}

func hotHyst(s *hotState, cur *[NBands]int32) uint32 {
	max := cur[0]
	for _, v := range cur[1:] {
		if v > max {
			max = v
		}
	}
	floor := max - HotFloorQ8
	for b := 0; b < NBands; b++ {
		bit := uint32(1) << uint(ContrastShift+b)
		if !s.inited {
			if cur[b] >= floor {
				s.word |= bit
			}
			continue
		}
		on := s.word&bit != 0
		switch {
		case !on && cur[b] >= floor+hystMargin:
			s.since[b]++
			if s.since[b] >= hystFrames {
				s.word |= bit
				s.since[b] = 0
			}
		case on && cur[b] < floor-hystMargin:
			s.since[b]++
			if s.since[b] >= hystFrames {
				s.word &^= bit
				s.since[b] = 0
			}
		default:
			s.since[b] = 0
		}
	}
	s.inited = true
	return s.word
}

// hotFromMedian floors at the median band + half the hot window: unlike the
// max, one loud or dead band cannot drag the threshold.
func hotFromMedian(cur *[NBands]int32) uint32 {
	s := *cur
	for i := 1; i < NBands; i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
	floor := s[NBands/2] + HotFloorQ8/2
	var w uint32
	for b := 0; b < NBands; b++ {
		if cur[b] >= floor {
			w |= 1 << uint(ContrastShift+b)
		}
	}
	return w
}

func hotFromDevAt(cur *[NBands]int32, margin int32) uint32 {
	dev := spectralDevs(cur)
	var w uint32
	for b := 0; b < NBands; b++ {
		if dev[b] >= margin {
			w |= 1 << uint(ContrastShift+b)
		}
	}
	return w
}

// scoring ------------------------------------------------------------------

type frame struct {
	hash   uint32
	smooth [NBands]int32
	pat    int32
}

func runFrames(data []uint16) []frame {
	var p Pipeline
	p.Init()
	out := make([]frame, 0, len(data)/Window)
	for s := 0; s+Window <= len(data); s += Window {
		for i := 0; i < Window; i++ {
			p.ProcessSample(data[s+i])
		}
		ev := p.EndFrame()
		out = append(out, frame{hash: ev.Hash, smooth: p.smoothSnap, pat: ev.Pattern})
	}
	return out
}

func TestHotRobust(t *testing.T) {
	path := os.Getenv("PODCAST")
	if path == "" {
		t.Skip("PODCAST not set")
	}
	data := readWavPCM16(t, path)
	ref := runFrames(data)
	mask := uint32(NBands-1) << ContrastShift

	variants := []struct {
		name             string
		noise, f0, g, bw float64
	}{
		{"noise20", 20, 0, 0, 0},
		{"noise60", 60, 0, 0, 0},
		{"noise150", 150, 0, 0, 0},
		{"null120", 0, 120, -0.7, 0.35},
		{"peak250", 0, 250, 0.7, 0.35},
		{"noise60+null180", 60, 180, -0.7, 0.35},
	}
	names := []string{"shape", "hot(cur)", "hot(dev)", "hot(med)", "hot(hyst)"}

	fmt.Printf("%-17s", "variant")
	for range names {
		fmt.Printf(" %6s/%-5s", "mean", "<=3")
	}
	fmt.Println("  pattern=")
	for _, v := range variants {
		pf := runFrames(perturb(data, v.noise, v.f0, v.g, v.bw, 7))
		n := min(len(ref), len(pf))
		acc := map[string]*tiltAcc{}
		for _, k := range names {
			acc[k] = &tiltAcc{}
		}
		var s1, s2 hotState
		patEq := 0
		for i := 0; i < n; i++ {
			r, o := ref[i], pf[i]
			acc["shape"].add(popcnt(((r.hash ^ o.hash) >> ShapeShift) &
				((1 << (2 * (NBands - 1))) - 1)))
			acc["hot(cur)"].add(popcnt((r.hash ^ o.hash) & mask))
			acc["hot(dev)"].add(popcnt((hotFromDevAt(&r.smooth, 85) ^
				hotFromDevAt(&o.smooth, 85)) & mask))
			acc["hot(med)"].add(popcnt((hotFromMedian(&r.smooth) ^
				hotFromMedian(&o.smooth)) & mask))
			acc["hot(hyst)"].add(popcnt((hotHyst(&s1, &r.smooth) ^
				hotHyst(&s2, &o.smooth)) & mask))
			if r.pat == o.pat {
				patEq++
			}
		}
		fmt.Printf("%-17s", v.name)
		for _, k := range names {
			f := acc[k]
			fmt.Printf(" %6.2f/%3.0f%%  ", float64(f.sum)/float64(n),
				100*float64(f.le3)/float64(n))
		}
		fmt.Printf(" %3.0f%%\n", 100*float64(patEq)/float64(n))
	}
	// sanity: identical signal against itself must agree everywhere
	self := runFrames(data)
	n := min(len(ref), len(self))
	bad := 0
	for i := 0; i < n; i++ {
		if ref[i].hash != self[i].hash {
			bad++
		}
	}
	fmt.Printf("determinism check: %d/%d frames differ (want 0)\n", bad, n)
}
