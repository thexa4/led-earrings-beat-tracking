package syncdsp

// Pattern selection over a long real recording, two simulated earrings.
//
// The synth in TestAgreement is one minute of one arrangement, which is shorter
// than the selector's own time constants. This harness runs the firmware's own
// integer pipeline over a long recording, once per simulated device, records
// what the selector reads every frame (the smoothed spectrum and the commit
// triggers), and replays the selector over those recordings with any set of
// constants. The pipeline's behaviour does not depend on the pattern, so the
// replay is exact; TestPodcastPattern checks it against the pipeline's own
// Events.Pattern on every frame before reporting anything.
//
//	PODCAST=/tmp/psync/edm16k.wav go test -run TestPodcastPattern -v -timeout 30m ./syncdsp/
//
// The WAV must be 16 kHz mono PCM16, e.g.
//
//	ffmpeg -i edm-music-podcast.mp3 -ac 1 -ar 16000 -c:a pcm_s16le /tmp/psync/edm16k.wav
//
// Samples are signed PCM and are mapped onto the ADC's unsigned codes around the
// device model's bias; feeding the int16 bit pattern straight in as uint16 wraps
// every negative sample by a full 65536 counts.
//
// Metrics, all over the frames where both devices are past the pattern warmup,
// paired on room time (a drifting device's frames are mapped back onto the
// reference clock):
//
//	agree     fraction of frames both devices show the same pattern
//	kappa     agreement corrected for what the two occupancies give by chance
//	shifted   kappa with device B delayed by 97 s: agreement that survives that
//	          is two devices sitting on the same slow state, not tracking music
//	minOcc    the least-used pattern's share of time, worst device
//	chg/min   pattern changes per minute, per device
//	stuck     fraction of time inside a disagreement that lasts >= 2 s, which
//	          is what a person watching actually notices

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
)

// readPCM16 returns the samples of a 16 kHz mono PCM16 WAV.
func readPCM16(t testing.TB, path string) []int16 {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var data []byte
	for i := 12; i+8 <= len(b); {
		id, n := string(b[i:i+4]), int(binary.LittleEndian.Uint32(b[i+4:i+8]))
		switch id {
		case "fmt ":
			if ch := binary.LittleEndian.Uint16(b[i+10:]); ch != 1 {
				t.Fatalf("%s: %d channels, need mono", path, ch)
			}
			if r := binary.LittleEndian.Uint32(b[i+12:]); r != 16000 {
				t.Fatalf("%s: %d Hz, need 16000", path, r)
			}
		case "data":
			if i+8+n > len(b) {
				n = len(b) - i - 8
			}
			data = b[i+8 : i+8+n]
		}
		i += 8 + n + n%2
	}
	out := make([]int16, len(data)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(data[2*i:]))
	}
	return out
}

// device is one earring's input path. Level is the music's RMS at the ADC in
// counts (pre-AGC). The bench measured ~40 counts RMS with a loud speaker at
// 1 cm, the bias at 17120, a broadband floor of MAD 1.5 (L) / 5 (R) counts and
// one-sided-looking impulses of ~40 counts on R (debug branch, bench_test.go).
type device struct {
	name      string
	rms       float64 // music RMS at the ADC, counts
	mad       float64 // broadband noise floor, counts
	spikeAmp  float64
	spikeRate float64
	tilt      bool    // +~4 dB HF shelf (hand shadowing)
	rumble    float64 // independent low-frequency noise (<~100 Hz), as a fraction of rms
	drift     float64 // clock error: this device consumes 1+drift room samples per own sample
	start     int     // room sample at which this device starts (boot offset)
	seed      uint32
}

// frameRec is what the selector reads on one frame.
type frameRec struct {
	smooth                        [NBands]int32
	decide, returned, bar, anchor bool
	pattern                       int32 // the pipeline's own Events.Pattern
	changed                       bool
}

