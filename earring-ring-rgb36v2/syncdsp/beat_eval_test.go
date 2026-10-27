package syncdsp

// Beat tracking measured against the music and between two earrings.
//
// The reference beat grid should come from a file: BEATREFDIR/<name>.beats,
// one beat time in seconds per line, made by tools/beatref/kbeats.py -
// librosa's dynamic-programming tracker (Ellis 2007) on an onset envelope from
// the mel bands below 200 Hz, so the beat is the kick. librosa's own full-band
// envelope puts the beat on the off-beat hats about half the time on the EDM
// set used here (the low band's energy peaks half a beat after its beats),
// which is why the kick version is the reference.
//
// Without a file, refBeats builds a grid itself (the same DP on a 4 ms
// low-band plus full-band log flux). Do not trust it: at 4 ms hops the
// low-band energy is mostly waveform phase, and its beats land at every phase
// of the kick reference.
//
// Each simulated earring is the firmware's pipeline behind the device model of
// podcast_pattern_test.go (level, noise floors, spikes, tilt, clock drift, boot
// offset). A beat is the frame the pipeline reports it on, placed at the end of
// that frame in room time - the moment the LEDs can flash.
//
//	python tools/beatref/kbeats.py /tmp/x/pod0.wav /tmp/x/pod0.beats   # once per file, needs librosa
//	BEATREFDIR=/tmp/x BEATWAV=/tmp/x/pod0.wav,/tmp/x/pod1200.wav go test -run TestBeatEval -v ./syncdsp/
//	BEATREFDIR=/tmp/x HEARSL=../../hears-left.wav HEARSR=../../hears-right.wav go test -run TestBeatHears -v ./syncdsp/
//
// Metrics, all after BeatEvalSkip seconds:
//
//	lock     seconds until the tracker locks and stays locked
//	hit      fraction of reference beats with a device beat within +-BeatTol
//	extra    device beats with no reference beat within +-BeatTol, per ref beat
//	off      median signed device-minus-reference time of the hits, ms
//	pair     fraction of A's beats with a B beat within +-PairTol
//	per      fraction of locked frames whose period is within 4% of the
//	         reference's (x2 and /2: the octave errors)

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const (
	beatEvalSkip = 20.0  // s: ignore the start-up
	beatTol      = 0.070 // s: the usual beat-tracking tolerance
	pairTol      = 0.032 // s: two frames; a flash this far apart reads as one
)

// refOnset returns an onset envelope at hop samples per value: half-wave
// rectified log-energy flux of a low band (< ~150 Hz, the kick) and of the
// full band, the low one weighted double, minus a 1 s moving average.
func refOnset(src []int16, hop int) []float64 {
	const fs = 16000.0
	// Two cascaded one-poles at 150 Hz for the low band.
	a := 1 - math.Exp(-2*math.Pi*150/fs)
	var l1, l2 float64
	n := len(src) / hop
	low := make([]float64, n)
	full := make([]float64, n)
	for i := range n {
		var el, ef float64
		for _, s := range src[i*hop : (i+1)*hop] {
			x := float64(s)
			l1 += a * (x - l1)
			l2 += a * (l1 - l2)
			el += l2 * l2
			ef += x * x
		}
		low[i] = math.Log(el + 1)
		full[i] = math.Log(ef + 1)
	}
	env := make([]float64, n)
	for i := 1; i < n; i++ {
		env[i] = 2*math.Max(low[i]-low[i-1], 0) + math.Max(full[i]-full[i-1], 0)
	}
	w := int(fs / float64(hop)) // 1 s
	out := make([]float64, n)
	var sum float64
	for i := range n {
		sum += env[i]
		if i >= w {
			sum -= env[i-w]
		}
		out[i] = math.Max(env[i]-sum/float64(min(i+1, w)), 0)
	}
	return out
}

