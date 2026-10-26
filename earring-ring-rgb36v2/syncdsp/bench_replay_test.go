package syncdsp

// Pattern selection replayed over a real two-earring bench capture.
//
// tools/paircapture.py --state (debug branch) records, per side, the telemetry ring
// (one record per analysis frame: the commit triggers and the pattern shown) and,
// about five times a second, the selector's inputs and state (the smoothed bands,
// cent, centGapQ8, centAvg, centDev). This rebuilds a frameRec per analysis frame from
// those - flags exact, bands interpolated between polls - and replays candidate
// selectors on both earrings' real spectra, paired on host time.
//
//	BENCH_CAP=/tmp/psync/cap1 go test -run TestBench -v ./syncdsp/
//
// Before scoring anything it replays the selector the capture firmware was running,
// seeded from that device's own first state sample, and reports how often the
// replay shows the pattern the device actually showed: the check that the rebuilt
// records are what the selector saw.

import (
	"encoding/csv"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
)

type benchSide struct {
	recs    []frameRec
	shown   []int8 // pattern in the device's own telemetry, per rec
	frame0  int32
	a, b    float64 // host time = a + b*frame
	st      selState
	missing int // frames with no telemetry record
}

func readCSV(t testing.TB, path string) (map[string]int, [][]string) {
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	col := map[string]int{}
	for i, h := range rows[0] {
		col[h] = i
	}
	return col, rows[1:]
}

func loadBenchSide(t testing.TB, dir, side string) benchSide {
	sc, srows := readCSV(t, filepath.Join(dir, "state_"+side+".csv"))
	geti := func(row []string, c map[string]int, name string) int64 {
		v, err := strconv.ParseInt(row[c[name]], 10, 64)
		if err != nil {
			t.Fatalf("%s: %s=%q: %v", side, name, row[c[name]], err)
		}
		return v
	}
	type spoll struct {
		t                   float64
		frame               int32
		cent, gap, avg, dev int32
		band                [NBands]int32
	}
	var polls []spoll
	for _, row := range srows {
		var p spoll
		p.t, _ = strconv.ParseFloat(row[sc["t"]], 64)
		p.frame = int32(geti(row, sc, "frame"))
		p.cent = int32(geti(row, sc, "cent"))
		p.gap = int32(geti(row, sc, "gapQ8"))
		p.avg = int32(geti(row, sc, "centAvg"))
		p.dev = int32(geti(row, sc, "centDev"))
		for i := range NBands {
			p.band[i] = int32(geti(row, sc, fmt.Sprintf("b%d", i)))
		}
		if len(polls) > 0 && p.frame <= polls[len(polls)-1].frame {
			continue
		}
		polls = append(polls, p)
	}

	type tel struct {
		bar, anchor, pushed, returned bool
		pattern                       int8
	}
	tc, trows := readCSV(t, filepath.Join(dir, side+".csv"))
	tels := map[int32]tel{}
	for _, row := range trows {
		if row[tc["kind"]] != "T" {
			continue
		}
		seq := int32(geti(row, tc, "seq"))
		b := func(name string) bool { return row[tc[name]] == "True" }
		tels[seq] = tel{bar: b("bar"), anchor: b("anchor"), pushed: b("pushed"),
			returned: b("returned"), pattern: int8(geti(row, tc, "pattern"))}
	}

	var s benchSide
	// Least-squares host time per frame: both sides are then paired on this clock.
	var n, sx, sy, sxx, sxy float64
	for _, p := range polls {
		x := float64(p.frame)
		n++
		sx += x
		sy += p.t
		sxx += x * x
		sxy += x * p.t
	}
	s.b = (n*sxy - sx*sy) / (n*sxx - sx*sx)
	s.a = (sy - s.b*sx) / n

	first, last := polls[0], polls[len(polls)-1]
	s.frame0 = first.frame
	s.st = selState{frame: first.frame, centQ8: first.cent << 8, avgQ8: first.avg << 8,
		devQ8: first.dev << 8, gapQ8: first.gap, last: int32(tels[first.frame].pattern)}
	k := 0
	for f := first.frame; f <= last.frame; f++ {
		for polls[k+1].frame < f {
			k++
		}
		p0, p1 := polls[k], polls[k+1]
		var r frameRec
		w := float64(f-p0.frame) / float64(p1.frame-p0.frame)
		for i := range NBands {
			r.smooth[i] = int32(math.Round(float64(p0.band[i]) + w*float64(p1.band[i]-p0.band[i])))
		}
		tr, ok := tels[f]
		if !ok {
			s.missing++
			if len(s.shown) > 0 {
				tr.pattern = s.shown[len(s.shown)-1]
			}
		}
		r.decide, r.bar, r.returned, r.anchor = tr.pushed, tr.bar, tr.returned, tr.anchor
		r.pattern = int32(tr.pattern)
		s.recs = append(s.recs, r)
		s.shown = append(s.shown, tr.pattern)
	}
	return s
}

