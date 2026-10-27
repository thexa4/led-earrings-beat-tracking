package syncdsp

import (
	"fmt"
	"math"
	"os"
	"sort"
	"testing"
)

// TestBeatDiag (BEATDIAG=file.wav, BEATREFDIR as for TestBeatEval): the onset
// envelope, the device's bass onsets and flux and its beats folded on the
// reference grid, per-20 s phase, and
// the device's period relative to the reference.
func TestBeatDiag(t *testing.T) {
	path := os.Getenv("BEATDIAG")
	if path == "" {
		t.Skip()
	}
	src := readPCM16(t, path)
	refT, refP := loadRef(t, path, src)
	env := refOnset(src, 64)
	var fold [16]float64
	for i := 1; i < len(refT); i++ {
		for k := range 16 {
			x := refT[i-1] + (refT[i]-refT[i-1])*float64(k)/16
			fold[k] += env[int(x*250)]
		}
	}
	t.Logf("onset folded on the reference grid (bin 0 = beat): %.0f", fold)
	var p Pipeline
	p.Init()
	win := make([]uint16, Window)
	hist := map[string]int{}
	var errs, errsI []float64
	var fOn, fFlux, fBeat [16]float64
	fold16 := func(f *[16]float64, tt, w float64) {
		j := sort.SearchFloat64s(refT, tt)
		if j < 1 || j >= len(refT) {
			return
		}
		x := (tt - refT[j-1]) / (refT[j] - refT[j-1])
		f[int(x*16+0.5)&15] += w
	}
	var pb, pb2 int32
	var segC, segS, segN []float64
	var feats [6][16]float64
	var pe, pe2 [NBands]int32
	for n := 0; (n+1)*Window < len(src); n++ {
		for i := range win {
			win[i] = uint16(int32(src[n*Window+i])/4 + benchDC)
		}
		ev := p.Process(win)
		tt := float64(n+1) * Window / 16000
		if tt > 20 {
			if p.bassOnset {
				fold16(&fOn, tt, 1)
			}
			if fl := p.prevBass - pb2; fl > 0 {
				fold16(&fFlux, tt, float64(fl))
			}
			if ev.Beat && ev.Locked {
				fold16(&fBeat, tt, 1)
				j := sort.SearchFloat64s(refT, tt)
				if j >= 1 && j < len(refT) {
					x := (tt - refT[j-1]) / (refT[j] - refT[j-1])
					w := int(tt / 20)
					for len(segC) <= w {
						segC = append(segC, 0)
						segS = append(segS, 0)
						segN = append(segN, 0)
					}
					segC[w] += math.Cos(2 * math.Pi * x)
					segS[w] += math.Sin(2 * math.Pi * x)
					segN[w]++
				}
			}
		}
		pb2, pb = pb, p.prevBass
		var e [NBands]int32
		for i, m := range p.energies {
			e[i] = log2e(m)
		}
		if tt > 20 {
			var f [6]int32
			for i := range e {
				d1 := max(e[i]-pe[i], 0)
				d2 := max(e[i]-pe2[i], 0)
				f[0] += d1
				f[1] += d2
				if i >= NBands-2 {
					f[2] += d1
				}
				if i >= 5 {
					f[3] += d2
				}
				if i <= 2 {
					f[4] += d1
				}
			}
			f[5] = max(e[NBands-1]-pe[NBands-1], 0)
			for k := range f {
				fold16(&feats[k], tt, float64(f[k]))
			}
		}
		pe2, pe = pe, e
		if !ev.Locked || tt < 20 {
			continue
		}
		j := min(sort.SearchFloat64s(refT, tt), len(refP)-1)
		r := float64(p.beat.Period) / 64 / refP[j]
		if rq := float64(p.beat.perQ8) / 256 / 64 / refP[j]; math.Abs(rq-1) < 0.04 {
			errs = append(errs, math.Abs(rq-1))
			errsI = append(errsI, math.Abs(r-1))
		}
		k := "other"
		for _, c := range []struct {
			name string
			v    float64
		}{{"1/2", 0.5}, {"2/3", 2.0 / 3}, {"3/4", 0.75}, {"1", 1}, {"4/3", 4.0 / 3}, {"3/2", 1.5}, {"2", 2}} {
			if math.Abs(r/c.v-1) < 0.04 {
				k = c.name
			}
		}
		hist[k]++
	}
	t.Logf("bass onsets on grid: %.0f", fOn)
	t.Logf("bass flux on grid:   %.0f", fFlux)
	t.Logf("device beats on grid: %.0f", fBeat)
	for k, n := range []string{"all d1", "all d2", "low2 d1", "low3 d2", "high3 d1", "bass d1"} {
		mx, sum := 0.0, 0.0
		for _, v := range feats[k] {
			mx = max(mx, v)
			sum += v
		}
		t.Logf("%-8s peak/mean %.2f  %.0f", n, mx/(sum/16), feats[k])
	}
	sort.Float64s(errs)
	sort.Float64s(errsI)
	if len(errs) > 0 {
		q := func(e []float64, f float64) float64 { return 100 * e[int(f*float64(len(e)-1))] }
		t.Logf("period error %%: Q8 p25 %.2f p50 %.2f p75 %.2f   integer p25 %.2f p50 %.2f p75 %.2f",
			q(errs, .25), q(errs, .5), q(errs, .75), q(errsI, .25), q(errsI, .5), q(errsI, .75))
	}
	var sb []string
	for w := range segN {
		if segN[w] == 0 {
			continue
		}
		ph := math.Atan2(segS[w], segC[w]) / (2 * math.Pi)
		if ph < 0 {
			ph++
		}
		r := math.Hypot(segS[w], segC[w]) / segN[w]
		sb = append(sb, fmt.Sprintf("%3.0f%%/%.1f", ph*100, r))
	}
	t.Logf("20 s segments, mean phase/resultant: %v", sb)
	t.Logf("period ratio device/ref: %v", hist)
}
