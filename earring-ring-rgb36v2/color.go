package main

type Color uint32

func NewColor(r, g, b uint8) Color {
	return Color(r)<<0 | Color(g)<<8 | Color(b)<<16
}

func (c Color) R() uint8 {
	return uint8(c >> 0)
}

func (c Color) G() uint8 {
	return uint8(c >> 8)
}

func (c Color) B() uint8 {
	return uint8(c >> 16)
}

// Linearly scale the given color by the given intensity, which must be 0..256.
func (c Color) Scale(intensity int) Color {
	return NewColor(
		uint8((int(c.R())*intensity)/256),
		uint8((int(c.G())*intensity)/256),
		uint8((int(c.B())*intensity)/256))
}

// mix blends top into bottom by alpha (0..255), per channel: the noise
// palettes' interpolation, shared with the ported patterns.
//
//go:noinline
func mix(bottom, top Color, alpha uint8) Color {
	return NewColor(
		blend8(bottom.R(), top.R(), alpha),
		blend8(bottom.G(), top.G(), alpha),
		blend8(bottom.B(), top.B(), alpha))
}

// blend8 mixes top into bottom by topAlpha. Identical arithmetic to
// ledsgo.blend for 32-bit targets.
func blend8(bottom, top, topAlpha uint8) uint8 {
	bottomPart := int(bottom) * (255 - int(topAlpha))
	topPart := int(top) * int(topAlpha)
	return uint8((bottomPart + topPart + 255) / 256)
}
