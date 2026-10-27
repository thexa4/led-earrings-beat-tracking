package screamrx

import (
	"fmt"
	"math"
	"math/bits"
	"math/rand/v2"
	"os"
	"testing"

	"codeberg.org/maaike328p/datatrans"
	"codeberg.org/maaike328p/datatrans/dsp"
)

// TestDiag runs one scenario with the new decoder and reports what went
// wrong: bit errors per tone against the known stream, the true timing
// error of each window, and the decoder's counters.
//
//	SCEN=spk30cm TRIAL=0 CLK=-0.005 go test ./screamrx -run TestDiag -v
func TestDiag(t *testing.T) {
	name := os.Getenv("SCEN")
	if name == "" {
		t.Skip("set SCEN")
	}
	var sc scenario
	for _, s := range scenarios() {
		if s.name == name {
			sc = s
		}
	}
	trial := envInt("TRIAL", 0)
	clk := 0.0
	fmt.Sscan(os.Getenv("CLK"), &clk)
	r := rand.New(rand.NewPCG(7, 7))
	data := make([]byte, envInt("SIZE", 698))
	for i := range data {
		data[i] = byte(r.Uint32())
	}
	stream := cachedStream(data, 0x1234, 90*50)
	tr := rand.New(rand.NewPCG(uint64(trial), 99))
	ch := sc.build(tr)
	ch.clockErr = clk
	start := tr.Float64() * 2.5
	dec := newRx(16, 799)
	rx := newReceiver(ch, stream, start, 799, tr)
	raw := make([]uint16, symLen)
	win := make([]int32, symLen+2)
	win[symLen] = 0x7fff_ffff
	rx.fill(raw)
	dc := dsp.CopyWithOffset(raw, win, 0)
	direct := ch.paths[0].delay
	var errs [nFreq]int
	nData := 0
	var timErr []float64
	for n := range 60 * 50 {
		t0 := rx.t
		rx.fill(raw)
		dc = dsp.CopyWithOffset(raw, win, dc)
		st := dec.process(win)
		// Window start relative to the direct arrival's symbol grid.
		pos := (t0 - direct) * datatrans.SymbolsPerSecond
		sym := int(math.Round(pos))
		frac := (pos - float64(sym)) * symLen
		if dec.d.LastData && sym >= 0 && sym < len(stream) {
			e := dec.d.LastRaw ^ stream[sym]
			for k := range nFreq {
				errs[k] += int(e>>k) & 1
			}
			nData++
			timErr = append(timErr, frac)
		}
		if a := dec.adjust(); a != 0 {
			rx.skip(a)
		}
		if tt := dec.trim(rx.trim); tt != rx.trim {
			rx.setTrim(tt)
		}
		if st == StatusReceived {
			t.Logf("received after %.1fs", float64(n)/50)
			break
		}
		if st < 0 {
			t.Logf("%.1fs: status %d", float64(n)/50, st)
			dec.reset()
		}
	}
	tot := 0
	for _, e := range errs {
		tot += e
	}
	t.Logf("stats %+v", dec.d.Stats)
	if nData > 0 {
		t.Logf("data windows %d, BER %.3f%%, per tone (%%): %v", nData, 100*float64(tot)/float64(nData*nFreq), pct(errs[:], nData))
		mean, sd := meanSD(timErr)
		t.Logf("timing error vs direct path: mean %.1f sd %.1f samples (last %v)", mean, sd, timErr[max(0, len(timErr)-5):])
	}
	t.Logf("levels [off on] %v", dec.d.lv)
	_ = bits.OnesCount32
}

func pct(e []int, n int) []int {
	out := make([]int, len(e))
	for i, v := range e {
		out[i] = int(math.Round(100 * float64(v) / float64(n)))
	}
	return out
}

func meanSD(x []float64) (float64, float64) {
	m := 0.0
	for _, v := range x {
		m += v
	}
	m /= float64(len(x))
	s := 0.0
	for _, v := range x {
		s += (v - m) * (v - m)
	}
	return m, math.Sqrt(s / float64(len(x)))
}
