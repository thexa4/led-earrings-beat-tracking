package screamrx

import (
	"bytes"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"

	"codeberg.org/maaike328p/datatrans"
	"codeberg.org/maaike328p/datatrans/dsp"
)

// rxDecoder is what the harness drives: one 20 ms window in, a status out
// (as ProcessSamples: >0 skip that many samples, <0 error or done), plus
// the PSC trim the firmware would apply afterwards.
type rxDecoder interface {
	process(window []int32) int
	// trim returns the new PSC offset given the current one.
	trim(cur int) int
	bytes() []byte
	reset()
	// adjust returns a signed move of the next window (software indexing).
	adjust() int
}

// The firmware's buffers: a fountain region bounded below the window
// overlay (datarecv.go), 436 words.
const fountainWords = 436

// v1 wraps the upstream decoder exactly as datarecv.go drives it.
type v1 struct {
	d       datatrans.Decoder
	maxTrim int
}

func newV1(maxTrim int) *v1 {
	v := &v1{maxTrim: maxTrim}
	v.d.Initialize(make([]uint32, fountainWords), 1532, fs)
	v.d.Reset()
	return v
}

func (v *v1) process(w []int32) int { return int(v.d.ProcessSamples(w)) }
func (v *v1) bytes() []byte         { return v.d.Bytes() }
func (v *v1) reset()                { v.d.Reset() }
func (v *v1) adjust() int           { return 0 }
func (v *v1) trim(cur int) int {
	// datarecv.go: SpeedAdjust < 0 -> PSC+1 (slower), > 0 -> PSC-1.
	switch adj := v.d.SpeedAdjust(); {
	case adj < 0:
		return min(v.maxTrim, cur+1)
	case adj > 0:
		return max(-v.maxTrim, cur-1)
	}
	return cur
}

type runResult struct {
	ok      bool
	seconds float64
	resets  int
}

// run drives one receive attempt: the receiver starts listening at `start`
// seconds into the transmission and gives up after limit seconds.
func run(dec rxDecoder, ch *channel, stream []uint32, data []byte, start, limit float64, psc int, rng *rand.Rand) runResult {
	rx := newReceiver(ch, stream, start, psc, rng)
	raw := make([]uint16, symLen)
	window := make([]int32, symLen+2)
	window[symLen] = 0x7fff_ffff
	dc := uint32(0)
	// Prime the DC estimate like the firmware's first window.
	rx.fill(raw)
	dc = dsp.CopyWithOffset(raw, window, dc)
	windows := int(limit * datatrans.SymbolsPerSecond)
	resets := 0
	for n := range windows {
		rx.fill(raw)
		dc = dsp.CopyWithOffset(raw, window, dc)
		status := dec.process(window)
		if t := dec.trim(rx.trim); t != rx.trim {
			rx.setTrim(t)
		}
		if a := dec.adjust(); a != 0 {
			rx.skip(a)
		}
		switch {
		case status > 0:
			rx.skip(status)
		case status == int(datatrans.ErrCodeReceived):
			return runResult{ok: bytes.Equal(dec.bytes(), data), seconds: float64(n+1) / datatrans.SymbolsPerSecond, resets: resets}
		case status < 0:
			dec.reset()
			resets++
		}
		if rx.t > float64(len(stream))/datatrans.SymbolsPerSecond {
			break
		}
	}
	return runResult{seconds: limit, resets: resets}
}

// scenario builds a channel for one trial.
type scenario struct {
	name  string
	build func(r *rand.Rand) *channel
}

func dB(x float64) float64 { return math.Pow(10, x/20) }

// The scenario matrix. Amplitudes are ADC codes per tone; the right earring
// was measured at MAD 4-6 codes with the LEDs dark, the left at 1-2.
func scenarios() []scenario {
	speaker := func(r *rand.Rand, amp, noise, drrDB float64, extra []path) *channel {
		ps := []path{flatPath(0.0006, 1).shaped(r, 4, 0, 0)}
		ps = append(ps, extra...)
		ps = append(ps, reverb(r, 24, 0.4, -drrDB)...)
		return &channel{paths: ps, amplitude: amp, noise: noise, dc: 30000}
	}
	return []scenario{
		{"headphone", func(r *rand.Rand) *channel {
			return &channel{paths: []path{flatPath(0.0001, 1).shaped(r, 2, 0, 0)}, amplitude: 120, noise: 5, dc: 30000}
		}},
		{"spk10cm", func(r *rand.Rand) *channel {
			return speaker(r, 60, 6, 14, []path{flatPath(0.0012, -0.3).shaped(r, 3, 0, 0)})
		}},
		{"spk30cm", func(r *rand.Rand) *channel {
			return speaker(r, 25, 6, 8, []path{flatPath(0.0019, 0.5).shaped(r, 3, 0, 0), flatPath(0.0041, -0.35)})
		}},
		{"spk100cm", func(r *rand.Rand) *channel {
			return speaker(r, 10, 6, 2, []path{flatPath(0.0021, 0.6).shaped(r, 3, 0, 0), flatPath(0.0052, -0.5)})
		}},
		{"2way30cm", func(r *rand.Rand) *channel {
			// Woofer and tweeter both in band, offset 4 cm (0.12 ms), crossover 1.2 kHz.
			w := flatPath(0.0006, 1).shaped(r, 3, 0, 1200)
			tw := flatPath(0.00072, 1).shaped(r, 3, 1200, 0)
			ch := speaker(r, 25, 6, 8, []path{flatPath(0.0019, 0.5), flatPath(0.0041, -0.35)})
			ch.paths[0] = w
			ch.paths = append(ch.paths, tw)
			return ch
		}},
		{"stereo30cm", func(r *rand.Rand) *channel {
			// A second speaker 0.4-1.2 ms further away at -2..-5 dB.
			d := 0.0004 + r.Float64()*0.0008
			return speaker(r, 25, 6, 8, []path{flatPath(0.0006+d, dB(-2-3*r.Float64())).shaped(r, 4, 0, 0), flatPath(0.0019, 0.4)})
		}},
		{"spk10cmR", func(r *rand.Rand) *channel {
			// The right earring: 1000-2000 code ADC spikes on 2% of samples.
			ch := speaker(r, 60, 6, 14, []path{flatPath(0.0012, -0.3).shaped(r, 3, 0, 0)})
			ch.impulses = 0.02
			ch.impulseSz = 1300
			return ch
		}},
		{"spk10cmR4k", func(r *rand.Rand) *channel {
			// As spk10cmR with the larger spikes the right earring also
			// shows (1250-3750 codes), above the gain stage's target peak.
			ch := speaker(r, 60, 6, 14, []path{flatPath(0.0012, -0.3).shaped(r, 3, 0, 0)})
			ch.impulses = 0.02
			ch.impulseSz = 2500
			return ch
		}},
		{"headphoneR", func(r *rand.Rand) *channel {
			ch := &channel{paths: []path{flatPath(0.0001, 1).shaped(r, 2, 0, 0)}, amplitude: 120, noise: 5, dc: 30000}
			ch.impulses = 0.03
			ch.impulseSz = 1300
			return ch
		}},
		{"loud", func(r *rand.Rand) *channel {
			// A speaker pressed against the microphone: 1000 codes per tone,
			// three times what the Goertzel loop can hold.
			return &channel{paths: []path{flatPath(0.0001, 1).shaped(r, 3, 0, 0)}, amplitude: 1000, noise: 5, dc: 30000}
		}},
		{"noisy30cm", func(r *rand.Rand) *channel {
			ch := speaker(r, 25, 18, 8, []path{flatPath(0.0019, 0.5).shaped(r, 3, 0, 0), flatPath(0.0041, -0.35)})
			ch.impulses = 0.03
			ch.impulseSz = 60
			ch.rumble = 40
			return ch
		}},
	}
}