func runDevice(src []int16, scale float64, d device) []frameRec {
	var p Pipeline
	p.Init()
	s := d.seed | 1
	rnd := func() float64 {
		s ^= s << 13
		s ^= s >> 17
		s ^= s << 5
		return (float64(s) + 0.5) / 4294967296
	}
	gain := d.rms * scale
	step := 1 + d.drift
	win := make([]uint16, Window)
	var out []frameRec
	var prevX, lf float64
	// A one-pole low-pass at ~100 Hz of uniform noise has an RMS of about
	// sqrt(alpha/2)/sqrt(3) of the input's peak; normalise so rumble is a
	// fraction of the music RMS.
	const alpha = 2 * math.Pi * 100 / 16000
	lfAmp := d.rumble * d.rms / (math.Sqrt(alpha/2) / math.Sqrt(3))
	n := 0
	for {
		base := float64(d.start) + float64(n*Window)*step
		if int(base+float64(Window)*step)+2 >= len(src) {
			break
		}
		for i := range win {
			pos := base + float64(i)*step
			j := int(pos)
			fr := pos - float64(j)
			x := (float64(src[j])*(1-fr) + float64(src[j+1])*fr) * gain
			if d.tilt {
				x, prevX = x+(x-prevX)/4, x
			}
			if d.mad > 0 {
				x += d.mad * 1.65 * (rnd() + rnd() + rnd() - 1.5)
			}
			if d.rumble > 0 {
				lf += alpha * ((2*rnd()-1)*lfAmp - lf)
				x += lf
			}
			if d.spikeRate > 0 && rnd() < d.spikeRate {
				if rnd() < 0.5 {
					x -= d.spikeAmp
				} else {
					x += d.spikeAmp
				}
			}
			y := math.Round(x) + benchDC
			if y < 0 {
				y = 0
			} else if y > 65535 {
				y = 65535
			}
			win[i] = uint16(y)
		}
		ev := p.Process(win)
		out = append(out, frameRec{
			smooth:   p.smooth,
			decide:   ev.HashAge == 0,
			returned: ev.Returned,
			bar:      ev.Bar,
			anchor:   ev.Anchor,
			pattern:  ev.Pattern,
			changed:  ev.PatternChanged,
		})
		n++
	}
	return out
}

// selParams is everything the selector can be tuned by.
type selParams struct {
	weights               [NBands]int32
	centShift, scaleShift int32
	gapShift              int32
	cuts                  [NumPatterns - 1]int32
	maxHold               int32
	refractory            int32
}

func shipped() selParams {
	return selParams{
		weights:    PatternWeights,
		centShift:  PatternCentroidShift,
		scaleShift: PatternScaleShift,
		gapShift:   PatternGapShift,
		cuts:       [NumPatterns - 1]int32{PatternCut1, PatternCut2, PatternCut3},
		maxHold:    PatternMaxHoldFrames,
		refractory: PatternRefractoryBeats,
	}
}

func (s selParams) String() string {
	return fmt.Sprintf("w=%v sh=%d/%d/%d cuts=%v hold=%d", s.weights, s.centShift, s.scaleShift, s.gapShift, s.cuts, s.maxHold)
}

func rshift(v, sh int32) int32 { return (v + (1 << (sh - 1))) >> sh }

// replay runs the selector over recorded frames, mirroring the pipeline: the
// centroid update (step 5b'), the backstop (6b), decide's commit (7) and the
// anchor's (8), in that order. It also returns z, the clamped deviation in Q8
// spreads that PatternIndex compares against the cuts (0 before warmup).
func replay(recs []frameRec, sp selParams) (pats []int8, zs []int16) {
	return replayFrom(recs, sp, nil)
}

// selState is the selector's state at the first recorded frame, for replaying a
// capture that starts on a device which has been running for a while. frame is
// the device's frame counter at recs[0].
type selState struct {
	frame                       int32
	centQ8, avgQ8, devQ8, gapQ8 int32
	last                        int32
}