// refBeats returns the reference beat times (s) and the local beat period (s)
// at each of them.
func refBeats(src []int16) (times, periods []float64) {
	const hop = 64
	const rate = 16000.0 / hop
	env := refOnset(src, hop)
	n := len(env)

	// Tempo per 20 s window (hop 5 s): autocorrelation over 90..180 BPM,
	// weighted toward 120 BPM by a log-Gaussian (Ellis' tempo prior), refined
	// by the parabola through the peak.
	win, step := int(20*rate), int(5*rate)
	minLag, maxLag := int(rate*60)/180, int(rate*60)/90
	tau := make([]float64, n) // period in hops, per hop
	var centers []int
	var taus []float64
	for s := 0; s+win <= n || s == 0; s += step {
		e := min(s+win, n)
		ac := make([]float64, maxLag+2)
		for lag := minLag - 1; lag <= maxLag+1; lag++ {
			var c float64
			for i := s + lag; i < e; i++ {
				c += env[i] * env[i-lag]
			}
			ac[lag] = c * math.Exp(-0.5*math.Pow(math.Log2(float64(lag)/(rate*0.5))/1.0, 2))
		}
		best := minLag
		for lag := minLag; lag <= maxLag; lag++ {
			if ac[lag] > ac[best] {
				best = lag
			}
		}
		t := float64(best)
		if d := ac[best-1] - 2*ac[best] + ac[best+1]; d < 0 {
			t += 0.5 * (ac[best-1] - ac[best+1]) / d
		}
		centers = append(centers, (s+e)/2)
		taus = append(taus, t)
		if e == n {
			break
		}
	}
	for i := range n {
		j := sort.SearchInts(centers, i)
		switch {
		case j == 0:
			tau[i] = taus[0]
		case j == len(centers):
			tau[i] = taus[len(taus)-1]
		default:
			f := float64(i-centers[j-1]) / float64(centers[j]-centers[j-1])
			tau[i] = taus[j-1]*(1-f) + taus[j]*f
		}
	}

	// Dynamic programming: score(t) = env(t) + max over p in [t-2tau, t-tau/2]
	// of score(p) - alpha * log((t-p)/tau)^2.
	const alpha = 400.0
	var norm float64
	for _, v := range env {
		norm += v
	}
	norm /= float64(n)
	score := make([]float64, n)
	back := make([]int, n)
	for t := range n {
		lo, hi := t-int(2*tau[t]), t-int(tau[t]/2)
		best, bp := 0.0, -1
		for p := max(lo, 0); p <= hi; p++ {
			d := math.Log(float64(t-p) / tau[t])
			if v := score[p] - alpha*norm*d*d; bp < 0 || v > best {
				best, bp = v, p
			}
		}
		score[t] = env[t]
		back[t] = -1
		if bp >= 0 && best > 0 {
			score[t] += best
			back[t] = bp
		}
	}
	// Backtrack from the best score in the last period.
	end := n - 1
	for t := n - 1; t >= max(0, n-int(2*tau[n-1])); t-- {
		if score[t] > score[end] {
			end = t
		}
	}
	var hops []int
	for t := end; t >= 0; t = back[t] {
		hops = append(hops, t)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		times = append(times, (float64(hops[i])+0.5)/rate)
		periods = append(periods, tau[hops[i]]/rate)
	}
	return times, periods
}

// beatRun is one simulated earring's beat output in room time.
type beatRun struct {
	beats    []float64 // s
	lockAt   float64   // s; +Inf if never locked for good
	perOK    float64   // fraction of locked frames at the reference period
	perOct   float64   // fraction at half or double it
	patterns []int32   // per frame
	frameT   []float64 // room time at the end of each frame
	anchors  []float64
}

// runBeatDevice runs the pipeline behind device model d over src (signed PCM,
// mapped onto ADC codes at gain scale*d.rms/rms(src)).
func runBeatDevice(src []int16, d device, refT, refP []float64) beatRun {
	var p Pipeline
	p.Init()
	s := d.seed | 1
	rnd := func() float64 {
		s ^= s << 13
		s ^= s >> 17
		s ^= s << 5
		return (float64(s) + 0.5) / 4294967296
	}
	var sq float64
	for _, v := range src {
		sq += float64(v) * float64(v)
	}
	gain := d.rms / math.Sqrt(sq/float64(len(src)))
	step := 1 + d.drift
	const alpha = 2 * math.Pi * 100 / 16000
	lfAmp := d.rumble * d.rms / (math.Sqrt(alpha/2) / math.Sqrt(3))
	var prevX, lf float64
	win := make([]uint16, Window)
	var r beatRun
	r.lockAt = math.Inf(1)
	var locked, perOK, perOct int
	for n := 0; ; n++ {
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
			win[i] = uint16(min(max(math.Round(x)+benchDC, 0), 65535))
		}
		ev := p.Process(win)
		t := (base + float64(Window)*step) / 16000
		r.frameT = append(r.frameT, t)
		r.patterns = append(r.patterns, ev.Pattern)
		if ev.Beat && ev.Locked {
			r.beats = append(r.beats, t)
		}
		if ev.Anchor {
			r.anchors = append(r.anchors, t)
		}
		if !ev.Locked {
			r.lockAt = math.Inf(1)
		} else if math.IsInf(r.lockAt, 1) {
			r.lockAt = t
		}
		if ev.Locked && t > beatEvalSkip {
			locked++
			j := min(sort.SearchFloat64s(refT, t), len(refP)-1)
			ratio := float64(p.beat.Period) / 62.5 / refP[j]
			switch {
			case math.Abs(ratio-1) < 0.04:
				perOK++
			case math.Abs(ratio-2) < 0.08 || math.Abs(ratio-0.5) < 0.02:
				perOct++
			}
		}
	}
	if locked > 0 {
		r.perOK = float64(perOK) / float64(locked)
		r.perOct = float64(perOct) / float64(locked)
	}
	return r
}

