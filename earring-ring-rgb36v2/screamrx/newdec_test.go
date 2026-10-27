package screamrx

import (
	"math/rand/v2"
	"testing"

	"codeberg.org/maaike328p/datatrans/dsp"
)

// rx wraps the new decoder for the harness.
type rx struct {
	d       Decoder
	maxTrim int
}

func newRx(maxTrim, psc int) *rx {
	r := &rx{maxTrim: maxTrim}
	r.d.Initialize(make([]uint32, fountainWords), 1532, fs, 200*256/(psc+1))
	return r
}

func (r *rx) process(w []int32) int {
	// The firmware applies the gain in its copy loop.
	if g := r.d.Gain(); g > 0 {
		for i := range w[:symLen] {
			w[i] >>= g
		}
	}
	return r.d.Process(w)
}
func (r *rx) bytes() []byte { return r.d.Bytes() }
func (r *rx) reset()        { r.d.Reset() }
func (r *rx) adjust() int   { return r.d.Adjust() }
func (r *rx) trim(cur int) int {
	return max(-r.maxTrim, min(r.maxTrim, cur+r.d.RateTrim()))
}

// TestTimingSign checks the timing estimator's sign and scale: a window
// that starts off samples after the symbol grid must read as +off.
func TestTimingSign(t *testing.T) {
	data := make([]byte, 100)
	stream := symbolStream(data, 1, 40)
	ch := &channel{paths: []path{flatPath(0, 1)}, amplitude: 50, dc: 30000}
	var d Decoder
	d.Initialize(make([]uint32, fountainWords), 1532, fs, 0)
	raw := make([]uint16, symLen)
	win := make([]int32, symLen+2)
	win[symLen] = 0x7fff_ffff
	for _, off := range []int{-99, -80, -30, -5, 0, 7, 33, 60, 99} {
		r := newReceiver(ch, stream, float64(off+100)/fs, 799, rand.New(rand.NewPCG(1, 1)))
		r.fill(raw)
		dc := dsp.CopyWithOffset(raw, win, 30000)
		r.fill(raw)
		dsp.CopyWithOffset(raw, win, dc)
		d.analyze(win)
		got, ok := d.timing(magic1Send)
		// The analysed (second) window starts off+300 samples in: off+100
		// past a symbol boundary, wrapped into (-100, 100].
		want := (off+100+100)%200 - 100
		if want == -100 {
			want = 100
		}
		g := int((got + 128) >> 8)
		if !ok || min(abs(g-want), 200-abs(g-want)) > 1 {
			t.Errorf("offset %d: got %d, want %d (Q8 %d) ok=%v", off, g, want, got, ok)
		}
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func TestNewVsV1(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	trials := envInt("TRIALS", 10)
	matrix(t, map[string]func() rxDecoder{
		"v1":  func() rxDecoder { return newV1(16) },
		"new": func() rxDecoder { return newRx(16, 799) },
	}, trials, []float64{0, -0.005}, envInt("SIZE", 698), 60)
}

// TestNoiseTrim listens to noise alone, as an earring waiting for a
// transmission does: brief false locks on magic1 look-alikes must not walk
// the clock trim away (the caller keeps it across resets).
func TestNoiseTrim(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	for trial := range 5 {
		tr := rand.New(rand.NewPCG(uint64(trial), 5))
		ch := &channel{paths: []path{flatPath(0.0001, 1)}, amplitude: 0, noise: 25, dc: 30000}
		if trial%2 == 1 {
			ch.impulses, ch.impulseSz = 0.02, 2500 // the right earring
		}
		dec := newRx(20, 799)
		rx := newReceiver(ch, []uint32{0}, 1e9, 799, tr)
		raw := make([]uint16, symLen)
		win := make([]int32, symLen+2)
		win[symLen] = 0x7fff_ffff
		dc := dsp.CopyWithOffset(raw, win, 0)
		worst := 0
		for range 300 * 50 {
			rx.fill(raw)
			dc = dsp.CopyWithOffset(raw, win, dc)
			st := dec.process(win)
			if a := dec.adjust(); a != 0 {
				rx.skip(a)
			}
			if tt := dec.trim(rx.trim); tt != rx.trim {
				rx.setTrim(tt)
			}
			if st < 0 {
				dec.reset()
			}
			worst = max(worst, abs(rx.trim))
		}
		t.Logf("trial %d: syncs %d, trim at end %+d, worst %d", trial, dec.d.Stats.Syncs, rx.trim, worst)
		// A false lock may trim while it lasts, but must take it back.
		if rx.trim != 0 || worst > 2 {
			t.Errorf("trial %d: the trim walked to %+d (worst %d) on noise alone", trial, rx.trim, worst)
		}
	}
}