func replayFrom(recs []frameRec, sp selParams, st *selState) (pats []int8, zs []int16) {
	pats = make([]int8, len(recs))
	zs = make([]int16, len(recs))
	var centQ8, avgQ8, devQ8, gapQ8 int32
	last := int32(0)
	frame0 := int32(0)
	if st != nil {
		frame0, centQ8, avgQ8, devQ8, gapQ8, last = st.frame, st.centQ8, st.avgQ8, st.devQ8, st.gapQ8, st.last
	}
	beatsSince, framesSince := int32(0), int32(0)
	index := func(frame int32) (int32, int32) {
		if frame < PatternWarmupFrames {
			return NumPatterns / 2, 0
		}
		gap, spread := gapQ8>>8, devQ8>>8
		if spread < PatternMinDev {
			return NumPatterns / 2, 0
		}
		if lim := spread * 2; gap > lim {
			gap = lim
		} else if gap < -lim {
			gap = -lim
		}
		z := gap * 256 / spread
		pat := int32(0)
		for _, c := range sp.cuts {
			if z > c {
				pat++
			}
		}
		return pat, z
	}
	set := func(pat int32) {
		if pat != last {
			last = pat
			beatsSince = 0
			framesSince = 0
		}
	}
	for f, r := range recs {
		frame := frame0 + int32(f)
		c := int32(0)
		for i, v := range r.smooth {
			c += v * sp.weights[i]
		}
		c *= PatternCentroidScale
		if frame < PatternSeedFrames {
			centQ8, avgQ8, devQ8, gapQ8 = c<<8, c<<8, 0, 0
		} else {
			centQ8 += rshift((c<<8)-centQ8, sp.centShift)
			gap := (centQ8 >> 8) - (avgQ8 >> 8)
			gapQ8 += rshift((gap<<8)-gapQ8, sp.gapShift)
			abs := gap
			if abs < 0 {
				abs = -abs
			}
			devQ8 += rshift((abs<<8)-devQ8, sp.scaleShift)
			avgQ8 += rshift(gap<<8, sp.scaleShift)
		}
		pat, z := index(frame)
		zs[f] = int16(z)
		if frame >= PatternWarmupFrames && framesSince >= sp.maxHold {
			set(pat)
		}
		framesSince++
		if r.decide {
			if (r.returned || r.bar) && beatsSince >= sp.refractory {
				set(pat)
			}
			beatsSince++
		}
		if r.anchor {
			set(pat)
		}
		pats[f] = int8(last)
	}
	return pats, zs
}

type score struct {
	frames              int
	agree, kappa, shift float64
	minOcc              float64
	occA, occB          [NumPatterns]float64
	chgA, chgB          float64 // per minute
	stuck               float64
}

func (s score) String() string {
	return fmt.Sprintf("agree %5.1f%%  kappa %+.3f  shifted %+.3f  minOcc %4.1f%%  chg/min %.2f/%.2f  stuck %4.1f%%  occA %s occB %s",
		100*s.agree, s.kappa, s.shift, 100*s.minOcc, s.chgA, s.chgB, 100*s.stuck, occString(s.occA), occString(s.occB))
}

func occString(o [NumPatterns]float64) string {
	parts := make([]string, len(o))
	for i, v := range o {
		parts[i] = fmt.Sprintf("%.0f", 100*v)
	}
	return strings.Join(parts, "/")
}

const fps = 16000.0 / Window

// pairIndex maps reference frame f onto device b's frame at the same room time.
func pairIndex(f int, a, b device) int {
	room := float64(a.start) + float64(f*Window)*(1+a.drift)
	return int(math.Round((room - float64(b.start)) / ((1 + b.drift) * Window)))
}

func kappaOf(a, b []int8) float64 {
	var conf [NumPatterns][NumPatterns]float64
	for i := range a {
		conf[a[i]][b[i]]++
	}
	n := float64(len(a))
	if n == 0 {
		return 0
	}
	po, pe := 0.0, 0.0
	for i := range NumPatterns {
		po += conf[i][i] / n
		ra, rb := 0.0, 0.0
		for j := range NumPatterns {
			ra += conf[i][j] / n
			rb += conf[j][i] / n
		}
		pe += ra * rb
	}
	if pe >= 1 {
		return 0
	}
	return (po - pe) / (1 - pe)
}

