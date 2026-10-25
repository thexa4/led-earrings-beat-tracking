package syncdsp

// Podcast-wide hash quality harness.
//
//	The problem TestAgreement cannot see: it measures agreement on one synth
//	patch, so a hash tuned against it overfits to that one arrangement. On real
//	podcast material the shipped hash sits on a single value (0x3fc00000) for
//	~90% of beats: hot saturates all 8 bits (EDM spans less than HotFloorQ8),
//	shape sits all-zero (the spectrum lies on the fitted line), temporal is all-
//	zero (the smoothed spectrum barely moves within a beat). A constant hash
//	matches every other hash, so "agreement" is trivially perfect and useless -
//	the ring cannot tell any moment from any other, and anchors almost never fire.
//
// Figure of merit: USABLE BITS, the mutual information the hash carries about
// the music through the device-to-device channel. For each of the 29 bits b:
//
//	act_b = fraction of (locked) frames where the reference sets bit b
//	err_b = fraction where a distorted device disagrees with the reference on b
//	MI_b  = H2(act_b) - H2(err_b), clipped at 0
//
// H2 is binary entropy. A bit stuck at 0 or 1 has H2(act)=0 (it says nothing
// about the music); a bit that is noise has H2(err)=1 (what it says is not
// reproducible). Either way it contributes 0. A bit that is ~half-set across
// the corpus AND agrees across devices contributes ~1. Summed over the word
// this is the number of bits of musical identity two earrings can actually
// share - it is 0 for the saturated hash and cannot be gamed by cranking a
// threshold until the word is noise.
//
// Hard constraint reported alongside: the steady-frame hash-distance p95 must
// stay <= DAccept, or ring matching misfires regardless of how many usable bits
// exist. Optimize usable bits, subject to p95 staying within DAccept.
//
//	Run over the whole podcast (never committed) with:
//	  PODCAST=/tmp/psync/full.wav go test -run TestPodcastHash -v ./syncdsp/
//	Restrict to a subset of the running time (seconds) with PODEND=300 for a
//	fast loop; the full pass is the one that counts.

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"sort"
	"sync"
	"testing"
)

// readWavPCM16 returns the data chunk of a PCM16 mono WAV as raw uint16
// (the pipeline consumes the biased bit pattern directly, as on the device).
func readWavPCM16(t *testing.T, path string) []uint16 {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var data []byte
	for i := 12; i+8 <= len(b); {
		id, n := string(b[i:i+4]), int(binary.LittleEndian.Uint32(b[i+4:i+8]))
		if id == "data" {
			data = b[i+8 : i+8+n]
		}
		i += 8 + n + n%2
	}
	out := make([]uint16, len(data)/2)
	for i := range out {
		out[i] = binary.LittleEndian.Uint16(data[2*i:])
	}
	return out
}

func h2(p float64) float64 {
	if p <= 0 || p >= 1 {
		return 0
	}
	return -(p*math.Log2(p) + (1-p)*math.Log2(1-p))
}

// runReal streams real audio through one distorted pipeline and returns the
// per-frame events. Mirrors instance.frame but reads data instead of Synth.
func runReal(data []uint16, dist func([]uint16, *int32), from, to int) []Events {
	var p Pipeline
	p.Init()
	win := make([]uint16, Window)
	var carry int32
	evs := make([]Events, to-from)
	for f := from; f < to; f++ {
		copy(win, data[f*Window:(f+1)*Window])
		if dist != nil {
			dist(win, &carry)
		}
		evs[f-from] = p.Process(win)
	}
	return evs
}

// fieldOf labels a bit index with its hash field for the per-field report.
func fieldOf(b int) string {
	switch {
	case b < NBands:
		return "temporal"
	case b < ContrastShift:
		return "shape"
	default:
		return "hot"
	}
}

