package syncdsp

// A deterministic, integer-only EDM-like stimulus generator.
//
// This is the test signal for the host agreement harness: a pure function of
// the sample index with integer math only, so every run sees the same audio.
//
// Output is 16-bit unsigned around mid-rail (32768), matching the ADC.
//
// Musical structure: 4/4, beat = 3840 samples, 4 beats/bar, 8 bars/section.
// Sections cycle intro → drop → break → build, so the hash sees strong
// recurring transitions: exactly the material the anchor and periodicity
// logic must handle.

const (
	synthBeat    = 3840 // samples per beat
	synthBar     = synthBeat * 4
	synthSection = synthBar * 8
)

// Synth fills out with the deterministic stimulus starting at global sample
// index from.
func Synth(from int64, out []uint16) {
	for i := range out {
		out[i] = synthSample(from + int64(i))
	}
}

func synthSample(n int64) uint16 {
	sec := int(n / synthSection)
	t := int(n % synthSection)
	mode := sectionMode(sec)

	mix := int32(0)

	switch mode {
	case 0: // intro: offbeat hats only
		if t%synthBeat == synthBeat/2 {
			mix += hat(n, 2000)
		}
		mix += pad(n)
	case 1: // build: denser hats, kick from mid-section, rising noise
		if t%(synthBeat/2) == synthBeat/2 {
			mix += hat(n, 2600)
		}
		if t >= synthSection/2 {
			mix += kick(n)
		}
		mix += riser(t)
	case 2: // drop: four-on-the-floor, offbeat hats, bass
		mix += kick(n)
		if t%synthBeat == synthBeat/2 {
			mix += hat(n, 3000)
		}
		mix += bass(n)
	case 3: // break: no drums, pad + arpeggio
		mix += pad(n)
		mix += arp(n)
	}

	mix += 32768
	if mix > 65000 {
		mix = 65000
	}
	if mix < 500 {
		mix = 500
	}
	return uint16(mix)
}

// sectionMode returns the arrangement for a section index: identical drops
// recur every 2-4 sections (material for the periodicity check) around
// builds and breaks (material for anchors).
func sectionMode(sec int) int {
	switch sec % 16 {
	case 0, 8:
		return 0 // intro
	case 3, 11:
		return 3 // break
	case 5, 7, 13, 15:
		return 1 // build
	default:
		return 2 // drop
	}
}

// kick: 60 Hz sine with an exponential-ish decay (one 1/8 step per 512
// samples, so ~0.1 s total). The staircase adds click harmonics, which only
// makes it kickier.
func kick(n int64) int32 {
	t := int(n % synthBeat)
	if t >= 4608 {
		return 0
	}
	env := int32(9500) >> uint(t/512)
	return env * isin(n, 800) / 255
}

// hat: hashed noise burst with a fast decay (~0.075 s), triggered off the
// beat. Uses the global sample index so successive hats decorrelate.
func hat(n int64, amp int32) int32 {
	t := int(n%synthBeat+synthBeat) % synthBeat
	dt := (t - synthBeat/2 + synthBeat) % synthBeat
	if dt >= 1200 {
		return 0
	}
	env := amp >> uint(dt/150)
	noise := int32(hash32(uint32(n))>>24) - 128
	return env * noise / 128
}

// bass: 100 Hz sine, short gate every half beat, only during drops.
func bass(n int64) int32 {
	t := int(n % (synthBeat / 2))
	if t >= 600 {
		return 0
	}
	env := int32(2200) >> uint(t/200)
	return env * isin(n, 480) / 255
}

// pad: two slow detuned sines, quiet bed for intros and breaks.
func pad(n int64) int32 {
	a := isin(n, 218)   // ~220 Hz at 48 kHz nominal
	b := isin(n*7, 137) // ~330 Hz-ish detune
	return (a + b) / 2
}

// arp: 4-note square pattern during breaks.
func arp(n int64) int32 {
	step := int(n / (synthBeat / 4))
	t := int(n % (synthBeat / 4))
	if t >= 240 {
		return 0
	}
	freqs := [4]int64{218, 327, 436, 546}
	period := 48000 / freqs[step%4]
	if n%period < period/2 {
		return 1600
	}
	return -1600
}

// riser: noise with a slow linear ramp, fills the builds.
func riser(t int) int32 {
	env := int32(t) / 8
	if env > 1500 {
		env = 1500
	}
	noise := int32(hash32(uint32(t))>>24) - 128
	return env * noise / 128
}

// isin returns sin(2*pi*pos/period) scaled to -255..255, in pure integer
// arithmetic: identical on every platform. The quarter-wave curve
// v(x) = x*(512-x)/256 over x in 0..255 approximates sin over a quarter
// period (endpoint-exact; mid error ~6%, irrelevant for a stimulus source).
func isin(pos int64, period int64) int32 {
	q := period / 4
	p := pos % period
	quarter := int32(p / q)
	rem := int32(p % q)
	x := rem * 255 / int32(q)
	v := x * (512 - x) / 256
	switch quarter {
	case 0:
		return v
	case 1:
		xm := 255 - x
		return xm * (512 - xm) / 256
	case 2:
		return -v
	default:
		xm := 255 - x
		return -(xm * (512 - xm) / 256)
	}
}

// hash32 is a 32-bit integer hash (integer-only splitmix-style) used as a
// deterministic noise source.
func hash32(x uint32) uint32 {
	x ^= x >> 16
	x *= 0x45d9f3b
	x ^= x >> 16
	x *= 0x45d9f3b
	x ^= x >> 16
	return x
}
