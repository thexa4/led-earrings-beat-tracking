package main

// Fast modulo 36 of the low byte of value: x/36 is x*7282>>18 exactly for
// every x < 256, which saves a 256-byte table and a call to the soft divide.
func fastModulo36(value int) int {
	x := value & 0xff
	return x - (x*7282>>18)*36
}

var xorstate uint32 = 0xb90ccbd4 // just a random number to start

func rand() uint32 {
	xorstate = xorshift32(xorstate)
	return xorstate
}

func xorshift32(state uint32) uint32 {
	state *= 0x9ab4ffa3 // just a random number
	state ^= state << 13
	state ^= state >> 17
	state ^= state << 5
	return state
}
