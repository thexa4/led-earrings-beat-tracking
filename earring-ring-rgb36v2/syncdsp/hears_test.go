package syncdsp

// Ground truth: two earrings listening to the same real sound, recorded off the
// devices (tools/micdump.py) and replayed here side by side.
//
// This is the only measurement in the suite that uses a real front end. Every
// other capability number in this package comes from frontEnd() in bench_test.go,
// which is a model - a good one, matched to tools/freeze.py - but a model, and
// the whole exercise of the last few sessions has been discovering where the model
// and the hardware disagree. So this file exists to answer the question the task
// actually asks ("do the two earrings pick the same colour") with recordings, and
// to say how far the model's prediction was from what happened.
//
//	HEARSL=/tmp/psync/gt1/hears-left.wav HEARSR=/tmp/psync/gt1/hears-right.wav \
//	  go test -count=1 -v -run TestHears ./syncdsp/
//
// What the recordings are, and the one thing they cannot tell us: the dump is
// 8-bit, DC-centred, and divided by a shift chosen from the AGC's own peak, so the
// waveform is faithful but the ABSOLUTE level is not recoverable - a byte of 40
// could be 40 codes or 640. The files therefore cannot be used to set
// SilenceEnterQ8, which is defined in codes at the microphone; tools/livelvl.py
// measures that directly. Everything level-blind - the hash, the pattern, the
// beat - is exactly what the dump preserves, which is what is being compared.
//
// The dump is also at 8 kSPS (2-tap decimated from the device's 16 kSPS), so it is
// interpolated back up here. That is honest below ~4 kHz, where nearly all the
// music's energy is, and soft in the top band.

import (
	"encoding/binary"
	"fmt"
	"math/bits"
	"os"
	"sort"
	"testing"
)

// readWav returns PCM16 mono samples and the rate. Tolerates the chunks
// ffmpeg/soak leave around data.
func readWav(t *testing.T, path string) (int, []int16) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("no recording at %s: %v (run tools/micdump.py)", path, err)
	}
	rate, ch := 0, 1
	var data []byte
	for i := 12; i+8 <= len(b); {
		id, n := string(b[i:i+4]), int(binary.LittleEndian.Uint32(b[i+4:i+8]))
		switch id {
		case "fmt ":
			rate = int(binary.LittleEndian.Uint32(b[i+12 : i+16]))
			ch = int(binary.LittleEndian.Uint16(b[i+10 : i+12]))
		case "data":
			data = b[i+8 : i+8+n]
		}
		i += 8 + n + n%2
	}
	if rate == 0 || data == nil {
		t.Fatalf("%s: no fmt/data chunk", path)
	}
	if ch != 1 {
		t.Fatalf("%s: %d channels, expected mono", path, ch)
	}
	out := make([]int16, len(data)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(data[2*i:]))
	}
	return rate, out
}

// upsampleTo linearly interpolates to want samples per input sample, which is how
// an 8 kSPS dump is fed to a pipeline fixed at 16 kSPS.
func upsampleTo(x []int16, want int) []int16 {
	out := make([]int16, len(x)*want)
	for i := range out {
		j := i / want
		f := int32(i%want) * 256 / int32(want)
		a, b := int32(x[j]), int32(x[j])
		if j+1 < len(x) {
			b = int32(x[j+1])
		}
		out[i] = int16(a + (b-a)*f/256)
	}
	return out
}

// hearsScale puts the dump's byte-scale deviations back into something the ADC
// could have produced. Any constant works - the pipeline is level-blind by design
// - but it has to be big enough that the AGC's floor and the band quantisation are
// well below the signal, so 64x on a +-100 peak: a 6400-code swing, a normal level
// for this front end.
const hearsScale = 64