// noisy adds white Gaussian noise of the given amplitude (ADC counts, around
// the mid-rail the pipeline centres on). It is the only distortion in the
// model set that is *independent per run*: gain, tilt and the bench offset are
// systematic earring-to-earring differences, but two captures - or the same
// earring twice - never share their microphone hiss and ADC noise. Measured
// on the bench this dominates: two device runs of the same 55 s clip, aligned
// to the same instant, differ by ~3.4 bits of identity with nothing else
// changed, because the band thresholds sit close to the noise floor of a
// signal that reaches the mic as a few tens of counts. The reference gets a
// noise model too (different seed), so every err_b below is the realistic
// two-device mismatch rather than clean-against-noisy.
//
// Box-Muller over a seeded xorshift: reproducible, and cheap enough that the
// full-podcast pass stays dominated by the pipeline.
func noisy(amp float64, seed uint32) func([]uint16, *int32) {
	s := seed | 1
	next := func() float64 {
		s ^= s << 13
		s ^= s >> 17
		s ^= s << 5
		return (float64(s) + 0.5) / 4294967296
	}
	return func(w []uint16, _ *int32) {
		for i, v := range w {
			u1, u2 := next(), next()
			g := math.Sqrt(-2*math.Log(u1)) * math.Cos(2*math.Pi*u2)
			y := 32768 + float64(int32(v)-32768) + amp*g
			if y > 65535 {
				y = 65535
			} else if y < 0 {
				y = 0
			}
			w[i] = uint16(y)
		}
	}
}

// spk models what the laptop's own speaker does to the signal before the mic
// hears it: a small driver high-passes everything below ~350 Hz at roughly
// 12 dB/octave. Measured: a device run and the clean host pipeline of the
// same clip disagree by ~7 identity bits - more than every gain/tilt/offset
// model combined - and the bass shelf is the obvious suspect, since the two
// device runs still agree with each other far better than either agrees with
// the host. (The room/mic EQ that rides along is systematic between runs, so
// it does not hurt cross-device agreement; this entry measures how wrong the
// host's notion of "clean" is, and how much of the hash survives the shelf.)
func spk(w []uint16, carry *int32) {
	// y = x - lp(x): one-pole high-pass, corner ~350 Hz at 16 kSPS.
	const a = 0.87 // exp(-2*pi*350/16000)
	lp := float64(*carry)
	for i, v := range w {
		x := float64(int32(v) - 32768)
		lp = a*lp + (1-a)*x
		y := x - lp
		if y > 32767 {
			y = 32767
		} else if y < -32768 {
			y = -32768
		}
		w[i] = uint16(int32(y) + 32768)
	}
	*carry = int32(lp)
}

