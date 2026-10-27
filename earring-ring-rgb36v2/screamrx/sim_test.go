package screamrx

// Channel simulator for the scream transport: the exact v1 symbol stream from
// the real encoder, played through a parameterised acoustic channel and
// sampled by a receiver whose clock can be off and trimmed, then put through
// the firmware's integer front end (DC removal per window, sentinel).
//
// The received signal is synthesised analytically, not by filtering a sampled
// waveform: every v1 tone has an integer number of cycles per 20 ms symbol, so
// the transmitted signal is exactly
//
//	x(t) = sum_k b_k(floor(50 t)) sin(2 pi f_k t) / 32
//
// and a path with delay tau and per-tone complex gain c_k contributes
// sum_k b_k(floor(50 (t - tau))) Im(c_k exp(2 pi i f_k (t - tau))). That makes
// fractional delays, clock offsets and mid-run PSC trims exact and cheap.

import (
	"math"
	"math/cmplx"
	"math/rand/v2"

	"codeberg.org/maaike328p/datatrans"
)

const (
	fs        = 10000
	nFreq     = datatrans.NumFreqs
	symLen    = fs / datatrans.SymbolsPerSecond
	freqStart = 400
	freqStep  = 50
)

// symbolStream returns the on/off bits of the first n symbols the encoder
// sends for data with the given seed, by projecting each 20 ms symbol of its
// 10 kHz output onto the 32 (orthogonal) tones.
func symbolStream(data []byte, seed uint32, n int) []uint32 {
	enc := datatrans.NewEncoder(fs)
	enc.Start(data, seed)
	var basis [nFreq][symLen]float64
	for k := range nFreq {
		f := float64(freqStart + freqStep*k)
		for i := range symLen {
			basis[k][i] = math.Sin(2 * math.Pi * f * float64(i) / fs)
		}
	}
	buf := make([]float32, symLen)
	out := make([]uint32, n)
	for s := range n {
		enc.Encode(buf)
		var bits uint32
		for k := range nFreq {
			p := 0.0
			for i := range symLen {
				p += float64(buf[i]) * basis[k][i]
			}
			// p = b * gain * symLen/2 = b * 200/2/32
			if p > 0.5*symLen/2/nFreq {
				bits |= 1 << k
			}
		}
		out[s] = bits
	}
	return out
}

// path is one arrival: a delay and a complex gain per tone.
type path struct {
	delay float64 // seconds
	gain  [nFreq]complex128
}

// channel describes the acoustic channel and the receiver.
type channel struct {
	paths     []path
	amplitude float64 // ADC codes per tone at gain 1 (a tone is 1/32 full scale)
	noise     float64 // white noise stddev, ADC codes
	impulses  float64 // probability per sample of an impulse
	impulseSz float64 // impulse amplitude, ADC codes (one-sided, like LED coupling)
	rumble    float64 // amplitude of a 30-90 Hz wobble, ADC codes
	music     []float32
	musicGain float64 // ADC codes per unit of music
	clockErr  float64 // receiver sample clock error (positive: samples faster)
	dc        float64
}

// flatPath is a path with the same gain and phase on every tone.
func flatPath(delay, gain float64) path {
	p := path{delay: delay}
	for k := range p.gain {
		p.gain[k] = complex(gain, 0)
	}
	return p
}

// tilt multiplies a path's tone gains by a smooth dB curve over the band.
func (p path) shaped(r *rand.Rand, rippleDB float64, lowCutHz float64, highCutHz float64) path {
	// Random smooth ripple: a few sine components over the band.
	var a [3]float64
	var ph [3]float64
	for i := range a {
		a[i] = r.NormFloat64() * rippleDB / 2
		ph[i] = r.Float64() * 2 * math.Pi
	}
	for k := range p.gain {
		f := float64(freqStart + freqStep*k)
		db := 0.0
		for i := range a {
			db += a[i] * math.Sin(float64(i+1)*math.Pi*float64(k)/nFreq+ph[i])
		}
		g := math.Pow(10, db/20)
		// First-order high/low-pass magnitude and phase (a woofer port, a
		// small driver's roll-off, or a crossover branch).
		h := complex(1, 0)
		if lowCutHz > 0 {
			h *= complex(0, f/lowCutHz) / complex(1, f/lowCutHz)
		}
		if highCutHz > 0 {
			h *= 1 / complex(1, f/highCutHz)
		}
		p.gain[k] *= complex(g, 0) * h
	}
	return p
}

