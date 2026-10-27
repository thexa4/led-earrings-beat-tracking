package main

// Patterns ported from C programs (patterns/ at the top of the repository):
// modeBasic's variants, which only follow the clock, and two sync mode
// variants that follow the beat. Ported rather than embedded as programs, and
// built from shared pieces - the per-LED hash (xorshift32), a triangle wave,
// the noise rainbow palette, the sparkle mode's LED buffer - because there is
// no flash for seven program images.

const numBasicPatterns = 5

// basicPattern draws modeBasic, ms milliseconds into the animation.
func basicPattern(led, ms, variant int) Color {
	switch variant {
	case 0:
		return fireflies(led, ms)
	case 1:
		return lavaLamp(led, ms)
	case 3:
		return sparkleStorm(led, ms)
	default:
		// Snake and vibey sparkle, drawn into the sparkle buffer once per
		// frame by cometsDraw. makeSparkle also fades what it shows by 32,
		// which cometsDraw overwrites every frame.
		return makeSparkle(led, 0, 0)
	}
}

// basicNextFrame draws the comets of snake (2) and vibey sparkle (4).
func basicNextFrame(variant, ms int) {
	switch variant {
	case 2:
		cometsDraw(ms, 0, 3)
	case 4:
		cometsDraw(ms, 3, 11)
	}
}

// cometTable: five bytes per comet - period in ms (uint16), offset in ms
// (uint16), flags - each going once around the ring per period. Flags: bits
// 0-1 the color (cometColors), 4 runs backwards, 8 fades from 255 to 0 over
// the lap (else full brightness), bits 5-6 the width w: 2w-1 LEDs, 80 less
// bright per LED from the middle. Snake is comets 0-2 (cyan, magenta, lime at 100, 83, 67
// LEDs/s); vibey sparkle 3-10, five warm sparks one way and three cyan the
// other, with the periods and offsets the C version's hash gave them.
const cometTable = "" +
	"\x68\x01\x00\x00\x60\xb0\x01\x00\x00\x61\x1c\x02\x00\x00\x62" +
	"\xd8\x06\x48\x04\x2b\x39\x08\xd0\x07\x2b\x25\x06\xfa\x03\x2b\x41\x0b\x59\x05\x2b\x1e\x0b\x38\x06\x2b" +
	"\x6b\x07\x60\x01\x2c\xcc\x0b\xd0\x07\x2c\x51\x09\x9c\x04\x2c"

const cometColors = "\x00\xff\xff\xff\x00\xff\x00\xff\x00\xff\x7f\x3f" // cyan, magenta, lime, warm (RGB)

// cometsDraw draws comets first..last-1 into the sparkle buffer. Where they
// overlap the brightest wins.
//
//go:noinline
func cometsDraw(ms, first, last int) {
	sparkleBrightness = [numLEDs]uint8{}
	for k := first; k < last; k++ {
		e := cometTable[k*5:]
		per := uint32(e[0]) | uint32(e[1])<<8
		pos := int((uint32(ms) + uint32(e[2]) | uint32(e[3])<<8) % per * numLEDs / per)
		f := e[4]
		bright, w := 255, int(f>>5)
		if f&8 != 0 {
			bright = 255 - pos*255/numLEDs
		}
		if f&4 != 0 && pos != 0 {
			pos = numLEDs - pos
		}
		p := cometColors[f&3*3:]
		c := NewColor(p[0], p[1], p[2])
		for d := 1 - w; d < w; d++ {
			i := pos + d
			if i < 0 {
				i += numLEDs
			} else if i >= numLEDs {
				i -= numLEDs
			}
			if b := bright - 80*max(d, -d); b > int(sparkleBrightness[i]) {
				sparkleBrightness[i] = uint8(b)
				sparkleColors[i] = c
			}
		}
	}
}