func TestHears(t *testing.T) {
	lp, rp := os.Getenv("HEARSL"), os.Getenv("HEARSR")
	if lp == "" || rp == "" {
		t.Skip("HEARSL/HEARSR not set")
	}
	lr, ld := readWav(t, lp)
	rr, rd := readWav(t, rp)
	if lr != 8000 || rr != 8000 {
		t.Fatalf("expected the 8 kSPS dump, got %d and %d Hz", lr, rr)
	}
	L, R := upsampleTo(ld, 2), upsampleTo(rd, 2)
	n := len(L)
	if len(R) < n {
		n = len(R)
	}
	n -= n % Window
	L, R = L[:n], R[:n]

	// Where do the two microphones actually land on each other? Both files are on
	// one wall-clock grid, so any residual lag is the sound travelling between the
	// two earrings plus the two converters. Correlating the envelopes finds it, and
	// the comparison is then run at zero lag too, which is the experiment that says
	// whether disagreement is a timing problem or a content problem.
	lag := bestLag(L, R)
	t.Logf("%s samples = %.1f s at %d kSPS; envelope best lag %+d samples (%.1f ms)",
		"paired", float64(n)/16000, 16, lag, float64(lag)/16)

	for _, off := range []int{0, lag} {
		compare(t, L, R, off)
	}
}

// bestLag returns the shift (in 16 kSPS samples) that maximises the correlation
// between |R| and |L| delayed by that shift. Positive means R is behind L.
func bestLag(L, R []int16) int {
	env := func(x []int16) []int64 {
		e := make([]int64, len(x)/160)
		for i := range e {
			var s int64
			for _, v := range x[i*160 : (i+1)*160] {
				if v < 0 {
					v = -v
				}
				s += int64(v)
			}
			e[i] = s
		}
		return e
	}
	el, er := env(L), env(R)
	best, bv := 0, int64(-1)
	for k := -40; k <= 40; k++ {
		var s int64
		for i := range el {
			j := i + k/10
			if k < 0 {
				j = i + (k-9)/10
			}
			if j < 0 || j >= len(er) {
				continue
			}
			s += el[i] * er[j]
		}
		if s > bv {
			best, bv = k*10, s
		}
	}
	return best
}

