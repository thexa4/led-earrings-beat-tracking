package main

// Read-only colour tables, stored as const strings so they cost flash only: an
// equivalent `var` would need the same bytes again as an initialiser plus the
// copy code and RAM to copy into.
//
// The ten flag palettes that used to be here moved to flags_palettes.go and are
// behind the `flags` build tag; this branch does not build them, see there.

// Palette is one flag: 36 packed Color values in a string.
type Palette string

func (p Palette) color(i int) Color {
	_ = p[4*i+3] // bounds check hint; four bytes per LED
	return Color(p[4*i]) | Color(p[4*i+1])<<8 | Color(p[4*i+2])<<16 | Color(p[4*i+3])<<24
}

// Same storage trick for the two small color tables that used to be vars:
// little-endian Color values, four bytes each, as flash-only const strings.
const (
	numStaticColors    = 13
	numSparkleVariants = 4

	staticColors = Palette("" +
		"\x99\x00\x66\x00" + // purple
		"\xee\x00\x11\x00" + // hot pink
		"\xff\x00\x00\x00" + // red
		"\xcc\x22\x00\x00" + // orange
		"\x88\x77\x00\x00" + // yellow (orange-ish)
		"\x33\xcc\x00\x00" + // yellow (green-ish)
		"\x00\xff\x00\x00" + // green
		"\x00\xdd\x22\x00" + // green-turquoise
		"\x00\x99\x66\x00" + // turquoise
		"\x00\x33\xcc\x00" + // greenish blue
		"\x00\x00\xff\x00" + // blue
		"\x11\x00\xee\x00" + // purple-ish blue
		"\x55\x00\xaa\x00") // blueish purple

	// sparkleVariants is numSparkleVariants pairs of colors.
	sparkleVariants = Palette("" +
		"\xff\x00\xff\x00" + "\x00\x99\x66\x00" + // purple, turquoise
		"\xff\x00\xff\x00" + "\xff\x00\x00\x00" + // purple, red
		"\xff\x00\x00\x00" + "\xff\xff\x00\x00" + // red, yellow
		"\xff\xff\x00\x00" + "\x00\xff\x00\x00") // yellow, green
)