// matchBeats returns the fraction of ref within tol of some x, the count of x
// within tol of no ref, and the signed offsets x-ref of the matches.
func matchBeats(ref, x []float64, tol, from float64) (hit float64, extra int, offs []float64) {
	var n, h int
	for _, t := range ref {
		if t < from {
			continue
		}
		n++
		j := sort.SearchFloat64s(x, t-tol)
		if j < len(x) && x[j] <= t+tol {
			h++
			// The nearest one.
			best := x[j]
			for k := j + 1; k < len(x) && x[k] <= t+tol; k++ {
				if math.Abs(x[k]-t) < math.Abs(best-t) {
					best = x[k]
				}
			}
			offs = append(offs, best-t)
		}
	}
	for _, t := range x {
		if t < from {
			continue
		}
		j := sort.SearchFloat64s(ref, t-tol)
		if !(j < len(ref) && ref[j] <= t+tol) {
			extra++
		}
	}
	if n == 0 {
		return 0, extra, offs
	}
	return float64(h) / float64(n), extra, offs
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return math.NaN()
	}
	w := append([]float64(nil), v...)
	sort.Float64s(w)
	return w[len(w)/2]
}

// beatScore is one device pair's result on one file.
type beatScore struct {
	lockA, lockB   float64
	hitA, hitB     float64
	extraA, extraB float64
	offA, offB     float64
	pair           float64
	perA, perB     float64
	octA, octB     float64
	patAgree       float64
	phase          [8]int // device beats by phase within the reference beat, both devices
}

func (s beatScore) String() string {
	return fmt.Sprintf("lock %4.1f/%4.1f s  hit %.2f/%.2f  extra %.2f/%.2f  off %+3.0f/%+3.0f ms  pair %.2f  per %.2f/%.2f (oct %.2f/%.2f)  pattern %.2f  phase %v",
		s.lockA, s.lockB, s.hitA, s.hitB, s.extraA, s.extraB, s.offA*1000, s.offB*1000,
		s.pair, s.perA, s.perB, s.octA, s.octB, s.patAgree, s.phase)
}

func scoreBeatPair(refT, refP []float64, a, b beatRun) beatScore {
	var s beatScore
	nref := 0
	for _, t := range refT {
		if t >= beatEvalSkip {
			nref++
		}
	}
	var offs []float64
	var ex int
	s.hitA, ex, offs = matchBeats(refT, a.beats, beatTol, beatEvalSkip)
	s.extraA, s.offA = float64(ex)/float64(max(nref, 1)), median(offs)
	s.hitB, ex, offs = matchBeats(refT, b.beats, beatTol, beatEvalSkip)
	s.extraB, s.offB = float64(ex)/float64(max(nref, 1)), median(offs)
	s.pair, _, _ = matchBeats(a.beats, b.beats, pairTol, beatEvalSkip)
	// Where in the reference beat the device beats fall: bin 0 is on the beat,
	// bin 4 the off-beat. A tracker that locks in anti-phase piles up in 4.
	for _, x := range append(append([]float64(nil), a.beats...), b.beats...) {
		j := sort.SearchFloat64s(refT, x)
		if x < beatEvalSkip || j == 0 || j >= len(refT) {
			continue
		}
		ph := (x - refT[j-1]) / (refT[j] - refT[j-1])
		s.phase[int(ph*8+0.5)%8]++
	}
	s.lockA, s.lockB = a.lockAt, b.lockAt
	s.perA, s.perB, s.octA, s.octB = a.perOK, b.perOK, a.perOct, b.perOct
	// Pattern agreement on room time: B's pattern at the time of each of A's
	// frames.
	var same, n int
	for i, t := range a.frameT {
		if t < beatEvalSkip+10 {
			continue
		}
		j := sort.SearchFloat64s(b.frameT, t)
		if j >= len(b.frameT) {
			break
		}
		n++
		if a.patterns[i] == b.patterns[j] {
			same++
		}
	}
	if n > 0 {
		s.patAgree = float64(same) / float64(n)
	}
	return s
}