// scorePair scores device B against device A over reference frames [from, to).
func scorePair(pa, pb []int8, da, db device, from, to int) score {
	if from < int(PatternWarmupFrames)*2 {
		from = int(PatternWarmupFrames) * 2
	}
	var A, B []int8
	for f := from; f < to && f < len(pa); f++ {
		g := pairIndex(f, da, db)
		if g < int(PatternWarmupFrames) || g >= len(pb) {
			continue
		}
		A = append(A, pa[f])
		B = append(B, pb[g])
	}
	return scoreAligned(A, B)
}

// scoreAligned scores two pattern sequences already paired frame by frame.
func scoreAligned(A, B []int8) score {
	var s score
	s.frames = len(A)
	if s.frames == 0 {
		return s
	}
	same, stuckN, run := 0, 0, 0
	minRun := int(2 * fps)
	for i := range A {
		s.occA[A[i]]++
		s.occB[B[i]]++
		if A[i] == B[i] {
			same++
			if run >= minRun {
				stuckN += run
			}
			run = 0
		} else {
			run++
		}
		if i > 0 {
			if A[i] != A[i-1] {
				s.chgA++
			}
			if B[i] != B[i-1] {
				s.chgB++
			}
		}
	}
	if run >= minRun {
		stuckN += run
	}
	n := float64(s.frames)
	s.agree = float64(same) / n
	s.stuck = float64(stuckN) / n
	s.minOcc = 1
	for i := range NumPatterns {
		s.occA[i] /= n
		s.occB[i] /= n
		s.minOcc = math.Min(s.minOcc, math.Min(s.occA[i], s.occB[i]))
	}
	mins := n / fps / 60
	s.chgA /= mins
	s.chgB /= mins
	s.kappa = kappaOf(A, B)
	lag := int(97 * fps)
	if len(A) > 2*lag {
		s.shift = kappaOf(A[:len(A)-lag], B[lag:])
	}
	return s
}

// scenario is a pair of earrings; A is the reference.
type scenario struct {
	name string
	a, b device
}

// scenarios spans the conditions the selector has to survive. Levels are the
// music's RMS at the ADC: ~40 counts is the measured bench, a party next to a PA
// is louder (AgcMaxShift's ceiling is a 187-count peak, so anything above a few
// hundred counts RMS runs at low gain).
func scenarios() []scenario {
	benchL := device{name: "L", mad: 1.5, spikeAmp: 40, spikeRate: 0.03, seed: 0x1d2c4a5b}
	benchR := device{name: "R", mad: 5, spikeAmp: 40, spikeRate: 0.03, seed: 0x8f3b2c1d}
	with := func(d device, f func(*device)) device { f(&d); return d }
	return []scenario{
		{"party: 400 vs 200 counts, tilt, +0.4% clock, 3 s boot offset",
			with(benchL, func(d *device) { d.rms = 400 }),
			with(benchR, func(d *device) { d.rms = 200; d.tilt = true; d.drift = 0.004; d.start = 3 * 16000 })},
		{"bench: 40 vs 40 counts, measured floors, +0.2% clock",
			with(benchL, func(d *device) { d.rms = 40 }),
			with(benchR, func(d *device) { d.rms = 40; d.drift = 0.002; d.start = 16000 })},
		{"level: 400 vs 40 counts (20 dB), tilt, -0.3% clock",
			with(benchL, func(d *device) { d.rms = 400 }),
			with(benchR, func(d *device) { d.rms = 40; d.tilt = true; d.drift = -0.003; d.start = 7 * 16000 })},
		// Two real microphones disagree about the bass far more than about the
		// rest (debug branch, 25 min bench capture: band 7 shape correlation
		// -0.42 against +0.80..+0.90 for bands 0, 4, 5). Independent low-frequency
		// noise on each device - handling, body, rumble - is one physical cause.
		{"realmic: 100 vs 70 counts, own rumble at 70% of music RMS",
			with(benchL, func(d *device) { d.rms = 100; d.rumble = 0.7 }),
			with(benchR, func(d *device) { d.rms = 70; d.rumble = 0.7; d.tilt = true; d.drift = 0.001; d.start = 2 * 16000 })},
	}
}

type pairRec struct {
	sc   scenario
	a, b []frameRec
}

