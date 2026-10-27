package screamrx

import (
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"testing"

	"codeberg.org/maaike328p/datatrans/dsp"
)

// TestCellEye measures, with perfect timing, each tone's magnitude
// conditioned on its current and previous bit, and the bit error rate of an
// ideal two-level and an ideal previous-bit-conditioned (four-level)
// decision. It separates what the channel allows from what the decoder's
// calibration and timing achieve.
//
//	SCEN=spk30cm TRIAL=0 OFF=0 go test ./screamrx -run TestCellEye -v
func TestCellEye(t *testing.T) {
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
	r := rand.New(rand.NewPCG(7, 7))
	data := make([]byte, 698)
	for i := range data {
		data[i] = byte(r.Uint32())
	}
	const nSym = 1500
	stream := cachedStream(data, 0x1234, nSym+10)
	tr := rand.New(rand.NewPCG(uint64(envInt("TRIAL", 0)), 99))
	ch := sc.build(tr)
	off := envInt("OFF", 0)
	direct := ch.paths[0].delay
	rx := newReceiver(ch, stream, direct+float64(off)/fs, 799, tr)
	var d Decoder
	d.Initialize(make([]uint32, fountainWords), 1532, fs, 0)
	raw := make([]uint16, symLen)
	win := make([]int32, symLen+2)
	win[symLen] = 0x7fff_ffff
	mags := make([][nFreq]float64, nSym)
	dc := uint32(30000)
	for n := range nSym {
		rx.fill(raw)
		dc = dsp.CopyWithOffset(raw, win, dc)
		d.analyze(win)
		for k := range nFreq {
			mags[n][k] = float64(d.tones[k].mag)
		}
	}
	var line2, line4 strings.Builder
	tot2, tot4 := 0.0, 0.0
	for k := range nFreq {
		var cells [4][]float64
		for n := 1; n < nSym; n++ {
			b := (stream[n] >> k) & 1
			p := (stream[n-1] >> k) & 1
			cells[2*b+p] = append(cells[2*b+p], mags[n][k])
		}
		var m [4]float64
		for c := range cells {
			m[c], _ = meanSD(cells[c])
		}
		// Ideal two-level: midpoint of the pooled on and off means.
		on0, _ := meanSD(append(append([]float64{}, cells[2]...), cells[3]...))
		off0, _ := meanSD(append(append([]float64{}, cells[0]...), cells[1]...))
		e2, e4, n := 0, 0, 0
		for c := range cells {
			b := c >> 1
			p := c & 1
			for _, v := range cells[c] {
				n++
				if (v > (on0+off0)/2) != (b == 1) {
					e2++
				}
				l0, l1 := m[p], m[2+p]
				if (abs64(v-l1) < abs64(v-l0)) != (b == 1) {
					e4++
				}
			}
		}
		ber2 := 100 * float64(e2) / float64(n)
		ber4 := 100 * float64(e4) / float64(n)
		tot2 += ber2
		tot4 += ber4
		fmt.Fprintf(&line2, " %2.0f", ber2)
		fmt.Fprintf(&line4, " %2.0f", ber4)
		if testing.Verbose() && os.Getenv("CELLS") != "" {
			t.Logf("tone %2d cells off|0 %5.0f off|1 %5.0f on|0 %5.0f on|1 %5.0f", k, m[0], m[1], m[2], m[3])
		}
	}
	t.Logf("BER%% 2-level:%s  (mean %.1f)", line2.String(), tot2/nFreq)
	t.Logf("BER%% 4-level:%s  (mean %.1f)", line4.String(), tot4/nFreq)
}

func abs64(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