func TestBeatEval(t *testing.T) {
	paths := os.Getenv("BEATWAV")
	if paths == "" {
		t.Skip("set BEATWAV to comma-separated 16 kHz mono PCM16 WAVs")
	}
	var sum beatScore
	var count float64
	var phase [8]int
	for _, path := range strings.Split(paths, ",") {
		src := readPCM16(t, path)
		refT, refP := loadRef(t, path, src)
		t.Logf("%s: %d reference beats, median period %.1f ms (%.1f BPM)",
			shortName(path), len(refT), median(refP)*1000, 60/median(refP))
		for _, sc := range scenarios() {
			a := runBeatDevice(src, sc.a, refT, refP)
			b := runBeatDevice(src, sc.b, refT, refP)
			s := scoreBeatPair(refT, refP, a, b)
			t.Logf("  %-8s %v", strings.SplitN(sc.name, ":", 2)[0], s)
			sum.hitA += s.hitA + s.hitB
			sum.pair += s.pair
			sum.perA += s.perA + s.perB
			sum.octA += s.octA + s.octB
			sum.extraA += s.extraA + s.extraB
			sum.patAgree += s.patAgree
			count++
			for i := range phase {
				phase[i] += s.phase[i]
			}
		}
	}
	t.Logf("MEAN hit %.3f  extra %.3f  pair %.3f  per %.3f  oct %.3f  pattern %.3f  phase %v",
		sum.hitA/count/2, sum.extraA/count/2, sum.pair/count, sum.perA/count/2, sum.octA/count/2, sum.patAgree/count, phase)
}

// TestBeatHears runs the two simultaneous earring recordings (hears-left.wav,
// hears-right.wav: what each earring's microphone heard of the same music)
// through two pipelines, as two earrings in the same room. Their lag is found
// from the envelopes and taken out, so "pair" measures the trackers.
func TestBeatHears(t *testing.T) {
	pl, pr := os.Getenv("HEARSL"), os.Getenv("HEARSR")
	if pl == "" || pr == "" {
		t.Skip("set HEARSL and HEARSR")
	}
	l, r := readPCM16(t, pl), readPCM16(t, pr)
	// Coarse lag from the hop-64 onset envelopes, +-1 s.
	el, er := refOnset(l, 64), refOnset(r, 64)
	bestLag, bestC := 0, -1.0
	for lag := -250; lag <= 250; lag++ {
		var c float64
		for i := 250; i+250 < min(len(el), len(er)); i++ {
			c += el[i] * er[i+lag]
		}
		if c > bestC {
			bestLag, bestC = lag, c
		}
	}
	lagS := float64(bestLag) * 64 / 16000
	t.Logf("R lags L by %+.0f ms", lagS*1000)
	refT, refP := loadRef(t, pl, l)
	t.Logf("%d reference beats (from L), %.1f BPM", len(refT), 60/median(refP))
	d := device{rms: 200}
	a := runBeatDevice(l, d, refT, refP)
	b := runBeatDevice(r, d, refT, refP)
	for i := range b.beats {
		b.beats[i] -= lagS
	}
	for i := range b.frameT {
		b.frameT[i] -= lagS
	}
	t.Logf("%v", scoreBeatPair(refT, refP, a, b))
}

// loadRef returns the reference beats for a recording: from BEATREFDIR/<name>.beats
// (one time in seconds per line) when that exists, else refBeats. With a file
// it also reports the phase of refBeats against it.
func loadRef(t *testing.T, path string, src []int16) ([]float64, []float64) {
	dir := os.Getenv("BEATREFDIR")
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	data, err := os.ReadFile(filepath.Join(dir, name+".beats"))
	if dir == "" || err != nil {
		return refBeats(src)
	}
	var times []float64
	for _, f := range strings.Fields(string(data)) {
		v, err := strconv.ParseFloat(f, 64)
		if err != nil {
			t.Fatal(err)
		}
		times = append(times, v)
	}
	periods := make([]float64, len(times))
	for i := range times {
		a, b := max(i-4, 0), min(i+4, len(times)-1)
		periods[i] = (times[b] - times[a]) / float64(b-a)
	}
	return times, periods
}
