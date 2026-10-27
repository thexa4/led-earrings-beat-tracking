package screamrx

import (
	"encoding/binary"
	"math"
	"math/rand/v2"
	"os"
	"testing"
)

// TestDespike measures the impulse filter on a scenario's signal: the share
// of impulses removed and of clean samples changed, and the residual error.
func TestDespike(t *testing.T) {
	for _, name := range []string{"headphoneR", "spk10cmR", "2way30cm"} {
		var sc scenario
		for _, s := range scenarios() {
			if s.name == name {
				sc = s
			}
		}
		r := rand.New(rand.NewPCG(7, 7))
		data := make([]byte, 698)
		for i := range data {
			data[i] = byte(r.Uint32())
		}
		stream := cachedStream(data, 0x1234, 400)
		build := func() *channel { return sc.build(rand.New(rand.NewPCG(3, 99))) }
		chA, chB := build(), build()
		chB.impulses = 0
		ra := newReceiver(chA, stream, 0.5, 799, rand.New(rand.NewPCG(5, 5)))
		rb := newReceiver(chB, stream, 0.5, 799, rand.New(rand.NewPCG(5, 5)))
		var d Decoder
		d.Initialize(make([]uint32, fountainWords), 1532, fs, 0)
		w := make([]int32, symLen)
		clean := make([]int32, symLen)
		hit, imp, falsePos, n := 0, 0, 0, 0
		errSum := 0.0
		for range 300 {
			for i := range w {
				a := ra.sample()
				b := rb.sample()
				w[i] = int32(a) - 30000
				clean[i] = int32(b) - 30000
			}
			before := append([]int32(nil), w...)
			d.despike(w)
			for i := 2; i < symLen-2; i++ {
				isImp := before[i]-clean[i] > 200
				changed := w[i] != before[i]
				n++
				if isImp {
					imp++
					if changed {
						hit++
					}
				} else if changed {
					falsePos++
				}
				e := float64(w[i] - clean[i])
				errSum += e * e
			}
		}
		t.Logf("%-11s impulses %5d caught %5.1f%%, clean changed %5.2f%%, rms error after %.1f codes, resid %d",
			name, imp, 100*float64(hit)/float64(max(imp, 1)), 100*float64(falsePos)/float64(n), math.Sqrt(errSum/float64(n)), d.spikeMed>>4)
	}
}

// TestDespikeWav runs the impulse filter over a recording (REPLAY) in
// 200-sample windows and reports how much it replaced and the rms before and
// after.
func TestDespikeWav(t *testing.T) {
	name := os.Getenv("REPLAY")
	if name == "" {
		t.Skip("set REPLAY")
	}
	x := readWav(t, name)
	var d Decoder
	d.Initialize(make([]uint32, fountainWords), 1532, fs, 0)
	w := make([]int32, symLen)
	var s0, s1 float64
	n := 0
	for p := 0; p+symLen <= len(x); p += symLen {
		for i := range w {
			w[i] = int32(x[p+i])
			s0 += x[p+i] * x[p+i]
		}
		d.despike(w)
		for i := range w {
			s1 += float64(w[i]) * float64(w[i])
			x[p+i] = float64(w[i])
		}
		n += symLen
	}
	if out := os.Getenv("OUT"); out != "" {
		b := make([]byte, 2*len(x))
		for i, v := range x {
			binary.LittleEndian.PutUint16(b[2*i:], uint16(int16(v)))
		}
		os.WriteFile(out, b, 0o644)
	}
	t.Logf("%s: replaced %.3f%% of samples, rms %.1f -> %.1f, final resid %d codes",
		name, 100*float64(d.Stats.Spikes)/float64(n), math.Sqrt(s0/float64(n)), math.Sqrt(s1/float64(n)), d.spikeMed>>4/6)
}