func compare(t *testing.T, L, R []int16, lag int) {
	var pl, pr Pipeline
	pl.Init()
	pr.Init()
	var agree, both, bothLock, lockL, lockR, silL, silR int
	dist := map[int]int{}
	var first []string
	patA := map[int32]int{}
	patB := map[int32]int{}
	// Per-field disagreement. The hash is three independent fields and they are not
	// equally trustworthy: the temporal bits compare against each device's own
	// previous push, so they churn on each device's own schedule, while shape and
	// hot are comparisons inside one spectrum. Averaging them into one Hamming
	// distance hides which one costs the agreement.
	var fieldBits [3]int
	var fieldDiff [3]int
	// How far the winning band beats the runner-up, in Q8 log2 units, split by
	// whether the two devices agreed. If disagreement concentrates at small margins
	// the problem is a tie being broken differently, which hysteresis can fix; if it
	// happens at large margins too, the two spectra genuinely differ and nothing
	// downstream can help.
	var winAgree, winDis []int32
	var centAgree, centN int
	frames := len(L) / Window
	for f := range frames {
		s := f * Window
		for i := range Window {
			pl.ProcessSample(uint16(32768 + hearsScale*int32(L[s+i])))
		}
		el := pl.EndFrame()
		for i := range Window {
			j := s + i + lag
			if j < 0 {
				j = 0
			}
			if j >= len(R) {
				break
			}
			pr.ProcessSample(uint16(32768 + hearsScale*int32(R[j])))
		}
		er := pr.EndFrame()
		if f < WarmupFrames {
			continue
		}
		if el.Locked {
			lockL++
		}
		if er.Locked {
			lockR++
		}
		if el.Silent {
			silL++
		}
		if er.Silent {
			silR++
		}
		// The number the task is about: the debug colour is the pattern, so this
		// is "same colour or not", counted on the frames where both earrings are
		// actually deciding.
		both++
		if el.Pattern == er.Pattern {
			agree++
		}
		if el.Locked && er.Locked {
			bothLock++
		}
		hash := el.Hash ^ er.Hash
		dist[bits.OnesCount32(hash)]++
		for i, m := range [3]uint32{(1 << NBands) - 1,
			(1 << (2 * (NBands - 1))) - 1<<ShapeShift,
			uint32((1<<NBands)-1) << ContrastShift} {
			c := bits.OnesCount32(hash & m)
			fieldBits[i] += c
			if c > 0 {
				fieldDiff[i]++
			}
		}
		patA[el.Pattern]++
		patB[er.Pattern]++
		dl, _ := spectralDevs(&pl.smoothSnap), spectralDevs(&pr.smoothSnap)
		// The shipped selector, read straight off each pipeline: what the device
		// would be showing at this frame, not a recomputation from the hash.
		if pl.pattern() == pr.pattern() {
			centAgree++
		}
		centN++
		m := func(d *[NBands]int32) int32 {
			top, next := d[1], int32(-1<<30)
			for k := int32(2); k < NBands-1; k++ {
				switch {
				case d[k] > top:
					next = top
					top = d[k]
				case d[k] > next:
					next = d[k]
				}
			}
			return top - next
		}
		if el.Pattern == er.Pattern {
			winAgree = append(winAgree, m(&dl))
		} else {
			winDis = append(winDis, m(&dl))
		}
		if el.Pattern != er.Pattern && len(first) < 8 && f%7 == 0 {
			first = append(first, fmt.Sprintf("f%d L p%d h%08x%s R p%d h%08x%s",
				f, el.Pattern, el.Hash, flag(el), er.Pattern, er.Hash, flag(er)))
		}
	}
	t.Logf("lag %+d: %d frames  locked L %.0f%% R %.0f%%  silent L %.0f%% R %.0f%%",
		lag, both, 100*float64(lockL)/float64(both), 100*float64(lockR)/float64(both),
		100*float64(silL)/float64(both), 100*float64(silR)/float64(both))
	t.Logf("lag %+d: PATTERN AGREEMENT %5.1f%% over all deciding frames, %5.1f%% over "+
		"frames where both are locked (%d)",
		lag, 100*float64(agree)/float64(max(both, 1)),
		100*float64(agree)/float64(max(bothLock, 1)), bothLock)
	t.Logf("lag %+d: hash Hamming distance %s", lag, distString(dist))
	names := [3]string{"temporal", "shape   ", "hot     "}
	for i := range fieldBits {
		t.Logf("lag %+d: %s field differs on %5.1f%% of frames, %.2f bits/frame of %d",
			lag, names[i], 100*float64(fieldDiff[i])/float64(both),
			float64(fieldBits[i])/float64(both), [3]int{NBands, 2 * (NBands - 1), NBands}[i])
	}
	t.Logf("lag %+d: centroid(dev) agrees %5.1f%%; winner-beats-runner-up margin, agree p50 %5.2f bits, "+
		"disagree p50 %5.2f bits",
		lag, 100*float64(centAgree)/float64(max(centN, 1)),
		medianQ8(winAgree)/256, medianQ8(winDis)/256)
	t.Logf("lag %+d: patterns used L %d R %d; L spread %s", lag, len(patA), len(patB),
		topString(patA, 6))
	for _, s := range first {
		t.Logf("  disagree: %s", s)
	}
}

func medianQ8(v []int32) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]int32{}, v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return float64(s[len(s)/2])
}

func flag(e Events) string {
	s := ""
	if e.Silent {
		s += "S"
	}
	if !e.Locked {
		s += "L"
	}
	if s == "" {
		s = "-"
	}
	return s
}

func distString(d map[int]int) string {
	ks := make([]int, 0, len(d))
	for k := range d {
		ks = append(ks, k)
	}
	sort.Ints(ks)
	total := 0
	for _, k := range ks {
		total += d[k]
	}
	out := ""
	for _, k := range ks {
		if k > 12 && d[k]*100 < total {
			continue
		}
		out += fmt.Sprintf("%d:%.1f%% ", k, 100*float64(d[k])/float64(total))
	}
	return out
}

func topString(m map[int32]int, n int) string {
	type kv struct {
		k int32
		v int
	}
	all := make([]kv, 0, len(m))
	tot := 0
	for k, v := range m {
		all = append(all, kv{k, v})
		tot += v
	}
	sort.Slice(all, func(i, j int) bool { return all[i].v > all[j].v })
	out := ""
	for i, e := range all {
		if i >= n {
			break
		}
		out += fmt.Sprintf("%d:%.0f%% ", e.k, 100*float64(e.v)/float64(tot))
	}
	return out
}
