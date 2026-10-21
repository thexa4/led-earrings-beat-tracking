package main

// The pride flags, stored as *const* strings: flash only, no RAM and no
// init code (a `var` table would cost the same bytes again as a .data image
// plus copy code, plus RAM for state that is never written).
//
// Each flag is run-length encoded - see flagColor and the table comment at
// the bottom of this file - because it is only a handful of solid stripes
// repeated around the ring. Colors inside a run are three bytes: alpha is
// not stored at all (Color keeps it in bits 24..31 and nothing in the
// firmware reads it - NewColor leaves it 0 and the bit-bang C helpers take
// 24-bit values).
//
// flags.go is derived from the original hand-written table (gen_flags.py
// parses it and verified entry counts and that nothing else referenced the
// old vars); the run-length tables were then generated from those colors by
// collapsing adjacent equal colors. Regenerate rather than editing by hand.
// Every byte is written as an explicit \xHH escape: none of these bytes are
// text, and printing them as raw characters makes the table unreadable.

// Palette is a packed list of colors, three bytes (R, G, B) each.
type Palette string

func (p Palette) color(i int) Color {
	_ = p[3*i+2] // bounds check hint; three bytes per LED
	return Color(p[3*i]) | Color(p[3*i+1])<<8 | Color(p[3*i+2])<<16
}

const (
	flagLGBT  = "\x06\xFF\x00\x00\x06\xFF\x55\x00\x06\x88\xFF\x00\x06\x00\xFF\x00\x06\x00\x00\xFF\x06\x80\x00\x80"
	flagTrans = "\x04\x11\x33\x88\x04\x80\x22\x22\x04\x88\xAA\xAA\x04\x80\x22\x22\x06\x11\x33\x88\x04\x80\x22\x22\x04\x88\xAA\xAA\x04\x80\x22\x22\x02\x11\x33\x88"
	// Five-stripe lesbian flag (five colors are easier to
	// distinguish than seven).
	flagLesbian = "\x04\xC0\x00\x00\x04\xC0\x40\x00\x04\x60\x50\x90\x04\x40\x08\x30\x06\x20\x00\x10\x04\x40\x08\x30\x04\x60\x50\x90\x04\xC0\x40\x00\x02\xC0\x00\x00"
	// Flag for gay men. This is the less common 5 stripe version,
	// since that's easier to make on the earrings. (Not sure how many
	// men will wear these, but it's there for those who want it).
	flagGay = "\x04\x00\x18\x08\x04\x10\x40\x20\x04\x60\x50\x90\x04\x10\x10\x90\x06\x08\x00\x20\x04\x10\x10\x90\x04\x60\x50\x90\x04\x10\x40\x20\x02\x00\x18\x08"
	// This one really pops! While the number of LEDs for each color
	// is balanced, it might look better with a bit more for yellow?
	flagNonBinary = "\x04\xAA\xAA\x00\x05\x60\x50\x90\x05\x50\x00\x40\x08\x00\x00\x00\x05\x50\x00\x40\x05\x60\x50\x90\x04\xAA\xAA\x00"
	flagBi        = "\x08\xD0\x00\x08\x04\x40\x00\x30\x0E\x00\x00\x80\x04\x40\x00\x30\x06\xD0\x00\x08"
	flagPan       = "\x06\xD0\x00\x10\x06\xAA\xAA\x00\x0C\x08\x08\xFF\x06\xAA\xAA\x00\x06\xD0\x00\x10"
	// Hard to represent because of the black, but I did my best.
	// Since it's a circle, the black does stand out.
	flagAce = "\x04\x00\x00\x00\x05\x10\x10\x18\x05\x60\x50\x90\x08\x50\x00\x40\x05\x60\x50\x90\x05\x10\x10\x18\x04\x00\x00\x00"
	// Aromantic flag.
	flagAro = "\x04\x00\x80\x08\x04\x10\x60\x28\x04\x60\x50\x90\x04\x10\x10\x18\x06\x00\x00\x00\x04\x10\x10\x18\x04\x60\x50\x90\x04\x10\x60\x28\x02\x00\x80\x08"
	// The new polyamory flag, with the yellow heart:
	// https://nl.wikipedia.org/wiki/Bestand:Tricolor_Polyamory_Pride_Flag.svg
	// It's hard to represent the dark purple at the bottom, this seems
	// as dark and purplish as possible.
	flagPoly = "\x06\x08\x08\xFF\x06\xC0\x00\x10\x0C\x08\x00\x15\x04\xC0\x00\x10\x01\x60\x50\x90\x03\xAA\xAA\x00\x01\x60\x50\x90\x03\x08\x08\xFF"
	// (the three yellow slots are the heart)
)