// Fireflies: each LED fades in yellow, holds, fades out and waits, with its
// own period (1.5-4.5 s) and phase, skipping one cycle in five at random.
//
//go:noinline
func fireflies(led, ms int) Color {
	seed := xorshift32(uint32(led)*101 + 13)
	period := 30 + seed%60 // in 50 ms steps
	clock := uint32(ms/50 + led*23)
	phase := clock % period
	level := uint32(0)
	switch {
	case phase < 6:
		level = phase * 42 // fade in
	case phase < 14:
		level = 235 // hold
	case phase < 22:
		level = (22 - phase) * 32 // fade out
	}
	if uint8(xorshift32(seed^clock/period)) < 51 {
		level = 0
	}
	return NewColor(uint8(level), uint8(level), 0)
}

// Lava lamp: electric blue blobs growing and shrinking over a warm amber
// background, each LED on its own 2-4 s cycle, some only to half or 80% size
// (redrawn every 16 s), with a white core at full size.
func lavaLamp(led, ms int) Color {
	h := xorshift32(uint32(led) + 1)
	blob := int(tri8(uint8(uint32(ms)*(2097+h&2047)>>15 + h>>24)))
	if n := uint8(xorshift32(h ^ uint32(ms>>14))); n < 50 {
		blob >>= 1
	} else if n < 100 {
		blob = blob * 13 >> 4
	}
	a := uint8(max(blob-60, 0) * 21 >> 4)
	c := mix(NewColor(180, 100, 20), NewColor(0, 180, 255), a)
	if a > 220 {
		c = addWhite(c, int(a-220)*4)
	}
	return c
}

// Sparkle storm: rainbow colors drifting around the ring, every LED pulsing
// at one of three speeds, flickering at random.
func sparkleStorm(led, ms int) Color {
	bright := tri8(uint8(ms>>(2+led%3) + led*10))
	if uint8(uint32(led)*7919+uint32(ms)*104729) < 120 {
		bright /= 2
	}
	return rainbow(uint8(ms/5 + led*7)).Scale(int(bright))
}

// tri8 is a triangle wave: 0 up to 254 at 127, back down to 0.
func tri8(x uint8) uint8 {
	if x >= 128 {
		x = ^x
	}
	return x * 2
}

// sin8 is a parabolic sine, 1..255 over 256 steps (FastLED's sin8 range).
//
//go:noinline
func sin8(x uint8) uint8 {
	h := int(x & 127)
	p := min(h*(128-h)>>5, 127)
	if x >= 128 {
		p = -p
	}
	return uint8(128 + p)
}

// rainbow is the noise animations' rainbow palette at hue (0..255).
func rainbow(hue uint8) Color {
	return palette16At(noisePalettes[48:96], uint16(hue)<<8)
}

// addWhite adds w (0..255) to every channel, saturating.
//
//go:noinline
func addWhite(c Color, w int) Color {
	return NewColor(
		uint8(min(int(c.R())+w, 255)),
		uint8(min(int(c.G())+w, 255)),
		uint8(min(int(c.B())+w, 255)))
}

// syncPattern draws sync mode's ported variants: 4 interference pulse, 5
// synthwave. The C versions faked a beat every 600 ms; these take it from the
// sync data (beatPhase: 0 on the beat, 255 just before the next one).
func syncPattern(led, variant int) Color {
	t := syncAnimFrame // 1/64 s
	phase := int(syncShare.beatPhase)
	if variant == 4 {
		// Interference pulse: two sine waves around the ring at slightly
		// different speeds (millis/10 and millis/11), through the rainbow,
		// plus white that fades from the beat to the next.
		w := int(sin8(uint8(led*7+t*25>>4))) + int(sin8(uint8(led*7+t*91>>6)))
		return addWhite(rainbow(uint8(w>>1)), 255-phase)
	}
	// Synthwave: the ring strobes red and cyan every 50 ms, white for the
	// first quarter of each beat.
	switch {
	case phase < 64:
		return NewColor(255, 255, 255)
	case t*5>>4&1 == 0:
		return NewColor(150, 0, 0)
	default:
		return NewColor(0, 255, 255)
	}
}
