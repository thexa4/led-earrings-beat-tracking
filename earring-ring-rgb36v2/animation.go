package main

import (
	"image/color"
	"math/bits"

	"codeberg.org/maaike328p/led-editor/pattern"
	"github.com/aykevl/ledsgo"
)

const numLEDs = 36

const initialMode = 0 // first mode

const (
	modeTrace = iota
	modeNoise
	modeFire
	modeFireSound
	modeStatic
	modeFlag
	modeSparkle
	modeVUMeter
	modeCustom0
	modeSync
	modeLast

	modeTest
	modeQA
	modePowerOn
)

// Number of variants per mode, indexed by mode value (custom modes are
// capped by the pattern itself, hence 0 entries there). A const string for
// the same reason as the color tables in flags.go: no initializer, no .data,
// one byte per mode in flash only.
const variantsPerMode = "\x03" + // modeTrace
	"\x05" + // modeNoise (numNoisePatterns)
	"\x06" + // modeFire
	"\x06" + // modeFireSound
	"\x0d" + // modeStatic (numStaticColors)
	"\x0a" + // modeFlag (numFlags)
	"\x04" + // modeSparkle (numSparkleVariants)
	"\x0d" + // modeVUMeter (numStaticColors)
	"\x00" + // modeCustom0: capped by the pattern itself
	"\x01" + // modeSync: the pattern is chosen by the music, not the user
	"\x00" + // modeLast (not a mode)
	"\x00" + // modeTest
	"\x02" // modeQA

// Cycle to the next variant within a mode.
func animationNextVariant(mode, variant int) int {
	variant++
	if mode == modeCustom0 {
		// Custom modes have their variant defined in the program itself.
		if variant > customPattern.MaxVariant() {
			variant = 0
		}
		customPattern.SetVariant(variant)
	} else if mode >= len(variantsPerMode) {
		variant = 0 // out of range
	} else if variant >= int(variantsPerMode[mode]) {
		variant = 0
	}
	return variant
}

func animate(mode, variant, led, frame int) Color {
	switch mode {
	case modeTrace:
		return trace(led, frame, variant)
	case modeNoise:
		return noise(led, frame, variant)
	case modeFire:
		return fire(led, frame, variant)
	case modeFireSound:
		return fireSound(led, frame, variant)
	case modeStatic:
		return staticColor(led, frame, variant)
	case modeFlag:
		return showFlag(led, frame, variant)
	case modeVUMeter:
		return vuMeter(led, frame, variant)
	case modeSparkle:
		return makeSparkle(led, frame, variant)
	case modeCustom0:
		return showCustom(led, frame, variant, 0)
	case modeSync:
		return syncAnimate(led)
	case modeTest:
		return testPulse(led, frame)
	case modePowerOn:
		return powerOn(led, frame)
	case modeQA:
		return testQAPattern(led, frame, variant)
	default:
		// bug
		return errorPattern(led, frame)
	}
}

func animationNeedsMic(mode int) bool {
	switch mode {
	case modeVUMeter, modeFireSound, modeSparkle:
		return true
	default:
		return false
	}
}

// Called on every new frame, to do per-frame processing.
// It should be very fast, otherwise the earrings will dip in brightness at 30Hz
// or so which looks annoying.
func newFrame(mode, variant, frame int) {
	switch mode {
	case modeVUMeter:
		addPower(uint16(processSamples()))
	case modeFireSound:
		addPower(uint16(processSamples()))
		vol := uint8(min(currentVolume()*3/512, 0xff))
		volumeHistoryIndex++
		if volumeHistoryIndex == 36 {
			volumeHistoryIndex = 0
		}
		volumeHistory[volumeHistoryIndex] = vol
	case modeSparkle:
		sparkleNextFrame(variant)
	case modeCustom0:
		customNextFrame(frame, variant)
	case modeQA:
		customNextFrame(frame, variant)
	}
}