func TestPodcastHash(t *testing.T) {
	path := os.Getenv("PODCAST")
	if path == "" {
		t.Skip("PODCAST not set")
	}
	data := readWavPCM16(t, path)
	frames := len(data) / Window
	if e := os.Getenv("PODEND"); e != "" {
		var sec float64
		fmt.Sscanf(e, "%f", &sec)
		if n := int(sec * 64); n < frames {
			frames = n
		}
	}
	if s := os.Getenv("PODSTART"); s != "" {
		var sec float64
		fmt.Sscanf(s, "%f", &sec)
		if n := int(sec * 64); n < frames {
			data = data[n*Window:]
			frames -= n
		}
	}
	t.Logf("podcast: %d frames (%.0f s)", frames, float64(frames)/64)

	// Device models spanning the plausible earring-to-earring differences.
	devs := []struct {
		name string
		dist func([]uint16, *int32)
	}{
		{"clean", nil},
		{"gain-6dB", halfGain},
		{"hf-tilt", hfTilt},
		{"gain+tilt", combo},
		{"quiet", quiet},
		{"bench", benchOffset},
		{"spk-hp", spk},
		{"rerun", nil},
	}

	// NOISE=<counts> turns on the per-run noise model (see noisy). The
	// reference runs noisy too, with a different seed, so the reported err_b
	// is two noisy devices, not one clean and one noisy.
	var noiseAmp float64
	if e := os.Getenv("NOISE"); e != "" {
		fmt.Sscanf(e, "%f", &noiseAmp)
	}
	mk := func(base func([]uint16, *int32), seed uint32) func([]uint16, *int32) {
		n := noisy(noiseAmp, seed)
		return func(w []uint16, c *int32) {
			if base != nil {
				base(w, c)
			}
			if os.Getenv("DEVICE") != "" {
				// The bench acoustic channel: laptop speaker (LF gone) at
				// the measured distance (few tens of counts). Shared by
				// every model including the reference, so the systematic
				// part of the channel cancels exactly as it does between
				// two earrings, and what is left is what two runs cannot
				// agree on.
				spk(w, c)
				quiet(w, nil)
			}
			if noiseAmp != 0 {
				n(w, c)
			}
		}
	}

	// Run every device model in parallel (32 cores, ~6 pipelines).
	results := make([][]Events, len(devs))
	var wg sync.WaitGroup
	for i, d := range devs {
		wg.Add(1)
		go func(i int, dist func([]uint16, *int32)) {
			defer wg.Done()
			results[i] = runReal(data, mk(dist, uint32(0x9e3779b1*(i+1))), 0, frames)
		}(i, d.dist)
	}
	wg.Wait()

	ref := results[0]

	// HASHDUMP=<path> writes the reference hash per frame (hex, one/line,
	// 8 chars + \n) so an analysis script can line it up against a device
	// capture of the same clip.
	if path := os.Getenv("HASHDUMP"); path != "" {
		var buf []byte
		for _, e := range ref {
			buf = append(buf, fmt.Sprintf("%08x\n", e.Hash)...)
		}
		os.WriteFile(path, buf, 0o644)
	}

	// Distinct hashes over locked frames: raw sanity, and the modal fraction
	// (the saturation indicator that started this).
	var modalHash uint32
	{
		cnt := map[uint32]int{}
		for _, e := range ref {
			if e.Locked {
				cnt[e.Hash]++
			}
		}
		best := 0
		for h, c := range cnt {
			if c > best {
				best, modalHash = c, h
			}
		}
		n := 0
		for _, c := range cnt {
			n += c
		}
		t.Logf("reference: %d locked frames, %d distinct hashes, modal %08x = %.0f%%",
			n, len(cnt), modalHash, 100*float64(best)/math.Max(float64(n), 1))
	}

	// Per-bit usable bits, and steady distance p95, for each device model.
	// "Chosen" frames are locked (the music the feature actually runs on);
	// steady excludes the ~SmoothShift frames right after a hash change.
	type stat struct {
		mi      float64
		p95     int
		meanD   float64
		idMean  float64 // identity-only distance over all locked frames
		idLe3   float64
		fieldMI map[string]float64
		dead    int // bits with MI ~ 0
	}
	stats := make([]stat, len(devs))
	var wg2 sync.WaitGroup
	for i := 1; i < len(devs); i++ {
		wg2.Add(1)
		go func(i int) {
			defer wg2.Done()
			dev := results[i]
			const nBits = ContrastShift + NBands
			var setBit [32]int
			var errBit [32]int
			nChosen := 0
			var idsum, idle3 int
			var ds []int
			stableRun := 0
			prev := uint32(0)
			for f := range ref {
				if f < int(WarmupFrames) {
					continue
				}
				if ref[f].Hash != prev {
					stableRun = 0
					prev = ref[f].Hash
				} else {
					stableRun++
				}
				if !ref[f].Locked || !dev[f].Locked {
					continue
				}
				nChosen++
				id := int(hashDist(ref[f].Hash&IdentityMask, dev[f].Hash&IdentityMask))
				idsum += id
				if id <= 3 {
					idle3++
				}
				for b := 0; b < nBits; b++ {
					m := uint32(1) << uint(b)
					r := ref[f].Hash & m
					d := dev[f].Hash & m
					if r != 0 {
						setBit[b]++
					}
					if r != d {
						errBit[b]++
					}
				}
				if stableRun >= 1<<SmoothShift {
					hd := int(hashDist(ref[f].Hash, dev[f].Hash))
					ds = append(ds, hd)
				}
			}
			s := stat{fieldMI: map[string]float64{}}
			var sum float64
			for b := 0; b < nBits; b++ {
				act := float64(setBit[b]) / math.Max(float64(nChosen), 1)
				err := float64(errBit[b]) / math.Max(float64(nChosen), 1)
				mi := h2(act) - h2(err)
				if mi < 0 {
					mi = 0
				}
				sum += mi
				s.fieldMI[fieldOf(b)] += mi
				if mi < 0.02 {
					s.dead++
				}
			}
			s.mi = sum
			s.idMean = float64(idsum) / math.Max(float64(nChosen), 1)
			s.idLe3 = 100 * float64(idle3) / math.Max(float64(nChosen), 1)
			sort.Ints(ds)
			if len(ds) > 0 {
				s.meanD = float64(sum0(ds)) / float64(len(ds))
				s.p95 = ds[len(ds)*95/100]
			}
			stats[i] = s
		}(i)
	}
	wg2.Wait()

	t.Logf("field bits: temporal=%d shape=%d hot=%d (total %d)",
		NBands, 2*(NBands-1), NBands, ContrastShift+NBands)
	for i := 1; i < len(devs); i++ {
		s := stats[i]
		t.Logf("%-10s usable=%.2f bits  (temporal %.2f  shape %.2f  hot %.2f)  dead=%2d  steady d mean=%.2f p95=%d  ident mean=%.2f <=3=%.0f%%",
			devs[i].name, s.mi, s.fieldMI["temporal"], s.fieldMI["shape"],
			s.fieldMI["hot"], s.dead, s.meanD, s.p95, s.idMean, s.idLe3)
	}

	// Anchor behaviour: the anchor is the reset point the whole sync relies on,
	// so a good hash must fire anchors - and fire them on the SAME transitions
	// on every device. Greedy nearest-match within +/-30 frames, matching
	// matchAnchors in agreement_test.go: devices lock at different points, so
	// what must agree is the musical transition, not the exact frame.
	anchorIdx := func(evs []Events) []int {
		var v []int
		for f := range evs {
			if f >= int(WarmupFrames) && evs[f].Anchor {
				v = append(v, f)
			}
		}
		return v
	}
	refA := anchorIdx(ref)
	t.Logf("reference anchors: %d (%.1f/min)", len(refA),
		float64(len(refA))*64*60/float64(frames))
	const anchorTol = 30
	for i := 1; i < len(devs); i++ {
		devA := anchorIdx(results[i])
		used := make([]bool, len(devA))
		matched := 0
		for _, a := range refA {
			best, bd := -1, anchorTol+1
			for j, b := range devA {
				if !used[j] {
					d := a - b
					if d < 0 {
						d = -d
					}
					if d < bd {
						bd, best = d, j
					}
				}
			}
			if best >= 0 {
				used[best] = true
				matched++
			}
		}
		f1 := 2 * float64(matched) / math.Max(float64(len(refA)+len(devA)), 1)
		t.Logf("anchors vs %-9s matched=%d onlyRef=%d onlyDev=%d F1=%.2f",
			devs[i].name, matched, len(refA)-matched, len(devA)-matched, f1)
	}

	// Headline: usable bits against the worst realistic device (level+tilt),
	// and the p95 constraint on that same worst device.
	worst := stats[3] // gain+tilt
	t.Logf("HEADLINE usable-bits(worst=gain+tilt)=%.2f  p95=%d (DAccept=%d)",
		worst.mi, worst.p95, DAccept)
	if worst.p95 > DAccept {
		t.Errorf("steady distance p95=%d exceeds DAccept=%d on worst device", worst.p95, DAccept)
	}
	if worst.mi < 1.0 {
		t.Errorf("only %.2f usable bits: hash carries almost no shared musical identity", worst.mi)
	}
}

func sum0(v []int) int {
	s := 0
	for _, x := range v {
		s += x
	}
	return s
}