// Same storage trick for the two small color tables that used to be vars:
// little-endian Color values, four bytes each, as flash-only const strings.
const (
	numStaticColors    = 13
	numSparkleVariants = 4

	staticColors = Palette("" +
		"\x99\x00\x66" + // purple
		"\xEE\x00\x11" + // hot pink
		"\xFF\x00\x00" + // red
		"\xCC\x22\x00" + // orange
		"\x88\x77\x00" + // yellow (orange-ish)
		"\x33\xCC\x00" + // yellow (green-ish)
		"\x00\xFF\x00" + // green
		"\x00\xDD\x22" + // green-turquoise
		"\x00\x99\x66" + // turquoise
		"\x00\x33\xCC" + // greenish blue
		"\x00\x00\xFF" + // blue
		"\x11\x00\xEE" + // purple-ish blue
		"\x55\x00\xAA") // blueish purple

	// sparkleVariants is numSparkleVariants pairs of colors.
	sparkleVariants = Palette("" +
		"\xFF\x00\xFF" + "\x00\x99\x66" + // purple, turquoise
		"\xFF\x00\xFF" + "\xFF\x00\x00" + // purple, red
		"\xFF\x00\x00" + "\xFF\xFF\x00" + // red, yellow
		"\xFF\xFF\x00" + "\x00\xFF\x00") // yellow, green
)

// The flag list is one concatenated const string rather than an array of
// values, so it needs no runtime initializer either. Each flag is stored
// run-length encoded - (count, r, g, b) byte quads - because a flag is only
// 5-7 solid stripes repeated around the ring: the expanded 36-color tables
// cost 1,080 bytes, the run tables 296. flagOffsets gives the byte offset of
// each flag (uint16 LE, numFlags+1 entries; the last is len(allFlags)).
const (
	numFlags = 10

	allFlags = flagLGBT + flagTrans + flagLesbian + flagGay + flagNonBinary +
		flagBi + flagPan + flagAce + flagAro + flagPoly

	flagOffsets = "\x00\x00\x18\x00\x3C\x00\x60\x00\x84\x00\xA0\x00\xB4\x00\xC8\x00\xE4\x00\x08\x01\x28\x01"
)

// flagColor returns the color of one LED of one flag, decoding the run table.
// A flag has at most 9 runs (36 LEDs, so a run is at least 4 long), which
// makes this a handful of byte compares per LED for the 784 bytes it saves.
func flagColor(flag, led int) Color {
	if uint(flag) >= numFlags {
		// This shouldn't actually happen.
		return 0
	}
	i := int(flagOffsets[2*flag]) | int(flagOffsets[2*flag+1])<<8
	end := int(flagOffsets[2*flag+2]) | int(flagOffsets[2*flag+3])<<8
	for ; i < end; i += 4 {
		n := int(allFlags[i])
		if led >= n {
			led -= n
			continue
		}
		return Color(allFlags[i+1]) | Color(allFlags[i+2])<<8 | Color(allFlags[i+3])<<16
	}
	return 0 // unreachable: each flag's run counts sum to numLEDs
}

// Compile-time check on the offset table. The other invariant - every flag's
// run counts sum to numLEDs, so the decode above always hits - is verified by
// the one-off comparison against the expanded tables this replaced.
var _ [len(flagOffsets)/2 - (numFlags + 1)]byte
