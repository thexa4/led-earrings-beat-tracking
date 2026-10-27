package main

// The sync data block: what party sync knows about the music, at a fixed RAM
// address, for uploaded programs (custom patterns) to read. The web compiler
// cannot be changed to link against firmware symbols, but any C, C++ or Rust
// program can read a constant address - so the address and the layout are the
// interface, documented for users in SYNC-PROGRAMS.md. Treat both as an ABI:
// never move or reorder a field, only append (and bump syncShareVersion).
//
// The block lives in the top 64 bytes of RAM, which stm32l031x6.ld leaves out
// of the RAM region so nothing else is ever placed there. It is not a Go
// global on purpose: a global's address moves with every build.
//
// syncPoll fills it once per analysis frame (64 Hz) while sync mode runs;
// outside sync mode the active flag is clear. Firmware without this feature
// leaves whatever happens to be in that RAM, which is why programs check the
// magic first.

import "unsafe"

const (
	syncShareAddr    = 0x20001fc0
	syncShareMagic   = 0x434e5953 // "SYNC" in memory order
	syncShareVersion = 1

	// Bits of syncShareT.flags.
	syncShareActive = 1 << 0 // sync mode is running and the block is live
	syncShareLocked = 1 << 1 // the beat tracker has a tempo
	syncShareSilent = 1 << 2 // too quiet to tell anything
)

// syncShareT is the block's layout. Offsets are fixed (see SYNC-PROGRAMS.md);
// the size check below catches an accidental change in padding.
type syncShareT struct {
	magic      uint32   // 0
	version    uint8    // 4
	flags      uint8    // 5
	pattern    uint8    // 6: music-chosen color scheme, 0..3, same on every earring
	beatPhase  uint8    // 7: 0 on the beat, rising to 255 just before the next
	frame      uint32   // 8: analysis frames since sync started (64 per second)
	beats      uint32   // 12: beats since sync started
	sections   uint32   // 16: section changes (anchors) since sync started
	beatMs     uint16   // 20: beat period in ms, 0 until locked
	level      uint8    // 22: overall loudness, 0..255
	bass       uint8    // 23: below 250 Hz
	mid        uint8    // 24: 250 Hz .. 1 kHz
	high       uint8    // 25: above 1.6 kHz
	sectionAge uint8    // 26: analysis frames since the last section change, saturating
	beatAge    uint8    // 27: analysis frames since the last beat, saturating
	bands      [8]uint8 // 28: band levels, lowest (<250 Hz) first, highest (>4 kHz) last
	hash       uint32   // 36: the current section's hash
	palette    uintptr  // 40: the pattern's palette, 16 RGB triples (48 bytes)
}

var _ [44 - unsafe.Sizeof(syncShareT{})]struct{}
var _ [unsafe.Sizeof(syncShareT{}) - 44]struct{}

var syncShare = (*syncShareT)(unsafe.Pointer(uintptr(syncShareAddr)))
