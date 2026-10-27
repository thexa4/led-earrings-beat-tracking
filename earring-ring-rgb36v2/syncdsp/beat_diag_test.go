package syncdsp

import (
	"math"
	"os"
	"sort"
	"testing"
)

// TestBeatDiag: reference sanity (onset envelope folded on the reference grid)
// and the distribution of the device's period relative to the reference.
func TestBeatDiag(t *testing.T) {
	path := os.Getenv("BEATDIAG")
	if path == "" {
		t.Skip()
	}
	src := readPCM16(t, path)
	refT, refP := refBeats(src)
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
	for n := 0; (n+1)*Window < len(src); n++ {
		for i := range win {
			win[i] = uint16(int32(src[n*Window+i])/4 + benchDC)
		}
		ev := p.Process(win)
		tt := float64(n+1) * Window / 16000
		if !ev.Locked || tt < 20 {
			continue
		}
		j := min(sort.SearchFloat64s(refT, tt), len(refP)-1)
		r := float64(p.beat.Period) / 64 / refP[j]
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
	t.Logf("period ratio device/ref: %v", hist)
}