// benchPair maps each L rec onto the R rec at the same host time plus lagFrames.
func benchPair(L, R benchSide, lagFrames int) []int {
	idx := make([]int, len(L.recs))
	for i := range L.recs {
		tt := L.a + L.b*float64(L.frame0+int32(i))
		j := int(math.Round((tt-R.a)/R.b)) - int(R.frame0) + lagFrames
		if j < 0 || j >= len(R.recs) {
			j = -1
		}
		idx[i] = j
	}
	return idx
}

// alignLag cross-correlates the two sides' band-weighted trajectories over
// +-2 s of residual lag. The host clock pairs them already; a peak far from zero
// means it does not.
func alignLag(L, R benchSide, w [NBands]int32) (int, float64) {
	feat := func(r frameRec) float64 {
		c := 0.0
		for i, v := range r.smooth {
			c += float64(v * w[i])
		}
		return c
	}
	diff := func(s benchSide) []float64 {
		out := make([]float64, len(s.recs))
		for i := 31; i < len(s.recs); i++ {
			out[i] = feat(s.recs[i]) - feat(s.recs[i-31])
		}
		return out
	}
	dl, dr := diff(L), diff(R)
	best, bestC := 0, -2.0
	for lag := -125; lag <= 125; lag += 2 {
		idx := benchPair(L, R, lag)
		var sxy, sxx, syy, sx, sy, n float64
		for i, j := range idx {
			if j < 0 || i < 1000 {
				continue
			}
			x, y := dl[i], dr[j]
			sx += x
			sy += y
			sxx += x * x
			syy += y * y
			sxy += x * y
			n++
		}
		c := (sxy - sx*sy/n) / math.Sqrt((sxx-sx*sx/n)*(syy-sy*sy/n))
		if c > bestC {
			best, bestC = lag, c
		}
	}
	return best, bestC
}

func benchScore(pl, pr []int8, idx []int, from, to int) score {
	var A, B []int8
	for i := from; i < to && i < len(idx); i++ {
		if idx[i] < 0 {
			continue
		}
		A = append(A, pl[i])
		B = append(B, pr[idx[i]])
	}
	return scoreAligned(A, B)
}

func loadBench(t *testing.T) (benchSide, benchSide, []int) {
	dir := os.Getenv("BENCH_CAP")
	if dir == "" {
		t.Skip("BENCH_CAP not set")
	}
	L, R := loadBenchSide(t, dir, "L"), loadBenchSide(t, dir, "R")
	lag, c := alignLag(L, R, [NBands]int32{-7, -3, 0, 0, 3, 4, 0, 0})
	t.Logf("L: %d frames (%.1f min), %d without telemetry; R: %d frames, %d without; residual lag %d frames (r=%.2f)",
		len(L.recs), float64(len(L.recs))/fps/60, L.missing, len(R.recs), R.missing, lag, c)
	if lag < -15 || lag > 15 {
		t.Fatalf("host-time pairing is off by %d frames", lag)
	}
	return L, R, benchPair(L, R, 0)
}

// debugSelector is the selector the debug-branch capture firmware runs.
func debugSelector() selParams {
	sp := shipped()
	sp.weights = [NBands]int32{-7, 0, 0, 0, 7, 0, 0, 0}
	sp.centShift, sp.scaleShift, sp.gapShift = 11, 11, 11
	sp.cuts = [NumPatterns - 1]int32{-150, 0, 150}
	sp.maxHold = 250
	return sp
}