// Called when switching to a new mode. Can be used to initialize some state for
// this mode.
func initMode(mode int) {
	switch mode {
	case modeCustom0:
		customLoadPattern(0, true)
	}
}

// The noise animation's per-pattern parameters, all compile-time constants:
// speed/spread bytes plus the five palettes (16 RGBA entries each, see
// noisePalettes.go) concatenated. An earlier form stored these as a var
// struct array, which cost an initializer function, a .data image and RAM
// for data that is never written; const strings live in flash only.
const (
	numNoisePatterns = 5
	noiseSpeeds      = "\x0a\x0a\x09\x09\x09" // higher means faster
	noiseSpreads     = "\x0b\x0b\x0c\x0c\x0c" // higher means colors more close together
	noisePalettes    = PartyColors + RainbowColors + ForestColors + OceanColors + CloudColors
)

// Show some Simplex noise on the earring, with various predefined patterns.
func noise(led, frame, variant int) Color {
	if variant >= numNoisePatterns {
		return NewColor(0, 0, 0) // shouldn't happen
	}
	x := uint32(frame) << uint32(noiseSpeeds[variant])
	y := uint32(led) << uint32(noiseSpreads[variant])
	r, g, b := palette16At(noisePalettes[variant*64:(variant+1)*64], ledsgo.Perlin2(x, y)*2)
	return NewColor(r, g, b)
}

var traceIndex uint8
var traceIndexFrame int

func trace(led, frame, variant int) Color {
	switch variant {
	case 0, 1:
		return twoTracers(led, frame, variant)
	default:
		return purpleCircles(led, frame)
	}
}

func twoTracers(led, frame, variant int) Color {
	// This essentially calculates (frame%72) but faster.
	if frame != traceIndexFrame {
		traceIndexFrame = frame
		traceIndex++
	}
	if traceIndex >= 72 {
		traceIndex = 0
	}

	// Calculate where in the circle we are for this LED.
	index := uint8(led*2) + 71 - traceIndex
	if index >= 72 {
		index -= 72
	}
	if index >= 72 {
		index -= 72
	}

	// Split out in the two traces.
	trace := 0
	if index >= 36 {
		index -= 36
		trace = 1
	}

	// Determine the color for the LED.
	var c Color
	switch variant {
	case 0:
		// Rainbow trace.
		var col color.RGBA
		switch trace {
		case 0:
			// First tracer.
			col = ledsgo.Color{H: uint16(frame * 128), S: 255, V: 255}.Rainbow()
		default:
			// Second tracer, offset 180° on the color wheel and on the actual LED ring.
			col = ledsgo.Color{H: uint16(frame*128) + 0x8000, S: 255, V: 255}.Rainbow()
		}
		c = NewColor(
			uint8(uint32(col.R)*(uint32(index)*7)/256),
			uint8(uint32(col.G)*(uint32(index)*7)/256),
			uint8(uint32(col.B)*(uint32(index)*7)/256))
	case 1:
		// Red (fire) and blue (ice) swirling around in circles.
		switch trace {
		case 0:
			c = NewColor(index*7, index*3/8, 0)
		default:
			c = NewColor(index, 0, index*7)
		}
	}

	// Dim at the start (fade in)
	if index == 35 {
		c = NewColor(c.R()/3, c.G()/3, c.B()/3)
	}

	return c
}

// Three purple tracers running in circles.
func purpleCircles(led, frame int) Color {
	if frame != traceIndexFrame {
		traceIndexFrame = frame
		traceIndex++
	}
	if traceIndex >= 24 {
		// This animation has three tracers.
		traceIndex = 0
	}

	index := uint8(led*2) + 23 - traceIndex
	if index >= 24 {
		index -= 24
	}
	if index >= 24 {
		index -= 24
	}
	if index >= 24 {
		index -= 24
	}

	if index == 23 {
		// fade in at the beginning
		return NewColor(0x20, 0, 0x10)
	}
	return NewColor(uint8(index*10), 0, uint8(index*5))
}