// measuredRho is the per-band correlation between the two bench earrings'
// smoothed band energies, each as a deviation from its own average (debug
// branch, tools/bandfit.py, 25 min capture). The simulated devices hear one
// signal and agree on every band; two real microphones do not.
var measuredRho = [NBands]float64{0.90, 0.70, 0.07, 0.07, 0.80, 0.84, 0.69, -0.42}

// withMeasuredChannel returns a copy of recs whose band deviations correlate
// with the originals by rho: dev' = rho*dev + sqrt(1-rho^2)*sd*n, with n an
// independent AR(1) process of time constant tau frames and dev the deviation
// from a 16 s average. The selector reads nothing else from the spectrum, so
// pairing the original with this copy reproduces the measured channel exactly
// in the quantity that matters. tau is an assumption: the measurement gives
// the correlation, not the time scale of what decorrelates.
func withMeasuredChannel(recs []frameRec, rho [NBands]float64, tau float64, seed uint32) []frameRec {
	out := make([]frameRec, len(recs))
	copy(out, recs)
	s := seed | 1
	gauss := func() float64 {
		s ^= s << 13
		s ^= s >> 17
		s ^= s << 5
		u1 := (float64(s) + 0.5) / 4294967296
		s ^= s << 13
		s ^= s >> 17
		s ^= s << 5
		u2 := (float64(s) + 0.5) / 4294967296
		return math.Sqrt(-2*math.Log(u1)) * math.Cos(2*math.Pi*u2)
	}
	a := math.Exp(-1 / tau)
	for b := range NBands {
		dev := make([]float64, len(recs))
		mean := float64(recs[0].smooth[b])
		var ss float64
		for f, r := range recs {
			v := float64(r.smooth[b])
			mean += (v - mean) / 1024
			dev[f] = v - mean
			ss += dev[f] * dev[f]
		}
		sd := math.Sqrt(ss / float64(len(recs)))
		k := math.Sqrt(1 - rho[b]*rho[b])
		n := 0.0
		for f := range recs {
			n = a*n + math.Sqrt(1-a*a)*gauss()
			v := float64(recs[f].smooth[b]) - dev[f] + rho[b]*dev[f] + k*sd*n
			out[f].smooth[b] = int32(math.Round(v))
		}
	}
	return out
}

// addMeasuredPairs adds pairs whose device B is the party pair's device A seen
// through the measured channel, at a few assumed time scales.
func addMeasuredPairs(pairs []pairRec) []pairRec {
	base := pairs[0]
	for _, tau := range []float64{fps / 2, 2 * fps, 8 * fps} {
		sc := scenario{name: fmt.Sprintf("measured channel, tau %.1f s", tau/fps), a: base.sc.a, b: base.sc.a}
		pairs = append(pairs, pairRec{sc: sc, a: base.a, b: withMeasuredChannel(base.a, measuredRho, tau, uint32(tau)*2654435761)})
	}
	return pairs
}

// recordPairs runs every scenario's two devices in parallel.
func recordPairs(t testing.TB, path string) []pairRec {
	src := readPCM16(t, path)
	var ss float64
	for _, v := range src {
		ss += float64(v) * float64(v)
	}
	rms := math.Sqrt(ss / float64(len(src)))
	scale := 1 / rms
	t.Logf("%s: %.0f s, file RMS %.0f", path, float64(len(src))/16000, rms)
	scs := scenarios()
	out := make([]pairRec, len(scs))
	var wg sync.WaitGroup
	for i, sc := range scs {
		out[i].sc = sc
		wg.Add(2)
		go func() { defer wg.Done(); out[i].a = runDevice(src, scale, sc.a) }()
		go func() { defer wg.Done(); out[i].b = runDevice(src, scale, sc.b) }()
	}
	wg.Wait()
	return out
}

// evaluate replays the selector on every pair and scores [lo, hi) as fractions
// of each recording.
func evaluate(pairs []pairRec, sp selParams, lo, hi float64) []score {
	out := make([]score, len(pairs))
	for i, pr := range pairs {
		pa, _ := replay(pr.a, sp)
		pb, _ := replay(pr.b, sp)
		n := len(pr.a)
		out[i] = scorePair(pa, pb, pr.sc.a, pr.sc.b, int(lo*float64(n)), int(hi*float64(n)))
	}
	return out
}