var streamCache sync.Map

func cachedStream(data []byte, seed uint32, n int) []uint32 {
	key := fmt.Sprintf("%x-%d-%d", data[:16], seed, n)
	if s, ok := streamCache.Load(key); ok {
		return s.([]uint32)
	}
	s := symbolStream(data, seed, n)
	streamCache.Store(key, s)
	return s
}

type stats struct {
	ok    int
	n     int
	times []float64
}

func (s stats) String() string {
	if s.ok == 0 {
		return fmt.Sprintf("%3d/%-3d", s.ok, s.n)
	}
	sort.Float64s(s.times)
	return fmt.Sprintf("%3d/%-3d med %5.1fs", s.ok, s.n, s.times[len(s.times)/2])
}

// matrix runs every scenario x clock error with each decoder, trials times.
func matrix(t *testing.T, decoders map[string]func() rxDecoder, trials int, clockErrs []float64, size int, limit float64) map[string]stats {
	r := rand.New(rand.NewPCG(7, 7))
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(r.Uint32())
	}
	stream := cachedStream(data, 0x1234, int((limit+30)*datatrans.SymbolsPerSecond))
	names := make([]string, 0, len(decoders))
	for n := range decoders {
		names = append(names, n)
	}
	sort.Strings(names)
	results := map[string]stats{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, sc := range scenarios() {
		if f := os.Getenv("SCEN"); f != "" && !strings.Contains(","+f+",", ","+sc.name+",") {
			continue
		}
		for _, ce := range clockErrs {
			for _, dn := range names {
				wg.Add(1)
				go func(sc scenario, ce float64, dn string) {
					defer wg.Done()
					var st stats
					for trial := range trials {
						tr := rand.New(rand.NewPCG(uint64(trial), 99))
						ch := sc.build(tr)
						ch.clockErr = ce
						start := tr.Float64() * 2.5
						res := run(decoders[dn](), ch, stream, data, start, limit, 799, tr)
						st.n++
						if res.ok {
							st.ok++
							st.times = append(st.times, res.seconds)
						}
					}
					mu.Lock()
					results[fmt.Sprintf("%s|%+.2f%%|%s", sc.name, ce*100, dn)] = st
					mu.Unlock()
				}(sc, ce, dn)
			}
		}
	}
	wg.Wait()
	keys := make([]string, 0, len(results))
	for k := range results {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("%-34s %s", k, results[k])
	}
	return results
}

func TestStreamProjection(t *testing.T) {
	data := []byte("hello, this is a test of the projection")
	s := symbolStream(data, 1, 20)
	// The first three symbols are magic1, the fourth its inverse.
	if s[0] != s[1] || s[1] != s[2] || s[3] != ^s[0] {
		t.Fatalf("preamble: %08x %08x %08x %08x", s[0], s[1], s[2], s[3])
	}
	// FSK: exactly one tone of each pair on, except the lowest pair (both on,
	// for timing).
	if s[0]&3 != 3 {
		t.Fatalf("magic1 pair 0: %02b", s[0]&3)
	}
	for i := 1; i < 16; i++ {
		if (s[0]>>(2*i))&3 == 0 || (s[0]>>(2*i))&3 == 3 {
			t.Fatalf("magic1 pair %d: %02b", i, (s[0]>>(2*i))&3)
		}
	}
}

func TestBaselineV1(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	trials := envInt("TRIALS", 10)
	matrix(t, map[string]func() rxDecoder{
		"v1": func() rxDecoder { return newV1(16) },
	}, trials, []float64{0, 0.003}, envInt("SIZE", 698), 60)
}

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		var n int
		fmt.Sscan(v, &n)
		return n
	}
	return def
}