// Determine fire color for both fire animations.
func fireColor(variant int) color.RGBA {
	switch variant {
	case 0: // red
		return color.RGBA{R: 255}
	case 1: // orange
		return color.RGBA{R: 220, G: 40}
	case 2: // green
		return color.RGBA{G: 255}
	case 3: // teal-ish
		return color.RGBA{G: 127, B: 127}
	case 4: // blue
		return color.RGBA{B: 255}
	default: // purple
		return color.RGBA{R: 127, B: 127}
	}
}

// Fire animation in various colors.
func fire(led, frame, variant int) Color {
	intensityIndex := indexFromBottom(led)
	noiseIndex := uint32(frame) - uint32(intensityIndex)
	if led > numLEDs/2 {
		// Use a different flame on the otherside of the earring.
		// Without this, it would simply mirror the flame on both sides.
		// The constant is just an arbitrary number to give it enough distance
		// from the left side.
		noiseIndex += 0x1234
	}

	// Calculate the amount of heat on this particular pixel.
	heat := uint8(ledsgo.Noise1(noiseIndex<<10) >> 8)
	cooling := uint8(intensityIndex * 11)
	if heat < uint8(cooling) {
		heat = 0
	} else {
		heat -= uint8(cooling)
	}

	// Turn it into a flame, based on the given palette.
	// Perhaps we could use an actual 0-255 (or 0-64) palette instead? That
	// might be faster.
	c := coloredFlame(heat, fireColor(variant))
	return NewColor(c.R, c.G, c.B)
}

var volumeHistory [36]uint8
var volumeHistoryIndex uint8

func fireSound(led, frame, variant int) Color {
	// LED indices calculated, so they start from the bottom and alternate
	// between the two sides:
	// LED 14: index 6
	// LED 15: index 4
	// LED 16: index 2
	// LED 17: index 0
	// LED 18: index 1
	// LED 18: index 3
	// LED 18: index 5
	var historyIndex int
	var intensityIndex uint8
	if led < 18 {
		intensityIndex = uint8(18 - led)
		historyIndex = (17 - led) * 2
	} else {
		historyIndex = (led-18)*2 + 1
		intensityIndex = uint8(led - 18)
	}

	// Calculate index into volumeHistory, since that is a circular buffer (with
	// index 0 being the most recent and higher numbers being further in the
	// past).
	historyIndex = int(volumeHistoryIndex) - historyIndex
	if historyIndex < 0 {
		historyIndex += 36
	}

	// Cool down the flame the higher it gets.
	heat := volumeHistory[historyIndex]
	cooling := uint8(intensityIndex * 8)
	if heat < uint8(cooling) {
		heat = 0
	} else {
		heat -= uint8(cooling)
	}

	c := coloredFlame(heat, fireColor(variant))
	return NewColor(c.R, c.G, c.B)
}

// Colored flame. Like a heat map, but the lowest temperatures are not fixed red
// but instead use the configured color.
//
//go:inline
func coloredFlame(index uint8, fireColor color.RGBA) color.RGBA {
	if index < 128 {
		// <color>
		c := ledsgo.ApplyAlpha(fireColor, index*2)
		c.A = 255
		return c
	}
	if index < 224 {
		// <color>-yellow
		c1 := ledsgo.ApplyAlpha(fireColor, 255-uint8(uint32(index-128)*8/3))
		c2 := ledsgo.ApplyAlpha(color.RGBA{255, 255, 0, 255}, uint8(uint32(index-128)*8/3))
		return color.RGBA{c1.R + c2.R, c1.G + c2.G, c1.B + c2.B, 255}
	}
	// yellow-white
	return color.RGBA{255, 255, (index - 224) * 8, 255}
}

func indexFromBottom(index int) int {
	// Start at the top with 18, move along the right size down to 0 at the
	// bottom, and then resume counting upwards again:
	// 18, 17, ..., 1, 0, 1, 2, ..., 17
	newIndex := 18 - index
	if index >= 18 {
		newIndex = index - 18
	}
	return newIndex
}

