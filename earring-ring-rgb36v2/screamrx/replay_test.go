package screamrx

// Replay of real recordings (tools/screamdump.py) into both decoders, and an
// oracle analysis of what the microphone heard against the known stream.
//
//	REPLAY=../captures/scream/anim-cfg0/L.wav DATA=../scream/animation.bin \
//	    go test ./screamrx -run TestReplay -v
//
// The recording is in ADC codes relative to the tracked DC at the device's
// actual sample rate. PSC trims are emulated by resampling (cubic), window
// skips and adjustments by index.

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"math/bits"
	"math/cmplx"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codeberg.org/maaike328p/datatrans"
	"codeberg.org/maaike328p/datatrans/dsp"
)

func readWav(t *testing.T, name string) []float64 {
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	// Find the data chunk.
	p := 12
	for p+8 <= len(b) {
		id := string(b[p : p+4])
		n := int(binary.LittleEndian.Uint32(b[p+4:]))
		if id == "data" {
			b = b[p+8 : p+8+n]
			break
		}
		p += 8 + n
	}
	x := make([]float64, len(b)/2)
	for i := range x {
		x[i] = float64(int16(binary.LittleEndian.Uint16(b[2*i:])))
	}
	return x
}

// recordedRate reads the device's fitted sample rate from meta.json next to
// the recording (screamdump.py), or returns the nominal rate.
func recordedRate(t *testing.T, wav string) float64 {
	b, err := os.ReadFile(filepath.Join(filepath.Dir(wav), "meta.json"))
	if err != nil {
		return fs
	}
	var meta struct {
		Devices map[string]struct{ Rate float64 }
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		t.Fatal(err)
	}
	side := strings.TrimSuffix(filepath.Base(wav), ".wav")
	if d, ok := meta.Devices[side]; ok && d.Rate > 0 {
		return d.Rate
	}
	return fs
}

// playIndex reads the sample index at which the player was started.
func playIndex(t *testing.T, wav string) int {
	b, err := os.ReadFile(filepath.Join(filepath.Dir(wav), "meta.json"))
	if err != nil {
		return 0
	}
	var meta struct {
		Devices map[string]struct {
			PlayIndex int `json:"play_index"`
		}
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		t.Fatal(err)
	}
	return meta.Devices[strings.TrimSuffix(filepath.Base(wav), ".wav")].PlayIndex
}

// resample converts x to the nominal rate: step is input samples per
// output sample.
func resample(x []float64, step float64) []float64 {
	out := make([]float64, 0, int(float64(len(x))/step))
	for p := 1.0; int(p)+3 < len(x); p += step {
		out = append(out, cubic(x, p))
	}
	return out
}

// cubic interpolates x at fractional position p.
func cubic(x []float64, p float64) float64 {
	i := int(math.Floor(p))
	if i < 1 || i+2 >= len(x) {
		return 0
	}
	f := p - float64(i)
	a, b, c, d := x[i-1], x[i], x[i+1], x[i+2]
	return b + 0.5*f*(c-a+f*(2*a-5*b+4*c-d+f*(3*(b-c)+d-a)))
}

// replayRun feeds a recording to a decoder like the firmware would, with
// trims as resampling. It returns the time to success or -1.
func replayRun(dec rxDecoder, x []float64, start float64, psc int) (float64, int) {
	raw := make([]uint16, symLen)
	win := make([]int32, symLen+2)
	win[symLen] = 0x7fff_ffff
	pos := start
	trim := 0
	step := 1.0
	fill := func() bool {
		for i := range raw {
			if int(pos)+3 >= len(x) {
				return false
			}
			raw[i] = uint16(max(0, min(65535, math.Round(30000+cubic(x, pos)))))
			pos += step
		}
		return true
	}
	fill()
	dc := dsp.CopyWithOffset(raw, win, 0)
	resets := 0
	for n := 0; ; n++ {
		if !fill() {
			return -1, resets
		}
		dc = dsp.CopyWithOffset(raw, win, dc)
		st := dec.process(win)
		if t := dec.trim(trim); t != trim {
			trim = t
			// PSC+trim: a larger prescaler samples slower, so each device
			// sample advances further through the recording.
			step = float64(psc+1+trim) / float64(psc+1)
		}
		pos += float64(dec.adjust()) * step
		switch {
		case st > 0:
			pos += float64(st) * step
		case st == StatusReceived:
			return float64(n+1) / datatrans.SymbolsPerSecond, resets
		case st < 0:
			dec.reset()
			resets++
		}
	}
}

