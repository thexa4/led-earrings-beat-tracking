//go:build baremetal

// Package rolled keeps loops rolled in the firmware, where flash is the
// budget: TinyGo builds with -opt=2, and LLVM's ARM unroller copies small
// loops four times, or out to every iteration. It leaves a loop with a call
// in it alone, and Loop is such a call that emits nothing (an empty asm
// statement). A loop of a few iterations that LLVM can count, from a constant
// or from the length of an array it indexes, is still unrolled in full (that
// is a different pass). On the host, where the packages that use it are
// tested, Loop is an empty function.
package rolled

import "device/arm"

// Loop, called anywhere in a loop's body, keeps that loop rolled.
func Loop() {
	arm.Asm("")
}