var ()

func staticColor(led, frame, variant int) Color {
	if variant >= numStaticColors {
		return NewColor(0, 0, 0) // should be unreachable
	}
	return staticColors.color(variant)
}

// (Palette and the flag tables moved to flags.go.)
func showFlag(led, frame, variant int) Color {
	if variant >= numFlags {
		// This shouldn't actually happen.
		return NewColor(0, 0, 0)
	}
	return allFlags[flagSize*variant : flagSize*(variant+1)].color(led)
}

// Basic VU meter like animation, with different colors.
func vuMeter(led, frame, variant int) Color {
	// LED indices calculated, so they start from the bottom and alternate
	// between the two sides:
	// LED 14: index 6
	// LED 15: index 4
	// LED 16: index 2
	// LED 17: index 0
	// LED 18: index 1
	// LED 18: index 3
	// LED 18: index 5
	var ledIndex int
	if led < 18 {
		ledIndex = (17 - led) * 2
	} else {
		ledIndex = (led-18)*2 + 1
	}

	// Determine color for this VU meter.
	c := NewColor(0, 0, 0)
	if variant < numStaticColors {
		c = staticColors.color(variant)
	}

	frameIntensity := int(currentVolume()) / 32
	intensity := frameIntensity - ledIndex*64
	if intensity > 255 {
		return c
	} else if intensity < 0 {
		return NewColor(0, 0, 0)
	} else {
		return c.Scale(intensity)
	}
}

// Sparkle animation!
// Credits to this person for suggesting it:
// https://hachyderm.io/@wmd@chaos.social/115816155535125138
// It's been vaguely inspired by the HALO-90 animation but this one looks very
// different.

var sparkleColors [numLEDs]Color
var sparkleBrightness [numLEDs]uint8

func sparkleNextFrame(variant int) {
	addPower(uint16(processSamples()))

	vol := int(currentVolume()) - 4096

	r := rand()
	if uint32(vol) > r&0x7fff {
		// Create a new sparkle!

		// Pick an index at random.
		index := fastModulo36(vol & 0xff)

		// Pick one of the two colors.
		if variant > numSparkleVariants {
			variant = 0 // shouldn't happen, but just to be sure
		}
		c := sparkleVariants.color(variant*2 + int(r>>31))

		// Store this sparkle.
		sparkleColors[index] = c
		sparkleBrightness[index] = 0xff
	}
}

func makeSparkle(led, frame, variant int) Color {
	dimming := sparkleBrightness[led]
	if dimming < 32 {
		return NewColor(0, 0, 0)
	}
	sparkleBrightness[led] = dimming - 32
	return sparkleColors[led].Scale(int(dimming))
}

// The loaded pattern. It is either loaded in memory, or not yet loaded.
var customPattern pattern.Pattern

var customPatternFile []byte // slice loaded in memory by loadPattern.

var customPatternLoadDelay uint8 // delay in frames before running the pattern

func customLoadPattern(slot int, delay bool) {
	// Invalidate the previous pattern.
	customPattern = pattern.Pattern{}

	// Load the pattern into RAM, but do not yet initialize it.
	// This is relatively slow.
	customPatternFile = loadPattern(slot)

	// Wait ~1s before actually loading this pattern, to give the user a chance
	// to move past it or reflash it if it is broken.
	customPatternLoadDelay = 1
	if delay {
		customPatternLoadDelay = 30
	}
}

// Approximation, if the pattern runs fast enough.
// We might want to use an actual timer instead some time in the future.
const millisPerFrame = 37