func TestBenchCapture(t *testing.T) {
	L, R, idx := loadBench(t)
	for _, s := range []struct {
		name string
		b    benchSide
	}{{"L", L}, {"R", R}} {
		pats, _ := replayFrom(s.b.recs, debugSelector(), &s.b.st)
		same := 0
		for i := range pats {
			if pats[i] == s.b.shown[i] {
				same++
			}
		}
		t.Logf("%s: replay of the device's own selector matches what it showed on %.2f%% of frames",
			s.name, 100*float64(same)/float64(len(pats)))
	}
	n := len(idx)
	t.Logf("as shown by the devices:  %s", benchScore(L.shown, R.shown, idx, 0, n))
	pl, _ := replay(L.recs, debugSelector())
	pr, _ := replay(R.recs, debugSelector())
	t.Logf("debug selector, fresh:    %s", benchScore(pl, pr, idx, 0, n))
	pl, _ = replay(L.recs, shipped())
	pr, _ = replay(R.recs, shipped())
	t.Logf("shipped, fresh:           %s", benchScore(pl, pr, idx, 0, n))
}

// TestBenchSweep fits on the first half of the capture and reports the second.
func TestBenchSweep(t *testing.T) {
	L, R, idx := loadBench(t)
	n := len(idx)
	weightSets := [][NBands]int32{
		{-7, 0, 0, 0, 7, 0, 0, 0},
		{-7, 0, 0, 0, 0, 7, 0, 0},
		{-7, 0, 0, 0, 4, 3, 0, 0},
		{-7, -3, 0, 0, 3, 7, 0, 0},
		{-7, -3, -1, 1, 3, 7, 0, 0},
		{-7, -4, -2, 0, 2, 4, 7, 0},
		{-7, -5, -3, -1, 1, 3, 5, 7},
		{-7, -3, 0, 0, 3, 4, 3, 0},
		{-7, -4, 0, 0, 4, 4, 3, 0},
		{-7, -5, 0, 0, 4, 4, 4, 0},
		{-7, -2, 0, 0, 2, 4, 3, 0},
	}
	ok := func(s score) bool {
		return s.minOcc >= 0.18 && s.chgA >= 0.5 && s.chgB >= 0.5 && s.chgA <= 8 && s.chgB <= 8 && s.shift <= 0.15
	}
	type result struct {
		sp       selParams
		fit, out score
	}
	var results []result
	for _, w := range weightSets {
		for cs := int32(8); cs <= 11; cs++ {
			for ss := int32(9); ss <= 12; ss++ {
				for gs := int32(7); gs <= 11; gs++ {
					sp := shipped()
					sp.weights, sp.centShift, sp.scaleShift, sp.gapShift = w, cs, ss, gs
					for _, k := range []int32{100, 150, 216, 256} {
						sp.cuts = [NumPatterns - 1]int32{-k, 0, k}
						pl, _ := replay(L.recs, sp)
						pr, _ := replay(R.recs, sp)
						results = append(results, result{sp, benchScore(pl, pr, idx, 0, n/2), benchScore(pl, pr, idx, n/2, n)})
					}
				}
			}
		}
	}
	sort.Slice(results, func(i, j int) bool {
		a, b := results[i], results[j]
		if ok(a.fit) != ok(b.fit) {
			return ok(a.fit)
		}
		return a.fit.kappa > b.fit.kappa
	})
	report := func(label string, r result) {
		t.Logf("%s ok=%v %s", label, ok(r.fit), r.sp)
		t.Logf("    fit      %s", r.fit)
		t.Logf("    held-out %s", r.out)
	}
	for _, r := range results {
		if r.sp == shipped() {
			report("shipped", r)
		}
	}
	for i := 0; i < 10; i++ {
		report(fmt.Sprintf("#%d", i+1), results[i])
	}
	for _, w := range weightSets {
		for _, r := range results {
			if r.sp.weights == w {
				report("best for weights", r)
				break
			}
		}
	}
	for _, want := range []selParams{
		func() selParams {
			s := shipped()
			s.weights = [NBands]int32{-7, -4, 0, 0, 4, 4, 3, 0}
			s.centShift, s.scaleShift, s.gapShift = 9, 11, 9
			s.cuts = [3]int32{-216, 0, 216}
			return s
		}(),
		func() selParams {
			s := shipped()
			s.weights = [NBands]int32{-7, -5, 0, 0, 4, 4, 4, 0}
			s.centShift, s.scaleShift, s.gapShift = 9, 11, 9
			s.cuts = [3]int32{-216, 0, 216}
			return s
		}(),
	} {
		for _, r := range results {
			if r.sp == want {
				report("sim winner", r)
			}
		}
	}
}
