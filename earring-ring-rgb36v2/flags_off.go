//go:build !flags

package main

// Stub for the flag palettes; flags_palettes.go holds the data and the reason it
// is not built by default here.
//
// numFlags = 0 makes showFlag return black for every variant and lets the compiler
// drop the table indexing entirely, while modeFlag keeps its place in the mode
// enum. That is deliberate: the mode number is what gets stored in the state page,
// so renumbering the modes silently turns a stored "party sync" into a different
// mode. Master (modeSync=10) and this branch (modeSync=9) already differ that way
// and their stored states are not interchangeable; nothing here should add a third
// numbering.

const (
	flagSize = numLEDs * 4
	numFlags = 0

	allFlags = Palette("")
)