func customNextFrame(frame, variant int) {
	// Call the 'nextFrame' function.
	fn := customPattern.NextFrame1()
	if fn != nil {
		fn(frame * millisPerFrame)
		return
	}

	// No pattern loaded.
	if customPatternLoadDelay > 1 {
		// Wait a bit before actually running any code from the pattern.
		// In particular, if the 2nd button is pressed, don't continue the
		// countdown since the user might want to reflash the pattern with a
		// working one (if this one is broken).
		if !button2Pressed() {
			customPatternLoadDelay--
		}
	} else if customPatternLoadDelay == 1 {
		// We reached the end of the countdown!
		customPatternLoadDelay = 0

		// Load the pattern, it was already loaded in memory so this is fast.
		customPattern = pattern.Load(customPatternFile)

		// Check if the pattern is of the right type.
		if customPattern.Shape() != pattern.ShapeCircle {
			customPattern = pattern.Pattern{} // reset it, so it won't be run
		}

		// Set the currently active variant.
		customPattern.SetVariant(variant)

		// Call the 'setup' function, if the pattern was successfully loaded.
		setup := customPattern.Setup1()
		if setup != nil {
			setup(numLEDs)
		}
	}
	return
}

// Show a pattern as it was loaded.
func showCustom(led, frame, variant, slot int) Color {
	fn := customPattern.GetPixel1()
	if fn != nil {
		c := fn(led, int(frame)*millisPerFrame)
		c = bits.ReverseBytes32(c << 8) // patterns use 0x00RRGGBB, we use 0x00BBGGRR so reverse
		return Color(c)
	}

	// Unsupported shape, or invalid (not loaded) pattern.
	// Show a single LED instead.
	if led < 18 && led+slot > 16 {
		return NewColor(0x00, 0x00, 0xff)
	}
	return NewColor(0, 0, 0)
}

// Blink the first LED, roughly 0.5s on, 0.5s off.
func errorPattern(led, frame int) Color {
	if led == 0 {
		// Roughly 500ms on, 500ms off (assuming 30fps).
		return NewColor(uint8(frame%32)/16*128, 0, 0)
	}
	// Other LEDs are dark.
	return NewColor(0, 0, 0)
}

func rotateSingleColor(led, frame int) Color {
	idx := int(frame / 8 % 36)
	value := uint8(0)
	if led == idx {
		value = 128
	}
	if (led+1)%36 == idx {
		value = 128 / 2
	}
	if (led+2)%36 == idx {
		value = 128 / 4
	}
	if (led+3)%36 == idx {
		value = 128 / 8
	}
	if (led+4)%36 == idx {
		value = 128 / 16
	}
	if (led+5)%36 == idx {
		value = 128 / 32
	}
	return NewColor(0, 0, value)
}

// Pulse red LEDs around once per second, for testing.
func testPulse(led, frame int) Color {
	return NewColor(uint8((frame%32)<<3), 0, 0)
}

// Startup animation. The duration until it will power on is set in
// waitForPowerOn, but this gives a nice animation to show how far it is.
func powerOn(led, frame int) Color {
	if frame*2 > led {
		return NewColor(0, 0x3f, 0) // green
	}
	return NewColor(0, 0, 0)
}

func testQAPattern(led, frame, variant int) Color {
	switch variant {
	case 0:
		// Test all LED colors.
		switch (frame / 16) % 4 {
		case 0:
			return NewColor(0xff, 0, 0)
		case 1:
			return NewColor(0, 0xff, 0)
		case 2:
			return NewColor(0, 0, 0xff)
		default:
			return NewColor(0xff, 0xff, 0xff)
		}
	default:
		// Test microphone (after 'variant' press).
		if !customPattern.Valid() && customPatternLoadDelay == 0 {
			// Load the pattern at "slot" -1, which indicates it should be kept
			// in RAM and not written to flash.
			dataRecv(-1)
			customLoadPattern(-1, false)
		}

		// Run this pattern from RAM, if possible.
		fn := customPattern.GetPixel1()
		if fn != nil {
			c := fn(led, int(frame)*millisPerFrame)
			c = bits.ReverseBytes32(c << 8) // patterns use 0x00RRGGBB, we use 0x00BBGGRR so reverse
			return Color(c)
		}

		// If something fails, show a red circle.
		return NewColor(0xff, 0x00, 0x00)
	}
}
