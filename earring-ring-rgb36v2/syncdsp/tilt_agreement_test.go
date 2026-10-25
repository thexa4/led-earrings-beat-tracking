package syncdsp

// One-off experiment (2026-10-23): is the hot field's disagreement between
// two earrings caused by the threshold being absolute-in-spectrum (cur vs
// max-HotFloorQ8) rather than relative-to-the-fitted-line (dev)?
//
// Two earrings differ in the same way as two microphones with opposite EQ
// tilts: the mic is closer to the speaker cone on one side, the ring body
// shadows the other, and the room adds a standing wave. Level the AGC already
// cancels; a *tilt* it cannot. This test replays the same file through
// pipelines whose inputs carry opposite tilts and scores the hot field before
// (cur vs max) and after (dev vs margin) the candidate change, plus the shape
// field as the control we know is tilt-robust (measured <=3: 90-99% on
// device).
//
//	PODCAST=/tmp/psync/h900.wav go test -count=1 -v -run TestTiltAgreement ./syncdsp/
//
// SKIPs without the env var.

import (
	"fmt"
	"os"
	"testing"
)

// tilt returns a copy of data with a first-order high-shelf: below ~300 Hz
// unchanged, above it scaled by (1+k). k=+0.5 ~ +3 dB on the highs, k=-0.5
// the opposite tilt - roughly the spread two earring positions measured
// against a speaker. Operates on the signed signal, re-biased on output.
func tilt(data []uint16, k float64) []uint16 {
	out := make([]uint16, len(data))
	var lp float64
	const a = 1.0 / 8 // ~318 Hz pole at 16 kHz
	for i, v := range data {
		x := float64(int16(v))
		lp += a * (x - lp)
		y := lp + (1+k)*(x-lp)
		// clip like the ADC front end
		out[i] = uint16(int16(maxF(minF(y, 32700), -32700)))
	}
	return out
}

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// replay returns per-frame (hash, devHot bits for each margin).
func replayTilt(t *testing.T, data []uint16) (hashes []uint32, devHot [][NBands]int32) {
	var p Pipeline
	p.Init()
	n := len(data) / Window
	hashes = make([]uint32, n)
	devHot = make([][NBands]int32, n)
	f := 0
	for s := 0; s+Window <= len(data); s += Window {
		for i := 0; i < Window; i++ {
			p.ProcessSample(data[s+i])
		}
		ev := p.EndFrame()
		hashes[f] = ev.Hash
		devHot[f] = spectralDevs(&p.smoothSnap)
		f++
	}
	return hashes[:f], devHot[:f]
}

func hotFromDev(dev *[NBands]int32, margin int32) uint32 {
	var w uint32
	for b := 0; b < NBands; b++ {
		if dev[b] >= margin {
			w |= 1 << uint(ContrastShift+b)
		}
	}
	return w
}

func popcnt(x uint32) int {
	c := 0
	for ; x != 0; x &= x - 1 {
		c++
	}
	return c
}

func TestTiltAgreement(t *testing.T) {
	path := os.Getenv("PODCAST")
	if path == "" {
		t.Skip("PODCAST not set")
	}
	data := readWavPCM16(t, path)

	ref, refDev := replayTilt(t, data)
	variants := map[string][]uint16{
		"tilt+3dB": tilt(data, 0.5),
		"tilt-3dB": tilt(data, -0.5),
		"tilt+6dB": tilt(data, 1.0),
		"tilt-6dB": tilt(data, -1.0),
	}

	margins := []int32{0, 85, 171, 256} // 0 / 1 / 2 / 3 dB above the line

	fmt.Printf("%-10s %-12s %8s %8s %8s\n", "variant", "field", "mean", "<=3", "exact")
	for name, vdata := range variants {
		vh, vdev := replayTilt(t, vdata)
		n := min(len(ref), len(vh))
		fields := map[string]*tiltAcc{"shape": {}, "hot(cur)": {}}
		for _, m := range margins {
			fields[fmt.Sprintf("hot(dev>=%d)", m)] = &tiltAcc{}
		}
		for i := 0; i < n; i++ {
			a, b := ref[i], vh[i]
			// shape: bits 8..21
			d := ((a ^ b) >> ShapeShift) & ((1 << (2 * (NBands - 1))) - 1)
			fields["shape"].add(popcnt(d))
			// hot(cur): bits ContrastShift..ContrastShift+7 as emitted
			mask := uint32(NBands-1) << ContrastShift
			fields["hot(cur)"].add(popcnt((a ^ b) & mask))
			// hot(dev) candidates: recompute both sides from dev
			for _, m := range margins {
				k := fmt.Sprintf("hot(dev>=%d)", m)
				x := hotFromDev(&refDev[i], m) ^ hotFromDev(&vdev[i], m)
				fields[k].add(popcnt(x & mask))
			}
		}
		for _, k := range []string{"shape", "hot(cur)", "hot(dev>=0)", "hot(dev>=85)",
			"hot(dev>=171)", "hot(dev>=256)"} {
			f := fields[k]
			fmt.Printf("%-10s %-12s %8.2f %7.1f%% %7.1f%%\n", name, k,
				float64(f.sum)/float64(f.cnt),
				100*float64(f.le3)/float64(f.cnt),
				100*float64(f.eq)/float64(f.cnt))
		}
		// degeneracy: how many distinct hot words does each side produce?
		distinct := func(get func(int) uint32) int {
			s := map[uint32]bool{}
			for i := 0; i < n; i++ {
				s[get(i)] = true
			}
			return len(s)
		}
		mask := uint32(NBands-1) << ContrastShift
		fmt.Printf("%-10s distinct hot words: cur=%d dev>=85: %d\n\n", name,
			distinct(func(i int) uint32 { return ref[i] & mask }),
			distinct(func(i int) uint32 { return hotFromDev(&refDev[i], 85) }))
	}
}

type tiltAcc struct {
	sum, le3, eq, cnt int
}

func (a *tiltAcc) add(d int) {
	a.sum += d
	if d <= 3 {
		a.le3++
	}
	if d == 0 {
		a.eq++
	}
	a.cnt++
}