func TestPodcastPattern(t *testing.T) {
	path := os.Getenv("PODCAST")
	if path == "" {
		t.Skip("PODCAST not set")
	}
	pairs := recordPairs(t, path)

	// The replay is only worth anything if it is the firmware: check it against
	// the pipeline's own pattern on every frame of every device, and check the
	// pipeline reports every change it makes.
	for _, pr := range pairs {
		for _, recs := range [][]frameRec{pr.a, pr.b} {
			pats, _ := replay(recs, shipped())
			prev := int32(0)
			for f, r := range recs {
				if int32(pats[f]) != r.pattern {
					t.Fatalf("%s: replay diverges from the pipeline at frame %d: %d vs %d", pr.sc.name, f, pats[f], r.pattern)
				}
				if r.pattern != prev && !r.changed {
					t.Fatalf("%s: frame %d changed pattern without PatternChanged", pr.sc.name, f)
				}
				prev = r.pattern
			}
		}
	}
	t.Logf("replay matches the pipeline on every frame of %d devices", 2*len(pairs))

	for _, half := range []struct {
		name   string
		lo, hi float64
	}{{"first half", 0, 0.5}, {"second half", 0.5, 1}, {"whole", 0, 1}} {
		for i, s := range evaluate(pairs, shipped(), half.lo, half.hi) {
			t.Logf("%-11s %-62s %s", half.name, pairs[i].sc.name, s)
		}
	}
}

// objective is what the sweep maximises on the fit half: mean kappa over the
// scenarios, with occupancy, change rate and the time-shifted control as hard
// constraints (a selector that parks on one pattern agrees perfectly and is
// useless). ok is false when a constraint fails.
func objective(ss []score) (val float64, ok bool) {
	ok = true
	for _, s := range ss {
		val += s.kappa
		if s.minOcc < 0.18 || s.chgA < 0.5 || s.chgB < 0.5 || s.chgA > 8 || s.chgB > 8 || s.shift > 0.15 {
			ok = false
		}
	}
	return val / float64(len(ss)), ok
}

// quantileCuts places the three cuts at the quartiles of z, pooled over every
// device on [lo, hi) of its recording: the fixed, asymmetric cut table. Both
// earrings then compare against the same three constants, so unlike a quartile
// tracked on each device there is no per-device estimate to wander.
func quantileCuts(pairs []pairRec, sp selParams, lo, hi float64) [NumPatterns - 1]int32 {
	var all []int
	for _, pr := range pairs {
		for _, recs := range [][]frameRec{pr.a, pr.b} {
			_, zs := replay(recs, sp)
			n := len(zs)
			from := max(int(lo*float64(n)), int(PatternWarmupFrames)*2)
			for _, z := range zs[from:int(hi*float64(n))] {
				all = append(all, int(z))
			}
		}
	}
	sort.Ints(all)
	var cuts [NumPatterns - 1]int32
	for i := range cuts {
		cuts[i] = int32(all[len(all)*(i+1)/NumPatterns])
	}
	// z > cut selects the upper bucket; nudge apart if quantization made two equal.
	for i := 1; i < len(cuts); i++ {
		if cuts[i] <= cuts[i-1] {
			cuts[i] = cuts[i-1] + 1
		}
	}
	return cuts
}

// summarize collapses a dataset's scenarios into one line: mean agreement and
// kappa, the worst scenario's kappa and least-used pattern, mean stuck time and
// change rate.
func summarize(ss []score) string {
	var ag, ka, st, ch float64
	worstK, worstOcc := 1.0, 1.0
	for _, s := range ss {
		ag += s.agree
		ka += s.kappa
		st += s.stuck
		ch += (s.chgA + s.chgB) / 2
		worstK = math.Min(worstK, s.kappa)
		worstOcc = math.Min(worstOcc, s.minOcc)
	}
	n := float64(len(ss))
	return fmt.Sprintf("agree %5.1f%%  kappa %+.3f (worst %+.3f)  minOcc %4.1f%%  stuck %4.1f%%  chg/min %.1f",
		100*ag/n, ka/n, worstK, 100*worstOcc, 100*st/n, ch/n)
}