// reverb adds n diffuse reflections between 5 ms and 5*rt60/6.9 s, decaying
// with the given RT60, with total energy level relative to the direct path.
func reverb(r *rand.Rand, n int, rt60, levelDB float64) []path {
	var ps []path
	total := 0.0
	for range n {
		d := 0.005 + r.Float64()*0.06
		g := math.Exp(-6.9*d/rt60) * (1 + 0.3*r.NormFloat64())
		if r.IntN(2) == 0 {
			g = -g
		}
		p := path{delay: d}
		for k := range p.gain {
			p.gain[k] = complex(g, 0) * cmplx.Rect(1, r.NormFloat64()*0.3)
		}
		ps = append(ps, p)
		total += g * g
	}
	scale := math.Pow(10, levelDB/20) / math.Sqrt(total)
	for i := range ps {
		for k := range ps[i].gain {
			ps[i].gain[k] *= complex(scale, 0)
		}
	}
	return ps
}

// receiver produces the ADC samples of one run.
type receiver struct {
	ch      *channel
	stream  []uint32
	t       float64 // time of the next sample, seconds since stream start
	dt      float64 // current sample period
	nominal float64 // sample period at trim 0
	trim    int     // PSC steps away from nominal (positive: slower)
	psc     int     // nominal PSC (sets the trim step size)
	rng     *rand.Rand
	irng    *rand.Rand // impulses only, so a run without them sees the same noise
	rumPh   float64
	musicT  float64
	coef    [][nFreq]complex128 // per path: gain * exp(-2 pi i f tau)
}

func newReceiver(ch *channel, stream []uint32, start float64, psc int, rng *rand.Rand) *receiver {
	r := &receiver{ch: ch, stream: stream, t: start, psc: psc, rng: rng}
	r.irng = rand.New(rand.NewPCG(rng.Uint64(), 1))
	r.nominal = 1 / (fs * (1 + ch.clockErr))
	r.setTrim(0)
	for _, p := range ch.paths {
		var c [nFreq]complex128
		for k := range c {
			f := float64(freqStart + freqStep*k)
			c[k] = p.gain[k] * cmplx.Rect(1, -2*math.Pi*f*p.delay)
		}
		r.coef = append(r.coef, c)
	}
	return r
}

// setTrim sets the PSC offset like the firmware: period = 2(psc+trim+1)/fclk.
func (r *receiver) setTrim(trim int) {
	r.trim = trim
	r.dt = r.nominal * float64(r.psc+1+trim) / float64(r.psc+1)
}

// sample returns the next raw ADC code (as the DMA writes it).
func (r *receiver) sample() uint16 {
	t := r.t
	r.t += r.dt
	ch := r.ch
	v := ch.dc
	// Base rotators for this instant.
	w0 := cmplx.Rect(1, 2*math.Pi*freqStart*t)
	ws := cmplx.Rect(1, 2*math.Pi*freqStep*t)
	for j, p := range ch.paths {
		ts := t - p.delay
		sym := int(math.Floor(ts * datatrans.SymbolsPerSecond))
		if sym < 0 || sym >= len(r.stream) {
			continue
		}
		bits := r.stream[sym]
		if bits == 0 {
			continue
		}
		z := w0
		acc := 0.0
		c := &r.coef[j]
		for k := range nFreq {
			if bits&(1<<k) != 0 {
				acc += imag(c[k] * z)
			}
			z *= ws
		}
		v += acc * ch.amplitude
	}
	if ch.noise > 0 {
		v += r.rng.NormFloat64() * ch.noise
	}
	if ch.impulses > 0 && r.irng.Float64() < ch.impulses {
		v += ch.impulseSz * (0.5 + r.irng.Float64())
	}
	if ch.rumble > 0 {
		v += ch.rumble * math.Sin(2*math.Pi*45*t+0.5*math.Sin(2*math.Pi*0.7*t))
	}
	if len(ch.music) > 0 {
		i := int(t*16000) % len(ch.music)
		v += float64(ch.music[i]) * ch.musicGain
	}
	v = math.Round(v)
	return uint16(max(0, min(65535, v)))
}

// fill writes the next len(buf) samples.
func (r *receiver) fill(buf []uint16) {
	for i := range buf {
		buf[i] = r.sample()
	}
}

// skip discards n samples.
func (r *receiver) skip(n int) {
	r.t += float64(n) * r.dt
}
