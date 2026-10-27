package syncdsp

import (
	"math"
	"os"
	"sort"
	"strconv"
	"testing"
)

// TestBeatSeg folds, for one 20 s segment (BEATSEG=index), the raw sub-150 Hz
// energy (offline, 4 ms hop), the reference onset envelope and the device's
// bass flux on the reference grid.
func TestBeatSeg(t *testing.T) {
	path := os.Getenv("BEATDIAG")
	seg, _ := strconv.Atoi(os.Getenv("BEATSEG"))
	if path == "" {
		t.Skip()
	}
	src := readPCM16(t, path)
	refT, _ := loadRef(t, path, src)
	env := refOnset(src, 64)
	t0, t1 := float64(seg)*20, float64(seg+1)*20
	if seg < 0 {
		t0, t1 = 20, 1e9
	}
	fold := func(f *[16]float64, tt, w float64) {
		if tt < t0 || tt >= t1 {
			return
		}
		j := sort.SearchFloat64s(refT, tt)
		if j < 1 || j >= len(refT) {
			return
		}
		x := (tt - refT[j-1]) / (refT[j] - refT[j-1])
		f[int(x*16)&15] += w
	}
	// One-pole low-pass at ~150 Hz twice, energy per 64-sample hop.
	var lo, onset, flux [16]float64
	var y1, y2 float64
	a := 1 - math.Exp(-2*math.Pi*150/16000)
	for h := 0; (h+1)*64 <= len(src); h++ {
		var e float64
		for i := h * 64; i < (h+1)*64; i++ {
			y1 += a * (float64(src[i]) - y1)
			y2 += a * (y1 - y2)
			e += y2 * y2
		}
		tt := float64(h*64+32) / 16000
		fold(&lo, tt, e)
		fold(&onset, tt, env[h])
	}
	var p Pipeline
	p.Init()
	win := make([]uint16, Window)
	for n := 0; (n+1)*Window < len(src); n++ {
		for i := range win {
			win[i] = uint16(int32(src[n*Window+i])/4 + benchDC)
		}
		p.Process(win)
		fold(&flux, float64(n+1)*Window/16000, float64(p.bassFlux))
	}
	norm := func(f [16]float64) []int {
		mx := 0.0
		for _, v := range f {
			mx = max(mx, v)
		}
		r := make([]int, 16)
		for i, v := range f {
			r[i] = int(99 * v / mx)
		}
		return r
	}
	t.Logf("seg %d  <150Hz energy %v", seg, norm(lo))
	t.Logf("seg %d  ref onset     %v", seg, norm(onset))
	t.Logf("seg %d  device flux   %v", seg, norm(flux))
}