// TestPodcastSweep fits the selector's constants on the first half of the
// recording and reports the winners on the second half, plus any other
// recordings in PODCAST_EXTRA (comma-separated), which were not used to fit.
func TestPodcastSweep(t *testing.T) {
	path := os.Getenv("PODCAST")
	if path == "" {
		t.Skip("PODCAST not set")
	}
	pairs := addMeasuredPairs(recordPairs(t, path))
	var extra [][]pairRec
	var extraNames []string
	if e := os.Getenv("PODCAST_EXTRA"); e != "" {
		for _, p := range strings.Split(e, ",") {
			extra = append(extra, addMeasuredPairs(recordPairs(t, p)))
			extraNames = append(extraNames, shortName(p))
		}
	}

	weightSets := [][NBands]int32{
		{-7, 0, 0, 0, 7, 0, 0, 0},    // band 4 against band 0 (debug working tree)
		{-7, 0, 0, 0, 0, 7, 0, 0},    // band 5 against band 0
		{-7, 0, 0, 0, 4, 3, 0, 0},    // bands 4+5 against band 0
		{-7, -3, 0, 0, 3, 7, 0, 0},   // bands 0+1 against 4+5
		{-7, -3, -1, 1, 3, 7, 0, 0},  // ramp over bands 0..5, no bass
		{-7, -4, -2, 0, 2, 4, 7, 0},  // ramp over bands 0..6, no bass
		{-7, -5, -3, -1, 1, 3, 5, 7}, // full ramp, includes band 7 (debug committed)
		// Only the bands the two bench microphones measurably share (0, 1, 4, 5, 6).
		{-7, -3, 0, 0, 3, 4, 3, 0},
		{-7, -4, 0, 0, 4, 4, 3, 0},
		{-7, -5, 0, 0, 4, 4, 4, 0},
		{-7, -2, 0, 0, 2, 4, 3, 0},
	}
	type result struct {
		sp        selParams
		fit       float64
		ok        bool
		symmetric bool
	}
	var results []result
	for _, w := range weightSets {
		for cs := int32(8); cs <= 11; cs++ {
			for ss := int32(9); ss <= 12; ss++ {
				for gs := int32(7); gs <= 11; gs++ {
					sp := shipped()
					sp.weights, sp.centShift, sp.scaleShift, sp.gapShift = w, cs, ss, gs
					for _, k := range []int32{100, 150, 216} {
						sp.cuts = [NumPatterns - 1]int32{-k, 0, k}
						v, ok := objective(evaluate(pairs, sp, 0, 0.5))
						results = append(results, result{sp: sp, fit: v, ok: ok, symmetric: true})
					}
					sp.cuts = quantileCuts(pairs, sp, 0, 0.5)
					v, ok := objective(evaluate(pairs, sp, 0, 0.5))
					results = append(results, result{sp: sp, fit: v, ok: ok})
				}
			}
		}
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].ok != results[j].ok {
			return results[i].ok
		}
		return results[i].fit > results[j].fit
	})
	report := func(label string, r result) {
		t.Logf("%s fit kappa %+.3f ok=%v  %s", label, r.fit, r.ok, r.sp)
		t.Logf("    %-10s %s", "held-out", summarize(evaluate(pairs, r.sp, 0.5, 1)))
		for k, ex := range extra {
			t.Logf("    %-10s %s", extraNames[k], summarize(evaluate(ex, r.sp, 0, 1)))
		}
	}
	report("shipped", result{sp: shipped(), fit: func() float64 { v, _ := objective(evaluate(pairs, shipped(), 0, 0.5)); return v }()})
	for i := 0; i < 10 && i < len(results); i++ {
		report(fmt.Sprintf("#%d", i+1), results[i])
	}
	// The best of each weight set, so the weights can be compared at their own
	// best time constants.
	for _, w := range weightSets {
		for _, r := range results {
			if r.sp.weights == w {
				report("best for weights", r)
				break
			}
		}
	}
}

func shortName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		p = p[i+1:]
	}
	return strings.TrimSuffix(p, ".wav")
}