func TestReplay(t *testing.T) {
	name := os.Getenv("REPLAY")
	if name == "" {
		t.Skip("set REPLAY=file.wav[,file.wav] and DATA=payload")
	}
	payload, err := os.ReadFile(os.Getenv("DATA"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range strings.Split(name, ",") {
		x := readWav(t, f)
		rate := recordedRate(t, f)
		t.Logf("%s: %d samples (%.1fs) at %.1f/s", f, len(x), float64(len(x))/fs, rate)
		oracle(t, resample(x, rate/fs), payload, int(float64(playIndex(t, f))*fs/rate))
		for _, start := range []float64{0, 13000, 27000, 41000} {
			for _, dn := range []string{"v1", "new"} {
				var dec rxDecoder
				psc := 209
				if dn == "v1" {
					dec = newV1(4)
				} else {
					dec = newRx(4, psc)
				}
				sec, resets := replayRun(dec, x, start, psc)
				ok := sec >= 0 && string(dec.bytes()) == string(payload)
				t.Logf("  start %5.1fs %-3s: ok=%v t=%.1fs resets=%d", start/fs, dn, ok, sec, resets)
				if r, isNew := dec.(*rx); isNew {
					t.Logf("      %+v", r.d.Stats)
				}
			}
		}
	}
}

// oracle aligns the recording to the known symbol stream (seed 12345, as
// screamcheck wav) and reports, per tone, the on/off magnitude separation
// at the best timing: the channel's eye, independent of any decoder.
func oracle(t *testing.T, x []float64, payload []byte, playIndex int) {
	stream := symbolStream(payload, 12345, len(x)/symLen+200)
	// Locate the stream: correlate window energy patterns. Coarse search for
	// the sample offset at which the 400 Hz-1950 Hz tone pattern of symbol s
	// matches, using DFT magnitudes over the first 8 s.
	mags := func(off int) [][nFreq]float64 {
		n := (len(x) - off) / symLen
		out := make([][nFreq]float64, n)
		for w := range n {
			seg := x[off+w*symLen : off+(w+1)*symLen]
			for k := range nFreq {
				f := float64(freqStart + freqStep*k)
				var z complex128
				for i, v := range seg {
					z += complex(v, 0) * cmplx.Rect(1, -2*math.Pi*f*float64(i)/fs)
				}
				out[w][k] = cmplx.Abs(z)
			}
		}
		return out
	}
	// Score an alignment over every window: the stream starts after the
	// player was started (AirPlay adds up to a few seconds), so s0 (the
	// stream symbol at window 0) is bounded by the recording's play index.
	lo, hi := -len(x)/symLen, 0
	if playIndex > 0 {
		hi = -playIndex / symLen
		lo = hi - 6*datatrans.SymbolsPerSecond
	}
	best, second, bestOff, bestS := -1e18, -1e18, 0, 0
	for off := 0; off < symLen; off += 5 {
		m := mags(off)
		// Remove each tone's mean so the score is a correlation.
		var mean [nFreq]float64
		for w := range m {
			for k := range nFreq {
				mean[k] += m[w][k] / float64(len(m))
			}
		}
		for s0 := lo; s0 <= hi; s0++ {
			score := 0.0
			for w := range m {
				s := s0 + w
				if s < 0 || s >= len(stream) {
					continue
				}
				for k := range nFreq {
					v := m[w][k] - mean[k]
					if stream[s]>>k&1 != 0 {
						score += v
					} else {
						score -= v
					}
				}
			}
			if score > best {
				if bestS != s0 {
					second = best
				}
				best, bestOff, bestS = score, off, s0
			} else if score > second && s0 != bestS {
				second = score
			}
		}
	}
	t.Logf("oracle: best score %.3g, runner-up %.3g", best, second)
	t.Logf("oracle: aligned window offset %d, stream symbol %d at window 0", bestOff, bestS)
	// Fine offset and per-tone stats; the device clock drifts, so align per
	// 2-second block.
	m := mags(bestOff)
	var onS, offS, onSS, offSS [nFreq]float64
	var onN, offN [nFreq]float64
	errs := 0
	total := 0
	for w := range m {
		s := bestS + w
		if s < 0 || s >= len(stream) {
			continue
		}
		for k := range nFreq {
			v := m[w][k]
			if stream[s]>>k&1 != 0 {
				onS[k] += v
				onSS[k] += v * v
				onN[k]++
			} else {
				offS[k] += v
				offSS[k] += v * v
				offN[k]++
			}
		}
	}
	line := ""
	for k := range nFreq {
		mOn, mOff := onS[k]/onN[k], offS[k]/offN[k]
		sOn := math.Sqrt(onSS[k]/onN[k] - mOn*mOn)
		sOff := math.Sqrt(offSS[k]/offN[k] - mOff*mOff)
		q := (mOn - mOff) / (sOn + sOff)
		line += " " + fmtQ(q)
		th := (mOn + mOff) / 2
		for w := range m {
			s := bestS + w
			if s < 0 || s >= len(stream) {
				continue
			}
			on := stream[s]>>k&1 != 0
			if (m[w][k] > th) != on {
				errs++
			}
			total++
		}
		if k%8 == 7 {
			t.Logf("  tones %2d-%2d on/off mean %s", k-7, k, fmtLevels(onS[k-7:k+1], onN[k-7:k+1], offS[k-7:k+1], offN[k-7:k+1]))
		}
	}
	t.Logf("oracle eye Q=(on-off)/(sd_on+sd_off) per tone:%s", line)
	// Echo profile: mean power of a tone given that it was on j symbols ago
	// (and off at every other lag in 0..8), relative to never on.
	const lags = 9
	var pw [lags + 1]float64
	var cnt [lags + 1]float64
	for w := lags; w < len(m); w++ {
		s := bestS + w
		if s-lags < 0 || s >= len(stream) {
			continue
		}
		for k := range nFreq {
			ons := 0
			lag := -1
			for j := range lags {
				if stream[s-j]>>k&1 != 0 {
					ons++
					lag = j
				}
			}
			if ons == 0 {
				pw[lags] += m[w][k] * m[w][k]
				cnt[lags]++
			} else if ons == 1 {
				pw[lag] += m[w][k] * m[w][k]
				cnt[lag]++
			}
		}
	}
	prof := ""
	for j := range lags {
		prof += sprintf(" %.1f", 10*math.Log10(pw[j]/cnt[j]/(pw[lags]/cnt[lags])))
	}
	t.Logf("echo profile: power (dB over never-on) when on only j symbols ago, j=0..%d:%s", lags-1, prof)
	// Distortion: the off tones' power by how many tones the symbol has on,
	// and by whether a neighbour is on. Noise is flat in both; intermodulation
	// in the speaker grows with the number of tones, leakage with neighbours.
	var byN, nN [5]float64
	var byNb, nNb [2]float64
	for w := range m {
		s := bestS + w
		if s < 0 || s >= len(stream) {
			continue
		}
		on := bits.OnesCount32(stream[s])
		for k := range nFreq {
			if stream[s]>>k&1 != 0 {
				continue
			}
			p := m[w][k] * m[w][k]
			b := min(4, max(0, (on-8)/4))
			byN[b] += p
			nN[b]++
			nb := 0
			if (k > 0 && stream[s]>>(k-1)&1 != 0) || (k < nFreq-1 && stream[s]>>(k+1)&1 != 0) {
				nb = 1
			}
			byNb[nb] += p
			nNb[nb]++
		}
	}
	dist := ""
	for b := range byN {
		if nN[b] > 0 {
			dist += sprintf(" %d+:%.1f", 8+4*b, 10*math.Log10(byN[b]/nN[b]/(byNb[0]/nNb[0])))
		}
	}
	t.Logf("off-tone power (dB over off with both neighbours off) by tones on:%s; with a neighbour on %.1f",
		dist, 10*math.Log10(byNb[1]/nNb[1]/(byNb[0]/nNb[0])))
	t.Logf("oracle per-tone threshold BER (fixed grid, no drift correction): %.2f%%", 100*float64(errs)/float64(total))
}

func fmtQ(q float64) string {
	return strings.TrimRight(strings.TrimRight(formatFloat(q, 1), "0"), ".")
}

func formatFloat(v float64, prec int) string {
	return strings.TrimSpace(func() string {
		s := make([]byte, 0, 8)
		s = append(s, []byte(fmtF(v, prec))...)
		return string(s)
	}())
}

func fmtF(v float64, prec int) string {
	p := math.Pow(10, float64(prec))
	r := math.Round(v*p) / p
	return strings.TrimSpace(strings.Replace(strings.Replace(
		func() string { return sprintf("%.*f", prec, r) }(), "-0.0", "0.0", 1), " ", "", -1))
}

func fmtLevels(onS, onN, offS, offN []float64) string {
	s := ""
	for i := range onS {
		s += sprintf(" %5.0f/%-5.0f", onS[i]/onN[i], offS[i]/offN[i])
	}
	return s
}

func sprintf(format string, a ...any) string { return fmt.Sprintf(format, a...) }
