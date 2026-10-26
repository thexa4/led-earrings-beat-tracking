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
	"strings"
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
	return loadBenchDir(t, dir)
}

func loadBenchDir(t *testing.T, dir string) (benchSide, benchSide, []int) {
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

// TestBenchMulti scores every candidate on several captures, each replayed from a
// cold start, and ranks by the worst capture: constants that only work on one
// stretch of music do not work on a set.
//
//	BENCH_CAPS=dir1,dir2 go test -run TestBenchMulti -v -timeout 2h ./syncdsp/
//
// TestBenchCandidates scores a fixed list of selector settings on every
// capture in BENCH_CAPS: for checking finalists on captures the searches did
// not see.
func TestBenchCandidates(t *testing.T) {
	env := os.Getenv("BENCH_CAPS")
	if env == "" {
		t.Skip("BENCH_CAPS not set")
	}
	mk := func(w [NBands]int32, cs, ss, gs int32, cuts [NumPatterns - 1]int32) selParams {
		sp := shipped()
		sp.weights, sp.centShift, sp.scaleShift, sp.gapShift, sp.cuts = w, cs, ss, gs, cuts
		return sp
	}
	b23 := [NBands]int32{-7, 0, 3, 4, 0, 0, 0, 0}
	cands := []struct {
		name string
		sp   selParams
	}{
		{"shipped 9/12/9 +-256", shipped()},
		{"11/10/11 +-180", mk(b23, 11, 10, 11, [3]int32{-180, 0, 180})},
		{"11/12/11 -148/32/212", mk(b23, 11, 12, 11, [3]int32{-148, 32, 212})},
		{"11/12/11 +-180", mk(b23, 11, 12, 11, [3]int32{-180, 0, 180})},
		{"11/11/11 +-180", mk(b23, 11, 11, 11, [3]int32{-180, 0, 180})},
		{"10/12/10 +-216", mk(b23, 10, 12, 10, [3]int32{-216, 0, 216})},
		{"7de1b4b b3-b0 10/10/10 +-216", mk([NBands]int32{-7, 0, 0, 7, 0, 0, 0, 0}, 10, 10, 10, [3]int32{-216, 0, 216})},
		{"debug", debugSelector()},
	}
	type capture struct {
		name string
		L, R benchSide
		idx  []int
	}
	var caps []capture
	for _, dir := range strings.Split(env, ",") {
		L, R, idx := loadBenchDir(t, dir)
		caps = append(caps, capture{filepath.Base(dir), L, R, idx})
	}
	for _, c := range cands {
		line, sum, worst, minOcc := "", 0.0, 2.0, 1.0
		for _, cp := range caps {
			pl, _ := replay(cp.L.recs, c.sp)
			pr, _ := replay(cp.R.recs, c.sp)
			s := benchScore(pl, pr, cp.idx, 0, len(cp.idx))
			line += fmt.Sprintf(" %+.3f", s.kappa)
			sum += s.kappa
			worst = min(worst, s.kappa)
			minOcc = min(minOcc, s.minOcc)
		}
		t.Logf("%-30s mean %+.3f worst %+.3f minOcc %4.1f%% |%s", c.name,
			sum/float64(len(caps)), worst, 100*minOcc, line)
	}
}

// TestBenchLateBoot measures how fast an earring that boots into running music
// catches up: R is replayed fresh from a boot point (every 2 minutes from
// minute 2) while L has been running since the start, and the agreement is
// averaged per 15 s after boot over all boot points and BENCH_CAPS captures,
// next to the agreement the same stretches have when both run from the start.
func TestBenchLateBoot(t *testing.T) {
	env := os.Getenv("BENCH_CAPS")
	if env == "" {
		t.Skip("BENCH_CAPS not set")
	}
	b23 := [NBands]int32{-7, 0, 3, 4, 0, 0, 0, 0}
	alt := shipped()
	alt.weights, alt.centShift, alt.scaleShift, alt.gapShift = b23, 11, 10, 11
	alt.cuts = [NumPatterns - 1]int32{-180, 0, 180}
	cands := []struct {
		name string
		sp   selParams
	}{{"shipped 9/12/9 +-256", shipped()}, {"11/10/11 +-180", alt}}
	const bin, bins = 15 * fps, 12
	for _, c := range cands {
		var late, base [bins]float64
		var n [bins]float64
		for _, dir := range strings.Split(env, ",") {
			L, R, idx := loadBenchDir(t, dir)
			pl, _ := replay(L.recs, c.sp)
			pr, _ := replay(R.recs, c.sp)
			for i0 := int(2 * 60 * fps); i0+bins*bin < len(idx); i0 += int(2 * 60 * fps) {
				j0 := idx[i0]
				if j0 < 0 {
					continue
				}
				pb, _ := replay(R.recs[j0:], c.sp)
				for b := range bins {
					for i := i0 + b*bin; i < i0+(b+1)*bin; i++ {
						j := idx[i]
						if j < j0 || j-j0 >= len(pb) {
							continue
						}
						n[b]++
						if pl[i] == pb[j-j0] {
							late[b]++
						}
						if pl[i] == pr[j] {
							base[b]++
						}
					}
				}
			}
		}
		line := ""
		for b := range bins {
			line += fmt.Sprintf(" %3.0f/%3.0f", 100*late[b]/n[b], 100*base[b]/n[b])
		}
		t.Logf("%-22s late/steady agree %% per 15 s:%s", c.name, line)
	}
}

func TestBenchMulti(t *testing.T) {
	env := os.Getenv("BENCH_CAPS")
	if env == "" {
		t.Skip("BENCH_CAPS not set")
	}
	type capture struct {
		name string
		L, R benchSide
		idx  []int
	}
	var caps []capture
	for _, dir := range strings.Split(env, ",") {
		L, R, idx := loadBenchDir(t, dir)
		caps = append(caps, capture{filepath.Base(dir), L, R, idx})
	}
	ok := func(s score) bool {
		return s.minOcc >= 0.15 && s.chgA >= 0.5 && s.chgB >= 0.5 && s.chgA <= 8 && s.chgB <= 8 && s.shift <= 0.15
	}
	type result struct {
		sp     selParams
		scores []score
		worst  float64
		allOK  bool
	}
	run := func(sp selParams) result {
		r := result{sp: sp, worst: 2, allOK: true}
		for _, c := range caps {
			pl, _ := replay(c.L.recs, sp)
			pr, _ := replay(c.R.recs, sp)
			s := benchScore(pl, pr, c.idx, 0, len(c.idx))
			r.scores = append(r.scores, s)
			r.worst = min(r.worst, s.kappa)
			r.allOK = r.allOK && ok(s)
		}
		return r
	}
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
		{-7, 0, 0, 7, 0, 0, 0, 0},
		{-7, 0, 7, 0, 0, 0, 0, 0},
		{-7, 0, 4, 3, 0, 0, 0, 0},
		{-7, 0, 3, 4, 0, 0, 0, 0},
		{-7, 0, 0, 4, 3, 0, 0, 0},
		{-7, -3, 0, 7, 0, 0, 0, 0},
		{-7, 0, 0, 7, 0, 0, 3, 0},
	}
	var results []result
	for _, w := range weightSets {
		for cs := int32(8); cs <= 11; cs++ {
			for ss := int32(8); ss <= 13; ss++ {
				for gs := int32(7); gs <= 11; gs++ {
					for _, k := range []int32{128, 150, 180, 216, 256, 320} {
						sp := shipped()
						sp.weights, sp.centShift, sp.scaleShift, sp.gapShift = w, cs, ss, gs
						sp.cuts = [NumPatterns - 1]int32{-k, 0, k}
						results = append(results, run(sp))
					}
				}
			}
		}
	}
	sort.Slice(results, func(i, j int) bool {
		a, b := results[i], results[j]
		if a.allOK != b.allOK {
			return a.allOK
		}
		return a.worst > b.worst
	})
	report := func(label string, r result) {
		t.Logf("%s ok=%v worst kappa %+.3f  %s", label, r.allOK, r.worst, r.sp)
		for i, s := range r.scores {
			t.Logf("    %-14s %s", caps[i].name, s)
		}
	}
	report("shipped", run(shipped()))
	report("debug", run(debugSelector()))
	for i := 0; i < 10 && i < len(results); i++ {
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
}

// TestBenchFine is TestBenchMulti around the chosen region, adding asymmetric
// cuts, the backstop hold and the refractory, and reports leave-one-out: the
// best candidate on all captures but one, scored on the one left out - the
// number to expect on music the tuning has not seen.
//
//	BENCH_CAPS=dir1,dir2,dir3 go test -run TestBenchFine -v -timeout 2h ./syncdsp/
func TestBenchFine(t *testing.T) {
	env := os.Getenv("BENCH_CAPS")
	if env == "" {
		t.Skip("BENCH_CAPS not set")
	}
	type capture struct {
		name string
		L, R benchSide
		idx  []int
	}
	var caps []capture
	for _, dir := range strings.Split(env, ",") {
		L, R, idx := loadBenchDir(t, dir)
		caps = append(caps, capture{filepath.Base(dir), L, R, idx})
	}
	ok := func(s score) bool {
		return s.minOcc >= 0.15 && s.chgA >= 0.5 && s.chgB >= 0.5 && s.chgA <= 6 && s.chgB <= 6 && s.shift <= 0.15
	}
	type result struct {
		sp     selParams
		scores []score
	}
	eval := func(sp selParams) result {
		r := result{sp: sp}
		for _, c := range caps {
			pl, _ := replay(c.L.recs, sp)
			pr, _ := replay(c.R.recs, sp)
			r.scores = append(r.scores, benchScore(pl, pr, c.idx, 0, len(c.idx)))
		}
		return r
	}
	weightSets := [][NBands]int32{
		{-7, 0, 0, 7, 0, 0, 0, 0},
		{-7, 0, 3, 4, 0, 0, 0, 0},
		{-7, 0, 7, 0, 0, 0, 0, 0},
		{-7, 0, 0, 4, 3, 0, 0, 0},
		{-7, 0, 0, 0, 7, 0, 0, 0},
	}
	var results []result
	for _, w := range weightSets {
		for cs := int32(9); cs <= 11; cs++ {
			for ss := int32(9); ss <= 12; ss++ {
				for gs := int32(9); gs <= 11; gs++ {
					for _, k := range []int32{180, 216, 256} {
						for _, off := range []int32{-64, -32, 0, 32, 64} {
							for _, hold := range []int32{250, 500} {
								for _, ref := range []int32{PatternRefractoryBeats, 2 * PatternRefractoryBeats} {
									sp := shipped()
									sp.weights, sp.centShift, sp.scaleShift, sp.gapShift = w, cs, ss, gs
									sp.cuts = [NumPatterns - 1]int32{off - k, off, off + k}
									sp.maxHold, sp.refractory = hold, ref
									results = append(results, eval(sp))
								}
							}
						}
					}
				}
			}
		}
	}
	// rank orders by the worst kappa over the captures in use, constraint first.
	rank := func(use func(int) bool) []result {
		rs := append([]result(nil), results...)
		key := func(r result) (bool, float64) {
			allOK, worst := true, 2.0
			for i, s := range r.scores {
				if use(i) {
					allOK = allOK && ok(s)
					worst = min(worst, s.kappa)
				}
			}
			return allOK, worst
		}
		sort.SliceStable(rs, func(i, j int) bool {
			oa, wa := key(rs[i])
			ob, wb := key(rs[j])
			if oa != ob {
				return oa
			}
			return wa > wb
		})
		return rs
	}
	report := func(label string, r result) {
		t.Logf("%s  %s ref=%d", label, r.sp, r.sp.refractory)
		for i, s := range r.scores {
			t.Logf("    %-14s ok=%-5v %s", caps[i].name, ok(s), s)
		}
	}
	report("shipped", eval(shipped()))
	all := rank(func(int) bool { return true })
	for i := 0; i < 8; i++ {
		report(fmt.Sprintf("all #%d", i+1), all[i])
	}
	for out := range caps {
		best := rank(func(i int) bool { return i != out })[0]
		report(fmt.Sprintf("leave out %s: best on the rest, scored on %s", caps[out].name, caps[out].name), best)
	}
}

// TestBenchDecompose splits the shipped selector's disagreement into the part the
// feature causes - the two earrings' z on opposite sides of a cut - and the part
// the commit timing adds on top, by scoring the instantaneous bucket of z next to
// the committed pattern.
func TestBenchDecompose(t *testing.T) {
	env := os.Getenv("BENCH_CAPS")
	if env == "" {
		t.Skip("BENCH_CAPS not set")
	}
	sp := shipped()
	bucket := func(z int16) int8 {
		p := int8(0)
		for _, c := range sp.cuts {
			if int32(z) > c {
				p++
			}
		}
		return p
	}
	for _, dir := range strings.Split(env, ",") {
		L, R, idx := loadBenchDir(t, dir)
		pl, zl := replay(L.recs, sp)
		pr, zr := replay(R.recs, sp)
		il := make([]int8, len(zl))
		ir := make([]int8, len(zr))
		for i := range zl {
			il[i] = bucket(zl[i])
		}
		for i := range zr {
			ir[i] = bucket(zr[i])
		}
		var sx, sy, sxx, syy, sxy, n float64
		var hist [9]int
		for i := PatternWarmupFrames; i < len(idx); i++ {
			if idx[i] < 0 {
				continue
			}
			x, y := float64(zl[i]), float64(zr[idx[i]])
			sx, sy, sxx, syy, sxy, n = sx+x, sy+y, sxx+x*x, syy+y*y, sxy+x*y, n+1
			d := int(math.Abs(x-y)) / 64
			hist[min(d, 8)]++
		}
		c := (sxy - sx*sy/n) / math.Sqrt((sxx-sx*sx/n)*(syy-sy*sy/n))
		t.Logf("%s: corr(zL, zR) %.3f, |zL-zR| in 64ths of a spread: %v", filepath.Base(dir), c, hist)
		t.Logf("    committed      %s", benchScore(pl, pr, idx, 0, len(idx)))
		t.Logf("    instantaneous  %s", benchScore(il, ir, idx, 0, len(idx)))
	}
}
